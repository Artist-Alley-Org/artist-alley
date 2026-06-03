// Package jobs implements the artist-alley background-job queue.
//
// The queue is generic — any feature package can register a Handler
// for a `type` string and call Service.Enqueue to schedule work.
// The first handler (Phase 1.18.A) is `preview.raster`; follow-on
// phases add video, audio, SVG, PDF, font, 3D, and federation-sync.
//
// Three worker shapes can drain the same queue without any change
// here:
//
//   1. In-process workers, started during app boot (Worker.Run).
//      They use the Handler interface directly.
//
//   2. External render farms hitting the HTTP claim/complete surface
//      (see /jobs/claim in openapi.yaml). The farm receives the same
//      payload shape as Handler.Handle would; it just executes the
//      work on its own metal and POSTs the result back.
//
//   3. Federated peer instances that opt in to help with our jobs.
//      Same HTTP surface as external farms — the peer authenticates
//      with a `worker` API token whose allowed types overlap with
//      our enqueued types.
//
// All three are race-free because ClaimNextJob uses
// `FOR UPDATE SKIP LOCKED` plus a lease (`lease_expires_at`) that a
// watchdog cleans up if a worker dies mid-job.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Type contract
// ---------------------------------------------------------------------------

// JobType is just a string — registered names look like
// "preview.raster", "preview.video", "federation.outbox", etc. We
// don't enum these so a new package can register a type without
// touching this one.
type JobType string

// Common pre-registered job types. Handlers in other packages refer
// to these to avoid stringly-typed misspellings.
const (
	TypePreviewRaster JobType = "preview.raster"
	TypePreviewVector JobType = "preview.vector"
	TypePreviewVideo  JobType = "preview.video"
	TypePreviewAudio  JobType = "preview.audio"
	TypePreviewPDF    JobType = "preview.pdf"
	TypePreviewFont   JobType = "preview.font"
	TypePreview3D     JobType = "preview.3d"
	TypePreviewEbook  JobType = "preview.ebook"
	TypePreviewEPS    JobType = "preview.eps"
	TypePreviewPSD    JobType = "preview.psd"
	TypePreviewComic  JobType = "preview.comic"
	TypePreviewText   JobType = "preview.text"

	// Audiobook background work — async because ffmpeg concat /
	// AAX decryption are minutes-per-hour-of-audio operations
	// that have no business blocking the upload response. Both
	// handlers are stubbed today (Phase B-2 placeholder); the
	// types exist so callers can enqueue + the admin queue page
	// renders the right labels when the implementations land.
	TypeAudiobookMerge   JobType = "audiobook.merge"   // concat N audio files → single .m4b
	TypeAudiobookDecrypt JobType = "audiobook.decrypt" // .aax (Audible) → .m4b using .key

)

// Priority defaults. Lower numbers run sooner. Handlers can override.
const (
	PriorityHigh    = 50
	PriorityNormal  = 100
	PriorityLow     = 200
	PriorityBackfil = 500
)

// Claim is the slim view of a job that a worker actually needs to do
// the work. We don't pass the sqlc-generated `Job` struct around
// because it leaks pgtype.UUID / pgtype.Timestamptz into handler code;
// stripping those out at the boundary keeps the rest of the codebase
// in idiomatic Go types.
type Claim struct {
	ID          uuid.UUID
	Type        JobType
	Payload     json.RawMessage
	Attempts    int32
	MaxAttempts int32
}

// Handler is the contract a feature package implements to process a
// job. The Service routes claimed jobs to the registered Handler for
// the job's type.
//
// Implementations should:
//   - Be idempotent on the payload (retries may re-run the same row).
//   - Hold no per-job mutable state on the Handler struct itself.
//   - Return a result JSON value the admin UI can show.
//   - Return Terminal=true on `error` when the work will NEVER
//     succeed (corrupt file, missing dep) so retries are skipped.
type Handler interface {
	// Type is the dispatch key registered with Service.Register.
	Type() JobType

	// Handle processes the payload. ctx is bounded by the lease;
	// long-running handlers must heartbeat through Service.
	Handle(ctx context.Context, job *Claim) (result json.RawMessage, err error)
}

// TerminalError signals a permanent failure — no retry. Wrap with
// `fmt.Errorf("...: %w", err)` to surface the inner error too.
type TerminalError struct{ Err error }

func (e *TerminalError) Error() string { return "terminal: " + e.Err.Error() }
func (e *TerminalError) Unwrap() error { return e.Err }

// IsTerminal reports whether err is a TerminalError (or wraps one).
func IsTerminal(err error) bool {
	var t *TerminalError
	return errors.As(err, &t)
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry maps JobType → Handler. Handlers register at boot before
// any worker starts.
type Registry struct {
	mu       sync.RWMutex
	handlers map[JobType]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: map[JobType]Handler{}}
}

func (r *Registry) Register(h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[h.Type()] = h
}

// Handler returns (h, true) if a handler is registered for t.
func (r *Registry) Handler(t JobType) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[t]
	return h, ok
}

// Types returns every registered type in a stable order.
func (r *Registry) Types() []JobType {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]JobType, 0, len(r.handlers))
	for t := range r.handlers {
		out = append(out, t)
	}
	return out
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service is the public façade other packages use to interact with
// the queue. It wraps the sqlc Queries and the in-process Registry.
type Service struct {
	Pool     *pgxpool.Pool
	Logger   *slog.Logger
	Registry *Registry

	// LeaseSeconds is how long a claimed row stays leased before the
	// watchdog can requeue it. Workers must heartbeat faster than
	// this. 5 minutes covers most raster jobs comfortably.
	LeaseSeconds int

	// DoneRetention controls how long completed `done` rows stay
	// around for the admin UI before purging. 7 days default.
	DoneRetention time.Duration
}

func NewService(pool *pgxpool.Pool, logger *slog.Logger, reg *Registry) *Service {
	return &Service{
		Pool:          pool,
		Logger:        logger,
		Registry:      reg,
		LeaseSeconds:  300,
		DoneRetention: 7 * 24 * time.Hour,
	}
}

// EnqueueOpts controls non-default fields on a new job.
type EnqueueOpts struct {
	Priority     *int    // default 100 in SQL
	MaxAttempts  *int    // default 3 in SQL
	ScheduledFor *time.Time
}

// Enqueue inserts a new job. Returns the row id.
func (s *Service) Enqueue(ctx context.Context, t JobType, payload any, opts EnqueueOpts) (uuid.UUID, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("jobs: marshal payload: %w", err)
	}
	params := EnqueueJobParams{
		Type:    string(t),
		Payload: b,
	}
	if opts.Priority != nil {
		p := int32(*opts.Priority)
		params.Priority = &p
	}
	if opts.MaxAttempts != nil {
		m := int32(*opts.MaxAttempts)
		params.MaxAttempts = &m
	}
	if opts.ScheduledFor != nil {
		params.ScheduledFor = pgtype.Timestamptz{Time: *opts.ScheduledFor, Valid: true}
	}
	row, err := New(s.Pool).EnqueueJob(ctx, params)
	if err != nil {
		return uuid.Nil, fmt.Errorf("jobs: enqueue: %w", err)
	}
	return uuid.UUID(row.ID.Bytes), nil
}

// ClaimNext atomically claims the next available pending job for
// the given workerID. Returns (nil, nil) when the queue is empty.
//
// scopeTypes filters the search; pass nil to claim from any type.
// Workers usually pass the types they have handlers for so they
// don't try to process work they can't.
func (s *Service) ClaimNext(ctx context.Context, workerID string, scopeTypes []JobType) (*Claim, error) {
	var typeArr []string
	if len(scopeTypes) > 0 {
		typeArr = make([]string, len(scopeTypes))
		for i, t := range scopeTypes {
			typeArr[i] = string(t)
		}
	}
	row, err := New(s.Pool).ClaimNextJob(ctx, ClaimNextJobParams{
		ScopeTypes:   typeArr,
		ClaimedBy:    workerID,
		LeaseSeconds: int32(s.LeaseSeconds),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("jobs: claim: %w", err)
	}
	return &Claim{
		ID:          uuid.UUID(row.ID.Bytes),
		Type:        JobType(row.Type),
		Payload:     row.Payload,
		Attempts:    row.Attempts,
		MaxAttempts: row.MaxAttempts,
	}, nil
}

// ClaimBatch claims up to `limit` pending jobs in one round-trip.
// External farm / federated workers use this to amortise the
// network round-trip when they have N processing slots available.
func (s *Service) ClaimBatch(ctx context.Context, workerID string, scopeTypes []JobType, limit int) ([]Claim, error) {
	if limit <= 0 {
		limit = 1
	}
	if limit > 25 {
		limit = 25
	}
	var typeArr []string
	if len(scopeTypes) > 0 {
		typeArr = make([]string, len(scopeTypes))
		for i, t := range scopeTypes {
			typeArr[i] = string(t)
		}
	}
	rows, err := New(s.Pool).ClaimJobBatch(ctx, ClaimJobBatchParams{
		ScopeTypes:   typeArr,
		ClaimedBy:    workerID,
		LeaseSeconds: int32(s.LeaseSeconds),
		RowLimit:     int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: claim batch: %w", err)
	}
	out := make([]Claim, 0, len(rows))
	for _, r := range rows {
		out = append(out, Claim{
			ID:          uuid.UUID(r.ID.Bytes),
			Type:        JobType(r.Type),
			Payload:     r.Payload,
			Attempts:    r.Attempts,
			MaxAttempts: r.MaxAttempts,
		})
	}
	return out, nil
}

// GetByID fetches a job's current state. Useful for status polling.
func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (Job, error) {
	return New(s.Pool).GetJob(ctx, pgtype.UUID{Bytes: id, Valid: true})
}

// Heartbeat extends the lease on a running job. Returns ErrLeaseLost
// if the lease has already been taken by another worker.
var ErrLeaseLost = errors.New("jobs: lease lost")

func (s *Service) Heartbeat(ctx context.Context, jobID uuid.UUID, workerID string) error {
	n, err := New(s.Pool).HeartbeatJob(ctx, HeartbeatJobParams{
		ID:           pgtype.UUID{Bytes: jobID, Valid: true},
		ClaimedBy:    workerID,
		LeaseSeconds: int32(s.LeaseSeconds),
	})
	if err != nil {
		return fmt.Errorf("jobs: heartbeat: %w", err)
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Complete marks a running job as done. result may be nil.
func (s *Service) Complete(ctx context.Context, jobID uuid.UUID, workerID string, result json.RawMessage) error {
	var resultArg []byte
	if len(result) > 0 {
		resultArg = result
	}
	n, err := New(s.Pool).CompleteJob(ctx, CompleteJobParams{
		ID:        pgtype.UUID{Bytes: jobID, Valid: true},
		ClaimedBy: workerID,
		Result:    resultArg,
	})
	if err != nil {
		return fmt.Errorf("jobs: complete: %w", err)
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Fail records a failure on a running job. If terminal=true the row
// goes to status='failed' regardless of attempts left.
func (s *Service) Fail(ctx context.Context, jobID uuid.UUID, workerID string, errMessage string, terminal bool) error {
	n, err := New(s.Pool).FailJob(ctx, FailJobParams{
		ID:           pgtype.UUID{Bytes: jobID, Valid: true},
		ClaimedBy:    workerID,
		ErrorMessage: errMessage,
		Terminal:     terminal,
	})
	if err != nil {
		return fmt.Errorf("jobs: fail: %w", err)
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// RequeueStuck runs the watchdog query. Safe to call repeatedly.
// Returns the number of rows put back in the queue.
func (s *Service) RequeueStuck(ctx context.Context) (int64, error) {
	n, err := New(s.Pool).RequeueStuckJobs(ctx)
	if err != nil {
		return 0, fmt.Errorf("jobs: requeue stuck: %w", err)
	}
	return n, nil
}

// PurgeOldDone deletes `done` rows older than s.DoneRetention.
func (s *Service) PurgeOldDone(ctx context.Context) (int64, error) {
	days := int32(s.DoneRetention / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	n, err := New(s.Pool).PurgeOldDoneJobs(ctx, days)
	if err != nil {
		return 0, fmt.Errorf("jobs: purge: %w", err)
	}
	return n, nil
}

