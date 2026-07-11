// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Outbox dispatcher per the 1.22.D design proposal §3.1 (Option
// B locked-in). Phase 1.22.D-b-3.
//
// # Architecture
//
// The dispatcher is the bridge between the activities ledger
// (source of truth per ADR 0044) and the federation_outbox
// (per-recipient queue). It runs as a single goroutine in
// Server.Run, blocking on LISTEN/NOTIFY for sub-100ms latency
// with a 30s ticker as correctness backstop.
//
// # Per signal/tick
//
//   1. SELECT activities WHERE (created_at, id) > cursor
//      ORDER BY (created_at, id) LIMIT BatchSize.
//   2. For each activity row:
//      - Resolve recipients via outbox.Resolver.
//      - If Skipped: emit federation.emission.skipped audit;
//        advance cursor past the activity; continue.
//      - If Recipients: INSERT one federation_outbox row per
//        recipient via ON CONFLICT DO NOTHING (refinement 4).
//   3. Advance cursor to the LAST processed activity's id
//      atomically with the outbox INSERTs in ONE transaction
//      (refinement 2 — worker crashes mid-batch resume cleanly).
//
// # LISTEN/NOTIFY shape
//
// The Postgres trigger from migration 00005 fires
// pg_notify('federation_dispatch_pending', NEW.id::text) on
// every activities INSERT. The dispatcher's LISTEN goroutine
// just wakes the main loop; the main loop reads from the cursor
// (not the notification payload) so missed/duplicate signals
// don't matter for correctness — they only affect latency.

package outbox

import (
	"context"
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

// DispatcherConfig controls the worker's cadence + batch size.
type DispatcherConfig struct {
	// TickInterval is the ticker-backstop period. Default 30s
	// per design §3.1 — LISTEN/NOTIFY catches 99%; the ticker
	// catches the 1% (connection blip, dropped event).
	TickInterval time.Duration

	// BatchSize per scan. Default 100.
	BatchSize int32
}

// DefaultDispatcherConfig returns the boot defaults.
func DefaultDispatcherConfig() DispatcherConfig {
	return DispatcherConfig{
		TickInterval: 30 * time.Second,
		BatchSize:    100,
	}
}

// SkippedAuditFn is the cross-package hook to audit emission
// refusals. Boot wires it to audit.Recorder.EmissionSkipped.
type SkippedAuditFn func(ctx context.Context, activityID, activityType, objectKind, objectID, visibility, sensitivity, reason string)

// SensitivityLookup resolves the sensitivity tier for an
// activity's target object. Returns ("", nil) when the object
// has no sensitivity column yet (pre-MVP — see schema gap in
// resolver.go). Boot wires it to a per-kind dispatch (currently
// just posts.GetSensitivity-equivalent).
type SensitivityLookup func(ctx context.Context, objectKind string, objectID uuid.UUID) (Sensitivity, error)

// VisibilityLookup resolves the visibility tier for an
// activity's target object. Same shape as SensitivityLookup;
// posts have visibility, comments inherit from their parent
// post, etc.
type VisibilityLookup func(ctx context.Context, objectKind string, objectID uuid.UUID) (Visibility, error)

// Dispatcher fans out activities into federation_outbox rows.
type Dispatcher struct {
	cfg              DispatcherConfig
	pool             *pgxpool.Pool
	q                *Queries
	resolver         *Resolver
	logger           *slog.Logger
	auditSkipped     SkippedAuditFn
	resolveVisibility VisibilityLookup
	resolveSensitivity SensitivityLookup

	// wake is signalled by the LISTEN goroutine whenever a NOTIFY
	// arrives. The main loop drains it before re-entering the
	// scan to coalesce bursts (1 NOTIFY + 1 tick should run
	// the scan ONCE, not twice).
	wake chan struct{}

	mu      sync.Mutex
	running bool
}

// NewDispatcher constructs the dispatcher. Resolver, audit hook,
// + the two lookups are required for production; tests can pass
// nils for the optional ones (audit becomes a no-op; sensitivity
// defaults to SensitivityPublic; visibility defaults to
// VisibilityPrivate so missing-lookup activities don't federate).
func NewDispatcher(
	cfg DispatcherConfig,
	pool *pgxpool.Pool,
	resolver *Resolver,
	logger *slog.Logger,
) *Dispatcher {
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 30 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	return &Dispatcher{
		cfg:      cfg,
		pool:     pool,
		q:        New(pool),
		resolver: resolver,
		logger:   logger,
		wake:     make(chan struct{}, 1),
	}
}

// SetSkippedAudit installs the cross-package audit hook for
// emission refusals. nil-safe (becomes a no-op).
func (d *Dispatcher) SetSkippedAudit(fn SkippedAuditFn) { d.auditSkipped = fn }

// SetVisibilityLookup installs the per-object visibility
// resolver. nil-safe (defaults to private = local-only).
func (d *Dispatcher) SetVisibilityLookup(fn VisibilityLookup) { d.resolveVisibility = fn }

// SetSensitivityLookup installs the per-object sensitivity
// resolver. nil-safe (defaults to public).
func (d *Dispatcher) SetSensitivityLookup(fn SensitivityLookup) { d.resolveSensitivity = fn }

// Run blocks until ctx is cancelled. Safe to call once per
// process; subsequent calls log + return.
func (d *Dispatcher) Run(ctx context.Context) {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		if d.logger != nil {
			d.logger.Warn("outbox.dispatcher: Run called more than once")
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
		d.logger.LogAttrs(ctx, slog.LevelInfo, "outbox.dispatcher.start",
			slog.Duration("tick_interval", d.cfg.TickInterval),
			slog.Int("batch_size", int(d.cfg.BatchSize)),
		)
	}

	// LISTEN goroutine: arms LISTEN federation_dispatch_pending,
	// signals wake on every notification. Survives connection
	// blips via the connect-retry inner loop.
	go d.listenLoop(ctx)

	// Drain at boot so rows landed during downtime don't wait
	// for the first tick or notify.
	d.RunOnce(ctx)

	t := time.NewTicker(d.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.RunOnce(ctx)
		case <-d.wake:
			// Coalesce: drain any extra signals piled up
			// while we were running the previous scan.
			drainWake(d.wake)
			d.RunOnce(ctx)
		}
	}
}

// RunOnce processes a single batch. Exported so tests can drive
// deterministically.
func (d *Dispatcher) RunOnce(ctx context.Context) (processed, skipped, enqueued int) {
	state, err := d.q.GetDispatchState(ctx)
	if err != nil {
		d.logErr(ctx, "outbox.dispatcher.state_read.error", err)
		return 0, 0, 0
	}

	var cursorID pgtype.UUID
	if state.LastDispatchedActivityID.Valid {
		cursorID = state.LastDispatchedActivityID
	}

	rows, err := d.q.ListUndispatchedActivities(ctx, ListUndispatchedActivitiesParams{
		CursorID: cursorID,
		Limit:    d.cfg.BatchSize,
	})
	if err != nil {
		d.logErr(ctx, "outbox.dispatcher.list.error", err)
		return 0, 0, 0
	}
	if len(rows) == 0 {
		return 0, 0, 0
	}

	// Process all rows in a SINGLE transaction so the cursor
	// advance commits atomically with the outbox INSERTs +
	// skipped-audit decisions. If anything in the batch fails
	// we roll the WHOLE batch back; next scan retries from the
	// same cursor.
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		d.logErr(ctx, "outbox.dispatcher.tx_begin.error", err)
		return 0, 0, 0
	}
	defer tx.Rollback(ctx)
	qtx := d.q.WithTx(tx)

	var lastID pgtype.UUID
	for i := range rows {
		row := rows[i]
		lastID = row.ID
		processed++

		// Resolve visibility + sensitivity. Defaults when no
		// lookup is wired: private (local only) + public (no
		// sensitivity refusal). Per-domain handlers register
		// real lookups in 1.22.D-b boot.
		var (
			vis Visibility
			sen Sensitivity
		)
		if d.resolveVisibility != nil && row.ObjectKind != nil && row.ObjectLocalID != nil {
			id, err := uuid.Parse(*row.ObjectLocalID)
			if err == nil {
				if v, err := d.resolveVisibility(ctx, *row.ObjectKind, id); err == nil {
					vis = v
				}
			}
		}
		if vis == "" {
			vis = VisibilityPrivate
		}
		if d.resolveSensitivity != nil && row.ObjectKind != nil && row.ObjectLocalID != nil {
			id, err := uuid.Parse(*row.ObjectLocalID)
			if err == nil {
				if s, err := d.resolveSensitivity(ctx, *row.ObjectKind, id); err == nil {
					sen = s
				}
			}
		}
		if sen == "" {
			sen = SensitivityPublic
		}

		// Build resolver input. AuthorRef can be NULL on the
		// activity row (federation from a remote peer); local-
		// origin rows always have it set.
		var authorRef int64
		if row.ActorUserRef != nil {
			authorRef = *row.ActorUserRef
		}

		var targetID uuid.UUID
		if row.ObjectLocalID != nil {
			if id, err := uuid.Parse(*row.ObjectLocalID); err == nil {
				targetID = id
			}
		}
		var targetKind string
		if row.ObjectKind != nil {
			targetKind = *row.ObjectKind
		}

		result, err := d.resolver.Resolve(ctx, Input{
			Verb:        row.ActivityType,
			TargetKind:  targetKind,
			TargetID:    targetID,
			AuthorRef:   authorRef,
			AuthorURI:   row.ActorUri,
			Visibility:  vis,
			Sensitivity: sen,
		})
		if err != nil {
			d.logErr(ctx, "outbox.dispatcher.resolve.error", err)
			// Tx rollback on resolve error — entire batch
			// retries next scan.
			return 0, 0, 0
		}

		if result.Skipped != "" {
			skipped++
			if d.auditSkipped != nil {
				objectIDStr := ""
				if row.ObjectLocalID != nil {
					objectIDStr = *row.ObjectLocalID
				}
				objectKindStr := ""
				if row.ObjectKind != nil {
					objectKindStr = *row.ObjectKind
				}
				// audit is pool-bound; OK to call outside the
				// tx (the dispatcher's tx is the
				// outbox-insert + cursor-advance unit, not
				// the audit unit).
				d.auditSkipped(ctx,
					uuid.UUID(row.ID.Bytes).String(),
					row.ActivityType,
					objectKindStr,
					objectIDStr,
					string(vis),
					string(sen),
					string(result.Skipped),
				)
			}
			continue
		}

		// Phase 1.22.I-g: denormalize sensitivity onto every outbox
		// row at INSERT time so the delivery Worker can consult
		// outbox.ChoosePathFor without re-touching the activities
		// table. NULL flows through unchanged when the resolver
		// dispatcher has no sensitivity lookup wired (test
		// fixtures) — the Worker treats absence as the same
		// conservative-public default the resolver uses.
		var sensitivityPtr *string
		if sen != "" {
			s := string(sen)
			sensitivityPtr = &s
		}

		// Insert one outbox row per recipient.
		for _, rec := range result.Recipients {
			targetURLPtr := (*string)(nil)
			if rec.TargetUserURL != "" {
				u := rec.TargetUserURL
				targetURLPtr = &u
			}
			_, err := qtx.InsertOutboxRow(ctx, InsertOutboxRowParams{
				ActivityID:    row.ID,
				PeerID:        pgtype.UUID{Bytes: rec.PeerID, Valid: true},
				TargetUserUrl: targetURLPtr,
				Sensitivity:   sensitivityPtr,
			})
			// pgx.ErrNoRows on conflict (RETURNING + DO NOTHING)
			// is the idempotent-no-op signal; not an error.
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				d.logErr(ctx, "outbox.dispatcher.insert.error", err)
				return 0, 0, 0
			}
			enqueued++
		}
	}

	// Atomic cursor advance — fires in the same tx as the
	// inserts so a crash here leaves the cursor unchanged + the
	// batch retries cleanly.
	if err := qtx.AdvanceDispatchCursor(ctx, lastID); err != nil {
		d.logErr(ctx, "outbox.dispatcher.cursor_advance.error", err)
		return 0, 0, 0
	}

	if err := tx.Commit(ctx); err != nil {
		d.logErr(ctx, "outbox.dispatcher.tx_commit.error", err)
		return 0, 0, 0
	}

	if d.logger != nil && processed > 0 {
		d.logger.LogAttrs(ctx, slog.LevelInfo, "outbox.dispatcher.tick",
			slog.Int("processed", processed),
			slog.Int("skipped", skipped),
			slog.Int("enqueued", enqueued),
		)
	}
	return processed, skipped, enqueued
}

// listenLoop arms LISTEN federation_dispatch_pending on a
// dedicated connection. On notify, signals d.wake. Survives
// connection blips via the outer for loop.
func (d *Dispatcher) listenLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := d.listenOnce(ctx); err != nil && d.logger != nil {
			d.logger.LogAttrs(ctx, slog.LevelWarn, "outbox.dispatcher.listen.error",
				slog.String("err", err.Error()),
			)
		}
		// Backoff before reconnect to avoid hot-looping a
		// broken DB.
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (d *Dispatcher) listenOnce(ctx context.Context) error {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN federation_dispatch_pending"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}
	for {
		_, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		// Non-blocking signal. wake is buffered=1 so this is
		// always a fast send.
		select {
		case d.wake <- struct{}{}:
		default:
			// Already pending — coalesce.
		}
	}
}

func drainWake(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func (d *Dispatcher) logErr(ctx context.Context, msg string, err error) {
	if d.logger != nil {
		d.logger.LogAttrs(ctx, slog.LevelError, msg, slog.String("err", err.Error()))
	}
}
