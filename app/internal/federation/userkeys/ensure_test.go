package userkeys_test

import (
	"context"
	"sync"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
)

func TestEnsureCurrentForUser_CreatesKeyForKeylessUser(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	alreadyHad, err := userkeys.EnsureCurrentForUser(ctx, q, userRef)
	if err != nil {
		t.Fatalf("EnsureCurrentForUser: %v", err)
	}
	if alreadyHad {
		t.Errorf("first call: alreadyHadKey should be false on fresh user")
	}

	// Verify one current key exists.
	got, err := q.GetCurrentUserKey(ctx, userRef)
	if err != nil {
		t.Fatalf("GetCurrentUserKey: %v", err)
	}
	if got.Version != 1 || !got.IsCurrent {
		t.Errorf("unexpected key shape after first ensure: %+v", got)
	}
	if got.Algorithm != userkeys.Algorithm {
		t.Errorf("algorithm = %q, want %q", got.Algorithm, userkeys.Algorithm)
	}
}

func TestEnsureCurrentForUser_NoOpWhenKeyAlreadyExists(t *testing.T) {
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	// Bootstrap a current key directly so the second call has to
	// take the idempotent no-op path.
	if _, err := userkeys.EnsureCurrentForUser(ctx, q, userRef); err != nil {
		t.Fatalf("setup ensure: %v", err)
	}
	before, err := q.GetCurrentUserKey(ctx, userRef)
	if err != nil {
		t.Fatalf("get current: %v", err)
	}

	alreadyHad, err := userkeys.EnsureCurrentForUser(ctx, q, userRef)
	if err != nil {
		t.Fatalf("second EnsureCurrentForUser: %v", err)
	}
	if !alreadyHad {
		t.Errorf("second call: alreadyHadKey should be true")
	}

	// The key row should be byte-identical (no second key, no
	// rotation, no overwrite).
	after, err := q.GetCurrentUserKey(ctx, userRef)
	if err != nil {
		t.Fatalf("get current after: %v", err)
	}
	if !bytesEqual(before.PublicKey, after.PublicKey) {
		t.Errorf("public key mutated across no-op ensure calls")
	}
	if !before.CreatedAt.Time.Equal(after.CreatedAt.Time) {
		t.Errorf("created_at mutated across no-op ensure calls")
	}
}

func TestEnsureCurrentForUser_ConcurrentCallsConvergeOnOneKey(t *testing.T) {
	// The partial UNIQUE index races the two concurrent inserts;
	// the loser's insert fails with a unique violation; the
	// helper's recovery path re-fetches the winner's row + returns
	// alreadyHadKey=true. Exactly one current key persists.
	initAtrestOnce(t)
	pool := openPool(t)
	defer pool.Close()
	ctx := context.Background()

	q := userkeys.New(pool)
	userRef := fixtureUser(t, ctx, pool)

	const concurrency = 8
	results := make([]bool, concurrency)
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := range concurrency {
		go func(i int) {
			defer wg.Done()
			alreadyHad, err := userkeys.EnsureCurrentForUser(ctx, q, userRef)
			results[i] = alreadyHad
			errs[i] = err
		}(i)
	}
	wg.Wait()

	winners := 0
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
		if !results[i] {
			winners++
		}
	}
	if winners < 1 {
		t.Errorf("at least one goroutine must have minted the key, got 0")
	}

	// Exactly one current key persists.
	if n, err := q.CountUserKeys(ctx, userRef); err != nil || n != 1 {
		t.Errorf("CountUserKeys after concurrent ensure: n=%d err=%v want 1", n, err)
	}
}
