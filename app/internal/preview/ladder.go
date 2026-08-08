// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.n16f.net/thumbhash"

	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// ---------------------------------------------------------------------------
// The shared "I have a rendered preview image, turn it into an asset's
// visible surface" step.
//
// WHY THIS EXISTS. Every non-raster preview handler ends the same way:
// it produces one image — an ffmpeg waveform, a three.js turntable frame,
// a pdftoppm page render, a glyph specimen, a video poster — and then
// fans that image across the configured variant ladder. Ten handlers
// carried ten near-identical copies of that loop, and the raster handler
// carried an eleventh that additionally stamped assets.thumbhash from
// the decoded source.
//
// The consequence of the duplication was #645: the blur-up placeholder
// was computed for image SOURCES only, so 618 assets that had a
// perfectly good 320² rendered preview sitting in storage — audio, 3D,
// fonts, video, epub, text — shipped thumbhash=NULL and their tiles
// flashed blank instead of fading up from a blur. The fix is not "add
// one more call to ten files"; it is to make the ladder fan and the
// thumbhash stamp the SAME step, so a handler cannot do one without the
// other, and a future format handler gets both for free.
// ---------------------------------------------------------------------------

// ladderInput is everything fanToLadder needs. Handler structs differ
// (each carries its own tool paths, timeouts, decoders), so the shared
// step takes explicit dependencies rather than an interface every
// handler would have to satisfy.
type ladderInput struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	// AssetID owns the thumbhash column. Required for the thumbhash
	// stamp; a zero UUID skips it (the ladder still runs).
	AssetID uuid.UUID
	// Hash is the content hash the variants are keyed by.
	Hash string
	// Src is the decoded preview image. Full resolution — the ladder
	// resizes per rung and the thumbhash encoder downsamples
	// internally.
	Src image.Image

	// Kind names the pipeline for log keys: "preview.<kind>.…".
	Kind string
	// Source is an optional discriminator for handlers that fan from
	// more than one origin (the model handler fans both a three.js
	// poster and an mview-extracted thumb).
	Source string
	// Overwrite re-encodes rungs that already exist on the backend
	// instead of leaving them alone (which is what makes a re-queue
	// nearly free).
	//
	// Set from the job payload's Force flag (#760): it is the operator
	// saying "the rungs in storage are stale, paint over them". Without
	// it a forced job re-renders the SOURCE and then quietly declines to
	// write the result, which is the bug wearing a different hat.
	Overwrite bool
}

// fanToLadder writes every configured preview variant from one decoded
// image, then stamps that image's shape onto the asset — see
// stampSourceShape.
//
// The stamps run LAST and UNCONDITIONALLY — including on the re-queue
// path where every rung already exists and nothing is encoded. That is
// deliberate: an asset whose variants were generated before #645 or
// #757 shipped has the bytes but not the facts, and re-running its
// preview job is the operator's natural "fix this asset" gesture.
// Skipping the stamps because there was no work to do would make that
// gesture a no-op.
//
// Both are computed from the FULL source image, not from a rung. The
// card renders a contain rung (CardThumb picks the smallest one —
// square `col` would "flash the wrong shape before swapping"), and both
// the placeholder and the reserved tile have to match the shape of the
// picture that arrives. A 2048×384 waveform hashed at source produces a
// wide blur that sits correctly in the ~16:3 tile masonry gives it;
// hashed from `col` it would be a square blur letterboxed inside a
// billboard, and measured from `col` it would report 1:1 — which is
// precisely the wall of squares #757 is about.
func fanToLadder(ctx context.Context, in ladderInput) error {
	if in.SysConfig != nil {
		if err := fanVariants(ctx, in); err != nil {
			return err
		}
	}
	stampSourceShape(ctx, in.Pool, in.Logger, in.Kind, in.AssetID, in.Src, in.Overwrite)
	return nil
}

// stampSourceShape records everything the card needs to know about the
// ladder's SOURCE image, as opposed to its rungs: the blur-up thumbhash
// and the pixel dimensions the tile reserves space from.
//
// ONE FUNCTION, TWO STAMPS, DELIBERATELY. They are the same fact —
// "this is the shape and colour of the picture this asset shows" —
// derived from the same decoded image at the same moment, and #645
// (thumbhash written for rasters only) and #757 (dimensions written for
// nobody) are the same bug twice: a per-asset property computed in one
// handler and forgotten by the other ten. Keeping the pair in a single
// call means a new format handler either gets both or gets neither, and
// "neither" is a compile error the moment it forgets to call the ladder.
//
// Both stamps are best-effort — a preview job must not fail because a
// placeholder or a tile height is missing — and both run even on the
// re-queue path where every rung already existed and nothing was
// encoded. That is the point: re-running an asset's preview job is the
// operator's natural "fix this asset" gesture, and an asset whose
// variants predate either stamp has the bytes but not the fact.
func stampSourceShape(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, kind string, id uuid.UUID, src image.Image, overwrite bool) {
	// A forced re-render replaces the stamp as well as the rungs (#760).
	// The thumbhash is a blur of the pixels we just declared wrong, and
	// it is what the card shows FIRST — leaving it means a corrected
	// thumbnail fades up out of the magenta one it replaced. Everywhere
	// else the never-overwrite rule stands; see setThumbhash*.
	if overwrite {
		setThumbhash(ctx, pool, logger, kind, id, src)
	} else {
		setThumbhashIfMissing(ctx, pool, logger, kind, id, src)
	}
	setPixelDims(ctx, pool, logger, kind, id, src)
}

// setPixelDims records the ladder source's width and height on the
// asset (#757).
//
// UNCONDITIONAL, unlike the thumbhash's if-missing default. There is no
// operator-authored value to protect here — the write is guarded by an
// IS DISTINCT FROM so an unchanged pair costs nothing — and a stale
// dimension is worse than an absent one: it is what the layout reserves
// space from before a single byte of image arrives, so a wrong pair
// mis-sizes the tile with total confidence and never corrects. If a
// replace-file or a renderer change altered the shape, the shape we
// just decoded is the true one.
func setPixelDims(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, kind string, id uuid.UUID, src image.Image) {
	if pool == nil || src == nil || id == uuid.Nil {
		return
	}
	b := src.Bounds()
	if err := pixeldims.Record(ctx, pool, id, b.Dx(), b.Dy()); err != nil {
		logAttrs(logger, ctx, slog.LevelDebug, "preview."+kind+".pixel_dims_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

// fanVariants is the ladder loop proper. Split out so fanToLadder reads
// as "variants, then thumbhash" and the nil-SysConfig guard (tests omit
// it) doesn't cost the thumbhash stamp.
func fanVariants(ctx context.Context, in ladderInput) error {
	cfg, err := in.SysConfig.GetPreviews(ctx)
	if err != nil {
		return fmt.Errorf("preview.%s: load preview config: %w", in.Kind, err)
	}
	for _, v := range cfg.Variants {
		if v.Key == storage.VariantOriginal {
			continue
		}
		// variantDone, not variantOnBackend: a skipped rung must still
		// leave the DB's record of it correct (#827). The two spellings
		// differ only in that side effect, and this loop is the single
		// biggest producer of skips in the system.
		if variantDone(ctx, in.Storage, in.Hash, v.Key, in.Overwrite) {
			continue
		}
		dst := resizeFor(ctx, in.Src, v)
		var buf bytes.Buffer
		ctype, err := encodeImage(&buf, dst, v)
		if err != nil {
			// An encode failure is per-rung and non-fatal: the other
			// rungs are still worth writing, and the job reporting
			// success with 3 of 4 rungs beats reporting failure with 0.
			logAttrs(in.Logger, ctx, slog.LevelWarn, "preview."+in.Kind+".encode_failed",
				slog.String("variant", v.Key),
				slog.String("source", in.Source),
				slog.String("err", err.Error()))
			continue
		}
		if _, err := in.Storage.Backend.Put(ctx, in.Hash, v.Key, bytes.NewReader(buf.Bytes())); err != nil {
			// Loud-fail. A silently skipped Put leaves the asset with a
			// missing thumbnail while the job reports success — the
			// col-404 class. The job system retries up to max_attempts,
			// so a transient backend blip self-heals.
			return fmt.Errorf("preview.%s: backend put variant %s: %w", in.Kind, v.Key, err)
		}
		if err := storage.New(in.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
			ObjectHash:  in.Hash,
			VariantKey:  v.Key,
			SizeBytes:   int64(buf.Len()),
			ContentType: ctype,
			Metadata:    []byte("{}"),
		}); err != nil {
			logAttrs(in.Logger, ctx, slog.LevelWarn, "preview."+in.Kind+".upsert_variant_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
		}
	}
	return nil
}

// variantOnBackend reports whether the rung's bytes are already on the
// storage backend. Backend-first (not the DB row) because the bytes are
// what a request actually serves.
//
// PURE — no reconcile. This is the spelling for paths that only want to
// COUNT skips (the rebuild report's Stale tally, `aa seed`'s willSkip),
// where a dry run must not write anything. The spelling that decides
// whether to skip real work is variantDone, and that one heals.
func variantOnBackend(ctx context.Context, st *storage.Service, hash, key string) bool {
	if st == nil {
		return false
	}
	_, err := st.Backend.Stat(ctx, hash, key)
	return err == nil
}

// variantDone is the ONE idempotency check every preview handler asks
// before deciding it has nothing to do: "this output already exists AND
// I have not been told to rebuild it".
//
// It replaces eleven byte-identical `func (h *XHandler) variantExists`
// methods (#760). The duplication was not merely untidy — it was the
// reason the force flag could not be added safely: honouring it meant
// eleven independent edits, and any handler that was missed would have
// gone on silently skipping while reporting success, which is precisely
// the bug being fixed, reintroduced with a new surface. With one
// function taking force as a REQUIRED argument, forgetting a handler is
// a compile error rather than a phantom control.
//
// Backend-first for the same reason variantOnBackend is: the bytes are
// what a request serves, and a DB row without bytes still 404s.
//
// A `true` answer also RECONCILES (#827). Backend-first is the right
// answer to "must I render this?" and the wrong answer to "may the API
// announce this?", and the gap between the two is not theoretical: it is
// what a restored database backup on an intact storage volume produces,
// on every asset at once, with every preview job reporting `done`. So
// the skip writes the storage_variants row the renderer would have
// written and the announcement catches up with the bytes. Best-effort
// and log-only — a preview job that correctly found nothing to do must
// not fail because the heal did not take.
func variantDone(ctx context.Context, st *storage.Service, hash, key string, force bool) bool {
	if force {
		return false
	}
	if st == nil {
		return false
	}
	info, err := st.Backend.Stat(ctx, hash, key)
	if err != nil {
		return false
	}
	st.ReconcileVariantBestEffort(ctx, hash, key, info)
	return true
}

// ladderAnchorRungs are the rungs whose presence a handler reads as
// "the raster ladder already ran for this hash". They are the four the
// frontend actually requests, so a set that is complete here is a set
// no card or viewer will 404 on.
var ladderAnchorRungs = []string{"col", "preview", "screen", "hires"}

// ladderDone is the re-queue early exit that eight handlers spelled out
// four lines at a time. One statement of the rule, so the force flag
// reaches all of them at once.
//
// Like variantDone, which it is built from, a `true` answer has also
// reconciled every anchor rung's storage_variants row (#827).
func ladderDone(ctx context.Context, st *storage.Service, hash string, force bool) bool {
	for _, key := range ladderAnchorRungs {
		if !variantDone(ctx, st, hash, key, force) {
			return false
		}
	}
	return true
}

// reconcileLadderRows heals the storage_variants row for every anchor
// rung whose bytes are on the backend, without deciding anything.
//
// ladderDone already does this as a side effect, which covers the eight
// handlers that ask it. The 3D handler does not: its re-queue sentinel
// is a bundle of five keys (`col`, the sprite sheet, a turntable frame,
// a reference view, the iso source) and never asks about `preview`,
// `screen` or `hires`. Left alone, a healed 3D asset comes back with
// preview_available true and ladder_available false — the flag that
// gates responsive `srcset` (#610) — because three of its four rungs
// have bytes and no row. Hence a spelling that reconciles and returns
// nothing, rather than a predicate called for its side effect.
func reconcileLadderRows(ctx context.Context, st *storage.Service, hash string) {
	for _, key := range ladderAnchorRungs {
		variantDone(ctx, st, hash, key, false)
	}
}

// healThumbhashOnSkip stamps a missing thumbhash from a rung that is
// already on the backend, and is what a handler calls INSTEAD of
// rendering when ladderDone said there was nothing to do.
//
// WHY IT IS NOT IN fanToLadder. Eight handlers never reach the ladder on
// a re-queue: they early-exit at ladderDone, before decoding anything, so
// there is no source image to hash. audio.go said so explicitly and
// declined to fix it, on the correct grounds that re-rendering a waveform
// to recover 30 bytes is the wrong trade. This does not re-render — it
// reads back a 320px rung the pipeline already wrote, which is precisely
// what the thumbhash backfill sweep does, per asset instead of per
// install.
//
// WHY IT IS NEEDED AT ALL, given that sweep exists. The sweep's
// population is DB-first (`EXISTS … storage_variants … 'col'`), so in the
// split-brain state #827 describes it selects nothing: bytes on disk, no
// rows, no candidates, and the deficit it was written to close (1,105
// thumbhashes for 1,947 assets, measured on dev) is invisible to it. The
// reconcile above restores those rows, but only the next boot re-enqueues
// the sweep. Stamping here closes the asset the job is already holding.
//
// Cost in the steady state is one indexed primary-key lookup that finds a
// non-NULL thumbhash and stops. Only an asset that actually lacks one
// pays for a variant read.
//
// DELIBERATELY THUMBHASH ONLY, not the pixel-dimension half of
// stampSourceShape. Dimensions are recorded from the ladder's SOURCE, and
// a rung is not the source: hashing a 320px `col` yields the same blur as
// hashing the 4096px render, but MEASURING it would record 320×240 as the
// asset's size and mis-reserve every tile with total confidence (#757).
func healThumbhashOnSkip(ctx context.Context, in ladderInput) {
	if in.Pool == nil || in.Storage == nil || in.AssetID == uuid.Nil || in.Hash == "" {
		return
	}
	var missing bool
	if err := in.Pool.QueryRow(ctx,
		`SELECT thumbhash IS NULL FROM assets WHERE id = $1::uuid AND deleted_at IS NULL`,
		in.AssetID.String(),
	).Scan(&missing); err != nil || !missing {
		return
	}
	src, err := loadRenderedLadderImage(ctx, in.Storage, in.SysConfig, in.Hash, defaultMaxVariantBytes)
	if err != nil {
		logAttrs(in.Logger, ctx, slog.LevelDebug, "preview."+in.Kind+".thumbhash_heal_no_source",
			slog.String("asset_id", in.AssetID.String()),
			slog.String("err", err.Error()))
		return
	}
	setThumbhashIfMissing(ctx, in.Pool, in.Logger, in.Kind, in.AssetID, src)
}

// defaultMaxVariantBytes caps a rendered-rung read-back. Rungs are small
// by construction (the largest default is a 4096px webp); the cap guards
// against a corrupt length, not a policy.
const defaultMaxVariantBytes int64 = 64 * 1024 * 1024

// loadRenderedLadderImage downloads and decodes the best rendered rung
// for a hash: the smallest CONTAIN rung the install's ladder defines,
// falling back to `col`.
//
// Free function, shared by the thumbhash backfill sweep and the
// skip-path heal, because "which rung best represents this asset" has
// exactly one right answer and two callers who must not drift on it.
//
// Contain beats `col` because `col` is a 320² centre-CROP: the card
// renders a contain rung and sizes its masonry tile from that rung's
// aspect ratio (#640/#646), so a hash taken from the square crop paints
// a square blur inside a 16:3 audio tile and then snaps.
func loadRenderedLadderImage(ctx context.Context, st *storage.Service, sc *sysconfig.Store, hash string, maxBytes int64) (image.Image, error) {
	if hash == "" {
		return nil, errors.New("asset has no file_hash")
	}
	for _, key := range ladderReadbackKeys(ctx, sc) {
		img, err := decodeStoredVariant(ctx, st, hash, key, maxBytes)
		if err == nil {
			return img, nil
		}
	}
	return nil, fmt.Errorf("no decodable rendered preview for hash %s", hash)
}

// ladderReadbackKeys is the ordered list of rungs to try when reading a
// render back out of storage. Read from the install's CONFIGURED ladder
// rather than hardcoded, so an operator who renamed or retuned their
// rungs still gets a read-back (#610's trap, server side). `col` is
// appended last as the universal fallback.
func ladderReadbackKeys(ctx context.Context, sc *sysconfig.Store) []string {
	keys := []string{}
	if sc != nil {
		if cfg, err := sc.GetPreviews(ctx); err == nil {
			contain := make([]sysconfig.PreviewVariant, 0, len(cfg.Variants))
			for _, v := range cfg.Variants {
				if v.Key == storage.VariantOriginal || v.Fit != sysconfig.PreviewFitContain {
					continue
				}
				contain = append(contain, v)
			}
			sort.Slice(contain, func(i, j int) bool { return contain[i].MaxDim < contain[j].MaxDim })
			for _, v := range contain {
				keys = append(keys, v.Key)
			}
		}
	}
	return append(keys, "col")
}

// decodeStoredVariant reads one rung off the backend and decodes it.
func decodeStoredVariant(ctx context.Context, st *storage.Service, hash, key string, maxBytes int64) (image.Image, error) {
	if st == nil {
		return nil, errors.New("no storage service")
	}
	rc, info, err := st.Download(ctx, hash, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if info != nil && info.Size > maxBytes {
		return nil, fmt.Errorf("variant %s too large: %d bytes", key, info.Size)
	}
	blob, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(blob))
	if err != nil {
		return nil, fmt.Errorf("decode variant %s: %w", key, err)
	}
	return img, nil
}

// setThumbhashIfMissing encodes src into a ~30-byte thumbhash and
// writes it onto the asset when the column is still NULL.
//
// Best-effort by design: the blur-up placeholder is a nicety, and no
// preview job should fail because of it. Never overwrites — see
// SetAssetThumbhashIfMissing, whose NULL guard is what keeps the
// synchronous compute in CreateAsset from racing the worker, and what
// makes the #645 backfill safe to re-run.
func setThumbhashIfMissing(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, kind string, id uuid.UUID, src image.Image) {
	encodeThumbhash(ctx, pool, logger, kind, id, src, false)
}

// setThumbhash overwrites the stamp. Only the forced re-render path
// calls it — see fanToLadder.
func setThumbhash(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, kind string, id uuid.UUID, src image.Image) {
	encodeThumbhash(ctx, pool, logger, kind, id, src, true)
}

func encodeThumbhash(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, kind string, id uuid.UUID, src image.Image, overwrite bool) {
	if pool == nil || src == nil || id == uuid.Nil {
		return
	}
	tb := thumbhash.EncodeImage(src)
	if len(tb) == 0 {
		return
	}
	q := assets.New(pool)
	var err error
	if overwrite {
		err = q.SetAssetThumbhash(ctx, assets.SetAssetThumbhashParams{
			ID:        pgtype.UUID{Bytes: id, Valid: true},
			Thumbhash: tb,
		})
	} else {
		err = q.SetAssetThumbhashIfMissing(ctx, assets.SetAssetThumbhashIfMissingParams{
			ID:        pgtype.UUID{Bytes: id, Valid: true},
			Thumbhash: tb,
		})
	}
	if err != nil {
		logAttrs(logger, ctx, slog.LevelDebug, "preview."+kind+".thumbhash_stamp_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

// logAttrs is a nil-safe slog shim. Several handlers are constructed
// bare in tests with no Logger.
func logAttrs(l *slog.Logger, ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) {
	if l == nil {
		return
	}
	l.LogAttrs(ctx, level, msg, attrs...)
}
