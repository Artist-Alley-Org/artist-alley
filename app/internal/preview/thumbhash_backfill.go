// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// JobTypeThumbhashBackfill sweeps assets that have a rendered preview
// in storage but no thumbhash, and stamps one from the rendered bytes.
const JobTypeThumbhashBackfill jobs.JobType = "preview.thumbhash_backfill"

// ThumbhashBackfillIdempotencyKey keys the boot-time enqueue so a
// restart loop can't stack duplicate sweeps. The jobs table's partial
// UNIQUE index only covers in-flight rows, so a finished sweep is
// re-enqueueable on the next boot — which is what makes this a safety
// net rather than a one-shot migration.
const ThumbhashBackfillIdempotencyKey = "preview.thumbhash_backfill.sweep"

// thumbhashBackfillBatch is the keyset page size. The per-asset work is
// one small storage read plus a sub-millisecond encode, so the page is
// sized for a responsive cancel/lease boundary rather than for
// throughput.
const thumbhashBackfillBatch = 200

// ThumbhashBackfillPayload is the (empty) job body. The population is
// derived from the database every run — there is nothing to carry.
type ThumbhashBackfillPayload struct{}

// ThumbhashBackfillResult is what lands in jobs.result.
//
// Stamped counts assets whose rendered preview decoded and was handed
// to the writer — not rows the UPDATE actually touched. The two are the
// same here because the population selects `thumbhash IS NULL`, and the
// writer's own NULL guard can only turn a write into a no-op if
// something else stamped the row mid-sweep. Reading the row count back
// would cost a round trip per asset to sharpen a progress number.
type ThumbhashBackfillResult struct {
	Scanned   int64   `json:"scanned"`
	Stamped   int64   `json:"stamped"`
	Failed    int64   `json:"failed"`
	DurationS float64 `json:"duration_s"`
}

// ThumbhashBackfillHandler stamps assets.thumbhash for assets whose
// preview was rendered from a NON-IMAGE source (#645).
//
// WHY A SEPARATE SWEEP AND NOT A PREVIEW RE-QUEUE. Re-running the
// preview job would work — the ladder step now stamps the hash — but it
// costs a three.js turntable, an ffmpeg waveform render or a Ghostscript
// rasterise per asset to recover 30 bytes that can be derived from
// output already sitting in storage. This reads that output back
// instead: one small GET, one decode, one UPDATE.
//
// WHICH VARIANT IT READS. Not `col`, even though `col` is what the
// population query keys on — see loadRenderedLadderImage, which owns
// that choice and is shared with the per-asset heal on the preview
// handlers' skip path (#827), so the two can never disagree about which
// rung best represents an asset.
//
// KNOWN LIMIT, and the reason that per-asset heal exists: the population
// below is DB-first. On an install whose storage_variants rows went
// missing while the bytes survived, this sweep selects nothing at all —
// there is no `col` row to match — and reports a clean run over an
// install where every asset needs it.
//
// Idempotent and re-runnable: the population excludes anything that
// already has a hash, and the write is SetAssetThumbhashIfMissing,
// whose WHERE clause has its own NULL guard. A second run stamps
// nothing.
type ThumbhashBackfillHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	// MaxVariantBytes caps the read of a rendered preview. Rungs are
	// small by construction (the largest default is 4096px webp);
	// the cap is a guard against a corrupt length, not a policy.
	MaxVariantBytes int64
}

// NewThumbhashBackfillHandler is the recommended constructor.
func NewThumbhashBackfillHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *ThumbhashBackfillHandler {
	return &ThumbhashBackfillHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxVariantBytes: 64 * 1024 * 1024,
	}
}

// Type implements jobs.Handler.
func (h *ThumbhashBackfillHandler) Type() jobs.JobType { return JobTypeThumbhashBackfill }

// Handle implements jobs.Handler. Walks the population in keyset pages
// and stamps each asset it can decode.
//
// A per-asset failure (variant gone from the backend, undecodable
// bytes) is counted and skipped, never fatal: one bad object must not
// strand the other 617. Only a query failure aborts the run, and the
// job system retries that.
func (h *ThumbhashBackfillHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()
	result := ThumbhashBackfillResult{}

	// The keyset cursor is what makes this terminate. Re-querying the
	// "needs a thumbhash" predicate without one would loop forever the
	// moment a whole page failed to decode — the predicate would keep
	// returning the same rows.
	var cursor pgtype.UUID
	for {
		page, err := h.listPage(ctx, cursor, thumbhashBackfillBatch)
		if err != nil {
			return nil, fmt.Errorf("preview.thumbhash_backfill: list page: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			cursor = row.ID
			result.Scanned++
			id := uuid.UUID(row.ID.Bytes)
			src, err := loadRenderedLadderImage(ctx, h.Storage, h.SysConfig, row.FileHash, h.MaxVariantBytes)
			if err != nil {
				result.Failed++
				logAttrs(h.Logger, ctx, slog.LevelWarn, "preview.thumbhash_backfill.load_failed",
					slog.String("asset_id", id.String()),
					slog.String("err", err.Error()))
				continue
			}
			setThumbhashIfMissing(ctx, h.Pool, h.Logger, "thumbhash_backfill", id, src)
			result.Stamped++
		}
		if len(page) < thumbhashBackfillBatch {
			break
		}
	}

	result.DurationS = time.Since(started).Seconds()
	if result.Scanned > 0 {
		logAttrs(h.Logger, ctx, slog.LevelInfo, "preview.thumbhash_backfill.done",
			slog.Int64("scanned", result.Scanned),
			slog.Int64("stamped", result.Stamped),
			slog.Int64("failed", result.Failed),
			slog.Float64("duration_s", result.DurationS))
	}
	return json.Marshal(result)
}

// thumbhashBackfillRow is one candidate asset.
type thumbhashBackfillRow struct {
	ID       pgtype.UUID
	FileHash string
}

// listPage returns the next page of assets that have a rendered `col`
// on record but no thumbhash.
//
// `col` is the population marker because it is the rung every preview
// handler writes — an asset with a `col` row has been through a preview
// pipeline and has renderable output, whatever its source format was.
// The three assets in the #645 measurement with no `col` at all are two
// genuine processing failures and a markdown file (#558); they are
// correctly excluded, because there is nothing to hash.
func (h *ThumbhashBackfillHandler) listPage(ctx context.Context, after pgtype.UUID, limit int32) ([]thumbhashBackfillRow, error) {
	var afterArg any
	if after.Valid {
		afterArg = uuid.UUID(after.Bytes)
	}
	rows, err := h.Pool.Query(ctx, `
		SELECT a.id, a.file_hash
		  FROM assets a
		 WHERE a.deleted_at IS NULL
		   AND a.thumbhash IS NULL
		   AND a.file_hash IS NOT NULL
		   AND ($1::UUID IS NULL OR a.id > $1::UUID)
		   AND EXISTS (
		         SELECT 1 FROM storage_variants sv
		          WHERE sv.object_hash = a.file_hash
		            AND sv.variant_key = 'col'
		       )
		 ORDER BY a.id ASC
		 LIMIT $2
	`, afterArg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]thumbhashBackfillRow, 0, limit)
	for rows.Next() {
		var r thumbhashBackfillRow
		var hash *string
		if err := rows.Scan(&r.ID, &hash); err != nil {
			return nil, err
		}
		if hash != nil {
			r.FileHash = *hash
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Compile-time assertion.
var _ jobs.Handler = (*ThumbhashBackfillHandler)(nil)
