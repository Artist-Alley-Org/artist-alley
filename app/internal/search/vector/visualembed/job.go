package visualembed

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
	"github.com/pgvector/pgvector-go"
	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualprovider"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualstore"
)

// JobTypeVisualEmbed is the async upload-hook job. One job per new
// image asset; the fanout in assets.CreateAsset enqueues it after the
// preview + ai.embed dispatches.
const JobTypeVisualEmbed jobs.JobType = "search.visual_embed"

// DefaultRateLimitPerSecond bounds embed calls across all in-flight
// visualembed jobs. Sysconfig search.visual.auto_embed_rate_limit_per_second
// overrides. Set high to disable.
const DefaultRateLimitPerSecond = 5.0

// DefaultRateLimitWaitTimeout caps how long a rate-limited job blocks
// before it gives up and returns a transient error. 5s absorbs bulk-
// upload bursts without letting a genuinely-overwhelmed sidecar wedge
// the worker pool.
const DefaultRateLimitWaitTimeout = 5 * time.Second

// Provider name recorded on the asset_visual_embedding row for later
// provenance tracking. Matches visualbackfill's value so a future
// "which pipeline embedded this?" question is answered by the job
// audit log, not the row.
const providerName = "aa-clip-visual-local"

// Payload is the JSON body of a visualembed job.
type Payload struct {
	AssetID uuid.UUID `json:"asset_id"`
}

// StorageAccessor is the narrow surface the job needs to fetch bytes.
// Interface-shaped so tests substitute a fake without depending on
// storage.Service (mirrors visualbackfill.StorageAccessor).
type StorageAccessor interface {
	Download(ctx context.Context, hash, variant string) (io.ReadCloser, StorageObjectInfo, error)
}

// StorageObjectInfo mirrors storage.ObjectInfo's minimal read surface.
type StorageObjectInfo struct {
	ContentType string
	SizeBytes   int64
}

// AssetLookup is the narrow surface for fetching one asset's
// file_hash + image-eligibility. Interface-shaped so tests substitute
// a fake without depending on visualstore.Queries directly (the
// concrete type is used in production).
type AssetLookup interface {
	Get(ctx context.Context, id uuid.UUID) (AssetRecord, error)
}

// AssetRecord carries the fields the job reads. Missing / non-image
// assets short-circuit as permanent skips (should not have been
// dispatched — this is defense in depth in case guards were bypassed).
type AssetRecord struct {
	FileHash *string
	HasImage bool
	Deleted  bool
}

// Job implements jobs.Handler for JobTypeVisualEmbed.
type Job struct {
	Assets      AssetLookup
	VisualStore *visualstore.Queries
	Storage     StorageAccessor
	Provider    visualprovider.Provider
	Logger      *slog.Logger
	Counter     *Counter
	// RateLimiter is process-shared across every job of this type
	// (single Handler registration means single struct instance).
	// Boot passes a *rate.Limiter seeded from sysconfig. Nil disables
	// rate limiting entirely.
	RateLimiter *rate.Limiter
	// RateLimitWaitTimeout caps how long limiter.Wait blocks. Zero
	// falls back to DefaultRateLimitWaitTimeout.
	RateLimitWaitTimeout time.Duration
}

// Type implements jobs.Handler.
func (j *Job) Type() jobs.JobType { return JobTypeVisualEmbed }

// Handle runs the embed flow for one asset. Retry is delegated to the
// jobs framework — return *jobs.TerminalError for permanent, plain
// error for transient. NO in-handler retry loop (mirrors 1.14.B
// ai.embed pattern per pre-audit Q3).
//
// Error classification:
//   - parse failure / missing asset_id → TerminalError (permanent)
//   - asset lookup miss / deleted / non-image → TerminalError (permanent)
//   - asset missing file_hash → TerminalError (permanent)
//   - storage.Download failure → TerminalError (permanent; the asset
//     row exists but its bytes are gone — retry won't help)
//   - Provider.ErrSidecarUnreachable → transient (framework retries)
//   - Provider.ErrDimMismatch → TerminalError (wrong-model sidecar)
//   - Provider decode error → TerminalError (bad image bytes)
//   - Rate-limit wait timeout → transient
//   - visualstore.UpsertAssetVisualEmbedding failure → transient (DB
//     hiccup; framework retries the whole job)
func (j *Job) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	j.Counter.StartPending()
	defer j.Counter.EndPending()

	var p Payload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		j.Counter.RecordPermanentFailed()
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualembed: parse payload: %w", err)}
	}
	if p.AssetID == uuid.Nil {
		j.Counter.RecordPermanentFailed()
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualembed: asset_id required")}
	}
	if j.Provider == nil {
		// Sidecar wasn't registered at boot but the job was in-flight.
		// Transient by design: an operator who wires the sidecar mid-
		// worker-restart shouldn't lose these jobs.
		j.Counter.RecordTransientFailed()
		return nil, errors.New("visualembed: provider not registered")
	}

	asset, err := j.Assets.Get(ctx, p.AssetID)
	if err != nil {
		j.Counter.RecordPermanentFailed()
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualembed: asset lookup: %w", err)}
	}
	if asset.Deleted {
		j.Counter.RecordPermanentFailed()
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualembed: asset deleted")}
	}
	if !asset.HasImage {
		// Guard bypass — dispatch should have caught this. Terminal so
		// the job doesn't retry on the same fact.
		j.Counter.RecordPermanentFailed()
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualembed: asset is not an image")}
	}
	if asset.FileHash == nil || *asset.FileHash == "" {
		j.Counter.RecordPermanentFailed()
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualembed: asset missing file_hash")}
	}

	// Wait for the shared rate limiter before touching the sidecar.
	// Bounded by RateLimitWaitTimeout; if the sidecar is overwhelmed
	// the job gives up as transient so the framework retries later.
	if j.RateLimiter != nil {
		waitTimeout := j.RateLimitWaitTimeout
		if waitTimeout <= 0 {
			waitTimeout = DefaultRateLimitWaitTimeout
		}
		waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
		waited := false
		if !j.RateLimiter.Allow() {
			waited = true
		}
		if err := j.RateLimiter.Wait(waitCtx); err != nil {
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				j.Counter.RecordTransientFailed()
				return nil, fmt.Errorf("visualembed: rate-limit wait timeout: %w", err)
			}
			// Parent-ctx cancellation is not a persistent failure; the
			// framework will re-dispatch when the worker restarts.
			j.Counter.RecordTransientFailed()
			return nil, fmt.Errorf("visualembed: rate wait: %w", err)
		}
		cancel()
		if waited {
			j.Counter.RecordRateLimitedWait()
		}
	}

	// Fetch bytes from storage.
	rc, _, err := j.Storage.Download(ctx, *asset.FileHash, "original")
	if err != nil {
		j.Counter.RecordPermanentFailed()
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualembed: storage download: %w", err)}
	}
	imageBytes, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		j.Counter.RecordPermanentFailed()
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualembed: read bytes: %w", readErr)}
	}
	if len(imageBytes) == 0 {
		j.Counter.RecordPermanentFailed()
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualembed: empty image bytes")}
	}

	embedding, embedErr := j.Provider.EmbedImage(ctx, imageBytes)
	if embedErr != nil {
		if IsTransientProviderError(embedErr) {
			if j.Logger != nil {
				j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualembed.provider_transient",
					slog.String("asset_id", p.AssetID.String()),
					slog.String("err", embedErr.Error()))
			}
			j.Counter.RecordTransientFailed()
			return nil, fmt.Errorf("visualembed: provider transient: %w", embedErr)
		}
		if j.Logger != nil {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualembed.provider_permanent",
				slog.String("asset_id", p.AssetID.String()),
				slog.String("err", embedErr.Error()))
		}
		j.Counter.RecordPermanentFailed()
		return nil, &jobs.TerminalError{Err: fmt.Errorf("visualembed: provider permanent: %w", embedErr)}
	}

	// Persist via the idempotent upsert. Retries land on the same
	// (asset_id) primary key + refresh model/checkpoint/updated_at.
	vec := pgvector.NewVector(embedding.Vector)
	assetIDPg := pgtype.UUID{Bytes: p.AssetID, Valid: true}
	if err := j.VisualStore.UpsertAssetVisualEmbedding(ctx, visualstore.UpsertAssetVisualEmbeddingParams{
		AssetID:    assetIDPg,
		Column2:    &vec,
		Model:      embedding.Model,
		Checkpoint: embedding.Checkpoint,
		Provider:   providerName,
	}); err != nil {
		if j.Logger != nil {
			j.Logger.LogAttrs(ctx, slog.LevelWarn, "visualembed.upsert_error",
				slog.String("asset_id", p.AssetID.String()),
				slog.String("err", err.Error()))
		}
		j.Counter.RecordTransientFailed()
		return nil, fmt.Errorf("visualembed: upsert: %w", err)
	}

	j.Counter.RecordSuccess()
	result, _ := json.Marshal(map[string]any{
		"asset_id":   p.AssetID.String(),
		"dim":        embedding.Dim,
		"model":      embedding.Model,
		"checkpoint": embedding.Checkpoint,
	})
	return result, nil
}

// IsTransientProviderError classifies provider errors. Sidecar
// unreachability is transient; dim mismatch + decode errors are
// permanent (retry won't fix a wrong-model sidecar or a corrupted
// image byte stream). Exported for the test suite + for the dispatch
// site to reference the same classification.
func IsTransientProviderError(err error) bool {
	return errors.Is(err, visualprovider.ErrSidecarUnreachable)
}

// Compile-time assertion.
var _ jobs.Handler = (*Job)(nil)
