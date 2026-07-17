// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package reindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// JobTypeReindex is the coordinator job type. One row per
// operator-triggered run; the job walks the scope + updates
// progress + short-circuits on cancel.
const JobTypeReindex jobs.JobType = "search.reindex"

// aiEmbedJobType duplicates the constant from
// app/internal/assets/handler.go to avoid a cross-package dep on
// the (much larger) assets subsystem. Documented as part of the
// enqueue bridge contract in that file.
const aiEmbedJobType jobs.JobType = "ai.embed"

// DefaultBatchSize is the fallback rows-per-tick. Sysconfig
// search.reindex_batch_size overrides.
const DefaultBatchSize int32 = 100

// Payload is the JSON body of a reindex job.
type Payload struct {
	RunID uuid.UUID `json:"run_id"`
}

// Counter is the observability hook. Nil-safe.
type Counter interface {
	RecordReindexStart()
	RecordReindexBatch(processed, succeeded, failed int64)
	RecordReindexComplete(result string)    // "completed" | "cancelled" | "failed"
	RecordEmbedHookTriggered(source string) // "reindex" | "boot_backfill"
}

// Job implements jobs.Handler for JobTypeReindex.
type Job struct {
	Pool      *pgxpool.Pool
	Store     *Store
	JobSvc    *jobs.Service
	Logger    *slog.Logger
	Counter   Counter
	BatchSize int32
}

// Type implements jobs.Handler.
func (j *Job) Type() jobs.JobType { return JobTypeReindex }

// Handle walks the scope in batches. Every batch:
//  1. Cancel-probes the run row (short-circuit on cancelled_at)
//  2. Fetches up to BatchSize asset IDs matching the scope past
//     the last cursor
//  3. For target ∈ {embedding, both}, enqueues ai.embed with the
//     shared idempotency key
//  4. For target ∈ {tsvector, both}, calls the entity's
//     rebuild function so the trigger recomputes
//  5. Records progress
//
// Loop exits when the fetch returns fewer than BatchSize rows.
func (j *Job) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	var p Payload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("reindex: parse payload: %w", err)}
	}
	run, err := j.Store.Get(ctx, p.RunID)
	if err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("reindex: load run: %w", err)}
	}
	if j.Counter != nil {
		j.Counter.RecordReindexStart()
	}

	batchSize := j.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	var (
		processed, succeeded, failed int64
		cursor                       *uuid.UUID
	)

	for {
		cancelled, err := j.Store.IsCancelled(ctx, run.ID)
		if err != nil {
			return nil, fmt.Errorf("reindex: cancel probe: %w", err)
		}
		if cancelled {
			if j.Counter != nil {
				j.Counter.RecordReindexComplete("cancelled")
			}
			return jsonMarshalIgnoreErr(map[string]any{
				"cancelled": true, "processed": processed,
			}), nil
		}

		ids, err := j.walkBatch(ctx, run.Scope, cursor, batchSize)
		if err != nil {
			// Persist the last-error string so the admin UI shows
			// the failure without waiting for the job framework's
			// separate error surface.
			if cerr := j.Store.Complete(ctx, run.ID, err.Error()); cerr != nil && j.Logger != nil {
				j.Logger.LogAttrs(ctx, slog.LevelWarn, "reindex.complete_error",
					slog.String("err", cerr.Error()))
			}
			if j.Counter != nil {
				j.Counter.RecordReindexComplete("failed")
			}
			return nil, fmt.Errorf("reindex: walk batch: %w", err)
		}
		if len(ids) == 0 {
			break
		}

		for _, id := range ids {
			ok := j.processAsset(ctx, run, id)
			processed++
			if ok {
				succeeded++
			} else {
				failed++
			}
		}
		cursor = &ids[len(ids)-1]
		if err := j.Store.RecordProgress(ctx, run.ID, processed, succeeded, failed); err != nil && j.Logger != nil {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "reindex.progress_error",
				slog.String("err", err.Error()))
		}
		if j.Counter != nil {
			j.Counter.RecordReindexBatch(int64(len(ids)), succeeded, failed)
		}
		if int32(len(ids)) < batchSize {
			break
		}
	}

	if err := j.Store.Complete(ctx, run.ID, ""); err != nil {
		return nil, fmt.Errorf("reindex: mark complete: %w", err)
	}
	if j.Counter != nil {
		j.Counter.RecordReindexComplete("completed")
	}
	return jsonMarshalIgnoreErr(map[string]any{
		"processed": processed, "succeeded": succeeded, "failed": failed,
	}), nil
}

// processAsset runs the per-asset action for the run's target.
// Returns true on success.
func (j *Job) processAsset(ctx context.Context, run Row, assetID uuid.UUID) bool {
	target := run.Target
	if target == TargetTsvector || target == TargetBoth {
		// Touching updated_at fires the rebuild trigger; the
		// weighted tsvector recomputes without any application-
		// side logic. Cheap on the write path.
		if _, err := j.Pool.Exec(ctx, `
			UPDATE assets SET updated_at = NOW() WHERE id = $1
		`, assetID); err != nil {
			if j.Logger != nil {
				j.Logger.LogAttrs(ctx, slog.LevelWarn, "reindex.tsvector_touch_error",
					slog.String("asset_id", assetID.String()),
					slog.String("err", err.Error()))
			}
			return false
		}
	}
	if target == TargetEmbedding || target == TargetBoth {
		payload := map[string]string{"asset_id": assetID.String()}
		if _, err := j.JobSvc.Enqueue(ctx, aiEmbedJobType, payload, jobs.EnqueueOpts{
			IdempotencyKey: aiEmbedIdempotencyKey(assetID.String(), ""),
		}); err != nil {
			if j.Logger != nil {
				j.Logger.LogAttrs(ctx, slog.LevelWarn, "reindex.embed_enqueue_error",
					slog.String("asset_id", assetID.String()),
					slog.String("err", err.Error()))
			}
			return false
		}
		if j.Counter != nil {
			j.Counter.RecordEmbedHookTriggered("reindex")
		}
	}
	return true
}

// walkBatch returns up to `limit` asset IDs matching the scope
// past `after` (nil = first page). Order is stable by id ASC so
// cursor pagination doesn't miss/dup rows even under concurrent
// deletes.
func (j *Job) walkBatch(ctx context.Context, scope Scope, after *uuid.UUID, limit int32) ([]uuid.UUID, error) {
	sql, args := buildScopeQuery(scope, after, limit)
	rows, err := j.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// buildScopeQuery renders the SQL + args for one batch fetch. Kept
// as a plain function (rather than a *Job method) so tests can pin
// the SQL shape for every scope kind.
func buildScopeQuery(scope Scope, after *uuid.UUID, limit int32) (string, []any) {
	base := `SELECT id FROM assets WHERE deleted_at IS NULL`
	args := []any{}
	nextParam := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	switch scope.Kind {
	case ScopeAssetType:
		base += " AND asset_type = (SELECT ref FROM asset_types WHERE id = " + nextParam(scope.AssetTypeID) + ")"
	case ScopeCollection:
		base += " AND id IN (SELECT asset_id FROM collection_resources WHERE collection_id = " + nextParam(scope.CollectionID) + ")"
	case ScopeEmbeddingModel:
		base += " AND id IN (SELECT asset_id FROM asset_embedding_d768 WHERE provider = " + nextParam(scope.EmbedProvider)
		if scope.EmbedModel != "" {
			base += " AND model = " + nextParam(scope.EmbedModel)
		}
		base += ")"
	case ScopeFederationMissing:
		base += " AND origin_server_id IS NOT NULL AND id NOT IN (SELECT asset_id FROM asset_embedding_d768)"
	}

	if after != nil {
		base += " AND id > " + nextParam(*after)
	}
	base += " ORDER BY id ASC LIMIT " + nextParam(limit)
	return base, args
}

// aiEmbedIdempotencyKey mirrors assets/handler.go verbatim so
// reindex + upload enqueues collide safely on the same asset.
func aiEmbedIdempotencyKey(assetID, model string) string {
	sum := sha256.Sum256([]byte("ai.embed|" + assetID + "|" + model))
	return hex.EncodeToString(sum[:])
}

func jsonMarshalIgnoreErr(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Compile-time assertion.
var _ jobs.Handler = (*Job)(nil)

// keep errors import alive for future error wrapping additions.
var _ = errors.New
