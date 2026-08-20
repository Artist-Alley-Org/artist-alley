// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1147 — IIIF Content Search ran permanently as the disqualified
// viewer.
//
// # Why this asserts on the QUERY and not on the hits
//
// The defect was a field left unset. `serveCollection` built its
// `search.Query` with `Text`, `Types`, `Limit` and `CallerUserRef` and
// no `Mature`, so the zero MatureViewer went to the Engine — and the
// zero value of that struct is deliberately the viewer who qualifies for
// nothing. Every IIIF content search on the install therefore ran as a
// reader who had opted out, whoever was asking.
//
// That fails CLOSED, so it is not a leak. It is worse in a different
// way: it is invisible. An opted-in reader searching inside a collection
// simply never matched a mature member again, no error was logged, and
// an end-to-end assertion cannot tell "the gate refused" from "the gate
// was never wired" — both produce zero hits. The only observable that
// separates them is the value handed to the Engine, which is why the
// Engine is an interface here and why these two tests read the Query.
//
// Skips without AA_DB_PASSWORD: the handler loads the collection's
// membership from its pool before it reaches the Engine, and stubbing
// that out would mean an interface for a line that is not under test.

package content_search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// csFakeEngine records the last Query it was handed and answers with
// nothing. "Nothing" is the right answer for this fixture: the tests
// below are about what was ASKED, and returning hits would only add a
// membership filter that has its own coverage.
type csFakeEngine struct{ got search.Query }

func (f *csFakeEngine) Run(_ context.Context, q search.Query) (search.QueryResult, error) {
	f.got = q
	return search.QueryResult{}, nil
}

// csPool opens the test database, or skips.
func csPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + env("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// csSeedCollection plants a collection holding one member asset. The
// handler skips the Engine entirely for a collection with no members, so
// this is load-bearing rather than scenery — see the guard in csDrive.
func csSeedCollection(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var assetID, colID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO assets (title, asset_type, owner_user_ref)
		 VALUES ('cs_mature_member', 1, 11470001) RETURNING id`).Scan(&assetID); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO collections (name, owner_user_ref, visibility)
		 VALUES ('cs_mature_col', 11470001, 'public') RETURNING id`).Scan(&colID); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	// Through a POST (#1161, ADR 0091): a collection contains posts, and
	// the member set the search filter reads is now
	// collection_posts → post_assets.
	var postID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO posts (author_user_ref, title, visibility)
		 VALUES (11470001, 'cs_mature_post', 'public') RETURNING id`).Scan(&postID); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,0)`,
		postID, assetID); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO collection_posts (collection_id, post_id, sort_order, pinned)
		 VALUES ($1, $2, 0, TRUE)`, colID, postID); err != nil {
		t.Fatalf("pin post: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, postID)
	})
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, colID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, assetID)
	})
	return colID
}

// csDrive mounts the handler behind a middleware carrying `v`, exactly
// as http/server.go mounts matureViewerMiddleware after ResolveIdentity,
// and returns the Query the Engine was handed.
func csDrive(t *testing.T, v visibility.MatureViewer, withViewer bool) search.Query {
	t.Helper()
	pool := csPool(t)
	colID := csSeedCollection(t, pool)
	eng := &csFakeEngine{}
	h := &Handler{Pool: pool, Engine: eng}
	r := chi.NewRouter()
	if withViewer {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(
					visibility.WithMatureViewer(req.Context(), v)))
			})
		})
	}
	h.Mount(r)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/iiif/3/collection/"+colID.String()+"/search?q=anything", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("content search: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if eng.got.Text == "" {
		t.Fatal("the Engine was never called — the fixture stopped short of the line " +
			"under test, so neither assertion below means anything")
	}
	return eng.got
}

// TestContentSearch_CarriesTheRequestsMatureViewer is the control arm:
// a qualified reader's search must actually run as them.
//
// This is the leg that fails against the shipped code. The withheld leg
// below passes against it too — that is the whole point of the pairing.
func TestContentSearch_CarriesTheRequestsMatureViewer(t *testing.T) {
	qualified := visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true}
	got := csDrive(t, qualified, true)
	if got.Mature != qualified {
		t.Errorf("Query.Mature = %+v, want %+v. A reader who signed in, opted in and is "+
			"on an instance that allows mature content was searching as somebody who "+
			"had done none of those things — fail-closed, but silent and forever "+
			"(#1147)", got.Mature, qualified)
	}
	if !visibility.QualifiesForMature(got.Mature) {
		t.Error("the Engine ran DISQUALIFIED for a qualified reader")
	}
}

// TestContentSearch_DisqualifiedViewerStaysDisqualified is the withheld
// arm, and it also pins the direction the middleware's absence must fail
// in.
//
// Two cases, and the second is the one worth having: when the middleware
// never ran at all, visibility.MatureFromContext answers with the
// disqualified viewer rather than an error or a permissive default. A
// route mounted without it must show LESS, never more.
func TestContentSearch_DisqualifiedViewerStaysDisqualified(t *testing.T) {
	optedOut := visibility.MatureViewer{SignedIn: true, InstanceAllows: true}
	if got := csDrive(t, optedOut, true); visibility.QualifiesForMature(got.Mature) {
		t.Errorf("Query.Mature = %+v qualifies, but this reader never opted in", got.Mature)
	}

	// No middleware at all — the un-wired route.
	got := csDrive(t, visibility.MatureViewer{}, false)
	if visibility.QualifiesForMature(got.Mature) {
		t.Errorf("Query.Mature = %+v on a route that never resolved a viewer. An absent "+
			"value must be the DISQUALIFIED viewer: a gate that has lost its inputs "+
			"refuses rather than widens", got.Mature)
	}
}
