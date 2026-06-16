// Integration tests for federation/userkeys SQL — exercises the
// sqlc-generated queries against a live Postgres so the migration's
// schema invariants (partial unique on is_current, current-XOR-
// retained CHECK, ON DELETE CASCADE) are validated end to end.
//
// Skips when AA_DB_PASSWORD is unset — same convention as the
// rest of the federation integration suites.

package userkeys_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
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
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func initAtrestOnce(t *testing.T) {
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

// fixtureUser inserts a throwaway user. t.Cleanup deletes the
// user (ON DELETE CASCADE drops any keys the test inserted along
// the way). The username has a random suffix so concurrent test
// runs against the same DB don't collide.
func fixtureUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	username := "userkey-test-" + randHex(t, 4)
	var ref int64
	err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username) VALUES ($1) RETURNING ref`,
		username,
	).Scan(&ref)
	if err != nil {
		t.Fatalf("fixture user insert: %v", err)
	}
	t.Cleanup(func() {
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx2, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// freshKeyParams mints a fresh keypair via the userkeys primitive,
// returns the InsertUserKey params for inserting it as a current
// key for the given user.
func freshKeyParams(t *testing.T, userRef int64, version int32, isCurrent bool) userkeys.InsertUserKeyParams {
	t.Helper()
	pub, wrapped, err := userkeys.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	p := userkeys.InsertUserKeyParams{
		UserRef:        userRef,
		Version:       version,
		Algorithm:     userkeys.Algorithm,
		PublicKey:     pub,
		PrivateKeyEnc: wrapped,
		IsCurrent:     isCurrent,
	}
	return p
}

// --- round-trip ----------------------------------------------------

func TestQueries_InsertAndGetCurrent_Roundtrip(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	in := freshKeyParams(t, userRef, 1, true)
	inserted, err := q.InsertUserKey(ctx, in)
	if err != nil {
		t.Fatalf("InsertUserKey: %v", err)
	}
	if inserted.Version != 1 || !inserted.IsCurrent {
		t.Fatalf("inserted row mismatch: version=%d is_current=%v", inserted.Version, inserted.IsCurrent)
	}
	if inserted.Algorithm != userkeys.Algorithm {
		t.Fatalf("algorithm not persisted: got %q want %q", inserted.Algorithm, userkeys.Algorithm)
	}

	got, err := q.GetCurrentUserKey(ctx, userRef)
	if err != nil {
		t.Fatalf("GetCurrentUserKey: %v", err)
	}
	if got.UserRef != userRef || got.Version != 1 || !got.IsCurrent {
		t.Fatalf("GetCurrentUserKey shape: %+v", got)
	}
	if !bytesEqual(got.PublicKey, in.PublicKey) {
		t.Fatalf("GetCurrentUserKey public_key mismatch")
	}
	if !bytesEqual(got.PrivateKeyEnc, in.PrivateKeyEnc) {
		t.Fatalf("GetCurrentUserKey private_key_enc mismatch")
	}

	// Unwrap on the round-tripped bytes should produce a private
	// key whose public matches.
	priv, err := userkeys.Unwrap(got.PrivateKeyEnc)
	if err != nil {
		t.Fatalf("Unwrap after round-trip: %v", err)
	}
	if !bytesEqual(priv.PublicKey().Bytes(), in.PublicKey) {
		t.Fatalf("Unwrap-derived public doesn't match stored public")
	}
}

func TestQueries_GetCurrentUserKey_NoRowsWhenUserHasNoKey(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	_, err := q.GetCurrentUserKey(ctx, userRef)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetCurrentUserKey for keyless user: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestQueries_GetUserKeyByVersion_FindsExactRow(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	// Insert version 1 as retained, version 2 as current. Models
	// the post-rotation steady state.
	v1 := freshKeyParams(t, userRef, 1, false)
	v1.RetainedUntil = pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}
	if _, err := q.InsertUserKey(ctx, v1); err != nil {
		t.Fatalf("insert v1 retained: %v", err)
	}
	v2 := freshKeyParams(t, userRef, 2, true)
	if _, err := q.InsertUserKey(ctx, v2); err != nil {
		t.Fatalf("insert v2 current: %v", err)
	}

	got, err := q.GetUserKeyByVersion(ctx, userkeys.GetUserKeyByVersionParams{
		UserRef:  userRef,
		Version: 1,
	})
	if err != nil {
		t.Fatalf("GetUserKeyByVersion(1): %v", err)
	}
	if got.IsCurrent {
		t.Fatalf("expected version 1 to be retained, got current")
	}
	if !got.RetainedUntil.Valid {
		t.Fatalf("expected retained_until to be set on v1")
	}
}

func TestQueries_ListPublicKeysByUser_OrdersCurrentFirst(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	v1 := freshKeyParams(t, userRef, 1, false)
	v1.RetainedUntil = pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}
	if _, err := q.InsertUserKey(ctx, v1); err != nil {
		t.Fatalf("insert v1: %v", err)
	}
	v2 := freshKeyParams(t, userRef, 2, true)
	if _, err := q.InsertUserKey(ctx, v2); err != nil {
		t.Fatalf("insert v2: %v", err)
	}

	rows, err := q.ListPublicKeysByUser(ctx, userRef)
	if err != nil {
		t.Fatalf("ListPublicKeysByUser: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("got %d rows, want %d", got, want)
	}
	// version DESC means current (v2) first.
	if rows[0].Version != 2 || !rows[0].IsCurrent {
		t.Fatalf("first row should be current v2: %+v", rows[0])
	}
	if rows[1].Version != 1 || rows[1].IsCurrent {
		t.Fatalf("second row should be retained v1: %+v", rows[1])
	}
}

func TestQueries_ListPublicKeysByUser_ExcludesExpiredRetention(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	// v1: retention expired an hour ago. Should not appear in the
	// listing (only the current key + within-grace retained keys
	// should). The sweeper (1.22.I-h) deletes these eventually;
	// until then, this query is the read-side filter.
	v1 := freshKeyParams(t, userRef, 1, false)
	v1.RetainedUntil = pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true}
	if _, err := q.InsertUserKey(ctx, v1); err != nil {
		t.Fatalf("insert v1 expired: %v", err)
	}
	v2 := freshKeyParams(t, userRef, 2, true)
	if _, err := q.InsertUserKey(ctx, v2); err != nil {
		t.Fatalf("insert v2 current: %v", err)
	}

	rows, err := q.ListPublicKeysByUser(ctx, userRef)
	if err != nil {
		t.Fatalf("ListPublicKeysByUser: %v", err)
	}
	if got, want := len(rows), 1; got != want {
		t.Fatalf("got %d rows, want %d (expired retention should not list)", got, want)
	}
	if rows[0].Version != 2 {
		t.Fatalf("only current should appear: %+v", rows[0])
	}
}

func TestQueries_CountUserKeys(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	if n, err := q.CountUserKeys(ctx, userRef); err != nil || n != 0 {
		t.Fatalf("Count empty: n=%d err=%v", n, err)
	}
	if _, err := q.InsertUserKey(ctx, freshKeyParams(t, userRef, 1, true)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n, err := q.CountUserKeys(ctx, userRef); err != nil || n != 1 {
		t.Fatalf("Count after 1 insert: n=%d err=%v", n, err)
	}
}

// --- invariants enforced by the migration --------------------------

func TestQueries_PartialUnique_RejectsTwoCurrent(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	if _, err := q.InsertUserKey(ctx, freshKeyParams(t, userRef, 1, true)); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Second is_current=true row for the same user must violate
	// the partial unique index.
	_, err := q.InsertUserKey(ctx, freshKeyParams(t, userRef, 2, true))
	if !isPgUniqueViolation(err) || !strings.Contains(err.Error(), "federation_user_keys_one_current_idx") {
		t.Fatalf("expected unique violation on federation_user_keys_one_current_idx, got: %v", err)
	}
}

func TestQueries_CurrentXorRetained_RejectsCurrentWithRetained(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	p := freshKeyParams(t, userRef, 1, true)
	p.RetainedUntil = pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}
	_, err := q.InsertUserKey(ctx, p)
	if !isPgCheckViolation(err) || !strings.Contains(err.Error(), "federation_user_keys_current_xor_retained") {
		t.Fatalf("expected check violation on federation_user_keys_current_xor_retained, got: %v", err)
	}
}

func TestQueries_CurrentXorRetained_RejectsRetainedWithNullRetainedUntil(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	// Retained (is_current=false) but no retained_until set is the
	// other side of the XOR — also invalid.
	p := freshKeyParams(t, userRef, 1, false)
	_, err := q.InsertUserKey(ctx, p)
	if !isPgCheckViolation(err) || !strings.Contains(err.Error(), "federation_user_keys_current_xor_retained") {
		t.Fatalf("expected check violation, got: %v", err)
	}
}

func TestQueries_OnDeleteCascade_DropsKeysWhenUserGoes(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	if _, err := q.InsertUserKey(ctx, freshKeyParams(t, userRef, 1, true)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM "user" WHERE ref = $1`, userRef); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n, err := q.CountUserKeys(ctx, userRef); err != nil || n != 0 {
		t.Fatalf("CountUserKeys after user delete: n=%d err=%v", n, err)
	}
}

// --- helpers ------------------------------------------------------

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isPgUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isPgCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}
