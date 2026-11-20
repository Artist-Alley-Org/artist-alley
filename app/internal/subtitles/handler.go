// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.18.B-3 — subtitle track domain handler.
//
// Owns the (GetForAsset, Upsert, Delete) trio + cache management
// + the cross-package InvalidateForAsset hook the hard-delete path
// calls after an asset row is destroyed.
//
// # Why the explicit InvalidateForAsset entry point
//
// FK + CASCADE on asset_subtitle_tracks.asset_id means an asset
// hard-delete wipes the track rows automatically — DB state is
// consistent. But our cache.Cache[T] LRU is in-process state
// that the schema doesn't know about, and GetForAsset is
// read-through: it answers from the LRU without ever looking at
// the asset. So the tracks of a destroyed asset are served
// verbatim until the process restarts.
//
// # Where the call actually comes from (#935)
//
// This comment previously said "the assets/ HardDelete path
// explicitly calls subtitles.InvalidateForAsset(h, assetID)". That
// was never true and there was no such call anywhere — the only
// InvalidateForAsset invoked from assets/ is posts.InvalidateForAsset,
// a different function that happens to share the name.
//
// There is also no HardDelete in assets/ to call it: DeleteAsset is a
// SOFT delete, and the hard delete lives in softdelete.Service's
// retention GC. That is where the CASCADE fires, so that is where the
// eviction belongs. The composition root (internal/http/api.go) wires
// it via Service.OnAssetsHardDeleted, alongside the other caches the
// same CASCADE empties.
//
// This is the same pattern other packages use for cross-cutting
// cache invalidation (cache.Registry NOTIFY broadcasts handle the
// cross-process side; this handles the in-process call site).

package subtitles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// cacheDomainAssetSubtitleTracks is the per-asset LRU domain
// used by the cache.Registry. Cross-process invalidation broadcasts
// (NOTIFY/LISTEN per ADR 0013) pick this up automatically.
const cacheDomainAssetSubtitleTracks = "asset.subtitle_tracks"

// PinSubjectTypeTrack is the storage pin subject a track's VTT bytes are
// held by, one subject per (asset, lang): see [TrackPinSubjectID].
const PinSubjectTypeTrack = "subtitle_track"

// SourceFormatAI is the source_format of a track the AI transcriber
// wrote. The upload API accepts only file formats (vtt, srt, ...), so
// this value marks a track no person supplied.
const SourceFormatAI = "whisper"

// TrackPinSubjectID is the pin subject id of the (asset, lang) track.
func TrackPinSubjectID(assetID uuid.UUID, lang string) string {
	return assetID.String() + "-" + lang
}

// Track is the package-public projection of a row from
// asset_subtitle_tracks. Distinct from the sqlc-generated row
// type because the public Track has time.Time + uuid.UUID
// (not pgx wrappers); the API layer marshals this directly.
type Track struct {
	AssetID      uuid.UUID
	Lang         string
	Label        string
	FileHash     string
	SourceFormat string
	Confidence   float64
	CreatedAt    time.Time

	// ExpectedAssetFileHash is write-only, read by [Handler.Upsert]
	// and never filled on a read. When set, the track is written only
	// while the asset still shows that original (#1596), and Upsert
	// reports [ErrAssetSuperseded] otherwise. The AI transcriber sets
	// it to the bytes it listened to; a person's upload leaves it empty.
	ExpectedAssetFileHash string
}

// Handler is the subtitles package's primary surface. Construct
// via NewHandler at boot; pass into the assets package for the
// cross-package InvalidateForAsset call site.
type Handler struct {
	pool    *pgxpool.Pool
	queries *Queries
	logger  *slog.Logger

	// Per-asset track-list cache. Read-heavy, write-rare.
	// Capacity 10k: each entry is a small []Track (typically 0-5
	// rows), so the memory ceiling is well under a MB even fully
	// populated.
	tracks *cache.Cache[[]Track]
}

// NewHandler wires the subtitles handler. registry may be nil
// (tests that don't exercise cache behaviour can skip it); when
// nil, the read path goes straight to Postgres every time.
func NewHandler(pool *pgxpool.Pool, registry *cache.Registry, logger *slog.Logger) *Handler {
	h := &Handler{
		pool:    pool,
		queries: New(pool),
		logger:  logger,
	}
	if registry != nil {
		h.tracks = cache.Register[[]Track](registry, cacheDomainAssetSubtitleTracks, 10_000)
	}
	return h
}

// GetForAsset returns every track for assetID, ordered by lang
// ASC. Empty slice when none — never nil, so frontend rendering
// doesn't need to nil-check.
//
// Cache: read-through. Hit returns immediately; miss queries +
// caches the result.
//
// Does NOT call RequiresAudioVideo — the read path is permissive
// (returns [] for an image asset rather than 422; the API layer
// adds the guard when surfaced via the public endpoint). This
// asymmetry is intentional: federation pass-through serialization
// + admin observability surfaces want the read to "just work".
func (h *Handler) GetForAsset(ctx context.Context, assetID uuid.UUID) ([]Track, error) {
	key := assetID.String()
	if h.tracks != nil {
		if cached, ok := h.tracks.Get(key); ok {
			return cached, nil
		}
	}
	rows, err := h.queries.ListSubtitleTracksForAsset(ctx, pgtype.UUID{Bytes: assetID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("subtitles: list tracks for %s: %w", assetID, err)
	}
	out := make([]Track, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	if h.tracks != nil {
		h.tracks.Add(key, out)
	}
	return out, nil
}

// Upsert inserts or replaces a track for (assetID, lang). Guards
// on RequiresAudioVideo (image / 3D / etc. assets reject with
// ErrSubtitlesNotApplicable) + ValidateLang (rejects malformed
// language tags before they hit the schema CHECK).
//
// Cache is invalidated AFTER the upsert commits.
//
// The row and its pin commit together (#1596). The caller stored the VTT
// bytes, pinned, before this call; the pin is asserted again here, in the
// row's own transaction, because a concurrent release of an unused pin
// (a refused late transcription with identical bytes, or a replacement
// retiring the old track) can drop that shared pin row in between. The
// bytes the row held before are released in the same transaction when no
// track of (asset, lang) uses them any more, so an overwrite never leaves
// the old transcript pinned.
func (h *Handler) Upsert(ctx context.Context, t Track) (Track, error) {
	if err := RequiresAudioVideo(ctx, h.queries, t.AssetID); err != nil {
		return Track{}, err
	}
	if err := ValidateLang(t.Lang); err != nil {
		return Track{}, err
	}
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Track{}, fmt.Errorf("subtitles: upsert begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := New(tx)
	pgAsset := pgtype.UUID{Bytes: t.AssetID, Valid: true}

	// Asset row first, guarded by the expected original: the replacement
	// takes this lock too, so a track write and a replacement serialise.
	if _, err := q.LockAssetForTrackWrite(ctx, LockAssetForTrackWriteParams{AssetID: pgAsset, AssetFileHash: t.ExpectedAssetFileHash}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if t.ExpectedAssetFileHash != "" {
				return Track{}, ErrAssetSuperseded
			}
			return Track{}, ErrAssetNotFound
		}
		return Track{}, fmt.Errorf("subtitles: upsert %s/%s: lock asset: %w", t.AssetID, t.Lang, err)
	}
	// The bytes this (asset, lang) track holds now, released below once
	// the row points elsewhere (#1596). Read under the asset lock, which
	// every track writer takes first, so it is the row being replaced.
	prior := ""
	if cur, err := q.GetSubtitleTrack(ctx, GetSubtitleTrackParams{AssetID: pgAsset, Lang: t.Lang}); err == nil {
		prior = cur.FileHash
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Track{}, fmt.Errorf("subtitles: upsert %s/%s: read current: %w", t.AssetID, t.Lang, err)
	}
	if err := lockTrackObjects(ctx, tx, t.FileHash, prior); err != nil {
		return Track{}, err
	}
	row, err := q.UpsertSubtitleTrack(ctx, UpsertSubtitleTrackParams{
		AssetID:       pgAsset,
		Lang:          t.Lang,
		Label:         t.Label,
		FileHash:      t.FileHash,
		SourceFormat:  t.SourceFormat,
		Confidence:    float32(t.Confidence),
		AssetFileHash: t.ExpectedAssetFileHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if t.ExpectedAssetFileHash != "" {
			return Track{}, ErrAssetSuperseded
		}
		return Track{}, ErrAssetNotFound
	}
	if err != nil {
		return Track{}, fmt.Errorf("subtitles: upsert %s/%s: %w", t.AssetID, t.Lang, err)
	}
	if err := storage.New(tx).ClearGCEligible(ctx, t.FileHash); err != nil {
		return Track{}, fmt.Errorf("subtitles: upsert %s/%s: pin: %w", t.AssetID, t.Lang, err)
	}
	if err := q.EnsureTrackPin(ctx, EnsureTrackPinParams{ObjectHash: t.FileHash, PinSubjectID: TrackPinSubjectID(t.AssetID, t.Lang)}); err != nil {
		return Track{}, fmt.Errorf("subtitles: upsert %s/%s: pin: %w", t.AssetID, t.Lang, err)
	}
	if prior != "" && prior != t.FileHash {
		if err := releaseTrackPin(ctx, tx, t.AssetID, t.Lang, prior, storage.DefaultGCGracePeriod); err != nil {
			return Track{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Track{}, fmt.Errorf("subtitles: upsert %s/%s: commit: %w", t.AssetID, t.Lang, err)
	}
	h.invalidate(ctx, t.AssetID)
	return fromRow(row), nil
}

// lockTrackObjects locks the object rows a track write touches, in hash
// order, after the asset row: two writers trading bytes between
// assets then queue instead of deadlocking. A hash with no object row
// locks nothing.
func lockTrackObjects(ctx context.Context, tx pgx.Tx, hashes ...string) error {
	uniq := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if h != "" && !slices.Contains(uniq, h) {
			uniq = append(uniq, h)
		}
	}
	slices.Sort(uniq)
	sq := storage.New(tx)
	for _, h := range uniq {
		if err := sq.LockObjectForPinChange(ctx, h); err != nil {
			return fmt.Errorf("subtitles: lock %s: %w", h, err)
		}
	}
	return nil
}

// ReleaseUnusedTrackBytes drops the (asset, lang) track pin from VTT
// bytes that no live track of that (asset, lang) uses, and marks them
// for GC after the default grace when nothing else holds them (#1596).
// The AI transcriber calls it when its write was refused because the
// asset's original was replaced: it stored and pinned the transcript
// first, and without this the pin would hold those bytes for ever.
func (h *Handler) ReleaseUnusedTrackBytes(ctx context.Context, assetID uuid.UUID, lang, hash string) error {
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("subtitles: release begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := releaseTrackPin(ctx, tx, assetID, lang, hash, storage.DefaultGCGracePeriod); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RetireAITracks removes, on the caller's transaction, every AI track of
// the asset and releases each one's pin (#1596). An original replacement
// calls it: those tracks are the words of the recording it replaces. A
// person's tracks stay. The caller holds the asset row lock and calls
// [InvalidateTracksOn] after its commit.
func RetireAITracks(ctx context.Context, tx pgx.Tx, assetID uuid.UUID, grace time.Duration) (int, error) {
	rows, err := New(tx).RetireAITracksForAsset(ctx, pgtype.UUID{Bytes: assetID, Valid: true})
	if err != nil {
		return 0, fmt.Errorf("subtitles: retire AI tracks: %w", err)
	}
	// Object rows in hash order, as every multi-object track write locks them.
	slices.SortFunc(rows, func(x, y RetireAITracksForAssetRow) int { return strings.Compare(x.FileHash, y.FileHash) })
	for _, r := range rows {
		if err := releaseTrackPin(ctx, tx, assetID, r.Lang, r.FileHash, grace); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// releaseTrackPin is the one pin release both paths use: object row
// locked first (the order every pin writer uses), the pin dropped only
// when no live track of (asset, lang) uses the bytes, and the GC mark
// decided in a fresh statement.
func releaseTrackPin(ctx context.Context, tx pgx.Tx, assetID uuid.UUID, lang, hash string, grace time.Duration) error {
	sq := storage.New(tx)
	if err := sq.LockObjectForPinChange(ctx, hash); err != nil {
		return fmt.Errorf("subtitles: release %s: lock: %w", hash, err)
	}
	n, err := New(tx).ReleaseUnusedTrackPin(ctx, ReleaseUnusedTrackPinParams{
		ObjectHash: hash, PinSubjectID: TrackPinSubjectID(assetID, lang),
		AssetID: pgtype.UUID{Bytes: assetID, Valid: true}, Lang: lang,
	})
	if err != nil {
		return fmt.Errorf("subtitles: release %s: %w", hash, err)
	}
	if n == 0 {
		return nil
	}
	if err := sq.MarkGCEligibleIfOrphaned(ctx, storage.MarkGCEligibleIfOrphanedParams{
		Hash: hash, Column2: pgtype.Interval{Microseconds: grace.Microseconds(), Valid: true},
	}); err != nil {
		return fmt.Errorf("subtitles: release %s: gc mark: %w", hash, err)
	}
	return nil
}

// InvalidateTracksOn drops the asset's cached track list in this process
// and broadcasts the drop to every other one, for a writer that changed
// the tracks without holding a [Handler] (#1596: the replacement).
func InvalidateTracksOn(ctx context.Context, registry *cache.Registry, assetID uuid.UUID) error {
	if registry == nil {
		return nil
	}
	return registry.InvalidateNow(ctx, cacheDomainAssetSubtitleTracks, assetID.String())
}

// Delete removes a single (assetID, lang) track. Returns:
//
//   - nil on success
//   - ErrTrackNotFound if no matching row existed
//   - ErrSubtitlesNotApplicable if the asset isn't video/audio
//
// The "not applicable" check fires BEFORE the not-found check —
// a client trying to delete a track on an image asset gets 422,
// not 404. This matches the upsert semantics (same gate fires
// first there too) so the API surface is consistent.
func (h *Handler) Delete(ctx context.Context, assetID uuid.UUID, lang string) error {
	if err := RequiresAudioVideo(ctx, h.queries, assetID); err != nil {
		return err
	}
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("subtitles: delete begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := New(tx)
	pgAsset := pgtype.UUID{Bytes: assetID, Valid: true}
	if _, err := q.LockAssetForTrackWrite(ctx, LockAssetForTrackWriteParams{AssetID: pgAsset}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAssetNotFound
		}
		return fmt.Errorf("subtitles: delete %s/%s: lock asset: %w", assetID, lang, err)
	}
	hash, err := q.DeleteSubtitleTrack(ctx, DeleteSubtitleTrackParams{AssetID: pgAsset, Lang: lang})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTrackNotFound
	}
	if err != nil {
		return fmt.Errorf("subtitles: delete %s/%s: %w", assetID, lang, err)
	}
	// The removed track's bytes lose their (asset, lang) pin in the same
	// transaction (#1596) and are scheduled for GC once nothing holds them.
	if err := releaseTrackPin(ctx, tx, assetID, lang, hash, storage.DefaultGCGracePeriod); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("subtitles: delete %s/%s: commit: %w", assetID, lang, err)
	}
	h.invalidate(ctx, assetID)
	return nil
}

// ErrTrackNotFound is returned by Delete when no (asset, lang)
// row matched. API layer maps to HTTP 404.
var ErrTrackNotFound = errors.New("subtitles: track not found")

// Get retrieves a single track. Returns ErrTrackNotFound when
// the row is absent. Used by the burned-export job to fetch the
// source VTT hash + by admin tooling.
func (h *Handler) Get(ctx context.Context, assetID uuid.UUID, lang string) (Track, error) {
	row, err := h.queries.GetSubtitleTrack(ctx, GetSubtitleTrackParams{
		AssetID: pgtype.UUID{Bytes: assetID, Valid: true},
		Lang:    lang,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Track{}, ErrTrackNotFound
		}
		return Track{}, fmt.Errorf("subtitles: get %s/%s: %w", assetID, lang, err)
	}
	return fromRow(row), nil
}

// Count returns the number of tracks for assetID. Observability +
// admin surface only — NOT used in any quota check. Subtitles
// don't count toward asset totals.
func (h *Handler) Count(ctx context.Context, assetID uuid.UUID) (int64, error) {
	return h.queries.CountSubtitleTracksForAsset(ctx, pgtype.UUID{Bytes: assetID, Valid: true})
}

// InvalidateForAsset is the cross-package entry point the asset
// hard-delete path calls. FK CASCADE wipes the rows; this clears the
// cache that would otherwise keep serving them.
//
// The caller is softdelete.Service.OnAssetsHardDeleted, wired at the
// composition root — see the package doc for why it is not in assets/,
// and for the claim this function carried for two phases while having
// no callers at all (#935).
//
// nil-safe handler: if subtitles isn't wired in a particular
// build (test fixtures), the call is a no-op.
func InvalidateForAsset(h *Handler, assetID uuid.UUID) {
	if h == nil {
		return
	}
	h.invalidate(context.Background(), assetID)
}

func (h *Handler) invalidate(ctx context.Context, assetID uuid.UUID) {
	if h.tracks == nil {
		return
	}
	if err := h.tracks.Invalidate(ctx, assetID.String()); err != nil && h.logger != nil {
		h.logger.LogAttrs(ctx, slog.LevelWarn,
			"subtitles.cache.invalidate.failed",
			slog.String("asset_id", assetID.String()),
			slog.String("err", err.Error()),
		)
	}
}

// fromRow converts a sqlc-generated row type to the public Track
// shape (uuid.UUID + time.Time instead of pgx wrappers).
func fromRow(r AssetSubtitleTrack) Track {
	return Track{
		AssetID:      uuid.UUID(r.AssetID.Bytes),
		Lang:         r.Lang,
		Label:        r.Label,
		FileHash:     r.FileHash,
		SourceFormat: r.SourceFormat,
		Confidence:   float64(r.Confidence),
		CreatedAt:    r.CreatedAt.Time,
	}
}
