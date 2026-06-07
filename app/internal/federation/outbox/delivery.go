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
	"encoding/json"
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

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
)

// DeliveryConfig controls the worker's cadence + HTTP client.
type DeliveryConfig struct {
	// Interval between delivery scans. Default 5s — matches the
	// inbox dispatcher cadence so operators have one mental model.
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
func DefaultDeliveryConfig() DeliveryConfig {
	return DeliveryConfig{
		Interval:            5 * time.Second,
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

// Worker is the delivery worker.
type Worker struct {
	cfg        DeliveryConfig
	pool       *pgxpool.Pool
	q          *Queries
	signer     HTTPSigner
	lookupPeer PeerLookup
	http       *http.Client
	logger     *slog.Logger

	mu      sync.Mutex
	running bool
}

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
		}
	}
}

// RunOnce processes a single batch of due rows. Exported so
// tests can drive deterministically.
func (w *Worker) RunOnce(ctx context.Context) (sent, failed, deferred int) {
	rows, err := w.q.ListDueOutbox(ctx, w.cfg.BatchSize)
	if err != nil {
		w.logErr(ctx, "outbox.delivery.list.error", err)
		return 0, 0, 0
	}
	if len(rows) == 0 {
		return 0, 0, 0
	}

	for i := range rows {
		switch w.deliverOne(ctx, rows[i]) {
		case deliveryOutcomeSent:
			sent++
		case deliveryOutcomeFailedTerminal:
			failed++
		case deliveryOutcomeFailedTransient:
			deferred++
		}
	}

	if w.logger != nil {
		w.logger.LogAttrs(ctx, slog.LevelInfo, "outbox.delivery.tick",
			slog.Int("sent", sent),
			slog.Int("failed", failed),
			slog.Int("deferred", deferred),
		)
	}
	return sent, failed, deferred
}

type deliveryOutcome int

const (
	deliveryOutcomeSent deliveryOutcome = iota
	deliveryOutcomeFailedTransient
	deliveryOutcomeFailedTerminal
)

// deliverOne POSTs a single outbox row's envelope to the
// recipient peer. Outcome drives the state transition.
func (w *Worker) deliverOne(ctx context.Context, row FederationOutbox) deliveryOutcome {
	// Rebuild envelope from the activities row.
	envelope, err := w.buildEnvelope(ctx, uuid.UUID(row.ActivityID.Bytes))
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

	// Build the POST.
	body, err := json.Marshal(envelope)
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
func (w *Worker) buildEnvelope(ctx context.Context, activityID uuid.UUID) (*federation.Envelope, error) {
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
	)
	err := w.pool.QueryRow(ctx, `
		SELECT activity_uri, activity_type, actor_uri, object_uri,
		       to_uris, cc_uris, payload, published_at,
		       signature_value, signature_pubkey
		FROM activities WHERE id = $1
	`, activityID).Scan(
		&activityURI, &activityType, &actorURI, &objectURI,
		&toURIs, &ccURIs, &payload, &publishedAt,
		&sigValue, &sigPubKey,
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
	return env, nil
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
