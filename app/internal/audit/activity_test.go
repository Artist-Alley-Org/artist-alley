// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #600 — GET /account/activity, driven end-to-end through the strict
// server.
//
// Every fixture row here is written by audit.Recorder — the production
// writer, the same one the handlers call — rather than by an INSERT the
// test composes. That is deliberate and it is load-bearing twice over:
// the Recorder is what decides which side of a row a user lands on
// (SessionRevoked puts the killer in actor and the victim in subject;
// AdminAssetSoftDeleted puts the deleter in actor and leaves subject
// NULL), so a hand-written row could assert a shape production never
// produces. And the ip / user_agent columns that this projection must
// never return are only populated by the Recorder's request path, so
// the "no ip on the wire" assertion is only worth anything if a real ip
// was actually written to the row first — which is why the fixtures
// below carry an *http.Request with a RemoteAddr and a User-Agent.
//
// Four properties:
//
//  1. Both halves arrive, correctly labelled. Rows the caller acted on
//     are by_me; rows that name them as subject are on_my_account; a
//     row where they are both is by_me exactly once.
//
//  2. It is caller-scoped, with a positive control. A third party's
//     rows never appear — proven against the same rows being visible to
//     the user they belong to, so an endpoint returning nothing fails
//     here rather than passing.
//
//  3. THE PROJECTION, ASSERTED ON THE WIRE. on_my_account rows are
//     checked as raw JSON keys, not as decoded struct fields: absence,
//     not emptiness. A decoded assertion cannot tell `"metadata": {}`
//     from no key at all, and it cannot see a field the Go type does
//     not declare — so a hand-written response that leaked
//     actor_user_ref would pass a struct check silently.
//
//  4. The (occurred_at, id) keyset holds across pages, including rows
//     that share occurred_at. Audit rows written inside one request
//     share a timestamp, so on this surface a tiebreak pinned the wrong
//     way loses or repeats most of the page, not an edge case of it.
//
// Skips without AA_DB_PASSWORD.

package audit_test

import (
	"context"
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

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

// --- harness ---------------------------------------------------------------

type activityShim struct {
	*strictservershim.PanicShim
	h *audit.AccountHandler
}

func (s activityShim) ListMyActivity(ctx context.Context, req openapi.ListMyActivityRequestObject) (openapi.ListMyActivityResponseObject, error) {
	return s.h.ListMyActivity(ctx, req)
}

type activityFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	rec  *audit.Recorder
	shim activityShim
}

func newActivityFixture(t *testing.T) *activityFixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openActivityPool(t, pwd)
	t.Cleanup(pool.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &activityFixture{
		t:    t,
		pool: pool,
		rec:  audit.NewRecorder(pool, logger),
		shim: activityShim{
			PanicShim: &strictservershim.PanicShim{},
			h:         audit.NewAccountHandler(pool, logger),
		},
	}
}

func openActivityPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	envOr := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + envOr("AA_DB_HOST", "postgres") +
		" port=" + envOr("AA_DB_PORT", "5432") +
		" user=" + envOr("AA_DB_USER", "artist_alley") +
		" dbname=" + envOr("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// routerAs mounts the strict server behind an identity. No capability
// is ever passed: this endpoint gates on being signed in and nothing
// else, and a test that granted one would hide a gate that had drifted.
func (f *activityFixture) routerAs(userRef int64) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := &auth.Identity{UserRef: userRef, AuthMethod: "session"}
			next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(f.shim, nil), r)
	return r
}

// rawGet returns the response code and the undecoded body, so the
// projection can be asserted on the bytes that actually go over the
// wire rather than on what a Go struct chose to keep.
func (f *activityFixture) rawGet(userRef int64, query string) (int, []byte) {
	f.t.Helper()
	rr := httptest.NewRecorder()
	url := "/account/activity"
	if query != "" {
		url += "?" + query
	}
	f.routerAs(userRef).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	return rr.Code, rr.Body.Bytes()
}

func (f *activityFixture) get(userRef int64, query string) openapi.ActivityList {
	f.t.Helper()
	code, body := f.rawGet(userRef, query)
	if code != http.StatusOK {
		f.t.Fatalf("GET /account/activity?%s as %d = %d, body=%s", query, userRef, code, body)
	}
	var page openapi.ActivityList
	if err := json.Unmarshal(body, &page); err != nil {
		f.t.Fatalf("decode activity page: %v (body=%s)", err, body)
	}
	return page
}

// rawItems re-reads the same response as a list of untyped key sets.
// This is the only view that can answer "is the key there at all".
func (f *activityFixture) rawItems(userRef int64, query string) []map[string]json.RawMessage {
	f.t.Helper()
	code, body := f.rawGet(userRef, query)
	if code != http.StatusOK {
		f.t.Fatalf("GET /account/activity?%s as %d = %d, body=%s", query, userRef, code, body)
	}
	var envelope struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		f.t.Fatalf("decode raw activity page: %v (body=%s)", err, body)
	}
	return envelope.Items
}

func (f *activityFixture) user(tag string) int64 {
	f.t.Helper()
	var ref int64
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
		"activity-"+tag+"-"+uuid.NewString()[:8],
	).Scan(&ref); err != nil {
		f.t.Fatalf("seed user %s: %v", tag, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM audit_events WHERE actor_user_ref = $1 OR subject_user_ref = $1`, ref)
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// req is the *http.Request the Recorder reads ip + user_agent from.
// Every fixture event carries one so the columns this endpoint must not
// return are genuinely populated in the row it reads.
func req() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/whatever", nil)
	r.RemoteAddr = "203.0.113.7:44321"
	r.Header.Set("User-Agent", "activity-test-agent/1.0")
	return r
}

// assertIPWasRecorded is the control for the "no ip on the wire"
// assertion. Without it, a Recorder that quietly stopped capturing ip
// would make the projection test pass for the wrong reason.
func (f *activityFixture) assertIPWasRecorded(eventType string, actor int64) {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events
		  WHERE event_type = $1 AND actor_user_ref = $2
		    AND ip IS NOT NULL AND user_agent IS NOT NULL`,
		eventType, actor).Scan(&n); err != nil {
		f.t.Fatalf("count ip rows: %v", err)
	}
	if n == 0 {
		f.t.Fatalf("control failed: no %s row for actor %d carries an ip + user_agent, "+
			"so asserting their absence from the response proves nothing", eventType, actor)
	}
}

func byID(page openapi.ActivityList) map[uuid.UUID]openapi.ActivityEvent {
	out := make(map[uuid.UUID]openapi.ActivityEvent, len(page.Items))
	for _, it := range page.Items {
		out[uuid.UUID(it.Id)] = it
	}
	return out
}

// --- 1. both halves, correctly labelled ------------------------------------

func TestListMyActivity_RolesAndBothHalves(t *testing.T) {
	f := newActivityFixture(t)
	me := f.user("me")
	admin := f.user("admin")

	// by_me — I deleted an asset. subject is NULL on this event and the
	// actor is the deleter, so it can only reach me as an act of mine.
	f.rec.AdminAssetSoftDeleted(context.Background(), req(), uuid.NewString(), me, "tidying up")

	// on_my_account — an admin disabled my account. I am the subject,
	// they are the actor.
	f.rec.AdminUserDisabled(context.Background(), req(), me, admin, "active", "disabled", "policy")

	// by_me, and BOTH — a self-service password change records me as
	// actor and subject at once. It must arrive once, as by_me.
	f.rec.PasswordChanged(context.Background(), req(), me, 3)

	page := f.get(me, "limit=200")
	if len(page.Items) != 3 {
		t.Fatalf("expected exactly 3 rows for me, got %d: %+v", len(page.Items), page.Items)
	}

	roles := map[string]openapi.ActivityEventRole{}
	seen := map[string]int{}
	for _, it := range page.Items {
		roles[it.EventType] = it.Role
		seen[it.EventType]++
	}

	for _, tc := range []struct {
		eventType string
		want      openapi.ActivityEventRole
	}{
		{"admin.asset.soft_deleted", openapi.ByMe},
		{"admin.users.disabled", openapi.OnMyAccount},
		{"user.password_changed", openapi.ByMe},
	} {
		got, ok := roles[tc.eventType]
		if !ok {
			t.Fatalf("%s missing from my activity", tc.eventType)
		}
		if got != tc.want {
			t.Errorf("%s role = %q, want %q", tc.eventType, got, tc.want)
		}
	}

	// The disjointness of the two UNION arms, asserted rather than
	// assumed: actor == subject == me must not emit twice.
	if n := seen["user.password_changed"]; n != 1 {
		t.Errorf("actor-and-subject row emitted %d times, want exactly 1", n)
	}
}

// --- 2. caller-scoped, with a positive control -----------------------------

func TestListMyActivity_IsCallerScopedNotAProbe(t *testing.T) {
	f := newActivityFixture(t)
	me := f.user("me")
	other := f.user("other")
	admin := f.user("admin")

	// Rows that name `other` on each side, and nothing to do with me.
	f.rec.AdminAssetSoftDeleted(context.Background(), req(), uuid.NewString(), other, "theirs")
	f.rec.AdminUserDisabled(context.Background(), req(), other, admin, "active", "disabled", "theirs")

	// One row of my own, so the assertion below is not satisfied by an
	// endpoint that returns nothing.
	f.rec.PasswordChanged(context.Background(), req(), me, 0)

	// POSITIVE CONTROL — those rows DO reach the user they belong to.
	otherPage := f.get(other, "limit=200")
	if len(otherPage.Items) != 2 {
		t.Fatalf("control failed: other's own activity has %d rows, want 2", len(otherPage.Items))
	}

	minePage := f.get(me, "limit=200")
	if len(minePage.Items) != 1 {
		t.Fatalf("my activity has %d rows, want exactly my own 1: %+v",
			len(minePage.Items), minePage.Items)
	}
	if minePage.Items[0].EventType != "user.password_changed" {
		t.Errorf("my only row is %q, want user.password_changed", minePage.Items[0].EventType)
	}

	// The admin acted on `other` and is neither actor nor subject of my
	// row, so my row must not reach them either.
	adminPage := f.get(admin, "limit=200")
	for _, it := range adminPage.Items {
		if it.EventType == "user.password_changed" {
			t.Errorf("my password change leaked into the admin's activity")
		}
	}
}

// --- 3. the projection, on the wire ----------------------------------------

// Absence, not emptiness — and asserted against the raw JSON, because
// the decoded struct cannot represent the difference between "no key"
// and "empty value", nor see a key the Go type never declared.
func TestListMyActivity_OnMyAccountRowDisclosesNothingButTheAct(t *testing.T) {
	f := newActivityFixture(t)
	me := f.user("me")
	admin := f.user("admin")

	// An administrative act ON my account whose metadata is exactly the
	// kind of thing that must not travel: the admin's free-text note.
	const adminNote = "escalated-by-legal-do-not-disclose"
	f.rec.AdminUserDisabled(context.Background(), req(), me, admin, "active", "disabled", adminNote)

	// CONTROL — the row really does carry ip + user_agent + that note,
	// so their absence below is a projection decision and not an empty
	// table. (The row is keyed by the admin as actor.)
	f.assertIPWasRecorded("admin.users.disabled", admin)
	var storedNote string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT metadata->>'reason' FROM audit_events
		  WHERE event_type = 'admin.users.disabled' AND subject_user_ref = $1`,
		me).Scan(&storedNote); err != nil {
		t.Fatalf("read stored note: %v", err)
	}
	if storedNote != adminNote {
		t.Fatalf("control failed: stored note = %q, want %q", storedNote, adminNote)
	}

	items := f.rawItems(me, "limit=200")
	if len(items) != 1 {
		t.Fatalf("expected 1 row, got %d", len(items))
	}
	row := items[0]

	// The keys that MUST be there — the act and when it happened.
	for _, k := range []string{"id", "event_type", "occurred_at", "role"} {
		if _, ok := row[k]; !ok {
			t.Errorf("on_my_account row is missing required key %q", k)
		}
	}
	// The keys that must NOT be there, at all.
	for _, k := range []string{
		"metadata", "actor_user_ref", "subject_user_ref", "ip", "user_agent",
	} {
		if v, ok := row[k]; ok {
			t.Errorf("on_my_account row leaked key %q = %s", k, v)
		}
	}

	// And the note itself is nowhere in the serialised row, whatever
	// key it might have arrived under.
	blob, _ := json.Marshal(row)
	if strings.Contains(string(blob), adminNote) {
		t.Errorf("the admin's private note appears in the response: %s", blob)
	}
}

// The by_me half of the same rule: my own action's payload IS mine, and
// dropping it would make the page useless rather than safe.
func TestListMyActivity_ByMeRowKeepsItsMetadata(t *testing.T) {
	f := newActivityFixture(t)
	me := f.user("me")
	assetID := uuid.NewString()

	f.rec.AdminAssetSoftDeleted(context.Background(), req(), assetID, me, "my own reason")

	items := f.rawItems(me, "limit=200")
	if len(items) != 1 {
		t.Fatalf("expected 1 row, got %d", len(items))
	}
	raw, ok := items[0]["metadata"]
	if !ok {
		t.Fatalf("by_me row dropped its metadata: %+v", items[0])
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("metadata is not an object: %s", raw)
	}
	if meta["asset_id"] != assetID {
		t.Errorf("metadata.asset_id = %v, want %s", meta["asset_id"], assetID)
	}

	// Still no request context, even on a row that is entirely mine —
	// /account/sessions owns that question.
	f.assertIPWasRecorded("admin.asset.soft_deleted", me)
	for _, k := range []string{"ip", "user_agent", "actor_user_ref", "subject_user_ref"} {
		if v, ok := items[0][k]; ok {
			t.Errorf("by_me row leaked key %q = %s", k, v)
		}
	}
}

// --- 4. the keyset ---------------------------------------------------------

// Every row written inside ONE transaction shares occurred_at exactly:
// the column defaults to now(), which in Postgres is transaction start
// time, not statement time. That is not a contrivance — Recorder.WriteInTx
// exists precisely so a domain write and its audit rows commit together,
// so a burst of rows on one timestamp is a shape production emits.
//
// It is also the ONLY way to get the collision. An earlier version of
// this test wrote the rows through the pool in a loop and guarded with a
// skip if they failed to collide; the guard fired every run — each
// pool write is its own transaction and gets its own clock — so the
// tiebreak was never once exercised by a test that reported success.
//
// Paged one at a time, every row must appear exactly once, in the same
// order a single unpaginated read gives.
func TestListMyActivity_KeysetPagesWithoutLossOrRepeat(t *testing.T) {
	f := newActivityFixture(t)
	me := f.user("me")

	const total = 7
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	q := audit.New(f.pool).WithTx(tx)
	for i := 0; i < total; i++ {
		f.rec.WriteInTx(ctx, q, "user.password_changed", nil, &me,
			map[string]any{"sessions_revoked": i})
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// CONTROL — the tiebreak is only exercised if the rows really do
	// collide on occurred_at. A FAILURE, not a skip: a test that quietly
	// opts out of its own subject is a test that reports success for
	// never having run.
	var distinctTimes int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(DISTINCT occurred_at) FROM audit_events WHERE actor_user_ref = $1`,
		me).Scan(&distinctTimes); err != nil {
		t.Fatalf("count distinct times: %v", err)
	}
	if distinctTimes != 1 {
		t.Fatalf("control failed: %d rows written in one transaction landed on %d distinct "+
			"timestamps, want 1 — the id tiebreak would not be exercised", total, distinctTimes)
	}

	seen := map[uuid.UUID]int{}
	var order []uuid.UUID
	cursor := ""
	for page := 0; page < total+3; page++ {
		q := "limit=1"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		p := f.get(me, q)
		for _, it := range p.Items {
			seen[uuid.UUID(it.Id)]++
			order = append(order, uuid.UUID(it.Id))
		}
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}

	if len(order) != total {
		t.Fatalf("paged %d rows one at a time, want %d", len(order), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s returned %d times across pages, want 1", id, n)
		}
	}

	// The paged order must match the order a single unpaginated read
	// gives — the keyset is a way of walking one ordering, not a second
	// ordering of its own.
	oneShot := f.get(me, "limit=200")
	if len(oneShot.Items) != total {
		t.Fatalf("one-shot read returned %d rows, want %d", len(oneShot.Items), total)
	}
	for i, it := range oneShot.Items {
		if uuid.UUID(it.Id) != order[i] {
			t.Fatalf("paged order diverges at %d: paged %s, one-shot %s",
				i, order[i], uuid.UUID(it.Id))
		}
	}

	// Descending on (occurred_at, id), tiebreak included: consecutive
	// rows sharing a timestamp must descend by id. A tiebreak pinned
	// the other way would still page 7 rows — it would just page the
	// wrong 7 the moment a cursor landed mid-collision.
	for i := 1; i < len(oneShot.Items); i++ {
		prev, cur := oneShot.Items[i-1], oneShot.Items[i]
		if cur.OccurredAt.After(prev.OccurredAt) {
			t.Fatalf("rows out of time order at %d", i)
		}
		if cur.OccurredAt.Equal(prev.OccurredAt) &&
			uuid.UUID(cur.Id).String() >= uuid.UUID(prev.Id).String() {
			t.Errorf("tiebreak not descending at %d: %s then %s",
				i, uuid.UUID(prev.Id), uuid.UUID(cur.Id))
		}
	}
}

// --- guards ----------------------------------------------------------------

func TestListMyActivity_RefusesAnonymousAndZeroRef(t *testing.T) {
	f := newActivityFixture(t)
	if code, _ := f.rawGet(0, ""); code != http.StatusUnauthorized {
		t.Errorf("ref 0 got %d, want 401", code)
	}
}

func TestListMyActivity_RefusesAMalformedCursor(t *testing.T) {
	f := newActivityFixture(t)
	me := f.user("me")
	if code, _ := f.rawGet(me, "cursor=not-a-cursor"); code != http.StatusBadRequest {
		t.Errorf("malformed cursor got %d, want 400", code)
	}
}
