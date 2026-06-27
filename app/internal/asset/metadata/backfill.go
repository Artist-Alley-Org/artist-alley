package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// JobTypeBackfill is the canonical job type for the operator-
// initiated re-extract sweep. Distinct from JobTypeExtract — the
// backfill is a coordinator that walks assets in pages + enqueues
// one metadata.extract job per eligible asset.
const JobTypeBackfill jobs.JobType = "metadata.backfill"

// BackfillScope narrows the asset population the backfill walks.
// Empty scope = every active asset (image OR paginated) with a
// file_hash. Filters compose with AND.
type BackfillScope struct {
	// AssetTypeRef limits to one asset type (e.g. the photo type's
	// numeric ref). nil = all asset types. Single-value for
	// historical compatibility; multi-select callers use
	// AssetTypeRefs below.
	AssetTypeRef *int64 `json:"asset_type_ref,omitempty"`

	// AssetTypeRefs is the multi-select extension introduced in
	// Phase 1.18.A-3.B. Empty = no filter (single-select
	// AssetTypeRef still applies if set). When both are populated,
	// AssetTypeRef is folded into the multi-select set + treated as
	// "any of these".
	AssetTypeRefs []int64 `json:"asset_type_refs,omitempty"`

	// FileExtensions narrows by file extension (lowercase, no leading
	// dot). Empty = no filter. Lets operators target a backfill at
	// "just my new raw uploads" or "just my PDFs" — the canonical
	// Phase 1.18.A-3.B use case for the new extractors.
	FileExtensions []string `json:"file_extensions,omitempty"`

	// IncludeNonImage opens the population to non-image assets
	// (PDFs today; comics + ebooks later). Defaults to false so
	// older callers keep their image-only scope behaviour from
	// Phase 1.18.A-2 PR-B.
	IncludeNonImage bool `json:"include_non_image,omitempty"`
}

// effectiveAssetTypeRefs returns the union of AssetTypeRef +
// AssetTypeRefs as a deduplicated slice. Empty result = no
// asset-type filter applies.
func (s BackfillScope) effectiveAssetTypeRefs() []int64 {
	if s.AssetTypeRef == nil && len(s.AssetTypeRefs) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(s.AssetTypeRefs)+1)
	out := make([]int64, 0, len(s.AssetTypeRefs)+1)
	add := func(v int64) {
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if s.AssetTypeRef != nil {
		add(*s.AssetTypeRef)
	}
	for _, v := range s.AssetTypeRefs {
		add(v)
	}
	return out
}

// effectiveExtensions returns the lowercase, no-leading-dot file
// extensions to filter by. Empty result = no extension filter.
func (s BackfillScope) effectiveExtensions() []string {
	if len(s.FileExtensions) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(s.FileExtensions))
	out := make([]string, 0, len(s.FileExtensions))
	for _, e := range s.FileExtensions {
		norm := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
		if norm == "" {
			continue
		}
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	return out
}

// BackfillJobPayload is the JSON shape the worker receives. Tiny —
// the run_id lets the handler read scope + update progress against
// the pre-inserted metadata_backfill_run row.
type BackfillJobPayload struct {
	RunID uuid.UUID `json:"run_id"`
}

// ChildEnqueuer is the closure boot wires the backfill against —
// "given an asset id, enqueue a metadata.extract job for it". Kept
// behind an interface so tests can substitute a counting fake.
type ChildEnqueuer interface {
	EnqueueExtract(ctx context.Context, assetID uuid.UUID) error
}

// BackfillJobHandler walks the asset table in pages, enqueuing one
// extract job per eligible asset + updating the metadata_backfill_
// run row with progress. Stateless; one per process.
//
// Eligibility (PR-B scope):
//   - active (status='active', deleted_at IS NULL)
//   - has a file_hash (NULL means upload pipeline hasn't materialised yet)
//   - has_image = true (we only run EXIF extraction on images right
//     now; broader extractors land in 1.18.A-3)
//   - matches the optional asset_type_ref filter
//
// Cancellation: the handler re-reads the run row on every batch
// boundary and stops early if cancelled_at is set. Caller's POST to
// /admin/metadata-extraction/backfills/{id}/cancel triggers this
// via plain SQL — no in-process signal needed.
type BackfillJobHandler struct {
	pool      *pgxpool.Pool
	enqueuer  ChildEnqueuer
	q         *Queries
	logger    *slog.Logger
	batchSize int32
}

// NewBackfillJobHandler wires the dependencies. batchSize defaults
// to 500 when 0 — small enough to keep one pass under the default
// 5min lease comfortably while big enough to amortise the COUNT(*)
// progress-update overhead.
func NewBackfillJobHandler(pool *pgxpool.Pool, enqueuer ChildEnqueuer, logger *slog.Logger) *BackfillJobHandler {
	return &BackfillJobHandler{
		pool:      pool,
		enqueuer:  enqueuer,
		q:         New(pool),
		logger:    logger,
		batchSize: 500,
	}
}

// WithBatchSize lets tests + operators override the default page.
func (h *BackfillJobHandler) WithBatchSize(n int32) *BackfillJobHandler {
	if n > 0 {
		h.batchSize = n
	}
	return h
}

// Type implements [jobs.Handler].
func (h *BackfillJobHandler) Type() jobs.JobType { return JobTypeBackfill }

// BackfillJobResult is the audit-row payload for the run.
type BackfillJobResult struct {
	RunID     uuid.UUID `json:"run_id"`
	Processed int64     `json:"processed"`
	Succeeded int64     `json:"succeeded"`
	Failed    int64     `json:"failed"`
	Cancelled bool      `json:"cancelled,omitempty"`
}

// Handle implements [jobs.Handler]. Walks the population, enqueues
// child extract jobs, persists progress.
func (h *BackfillJobHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	var p BackfillJobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("metadata.backfill: parse payload: %w", err)}
	}
	pgRunID := pgtype.UUID{Bytes: p.RunID, Valid: true}

	run, err := h.q.GetMetadataBackfillRun(ctx, pgRunID)
	if err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("metadata.backfill: load run: %w", err)}
	}
	if run.CompletedAt.Valid {
		// Re-fired job for a finished run — no-op.
		return jsonMarshalIgnoreErr(BackfillJobResult{
			RunID: p.RunID, Processed: run.Processed,
			Succeeded: run.Succeeded, Failed: run.Failed,
		}), nil
	}

	var scope BackfillScope
	if len(run.Scope) > 0 {
		_ = json.Unmarshal(run.Scope, &scope)
	}

	var (
		processed int64
		succeeded int64
		failed    int64
		lastID    pgtype.UUID // keyset cursor
	)

	for {
		// Cancel-check on every batch boundary.
		fresh, err := h.q.GetMetadataBackfillRun(ctx, pgRunID)
		if err != nil {
			return nil, fmt.Errorf("metadata.backfill: re-read run: %w", err)
		}
		if fresh.CancelledAt.Valid {
			h.persistProgress(ctx, pgRunID, processed, succeeded, failed)
			return jsonMarshalIgnoreErr(BackfillJobResult{
				RunID: p.RunID, Processed: processed,
				Succeeded: succeeded, Failed: failed, Cancelled: true,
			}), nil
		}

		page, err := h.listAssetsPage(ctx, scope, lastID, h.batchSize)
		if err != nil {
			return nil, fmt.Errorf("metadata.backfill: list page: %w", err)
		}
		if len(page) == 0 {
			break
		}

		for _, row := range page {
			processed++
			id := uuid.UUID(row.ID.Bytes)
			if err := h.enqueuer.EnqueueExtract(ctx, id); err != nil {
				failed++
				if h.logger != nil {
					h.logger.LogAttrs(ctx, slog.LevelWarn,
						"metadata.backfill.enqueue_error",
						slog.String("asset_id", id.String()),
						slog.String("err", err.Error()),
					)
				}
				continue
			}
			succeeded++
			lastID = row.ID
		}

		// Persist progress at every batch boundary so the admin UI's
		// polling sees movement; if the worker crashes we lose at
		// most one batch worth of attribution.
		h.persistProgress(ctx, pgRunID, processed, succeeded, failed)

		if int32(len(page)) < h.batchSize {
			break
		}
	}

	if err := h.q.CompleteMetadataBackfillRun(ctx, CompleteMetadataBackfillRunParams{
		ID:        pgRunID,
		Processed: processed,
		Succeeded: succeeded,
		Failed:    failed,
	}); err != nil {
		return nil, fmt.Errorf("metadata.backfill: complete: %w", err)
	}
	return jsonMarshalIgnoreErr(BackfillJobResult{
		RunID:     p.RunID,
		Processed: processed,
		Succeeded: succeeded,
		Failed:    failed,
	}), nil
}

type backfillAssetRow struct {
	ID pgtype.UUID
}

// listAssetsPage returns up to `limit` eligible asset ids past
// `afterID` (keyset pagination on id). Filters by scope.
//
// Three knobs (any combination):
//
//   - asset_type filter: scope.AssetTypeRef + AssetTypeRefs are
//     folded into one IN-list. NULL list = no asset-type filter.
//   - file-extension filter: scope.FileExtensions is matched against
//     assets.file_extension (lowercased on both sides). Empty list =
//     no extension filter.
//   - has_image gate: by default we keep the Phase 1.18.A-2 PR-B
//     image-only behaviour. scope.IncludeNonImage opens up to PDFs
//     + future paginated asset types.
//
// Pure SQL — no dynamic strcat, so the EXPLAIN plan stays stable
// across scope combinations. The COALESCE+ANY-OR-NULL idiom keeps
// the query parameter-only.
func (h *BackfillJobHandler) listAssetsPage(ctx context.Context, scope BackfillScope, afterID pgtype.UUID, limit int32) ([]backfillAssetRow, error) {
	assetTypeFilter := scope.effectiveAssetTypeRefs()
	extensions := scope.effectiveExtensions()
	rows, err := h.pool.Query(ctx, `
		SELECT id FROM assets
		 WHERE status = 'active'
		   AND deleted_at IS NULL
		   AND ($5::BOOLEAN = TRUE OR has_image = TRUE)
		   AND file_hash IS NOT NULL
		   AND ($1::UUID IS NULL OR id > $1::UUID)
		   AND ($2::BIGINT[] IS NULL OR asset_type = ANY($2::BIGINT[]))
		   AND ($3::TEXT[] IS NULL OR LOWER(file_extension) = ANY($3::TEXT[]))
		 ORDER BY id ASC
		 LIMIT $4
	`,
		nullableUUID(afterID),
		nullableInt64Slice(assetTypeFilter),
		nullableStringSlice(extensions),
		limit,
		scope.IncludeNonImage,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]backfillAssetRow, 0, limit)
	for rows.Next() {
		var r backfillAssetRow
		if err := rows.Scan(&r.ID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// nullableInt64Slice returns nil so pgx binds NULL when the input is
// empty — letting the COALESCE-style "IS NULL OR ..." filter become
// a no-op. Returns the slice unchanged otherwise.
func nullableInt64Slice(in []int64) any {
	if len(in) == 0 {
		return nil
	}
	return in
}

// nullableStringSlice is the string analogue. pgx serialises a
// nil-typed any as SQL NULL; a populated []string becomes a
// TEXT[] literal.
func nullableStringSlice(in []string) any {
	if len(in) == 0 {
		return nil
	}
	return in
}

func nullableUUID(id pgtype.UUID) any {
	if !id.Valid {
		return nil
	}
	return uuid.UUID(id.Bytes)
}

// persistProgress writes the running counters. Soft-error — we log
// + keep going; the worst case is the admin UI sees stale progress
// for one batch.
func (h *BackfillJobHandler) persistProgress(ctx context.Context, runID pgtype.UUID, processed, succeeded, failed int64) {
	if err := h.q.UpdateMetadataBackfillRunProgress(ctx, UpdateMetadataBackfillRunProgressParams{
		ID:        runID,
		Processed: processed,
		Succeeded: succeeded,
		Failed:    failed,
	}); err != nil && h.logger != nil {
		h.logger.LogAttrs(ctx, slog.LevelWarn,
			"metadata.backfill.progress_update_error",
			slog.String("err", err.Error()),
		)
	}
}

// Compile-time assertion.
var _ jobs.Handler = (*BackfillJobHandler)(nil)

// ---------------------------------------------------------------------------
// Admin surface — start / get / cancel a backfill run.
// ---------------------------------------------------------------------------

// BackfillStartParams is the operator-facing input.
type BackfillStartParams struct {
	Scope     BackfillScope
	StartedBy *int64 // user_ref; passed through to metadata_backfill_run.started_by_user_ref
}

// BackfillRunRow is the admin-side projection — plain Go types.
type BackfillRunRow struct {
	ID               uuid.UUID
	Scope            BackfillScope
	Total            int64
	Processed        int64
	Succeeded        int64
	Failed           int64
	StartedAt        time.Time
	CompletedAt      *time.Time
	CancelledAt      *time.Time
	StartedByUserRef *int64
}

// StartBackfill inserts the run row + enqueues the coordinator
// job. Returns the new run id; the actual walk happens
// asynchronously in the job worker. Caller is expected to map
// errors → HTTP status.
func (h *AdminHandler) StartBackfill(ctx context.Context, jobSvc *jobs.Service, p BackfillStartParams) (BackfillRunRow, error) {
	if jobSvc == nil {
		return BackfillRunRow{}, errors.New("metadata.AdminHandler.StartBackfill: jobs.Service is nil — extraction subsystem not wired")
	}
	scopeJSON, err := json.Marshal(p.Scope)
	if err != nil {
		return BackfillRunRow{}, fmt.Errorf("metadata.StartBackfill: marshal scope: %w", err)
	}
	row, err := h.q.InsertMetadataBackfillRun(ctx, InsertMetadataBackfillRunParams{
		Scope:            scopeJSON,
		Total:            0, // unknown until the walk gets going; UI shows "—"
		StartedByUserRef: p.StartedBy,
	})
	if err != nil {
		return BackfillRunRow{}, fmt.Errorf("metadata.StartBackfill: insert run: %w", err)
	}
	runID := uuid.UUID(row.ID.Bytes)
	if _, err := jobSvc.Enqueue(ctx, JobTypeBackfill, BackfillJobPayload{RunID: runID}, jobs.EnqueueOpts{}); err != nil {
		return BackfillRunRow{}, fmt.Errorf("metadata.StartBackfill: enqueue: %w", err)
	}
	return backfillRowFromDB(row), nil
}

// GetBackfill fetches one run. ErrFailureNotFound is reused for the
// "no such run" classification — kept consistent with the failures
// surface so the HTTP layer has one 404 sentinel to map.
var ErrBackfillNotFound = errors.New("metadata: backfill run not found")

// GetBackfill returns one run by id. ErrBackfillNotFound when
// missing.
func (h *AdminHandler) GetBackfill(ctx context.Context, id uuid.UUID) (BackfillRunRow, error) {
	pgID := pgtype.UUID{Bytes: id, Valid: true}
	row, err := h.q.GetMetadataBackfillRun(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BackfillRunRow{}, ErrBackfillNotFound
		}
		return BackfillRunRow{}, fmt.Errorf("metadata.GetBackfill: %w", err)
	}
	return backfillRowFromDB(row), nil
}

// ListRecentBackfills returns the most-recently started runs. The
// admin UI uses this as a stream of "what backfills have I kicked
// off lately". Capped at 50 server-side (caller's limit honored
// within [1, 50]).
func (h *AdminHandler) ListRecentBackfills(ctx context.Context, limit int32) ([]BackfillRunRow, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := h.q.ListRecentMetadataBackfillRuns(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("metadata.ListRecentBackfills: %w", err)
	}
	out := make([]BackfillRunRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, backfillRowFromDB(r))
	}
	return out, nil
}

// CancelBackfill flips cancelled_at to NOW(). The running worker
// observes this on its next batch boundary + stops cleanly. No-op
// if already cancelled or completed.
func (h *AdminHandler) CancelBackfill(ctx context.Context, id uuid.UUID) error {
	pgID := pgtype.UUID{Bytes: id, Valid: true}
	var exists bool
	if err := h.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM metadata_backfill_run WHERE id = $1)`,
		id,
	).Scan(&exists); err != nil {
		return fmt.Errorf("metadata.CancelBackfill: probe: %w", err)
	}
	if !exists {
		return ErrBackfillNotFound
	}
	if err := h.q.CancelMetadataBackfillRun(ctx, pgID); err != nil {
		return fmt.Errorf("metadata.CancelBackfill: %w", err)
	}
	return nil
}

func backfillRowFromDB(r MetadataBackfillRun) BackfillRunRow {
	out := BackfillRunRow{
		Total:            r.Total,
		Processed:        r.Processed,
		Succeeded:        r.Succeeded,
		Failed:           r.Failed,
		StartedByUserRef: r.StartedByUserRef,
	}
	if r.ID.Valid {
		out.ID = uuid.UUID(r.ID.Bytes)
	}
	if r.StartedAt.Valid {
		out.StartedAt = r.StartedAt.Time
	}
	if r.CompletedAt.Valid {
		t := r.CompletedAt.Time
		out.CompletedAt = &t
	}
	if r.CancelledAt.Valid {
		t := r.CancelledAt.Time
		out.CancelledAt = &t
	}
	if len(r.Scope) > 0 {
		_ = json.Unmarshal(r.Scope, &out.Scope)
	}
	return out
}
