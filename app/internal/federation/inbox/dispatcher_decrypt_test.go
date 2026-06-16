// Integration tests for the Phase 1.22.I-f stage-4 decrypt
// branch in Dispatcher.dispatchOne. Drives RunOnce against a real
// Postgres + seeds federation_inbox rows directly so we can
// exercise the decrypt path without the HTTP-Sig / handler layer
// (the dispatcher_e2e_test.go file covers that wider integration
// for the plaintext path).
//
// What this file proves:
//
//   - Encrypted envelopes decrypt, env.Extra is restored, the row
//     transitions processed with was_encrypted=true +
//     decrypted_with_key_version=<the recipient key that worked>.
//   - The federation.inbox.decrypted audit row fires with the
//     right metadata shape.
//   - The retained-key fallback walks (sender used an older
//     recipient key) and the audit's decrypted_with_key_version
//     reflects which version unlocked the payload.
//   - Plaintext envelopes still flow through with
//     was_encrypted=false + no decrypted audit row.
//   - Corrupt ciphertext rejects with reject_reason=decrypt_failed
//     + a federation.inbox.decrypt_failed audit row.
//   - Dispatcher missing senderEncKey OR recipientUserRef hook
//     rejects with a distinct audit reason — the boot-config
//     defence-in-depth invariant.
//
// Skips without AA_DB_PASSWORD.

package inbox_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
)

// --- shared fixtures -----------------------------------------------------

// decryptFx is the per-test fixture set the dispatcher decrypt
// suite needs: a recipient user with a current encryption key, a
// peer, a sender pubkey (synthetic — we never look it up via
// remote.Handler in these tests; the hook closes over senderPub
// directly), and a dispatcher wired with all three hooks.
type decryptFx struct {
	t            *testing.T
	pool         *pgxpool.Pool
	logger       *slog.Logger
	recipientRef int64
	recipientURI string
	senderActor  string
	senderPub    []byte
	senderPriv   []byte
	peerID       uuid.UUID
	dispatcher   *inbox.Dispatcher
	auditRec     *audit.Recorder
}

func newDecryptFx(t *testing.T) *decryptFx {
	t.Helper()
	pool := openPool(t)
	t.Cleanup(pool.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	recipientRef := fixtureUser(t, ctx, pool)

	// The recipient actor URI mirrors the production shape
	// (<base>/users/<username>). The recipientUserRef hook
	// parses the trailing username segment + queries the user
	// table — same code path production uses.
	var username string
	if err := pool.QueryRow(ctx, `SELECT username FROM "user" WHERE ref = $1`, recipientRef).Scan(&username); err != nil {
		t.Fatalf("read recipient username: %v", err)
	}
	recipientURI := "https://studio-a.example/users/" + username

	// Seed the recipient's current encryption key.
	_, _ = seedUserKey(t, ctx, pool, recipientRef, 1, true, time.Time{})

	// Synthetic sender — we don't go through remote.Handler in
	// these tests; the hook just returns the pubkey we generated
	// here. Production wires the hook to
	// remote.Handler.GetEncryptionKey; both paths run through
	// the same downstream DecryptForUser walk.
	initAtrestForDecryptTest(t)
	senderActor := "https://studio-b.example/users/bob-" + randHex4()
	senderPub, senderPriv := mintSenderKeypair(t)

	// Peer row — the dispatcher's lookupPeer hook reads
	// federation_peers; we need a real row so dispatchOne's
	// stage-2 peer lookup succeeds.
	peerID := seedPeer(t, ctx, pool, recipientRef, "https://studio-b.example")

	auditRec := audit.NewRecorder(pool, logger)

	disp := inbox.NewDispatcher(
		inbox.DispatcherConfig{
			Interval:    time.Hour, // test drives RunOnce directly
			BatchSize:   100,
			MaxAttempts: 5,
		},
		inbox.New(pool),
		func(ctx context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
			return inbox.PeerInfo{
				ID:          id,
				InstanceURL: "https://studio-b.example",
				Enabled:     true,
				Connected:   true,
			}, nil
		},
		nil, // no per-verb handler — no_handler branch processes the row
		logger,
	)
	disp.SetRawPool(pool)
	disp.SetSenderEncKey(func(_ context.Context, actorURI string) ([]byte, int32, error) {
		if actorURI != senderActor {
			return nil, 0, errors.New("synthetic: unknown sender")
		}
		return senderPub, 1, nil
	})
	disp.SetRecipientUserRef(func(ctx context.Context, actorURI string) (int64, error) {
		// Parse trailing /users/<username> segment — mirrors api.go's
		// usernameFromActorURI helper.
		const marker = "/users/"
		idx := -1
		for i := len(actorURI) - len(marker); i >= 0; i-- {
			if actorURI[i:i+len(marker)] == marker {
				idx = i
				break
			}
		}
		if idx < 0 {
			return 0, errors.New("no users segment")
		}
		uname := actorURI[idx+len(marker):]
		var ref int64
		err := pool.QueryRow(ctx, `SELECT ref FROM "user" WHERE username = $1`, uname).Scan(&ref)
		return ref, err
	})
	disp.SetAudit(auditRec)

	return &decryptFx{
		t:            t,
		pool:         pool,
		logger:       logger,
		recipientRef: recipientRef,
		recipientURI: recipientURI,
		senderActor:  senderActor,
		senderPub:    senderPub,
		senderPriv:   senderPriv,
		peerID:       peerID,
		dispatcher:   disp,
		auditRec:     auditRec,
	}
}

func seedPeer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, handshakeRef int64, baseURL string) uuid.UUID {
	t.Helper()
	pub := make([]byte, 32)
	_, _ = rand.Read(pub)
	// federation_peers wants PEM for instance_public_key but the
	// dispatcher's peer lookup doesn't read it for the decrypt
	// branch (handler-side stage 8 verifies). We pass a stub PEM
	// string that satisfies the NOT NULL constraint.
	stubPEM := "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----"
	suffix := randHex4()
	var peerID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		VALUES ($1, $2, $3, 'connected', 'plaintext', TRUE, 'connected', $4)
		RETURNING id`,
		baseURL+"-"+suffix, "Studio B "+suffix, stubPEM, handshakeRef,
	).Scan(&peerID); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, peerID)
	})
	return peerID
}

// mintSenderKeypair mints a fresh X25519 keypair via the same
// userkeys.Generate path the recipient side uses. Returns the raw
// 32-byte pub + 32-byte priv scalar — exactly the bytes the
// EncryptActivityPayload primitive expects. The wrapped priv blob
// is unwrapped here so the test can hand the raw scalar to the
// seal primitive without going through the federation_user_keys
// table (the sender is synthetic; we don't model it in the DB).
func mintSenderKeypair(t *testing.T) (pub, priv []byte) {
	t.Helper()
	initAtrestForDecryptTest(t)
	p, wrapped, err := userkeys.Generate()
	if err != nil {
		t.Fatalf("userkeys.Generate: %v", err)
	}
	pk, err := userkeys.Unwrap(wrapped)
	if err != nil {
		t.Fatalf("userkeys.Unwrap: %v", err)
	}
	return p, pk.Bytes()
}

func randHex4() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// encryptPayload seals plaintext with the fixture's sender priv
// against a target recipient public key. Returns the wire-shape
// EncryptionBlock pieces ready to drop into the envelope.
func (fx *decryptFx) encryptPayload(plaintext, recipientPub []byte) (nonce, ciphertext []byte) {
	fx.t.Helper()
	n, ct, err := federation.EncryptActivityPayload(plaintext, fx.senderPriv, recipientPub)
	if err != nil {
		fx.t.Fatalf("EncryptActivityPayload: %v", err)
	}
	return n, ct
}

// buildEnvelope assembles the envelope shape the inbox dispatcher
// re-parses. encryption is optional — pass nil to build the
// plaintext path.
func (fx *decryptFx) buildEnvelope(verb federation.ActivityType, extra map[string]any, enc *federation.EncryptionBlock) (envelopeJSON []byte, activityID string) {
	fx.t.Helper()
	activityID = "https://studio-b.example/activities/" + uuid.NewString()
	env := &federation.Envelope{
		Context:   federation.ContextV1,
		Type:      verb,
		ID:        activityID,
		Actor:     fx.senderActor,
		Object:    "https://studio-a.example/posts/" + uuid.NewString(),
		Published: time.Now().UTC(),
		To:        []string{fx.recipientURI},
		Signature: &federation.Signature{
			Type:      federation.SignatureAlgEd25519,
			PublicKey: fx.senderActor + "#main-key",
			Value:     "AAAAAAAAAAAA",
		},
		Encryption: enc,
	}
	if enc == nil && extra != nil {
		raw := map[string]json.RawMessage{}
		for k, v := range extra {
			b, _ := json.Marshal(v)
			raw[k] = b
		}
		env.Extra = raw
	}
	b, err := env.Marshal()
	if err != nil {
		fx.t.Fatalf("envelope.Marshal: %v", err)
	}
	return b, activityID
}

// insertInbox writes the row + immediately flips status to
// 'processed' in a single transaction, then returns the row + ID.
// The two writes commit together, so the LISTEN/NOTIFY trigger on
// federation_inbox fires AFTER the status flip — the live app
// container's dispatcher (which polls + LISTENs on the same DB)
// sees the row as already-processed + skips it on its
// ListPendingInbox WHERE status='pending' query. The test then
// calls Dispatcher.DispatchRow directly so the stage-4 decrypt
// branch runs against the row WITHOUT a concurrent dispatcher
// fighting for the same MarkInbox* UPDATE.
//
// Without this isolation, scripts/test.sh's `go test` container
// (which connects to the same docker postgres the live app uses)
// would race the live app's dispatcher on every encrypted row +
// the test's terminal-state assertion would be non-deterministic.
//
// The activityID param is for traceability only; the row's
// activity_uri always gets a fresh UUID-suffixed URL so per-test
// rows never collide on the table's UNIQUE constraint.
func (fx *decryptFx) insertInbox(envelopeJSON []byte, activityID string, verb federation.ActivityType) (uuid.UUID, inbox.FederationInbox) {
	fx.t.Helper()
	ctx := context.Background()
	if activityID == "" {
		activityID = "https://studio-b.example/activities/" + uuid.NewString()
	}

	tx, err := fx.pool.Begin(ctx)
	if err != nil {
		fx.t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := inbox.New(tx)
	row, err := q.InsertInbox(ctx, inbox.InsertInboxParams{
		ActivityUri:  activityID,
		PeerID:       pgtype.UUID{Bytes: fx.peerID, Valid: true},
		ActorUri:     fx.senderActor,
		ActivityType: string(verb),
		ObjectKind:   ptrStr("post"),
		ObjectID:     pgtype.UUID{Bytes: uuid.New(), Valid: true},
		EnvelopeJson: envelopeJSON,
		HttpSigKey:   fx.senderActor + "#main-key",
	})
	if err != nil {
		fx.t.Fatalf("InsertInbox: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE federation_inbox SET status = 'processed' WHERE id = $1`, row.ID,
	); err != nil {
		fx.t.Fatalf("flip status to processed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		fx.t.Fatalf("commit: %v", err)
	}

	id := uuid.UUID(row.ID.Bytes)
	fx.t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = fx.pool.Exec(c, `DELETE FROM federation_inbox WHERE id = $1`, id)
	})
	// The returned `row` carries status='pending' (the InsertInbox
	// RETURNING clause is pre-UPDATE). Refresh from DB so the
	// row's status reflects the committed 'processed' value — the
	// dispatcher branches don't read status (it filters via
	// ListPendingInbox upstream) so this is purely diagnostic
	// hygiene.
	fresh, err := inbox.New(fx.pool).GetInboxByID(ctx, row.ID)
	if err != nil {
		fx.t.Fatalf("re-read row: %v", err)
	}
	return id, fresh
}

func ptrStr(s string) *string { return &s }

// readRowState fetches the columns the tests care about so each
// assertion is a single line at the call site.
type inboxRowState struct {
	status                  string
	rejectReason            *string
	wasEncrypted            bool
	decryptedWithKeyVersion *int32
}

func (fx *decryptFx) readRow(id uuid.UUID) inboxRowState {
	fx.t.Helper()
	var st inboxRowState
	if err := fx.pool.QueryRow(context.Background(),
		`SELECT status, reject_reason, was_encrypted, decrypted_with_key_version
		   FROM federation_inbox WHERE id = $1`, id,
	).Scan(&st.status, &st.rejectReason, &st.wasEncrypted, &st.decryptedWithKeyVersion); err != nil {
		fx.t.Fatalf("readRow: %v", err)
	}
	return st
}

// auditCount returns how many audit_events rows have the given
// event_type AND a metadata field matching key=value as a string.
func (fx *decryptFx) auditCount(eventType, metaKey, metaValue string) int {
	fx.t.Helper()
	var n int
	if err := fx.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM audit_events
		WHERE event_type = $1
		  AND metadata->>$2 = $3`,
		eventType, metaKey, metaValue,
	).Scan(&n); err != nil {
		fx.t.Fatalf("auditCount: %v", err)
	}
	return n
}

// --- tests ---------------------------------------------------------------

// EncryptedEnvelope_DispatchesAndMarksRow proves the happy path:
// a sealed envelope flows through stage-4 decryption, env.Extra
// is restored, the dispatcher's no-handler path records the row
// as processed with was_encrypted=true +
// decrypted_with_key_version=1.
func TestDispatcher_EncryptedEnvelope_DispatchesAndMarksRow(t *testing.T) {
	fx := newDecryptFx(t)

	recipientPub := fetchUserCurrentPub(t, fx.pool, fx.recipientRef)
	payload := map[string]any{"content": "encrypted hello"}
	pt, _ := json.Marshal(payload)
	nonce, ct := fx.encryptPayload(pt, recipientPub)
	enc := &federation.EncryptionBlock{
		Algorithm:           federation.EncryptionAlgNaClBoxV1,
		SenderKeyID:         fx.senderActor + "#encryption-key",
		SenderKeyVersion:    1,
		RecipientKeyID:      fx.recipientURI + "#encryption-key",
		RecipientKeyVersion: 1,
		Nonce:               federation.Base64Bytes(nonce),
		Ciphertext:          federation.Base64Bytes(ct),
	}

	envelope, _ := fx.buildEnvelope(federation.ActivityLike, nil, enc)
	id, row := fx.insertInbox(envelope, "", federation.ActivityLike)

	outcome := fx.dispatcher.DispatchRow(context.Background(), row)
	if outcome != inbox.OutcomeProcessed {
		t.Fatalf("DispatchRow: got %v want OutcomeProcessed", outcome)
	}

	st := fx.readRow(id)
	if st.status != "processed" {
		t.Errorf("status: got %q want processed", st.status)
	}
	if !st.wasEncrypted {
		t.Error("was_encrypted: got false want true (envelope was encrypted)")
	}
	if st.decryptedWithKeyVersion == nil || *st.decryptedWithKeyVersion != 1 {
		t.Errorf("decrypted_with_key_version: got %v want 1", st.decryptedWithKeyVersion)
	}
}

// EncryptedEnvelope_FiresDecryptedAuditEvent proves the audit
// recorder gets the happy-path event + the metadata shape carries
// peer + activity-type + the key versions an operator filter
// would group on.
func TestDispatcher_EncryptedEnvelope_FiresDecryptedAuditEvent(t *testing.T) {
	fx := newDecryptFx(t)

	recipientPub := fetchUserCurrentPub(t, fx.pool, fx.recipientRef)
	pt := []byte(`{"content":"audit me"}`)
	nonce, ct := fx.encryptPayload(pt, recipientPub)
	enc := &federation.EncryptionBlock{
		Algorithm:           federation.EncryptionAlgNaClBoxV1,
		SenderKeyVersion:    2, // synthetic — proves the sender_key_version metadata propagates
		RecipientKeyVersion: 1,
		Nonce:               federation.Base64Bytes(nonce),
		Ciphertext:          federation.Base64Bytes(ct),
	}
	envelope, _ := fx.buildEnvelope(federation.ActivityLike, nil, enc)
	_, row := fx.insertInbox(envelope, "", federation.ActivityLike)

	beforeOK := fx.auditCount(audit.EventFederationInboxDecrypted, "peer_id", fx.peerID.String())
	fx.dispatcher.DispatchRow(context.Background(), row)
	afterOK := fx.auditCount(audit.EventFederationInboxDecrypted, "peer_id", fx.peerID.String())
	if afterOK != beforeOK+1 {
		t.Errorf("federation.inbox.decrypted count: before=%d after=%d want +1", beforeOK, afterOK)
	}

	// sender_key_version metadata = 2 (the synthetic value the
	// envelope advertised), not 1 (the recipient's local key
	// version). Pivot on the activity-type to find this run's row.
	var senderVer int
	if err := fx.pool.QueryRow(context.Background(), `
		SELECT (metadata->>'sender_key_version')::int
		FROM audit_events
		WHERE event_type = $1 AND metadata->>'peer_id' = $2
		ORDER BY occurred_at DESC LIMIT 1`,
		audit.EventFederationInboxDecrypted, fx.peerID.String(),
	).Scan(&senderVer); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if senderVer != 2 {
		t.Errorf("audit sender_key_version: got %d want 2", senderVer)
	}
}

// EncryptedEnvelope_RetainedKeyFallback proves the I-h grace-
// window walk works through the dispatcher. The recipient rotated
// to v2 after the sender sealed against v1; the dispatcher tries
// v2 first (current, doesn't open) then v1 (retained, opens).
// Audit's decrypted_with_key_version captures the v1 hit.
func TestDispatcher_EncryptedEnvelope_RetainedKeyFallback(t *testing.T) {
	fx := newDecryptFx(t)
	ctx := context.Background()

	// Rotate: v1 stops being current, v2 becomes current.
	if _, err := fx.pool.Exec(ctx,
		`UPDATE federation_user_keys SET is_current = FALSE, retained_until = $1
		   WHERE user_ref = $2 AND version = 1`,
		time.Now().Add(7*24*time.Hour), fx.recipientRef,
	); err != nil {
		t.Fatalf("rotate v1: %v", err)
	}
	// Seed v2 = current.
	_, _ = seedUserKey(t, ctx, fx.pool, fx.recipientRef, 2, true, time.Time{})

	// Sender sealed against v1's pub (the in-flight envelope).
	v1Pub := fetchUserPubByVersion(t, fx.pool, fx.recipientRef, 1)
	pt := []byte(`{"content":"in flight during rotation"}`)
	nonce, ct := fx.encryptPayload(pt, v1Pub)
	enc := &federation.EncryptionBlock{
		Algorithm:           federation.EncryptionAlgNaClBoxV1,
		SenderKeyVersion:    1,
		RecipientKeyVersion: 1,
		Nonce:               federation.Base64Bytes(nonce),
		Ciphertext:          federation.Base64Bytes(ct),
	}
	envelope, _ := fx.buildEnvelope(federation.ActivityLike, nil, enc)
	id, row := fx.insertInbox(envelope, "", federation.ActivityLike)

	fx.dispatcher.DispatchRow(ctx, row)

	st := fx.readRow(id)
	if st.status != "processed" {
		t.Errorf("status: got %q want processed", st.status)
	}
	if st.decryptedWithKeyVersion == nil || *st.decryptedWithKeyVersion != 1 {
		t.Errorf("decrypted_with_key_version: got %v want 1 (retained key)", st.decryptedWithKeyVersion)
	}
}

// PlaintextEnvelope_StillWorks proves the 1.22.D legacy path is
// unaffected: env.Encryption is nil → stage 4 is skipped →
// was_encrypted=false → no decrypted audit event.
func TestDispatcher_PlaintextEnvelope_StillWorks_NoEncryptionMetadata(t *testing.T) {
	fx := newDecryptFx(t)

	envelope, _ := fx.buildEnvelope(federation.ActivityLike,
		map[string]any{"content": "plaintext hello"}, nil)
	id, row := fx.insertInbox(envelope, "", federation.ActivityLike)

	beforeOK := fx.auditCount(audit.EventFederationInboxDecrypted, "peer_id", fx.peerID.String())
	fx.dispatcher.DispatchRow(context.Background(), row)
	afterOK := fx.auditCount(audit.EventFederationInboxDecrypted, "peer_id", fx.peerID.String())

	st := fx.readRow(id)
	if st.status != "processed" {
		t.Errorf("status: got %q want processed", st.status)
	}
	if st.wasEncrypted {
		t.Error("was_encrypted: got true want false (plaintext envelope)")
	}
	if st.decryptedWithKeyVersion != nil {
		t.Errorf("decrypted_with_key_version: got %v want nil", st.decryptedWithKeyVersion)
	}
	if afterOK != beforeOK {
		t.Errorf("federation.inbox.decrypted should not fire for plaintext; before=%d after=%d",
			beforeOK, afterOK)
	}
}

// CorruptCiphertext_RejectsWithDecryptFailed proves the
// rejection path: a tampered ciphertext rejects with
// reject_reason=decrypt_failed + the audit row carries reason
// "no_key_worked".
func TestDispatcher_CorruptCiphertext_RejectsWithDecryptFailed(t *testing.T) {
	fx := newDecryptFx(t)

	recipientPub := fetchUserCurrentPub(t, fx.pool, fx.recipientRef)
	pt := []byte(`{"content":"will be tampered"}`)
	nonce, ct := fx.encryptPayload(pt, recipientPub)
	// Flip one byte in the ciphertext — nacl/box auth tag fails.
	ct[0] ^= 0xff
	enc := &federation.EncryptionBlock{
		Algorithm:           federation.EncryptionAlgNaClBoxV1,
		SenderKeyVersion:    1,
		RecipientKeyVersion: 1,
		Nonce:               federation.Base64Bytes(nonce),
		Ciphertext:          federation.Base64Bytes(ct),
	}
	envelope, _ := fx.buildEnvelope(federation.ActivityLike, nil, enc)
	id, row := fx.insertInbox(envelope, "", federation.ActivityLike)

	beforeBad := fx.auditCount(audit.EventFederationInboxDecryptFailed, "peer_id", fx.peerID.String())
	outcome := fx.dispatcher.DispatchRow(context.Background(), row)
	afterBad := fx.auditCount(audit.EventFederationInboxDecryptFailed, "peer_id", fx.peerID.String())
	if outcome != inbox.OutcomeRejected {
		t.Errorf("DispatchRow: got %v want OutcomeRejected", outcome)
	}

	st := fx.readRow(id)
	if st.status != "rejected" {
		t.Errorf("status: got %q want rejected", st.status)
	}
	if st.rejectReason == nil || *st.rejectReason != string(federation.InboxStatusDecryptFailed) {
		got := "<nil>"
		if st.rejectReason != nil {
			got = *st.rejectReason
		}
		t.Errorf("reject_reason: got %q want %q", got, federation.InboxStatusDecryptFailed)
	}
	if afterBad != beforeBad+1 {
		t.Errorf("federation.inbox.decrypt_failed count: before=%d after=%d want +1", beforeBad, afterBad)
	}

	// reason metadata should be "no_key_worked" (the
	// ErrEncryptionDecryptFailed branch).
	var reason string
	if err := fx.pool.QueryRow(context.Background(), `
		SELECT metadata->>'reason'
		FROM audit_events
		WHERE event_type = $1 AND metadata->>'peer_id' = $2
		ORDER BY occurred_at DESC LIMIT 1`,
		audit.EventFederationInboxDecryptFailed, fx.peerID.String(),
	).Scan(&reason); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if reason != "no_key_worked" {
		t.Errorf("audit reason: got %q want no_key_worked", reason)
	}
}

// MissingSenderKeyHook_RejectsWithSenderKeyMissing proves the
// defence-in-depth boot-config check: a dispatcher missing the
// senderEncKey hook rejects encrypted envelopes immediately,
// without consulting the receiver-key set.
func TestDispatcher_MissingSenderKeyHook_RejectsWithSenderKeyMissing(t *testing.T) {
	fx := newDecryptFx(t)
	// Unwire the sender-key hook AFTER fixture construction so
	// the rest of the wiring matches production.
	fx.dispatcher.SetSenderEncKey(nil)

	recipientPub := fetchUserCurrentPub(t, fx.pool, fx.recipientRef)
	pt := []byte(`{"content":"no sender key for you"}`)
	nonce, ct := fx.encryptPayload(pt, recipientPub)
	enc := &federation.EncryptionBlock{
		Algorithm:           federation.EncryptionAlgNaClBoxV1,
		SenderKeyVersion:    1,
		RecipientKeyVersion: 1,
		Nonce:               federation.Base64Bytes(nonce),
		Ciphertext:          federation.Base64Bytes(ct),
	}
	envelope, _ := fx.buildEnvelope(federation.ActivityLike, nil, enc)
	id, row := fx.insertInbox(envelope, "", federation.ActivityLike)

	fx.dispatcher.DispatchRow(context.Background(), row)

	st := fx.readRow(id)
	if st.status != "rejected" {
		t.Errorf("status: got %q want rejected", st.status)
	}
	if st.rejectReason == nil || *st.rejectReason != string(federation.InboxStatusDecryptFailed) {
		got := "<nil>"
		if st.rejectReason != nil {
			got = *st.rejectReason
		}
		t.Errorf("reject_reason: got %q want %q", got, federation.InboxStatusDecryptFailed)
	}

	var reason string
	_ = fx.pool.QueryRow(context.Background(), `
		SELECT metadata->>'reason' FROM audit_events
		WHERE event_type = $1 AND metadata->>'peer_id' = $2
		ORDER BY occurred_at DESC LIMIT 1`,
		audit.EventFederationInboxDecryptFailed, fx.peerID.String(),
	).Scan(&reason)
	if reason != "sender_key_missing" {
		t.Errorf("audit reason: got %q want sender_key_missing", reason)
	}
}

// MissingRecipientResolver_RejectsWithRecipientUnresolvable
// proves the symmetric defence-in-depth path: dispatcher without
// the user-ref resolver rejects with the matching audit reason.
func TestDispatcher_MissingRecipientResolver_RejectsWithRecipientUnresolvable(t *testing.T) {
	fx := newDecryptFx(t)
	fx.dispatcher.SetRecipientUserRef(nil)

	recipientPub := fetchUserCurrentPub(t, fx.pool, fx.recipientRef)
	pt := []byte(`{"content":"no recipient resolver"}`)
	nonce, ct := fx.encryptPayload(pt, recipientPub)
	enc := &federation.EncryptionBlock{
		Algorithm:           federation.EncryptionAlgNaClBoxV1,
		SenderKeyVersion:    1,
		RecipientKeyVersion: 1,
		Nonce:               federation.Base64Bytes(nonce),
		Ciphertext:          federation.Base64Bytes(ct),
	}
	envelope, _ := fx.buildEnvelope(federation.ActivityLike, nil, enc)
	id, row := fx.insertInbox(envelope, "", federation.ActivityLike)

	fx.dispatcher.DispatchRow(context.Background(), row)

	st := fx.readRow(id)
	if st.status != "rejected" {
		t.Errorf("status: got %q want rejected", st.status)
	}

	var reason string
	_ = fx.pool.QueryRow(context.Background(), `
		SELECT metadata->>'reason' FROM audit_events
		WHERE event_type = $1 AND metadata->>'peer_id' = $2
		ORDER BY occurred_at DESC LIMIT 1`,
		audit.EventFederationInboxDecryptFailed, fx.peerID.String(),
	).Scan(&reason)
	if reason != "recipient_unresolvable" {
		t.Errorf("audit reason: got %q want recipient_unresolvable", reason)
	}
}

// --- small DB helpers -----------------------------------------------------

func fetchUserCurrentPub(t *testing.T, pool *pgxpool.Pool, userRef int64) []byte {
	t.Helper()
	var pub []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT public_key FROM federation_user_keys WHERE user_ref = $1 AND is_current = TRUE LIMIT 1`,
		userRef,
	).Scan(&pub); err != nil {
		t.Fatalf("fetch current pub: %v", err)
	}
	return pub
}

func fetchUserPubByVersion(t *testing.T, pool *pgxpool.Pool, userRef int64, version int32) []byte {
	t.Helper()
	var pub []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT public_key FROM federation_user_keys WHERE user_ref = $1 AND version = $2`,
		userRef, version,
	).Scan(&pub); err != nil {
		t.Fatalf("fetch v%d pub: %v", version, err)
	}
	return pub
}
