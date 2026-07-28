// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.n16f.net/thumbhash"

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
	// NOTE: no caller sets this today. Its only user was the Blender
	// isometric re-fan, which had to paint over the workbench poster's
	// magenta bytes; that left with Blender in #500. Kept because a
	// second writer to the same ladder is a real scenario the shared
	// primitive should still answer — but if you are reading this
	// looking for the code that overwrites variants, there isn't any.
	Overwrite bool
}

// fanToLadder writes every configured preview variant from one decoded
// image, then stamps the asset's thumbhash if it has none.
//
// The thumbhash runs LAST and UNCONDITIONALLY — including on the
// re-queue path where every rung already exists and nothing is
// encoded. That is deliberate: an asset whose variants were generated
// before #645 shipped has the bytes but not the hash, and re-running
// its preview job is the operator's natural "fix this asset" gesture.
// Skipping the stamp because there was no work to do would make that
// gesture a no-op.
//
// The hash is computed from the FULL source image, not from a rung.
// The card renders a contain rung (CardThumb picks the smallest one —
// square `col` would "flash the wrong shape before swapping"), and the
// placeholder has to match the shape of the picture it fades into. A
// 2048×384 waveform hashed at source produces a wide blur that sits
// correctly in the ~16:3 tile masonry gives it; hashed from `col` it
// would be a square blur letterboxed inside a billboard.
func fanToLadder(ctx context.Context, in ladderInput) error {
	if in.SysConfig != nil {
		if err := fanVariants(ctx, in); err != nil {
			return err
		}
	}
	setThumbhashIfMissing(ctx, in.Pool, in.Logger, in.Kind, in.AssetID, in.Src)
	return nil
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
		if !in.Overwrite && variantOnBackend(ctx, in.Storage, in.Hash, v.Key) {
			continue
		}
		dst := resizeFor(in.Src, v)
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
func variantOnBackend(ctx context.Context, st *storage.Service, hash, key string) bool {
	if st == nil {
		return false
	}
	_, err := st.Backend.Stat(ctx, hash, key)
	return err == nil
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
	if pool == nil || src == nil || id == uuid.Nil {
		return
	}
	tb := thumbhash.EncodeImage(src)
	if len(tb) == 0 {
		return
	}
	if err := assets.New(pool).SetAssetThumbhashIfMissing(ctx, assets.SetAssetThumbhashIfMissingParams{
		ID:        pgtype.UUID{Bytes: id, Valid: true},
		Thumbhash: tb,
	}); err != nil {
		logAttrs(logger, ctx, slog.LevelDebug, "preview."+kind+".thumbhash_backfill_failed",
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
