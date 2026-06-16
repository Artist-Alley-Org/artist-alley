// Phase 1.18.B-3 — subtitle track domain handler.
//
// Owns the (GetForAsset, Upsert, Delete) trio + cache management
// + the cross-package InvalidateForAsset hook that assets/ calls
// after hard-deleting an asset row.
//
// # Why the explicit InvalidateForAsset entry point
//
// FK + CASCADE on asset_subtitle_tracks.asset_id means an asset
// hard-delete wipes the track rows automatically — DB state is
// consistent. But our cache.Cache[T] LRU is in-process state
// that the schema doesn't know about. The assets/ HardDelete
// path explicitly calls subtitles.InvalidateForAsset(h, assetID)
// so a stale read after delete returns the empty slice (not the
// pre-delete cached tracks).
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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// cacheDomainAssetSubtitleTracks is the per-asset LRU domain
// used by the cache.Registry. Cross-process invalidation broadcasts
// (NOTIFY/LISTEN per ADR 0013) pick this up automatically.
const cacheDomainAssetSubtitleTracks = "asset.subtitle_tracks"

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
func (h *Handler) Upsert(ctx context.Context, t Track) (Track, error) {
	if err := RequiresAudioVideo(ctx, h.queries, t.AssetID); err != nil {
		return Track{}, err
	}
	if err := ValidateLang(t.Lang); err != nil {
		return Track{}, err
	}
	row, err := h.queries.UpsertSubtitleTrack(ctx, UpsertSubtitleTrackParams{
		AssetID:      pgtype.UUID{Bytes: t.AssetID, Valid: true},
		Lang:         t.Lang,
		Label:        t.Label,
		FileHash:     t.FileHash,
		SourceFormat: t.SourceFormat,
		Confidence:   float32(t.Confidence),
	})
	if err != nil {
		return Track{}, fmt.Errorf("subtitles: upsert %s/%s: %w", t.AssetID, t.Lang, err)
	}
	h.invalidate(ctx, t.AssetID)
	return fromRow(row), nil
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
	n, err := h.queries.DeleteSubtitleTrack(ctx, DeleteSubtitleTrackParams{
		AssetID: pgtype.UUID{Bytes: assetID, Valid: true},
		Lang:    lang,
	})
	if err != nil {
		return fmt.Errorf("subtitles: delete %s/%s: %w", assetID, lang, err)
	}
	if n == 0 {
		return ErrTrackNotFound
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

// InvalidateForAsset is the cross-package entry point assets/
// calls in its HardDelete path. FK CASCADE wipes the rows; this
// clears the cache.
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
