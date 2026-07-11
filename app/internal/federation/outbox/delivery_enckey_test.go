// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-c-3 outbound emission tests. Verifies the
// LEFT JOIN federation_user_keys path in buildEnvelope:
//
//   - User has a current X25519 key → envelope.Extra carries
//     the aa:encryptionPublicKey block with type discriminator,
//     32-byte base64 key, and version
//   - User has no current key (system-generated activity or
//     pre-I-b leftover) → envelope.Extra omits the block

package outbox_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

func randomEncKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

// seedFederationUserKey inserts a current (is_current=true)
// federation_user_keys row for the given user. atrest doesn't
// run here — we write a dummy private_key_enc that satisfies the
// migration's CHECK constraint (length >= 13). The public key
// is what gets emitted in the envelope; the private key is
// irrelevant to this test.
func seedFederationUserKey(t *testing.T, ctx context.Context, pool interface {
	Exec(context.Context, string, ...interface{}) (interface{}, error)
}, userRef int64, publicKey []byte, version int32) {
	// keep the import edge clean — use the pool's Exec via the
	// pgx interface
}

func TestBuildEnvelope_IncludesEncryptionKeyWhenUserHasOne(t *testing.T) {
	pool := openTestPool(t)
	var capturedBody []byte
	worker, _, _, grantorRef, _, _ := newDeliveryFixture(t, http.StatusAccepted, &capturedBody)

	// Insert a current federation_user_keys row for the
	// grantorRef. The migration's CHECK constraints require
	// public_key length 32 + private_key_enc length >= 13.
	wantKey := randomEncKey(t)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO federation_user_keys (
		    user_ref, version, algorithm, public_key, private_key_enc, is_current
		 ) VALUES ($1, 1, 'naclbox-x25519-v1', $2, $3, TRUE)
		 ON CONFLICT (user_ref, version) DO UPDATE
		 SET public_key = EXCLUDED.public_key, is_current = TRUE`,
		grantorRef, wantKey, make([]byte, 13),
	); err != nil {
		t.Fatalf("seed federation_user_keys: %v", err)
	}

	// Sanity: the row exists + the LEFT JOIN finds it. If this
	// fails, the body check below would falsely look like an
	// emission bug.
	var joinKey []byte
	var joinVer *int32
	if err := pool.QueryRow(context.Background(), `
		SELECT fuk.public_key, fuk.version
		  FROM "user" u
		  LEFT JOIN federation_user_keys fuk
		    ON fuk.user_ref = u.ref AND fuk.is_current = TRUE
		 WHERE u.ref = $1`, grantorRef,
	).Scan(&joinKey, &joinVer); err != nil {
		t.Fatalf("sanity join: %v", err)
	}
	if len(joinKey) != 32 || joinVer == nil || *joinVer != 1 {
		t.Fatalf("sanity: key seed didn't take; keylen=%d ver=%v", len(joinKey), joinVer)
	}

	worker.RunOnce(context.Background())

	if len(capturedBody) == 0 {
		t.Fatal("envelope body not captured")
	}
	// Parse and inspect the encryption-key block.
	var env map[string]json.RawMessage
	if err := json.Unmarshal(capturedBody, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v body=%s", err, string(capturedBody))
	}
	raw, ok := env[federation.PropEncryptionPublicKey]
	if !ok {
		t.Fatalf("envelope missing %s field; body=%s",
			federation.PropEncryptionPublicKey, string(capturedBody))
	}
	var block struct {
		Type            string `json:"type"`
		PublicKeyBase64 string `json:"publicKeyBase64"`
		Version         int32  `json:"version"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatalf("block unmarshal: %v raw=%s", err, string(raw))
	}
	if block.Type != federation.TypeX25519PublicKey {
		t.Errorf("type = %q, want %q", block.Type, federation.TypeX25519PublicKey)
	}
	if block.Version != 1 {
		t.Errorf("version = %d, want 1", block.Version)
	}
	decoded, err := base64.StdEncoding.DecodeString(block.PublicKeyBase64)
	if err != nil {
		t.Fatalf("publicKeyBase64 decode: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("decoded len = %d, want 32", len(decoded))
	}
	for i := range wantKey {
		if decoded[i] != wantKey[i] {
			t.Errorf("public key byte %d: got %02x want %02x", i, decoded[i], wantKey[i])
			break
		}
	}
}

func TestBuildEnvelope_OmitsEncryptionKeyWhenUserHasNone(t *testing.T) {
	// The fixture inserts a fresh user via raw SQL without
	// federation_user_keys row. The LEFT JOIN in buildEnvelope
	// returns NULL for the key columns; the block is omitted
	// from env.Extra.
	//
	// Phase 1.22.I-b's boot-time backfill sweep
	// ([userkeys.BackfillMissingKeys]) mints a keypair for any
	// approved+keyless user. Across-package `go test ./...`
	// concurrency means the userkeys backfill tests can sweep
	// the fixture's user between INSERT and worker.RunOnce —
	// the test's "no key" precondition needs to be re-asserted
	// at the exact moment of dispatch. A defensive DELETE
	// scopes the assertion to "given the user has no key when
	// the envelope is built, the block is omitted" — which is
	// the actual contract under test, independent of when the
	// I-b sweep happens to fire.
	pool := openTestPool(t)
	var capturedBody []byte
	worker, _, _, grantorRef, _, _ := newDeliveryFixture(t, http.StatusAccepted, &capturedBody)

	if _, err := pool.Exec(context.Background(),
		`DELETE FROM federation_user_keys WHERE user_ref = $1`, grantorRef,
	); err != nil {
		t.Fatalf("ensure keyless precondition: %v", err)
	}

	worker.RunOnce(context.Background())

	if len(capturedBody) == 0 {
		t.Fatal("envelope body not captured")
	}
	if contains(string(capturedBody), `"`+federation.PropEncryptionPublicKey+`":`) {
		t.Errorf("envelope unexpectedly carries %s; body=%s",
			federation.PropEncryptionPublicKey, string(capturedBody))
	}
}
