// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Delivery worker per the 1.22.D design proposal §3.3-§3.4.
// Phase 1.22.D-b-4.
//
// # Lifecycle
//
// Single goroutine; ticker-driven (default 5s). Per tick:
//
//   1. ListDueOutbox(BatchSize) — partial index keeps the
//      working set bounded.
//   2. Group by peer (HTTP/2 keep-alive friendliness).
//   3. For each row: rebuild envelope from activities ledger
//      → sign HTTP-Sig with instance Ed25519 key → POST to
//      peer's /federation/inbox.
//   4. 2xx → status='sent'; 4xx (non-retryable except 429) →
//      status='failed'; 5xx / 429 / timeout → bump attempts +
//      schedule next_attempt_at per §3.4 backoff schedule.
//
// # Backoff schedule (§3.4)
//
// Attempt 1 → instant (queued at NOW())
// Attempt 2 → +30s
// Attempt 3 → +5min
// Attempt 4 → +1h
// Attempt 5 → +6h
// > 5      → status='failed'
//
// # HTTP/2 connection reuse
//
// MaxIdleConnsPerHost raised from default 2 → 10 so 100
// parallel POSTs to one peer share one TLS handshake (HPACK
// header compression amortises across the multiplexed stream).
//
// # Batching defers to b-5
//
// Per-peer batching via the new /federation/inbox/batch endpoint
// lands in 1.22.D-b-5. Until then, delivery is one POST per
// outbox row.

package outbox

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
)

// DeliveryConfig controls the worker's cadence + HTTP client.
type DeliveryConfig struct {
	// Interval is the ticker-backstop period. The primary wake
	// signal is LISTEN/NOTIFY on federation_outbox INSERT per
	// migration 00006; the ticker catches missed notifications
	// under load. Default 30s per 1.22.D-b-6 G1 — same
	// "correctness backstop only" pattern as the dispatchers.
	Interval time.Duration

	// BatchSize per tick. Default 100.
	BatchSize int32

	// RequestTimeout per POST. Default 10s per design §5.5 Q2.
	RequestTimeout time.Duration

	// MaxIdleConnsPerHost for the HTTP/2 transport. Default 10
	// so 100 parallel POSTs to one peer share one TLS handshake.
	MaxIdleConnsPerHost int
}

// DefaultDeliveryConfig returns the boot defaults.
//
// Interval = 30s matches the gold-standard correctness-backstop
// pattern locked in by 1.22.D-b-6 G1. The actual responsiveness
// comes from LISTEN/NOTIFY on federation_outbox INSERT;
// production p99 end-to-end is sub-1s in the happy path.
func DefaultDeliveryConfig() DeliveryConfig {
	return DeliveryConfig{
		Interval:            30 * time.Second,
		BatchSize:           100,
		RequestTimeout:      10 * time.Second,
		MaxIdleConnsPerHost: 10,
	}
}

// HTTPSigner is the contract the delivery worker uses to sign
// outbound POSTs. Boot wires it to a closure over
// identity.Identity that calls httpsig.SignAndAttach with the
// instance's Ed25519 key. Keeps the private key encapsulated
// in the identity package; no other code touches the raw key.
type HTTPSigner interface {
	Sign(req *http.Request, body []byte) error
	KeyID() string // returned for the federation_outbox.delivered_with_key_id audit column
}

// PeerInfo is the subset of peer.Peer the delivery worker
// needs. Boot wires it via a closure over peer.Registry.ByID
// (mirrors the inbox dispatcher's peerLookup pattern).
type PeerInfo struct {
	ID          uuid.UUID
	InstanceURL string
	Enabled     bool
	Connected   bool
}

// PeerLookup resolves PeerInfo by id at delivery time.
type PeerLookup func(ctx context.Context, peerID uuid.UUID) (PeerInfo, error)

// PeerSupportsE2EFunc reports whether a peer has negotiated
// end-to-end encryption support. Phase 1.22.I-e; wired by boot
// to peer.Registry.ByID + Capabilities.SupportsE2E. Nil-safe.
type PeerSupportsE2EFunc func(ctx context.Context, peerID uuid.UUID) bool

// RecipientEncKeyFunc returns the recipient actor's current
// encryption public key (32 bytes) + version number for the
// EncryptionBlock.RecipientKeyVersion field. Phase 1.22.I-e;
// wired by boot to federation/remote.Handler.GetEncryptionKey.
// Returns a typed error when the cache miss can't be resolved;
// the dispatcher logs + falls through to plaintext.
type RecipientEncKeyFunc func(ctx context.Context, actorURI string) (pubBytes []byte, version int32, err error)

// Worker is the delivery worker.
type Worker struct {
	cfg        DeliveryConfig
	pool       *pgxpool.Pool
	q          *Queries
	signer     HTTPSigner
	lookupPeer PeerLookup
	http       *http.Client
	logger     *slog.Logger

	// 1.22.I-e per-recipient encryption hooks. All nil-safe;
	// when any of the three is unwired the dispatcher falls
	// back to the existing 1.22.D plaintext path.
	peerSupportsE2E PeerSupportsE2EFunc
	recipientEncKey RecipientEncKeyFunc
	audit           *audit.Recorder

	// wake is signalled by the LISTEN goroutine on every
	// federation_outbox_pending notification. Buffered=1 so the
	// LISTEN never blocks; main loop drains extras before
	// re-entering the scan to coalesce bursts. Per 1.22.D-b-6
	// G1: end-to-end p99 sub-1s is the contract; LISTEN is the
	// primary signal, ticker is correctness backstop only.
	wake chan struct{}

	mu      sync.Mutex
	running bool
}

// SetPeerSupportsE2E wires the per-recipient capability check.
// Call once at boot AFTER the peer registry is constructed.
// Idempotent; passing nil disables the encryption gate.
func (w *Worker) SetPeerSupportsE2E(f PeerSupportsE2EFunc) { w.peerSupportsE2E = f }

// SetRecipientEncKey wires the recipient pubkey lookup. Call
// once at boot. Idempotent; passing nil disables encryption
// (the gate would have to soft-fail anyway, so the cleaner
// thing is to skip the whole path).
func (w *Worker) SetRecipientEncKey(f RecipientEncKeyFunc) { w.recipientEncKey = f }

// SetAudit wires the audit recorder for federation.emission.encrypted
// + federation.emission.skipped(reason=recipient_key_unfetchable).
// nil-safe (the gate works without audit, just with no observability).
func (w *Worker) SetAudit(rec *audit.Recorder) { w.audit = rec }

// ErrTestRecipientKeyMissing is a sentinel the integration test
// suite returns from its synthetic RecipientEncKey hook when
// modelling a cache miss. Not surfaced from production code
// paths; exported so tests in outbox_test can reference it.
var ErrTestRecipientKeyMissing = errors.New("outbox: recipient key not available (test sentinel)")

// NewWorker constructs the delivery worker. The HTTP client is
// built here with the HTTP/2 + connection-pool defaults so
// callers don't have to know the underlying transport details.
func NewWorker(
	cfg DeliveryConfig,
	pool *pgxpool.Pool,
	signer HTTPSigner,
	lookupPeer PeerLookup,
	logger *slog.Logger,
) *Worker {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = 10
	}
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &Worker{
		cfg:        cfg,
		pool:       pool,
		q:          New(pool),
		signer:     signer,
		lookupPeer: lookupPeer,
		http: &http.Client{
			Transport: transport,
			Timeout:   cfg.RequestTimeout,
		},
		logger: logger,
		wake:   make(chan struct{}, 1),
	}
}

// Run blocks until ctx is cancelled. Safe to call once per
// process; subsequent calls log + return.
func (w *Worker) Run(ctx context.Context) {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		if w.logger != nil {
			w.logger.Warn("outbox.delivery: Run called more than once")
		}
		return
	}
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	if w.logger != nil {
		w.logger.LogAttrs(ctx, slog.LevelInfo, "outbox.delivery.start",
			slog.Duration("interval", w.cfg.Interval),
			slog.Int("batch_size", int(w.cfg.BatchSize)),
		)
	}

	// LISTEN goroutine — primary wake signal per 1.22.D-b-6 G1.
	// Survives connection blips via the inner reconnect loop.
	go w.listenLoop(ctx)

	// Drain at boot.
	w.RunOnce(ctx)

	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.RunOnce(ctx)
		case <-w.wake:
			drainDeliveryWake(w.wake)
			w.RunOnce(ctx)
		}
	}
}

// listenLoop arms LISTEN federation_outbox_pending on a
// dedicated connection. On notify, signals w.wake. Survives
// connection blips via the outer reconnect loop. Per
// 1.22.D-b-6 G1: this is the load-bearing latency primitive —
// without LISTEN, the ticker (30s default) sets the worst-
// case latency.
func (w *Worker) listenLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := w.listenOnce(ctx); err != nil && w.logger != nil {
			w.logger.LogAttrs(ctx, slog.LevelWarn, "outbox.delivery.listen.error",
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

func (w *Worker) listenOnce(ctx context.Context) error {
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN federation_outbox_pending"); err != nil {
		return err
	}
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case w.wake <- struct{}{}:
		default: // already pending; coalesce
		}
	}
}

func drainDeliveryWake(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// RunOnce processes a single batch of due rows. Exported so
// tests can drive deterministically.
//
// Per-peer batching per spec §10.4 + design §3.10: rows are
// grouped by peer_id; per peer with >1 row we POST the batch to
// /federation/inbox/batch in one signed request. Solo activity →
// singleton POST to /federation/inbox (no batching overhead).
func (w *Worker) RunOnce(ctx context.Context) (sent, failed, deferred int) {
	rows, err := w.q.ListDueOutbox(ctx, w.cfg.BatchSize)
	if err != nil {
		w.logErr(ctx, "outbox.delivery.list.error", err)
		return 0, 0, 0
	}
	if len(rows) == 0 {
		return 0, 0, 0
	}

	// Group by peer for the batched-POST optimisation.
	byPeer := make(map[uuid.UUID][]FederationOutbox, 4)
	for i := range rows {
		pid := uuid.UUID(rows[i].PeerID.Bytes)
		byPeer[pid] = append(byPeer[pid], rows[i])
	}

	refused := 0
	for _, peerRows := range byPeer {
		if len(peerRows) == 1 {
			// Solo activity — singleton POST.
			switch w.deliverOne(ctx, peerRows[0]) {
			case deliveryOutcomeSent:
				sent++
			case deliveryOutcomeFailedTerminal:
				failed++
			case deliveryOutcomeFailedTransient:
				deferred++
			case deliveryOutcomeRefused:
				// Phase 1.22.I-g policy refusal — already
				// audited + status='refused' on the row. Track
				// separately so the tick log distinguishes
				// "blocked by policy" from delivery failures;
				// not surfaced in the (sent, failed, deferred)
				// return for backwards-compat with the test
				// harness signature.
				refused++
			}
		} else {
			// Batched POST. The receiver returns 200 with per-
			// envelope status; we transition each row
			// individually based on its result.
			s, f, d := w.deliverBatched(ctx, peerRows)
			sent += s
			failed += f
			deferred += d
		}
	}

	if w.logger != nil {
		w.logger.LogAttrs(ctx, slog.LevelInfo, "outbox.delivery.tick",
			slog.Int("sent", sent),
			slog.Int("failed", failed),
			slog.Int("deferred", deferred),
			slog.Int("refused", refused),
		)
	}
	return sent, failed, deferred
}

// deliverBatched POSTs N outbox rows for the SAME peer in one
// /federation/inbox/batch request. The receiver returns a
// per-envelope results array; we transition each row based on
// its individual status.
//
// Caps at 50 envelopes per batch per spec §10.4; if peerRows
// has more than 50 we split into multiple batched POSTs.
func (w *Worker) deliverBatched(ctx context.Context, peerRows []FederationOutbox) (sent, failed, deferred int) {
	const batchCap = 50
	for start := 0; start < len(peerRows); start += batchCap {
		end := start + batchCap
		if end > len(peerRows) {
			end = len(peerRows)
		}
		s, f, d := w.deliverOneBatch(ctx, peerRows[start:end])
		sent += s
		failed += f
		deferred += d
	}
	return sent, failed, deferred
}

// deliverOneBatch POSTs a single batch of up to 50 outbox rows
// to /federation/inbox/batch on the shared peer.
func (w *Worker) deliverOneBatch(ctx context.Context, rows []FederationOutbox) (sent, failed, deferred int) {
	if len(rows) == 0 {
		return 0, 0, 0
	}
	peerID := uuid.UUID(rows[0].PeerID.Bytes)
	peer, err := w.lookupPeer(ctx, peerID)
	if err != nil {
		for _, row := range rows {
			w.markAttemptFailed(ctx, row, fmt.Errorf("peer lookup: %w", err))
			deferred++
		}
		return
	}
	if !peer.Enabled || !peer.Connected {
		for _, row := range rows {
			_, _ = w.q.MarkOutboxFailedTerminal(ctx, MarkOutboxFailedTerminalParams{
				ID:        row.ID,
				LastError: "peer disabled or not connected at delivery time",
			})
			failed++
		}
		return
	}

	// Build the batch body. activity_uri → outbox row id so we
	// can look up the right row when the per-envelope result
	// arrives.
	envelopes := make([]json.RawMessage, 0, len(rows))
	uriToRow := make(map[string]FederationOutbox, len(rows))
	for _, row := range rows {
		recipientActorURI := ""
		if row.TargetUserUrl != nil {
			recipientActorURI = *row.TargetUserUrl
		}
		env, err := w.buildEnvelope(ctx,
			uuid.UUID(row.ActivityID.Bytes),
			uuid.UUID(row.ID.Bytes),
			uuid.UUID(row.PeerID.Bytes),
			recipientActorURI,
			row.Sensitivity,
		)
		if errors.Is(err, ErrEmissionRefused) {
			// Phase 1.22.I-g: tryEncryptFor already marked the
			// row refused + audited. Skip the per-row batch
			// without counting as deferred — refusal is a
			// successful decision, not a delivery failure.
			continue
		}
		if err != nil {
			w.markAttemptFailed(ctx, row, fmt.Errorf("rebuild envelope: %w", err))
			deferred++
			continue
		}
		// Envelope.Marshal (not json.Marshal) — Extra would be
		// dropped by reflection-based JSON; see deliverOne for
		// the longer note.
		b, err := env.Marshal()
		if err != nil {
			w.markAttemptFailed(ctx, row, fmt.Errorf("marshal envelope: %w", err))
			deferred++
			continue
		}
		envelopes = append(envelopes, b)
		uriToRow[env.ID] = row
	}
	if len(envelopes) == 0 {
		return
	}

	batchBody, err := json.Marshal(map[string]any{"envelopes": envelopes})
	if err != nil {
		for _, row := range uriToRow {
			w.markAttemptFailed(ctx, row, fmt.Errorf("marshal batch: %w", err))
			deferred++
		}
		return
	}

	endpoint := strings.TrimRight(peer.InstanceURL, "/") + "/federation/inbox/batch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(batchBody))
	if err != nil {
		for _, row := range uriToRow {
			w.markAttemptFailed(ctx, row, fmt.Errorf("build request: %w", err))
			deferred++
		}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if err := w.signer.Sign(req, batchBody); err != nil {
		for _, row := range uriToRow {
			w.markAttemptFailed(ctx, row, fmt.Errorf("sign: %w", err))
			deferred++
		}
		return
	}

	resp, err := w.http.Do(req)
	if err != nil {
		for _, row := range uriToRow {
			w.markAttemptFailed(ctx, row, fmt.Errorf("POST %s: %w", endpoint, err))
			deferred++
		}
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// Top-level HTTP failure → defer every row.
	if resp.StatusCode != 200 {
		errMsg := fmt.Sprintf("HTTP %d from %s", resp.StatusCode, endpoint)
		terminal := resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429
		for _, row := range uriToRow {
			if terminal {
				_, _ = w.q.MarkOutboxFailedTerminal(ctx, MarkOutboxFailedTerminalParams{
					ID:        row.ID,
					LastError: errMsg + " (batch endpoint rejected; non-retryable)",
				})
				failed++
			} else {
				w.markAttemptFailed(ctx, row, errors.New(errMsg))
				deferred++
			}
		}
		return
	}

	// Parse per-envelope results.
	var parsed struct {
		Results []struct {
			ActivityURI string `json:"activity_uri"`
			Status      string `json:"status"`
			Reason      string `json:"reason"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		// Receiver claimed 200 but body is malformed — treat as
		// transient (something broke between transports).
		for _, row := range uriToRow {
			w.markAttemptFailed(ctx, row, fmt.Errorf("parse batch response: %w", err))
			deferred++
		}
		return
	}

	seen := make(map[string]bool, len(parsed.Results))
	for _, res := range parsed.Results {
		seen[res.ActivityURI] = true
		row, ok := uriToRow[res.ActivityURI]
		if !ok {
			continue
		}
		switch res.Status {
		case "accepted", "replayed":
			_, _ = w.q.MarkOutboxSent(ctx, MarkOutboxSentParams{
				ID:                 row.ID,
				DeliveredWithKeyID: ptrStr(w.signer.KeyID()),
			})
			sent++
		case "rejected":
			// Terminal — the receiver gave a typed §12.1 reason.
			_, _ = w.q.MarkOutboxFailedTerminal(ctx, MarkOutboxFailedTerminalParams{
				ID:        row.ID,
				LastError: "receiver rejected: " + res.Reason,
			})
			failed++
		default:
			w.markAttemptFailed(ctx, row, fmt.Errorf("unknown batch status %q", res.Status))
			deferred++
		}
	}
	// Rows the receiver didn't acknowledge → transient retry.
	for uri, row := range uriToRow {
		if !seen[uri] {
			w.markAttemptFailed(ctx, row, errors.New("receiver omitted activity from batch response"))
			deferred++
		}
	}
	return
}

type deliveryOutcome int

const (
	deliveryOutcomeSent deliveryOutcome = iota
	deliveryOutcomeFailedTransient
	deliveryOutcomeFailedTerminal

	// deliveryOutcomeRefused — Phase 1.22.I-g sender-refusal
	// policy decision. The row was NOT POSTed; tryEncryptFor
	// already marked it status='refused' + audited via
	// federation.emission.refused. Distinct from
	// FailedTerminal so per-peer tally counters can separate
	// policy refusals from delivery failures (different
	// operator-side diagnoses; admin dashboard pivots on the
	// two distinctly).
	deliveryOutcomeRefused
)

// deliverOne POSTs a single outbox row's envelope to the
// recipient peer. Outcome drives the state transition.
func (w *Worker) deliverOne(ctx context.Context, row FederationOutbox) deliveryOutcome {
	// Rebuild envelope from the activities row. Phase 1.22.I-e
	// folds in per-recipient encryption when the peer supports
	// it; arguments carry outbox + peer + recipient context the
	// encryption branch needs.
	recipientActorURI := ""
	if row.TargetUserUrl != nil {
		recipientActorURI = *row.TargetUserUrl
	}
	envelope, err := w.buildEnvelope(ctx,
		uuid.UUID(row.ActivityID.Bytes),
		uuid.UUID(row.ID.Bytes),
		uuid.UUID(row.PeerID.Bytes),
		recipientActorURI,
		row.Sensitivity,
	)
	if errors.Is(err, ErrEmissionRefused) {
		// Phase 1.22.I-g: tryEncryptFor already marked the row
		// refused + audited. Return a distinct outcome so the
		// caller's tally separates refused from sent/failed/
		// deferred — refusal is a policy decision, not a
		// failure.
		return deliveryOutcomeRefused
	}
	if err != nil {
		w.markAttemptFailed(ctx, row, fmt.Errorf("rebuild envelope: %w", err))
		return deliveryOutcomeFailedTransient
	}

	// Resolve peer + check still connected.
	peer, err := w.lookupPeer(ctx, uuid.UUID(row.PeerID.Bytes))
	if err != nil {
		w.markAttemptFailed(ctx, row, fmt.Errorf("peer lookup: %w", err))
		return deliveryOutcomeFailedTransient
	}
	if !peer.Enabled || !peer.Connected {
		// Peer disabled or in defederation cascade — mark
		// cancelled so we don't burn retries.
		_, _ = w.q.MarkOutboxFailedTerminal(ctx, MarkOutboxFailedTerminalParams{
			ID:        row.ID,
			LastError: "peer disabled or not connected at delivery time",
		})
		return deliveryOutcomeFailedTerminal
	}

	// Build the POST. Use Envelope.Marshal (not json.Marshal):
	// the Extra map carries `json:"-"` so the default Go
	// reflection serializer would drop it on the floor.
	// Envelope.Marshal expands Extra into top-level fields per
	// spec §3.1 so e.g. the activity-type payload + the
	// 1.22.I-c aa:encryptionPublicKey block actually reach the
	// peer.
	body, err := envelope.Marshal()
	if err != nil {
		w.markAttemptFailed(ctx, row, fmt.Errorf("marshal envelope: %w", err))
		return deliveryOutcomeFailedTransient
	}

	endpoint := strings.TrimRight(peer.InstanceURL, "/") + "/federation/inbox"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		w.markAttemptFailed(ctx, row, fmt.Errorf("build request: %w", err))
		return deliveryOutcomeFailedTransient
	}
	req.Header.Set("Content-Type", "application/activity+json")
	// httpsig.SignAndAttach sets Date + Digest + Host then
	// signs the canonical request line. Anchored on the
	// instance Ed25519 key via the injected signer.
	if err := w.signer.Sign(req, body); err != nil {
		w.markAttemptFailed(ctx, row, fmt.Errorf("sign: %w", err))
		return deliveryOutcomeFailedTransient
	}

	resp, err := w.http.Do(req)
	if err != nil {
		w.markAttemptFailed(ctx, row, fmt.Errorf("POST %s: %w", endpoint, err))
		return deliveryOutcomeFailedTransient
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused (HTTP/2
	// multiplexing benefits cap at server-side advertised
	// concurrency; reading the body is mandatory regardless).
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		_, _ = w.q.MarkOutboxSent(ctx, MarkOutboxSentParams{
			ID:                  row.ID,
			DeliveredWithKeyID:  ptrStr(w.signer.KeyID()),
		})
		return deliveryOutcomeSent

	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		// Transient — retry per backoff.
		w.markAttemptFailed(ctx, row, fmt.Errorf("HTTP %d from %s", resp.StatusCode, endpoint))
		return deliveryOutcomeFailedTransient

	default:
		// 4xx (non-429): the inbox rejected with a typed reason.
		// Terminal — operator must investigate via admin UI.
		errMsg := fmt.Sprintf("HTTP %d from %s — non-retryable", resp.StatusCode, endpoint)
		_, _ = w.q.MarkOutboxFailedTerminal(ctx, MarkOutboxFailedTerminalParams{
			ID:        row.ID,
			LastError: errMsg,
		})
		return deliveryOutcomeFailedTerminal
	}
}

// markAttemptFailed bumps attempts + schedules next_attempt_at
// per the §3.4 backoff schedule OR terminal-fails when the
// 5-attempt cap is hit.
func (w *Worker) markAttemptFailed(ctx context.Context, row FederationOutbox, err error) {
	const maxAttempts = 5
	if row.Attempts+1 >= maxAttempts {
		_, _ = w.q.MarkOutboxFailedTerminal(ctx, MarkOutboxFailedTerminalParams{
			ID:        row.ID,
			LastError: err.Error(),
		})
		return
	}
	next := nextAttemptAt(row.Attempts + 1)
	_, _ = w.q.MarkOutboxAttemptFailed(ctx, MarkOutboxAttemptFailedParams{
		ID:            row.ID,
		NextAttemptAt: pgtype.Timestamptz{Time: next, Valid: true},
		LastError:     err.Error(),
	})
}

// nextAttemptAt returns the absolute time of the n-th attempt
// per the §3.4 backoff schedule. n is the 1-indexed attempt
// number (so n=2 is the first retry after the initial attempt).
func nextAttemptAt(n int16) time.Time {
	switch {
	case n <= 2:
		return time.Now().Add(30 * time.Second)
	case n == 3:
		return time.Now().Add(5 * time.Minute)
	case n == 4:
		return time.Now().Add(1 * time.Hour)
	default:
		return time.Now().Add(6 * time.Hour)
	}
}

// buildEnvelope reconstructs the v1 envelope JSON from an
// activities ledger row. The body is what we POST to the peer
// + what HTTP-Sig signs.
//
// Phase 1.22.I-c: the query LEFT JOINs federation_user_keys for
// the actor's current X25519 public key, and (when present)
// injects the aa:encryptionPublicKey block into env.Extra. The
// receiver's inbox actor-cache upsert harvests the key into
// federation_remote_actors so the future I-e outbox encryption +
// I-f inbox decryption have a known recipient key to dispatch
// against.
//
// Phase 1.22.I-e: the JOIN also surfaces the wrapped private key
// so [tryEncryptFor] can unwrap + seal env.Extra against the
// recipient's public key when the peer has negotiated e2e
// support. Activities without an actor_user_ref (system-generated)
// dispatch unencrypted regardless.
func (w *Worker) buildEnvelope(ctx context.Context, activityID, outboxID, peerID uuid.UUID, recipientActorURI string, sensitivityFromRow *string) (*federation.Envelope, error) {
	var (
		activityURI  string
		activityType string
		actorURI     string
		objectURI    *string
		toURIs       []byte
		ccURIs       []byte
		payload      []byte
		publishedAt  pgtype.Timestamptz
		sigValue     *string
		sigPubKey    *string

		encKeyBytes      []byte      // 32 bytes if the actor has a current key, nil otherwise
		encKeyVersion    pgtype.Int4 // matches encKeyBytes — both Valid=false or both Valid=true
		encKeyPrivateEnc []byte      // master-key-wrapped private key bytes; nil when no key
	)
	err := w.pool.QueryRow(ctx, `
		SELECT a.activity_uri, a.activity_type, a.actor_uri, a.object_uri,
		       a.to_uris, a.cc_uris, a.payload, a.published_at,
		       a.signature_value, a.signature_pubkey,
		       fuk.public_key, fuk.version, fuk.private_key_enc
		  FROM activities a
		  LEFT JOIN federation_user_keys fuk
		    ON fuk.user_ref = a.actor_user_ref
		   AND fuk.is_current = TRUE
		 WHERE a.id = $1
	`, activityID).Scan(
		&activityURI, &activityType, &actorURI, &objectURI,
		&toURIs, &ccURIs, &payload, &publishedAt,
		&sigValue, &sigPubKey,
		&encKeyBytes, &encKeyVersion, &encKeyPrivateEnc,
	)
	if err != nil {
		return nil, err
	}

	env := &federation.Envelope{
		Context:   federation.ContextV1,
		Type:      federation.ActivityType(activityType),
		ID:        activityURI,
		Actor:     actorURI,
		Published: publishedAt.Time,
	}
	if objectURI != nil {
		env.Object = *objectURI
	}
	if len(toURIs) > 0 {
		_ = json.Unmarshal(toURIs, &env.To)
	}
	if len(ccURIs) > 0 {
		_ = json.Unmarshal(ccURIs, &env.CC)
	}

	// Per-actor signature block. Phase 1.22.D-b ships
	// structural-only verify on the receiver (per §5.6); we
	// emit the same Ed25519 placeholder shape as the inbox
	// expects. 1.22.I will swap this for real per-actor crypto.
	if sigValue != nil && sigPubKey != nil {
		env.Signature = &federation.Signature{
			Type:      federation.SignatureAlgEd25519,
			PublicKey: *sigPubKey,
			Value:     *sigValue,
		}
	} else {
		// No persisted signature — synthesize the placeholder
		// the inbox needs for the structural-sig check.
		env.Signature = &federation.Signature{
			Type:      federation.SignatureAlgEd25519,
			PublicKey: actorURI + "#main-key",
			Value:     "AAAAAAAAAAAA",
		}
	}

	// Surface activity-specific fields from the payload JSON
	// into env.Extra so the receiver sees the verb's payload
	// (e.g. content for Create(Note)). Per spec §3.1 envelope
	// extra fields are merged at the top level.
	if len(payload) > 0 {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(payload, &extra); err == nil {
			env.Extra = extra
		}
	}

	// Phase 1.22.I-c — advertise the actor's current encryption
	// public key inline so receivers can populate their
	// federation_remote_actors cache without a follow-up fetch.
	// Skipped when the activity has no actor_user_ref (system-
	// generated) or the user somehow has no current key (post-I-b
	// invariant violation; defensive).
	if len(encKeyBytes) == 32 && encKeyVersion.Valid && encKeyVersion.Int32 >= 1 {
		if env.Extra == nil {
			env.Extra = make(map[string]json.RawMessage, 1)
		}
		encKeyRaw, err := json.Marshal(map[string]any{
			"type":            federation.TypeX25519PublicKey,
			"publicKeyBase64": base64.StdEncoding.EncodeToString(encKeyBytes),
			"version":         encKeyVersion.Int32,
		})
		if err == nil {
			env.Extra[federation.PropEncryptionPublicKey] = encKeyRaw
		}
	}

	// Phase 1.22.I-e + I-g — per-recipient encryption + sender-
	// refusal policy. tryEncryptFor consults policy.ChoosePathFor
	// against the row's sensitivity tier (denormalized at INSERT
	// time per migration 00012) + the peer's e2e capability +
	// the recipient's pubkey availability. Three paths:
	//
	//   EmissionEncrypted — env mutates to encrypted shape; we
	//                       return env with Encryption populated.
	//   EmissionPlaintext — env unchanged; we return env as the
	//                       legacy 1.22.D shape.
	//   EmissionRefused   — env DOES NOT dispatch; we return
	//                       policy.ErrEmissionRefused so the
	//                       caller marks the row refused + audits.
	if err := w.tryEncryptFor(ctx, env, outboxID, peerID, recipientActorURI,
		sensitivityFromRow, encKeyVersion, encKeyPrivateEnc); err != nil {
		return nil, err
	}
	return env, nil
}

// tryEncryptFor applies the 1.22.I-g emission policy then, if
// the policy says encrypt, mutates env into its encrypted shape.
// Three returnable outcomes:
//
//   - (nil, EmissionEncrypted) — env now carries an
//     [federation.EncryptionBlock] in place of its Extra map; the
//     was_encrypted column flips to true + the
//     federation.emission.encrypted audit row fires.
//
//   - (nil, EmissionPlaintext) — env is unchanged; the caller
//     POSTs the legacy 1.22.D plaintext shape. Reachable for
//     public + team tiers when capability or key isn't
//     available, and for any tier when the Worker hooks aren't
//     wired (test fixtures + legacy boot paths).
//
//   - ([policy.ErrEmissionRefused], EmissionRefused) — the
//     share sensitivity tier mandated encryption + the
//     capability or key wasn't there. The caller marks the row
//     refused + audits + does NOT POST. Refusal is terminal:
//     no retries, no backoff, no auto-recovery on capability
//     change.
//
// # Inputs the caller threads in
//
//   - outboxID                  — federation_outbox row ID;
//                                  used to mark was_encrypted or
//                                  refused on the right row.
//   - peerID                    — recipient peer's UUID; audit
//                                  metadata + capability lookup.
//   - recipientActorURI         — recipient's actor URI; remote-
//                                  actor encryption-key lookup +
//                                  EncryptionBlock.RecipientKeyID.
//   - sensitivityFromRow        — federation_outbox.sensitivity
//                                  denormalized at INSERT time
//                                  (1.22.I-g, migration 00012);
//                                  NULL → conservative-public.
//   - senderKeyVersion +
//     senderPrivateEnc          — buildEnvelope's JOIN against
//                                  federation_user_keys; wrapped
//                                  private key gets unwrapped
//                                  here for box.Seal + zeroed
//                                  via the userkeys.Unwrap helper.
//
// # Why the path comes back as a value
//
// The buildEnvelope caller doesn't need the value (it only cares
// whether env got mutated), but deliverOne / deliverOneBatch
// downstream want to know which path was taken so they can
// decide between MarkOutboxSent + MarkOutboxRefused after
// network completion. Returning the path here means the
// decision is made once + flowed through.
//
// # Why audit happens inside this function, not at the call site
//
// The audit event corresponds 1:1 to the path choice. Putting
// the recorder call here keeps the (path, audit) pairing on a
// single screen — a future maintainer can confirm "every
// EmissionRefused fires emission.refused" without grepping the
// caller. Plaintext path is silent (no audit) by design — it's
// the 1.22.D legacy behaviour, observable via was_encrypted=false
// in the row.
func (w *Worker) tryEncryptFor(
	ctx context.Context,
	env *federation.Envelope,
	outboxID, peerID uuid.UUID,
	recipientActorURI string,
	sensitivityFromRow *string,
	senderKeyVersion pgtype.Int4,
	senderPrivateEnc []byte,
) error {
	// Decode the per-row sensitivity. NULL or unrecognised text
	// → conservative-public (matches the resolver dispatcher's
	// default + the documented "pre-I-g rows treated as public"
	// migration note).
	tier := Sensitivity(SensitivityPublic)
	if sensitivityFromRow != nil && *sensitivityFromRow != "" {
		tier = Sensitivity(*sensitivityFromRow)
	}

	// Probe the encryption inputs without paying for the unwrap
	// + seal yet. Both probes are cheap (one in-memory bool
	// lookup + one cache hit / miss).
	peerCan := false
	if w.peerSupportsE2E != nil {
		peerCan = w.peerSupportsE2E(ctx, peerID)
	}

	// Recipient-key probe — only resolve when the peer says it
	// supports e2e (saves a cache miss + DB hit for the legacy
	// peers that won't encrypt anyway). The probe's typed err
	// distinguishes cold-miss from connection error; either
	// counts as "key not available" for the policy decision.
	var (
		recPub []byte
		recVer int32
	)
	keyAvailable := false
	if peerCan && w.recipientEncKey != nil && recipientActorURI != "" {
		p, v, err := w.recipientEncKey(ctx, recipientActorURI)
		if err == nil {
			recPub = p
			recVer = v
			keyAvailable = true
		} else {
			w.logErr(ctx, "outbox.encrypt.recipient_key.miss", err)
			// Audit the cache miss only when the policy still
			// allows plaintext fallback — for required-tier
			// rows the upcoming MarkOutboxRefused + emission.refused
			// audit will carry the full diagnosis. Skipping the
			// extra row keeps the audit feed signal-clean.
			if !RequiresEncryption(tier) && w.audit != nil {
				w.audit.FederationEmissionSkippedForPeer(ctx,
					peerID.String(),
					"recipient_key_unfetchable",
					string(env.Type),
				)
			}
		}
	}

	// Path decision is now a pure function over the probed state.
	path := ChoosePathFor(tier, peerCan, keyAvailable)

	switch path {
	case EmissionRefused:
		// Mark + audit + signal caller. Terminal — no POST, no
		// retry. The status='refused' transition routes the row
		// out of the queued-partial-index automatically.
		reasonStr := string(RefuseReasonEncryptionRequiredButUnavailable)
		reasonPtr := &reasonStr
		if _, err := w.q.MarkOutboxRefused(ctx, MarkOutboxRefusedParams{
			ID:            pgtype.UUID{Bytes: outboxID, Valid: true},
			RefusedReason: reasonPtr,
		}); err != nil {
			w.logErr(ctx, "outbox.refuse.mark_row", err)
		}
		if w.audit != nil {
			w.audit.FederationEmissionRefused(ctx,
				peerID.String(),
				string(env.Type),
				string(tier),
				reasonStr,
			)
		}
		return ErrEmissionRefused

	case EmissionPlaintext:
		// Leave env in 1.22.D plaintext shape. The caller's POST
		// proceeds; was_encrypted stays false. No audit (the
		// row state carries the truth).
		return nil

	case EmissionEncrypted:
		// Fall through to the seal logic below.
	}

	// Sanity-check the inputs the seal needs. Encryption path
	// requires the sender's wrapped private key + a valid
	// version. Missing either is a misconfiguration at the
	// resolver/keypair layer — log + soft-fail to plaintext so
	// the row delivers (worse than silent, but the alternative
	// is silently dropping deliveries on a config bug).
	if !senderKeyVersion.Valid || senderKeyVersion.Int32 < 1 || len(senderPrivateEnc) == 0 {
		w.logErr(ctx, "outbox.encrypt.sender_key.missing",
			fmt.Errorf("path=encrypted but sender key version invalid or wrapped bytes empty"))
		return nil
	}

	// Unwrap the sender's at-rest private key. The userkeys
	// package owns the atrest+ecdh wire; we consume the 32-byte
	// X25519 scalar via PrivateKey.Bytes(). Defer a zero of the
	// unwrapped bytes so a panic in the seal step doesn't leak
	// them on the stack.
	senderPriv, err := userkeys.Unwrap(senderPrivateEnc)
	if err != nil {
		w.logErr(ctx, "outbox.encrypt.sender_unwrap", err)
		return nil
	}
	senderPrivBytes := senderPriv.Bytes()
	defer func() {
		for i := range senderPrivBytes {
			senderPrivBytes[i] = 0
		}
	}()

	// Serialize the plaintext payload (env.Extra). Receivers
	// (1.22.I-f) decrypt + restore Extra so the rest of the
	// inbox dispatch path doesn't change. Marshal as JSON object
	// even when Extra is nil/empty — encrypts to box.Overhead
	// bytes + decrypts to `{}` cleanly.
	plaintext, err := json.Marshal(env.Extra)
	if err != nil {
		w.logErr(ctx, "outbox.encrypt.marshal_extra", err)
		return nil
	}

	// Seal.
	nonce, ciphertext, err := federation.EncryptActivityPayload(plaintext, senderPrivBytes, recPub)
	if err != nil {
		w.logErr(ctx, "outbox.encrypt.seal", err)
		return nil
	}

	// Replace env.Extra with the encryption block + mark the row.
	env.Encryption = &federation.EncryptionBlock{
		Algorithm:           federation.EncryptionAlgNaClBoxV1,
		SenderKeyID:         env.Actor + "#encryption-key",
		SenderKeyVersion:    senderKeyVersion.Int32,
		RecipientKeyID:      recipientActorURI + "#encryption-key",
		RecipientKeyVersion: recVer,
		Nonce:               federation.Base64Bytes(nonce),
		Ciphertext:          federation.Base64Bytes(ciphertext),
	}
	env.Extra = nil

	// Observability — best-effort, non-blocking. Mark before
	// audit so the row's was_encrypted column reflects reality
	// even if the audit-row write fails.
	if err := w.q.MarkOutboxEncrypted(ctx, pgtype.UUID{Bytes: outboxID, Valid: true}); err != nil {
		w.logErr(ctx, "outbox.encrypt.mark_row", err)
	}
	if w.audit != nil {
		w.audit.FederationEmissionEncrypted(ctx,
			peerID.String(),
			string(env.Type),
			senderKeyVersion.Int32,
			recVer,
		)
	}
	return nil
}

func (w *Worker) logErr(ctx context.Context, msg string, err error) {
	if w.logger != nil {
		w.logger.LogAttrs(ctx, slog.LevelError, msg, slog.String("err", err.Error()))
	}
}

// IdentitySigner wraps the federation/identity package to
// satisfy the HTTPSigner contract. Kept in this file so the
// outbox package owns the wiring; boot just constructs it.
type IdentitySigner struct {
	PrivateKey ed25519.PrivateKey
	KeyURL     string // e.g. https://local.example/instance#main-key
}

func (s *IdentitySigner) Sign(req *http.Request, body []byte) error {
	// httpsig requires "host" in the signed-headers set; Go's
	// net/http doesn't auto-populate the Host header until the
	// request is dispatched, so the signing path sees ""
	// unless we set it here. Use req.URL.Host (which is always
	// present on a built request) as the canonical source.
	if req.Header.Get("Host") == "" {
		req.Header.Set("Host", req.URL.Host)
	}
	return httpsig.SignAndAttach(req, body, s.KeyURL, s.PrivateKey)
}

func (s *IdentitySigner) KeyID() string { return s.KeyURL }

func ptrStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
