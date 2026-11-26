// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The access-copy worker (#1597, spec v1 §16.7, migration 00105): the
// bounded background half of receiving a version's access copies.
//
// It is a loop of its own inside Worker.Run, started only when the
// worker's Fetcher can also recheck an access request (AccessFetcher;
// production: *casfetch.Client). It has its own goroutines and its own
// limits, so it never takes an original import's slot, and nothing in
// the original path (Schedule, Process, RunOnce, complete) waits for it,
// writes for it or queues it.
//
// # Limits
//
//   - AccessMaxOutstanding: admitted, undecided work rows at once, across
//     every process (federation_received_access_work).
//   - AccessGlobalConcurrency: claimed rows at once, across every process.
//   - AccessConcurrency: transfers this process runs at once.
//   - AccessDiscoveryPage: access entries examined per discovery pass.
//   - AccessMaxAttempts: attempts per entry before a transient failure is
//     recorded as the entry's outcome.
//
// The cross-process bounds are counted under a transaction-level
// try-advisory lock (namespace 1597: "access admission", "access claim"):
// a process that does not get it skips the pass instead of waiting.
//
// # Invariants
//
//   - Candidates and outcomes are the received file rows. A work row
//     exists only for an admitted entry that is not decided yet, and goes
//     when it is decided (verified, or a failure reason) or stale.
//   - Admission takes only an access entry that is not verified and was
//     never attempted (no access_failure_reason), of a current complete
//     record bound for this instance, from an enabled, connected peer with
//     all four derived-transfer-1 profiles and a live access-copy grant
//     (asked before any request). Preservation entries are never work.
//     Discovery reads one keyset page of the unattempted access entries
//     (a partial index) per pass and moves on, wrapping at the end, so an
//     entry beyond the bound is considered on a later pass, and an entry
//     whose grant arrives later is found then.
//   - Every write that decides a claimed row (the verified copy, a failure
//     reason, a reschedule, a release, a drop) runs in one transaction
//     that first locks the row with its claim token FOR UPDATE (after the
//     asset lock, record and file row where it records: the order
//     complete and Withdraw use, and the order a cascade takes), holds it
//     until commit, and only then reads the database's current time to
//     judge expiry (AccessClaim.fence), so a claim that expired while the
//     transaction waited writes nothing.
//   - An attempt starts only with a live claim, and runs under its own
//     context: the heartbeat extends only this token's unexpired claim,
//     and when it finds the claim lost (or cannot renew it before its
//     local deadline) the attempt is cancelled at once and decides
//     nothing. Only an orderly stop releases the row, if still held.
//   - Only timeout, transport_error, HTTP 429/5xx and lease_lost are
//     retried, with the original worker's capped backoff, until
//     AccessMaxAttempts; every other failure (no grant, origin refusal,
//     verification, size) is recorded on the file at once. A claim taken
//     with its attempts already spent (a crash on the last one) is the
//     decision (AccessClaim.Exhausted, computed from the attempts before
//     the claim, so it holds at the 100 cap). An attempt that cannot read
//     its own work decides nothing from a guess: it is deferred, not
//     counted. A record that is no longer the current complete version
//     (superseded, withdrawn) gets no file result; its work row is
//     dropped.
//   - The transfer's temporary ingest pin is released on every exit that
//     does not consume it, with a bounded context that is not the
//     caller's (casfetch.Client.Reconcile is the backstop after a crash).
//   - The last network step before recording is
//     casfetch.Client.RecheckAccess: the exact access request, signed,
//     at the registered origin, and the live access grant (§8.5).
//
// Shared bytes: casfetch keys an access transfer's in-process claim and
// database lease in their own namespace (casfetch leaseKeyFor), so an
// original import of the same peer, asset, bytes and signer never waits
// behind an access transfer, nor the reverse; access transfers of the
// same bytes (other versions) still share one transfer.
package assetingest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
)

// AccessFetcher is a Fetcher that can also recheck an access request at
// the last network boundary before recording (casfetch.Client).
type AccessFetcher interface {
	Fetcher
	RecheckAccess(ctx context.Context, req casfetch.Request) error
}

var _ AccessFetcher = (*casfetch.Client)(nil)

// ErrAccessUnavailable: the worker's Fetcher cannot recheck an access
// request, so no access copy is fetched.
var ErrAccessUnavailable = errors.New("assetingest: the fetcher cannot recheck access requests")

// errStaleClaim: the claim lapsed or was replaced; nothing was written.
var errStaleClaim = errors.New("assetingest: the access claim is no longer held")

// Access work outcomes (AccessResult.Outcome).
const (
	AccessVerified    = "verified"    // the copy was recorded under its record
	AccessDuplicate   = "duplicate"   // the copy was already verified; work dropped
	AccessFailed      = "failed"      // a failure reason was recorded on the file
	AccessRescheduled = "rescheduled" // a transient failure; due again later
	AccessDropped     = "dropped"     // the record is no longer current; no file result
	AccessStale       = "stale"       // the claim lapsed; nothing written
	AccessReleased    = "released"    // stopped (shutdown); due again, attempt not counted
)

const (
	accessAdmissionLock = "access admission"
	accessClaimLock     = "access claim"
	accessLockSpace     = 1597
	accessPinRelease    = 30 * time.Second
	accessStalePurge    = 100
)

// AccessClaim is one claimed access work row.
type AccessClaim struct {
	RecordID uuid.UUID
	Position int
	Token    uuid.UUID
	Attempts int32 // including this one (saturates at 100)
	// Exhausted: the row's attempts were already spent when it was
	// claimed (an earlier attempt stopped with no decision, a crash on the
	// last attempt). It is decided, not tried again.
	Exhausted bool
	// claimedAt is this process's clock when the claim was taken: the
	// local bound on the claim's life until a heartbeat extends it.
	claimedAt time.Time
}

// AccessResult is what running one claim decided.
type AccessResult struct {
	RecordID uuid.UUID
	Position int
	Outcome  string
	Reason   string
}

func (w *Worker) accessFetcher() (AccessFetcher, bool) {
	af, ok := w.svc.d.Fetcher.(AccessFetcher)
	return af, ok
}

func (w *Worker) wakeAccess() {
	select {
	case w.accessWake <- struct{}{}:
	default:
	}
}

// runAccess is the access loop (Run starts it): admit, claim what this
// process has room for, run each claim on its own goroutine.
func (w *Worker) runAccess(ctx context.Context) {
	slots := make(chan struct{}, w.cfg.AccessConcurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	t := time.NewTicker(w.cfg.AccessInterval)
	defer t.Stop()
	for {
		if _, err := w.AdmitAccessWork(ctx); err != nil && ctx.Err() == nil {
			w.log(ctx, slog.LevelWarn, "assetingest.access: admit", "error", err)
		}
		if free := cap(slots) - len(slots); free > 0 && ctx.Err() == nil {
			claims, err := w.ClaimAccessWork(ctx, free)
			if err != nil && ctx.Err() == nil {
				w.log(ctx, slog.LevelWarn, "assetingest.access: claim", "error", err)
			}
			for _, c := range claims {
				slots <- struct{}{}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-slots; w.wakeAccess() }()
					if _, err := w.RunAccessClaim(ctx, c); err != nil && ctx.Err() == nil {
						w.log(ctx, slog.LevelWarn, "assetingest.access: run", "record_id", c.RecordID.String(), "position", c.Position, "error", err)
					}
				}()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.accessWake:
		}
	}
}

// RunAccessOnce admits, claims up to AccessConcurrency rows, runs them
// concurrently and returns what each decided. For tests and operators;
// production uses Run.
func (w *Worker) RunAccessOnce(ctx context.Context) ([]AccessResult, error) {
	if _, err := w.AdmitAccessWork(ctx); err != nil {
		return nil, err
	}
	claims, err := w.ClaimAccessWork(ctx, w.cfg.AccessConcurrency)
	if err != nil {
		return nil, err
	}
	out := make([]AccessResult, len(claims))
	errs := make([]error, len(claims))
	var wg sync.WaitGroup
	for i, c := range claims {
		wg.Add(1)
		go func() { defer wg.Done(); out[i], errs[i] = w.RunAccessClaim(ctx, c) }()
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

// accessCandidate is one examined entry of a discovery page.
type accessCandidate struct {
	recordID       uuid.UUID
	position       int
	eligible       bool
	peerID         uuid.UUID
	originAssetURI string
}

// AdmitAccessWork runs one discovery pass and admits what the outstanding
// bound has room for; it returns how many rows it admitted. It does
// nothing (0, nil) when another process holds the admission lock.
func (w *Worker) AdmitAccessWork(ctx context.Context) (int, error) {
	if _, ok := w.accessFetcher(); !ok {
		return 0, ErrAccessUnavailable
	}
	signer := w.signer()
	if signer.KeyID == "" {
		return 0, nil
	}
	cands, err := w.discoverAccess(ctx, signer.KeyID)
	if err != nil {
		return 0, err
	}
	// The access grant, before any request and outside any transaction.
	var admit []accessCandidate
	for _, c := range cands {
		if !c.eligible {
			continue
		}
		if _, err := w.svc.d.Grants.Allows(ctx, inboundgrants.Query{PeerID: c.peerID, ObjectURI: c.originAssetURI,
			Class: inboundgrants.ClassAccess}); err != nil {
			if errors.Is(err, inboundgrants.ErrNoGrant) {
				continue
			}
			return 0, err
		}
		admit = append(admit, c)
	}
	admitted := 0
	err = pgx.BeginFunc(ctx, w.svc.d.Pool, func(tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, $2))`,
			accessAdmissionLock, accessLockSpace).Scan(&got); err != nil || !got {
			return err
		}
		// Stale rows first (record no longer current, file decided), a
		// bounded number per pass, so they never hold the bound.
		if _, err := tx.Exec(ctx, `WITH stale AS (
			  SELECT q.received_asset_id, q.position FROM federation_received_access_work q
			    JOIN federation_received_assets r ON r.id = q.received_asset_id
			    JOIN federation_received_asset_files f ON f.received_asset_id = q.received_asset_id AND f.position = q.position
			   WHERE r.state <> 'complete' OR f.state <> 'not_transferred' OR f.access_failure_reason IS NOT NULL
			   LIMIT $1)
			DELETE FROM federation_received_access_work w USING stale
			 WHERE w.received_asset_id = stale.received_asset_id AND w.position = stale.position`, accessStalePurge); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM federation_received_access_work`).Scan(&n); err != nil {
			return err
		}
		for _, c := range admit {
			if n+admitted >= w.cfg.AccessMaxOutstanding {
				break
			}
			tag, err := tx.Exec(ctx, `INSERT INTO federation_received_access_work (received_asset_id, position)
				VALUES ($1, $2) ON CONFLICT DO NOTHING`, c.recordID, c.position)
			if err != nil {
				return err
			}
			admitted += int(tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if admitted > 0 {
		w.wakeAccess()
	}
	return admitted, nil
}

// discoverAccess reads the next keyset page of unattempted access entries
// after the cursor and advances it (wrapping at the end), marking each
// eligible or not by its record, peer, profiles and signer.
func (w *Worker) discoverAccess(ctx context.Context, instanceKey string) ([]accessCandidate, error) {
	profiles, err := json.Marshal(federation.DerivedTransferProfiles)
	if err != nil {
		return nil, err
	}
	w.accessMu.Lock()
	afterID, afterPos := w.accessCursorID, w.accessCursorPos
	w.accessMu.Unlock()
	rows, err := w.svc.d.Pool.Query(ctx, `WITH page AS (
		  SELECT received_asset_id, position FROM federation_received_asset_files
		   WHERE role = 'access' AND state = 'not_transferred' AND access_failure_reason IS NULL
		     AND (received_asset_id, position) > ($1::uuid, $2::integer)
		   ORDER BY received_asset_id, position LIMIT $3)
		SELECT page.received_asset_id, page.position,
		       (r.state = 'complete' AND p.enabled AND p.status = 'connected' AND p.capabilities @> $4::jsonb
		        AND r.signer_key_id = $5 AND r.signer_actor_uri IS NULL
		        AND NOT EXISTS (SELECT 1 FROM federation_received_access_work q
		                         WHERE q.received_asset_id = page.received_asset_id AND q.position = page.position)),
		       r.peer_id, r.origin_asset_uri
		  FROM page
		  JOIN federation_received_assets r ON r.id = page.received_asset_id
		  JOIN federation_peers p ON p.id = r.peer_id
		 ORDER BY page.received_asset_id, page.position`,
		afterID, afterPos, w.cfg.AccessDiscoveryPage, string(profiles), instanceKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []accessCandidate
	for rows.Next() {
		var c accessCandidate
		if err := rows.Scan(&c.recordID, &c.position, &c.eligible, &c.peerID, &c.originAssetURI); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	w.accessMu.Lock()
	if len(out) < w.cfg.AccessDiscoveryPage {
		w.accessCursorID, w.accessCursorPos = uuid.Nil, -1
	} else {
		last := out[len(out)-1]
		w.accessCursorID, w.accessCursorPos = last.recordID, last.position
	}
	w.accessMu.Unlock()
	return out, nil
}

// ClaimAccessWork takes up to n due rows for a lease, within the
// cross-process concurrency bound. It returns none when another process
// holds the claim lock.
func (w *Worker) ClaimAccessWork(ctx context.Context, n int) ([]AccessClaim, error) {
	if _, ok := w.accessFetcher(); !ok {
		return nil, ErrAccessUnavailable
	}
	var out []AccessClaim
	err := pgx.BeginFunc(ctx, w.svc.d.Pool, func(tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, $2))`,
			accessClaimLock, accessLockSpace).Scan(&got); err != nil || !got {
			return err
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM federation_received_access_work
			WHERE claim_token IS NOT NULL AND claim_expires_at > clock_timestamp()`).Scan(&active); err != nil {
			return err
		}
		k := min(n, w.cfg.AccessGlobalConcurrency-active)
		if k <= 0 {
			return nil
		}
		claimedAt := time.Now()
		// The bounded pick is a CTE, evaluated once: a sub-select under
		// IN may be re-run per outer row, and LIMIT would not bound it.
		// spent is the attempts the row had before this claim: at the
		// bound, the claim is the decision (Exhausted), not another try,
		// and consumes no attempt (the count stays as it was).
		rows, err := tx.Query(ctx, `WITH picked AS (
			  SELECT received_asset_id, position, attempts AS spent FROM federation_received_access_work
			   WHERE next_attempt_at <= clock_timestamp() AND (claim_token IS NULL OR claim_expires_at <= clock_timestamp())
			   ORDER BY next_attempt_at, received_asset_id, position LIMIT $1 FOR UPDATE SKIP LOCKED)
			UPDATE federation_received_access_work q
			   SET claim_token = gen_random_uuid(), claim_expires_at = clock_timestamp() + make_interval(secs => $2),
			       attempts = CASE WHEN picked.spent >= $3 THEN q.attempts ELSE LEAST(q.attempts + 1, 100) END,
			       updated_at = now()
			  FROM picked
			 WHERE q.received_asset_id = picked.received_asset_id AND q.position = picked.position
			RETURNING q.received_asset_id, q.position, q.claim_token, q.attempts, picked.spent >= $3`,
			k, w.svc.cfg.Lease.Seconds(), w.cfg.AccessMaxAttempts)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c := AccessClaim{claimedAt: claimedAt}
			if err := rows.Scan(&c.RecordID, &c.Position, &c.Token, &c.Attempts, &c.Exhausted); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// fence is the claim check every deciding write runs first, after the
// locks the write needs (asset, record, file, where it records). In two
// statements, like the inbound grant's expiry (inboundgrants.allows): the
// row is locked FOR UPDATE with this token (and held until commit), then
// the database's current time is read once that lock is held, so a claim
// that expired while the transaction waited is refused. Neither now()
// (the transaction's start) nor a clock read inside the locking statement
// would do: both can be taken before the wait.
func (c AccessClaim) fence(ctx context.Context, tx pgx.Tx) error {
	var expires time.Time
	err := tx.QueryRow(ctx, `SELECT claim_expires_at FROM federation_received_access_work
		WHERE received_asset_id = $1 AND position = $2 AND claim_token = $3
		FOR UPDATE`, c.RecordID, c.Position, c.Token).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return errStaleClaim
	}
	if err != nil {
		return err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if !expires.After(now) {
		return errStaleClaim
	}
	return nil
}

// live reports whether c is still the row's unexpired claim, by the
// database's current time (no lock; a starting attempt's check).
func (w *Worker) live(ctx context.Context, c AccessClaim) (bool, error) {
	var ok bool
	err := w.svc.d.Pool.QueryRow(ctx, `SELECT claim_expires_at > clock_timestamp() FROM federation_received_access_work
		WHERE received_asset_id = $1 AND position = $2 AND claim_token = $3`, c.RecordID, c.Position, c.Token).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// settle runs a fenced write on the claimed row alone (no file result):
// drop deletes it; otherwise it is released for later.
func (w *Worker) settle(ctx context.Context, c AccessClaim, q string, args ...any) error {
	return pgx.BeginFunc(ctx, w.svc.d.Pool, func(tx pgx.Tx) error {
		if err := c.fence(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, q, append([]any{c.RecordID, c.Position}, args...)...)
		return err
	})
}

const (
	accessDropSQL       = `DELETE FROM federation_received_access_work WHERE received_asset_id = $1 AND position = $2`
	accessRescheduleSQL = `UPDATE federation_received_access_work
		SET claim_token = NULL, claim_expires_at = NULL, next_attempt_at = now() + make_interval(secs => $3),
		    last_reason = $4, updated_at = now()
		WHERE received_asset_id = $1 AND position = $2`
	// A stopped attempt is not counted: its attempt is refunded ($3) when
	// this claim consumed one (an Exhausted claim consumed none).
	accessReleaseSQL = `UPDATE federation_received_access_work
		SET claim_token = NULL, claim_expires_at = NULL, next_attempt_at = now(),
		    attempts = CASE WHEN $3 THEN GREATEST(attempts - 1, 0) ELSE attempts END, updated_at = now()
		WHERE received_asset_id = $1 AND position = $2`
	// An attempt that could not read its own work (a database failure)
	// decided nothing: it is due again after a backoff ($3 seconds),
	// keeping the last transient reason, with its attempt refunded ($4) as
	// for a release.
	accessDeferSQL = `UPDATE federation_received_access_work
		SET claim_token = NULL, claim_expires_at = NULL, next_attempt_at = now() + make_interval(secs => $3),
		    attempts = CASE WHEN $4 THEN GREATEST(attempts - 1, 0) ELSE attempts END, updated_at = now()
		WHERE received_asset_id = $1 AND position = $2`
	// The heartbeat's renewal, after the fence (renewAccess) locked this
	// token's row and found it unexpired by the database's current time.
	accessHeartbeatSQL = `UPDATE federation_received_access_work
		SET claim_expires_at = clock_timestamp() + make_interval(secs => $4), updated_at = now()
		WHERE received_asset_id = $1 AND position = $2 AND claim_token = $3`
)

// accessTransient names a casfetch failure the access worker retries:
// timeout, transport_error, HTTP 429/5xx and lease_lost.
func accessTransient(err error) (string, bool) {
	var e *casfetch.Error
	if !errors.As(err, &e) {
		return "", false
	}
	switch e.Reason {
	case casfetch.ReasonTimeout, casfetch.ReasonTransport, casfetch.ReasonLeaseLost:
		return e.Reason, true
	case casfetch.ReasonHTTPStatus:
		return e.Reason, e.Status == 429 || e.Status >= 500
	}
	return e.Reason, false
}

// failureReason is the file outcome stored for err: its own reason when
// it is one of AccessFailureReasons, storage_error otherwise.
func failureReason(err error) string {
	r := casfetch.ReasonOf(err)
	if r == "" {
		r = ReasonOf(err)
	}
	for _, ok := range AccessFailureReasons {
		if r == ok {
			return r
		}
	}
	return casfetch.ReasonStorage
}

// RunAccessClaim runs one claimed row to a decision (see the file header):
// it loads the persisted work, fetches the copy as the record's bound
// signer, rechecks the exact request, records it, and releases the
// temporary pin on every exit that does not consume it.
//
// The attempt runs under its own context, a child of ctx. A heartbeat
// extends the claim while it is live; when the claim is found lost (its
// token replaced, or expired by the database's clock), or this process
// cannot renew it before its local deadline, the attempt is cancelled at
// once and decides nothing (AccessStale), so a lost claim stops holding
// its access slot and its transfer. Only an orderly stop of ctx while the
// claim is still held releases the row (fenced).
func (w *Worker) RunAccessClaim(ctx context.Context, c AccessClaim) (AccessResult, error) {
	af, ok := w.accessFetcher()
	if !ok {
		return AccessResult{}, ErrAccessUnavailable
	}
	res := AccessResult{RecordID: c.RecordID, Position: c.Position}
	// A claim that is already stale does no storage or origin work.
	if held, err := w.live(ctx, c); err != nil || !held {
		if err != nil {
			return res, err
		}
		res.Outcome = AccessStale
		return res, nil
	}
	attempt, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	var lost atomic.Bool
	hbCtx, stop := context.WithCancel(ctx)
	var hb sync.WaitGroup
	hb.Add(1)
	go func() {
		defer hb.Done()
		w.heartbeatAccess(hbCtx, c, func() { lost.Store(true); cancelAttempt() })
	}()
	defer func() { stop(); hb.Wait() }()

	outcome, reason, err := w.decideAccess(attempt, af, c, &lost)
	res.Outcome, res.Reason = outcome, reason
	if errors.Is(err, errStaleClaim) {
		res.Outcome, res.Reason, err = AccessStale, "", nil
	}
	return res, err
}

// heartbeatAccess extends c's claim every third of the lease while it is
// live, and calls lose (once) when it is not, or when this process cannot
// show it is before the claim's local deadline (its lease from the last
// renewal this process saw, which only moves forward).
//
// Each renewal is one short transaction bound to that deadline: the pool
// acquisition, the fence (the row locked with this token, then its expiry
// judged by the database's clock once the lock is held) and the update
// with its commit. A renewal that cannot finish before the deadline (a
// busy pool, a lock held on the claim row) is cancelled there and the
// claim is lost; a lock wait never extends an expired claim.
func (w *Worker) heartbeatAccess(ctx context.Context, c AccessClaim, lose func()) {
	lease := w.svc.cfg.Lease
	deadline := c.claimedAt.Add(lease)
	if c.claimedAt.IsZero() {
		deadline = time.Now().Add(lease)
	}
	tick := time.NewTicker(max(lease/3, 10*time.Millisecond))
	defer tick.Stop()
	expiry := time.NewTimer(time.Until(deadline))
	defer expiry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-expiry.C:
			lose()
			return
		case <-tick.C:
		}
		at := time.Now()
		err := w.renewAccess(ctx, c, deadline, lease)
		switch {
		case ctx.Err() != nil:
			return // orderly stop
		case errors.Is(err, errStaleClaim), errors.Is(err, context.DeadlineExceeded), !time.Now().Before(deadline):
			lose()
			return
		case err != nil:
			// Not renewed; the local deadline still stands.
			w.log(ctx, slog.LevelWarn, "assetingest.access: heartbeat", "record_id", c.RecordID.String(), "error", err)
		default:
			if next := at.Add(lease); next.After(deadline) {
				deadline = next
				if !expiry.Stop() {
					select {
					case <-expiry.C:
					default:
					}
				}
				expiry.Reset(time.Until(deadline))
			}
		}
	}
}

// renewAccess is one heartbeat renewal of c, bound to deadline.
func (w *Worker) renewAccess(ctx context.Context, c AccessClaim, deadline time.Time, lease time.Duration) error {
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return pgx.BeginFunc(rctx, w.svc.d.Pool, func(tx pgx.Tx) error {
		if err := c.fence(rctx, tx); err != nil {
			return err
		}
		tag, err := tx.Exec(rctx, accessHeartbeatSQL, c.RecordID, c.Position, c.Token, lease.Seconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errStaleClaim
		}
		return nil
	})
}

// bounded is a context for cleanup that outlives the caller's.
func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), accessPinRelease)
}

// decideAccess is RunAccessClaim's body, under the attempt context ctx;
// lost is set when the heartbeat found the claim lost. It returns the
// outcome, the reason and any error.
func (w *Worker) decideAccess(ctx context.Context, af AccessFetcher, c AccessClaim, lost *atomic.Bool) (string, string, error) {
	// fail records reason on the file and drops the work; a record that
	// is no longer current gets no result.
	fail := func(work AccessWork, reason string) (string, string, error) {
		if reason == "" {
			reason = casfetch.ReasonStorage
		}
		cctx, cancel := bounded(ctx)
		defer cancel()
		var out AccessOutcome
		err := pgx.BeginFunc(cctx, w.svc.d.Pool, func(tx pgx.Tx) error {
			var err error
			if out, err = w.svc.recordAccessFailureTx(cctx, tx, work, reason, c.fence); err != nil {
				return err
			}
			_, err = tx.Exec(cctx, accessDropSQL, c.RecordID, c.Position)
			return err
		})
		switch ReasonOf(err) {
		case "":
			if err != nil {
				return "", reason, err
			}
			if out.Duplicate {
				return AccessDuplicate, "", nil
			}
			return AccessFailed, reason, nil
		case ReasonNotCurrentVersion, ReasonAccessWorkChanged, ReasonWithdrawn:
			return AccessDropped, ReasonOf(err), w.settle(cctx, c, accessDropSQL)
		}
		return "", reason, err
	}
	drop := func(reason string) (string, string, error) {
		cctx, cancel := bounded(ctx)
		defer cancel()
		return AccessDropped, reason, w.settle(cctx, c, accessDropSQL)
	}
	// stopped: the attempt's context ended. A lost claim decides
	// nothing; an orderly stop releases the row if the claim is still
	// held (the fence), with the attempt not counted.
	stopped := func() (string, string, error) {
		if lost.Load() {
			return AccessStale, "", errStaleClaim
		}
		cctx, cancel := bounded(ctx)
		defer cancel()
		return AccessReleased, "", w.settle(cctx, c, accessReleaseSQL, !c.Exhausted)
	}
	// deferWork: the attempt could not read its own work; nothing is
	// decided from a guess. Not counted, due again after a backoff.
	deferWork := func() (string, string, error) {
		cctx, cancel := bounded(ctx)
		defer cancel()
		return AccessRescheduled, "", w.settle(cctx, c, accessDeferSQL, w.svc.retryDelay(max(c.Attempts, 1)).Seconds(), !c.Exhausted)
	}
	// retry reschedules a transient failure, or records it once the
	// attempts are spent.
	retry := func(work AccessWork, reason string) (string, string, error) {
		if int(c.Attempts) >= w.cfg.AccessMaxAttempts {
			return fail(work, reason)
		}
		cctx, cancel := bounded(ctx)
		defer cancel()
		var last any
		if reason != "" {
			last = reason
		}
		return AccessRescheduled, reason, w.settle(cctx, c, accessRescheduleSQL, w.svc.retryDelay(c.Attempts).Seconds(), last)
	}

	work, err := w.svc.LoadAccessWork(ctx, c.RecordID, c.Position)
	if err != nil {
		switch ReasonOf(err) {
		case ReasonNotCurrentVersion, ReasonAccessWorkChanged:
			return drop(ReasonOf(err))
		}
		if ctx.Err() != nil {
			return stopped()
		}
		return deferWork()
	}
	// Already decided (verified, or failed): nothing to fetch.
	var state string
	var decided, lastReason *string
	if err := w.svc.d.Pool.QueryRow(ctx, `SELECT f.state, f.access_failure_reason, q.last_reason
		FROM federation_received_asset_files f
		LEFT JOIN federation_received_access_work q ON q.received_asset_id = f.received_asset_id AND q.position = f.position
		WHERE f.received_asset_id = $1 AND f.position = $2`, c.RecordID, c.Position).Scan(&state, &decided, &lastReason); err != nil {
		if ctx.Err() != nil {
			return stopped()
		}
		return deferWork()
	}
	if state == FileStateVerified {
		out, reason, err := drop("")
		if out == AccessDropped {
			out = AccessDuplicate
		}
		return out, reason, err
	}
	if state != FileStateNotTransferred || decided != nil {
		return drop("decided")
	}
	// A claim whose attempts were spent before it was taken (an earlier
	// attempt stopped with no decision): it is recorded, with the last
	// transient reason if there was one, lease_lost otherwise.
	if c.Exhausted {
		reason := casfetch.ReasonLeaseLost
		if lastReason != nil {
			reason = *lastReason
		}
		return fail(work, reason)
	}
	signer, err := w.resolve(work.SignerKeyID, work.SignerActorURI)
	if err != nil {
		return fail(work, casfetch.ReasonForeignSigner)
	}
	req, err := work.Request(signer)
	if err != nil {
		return fail(work, failureReason(err))
	}
	fetched, err := af.Fetch(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return stopped()
		}
		if reason, ok := accessTransient(err); ok {
			return retry(work, reason)
		}
		return fail(work, failureReason(err))
	}
	consumed := false
	defer func() {
		if consumed {
			return
		}
		cctx, cancel := bounded(ctx)
		defer cancel()
		if err := w.svc.d.Storage.RemovePin(cctx, fetched.Pin, fetched.Hash); err != nil {
			w.log(ctx, slog.LevelWarn, "assetingest.access: release ingest pin", "hash", fetched.Hash, "error", err)
		}
	}()
	// The last network step: the exact request, at the origin, now.
	if err := af.RecheckAccess(ctx, req); err != nil {
		if ctx.Err() != nil {
			return stopped()
		}
		if reason, ok := accessTransient(err); ok {
			return retry(work, reason)
		}
		return fail(work, failureReason(err))
	}
	var out AccessOutcome
	err = pgx.BeginFunc(ctx, w.svc.d.Pool, func(tx pgx.Tx) error {
		var err error
		if out, err = w.svc.recordAccessCopyTx(ctx, tx, work, fetched, c.fence); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, accessDropSQL, c.RecordID, c.Position)
		return err
	})
	if err == nil {
		consumed = out.Consumed
		if out.Duplicate {
			return AccessDuplicate, "", nil
		}
		return AccessVerified, "", nil
	}
	switch reason := ReasonOf(err); {
	case errors.Is(err, errStaleClaim):
		return AccessStale, "", err
	case reason == ReasonNotCurrentVersion || reason == ReasonAccessWorkChanged || reason == ReasonWithdrawn:
		return drop(reason)
	case reason != "":
		return fail(work, failureReason(err))
	case ctx.Err() != nil:
		return stopped()
	}
	return retry(work, "")
}
