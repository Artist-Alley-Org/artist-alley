// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25a: the PRE-PERSISTENCE CONTRACT: a saved search may
// not persist a canonical query that execution will later reject, and
// what it persists is the canonical form.
//
// # What was true before
//
// `create` composed the canonical string and called `dsl.Parse` on it,
// which proves SYNTAX. Execution parses, compiles and bridges into a
// facet selection, and each later step refuses things the parse
// accepts. So `NOT !nopreviews` saved with a 201 and would have failed
// on every coordinator tick, and `!nopreviews` saved as the alias
// rather than as the dimension.
//
// # Every rejection is asserted on the TABLE, not on the status
//
// A 400 with a row written is the defect this file exists to catch, so
// each rejected create counts the owner's rows before and after, and
// the rejected patch re-reads the stored row and requires it unchanged.
//
// # Driven through the mounted routes
//
// [Handler.Mount] on a chi router, with the identity on the request
// context, so the path exercised is the one the browser uses. The
// replay half runs the real [Executor] over the real Engine against a
// fixture, so "replays as the dimension" is a row count, not a Query
// inspection.
//
// Written against the surface, so on the commit before this sprint the
// file compiles and the positive assertions go red: a verb stored as
// typed, a placement error stored with a 201.
//
// Skips without AA_DB_PASSWORD.

package saved_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search"
	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/search/saved"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

const (
	ssOwner  int64 = 25030101
	ssPhrase       = "zindlecraw"
)

func ssPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	envOr := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	dsn := "host=" + envOr("AA_DB_HOST", "postgres") +
		" port=" + envOr("AA_DB_PORT", "5432") +
		" user=" + envOr("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type ssFixture struct {
	// Two previewable public assets by the owner: one with a `col`, one
	// without, so `preview:missing` replays to exactly one row.
	withCol, noCol uuid.UUID
	// A public post, so an asset-only replay of a list naming it
	// contributes nothing.
	post uuid.UUID
}

func ssSeed(t *testing.T, pool *pgxpool.Pool) ssFixture {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username) VALUES ($1, $2)
		 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
		ssOwner, "ss-owner-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, ssOwner,
			`DELETE FROM saved_search WHERE owner_user_ref = $1`,
			`DELETE FROM "user" WHERE ref = $1`)
	})
	asset := func(label string, withCol bool) uuid.UUID {
		id := uuid.New()
		sum := sha256.Sum256([]byte("ss " + id.String()))
		hash := hex.EncodeToString(sum[:])
		if _, err := pool.Exec(ctx, `INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 16, 'fs')`, hash); err != nil {
			t.Fatalf("seed object: %v", err)
		}
		t.Cleanup(func() { testdb.Purge(t, pool, hash, `DELETE FROM storage_objects WHERE hash = $1`) })
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
			                    sensitivity, processing_status, file_extension, file_hash)
			VALUES ($1,$2,'fixture body',$3,(SELECT MIN(ref) FROM asset_types),'active','public','ready','png',$4)`,
			id, ssPhrase+" "+label, ssOwner, hash); err != nil {
			t.Fatalf("seed asset: %v", err)
		}
		t.Cleanup(func() { testdb.Purge(t, pool, id, `DELETE FROM assets WHERE id = $1`) })
		if withCol {
			if _, err := pool.Exec(ctx, `INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1, 'col', 8)`, hash); err != nil {
				t.Fatalf("seed col: %v", err)
			}
		}
		return id
	}
	f := ssFixture{withCol: asset("with col", true), noCol: asset("no col", false), post: uuid.New()}
	if _, err := pool.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, description, visibility)
		VALUES ($1, $2, $3, $3, 'public')`, f.post, ssOwner, ssPhrase+" post"); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() { testdb.Purge(t, pool, f.post, `DELETE FROM posts WHERE id = $1`) })
	return f
}

type ssRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	router chi.Router
	store  *saved.Store
}

func ssNewRig(t *testing.T, pool *pgxpool.Pool) ssRig {
	t.Helper()
	store := saved.NewStore(pool)
	// The cap is per-owner and this file saves more than ten rows.
	store.MaxPerUser = 1000
	h := &saved.Handler{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := chi.NewRouter()
	h.Mount(r)
	return ssRig{t: t, pool: pool, router: r, store: store}
}

func (r ssRig) do(method, path string, body any) (int, map[string]any) {
	r.t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		r.t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, &buf)
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserRef: ssOwner, AuthMethod: "session"}))
	rr := httptest.NewRecorder()
	r.router.ServeHTTP(rr, req)
	out := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func (r ssRig) create(name, dslInput string, filters []string) (int, map[string]any) {
	r.t.Helper()
	if filters == nil {
		filters = []string{}
	}
	return r.do(http.MethodPost, "/search/saved", map[string]any{
		"name": name, "dsl": dslInput, "filters": filters, "notify_channel": "none",
	})
}

func (r ssRig) rows() int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM saved_search WHERE owner_user_ref = $1`, ssOwner).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r ssRig) stored(name string) string {
	r.t.Helper()
	var s string
	if err := r.pool.QueryRow(context.Background(),
		`SELECT dsl FROM saved_search WHERE owner_user_ref = $1 AND name = $2`, ssOwner, name).Scan(&s); err != nil {
		r.t.Fatalf("stored %q: %v", name, err)
	}
	return s
}

// replay runs the stored row through the real executor over the real
// engine and returns the asset ids it found.
func (r ssRig) replay(id string) []uuid.UUID {
	r.t.Helper()
	row, err := r.store.Get(context.Background(), uuid.MustParse(id))
	if err != nil {
		r.t.Fatal(err)
	}
	res, err := saved.NewExecutor(r.pool, search.NewEngine(r.pool), nil).Run(context.Background(), row)
	if err != nil {
		r.t.Fatalf("replay %q: %v", row.DSL, err)
	}
	return res.HitIDs
}

func ssIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, uuid.NewSHA1(uuid.NameSpaceOID, []byte{byte(i), byte(i >> 8), 0x25}).String())
	}
	return out
}

// ── accepted, stored canonical, replayed as the dimension ────────────

func TestSavedSearch_PreviewSavesCanonicalAndReplays(t *testing.T) {
	pool := ssPool(t)
	f := ssSeed(t, pool)
	rig := ssNewRig(t, pool)
	for i, in := range []string{"!nopreviews", "preview:missing", ssPhrase + " AND !nopreviews"} {
		name := "pv-" + string(rune('a'+i))
		status, body := rig.create(name, in, nil)
		if status != http.StatusCreated {
			t.Fatalf("create %q → %d %v", in, status, body)
		}
		stored := rig.stored(name)
		if strings.Contains(stored, "!") || !strings.Contains(stored, "preview:missing") {
			t.Errorf("create %q stored %q; the column must hold the canonical form", in, stored)
		}
		got := rig.replay(body["id"].(string))
		found := map[uuid.UUID]bool{}
		for _, id := range got {
			found[id] = true
		}
		if !found[f.noCol] || found[f.withCol] {
			t.Errorf("replay of %q (stored %q) returned noCol=%v withCol=%v; want true/false: the "+
				"stored query must replay as the DIMENSION", in, stored, found[f.noCol], found[f.withCol])
		}
	}
}

func TestSavedSearch_IDListSavesCanonicalAndReplaysAssetOnly(t *testing.T) {
	pool := ssPool(t)
	f := ssSeed(t, pool)
	rig := ssNewRig(t, pool)
	a, b := f.noCol.String(), f.withCol.String()
	for i, c := range []struct {
		in        string
		wantInDSL string
	}{
		{"!list" + a, "id:" + a},
		{"!list" + a + "," + b, "(id:" + a + " AND id:" + b + ")"},
		{"id:" + a, "id:" + a},
		{"id:" + a + " AND id:" + b, "id:" + a + " AND id:" + b},
		// A post id in an asset-only replay contributes nothing.
		{"!list" + a + "," + f.post.String(), "id:" + f.post.String()},
	} {
		name := "id-" + string(rune('a'+i))
		status, body := rig.create(name, c.in, nil)
		if status != http.StatusCreated {
			t.Fatalf("create %q → %d %v", c.in, status, body)
		}
		stored := rig.stored(name)
		if strings.Contains(stored, "!list") || !strings.Contains(stored, c.wantInDSL) {
			t.Errorf("create %q stored %q, want it to carry %q and no alias", c.in, stored, c.wantInDSL)
		}
		got := rig.replay(body["id"].(string))
		want := map[uuid.UUID]bool{f.noCol: true}
		if strings.Contains(c.in, b) {
			want[f.withCol] = true
		}
		if len(got) != len(want) {
			t.Errorf("replay of %q returned %d ids, want %d (%v)", c.in, len(got), len(want), got)
		}
		for _, id := range got {
			if !want[id] || id == f.post {
				t.Errorf("replay of %q returned %v", c.in, id)
			}
		}
	}
}

func TestSavedSearch_FiftyDistinctFromFiftyTwoRawIsStoredAndReparses(t *testing.T) {
	pool := ssPool(t)
	ssSeed(t, pool)
	rig := ssNewRig(t, pool)
	ids := ssIDs(50)
	raw := append(append([]string{}, ids...), strings.ToUpper(ids[0]), "{"+ids[1]+"}")
	status, body := rig.create("fifty", "!list"+strings.Join(raw, ","), nil)
	if status != http.StatusCreated {
		t.Fatalf("52 raw / 50 distinct → %d %v", status, body)
	}
	stored := rig.stored("fifty")
	if len(stored) >= dsl.MaxInputBytes {
		t.Errorf("stored form is %d bytes, at or over the %d cap", len(stored), dsl.MaxInputBytes)
	}
	// The executor's three steps, spelled out so this witness compiles
	// on the commit before search.CompileDSL existed.
	parsed, err := dsl.Parse(stored)
	if err != nil {
		t.Fatalf("the stored form does not parse: %v", err)
	}
	compiled, err := dsl.Compile(parsed)
	if err != nil {
		t.Fatalf("the stored form does not compile: %v", err)
	}
	sel, err := search.SelectionFromDSL(compiled.Filters, facet.Selection{})
	if err != nil {
		t.Fatalf("the stored form does not bridge: %v", err)
	}
	if n := len(sel.Terms()); n != 50 {
		t.Errorf("the stored form reparses to %d terms, want 50", n)
	}
	// And through the rail's spelling, which the handler parses itself.
	status, body = rig.create("fifty-filter", "", func() []string {
		out := make([]string, 0, len(raw))
		for _, id := range raw {
			out = append(out, "id:"+id)
		}
		return out
	}())
	if status != http.StatusCreated {
		t.Fatalf("52 raw filters / 50 distinct → %d %v", status, body)
	}
}

// ── rejected BEFORE persistence ──────────────────────────────────────

func TestSavedSearch_ExecutionRejectsAreRefusedBeforePersistence(t *testing.T) {
	pool := ssPool(t)
	f := ssSeed(t, pool)
	rig := ssNewRig(t, pool)
	a := f.noCol.String()
	fiftyOne := ssIDs(51)
	fiftyOneFilters := make([]string, 0, 51)
	for _, id := range fiftyOne {
		fiftyOneFilters = append(fiftyOneFilters, "id:"+id)
	}
	// 50 ids (2,195 bytes canonical) plus enough free text to cross the
	// cap: the composed string is over MaxInputBytes and the SIZE
	// authority refuses it.
	oversized := strings.Repeat("word ", 400) + "!list" + strings.Join(ssIDs(50), ",")

	for _, c := range []struct {
		name     string
		dsl      string
		filters  []string
		wantCode string
	}{
		{"NOT !nopreviews", "NOT !nopreviews", nil, "dsl_error"},
		{"NOT preview:missing", "NOT preview:missing", nil, "dsl_error"},
		{"NOT !list", "NOT !list" + a, nil, "dsl_error"},
		{"NOT id:", "NOT id:" + a, nil, "dsl_error"},
		{"OR !nopreviews", ssPhrase + " OR !nopreviews", nil, "dsl_error"},
		{"OR id:", ssPhrase + " OR id:" + a, nil, "dsl_error"},
		{"preview:present", "preview:present", nil, "dsl_error"},
		{"filter=preview:present", "", []string{"preview:present"}, "invalid_filter"},
		{"51 via !list", "!list" + strings.Join(fiftyOne, ","), nil, "dsl_error"},
		{"51 via id: chain", "id:" + strings.Join(fiftyOne, " AND id:"), nil, "dsl_error"},
		{"51 via filter=id:", "", fiftyOneFilters, "invalid_filter"},
		{"51 split across dsl and filter", "!list" + strings.Join(fiftyOne[:26], ","), fiftyOneFilters[26:], "dsl_error"},
		{"oversized composed DSL", oversized, nil, "dsl_parse_error"},
		{"unknown verb", "!bogus", nil, "dsl_error"},
		{"25b verb pinned unknown", "!last5", nil, "dsl_error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := rig.rows()
			status, body := rig.create("rej-"+c.name, c.dsl, c.filters)
			if status != http.StatusBadRequest {
				t.Errorf("→ %d %v, want 400", status, body)
			}
			if got, _ := body["error"].(string); got != c.wantCode {
				t.Errorf("error code %q, want %q (%v)", got, c.wantCode, body)
			}
			if after := rig.rows(); after != before {
				t.Errorf("a saved_search row was WRITTEN for a query execution rejects (%d → %d rows)", before, after)
			}
		})
	}
}

func TestSavedSearch_RejectedPatchLeavesTheRowUnchanged(t *testing.T) {
	pool := ssPool(t)
	f := ssSeed(t, pool)
	rig := ssNewRig(t, pool)
	status, body := rig.create("patched", ssPhrase, nil)
	if status != http.StatusCreated {
		t.Fatalf("create → %d %v", status, body)
	}
	id := body["id"].(string)
	original := rig.stored("patched")

	for _, bad := range []string{"NOT !nopreviews", "NOT id:" + f.noCol.String(), "preview:present",
		"!list" + strings.Join(ssIDs(51), ","), "!bogus"} {
		status, body := rig.do(http.MethodPatch, "/search/saved/"+id, map[string]any{"dsl": bad})
		if status != http.StatusBadRequest {
			t.Errorf("patch %q → %d %v, want 400", bad, status, body)
		}
		if now := rig.stored("patched"); now != original {
			t.Errorf("patch %q CHANGED the stored row: %q → %q", bad, original, now)
		}
	}
	// And an accepted patch stores the canonical form.
	status, body = rig.do(http.MethodPatch, "/search/saved/"+id, map[string]any{"dsl": ssPhrase + " AND !nopreviews"})
	if status != http.StatusOK {
		t.Fatalf("patch → %d %v", status, body)
	}
	if now := rig.stored("patched"); now != ssPhrase+" AND preview:missing" {
		t.Errorf("accepted patch stored %q, want the canonical %q", now, ssPhrase+" AND preview:missing")
	}
	got := rig.replay(id)
	if len(got) != 1 || got[0] != f.noCol {
		t.Errorf("patched row replays to %v, want exactly the no-col asset", got)
	}
}
