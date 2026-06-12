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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
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

// SenderEncKeyFunc returns the sender actor's current encryption
// public key (32 bytes) + version number — the values needed for
// nacl/box.Open. Phase 1.22.I-f; wired by boot to
// federation/remote.Handler.GetEncryptionKey. Nil-safe: when
// unwired, encrypted envelopes get rejected with reason
// "sender_key_missing".
type SenderEncKeyFunc func(ctx context.Context, actorURI string) (pubBytes []byte, version int32, err error)

// RecipientUserRefFunc returns the local user_ref for the recipient
// actor URI in envelope.To. The dispatcher feeds that user_ref to
// [DecryptForUser] so the receiver's retained-key set can be walked.
// Phase 1.22.I-f; wired by boot to a function that parses the URI's
// `/users/<username>` path segment + calls auth.FindUserByUsername.
// Nil-safe: when unwired, encrypted envelopes get rejected with
// reason "recipient_unresolvable".
type RecipientUserRefFunc func(ctx context.Context, actorURI string) (int64, error)

// DispatcherConfig controls the worker's cadence + batch.
type DispatcherConfig struct {
	// Interval is the ticker-backstop period. The primary
	// wake signal is LISTEN/NOTIFY on federation_inbox INSERT
	// (per migration 00006); the ticker catches missed
	// notifications under load. Default 30s per the design
	// proposal §3.1 "correctness backstop only" pattern.
	Interval time.Duration

	// BatchSize per tick. Default 100 — matches the §5.5 Q2
	// "1,200 processings/min" envelope target.
	BatchSize int32

	// MaxAttempts before a transient failure terminal-fails.
	// Default 5 per §5.5 Q3 backoff schedule.
	MaxAttempts int32
}

// DefaultDispatcherConfig returns the boot defaults.
//
// Interval = 30s matches the gold-standard correctness-backstop
// pattern locked in by 1.22.D-b-6 G1. The actual responsiveness
// comes from LISTEN/NOTIFY on federation_inbox INSERT;
// production p99 end-to-end is sub-1s in the happy path.
func DefaultDispatcherConfig() DispatcherConfig {
	return DispatcherConfig{
		Interval:    30 * time.Second,
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
	rawPool  *pgxpool.Pool // for the LISTEN goroutine — needs Acquire
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

	// 1.22.I-f stage-4 decrypt-branch hooks. All nil-safe; when
	// any is unwired AND a row arrives encrypted, the dispatcher
	// rejects with reject_reason=decrypt_failed + audits with
	// reason=sender_key_missing / recipient_unresolvable / etc.
	// Plaintext rows are unaffected.
	//
	// rawPool is reused for the receiver-key walk (DecryptForUser
	// needs a *pgxpool.Pool to construct the userkeys.Queries).
	// Test fixtures that exercise the decrypt branch MUST call
	// SetRawPool — there's no fallback. Plaintext-only tests
	// don't need it.
	senderEncKey     SenderEncKeyFunc
	recipientUserRef RecipientUserRefFunc
	audit            *audit.Recorder

	// wake is signalled by the LISTEN goroutine on every
	// federation_inbox_pending notification. Buffered=1 so the
	// LISTEN never blocks; main loop drains extras before
	// re-entering the scan to coalesce bursts. Per 1.22.D-b-6
	// G1: end-to-end p99 sub-1s is the contract; LISTEN is the
	// primary signal, ticker is correctness backstop only.
	wake chan struct{}

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
		wake:       make(chan struct{}, 1),
	}
}

// SetRawPool wires the underlying pgxpool.Pool so the dispatcher
// can run a LISTEN goroutine on federation_inbox_pending. nil-
// safe: when not wired, the dispatcher falls back to ticker-only
// (the cfg.Interval cadence). Production wires this at boot;
// tests that use RunOnce directly can skip it.
//
// Per 1.22.D-b-6 G1: LISTEN/NOTIFY is the load-bearing signal
// for sub-1s end-to-end latency; ticker is correctness backstop
// only at 30s default.
func (d *Dispatcher) SetRawPool(p *pgxpool.Pool) { d.rawPool = p }

// SetSenderEncKey wires the sender-pubkey lookup the stage-4
// decrypt branch needs. Call once at boot AFTER the remote actor
// cache is constructed. Idempotent; passing nil disables
// decryption (encrypted envelopes get rejected with reason
// "sender_key_missing"). Phase 1.22.I-f.
func (d *Dispatcher) SetSenderEncKey(f SenderEncKeyFunc) { d.senderEncKey = f }

// SetRecipientUserRef wires the recipient-actor-URI → local
// user_ref resolver. Call once at boot. Idempotent; passing nil
// disables decryption (encrypted envelopes get rejected with
// reason "recipient_unresolvable"). Phase 1.22.I-f.
func (d *Dispatcher) SetRecipientUserRef(f RecipientUserRefFunc) { d.recipientUserRef = f }

// SetAudit wires the audit recorder for
// federation.inbox.decrypted + federation.inbox.decrypt_failed.
// nil-safe (the decrypt path works without audit, just with no
// observability). Phase 1.22.I-f.
func (d *Dispatcher) SetAudit(rec *audit.Recorder) { d.audit = rec }

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
			slog.Bool("listen_enabled", d.rawPool != nil),
		)
	}

	// LISTEN goroutine — primary wake signal per 1.22.D-b-6
	// G1. Survives connection blips via the inner reconnect
	// loop. nil-safe: when rawPool isn't wired we fall back to
	// ticker-only.
	if d.rawPool != nil {
		go d.listenLoop(ctx)
	}

	// Run once at startup so rows that landed during downtime
	// don't wait a full interval (or a NOTIFY).
	d.RunOnce(ctx)

	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.RunOnce(ctx)
		case <-d.wake:
			// Coalesce: drain any extra signals piled up while
			// we were running the previous scan. Matches the
			// outbox dispatcher's pattern.
			drainInboxWake(d.wake)
			d.RunOnce(ctx)
		}
	}
}

// listenLoop arms LISTEN federation_inbox_pending on a dedicated
// connection. On notify, signals d.wake. Survives connection
// blips via the outer reconnect loop. Per 1.22.D-b-6 G1: this
// is the load-bearing latency primitive — without LISTEN, the
// ticker (30s default) sets the worst-case latency.
func (d *Dispatcher) listenLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := d.listenOnce(ctx); err != nil && d.logger != nil {
			d.logger.LogAttrs(ctx, slog.LevelWarn, "inbox.dispatcher.listen.error",
				slog.String("err", err.Error()),
			)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (d *Dispatcher) listenOnce(ctx context.Context) error {
	conn, err := d.rawPool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN federation_inbox_pending"); err != nil {
		return err
	}
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case d.wake <- struct{}{}:
		default: // already pending; coalesce
		}
	}
}

func drainInboxWake(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
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

// DispatchRow is the test-friendly exported wrapper around
// [dispatchOne]. Integration tests use this to bypass the
// LISTEN/NOTIFY + ListPendingInbox loop so a concurrent
// dispatcher sharing the same DB (e.g., the live app container
// during scripts/test.sh) can't race the row to a different
// terminal state. Production code MUST use [Run] / [RunOnce];
// this helper exists ONLY for the Phase 1.22.I-f decrypt
// integration tests + any future test that needs the same
// isolation guarantee.
//
// The row is processed as if [RunOnce] had picked it up: stage-4
// decrypt branch, peer lookup, verb handler invocation, mark-as-
// processed / mark-as-rejected.
func (d *Dispatcher) DispatchRow(ctx context.Context, row FederationInbox) DispatchOutcome {
	return d.dispatchOne(ctx, row)
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

	// Phase 1.22.I-f stage 4: per-recipient decryption. When the
	// envelope arrived encrypted, unwrap + open against the
	// recipient's current key (or walk the retained-key set if
	// the sender used an older recipient-key version during a
	// rotation grace window). Restoration of env.Extra makes the
	// rest of the pipeline (verb dispatch, handler, MarkInbox*)
	// indifferent to whether this envelope started encrypted.
	var wasEncrypted bool
	var decryptedWithKeyVersion *int32
	if env.Encryption != nil {
		wasEncrypted = true
		decryptedVer, ok := d.tryDecryptInbound(ctx, row, env, peerID)
		if !ok {
			return OutcomeRejected
		}
		decryptedWithKeyVersion = &decryptedVer
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
			ID:                      row.ID,
			CorrelationActivityID:   pgtype.UUID{}, // no correlation row
			WasEncrypted:            wasEncrypted,
			DecryptedWithKeyVersion: decryptedWithKeyVersion,
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
			ID:                      row.ID,
			CorrelationActivityID:   corr,
			WasEncrypted:            wasEncrypted,
			DecryptedWithKeyVersion: decryptedWithKeyVersion,
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

// tryDecryptInbound runs the stage-4 decrypt branch. On success,
// repopulates env.Extra with the decrypted JSON object + returns
// (decryptedWithKeyVersion, true). On failure, marks the row
// rejected with reject_reason=decrypt_failed + fires the
// federation.inbox.decrypt_failed audit + returns (0, false).
//
// # Why this method, not a sequence inlined in dispatchOne
//
// Encrypted-envelope handling has three distinct failure modes
// (recipient unresolvable, sender key missing, no key opened) and
// each maps to a different audit `reason` string + the same
// reject row. Pulling the orchestration out keeps dispatchOne's
// control flow readable.
//
// # Why decryption happens AFTER the peer lookup
//
// The audit row carries peerID metadata + the markRejected* paths
// need peer context for the operator dashboard. The dispatcher
// already has peer in scope by the time we get here; failing
// fast on peer-lookup is the sole reason that stage runs first.
//
// # Why env.Extra is restored from the plaintext
//
// The outbox-side EncryptActivityPayload sealed `json.Marshal(env.Extra)`
// (delivery.go §5–7). The receiver restoring env.Extra from the
// plaintext gives every downstream verb handler the same view it
// would have on a plaintext envelope — they don't need to know
// whether the row arrived encrypted. wasEncrypted is captured for
// the audit + the row's was_encrypted column; the handler stays
// indifferent.
func (d *Dispatcher) tryDecryptInbound(
	ctx context.Context,
	row FederationInbox,
	env *federation.Envelope,
	peerID uuid.UUID,
) (decryptedWithKeyVersion int32, ok bool) {
	block := env.Encryption

	// Pre-flight: the dispatcher must be wired with both
	// resolvers. Boot wires both in production; an encrypted
	// envelope landing on a dispatcher that hasn't been wired is
	// either misconfiguration OR a test-fixture omission. Reject
	// loudly so the operator notices.
	if d.recipientUserRef == nil {
		d.markRejectedDecryptFailed(ctx, row, env, peerID, block,
			"recipient_unresolvable", 0,
			"dispatcher missing recipient user-ref resolver")
		return 0, false
	}
	if d.senderEncKey == nil {
		d.markRejectedDecryptFailed(ctx, row, env, peerID, block,
			"sender_key_missing", 0,
			"dispatcher missing sender enc-key lookup")
		return 0, false
	}
	if d.rawPool == nil {
		d.markRejectedDecryptFailed(ctx, row, env, peerID, block,
			"recipient_unresolvable", 0,
			"dispatcher missing raw pool for receiver-key walk")
		return 0, false
	}

	// 1. Resolve the recipient's local user_ref. The outbox seals
	// against ONE recipient per emission (see delivery.go's
	// per-peer loop) so envelope.To has exactly one entry by the
	// time the inbox sees it; fall back to env.Actor's host's
	// users-tree only if needed.
	recipientURI := firstRecipientURI(env)
	if recipientURI == "" {
		d.markRejectedDecryptFailed(ctx, row, env, peerID, block,
			"recipient_unresolvable", 0,
			"envelope.to is empty")
		return 0, false
	}
	recipientRef, err := d.recipientUserRef(ctx, recipientURI)
	if err != nil {
		d.markRejectedDecryptFailed(ctx, row, env, peerID, block,
			"recipient_unresolvable", 0,
			"recipient resolver: "+err.Error())
		return 0, false
	}

	// 2. Resolve the sender's encryption public key. Cache hit
	// via the I-c remote-actor cache is the common case.
	senderPub, _, err := d.senderEncKey(ctx, env.Actor)
	if err != nil || len(senderPub) == 0 {
		reason := "sender_key_missing"
		errStr := "sender key lookup failed"
		if err != nil {
			errStr = "sender key lookup: " + err.Error()
		}
		d.markRejectedDecryptFailed(ctx, row, env, peerID, block,
			reason, 0, errStr)
		return 0, false
	}

	// 3. Walk the recipient's retained keys + try each one.
	result, err := DecryptForUser(ctx, d.rawPool, recipientRef,
		senderPub, []byte(block.Nonce), []byte(block.Ciphertext))
	if err != nil {
		reason := "no_key_worked"
		switch {
		case errors.Is(err, ErrNoReceiverKey):
			reason = "no_keys_walked"
		case errors.Is(err, ErrSenderKeyMissing):
			reason = "sender_key_missing"
		case errors.Is(err, federation.ErrEncryptionDecryptFailed):
			reason = "no_key_worked"
		}
		d.markRejectedDecryptFailed(ctx, row, env, peerID, block,
			reason, block.RecipientKeyVersion,
			"decrypt: "+err.Error())
		return 0, false
	}

	// 4. Restore env.Extra from the plaintext payload so every
	// downstream verb handler sees the same view it would on a
	// plaintext envelope. Outbox sealed json.Marshal(env.Extra)
	// (delivery.go §5); receiver Unmarshal-into-Extra is the
	// inverse. Empty/null plaintext maps to an empty Extra so
	// handlers that don't read Extra keep working.
	var restored map[string]json.RawMessage
	if len(result.Plaintext) > 0 && string(result.Plaintext) != "null" {
		if err := json.Unmarshal(result.Plaintext, &restored); err != nil {
			d.markRejectedDecryptFailed(ctx, row, env, peerID, block,
				"no_key_worked", block.RecipientKeyVersion,
				"plaintext re-parse: "+err.Error())
			return 0, false
		}
	}
	env.Extra = restored
	env.Encryption = nil // handlers don't need to re-check

	// 5. Audit happy-path.
	if d.audit != nil {
		d.audit.FederationInboxDecrypted(
			ctx, nil,
			peerID.String(),
			string(env.Type),
			env.ID,
			block.SenderKeyVersion,
			result.DecryptedWithKeyVersion,
			result.AttemptCount,
		)
	}

	return result.DecryptedWithKeyVersion, true
}

// markRejectedDecryptFailed transitions the inbox row to
// status=rejected with reject_reason=decrypt_failed + fires the
// federation.inbox.decrypt_failed audit. Both writes are
// best-effort; the dispatcher can't recover from a DB failure on
// the rejection path either.
func (d *Dispatcher) markRejectedDecryptFailed(
	ctx context.Context,
	row FederationInbox,
	env *federation.Envelope,
	peerID uuid.UUID,
	block *federation.EncryptionBlock,
	reason string,
	recipientKeyVersionAttempted int32,
	lastErr string,
) {
	rejectReason := string(federation.InboxStatusDecryptFailed)
	_, _ = d.pool.MarkInboxRejected(ctx, MarkInboxRejectedParams{
		ID:           row.ID,
		RejectReason: &rejectReason,
		LastError:    lastErr,
	})
	if d.audit != nil {
		var senderKeyVersion int32
		if block != nil {
			senderKeyVersion = block.SenderKeyVersion
		}
		d.audit.FederationInboxDecryptFailed(
			ctx, nil,
			peerID.String(),
			string(env.Type),
			env.ID,
			reason,
			senderKeyVersion,
			recipientKeyVersionAttempted,
		)
	}
	if d.logger != nil {
		d.logger.LogAttrs(ctx, slog.LevelWarn, "inbox.dispatcher.decrypt_failed",
			slog.String("inbox_id", uuid.UUID(row.ID.Bytes).String()),
			slog.String("activity_id", env.ID),
			slog.String("reason", reason),
			slog.String("err", lastErr),
		)
	}
}

// firstRecipientURI returns env.To[0] when populated, falling
// back to the first CC entry when To is empty. The outbox sealer
// targets a single recipient per emission so this always picks
// the right user; the CC fallback covers a defensive case where
// a peer rewrites To→CC mid-delivery.
func firstRecipientURI(env *federation.Envelope) string {
	for _, u := range env.To {
		if u != "" {
			return u
		}
	}
	for _, u := range env.CC {
		if u != "" {
			return u
		}
	}
	return ""
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
