// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// REAL overlapping writers on one field value (#1119).
//
// # Why these exist separately from the sequential guard tests
//
// The sequential matrix proves the RULE. It cannot prove the
// ATOMICITY, because a "read the row, compare set_at, then run the
// unconditional upsert" implementation passes every one of those tests:
// the window it leaves open is never entered when nothing else is
// running. All four handlers open their transaction with
// `pgx.TxOptions{}` — EMPTY options, so READ COMMITTED, where a plain
// SELECT takes no lock at all and the gap between the read and the
// write is wide enough for an entire competing request.
//
// So the precondition and the mutation are ONE STATEMENT (see the
// guarded queries in queries.sql), and these tests are what says so.
//
// # THE SYNCHRONISATION SEAM, and why it is not a sleep
//
// A test that fires two requests and hopes they collide proves nothing:
// on a quiet machine the first finishes before the second's connection
// is even checked out, and the test then passes against the broken
// implementation it was written to catch. A sleep is the same failure
// with a longer runtime.
//
// The seam here is a HELD DATABASE LOCK plus an OBSERVED wait:
//
//  1. A gate transaction takes a lock the contenders must have before
//     they can mutate — `SELECT ... FOR UPDATE` on the value row, or,
//     where the case is about a row that does not exist yet, an
//     uncommitted INSERT of it.
//  2. Both contenders are launched. Each runs its whole handler and
//     BLOCKS at its mutating statement.
//  3. The test WAITS UNTIL `pg_stat_activity` reports both contender
//     backends waiting on a lock. This is an observation of a state,
//     not an elapsed duration: the test does not proceed until the
//     overlap it needs has provably happened, and fails outright if it
//     never does.
//  4. The gate releases. Both contenders are now inside the critical
//     section with neither one's mutation visible to the other, which
//     is precisely the property being asserted.
//
// The contenders run on their OWN pool with a distinctive
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
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type raceEnv struct {
	*vocabEnv
	pool    *pgxpool.Pool // the CONTENDERS' pool, tagged with appName
	router  chi.Router    // handlers bound to that pool
	appName string
}

func newRaceEnv(t *testing.T) *raceEnv {
	t.Helper()
	base := newVocabEnv(t)

	appName := fmt.Sprintf("aa-race-%d", time.Now().UnixNano())
	pwd := os.Getenv("AA_DB_PASSWORD")
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s dbname=%s sslmode=disable password=%s application_name=%s pool_max_conns=8",
		envOr("AA_DB_HOST", "postgres"), envOr("AA_DB_PORT", "5432"),
		envOr("AA_DB_USER", "artist_alley"), testdb.Name(t), pwd, appName,
	)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("race pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("race pool ping: %v", err)
	}
	t.Cleanup(pool.Close)

	router, _ := makeRouter(t, pool /*admin=*/, true)
	return &raceEnv{vocabEnv: base, pool: pool, router: router, appName: appName}
}

// waitForBlockedContenders blocks until `want` backends on the
// contenders' pool are waiting on a heavyweight lock.
//
// This is the happens-before witness. It fails the test rather than
// continuing if the overlap never materialises, because a race test
// that quietly runs its contenders sequentially is a test that reports
// green for the bug it exists to catch.
func (e *raceEnv) waitForBlockedContenders(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		var n int
		err := e.vocabEnv.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND application_name = $1
			   AND wait_event_type = 'Lock'`, e.appName).Scan(&n)
		if err != nil {
			t.Fatalf("observe contenders: %v", err)
		}
		last = n
		if n >= want {
			t.Logf("synchronisation seam: %d contender backend(s) observed BLOCKED on a lock before any mutation was visible", n)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("only %d of %d contenders ever blocked — the overlap this test asserts never happened, so it would have proved nothing", last, want)
}

// gate holds a transaction open so contenders pile up behind it.
type gate struct {
	tx pgx.Tx
}

// lockAssetValueRow takes a row lock on an EXISTING value row.
func (e *raceEnv) lockAssetValueRow(t *testing.T, fieldID string) *gate {
	t.Helper()
	tx, err := e.vocabEnv.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("gate begin: %v", err)
	}
	var id string
	if err := tx.QueryRow(context.Background(),
		`SELECT asset_id::text FROM asset_field_value WHERE asset_id=$1 AND field_id=$2 FOR UPDATE`,
		e.assetID, fieldID).Scan(&id); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("gate lock: %v", err)
	}
	return &gate{tx: tx}
}

// blockInsertsOfAssetValue makes the row's slot in the unique index
// unavailable by INSERTing it and not committing. Rolling the gate back
// releases the waiters into a genuine two-way insert race on a row that
// never existed.
func (e *raceEnv) blockInsertsOfAssetValue(t *testing.T, fieldID string) *gate {
	t.Helper()
	tx, err := e.vocabEnv.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("gate begin: %v", err)
	}
	if _, err := tx.Exec(context.Background(),
		`INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by, set_at)
		 VALUES ($1, $2, 'gate', 'manual', NOW())`, e.assetID, fieldID); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("gate insert: %v", err)
	}
	return &gate{tx: tx}
}

func (g *gate) release(t *testing.T) {
	t.Helper()
	// ROLLBACK, never COMMIT: the gate exists to create the overlap, and
	// nothing it did should be part of what the contenders see.
	if err := g.tx.Rollback(context.Background()); err != nil {
		t.Fatalf("gate release: %v", err)
	}
}

type attempt struct {
	status int
	body   map[string]any
}

// runContenders launches every request, waits until they are all
// observed blocked, releases the gate, and returns the outcomes in
// order.
func (e *raceEnv) runContenders(t *testing.T, g *gate, reqs ...func() *httptest.ResponseRecorder) []attempt {
	t.Helper()
	out := make([]attempt, len(reqs))
	var wg sync.WaitGroup
	for i, fn := range reqs {
		wg.Add(1)
		go func(i int, fn func() *httptest.ResponseRecorder) {
			defer wg.Done()
			rr := fn()
			var m map[string]any
			if rr.Body.Len() > 0 {
				_ = jsonUnmarshalSoft(rr.Body.Bytes(), &m)
			}
			out[i] = attempt{status: rr.Code, body: m}
		}(i, fn)
	}
	e.waitForBlockedContenders(t, len(reqs))
	g.release(t)
	wg.Wait()
	return out
}

func countStatus(as []attempt, code int) int {
	n := 0
	for _, a := range as {
		if a.status == code {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The contender request builders
// ---------------------------------------------------------------------------

func (e *raceEnv) putAssetReq(fieldID string, body map[string]any) func() *httptest.ResponseRecorder {
	return func() *httptest.ResponseRecorder {
		return putJSONRaw(e.router, fmt.Sprintf("/assets/%s/fields/%s", e.assetID, fieldID), body)
	}
}

func (e *raceEnv) clearAssetReq(fieldID, token string) func() *httptest.ResponseRecorder {
	return func() *httptest.ResponseRecorder {
		path := fmt.Sprintf("/assets/%s/fields/%s", e.assetID, fieldID)
		if token != "" {
			path += "?if_unchanged_since=" + urlQueryEscape(token)
		}
		rr := httptest.NewRecorder()
		e.router.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, path, nil))
		return rr
	}
}

// ---------------------------------------------------------------------------
// A-a — two overlapping guarded Sets from ONE token
// ---------------------------------------------------------------------------

func TestRace_TwoGuardedSetsFromOneToken(t *testing.T) {
	env := newRaceEnv(t)
	fid := env.assetField(t, "race_ss", "text", nil)
	_, seeded := env.putAsset(t, fid, map[string]any{"value_text": "baseline"})
	token := setAtOf(t, seeded)

	g := env.lockAssetValueRow(t, fid)
	got := env.runContenders(t, g,
		env.putAssetReq(fid, map[string]any{"value_text": "writer A", "if_unchanged_since": token}),
		env.putAssetReq(fid, map[string]any{"value_text": "writer B", "if_unchanged_since": token}),
	)

	if countStatus(got, http.StatusOK) != 1 || countStatus(got, http.StatusConflict) != 1 {
		t.Fatalf("want exactly one 200 and one 409, got %v / %v", got[0].status, got[1].status)
	}
	var winner string
	for _, a := range got {
		if a.status == http.StatusOK {
			winner, _ = a.body["value_text"].(string)
		}
	}
	stored, ok := readStored(t, env.vocabEnv.pool, env.assetID, fid)
	if !ok || stored.Text == nil || *stored.Text != winner {
		t.Errorf("persisted value %v is not the successful write %q", stored.Text, winner)
	}
	for _, a := range got {
		if a.status == http.StatusConflict {
			if p, _ := a.body["present"].(bool); !p {
				t.Errorf("the loser's 409 must report present:true; body=%v", a.body)
			}
			cur, keyPresent := a.body["current"]
			if !keyPresent || cur == nil {
				t.Errorf("the loser's 409 must carry the winner's value; body=%v", a.body)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// A-b — two overlapping if_absent FIRST writes
// ---------------------------------------------------------------------------

func TestRace_TwoIfAbsentFirstWrites(t *testing.T) {
	env := newRaceEnv(t)
	fid := env.assetField(t, "race_ia", "text", nil)

	// No row exists, so the seam is the unique index rather than a row
	// lock: the gate INSERTs and does not commit, both contenders block
	// on that tuple, and the ROLLBACK releases them into a genuine
	// two-way insert race on a row that never existed.
	g := env.blockInsertsOfAssetValue(t, fid)
	got := env.runContenders(t, g,
		env.putAssetReq(fid, map[string]any{"value_text": "first A", "if_absent": true}),
		env.putAssetReq(fid, map[string]any{"value_text": "first B", "if_absent": true}),
	)

	if countStatus(got, http.StatusOK) != 1 || countStatus(got, http.StatusConflict) != 1 {
		t.Fatalf("want exactly one 200 and one 409, got %d / %d", got[0].status, got[1].status)
	}
	var n int
	if err := env.vocabEnv.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM asset_field_value WHERE asset_id=$1 AND field_id=$2`,
		env.assetID, fid).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("EXACTLY ONE stored row expected after two overlapping first writes, got %d", n)
	}
	for _, a := range got {
		if a.status == http.StatusConflict {
			if p, _ := a.body["present"].(bool); !p {
				t.Errorf("if_absent loser must see present:true; body=%v", a.body)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// A-c — overlapping Set and Clear, in BOTH orders
// ---------------------------------------------------------------------------

func TestRace_SetAndClearBothOrders(t *testing.T) {
	for _, order := range []string{"set_first", "clear_first"} {
		t.Run(order, func(t *testing.T) {
			env := newRaceEnv(t)
			fid := env.assetField(t, "race_sc_"+order, "text", nil)
			_, seeded := env.putAsset(t, fid, map[string]any{"value_text": "baseline"})
			token := setAtOf(t, seeded)

			set := env.putAssetReq(fid, map[string]any{"value_text": "the set", "if_unchanged_since": token})
			clr := env.clearAssetReq(fid, token)

			g := env.lockAssetValueRow(t, fid)
			var got []attempt
			if order == "set_first" {
				got = env.runContenders(t, g, set, clr)
			} else {
				got = env.runContenders(t, g, clr, set)
			}

			ok200 := countStatus(got, http.StatusOK)
			ok204 := countStatus(got, http.StatusNoContent)
			conflicts := countStatus(got, http.StatusConflict)
			if ok200+ok204 != 1 || conflicts != 1 {
				t.Fatalf("exactly one winner expected: 200=%d 204=%d 409=%d (%v)", ok200, ok204, conflicts, got)
			}

			stored, exists := readStored(t, env.vocabEnv.pool, env.assetID, fid)
			if ok204 == 1 {
				// The Clear won, so the Set must have been refused
				// against absence and must NOT have resurrected the row.
				if exists {
					t.Errorf("the losing Set resurrected a cleared row: %v", stored.Text)
				}
				for _, a := range got {
					if a.status == http.StatusConflict {
						if p, _ := a.body["present"].(bool); p {
							t.Errorf("after a winning Clear the loser must see present:false; body=%v", a.body)
						}
					}
				}
			} else {
				// The Set won, so the losing Clear must not have erased it.
				if !exists || stored.Text == nil || *stored.Text != "the set" {
					t.Errorf("the losing Clear erased the winning Set: %v", stored.Text)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A-d — two overlapping guarded Clears from one baseline
// ---------------------------------------------------------------------------

func TestRace_TwoGuardedClears(t *testing.T) {
	env := newRaceEnv(t)
	fid := env.assetField(t, "race_cc", "text", nil)
	_, seeded := env.putAsset(t, fid, map[string]any{"value_text": "baseline"})
	token := setAtOf(t, seeded)

	g := env.lockAssetValueRow(t, fid)
	got := env.runContenders(t, g,
		env.clearAssetReq(fid, token),
		env.clearAssetReq(fid, token),
	)

	if countStatus(got, http.StatusNoContent) != 1 || countStatus(got, http.StatusConflict) != 1 {
		t.Fatalf("want one 204 and one 409, got %d / %d", got[0].status, got[1].status)
	}
	for _, a := range got {
		if a.status == http.StatusConflict {
			if p, _ := a.body["present"].(bool); p {
				t.Errorf("the losing Clear must see present:false; body=%v", a.body)
			}
			if cur, keyPresent := a.body["current"]; !keyPresent {
				t.Errorf("`current` key must be present even when null; body=%v", a.body)
			} else if cur != nil {
				t.Errorf("current must be null; got %v", cur)
			}
		}
	}
	if _, ok := readStored(t, env.vocabEnv.pool, env.assetID, fid); ok {
		t.Error("the row survived two clears")
	}
}

// ---------------------------------------------------------------------------
// A-e — two overlapping UNGUARDED writes both succeed
// ---------------------------------------------------------------------------

// The compatibility half. Last-write-wins is the CONTRACT for an
// unguarded caller, and a fix that made overlapping unguarded writes
// start conflicting would break the upload flush.
func TestRace_TwoUnguardedWritesBothSucceed(t *testing.T) {
	env := newRaceEnv(t)
	fid := env.assetField(t, "race_ug", "text", nil)
	if code, _ := env.putAsset(t, fid, map[string]any{"value_text": "baseline"}); code != http.StatusOK {
		t.Fatal("seed")
	}

	g := env.lockAssetValueRow(t, fid)
	got := env.runContenders(t, g,
		env.putAssetReq(fid, map[string]any{"value_text": "unguarded A"}),
		env.putAssetReq(fid, map[string]any{"value_text": "unguarded B"}),
	)

	if countStatus(got, http.StatusOK) != 2 {
		t.Fatalf("both unguarded writes must succeed, got %d / %d (%v)", got[0].status, got[1].status, got)
	}
	stored, ok := readStored(t, env.vocabEnv.pool, env.assetID, fid)
	if !ok || stored.Text == nil || (*stored.Text != "unguarded A" && *stored.Text != "unguarded B") {
		t.Errorf("stored=%v, want one of the two writes", stored.Text)
	}
}

// ---------------------------------------------------------------------------
// Collection twin — the second handler is a different function
// ---------------------------------------------------------------------------

func TestRace_CollectionGuardedSets(t *testing.T) {
	env := newRaceEnv(t)
	fid := env.collectionField(t, "race_ss", "text", nil)
	_, seeded := env.putCollection(t, fid, map[string]any{"value_text": "baseline"})
	token := setAtOf(t, seeded)

	tx, err := env.vocabEnv.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("gate begin: %v", err)
	}
	var id string
	if err := tx.QueryRow(context.Background(),
		`SELECT collection_id::text FROM collection_field_value WHERE collection_id=$1 AND field_id=$2 FOR UPDATE`,
		env.collID, fid).Scan(&id); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("gate lock: %v", err)
	}
	g := &gate{tx: tx}

	req := func(v string) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			return putJSONRaw(env.router, fmt.Sprintf("/collections/%s/fields/%s", env.collID, fid),
				map[string]any{"value_text": v, "if_unchanged_since": token})
		}
	}
	got := env.runContenders(t, g, req("collection A"), req("collection B"))

	if countStatus(got, http.StatusOK) != 1 || countStatus(got, http.StatusConflict) != 1 {
		t.Fatalf("want one 200 and one 409, got %d / %d", got[0].status, got[1].status)
	}
	var winner string
	for _, a := range got {
		if a.status == http.StatusOK {
			winner, _ = a.body["value_text"].(string)
		}
	}
	stored, ok := readCollectionStored(t, env.vocabEnv.pool, env.collID, fid)
	if !ok || stored.Text == nil || *stored.Text != winner {
		t.Errorf("persisted %v is not the winning write %q", stored.Text, winner)
	}
}

// ---------------------------------------------------------------------------
// Small plumbing the race harness needs and the sequential helpers do not
// ---------------------------------------------------------------------------

// putJSONRaw is putJSON without the *testing.T, so it can be called
// from inside a goroutine (t.Helper and t.Fatal are not goroutine-safe
// in the way a contender needs).
func putJSONRaw(r chi.Router, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func jsonUnmarshalSoft(b []byte, v any) error { return json.Unmarshal(b, v) }

func urlQueryEscape(s string) string { return url.QueryEscape(s) }
