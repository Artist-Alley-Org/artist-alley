// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE CYCLE INVARIANT IS ATOMIC, OR IT IS NOT AN INVARIANT (#1173,
// #1119, ADR 0099 §8).
//
// # What the sequential suite cannot prove
//
// display_condition_e2e_test.go proves the RULE: `A -> B`, then `B -> C`,
// then `C -> A` is refused on the third write. It cannot prove the
// ATOMICITY, because a "read the graph, walk it, then run the update"
// implementation passes every one of those assertions. The window it
// leaves open is never entered when nothing else is running.
//
// And the window is exactly the case the rule exists for. Two operators,
// one writing `A -> B` and one writing `B -> A`, each read a graph in
// which the other's edge is not yet visible. Both walks find no cycle.
// Both commit. The graph now holds a 2-cycle that neither write could
// have created alone, and every later validation walks it.
//
// # THE SYNCHRONISATION SEAM, and why it is not a sleep
//
// This is 20a's `field_value_race_test.go` mechanism applied to a
// different lock. A test that fires two requests and hopes they collide
// proves nothing: on a quiet machine the first finishes before the
// second's connection is checked out, and the test then passes against
// the broken implementation it was written to catch. A sleep is the same
// failure with a longer runtime.
//
// The seam is a HELD LOCK plus an OBSERVED WAIT:
//
//  1. A gate transaction takes the SAME advisory lock the handler must
//     hold before it may read the graph.
//  2. Both contenders are launched. Each runs its whole handler and
//     BLOCKS at `pg_advisory_xact_lock`, BEFORE reading the graph — which
//     is the property that matters, because a lock taken AFTER the read
//     would serialise the writes while still letting both walks see a
//     stale graph.
//  3. The test WAITS UNTIL `pg_stat_activity` reports both contender
//     backends waiting on a lock. That is an observation of a STATE, not
//     an elapsed duration: the test does not proceed until the overlap
//     has provably happened, and fails outright if it never does.
//  4. The gate releases. The two contenders are now serialised through
//     the critical section, so the second one's walk sees the first one's
//     COMMITTED edge.
//
// The contenders run on their own pool with a distinctive
// `application_name`, so the wait count cannot be confused by another
// package's tests sharing the database.
package metadata_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// The two keys the handler passes to pg_advisory_xact_lock for the ASSET
// graph. Restated here rather than exported from the package on purpose:
// if somebody changes the handler's keys without changing these, the gate
// stops blocking anything, `waitForBlockedGraphContenders` times out, and
// the test FAILS LOUDLY rather than quietly proving nothing.
const (
	graphLockSpace   = 1173
	graphLockSubject = 1 // asset
)

type graphRaceEnv struct {
	*dcEnv
	pool    *pgxpool.Pool // the CONTENDERS' pool
	router  chi.Router
	appName string
}

func newGraphRaceEnv(t *testing.T) *graphRaceEnv {
	t.Helper()
	base := newDCEnv(t)

	appName := fmt.Sprintf("aa-graphrace-%d", time.Now().UnixNano())
	pwd := os.Getenv("AA_DB_PASSWORD")
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s dbname=%s sslmode=disable password=%s application_name=%s pool_max_conns=8",
		envOr("AA_DB_HOST", "postgres"), envOr("AA_DB_PORT", "5432"),
		envOr("AA_DB_USER", "artist_alley"), testdb.Name(t), pwd, appName,
	)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("graph race pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("graph race pool ping: %v", err)
	}
	t.Cleanup(pool.Close)

	router, _ := makeRouter(t, pool /*admin=*/, true)
	return &graphRaceEnv{dcEnv: base, pool: pool, router: router, appName: appName}
}

// waitForBlockedGraphContenders is the HAPPENS-BEFORE WITNESS.
//
// It fails the test rather than continuing if the overlap never
// materialises, because a race test that quietly runs its contenders
// sequentially is a test that reports green for the bug it exists to
// catch.
func (e *graphRaceEnv) waitForBlockedGraphContenders(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		var n int
		err := e.dcEnv.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND application_name = $1
			   AND wait_event_type = 'Lock'`, e.appName).Scan(&n)
		if err != nil {
			t.Fatalf("observe contenders: %v", err)
		}
		last = n
		if n >= want {
			t.Logf("synchronisation seam: %d contender backend(s) observed BLOCKED on the display-condition graph lock, before either could read the graph", n)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("only %d of %d contenders ever blocked on the graph lock — the overlap this test asserts never happened, so it would have proved nothing", last, want)
}

// graphGate holds the advisory lock so contenders pile up behind it.
type graphGate struct{ tx pgx.Tx }

func (e *graphRaceEnv) holdGraphLock(t *testing.T) *graphGate {
	t.Helper()
	tx, err := e.dcEnv.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("gate begin: %v", err)
	}
	if _, err := tx.Exec(context.Background(),
		`SELECT pg_advisory_xact_lock($1::int, $2::int)`, graphLockSpace, graphLockSubject); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("gate lock: %v", err)
	}
	return &graphGate{tx: tx}
}

func (g *graphGate) release(t *testing.T) {
	t.Helper()
	// ROLLBACK, never COMMIT: the gate exists to create the overlap, and
	// nothing it did should be part of what the contenders see.
	if err := g.tx.Rollback(context.Background()); err != nil {
		t.Fatalf("gate release: %v", err)
	}
}

type graphAttempt struct {
	status int
	body   string
}

func (e *graphRaceEnv) patchCondition(fieldID string, cond []string) func() graphAttempt {
	return func() graphAttempt {
		b, _ := json.Marshal(map[string]any{"display_condition": cond})
		req := httptest.NewRequest(http.MethodPatch, "/fields/"+fieldID, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		e.router.ServeHTTP(rr, req)
		return graphAttempt{status: rr.Code, body: rr.Body.String()}
	}
}

// runGraphContenders launches every request, waits until they are all
// observed blocked, releases the gate, and returns the outcomes in order.
func (e *graphRaceEnv) runGraphContenders(t *testing.T, g *graphGate, reqs ...func() graphAttempt) []graphAttempt {
	t.Helper()
	out := make([]graphAttempt, len(reqs))
	var wg sync.WaitGroup
	for i, fn := range reqs {
		wg.Add(1)
		go func(i int, fn func() graphAttempt) {
			defer wg.Done()
			out[i] = fn()
		}(i, fn)
	}
	e.waitForBlockedGraphContenders(t, len(reqs))
	g.release(t)
	wg.Wait()
	return out
}

// storedEdges reports how many of the given fields ended up with a
// non-NULL display_condition.
func (e *graphRaceEnv) storedEdges(t *testing.T, fieldIDs ...string) int {
	t.Helper()
	n := 0
	for _, id := range fieldIDs {
		if _, isNull := e.dcEnv.storedCondition(t, id); !isNull {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// A-12 — the 2-cycle race
// ---------------------------------------------------------------------------

// TestGraphRace_TwoWayCycleCannotBothCommit is the case the invariant
// exists for.
//
// A and B start UNCONDITIONED. One request writes `A -> B` and the other
// writes `B -> A`, and they provably overlap. Exactly one may commit, and
// the final graph must be acyclic.
//
// Without the advisory lock both walks read a graph with no edges at all,
// both find no cycle, and both commit: the assertion below would see two
// stored conditions and a 2-cycle.
func TestGraphRace_TwoWayCycleCannotBothCommit(t *testing.T) {
	env := newGraphRaceEnv(t)
	a := env.dcEnv.field(t, "race_a", "text", nil)
	b := env.dcEnv.field(t, "race_b", "text", nil)

	// Both start unconditioned, which is what makes each write
	// individually valid and the pair jointly invalid.
	if n := env.storedEdges(t, a, b); n != 0 {
		t.Fatalf("precondition: %d edges stored, want 0", n)
	}

	gate := env.holdGraphLock(t)
	got := env.runGraphContenders(t, gate,
		env.patchCondition(a, []string{"metadata_test_race_b=x"}),
		env.patchCondition(b, []string{"metadata_test_race_a=x"}),
	)

	ok, refused := 0, 0
	for i, r := range got {
		switch r.status {
		case http.StatusOK:
			ok++
		case http.StatusBadRequest:
			refused++
		default:
			t.Fatalf("contender %d: unexpected status %d body=%s", i, r.status, r.body)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("exactly one write may commit; got %d accepted and %d refused. "+
			"Two acceptances mean the cycle precondition was evaluated against a graph the other write had already changed, "+
			"which is the defect the advisory lock exists to close. Bodies: %q / %q",
			ok, refused, got[0].body, got[1].body)
	}
	if n := env.storedEdges(t, a, b); n != 1 {
		t.Fatalf("final graph holds %d edges, want 1: a 2-cycle was committed", n)
	}
}

// TestGraphRace_ThreeWayClosingEdge covers the N>=3 closing-edge overlap,
// which is materially distinct for this mechanism.
//
// `A -> B` and `B -> C` are already stored and committed. The two
// contenders are `C -> A` (which CLOSES the ring) and a harmless
// `D -> A`. Both take the same lock, so this proves the lock does not
// merely serialise identical requests: the closing edge is refused on the
// strength of a WALK, while the unrelated edge in the same critical
// section commits.
//
// It also guards the opposite failure: a lock scoped too widely, or a
// validator that refused anything it found under contention, would refuse
// both.
func TestGraphRace_ThreeWayClosingEdge(t *testing.T) {
	env := newGraphRaceEnv(t)
	a := env.dcEnv.field(t, "r3_a", "text", nil)
	b := env.dcEnv.field(t, "r3_b", "text", nil)
	c := env.dcEnv.field(t, "r3_c", "text", nil)
	d := env.dcEnv.field(t, "r3_d", "text", nil)

	for _, seed := range []struct{ id, term string }{
		{a, "metadata_test_r3_b=x"},
		{b, "metadata_test_r3_c=x"},
	} {
		if rr := patchJSON(t, env.dcEnv.router, "/fields/"+seed.id,
			map[string]any{"display_condition": []string{seed.term}}); rr.Code != http.StatusOK {
			t.Fatalf("seed %s: %d %s", seed.term, rr.Code, rr.Body.String())
		}
	}

	gate := env.holdGraphLock(t)
	got := env.runGraphContenders(t, gate,
		env.patchCondition(c, []string{"metadata_test_r3_a=x"}), // closes A -> B -> C -> A
		env.patchCondition(d, []string{"metadata_test_r3_a=x"}), // harmless leaf
	)

	if got[0].status != http.StatusBadRequest {
		t.Fatalf("the closing edge must be refused even under contention: status=%d body=%s", got[0].status, got[0].body)
	}
	if got[1].status != http.StatusOK {
		t.Fatalf("an unrelated edge in the same critical section must still commit: status=%d body=%s", got[1].status, got[1].body)
	}
	if _, isNull := env.dcEnv.storedCondition(t, c); !isNull {
		t.Fatal("the closing edge was written despite being refused")
	}
	if _, isNull := env.dcEnv.storedCondition(t, d); isNull {
		t.Fatal("the harmless edge was not written")
	}
}

// TestGraphRace_OrdinaryFieldEditsDoNotQueueBehindTheGraphLock is the
// scoping proof, and it is the reason the lock is conditional.
//
// With the graph lock HELD, a PATCH that does not touch
// display_condition must complete. If it blocked, every ordinary field
// edit on the install would serialise behind whatever graph walk happened
// to be running, which is a cost nobody signed up for.
//
// No gate observation is needed here: the assertion is that the request
// FINISHES while the lock is held, and a generous deadline distinguishes
// "did not block" from "blocked" without depending on timing for the
// verdict — a blocked request would never return at all until the gate
// released.
func TestGraphRace_OrdinaryFieldEditsDoNotQueueBehindTheGraphLock(t *testing.T) {
	env := newGraphRaceEnv(t)
	f := env.dcEnv.field(t, "unrelated", "text", nil)

	gate := env.holdGraphLock(t)
	done2 := make(chan int, 1)
	go func() {
		b, _ := json.Marshal(map[string]any{"label": "renamed while the graph lock is held"})
		req := httptest.NewRequest(http.MethodPatch, "/fields/"+f, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		env.router.ServeHTTP(rr, req)
		done2 <- rr.Code
	}()
	select {
	case code := <-done2:
		if code != http.StatusOK {
			t.Fatalf("unrelated PATCH: status=%d", code)
		}
	case <-time.After(10 * time.Second):
		gate.release(t)
		t.Fatal("an ordinary field edit blocked on the display-condition graph lock; the lock must be taken only when a request touches display_condition")
	}
	gate.release(t)
}
