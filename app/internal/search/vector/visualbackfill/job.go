// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visualbackfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualprovider"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualstore"
)

// JobTypeVisualBackfill is the coordinator job type. One row per
// operator-triggered visual-embedding backfill; the job walks image
// assets missing an embedding + short-circuits on cancel.
const JobTypeVisualBackfill jobs.JobType = "search.visual_backfill"

// DefaultBatchSize is the fallback rows-per-tick. Sysconfig
// search.visual.backfill_batch_size overrides.
const DefaultBatchSize int32 = 100

// DefaultRateLimit is the fallback embed calls per second across the
// whole run. CLIP inference is ~100–500 ms on CPU; a conservative
// default keeps a single-instance CPU sidecar out of thrashing while
// still making meaningful progress on large libraries.
const DefaultRateLimit = 5.0

// DefaultRetryCount is the fallback transient-error retry budget per
// asset. One retry covers the common cases (temporary sidecar timeout,
// briefly-unreachable backend) without amplifying persistent failures.
const DefaultRetryCount = 1

// Payload is the JSON body of a visual-backfill job.
type Payload struct {
	RunID uuid.UUID `json:"run_id"`
}

// Counter is the observability hook. Nil-safe. Mirrors the
// reindex.Counter contract so the search subsystem's shared Counter
// can adapt both surfaces onto the /admin/search/health Result map.
type Counter interface {
	RecordVisualBackfillStart()
	RecordVisualBackfillBatch(processed, succeeded, failed int64)
	RecordVisualBackfillComplete(result string) // "completed" | "cancelled" | "failed"
}

// StorageAccessor is the narrow surface the job needs to fetch bytes
// for one asset. Interface-shaped so tests substitute a fake without
// depending on storage.Service.
type StorageAccessor interface {
	Download(ctx context.Context, hash, variant string) (io.ReadCloser, StorageObjectInfo, error)
}

// StorageObjectInfo mirrors storage.ObjectInfo's minimal read surface.
// Callers ignore ContentType in the visual backfill path (the sidecar
// sniffs image bytes via Pillow), but tests can populate it.
type StorageObjectInfo struct {
	ContentType string
	SizeBytes   int64
}

// Job implements jobs.Handler for JobTypeVisualBackfill.
type Job struct {
	Pool        *pgxpool.Pool
	Store       *Store
	VisualStore *visualstore.Queries
	Storage     StorageAccessor
	Provider    visualprovider.Provider
	Logger      *slog.Logger
	Counter     Counter
	BatchSize   int32
	// RateLimitPerSecond bounds embed calls across the whole run so a
	// single backfill doesn't saturate a CPU sidecar. 0 falls back to
	// DefaultRateLimit. Set very high (e.g. 1_000_000) to disable.
	RateLimitPerSecond float64
	// TransientRetryCount is the per-asset retry budget for transient
	// errors (sidecar unreachable, network timeout). Persistent
	// failures (decode error, 400 from sidecar) don't retry. Default
	// DefaultRetryCount.
	TransientRetryCount int32
}

// Type implements jobs.Handler.
func (j *Job) Type() jobs.JobType { return JobTypeVisualBackfill }

// Handle walks the queue. Every batch:
//  1. Cancel-probes the run row (short-circuit on cancelled_at).
//  2. Fetches up to BatchSize image assets lacking a visual embedding.
//  3. For each: fetches bytes from storage, calls Provider.EmbedImage,
//     upserts the embedding.
//  4. Records progress.
//
// Loop exits when the queue is empty. Persistent failures (asset
// missing file_hash, provider decode error) count toward `failed`
// without aborting the whole run; transient errors (provider
// unreachable) trigger the retry budget + then bail out of the whole
// run so the operator sees the failure.
func (j *Job) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	var p Payload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualbackfill: parse payload: %w", err)}
	}
	run, err := j.Store.Get(ctx, p.RunID)
	if err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualbackfill: load run: %w", err)}
	}
	if j.Provider == nil {
		if cerr := j.Store.Complete(ctx, run.ID, "provider not registered"); cerr != nil && j.Logger != nil {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualbackfill.complete_error",
				slog.String("err", cerr.Error()))
		}
		if j.Counter != nil {
			j.Counter.RecordVisualBackfillComplete("failed")
		}
		return nil, &jobs.TerminalError{Err: ErrProviderUnavailable}
	}
	if j.Counter != nil {
		j.Counter.RecordVisualBackfillStart()
	}

	batchSize := j.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	retries := j.TransientRetryCount
	if retries < 0 {
		retries = 0
	}
	rateLimit := j.RateLimitPerSecond
	if rateLimit <= 0 {
		rateLimit = DefaultRateLimit
	}
	limiter := rate.NewLimiter(rate.Limit(rateLimit), 1)

	var processed, succeeded, failed int64
	for {
		cancelled, err := j.Store.IsCancelled(ctx, run.ID)
		if err != nil {
			return nil, fmt.Errorf("visualbackfill: cancel probe: %w", err)
		}
		if cancelled {
			if j.Counter != nil {
				j.Counter.RecordVisualBackfillComplete("cancelled")
			}
			return jsonMarshalIgnoreErr(map[string]any{
				"cancelled": true, "processed": processed,
			}), nil
		}

		queue, err := j.VisualStore.ListImageAssetsNeedingVisualEmbedding(ctx, batchSize)
		if err != nil {
			if cerr := j.Store.Complete(ctx, run.ID, err.Error()); cerr != nil && j.Logger != nil {
				j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualbackfill.complete_error",
					slog.String("err", cerr.Error()))
			}
			if j.Counter != nil {
				j.Counter.RecordVisualBackfillComplete("failed")
			}
			return nil, fmt.Errorf("visualbackfill: list queue: %w", err)
		}
		if len(queue) == 0 {
			break
		}

		for _, row := range queue {
			if err := limiter.Wait(ctx); err != nil {
				return nil, fmt.Errorf("visualbackfill: rate wait: %w", err)
			}
			assetID := uuid.UUID(row.ID.Bytes)
			outcome := j.processAsset(ctx, assetID, row.FileHash, retries)
			processed++
			switch outcome.kind {
			case outcomeSucceeded:
				succeeded++
			case outcomeFailedTransient:
				failed++
				if cerr := j.Store.Complete(ctx, run.ID, outcome.err.Error()); cerr != nil && j.Logger != nil {
					j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualbackfill.complete_error",
						slog.String("err", cerr.Error()))
				}
				if j.Counter != nil {
					j.Counter.RecordVisualBackfillComplete("failed")
				}
				return nil, fmt.Errorf("visualbackfill: transient failure: %w", outcome.err)
			default:
				failed++
			}
		}
		if err := j.Store.RecordProgress(ctx, run.ID, processed, succeeded, failed); err != nil && j.Logger != nil {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualbackfill.progress_error",
				slog.String("err", err.Error()))
		}
		if j.Counter != nil {
			j.Counter.RecordVisualBackfillBatch(int64(len(queue)), succeeded, failed)
		}
		if int32(len(queue)) < batchSize {
			break
		}
	}

	if err := j.Store.Complete(ctx, run.ID, ""); err != nil {
		return nil, fmt.Errorf("visualbackfill: mark complete: %w", err)
	}
	if j.Counter != nil {
		j.Counter.RecordVisualBackfillComplete("completed")
	}
	return jsonMarshalIgnoreErr(map[string]any{
		"processed": processed, "succeeded": succeeded, "failed": failed,
	}), nil
}

// outcome is the per-asset result classification. Persistent failures
// count against `failed` and the loop continues; transient failures
// bail out of the whole run after retries are exhausted so the
// operator sees the sidecar/storage problem.
type outcome struct {
	kind outcomeKind
	err  error
}

type outcomeKind int

const (
	outcomeSucceeded       outcomeKind = 0
	outcomeFailedPermanent outcomeKind = 1
	outcomeFailedTransient outcomeKind = 2
)

// processAsset embeds one asset. Returns the classified outcome.
func (j *Job) processAsset(ctx context.Context, assetID uuid.UUID, fileHash *string, retries int32) outcome {
	if fileHash == nil || *fileHash == "" {
		if j.Logger != nil {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualbackfill.missing_file_hash",
				slog.String("asset_id", assetID.String()))
		}
		return outcome{kind: outcomeFailedPermanent, err: errors.New("asset missing file_hash")}
	}
	// Fetch bytes.
	rc, _, err := j.Storage.Download(ctx, *fileHash, "original")
	if err != nil {
		if j.Logger != nil {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualbackfill.storage_download_error",
				slog.String("asset_id", assetID.String()),
				slog.String("err", err.Error()))
		}
		return outcome{kind: outcomeFailedPermanent, err: fmt.Errorf("storage download: %w", err)}
	}
	imageBytes, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		return outcome{kind: outcomeFailedPermanent, err: fmt.Errorf("read bytes: %w", readErr)}
	}
	if len(imageBytes) == 0 {
		return outcome{kind: outcomeFailedPermanent, err: errors.New("empty image bytes")}
	}

	// Embed with retry budget for transient errors only.
	var (
		embedding visualprovider.Embedding
		embedErr  error
	)
	for attempt := int32(0); attempt <= retries; attempt++ {
		embedding, embedErr = j.Provider.EmbedImage(ctx, imageBytes)
		if embedErr == nil {
			break
		}
		if !isTransientProviderError(embedErr) {
			break
		}
		if attempt < retries {
			// Small linear backoff — the retry budget is tiny so the
			// total wait is bounded regardless of how the operator
			// configures it. Context is honoured.
			select {
			case <-ctx.Done():
				return outcome{kind: outcomeFailedTransient, err: ctx.Err()}
			case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
			}
		}
	}
	if embedErr != nil {
		if isTransientProviderError(embedErr) {
			if j.Logger != nil {
				j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualbackfill.provider_transient",
					slog.String("asset_id", assetID.String()),
					slog.String("err", embedErr.Error()))
			}
			return outcome{kind: outcomeFailedTransient, err: embedErr}
		}
		if j.Logger != nil {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualbackfill.provider_permanent",
				slog.String("asset_id", assetID.String()),
				slog.String("err", embedErr.Error()))
		}
		return outcome{kind: outcomeFailedPermanent, err: embedErr}
	}

	// Persist.
	vec := pgvector.NewVector(embedding.Vector)
	assetIDPg := pgtype.UUID{Bytes: assetID, Valid: true}
	if err := j.VisualStore.UpsertAssetVisualEmbedding(ctx, visualstore.UpsertAssetVisualEmbeddingParams{
		AssetID:    assetIDPg,
		Column2:    &vec,
		Model:      embedding.Model,
		Checkpoint: embedding.Checkpoint,
		Provider:   "aa-clip-visual-local",
	}); err != nil {
		if j.Logger != nil {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualbackfill.upsert_error",
				slog.String("asset_id", assetID.String()),
				slog.String("err", err.Error()))
		}
		return outcome{kind: outcomeFailedPermanent, err: fmt.Errorf("upsert: %w", err)}
	}
	return outcome{kind: outcomeSucceeded}
}

// isTransientProviderError classifies provider errors. Sidecar
// unreachability is transient; dim-mismatch + decode errors are
// permanent (retry won't fix a wrong-model sidecar or a corrupted
// image byte stream).
func isTransientProviderError(err error) bool {
	return errors.Is(err, visualprovider.ErrSidecarUnreachable)
}

func jsonMarshalIgnoreErr(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Compile-time assertion.
var _ jobs.Handler = (*Job)(nil)
