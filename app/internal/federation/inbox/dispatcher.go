// Dispatch worker per the 1.22.D design proposal §2.4.
// Phase 1.22.D-a-4-dispatch.
//
// # Lifecycle
//
// Started in Server.Run alongside the directory poller +
// shares sweeper. Single goroutine; ticks every N seconds
// (default 5). Per tick:
//
//   1. ListPendingInbox(BatchSize) — partial index keeps the
//      working set bounded.
//   2. For each row: resolve the per-verb handler from the
//      registry; invoke it; transition the row to
//      processed / rejected / failed.
//   3. Errors are logged + audited; one bad row does NOT
//      block the rest of the batch.
//
// # Handler contract
//
// HandlerFn takes (ctx, env, peerID, peerURL, row) and returns
// (correlationActivityID, DispatchOutcome, error).
//
// Outcomes:
//   - OutcomeProcessed   — domain write succeeded; row → processed
//   - OutcomeRejected    — domain refused (typed reason); row → rejected
//   - OutcomeFailed      — transient error; row stays pending until
//                          attempts cap hit then terminal-fail
//
// # Per Q1 lock-in
//
// Real handlers ship for `Like` (federation/inbox/handler_like.go)
// and `Create` mapped to comments (handler_comment.go). Every
// other verb gets a no-op stub that records `processed` without
// a domain write. That keeps the demo runnable end-to-end while
// scoped per-domain follow-up phases land the rest.

package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// DispatchOutcome is the typed result of a per-verb handler.
type DispatchOutcome int

const (
	// OutcomeProcessed — handler wrote (or no-op'd) successfully;
	// the inbox row transitions to status='processed'.
	OutcomeProcessed DispatchOutcome = iota

	// OutcomeRejected — handler refused with a typed §12.1
	// reason (e.g. unshared_object from the gate, unknown_object
	// from a missing local row, sig_invalid from a 1.22.I-era
	// crypto failure). Row → rejected; no retry.
	OutcomeRejected

	// OutcomeFailed — transient error. Row stays pending until
	// dispatch_attempts exceeds MaxAttempts; then it terminal-
	// fails and waits for the admin re-queue button.
	OutcomeFailed
)

// HandlerFn is the contract every per-verb handler implements.
type HandlerFn func(ctx context.Context, env *federation.Envelope, peerID uuid.UUID, peerURL string, row FederationInbox) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error)

// DispatcherConfig controls the worker's cadence + batch.
type DispatcherConfig struct {
	// Interval between batches. 5s matches the design §2.4
	// default. Tests override.
	Interval time.Duration

	// BatchSize per tick. Default 100 — matches the §5.5 Q2
	// "1,200 processings/min" envelope at the default 5s
	// interval.
	BatchSize int32

	// MaxAttempts before a transient failure terminal-fails.
	// Default 5 per §5.5 Q3 backoff schedule.
	MaxAttempts int32
}

// DefaultDispatcherConfig returns the boot defaults.
func DefaultDispatcherConfig() DispatcherConfig {
	return DispatcherConfig{
		Interval:    5 * time.Second,
		BatchSize:   100,
		MaxAttempts: 5,
	}
}

// Dispatcher pulls pending federation_inbox rows + invokes the
// per-verb handler. Single goroutine per process; runs alongside
// the directory poller + shares sweeper.
type Dispatcher struct {
	cfg      DispatcherConfig
	pool     *Queries
	registry map[federation.ActivityType]HandlerFn
	logger   *slog.Logger

	// PeerLookupForDispatch is the read-time peer info the
	// handler needs (URL + enabled). Boot wires it to
	// inboxPeerLookupFor — same shape as the inbox-side lookup
	// but called with a known peer_id.
	lookupPeer func(ctx context.Context, peerID uuid.UUID) (PeerInfo, error)

	// social + actorCache are the cross-package contracts the
	// REAL Like + Comment handlers need. Boot wires both via
	// SetSocialPoster + SetRemoteActorUpserter. nil-safe:
	// handlers return OutcomeFailed with a clear error if
	// unwired (loud misconfiguration).
	social     SocialPoster
	actorCache RemoteActorUpserter

	mu      sync.Mutex
	running bool
}

// NewDispatcher wires the worker.
func NewDispatcher(
	cfg DispatcherConfig,
	pool *Queries,
	lookupPeer func(ctx context.Context, peerID uuid.UUID) (PeerInfo, error),
	registry map[federation.ActivityType]HandlerFn,
	logger *slog.Logger,
) *Dispatcher {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.BatchSize <= 0 || cfg.BatchSize > 5000 {
		cfg.BatchSize = 100
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if registry == nil {
		registry = map[federation.ActivityType]HandlerFn{}
	}
	return &Dispatcher{
		cfg:        cfg,
		pool:       pool,
		registry:   registry,
		logger:     logger,
		lookupPeer: lookupPeer,
	}
}

// Run blocks until ctx is cancelled. Safe to call once per
// process; subsequent calls log + return.
func (d *Dispatcher) Run(ctx context.Context) {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		if d.logger != nil {
			d.logger.Warn("inbox.dispatcher: Run called more than once")
		}
		return
	}
	d.running = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.running = false
		d.mu.Unlock()
	}()

	if d.logger != nil {
		d.logger.LogAttrs(ctx, slog.LevelInfo, "inbox.dispatcher.start",
			slog.Duration("interval", d.cfg.Interval),
			slog.Int("batch_size", int(d.cfg.BatchSize)),
		)
	}
	// Run once at startup so rows that landed during downtime
	// don't wait a full interval.
	d.RunOnce(ctx)

	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.RunOnce(ctx)
		}
	}
}

// RunOnce processes a single batch of pending rows. Exported so
// tests can drive deterministically.
func (d *Dispatcher) RunOnce(ctx context.Context) (processed, rejected, failed int) {
	rows, err := d.pool.ListPendingInbox(ctx, d.cfg.BatchSize)
	if err != nil {
		if d.logger != nil {
			d.logger.LogAttrs(ctx, slog.LevelWarn, "inbox.dispatcher.list.error",
				slog.String("err", err.Error()),
			)
		}
		return 0, 0, 0
	}
	if len(rows) == 0 {
		return 0, 0, 0
	}
	for i := range rows {
		row := rows[i]
		switch d.dispatchOne(ctx, row) {
		case OutcomeProcessed:
			processed++
		case OutcomeRejected:
			rejected++
		case OutcomeFailed:
			failed++
		}
	}
	if d.logger != nil && (processed+rejected+failed) > 0 {
		d.logger.LogAttrs(ctx, slog.LevelInfo, "inbox.dispatcher.tick",
			slog.Int("processed", processed),
			slog.Int("rejected", rejected),
			slog.Int("failed", failed),
		)
	}
	return processed, rejected, failed
}

// dispatchOne processes a single row. Returns the outcome so
// the batch loop can tally.
func (d *Dispatcher) dispatchOne(ctx context.Context, row FederationInbox) DispatchOutcome {
	// Parse the captured envelope JSON. If it doesn't parse,
	// something's deeply wrong (the inbox-side stage 8 already
	// validated). Terminal-fail.
	env, err := federation.Unmarshal(row.EnvelopeJson)
	if err != nil {
		d.markFailed(ctx, row, "envelope re-parse failed: "+err.Error())
		return OutcomeFailed
	}

	// Resolve peer (for handler context).
	peerID := uuid.UUID(row.PeerID.Bytes)
	peer, err := d.lookupPeer(ctx, peerID)
	if err != nil {
		d.markFailed(ctx, row, "peer lookup: "+err.Error())
		return OutcomeFailed
	}

	// Find handler. Apply RevokeShare → Unshare normalization
	// per §12.5 #3 before lookup.
	verb := federation.ActivityType(row.ActivityType)
	if verb == federation.ActivityAARevokeShare {
		verb = federation.ActivityAAUnshare
	}
	handler, ok := d.registry[verb]
	if !ok {
		// No handler registered — treat as processed (the row
		// landed durably; no domain write needed for verbs we
		// don't yet implement). Future per-domain phases
		// register their handlers.
		if d.logger != nil {
			d.logger.LogAttrs(ctx, slog.LevelDebug, "inbox.dispatcher.no_handler",
				slog.String("activity_type", row.ActivityType),
				slog.String("inbox_id", uuid.UUID(row.ID.Bytes).String()),
			)
		}
		_, _ = d.pool.MarkInboxProcessed(ctx, MarkInboxProcessedParams{
			ID:                    row.ID,
			CorrelationActivityID: pgtype.UUID{}, // no correlation row
		})
		return OutcomeProcessed
	}

	// Invoke the handler.
	outcome, reason, correlationID, err := handler(ctx, env, peerID, peer.InstanceURL, row)
	switch outcome {
	case OutcomeProcessed:
		corr := pgtype.UUID{}
		if correlationID != (uuid.UUID{}) {
			corr = pgtype.UUID{Bytes: correlationID, Valid: true}
		}
		_, perr := d.pool.MarkInboxProcessed(ctx, MarkInboxProcessedParams{
			ID:                    row.ID,
			CorrelationActivityID: corr,
		})
		if perr != nil {
			d.markFailed(ctx, row, "mark processed: "+perr.Error())
			return OutcomeFailed
		}
		return OutcomeProcessed

	case OutcomeRejected:
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		_, _ = d.pool.MarkInboxRejected(ctx, MarkInboxRejectedParams{
			ID:           row.ID,
			RejectReason: ptrTo(string(reason)),
			LastError:    errMsg,
		})
		return OutcomeRejected

	default: // OutcomeFailed
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		// If attempts have hit the cap, terminal-fail.
		if row.DispatchAttempts+1 >= d.cfg.MaxAttempts {
			d.markFailed(ctx, row, errMsg)
		} else {
			_, _ = d.pool.MarkInboxAttemptFailed(ctx, MarkInboxAttemptFailedParams{
				ID:        row.ID,
				LastError: errMsg,
			})
		}
		return OutcomeFailed
	}
}

func (d *Dispatcher) markFailed(ctx context.Context, row FederationInbox, msg string) {
	_, _ = d.pool.MarkInboxFailedTerminal(ctx, MarkInboxFailedTerminalParams{
		ID:        row.ID,
		LastError: msg,
	})
	if d.logger != nil {
		d.logger.LogAttrs(ctx, slog.LevelWarn, "inbox.dispatcher.terminal_failed",
			slog.String("inbox_id", uuid.UUID(row.ID.Bytes).String()),
			slog.String("err", msg),
		)
	}
}

// Errors a per-verb handler may surface. These map back to
// §12.1 reject reasons at the dispatch layer; the handler
// returns OutcomeRejected with the typed reason + an optional
// error for the audit message.
var (
	ErrUnsharedObject = errors.New("inbox: object not shared with sender")
	ErrInvalidPayload = errors.New("inbox: envelope payload not valid for verb")
)

func ptrTo[T any](v T) *T { return &v }

// keep encoding/json import live for handler files that
// might unmarshal payloads in the same package.
var _ = json.Unmarshal
