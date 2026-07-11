// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-g integration tests for the outbox delivery
// Worker's sender-refusal policy. Real Postgres + httptest +
// real audit.Recorder against the DB so the
// federation.emission.refused event row is observable.
//
// What the suite proves:
//
//   - Restricted share + legacy peer (no e2e cap) → refused;
//     status='refused', refused_reason populated, no POST.
//   - Restricted share + e2e-capable peer + cached key →
//     encrypted; status='sent', was_encrypted=true.
//   - Public share + legacy peer → plaintext POST (1.22.D
//     backwards compat); status='sent', was_encrypted=false,
//     no refused audit.
//   - Public share + e2e-capable peer + cached key →
//     opportunistic encryption; status='sent', was_encrypted=true.
//   - Embargo share + legacy peer → refused (same as
//     restricted; both required-encryption tiers).
//   - Mixed recipients (same activity, two outbox rows, one
//     peer e2e-capable, one not) → partial emit: 1 sent
//     encrypted + 1 refused; the encrypted recipient gets the
//     emission, the refusal doesn't poison its sibling.
//
// Skips without AA_DB_PASSWORD.

package outbox_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

// setSensitivityOnRow flips the test outbox row's sensitivity to
// the given tier — emulates what the dispatcher's InsertOutboxRow
// does at boot. Necessary because the newDeliveryFixture seed
// uses a raw INSERT that doesn't pass sensitivity.
func setSensitivityOnRow(t *testing.T, pool *pgxpool.Pool, activityID uuid.UUID, tier outbox.Sensitivity) {
	t.Helper()
	s := string(tier)
	if _, err := pool.Exec(context.Background(),
		`UPDATE federation_outbox SET sensitivity = $1 WHERE activity_id = $2`,
		s, activityID,
	); err != nil {
		t.Fatalf("set sensitivity: %v", err)
	}
}

// countAuditEvents returns how many audit_events rows match the
// given event_type filter (optionally narrowed by metadata key
// equal to a value). Uses the real audit_events table the
// Recorder writes to so assertions observe what an operator would.
func countAuditEvents(t *testing.T, pool *pgxpool.Pool, eventType, metaKey, metaValue string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_events
		   WHERE event_type = $1 AND metadata->>$2 = $3`,
		eventType, metaKey, metaValue,
	).Scan(&n); err != nil {
		t.Fatalf("countAuditEvents: %v", err)
	}
	return n
}

// readOutboxRow returns the columns the policy assertions care
// about so each test reads them once at the bottom + asserts
// without inlining the SQL.
type outboxRowState struct {
	status         string
	wasEncrypted   bool
	refusedReason  *string
}

func readOutboxRow(t *testing.T, pool *pgxpool.Pool, activityID uuid.UUID) outboxRowState {
	t.Helper()
	var st outboxRowState
	if err := pool.QueryRow(context.Background(),
		`SELECT status, was_encrypted, refused_reason
		   FROM federation_outbox WHERE activity_id = $1`,
		activityID,
	).Scan(&st.status, &st.wasEncrypted, &st.refusedReason); err != nil {
		t.Fatalf("readOutboxRow: %v", err)
	}
	return st
}

// wireRealAudit attaches a real Recorder to the worker so its
// federation.emission.refused / encrypted writes land in
// audit_events. Returns the peer ID metadata key the tests filter
// on (the row's peer UUID stringified).
func wireRealAudit(t *testing.T, w *outbox.Worker, pool *pgxpool.Pool) {
	t.Helper()
	w.SetAudit(audit.NewRecorder(pool, slog.New(slog.NewTextHandler(io.Discard, nil))))
}

// --- 1. restricted + legacy peer → refused ---

func TestDispatch_RestrictedShare_PeerNoE2E_RefusedAndAudited(t *testing.T) {
	pool := openTestPool(t)
	worker, peerID, _, _, activityID, _ := newDeliveryFixture(t, http.StatusAccepted, nil)
	setSensitivityOnRow(t, pool, activityID, outbox.SensitivityRestricted)
	wireRealAudit(t, worker, pool)
	// peerSupportsE2E unwired → defaults to false → restricted
	// tier policy refuses.

	beforeRefused := countAuditEvents(t, pool,
		"federation.emission.refused", "peer_id", peerID.String())

	sent, failed, deferred := worker.RunOnce(context.Background())
	if sent+failed+deferred != 0 {
		t.Errorf("refused row should not count as sent/failed/deferred; got %d/%d/%d",
			sent, failed, deferred)
	}

	st := readOutboxRow(t, pool, activityID)
	if st.status != "refused" {
		t.Errorf("status: got %q want refused", st.status)
	}
	if st.refusedReason == nil || *st.refusedReason != string(outbox.RefuseReasonEncryptionRequiredButUnavailable) {
		got := "<nil>"
		if st.refusedReason != nil {
			got = *st.refusedReason
		}
		t.Errorf("refused_reason: got %q want %q",
			got, outbox.RefuseReasonEncryptionRequiredButUnavailable)
	}
	if st.wasEncrypted {
		t.Error("was_encrypted should be false on refusal (no seal happened)")
	}

	afterRefused := countAuditEvents(t, pool,
		"federation.emission.refused", "peer_id", peerID.String())
	if afterRefused != beforeRefused+1 {
		t.Errorf("federation.emission.refused audit: before=%d after=%d want +1",
			beforeRefused, afterRefused)
	}
}

// --- 2. restricted + e2e-capable peer + cached key → encrypted ---

func TestDispatch_RestrictedShare_PeerSupportsE2E_KeyAvailable_Encrypted(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	worker, _, _, grantorRef, activityID, _ := newDeliveryFixture(t, http.StatusAccepted, nil)
	setSensitivityOnRow(t, pool, activityID, outbox.SensitivityRestricted)
	wireRealAudit(t, worker, pool)
	seedSenderKeypair(t, ctx, pool, grantorRef)
	recipientPub, _ := freshRecipientKeypair(t)
	wireHooks(t, worker, true, recipientPub)

	sent, _, _ := worker.RunOnce(ctx)
	if sent != 1 {
		t.Errorf("sent: got %d want 1 (restricted + capable = encrypted dispatch)", sent)
	}

	st := readOutboxRow(t, pool, activityID)
	if st.status != "sent" {
		t.Errorf("status: got %q want sent", st.status)
	}
	if !st.wasEncrypted {
		t.Error("was_encrypted: got false want true (restricted MUST encrypt)")
	}
	if st.refusedReason != nil {
		t.Errorf("refused_reason: got %v want nil (not refused)", *st.refusedReason)
	}
}

// --- 3. public + legacy peer → plaintext (1.22.D backwards compat) ---

func TestDispatch_PublicShare_PeerNoE2E_PlaintextDispatched(t *testing.T) {
	pool := openTestPool(t)
	worker, peerID, _, _, activityID, _ := newDeliveryFixture(t, http.StatusAccepted, nil)
	setSensitivityOnRow(t, pool, activityID, outbox.SensitivityPublic)
	wireRealAudit(t, worker, pool)
	// Hooks unwired → tryEncryptFor falls back to plaintext for
	// the public tier (the legacy 1.22.D path).

	beforeRefused := countAuditEvents(t, pool,
		"federation.emission.refused", "peer_id", peerID.String())

	sent, _, _ := worker.RunOnce(context.Background())
	if sent != 1 {
		t.Errorf("sent: got %d want 1 (public should fall back to plaintext, not refuse)", sent)
	}

	st := readOutboxRow(t, pool, activityID)
	if st.status != "sent" {
		t.Errorf("status: got %q want sent", st.status)
	}
	if st.wasEncrypted {
		t.Error("was_encrypted: got true want false (plaintext fallback)")
	}

	afterRefused := countAuditEvents(t, pool,
		"federation.emission.refused", "peer_id", peerID.String())
	if afterRefused != beforeRefused {
		t.Errorf("federation.emission.refused MUST NOT fire for public tier; before=%d after=%d",
			beforeRefused, afterRefused)
	}
}

// --- 4. public + e2e-capable peer + cached key → opportunistic encrypt ---

func TestDispatch_PublicShare_PeerSupportsE2E_OpportunisticallyEncrypted(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	worker, _, _, grantorRef, activityID, _ := newDeliveryFixture(t, http.StatusAccepted, nil)
	setSensitivityOnRow(t, pool, activityID, outbox.SensitivityPublic)
	wireRealAudit(t, worker, pool)
	seedSenderKeypair(t, ctx, pool, grantorRef)
	recipientPub, _ := freshRecipientKeypair(t)
	wireHooks(t, worker, true, recipientPub)

	sent, _, _ := worker.RunOnce(ctx)
	if sent != 1 {
		t.Errorf("sent: got %d want 1", sent)
	}

	st := readOutboxRow(t, pool, activityID)
	if !st.wasEncrypted {
		t.Error("was_encrypted: got false want true (opportunistic encryption when both sides can)")
	}
}

// --- 5. embargo + legacy peer → refused (symmetric with restricted) ---

func TestDispatch_EmbargoShare_NoE2E_Refused(t *testing.T) {
	pool := openTestPool(t)
	worker, _, _, _, activityID, _ := newDeliveryFixture(t, http.StatusAccepted, nil)
	setSensitivityOnRow(t, pool, activityID, outbox.SensitivityEmbargo)
	wireRealAudit(t, worker, pool)
	// No hooks wired — embargo + legacy = refuse.

	worker.RunOnce(context.Background())

	st := readOutboxRow(t, pool, activityID)
	if st.status != "refused" {
		t.Errorf("status: got %q want refused (embargo tier MUST refuse)", st.status)
	}
}

// --- 6. mixed recipients: one capable, one not → partial emit ---

// TestDispatch_MixedRecipients_PartialRefusal proves the load-
// bearing per-recipient invariant: a single activity fanning out
// to two peers (one e2e-capable, one not) results in 1 encrypted
// dispatch + 1 refusal. The refusal MUST NOT poison the capable
// recipient's emission.
//
// Setup: two federation_peers (peer A capable, peer B legacy),
// one restricted activity, two federation_outbox rows pointing
// at the same activity → different peers. SetPeerSupportsE2E
// branches on peer ID so A returns true + B returns false.
func TestDispatch_MixedRecipients_PartialRefusal(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	worker, peerA, _, grantorRef, activityID, srvA := newDeliveryFixture(t, http.StatusAccepted, nil)
	setSensitivityOnRow(t, pool, activityID, outbox.SensitivityRestricted)
	wireRealAudit(t, worker, pool)
	seedSenderKeypair(t, ctx, pool, grantorRef)
	recipientPub, _ := freshRecipientKeypair(t)

	// Stand up a SECOND fake inbox for peer B.
	srvBMu := sync.Mutex{}
	postedToB := 0
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		srvBMu.Lock()
		postedToB++
		srvBMu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srvB.Close)

	// Seed peer B + a second outbox row for the same activity.
	var peerB uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'Peer B (legacy)', '', 'connected', 'plaintext', TRUE, 'connected', $2)
		RETURNING id`,
		srvB.URL, grantorRef,
	).Scan(&peerB); err != nil {
		t.Fatalf("seed peer B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM federation_outbox WHERE peer_id = $1`, peerB)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM federation_peers WHERE id = $1`, peerB)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO federation_outbox (activity_id, peer_id, target_user_url, sensitivity)
		VALUES ($1, $2, $3, 'restricted')`,
		activityID, peerB, srvB.URL+"/users/bob-legacy",
	); err != nil {
		t.Fatalf("seed outbox B: %v", err)
	}

	// Worker's peer lookup: route srvA / srvB to the right URL
	// regardless of peer ID. Replace the fixture's lookupPeer
	// closure by re-wiring via a wrapper — easier to write a
	// new worker than to expose mutation surface.
	worker = outbox.NewWorker(
		outbox.DeliveryConfig{
			Interval:       time.Hour,
			BatchSize:      100,
			RequestTimeout: 5 * time.Second,
		},
		pool,
		&fixedSigner{keyURL: "https://local.example/instance#main-key"},
		func(ctx context.Context, id uuid.UUID) (outbox.PeerInfo, error) {
			switch id {
			case peerA:
				return outbox.PeerInfo{ID: id, InstanceURL: srvA.URL, Enabled: true, Connected: true}, nil
			case peerB:
				return outbox.PeerInfo{ID: id, InstanceURL: srvB.URL, Enabled: true, Connected: true}, nil
			default:
				return outbox.PeerInfo{}, nil
			}
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	wireRealAudit(t, worker, pool)
	worker.SetPeerSupportsE2E(func(_ context.Context, id uuid.UUID) bool {
		return id == peerA // only A supports e2e
	})
	worker.SetRecipientEncKey(func(_ context.Context, _ string) ([]byte, int32, error) {
		return recipientPub, 1, nil
	})

	sent, failed, deferred := worker.RunOnce(ctx)
	if sent != 1 {
		t.Errorf("sent: got %d want 1 (only peer A emits)", sent)
	}
	if failed+deferred != 0 {
		t.Errorf("no failures/defers expected; failed=%d deferred=%d", failed, deferred)
	}

	// Peer A row: sent + encrypted.
	var stA outboxRowState
	if err := pool.QueryRow(ctx,
		`SELECT status, was_encrypted, refused_reason
		   FROM federation_outbox WHERE activity_id = $1 AND peer_id = $2`,
		activityID, peerA,
	).Scan(&stA.status, &stA.wasEncrypted, &stA.refusedReason); err != nil {
		t.Fatalf("readRow A: %v", err)
	}
	if stA.status != "sent" || !stA.wasEncrypted {
		t.Errorf("peer A row: status=%q wasEncrypted=%v want sent/true", stA.status, stA.wasEncrypted)
	}

	// Peer B row: refused with encryption_required_but_unavailable.
	var stB outboxRowState
	if err := pool.QueryRow(ctx,
		`SELECT status, was_encrypted, refused_reason
		   FROM federation_outbox WHERE activity_id = $1 AND peer_id = $2`,
		activityID, peerB,
	).Scan(&stB.status, &stB.wasEncrypted, &stB.refusedReason); err != nil {
		t.Fatalf("readRow B: %v", err)
	}
	if stB.status != "refused" {
		t.Errorf("peer B row: status=%q want refused", stB.status)
	}

	// Audit: 1 refused event for peer B, 0 for peer A.
	if got := countAuditEvents(t, pool,
		"federation.emission.refused", "peer_id", peerB.String()); got < 1 {
		t.Errorf("peer B refused audit count: got %d want >=1", got)
	}
	if got := countAuditEvents(t, pool,
		"federation.emission.refused", "peer_id", peerA.String()); got != 0 {
		t.Errorf("peer A refused audit count: got %d want 0 (A emitted, no refusal)", got)
	}

	// Peer B's fake inbox should NOT have received a POST.
	srvBMu.Lock()
	defer srvBMu.Unlock()
	if postedToB != 0 {
		t.Errorf("peer B fake inbox got %d POSTs; want 0 (refusal means no POST)", postedToB)
	}
}

// fixedSigner is a minimal stand-in for the IdentitySigner —
// produces a placeholder signature header so the worker's
// HTTPSigner contract is satisfied. The fake inboxes here don't
// verify so the signature value is irrelevant; only the header
// presence + Sign-call success matters.
type fixedSigner struct {
	keyURL string
}

func (s *fixedSigner) Sign(req *http.Request, _ []byte) error {
	req.Header.Set("Signature", `keyId="`+s.keyURL+`",signature="AAAA"`)
	return nil
}

func (s *fixedSigner) KeyID() string { return s.keyURL }

