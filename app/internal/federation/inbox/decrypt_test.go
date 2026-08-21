// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Integration tests for the Phase 1.22.I-f retained-key fallback
// walk in [DecryptForUser]. Real Postgres (skips without
// AA_DB_PASSWORD); the userkeys.ListUserKeysForDecrypt query
// + the atrest unwrap path are both exercised end to end.

package inbox_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := testdb.Name(t)
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// envOr lives in handler_test.go in this same package — reuse.

// initAtrestForDecryptTest seeds atrest with a throwaway master key
// so userkeys.Generate / Unwrap work. Idempotent.
func initAtrestForDecryptTest(t *testing.T) {
	t.Helper()
	if atrest.Initialised() {
		return
	}
	key := make([]byte, atrest.MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("seed master key: %v", err)
	}
	if err := atrest.InitWithKey(key); err != nil {
		t.Fatalf("atrest init: %v", err)
	}
}

// fixtureUser inserts a throwaway user + returns the ref. Cleanup
// CASCADEs to drop any federation_user_keys rows.
func fixtureUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	username := "decrypt-test-" + hex.EncodeToString(suffix)
	var ref int64
	err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username) VALUES ($1) RETURNING ref`,
		username,
	).Scan(&ref)
	if err != nil {
		t.Fatalf("fixture user: %v", err)
	}
	t.Cleanup(func() {
		// Cleanup runs after the test's context is cancelled, so this
		// keeps its own deadline — t.Context() here would be dead on
		// arrival and the cleanup a silent no-op (#622).
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, _ = pool.Exec(ctx2, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// seedUserKey inserts a single federation_user_keys row for the
// user. Returns the row's public key bytes (needed for the
// sender-pub argument when the test acts as the sender too).
//
// is_current=true → the "current" key. Pass false + retained_until
// to model a retained-from-rotation key.
func seedUserKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userRef int64, version int32, isCurrent bool, retainedUntil time.Time) (publicKey, privKeyBytes []byte) {
	t.Helper()
	initAtrestForDecryptTest(t)
	pub, wrappedPriv, err := userkeys.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	priv, err := userkeys.Unwrap(wrappedPriv)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	var retainedArg any = nil
	if !retainedUntil.IsZero() {
		retainedArg = retainedUntil
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO federation_user_keys (
		    user_ref, version, algorithm, public_key, private_key_enc, is_current, retained_until
		) VALUES ($1, $2, 'naclbox-x25519-v1', $3, $4, $5, $6)`,
		userRef, version, pub, wrappedPriv, isCurrent, retainedArg,
	); err != nil {
		t.Fatalf("seed federation_user_keys: %v", err)
	}
	return pub, priv.Bytes()
}

// encryptForRecipient seals plaintext as the sender, returning
// the nonce + ciphertext the dispatcher would store on
// env.Encryption. Sender keypair is freshly minted per test.
func encryptForRecipient(t *testing.T, plaintext, recipientPub []byte) (senderPub, nonce, ciphertext []byte) {
	t.Helper()
	initAtrestForDecryptTest(t)
	senderPub, wrappedSenderPriv, err := userkeys.Generate()
	if err != nil {
		t.Fatalf("sender Generate: %v", err)
	}
	senderPriv, err := userkeys.Unwrap(wrappedSenderPriv)
	if err != nil {
		t.Fatalf("sender Unwrap: %v", err)
	}
	senderPrivBytes := senderPriv.Bytes()
	nonce, ciphertext, err = federation.EncryptActivityPayload(plaintext, senderPrivBytes, recipientPub)
	if err != nil {
		t.Fatalf("EncryptActivityPayload: %v", err)
	}
	return senderPub, nonce, ciphertext
}

// --- happy path: current key works -----------------------------------

func TestDecryptForUser_CurrentKey_AttemptCountOne(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	userRef := fixtureUser(t, ctx, pool)

	// Seed the user's CURRENT key.
	recipientPub, _ := seedUserKey(t, ctx, pool, userRef, 1, true, time.Time{})

	plaintext := []byte(`{"content":"hi"}`)
	senderPub, nonce, ciphertext := encryptForRecipient(t, plaintext, recipientPub)

	res, err := inbox.DecryptForUser(ctx, pool, userRef, senderPub, nonce, ciphertext)
	if err != nil {
		t.Fatalf("DecryptForUser: %v", err)
	}
	if string(res.Plaintext) != string(plaintext) {
		t.Errorf("Plaintext = %q, want %q", res.Plaintext, plaintext)
	}
	if res.DecryptedWithKeyVersion != 1 {
		t.Errorf("DecryptedWithKeyVersion = %d, want 1", res.DecryptedWithKeyVersion)
	}
	if res.AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1", res.AttemptCount)
	}
}

// --- retained-key fallback ----------------------------------------

func TestDecryptForUser_RetainedKey_FallbackAttemptCountTwo(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	userRef := fixtureUser(t, ctx, pool)

	// Two keys: v1 retained (the sender used this one before our
	// rotation), v2 current (post-rotation). The sender's
	// in-flight envelope was encrypted to v1's public key.
	v1Pub, _ := seedUserKey(t, ctx, pool, userRef, 1, false, time.Now().Add(7*24*time.Hour))
	_, _ = seedUserKey(t, ctx, pool, userRef, 2, true, time.Time{})

	plaintext := []byte(`{"content":"in-flight during rotation"}`)
	senderPub, nonce, ciphertext := encryptForRecipient(t, plaintext, v1Pub)

	res, err := inbox.DecryptForUser(ctx, pool, userRef, senderPub, nonce, ciphertext)
	if err != nil {
		t.Fatalf("DecryptForUser: %v", err)
	}
	if string(res.Plaintext) != string(plaintext) {
		t.Errorf("Plaintext = %q, want %q", res.Plaintext, plaintext)
	}
	if res.DecryptedWithKeyVersion != 1 {
		t.Errorf("DecryptedWithKeyVersion = %d, want 1 (retained key)", res.DecryptedWithKeyVersion)
	}
	// Attempt #1 is the current key (v2, doesn't match); attempt
	// #2 is the retained v1 (matches). Walk order is is_current
	// DESC first.
	if res.AttemptCount != 2 {
		t.Errorf("AttemptCount = %d, want 2 (current tried first then retained)", res.AttemptCount)
	}
}

func TestDecryptForUser_OldestRetainedKey_AttemptCountIsRowPosition(t *testing.T) {
	// Three retained keys + one current; the sender used the
	// oldest retained. Walk: current → retained v3 → retained v2
	// → retained v1 (success). Attempt count = 4.
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	userRef := fixtureUser(t, ctx, pool)

	v1Pub, _ := seedUserKey(t, ctx, pool, userRef, 1, false, time.Now().Add(7*24*time.Hour))
	_, _ = seedUserKey(t, ctx, pool, userRef, 2, false, time.Now().Add(7*24*time.Hour))
	_, _ = seedUserKey(t, ctx, pool, userRef, 3, false, time.Now().Add(7*24*time.Hour))
	_, _ = seedUserKey(t, ctx, pool, userRef, 4, true, time.Time{})

	plaintext := []byte(`{"content":"oldest sender"}`)
	senderPub, nonce, ciphertext := encryptForRecipient(t, plaintext, v1Pub)

	res, err := inbox.DecryptForUser(ctx, pool, userRef, senderPub, nonce, ciphertext)
	if err != nil {
		t.Fatalf("DecryptForUser: %v", err)
	}
	if res.DecryptedWithKeyVersion != 1 {
		t.Errorf("DecryptedWithKeyVersion = %d, want 1", res.DecryptedWithKeyVersion)
	}
	if res.AttemptCount != 4 {
		t.Errorf("AttemptCount = %d, want 4 (current + 2 newer retained + 1 oldest retained)", res.AttemptCount)
	}
}

// --- terminal failure paths ---------------------------------------

func TestDecryptForUser_NoKeyWorks_ErrEncryptionDecryptFailed(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	userRef := fixtureUser(t, ctx, pool)

	// Recipient has a current key; sender encrypted to a
	// completely different recipient key (we generate two
	// recipient keys + only seed the wrong one).
	_, _ = seedUserKey(t, ctx, pool, userRef, 1, true, time.Time{})
	otherPub, _, err := userkeys.Generate()
	if err != nil {
		t.Fatalf("other Generate: %v", err)
	}
	plaintext := []byte(`{"content":"x"}`)
	senderPub, nonce, ciphertext := encryptForRecipient(t, plaintext, otherPub)

	_, err = inbox.DecryptForUser(ctx, pool, userRef, senderPub, nonce, ciphertext)
	if !errors.Is(err, federation.ErrEncryptionDecryptFailed) {
		t.Errorf("err = %v, want ErrEncryptionDecryptFailed", err)
	}
}

func TestDecryptForUser_NoReceiverKey_TypedError(t *testing.T) {
	// User exists but has no federation_user_keys row at all
	// (post-I-b invariant violation; defensive).
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	userRef := fixtureUser(t, ctx, pool)

	senderPub := make([]byte, federation.NaClBoxKeyLen) // dummy 32-byte; ListUserKeysForDecrypt returns empty before we use it
	nonce := make([]byte, federation.NaClBoxNonceLen)
	ciphertext := make([]byte, 16)
	_, err := inbox.DecryptForUser(ctx, pool, userRef, senderPub, nonce, ciphertext)
	if !errors.Is(err, inbox.ErrNoReceiverKey) {
		t.Errorf("err = %v, want ErrNoReceiverKey", err)
	}
}

func TestDecryptForUser_EmptySenderPubkey_TypedError(t *testing.T) {
	// Pre-I-c sender that never advertised an encryption key
	// surfaces here as `senderPub = nil` from the caller. Distinct
	// error from ErrEncryptionDecryptFailed so the dispatcher
	// can route the right audit reason.
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	userRef := fixtureUser(t, ctx, pool)
	_, _ = seedUserKey(t, ctx, pool, userRef, 1, true, time.Time{})

	_, err := inbox.DecryptForUser(ctx, pool, userRef, nil, make([]byte, 24), make([]byte, 16))
	if !errors.Is(err, inbox.ErrSenderKeyMissing) {
		t.Errorf("err = %v, want ErrSenderKeyMissing", err)
	}
}

// --- tamper detection (regression net) ----------------------------

func TestDecryptForUser_TamperedCiphertext_ErrEncryptionDecryptFailed(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	userRef := fixtureUser(t, ctx, pool)

	pub, _ := seedUserKey(t, ctx, pool, userRef, 1, true, time.Time{})
	plaintext := []byte(`{"content":"x"}`)
	senderPub, nonce, ciphertext := encryptForRecipient(t, plaintext, pub)

	// Tamper.
	ciphertext[len(ciphertext)/2] ^= 0xFF

	_, err := inbox.DecryptForUser(ctx, pool, userRef, senderPub, nonce, ciphertext)
	if !errors.Is(err, federation.ErrEncryptionDecryptFailed) {
		t.Errorf("err = %v, want ErrEncryptionDecryptFailed", err)
	}
}

// --- expired-retention exclusion (1.22.I-h sweeper assumption) ----

func TestDecryptForUser_ExpiredRetainedKey_NotAttempted(t *testing.T) {
	// A retained key past its retained_until cutoff MUST NOT be
	// tried — the I-h sweeper may not have cleaned it up yet,
	// but the query filter is what enforces the rotation grace
	// window contract.
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	userRef := fixtureUser(t, ctx, pool)

	// v1 retained but already EXPIRED (yesterday); v2 current.
	expiredPub, _ := seedUserKey(t, ctx, pool, userRef, 1, false, time.Now().Add(-24*time.Hour))
	_, _ = seedUserKey(t, ctx, pool, userRef, 2, true, time.Time{})

	// Sender encrypted to v1 (expired). Decrypt must fail because
	// the query filter excludes it.
	plaintext := []byte(`{"content":"too late"}`)
	senderPub, nonce, ciphertext := encryptForRecipient(t, plaintext, expiredPub)

	_, err := inbox.DecryptForUser(ctx, pool, userRef, senderPub, nonce, ciphertext)
	if !errors.Is(err, federation.ErrEncryptionDecryptFailed) {
		t.Errorf("err = %v, want ErrEncryptionDecryptFailed (expired retained must not be tried)", err)
	}
}
