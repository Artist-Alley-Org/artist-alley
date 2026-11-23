// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// POST /api/v1/fields/import/preview (#1576 S1) through the production
// router and a real database: authorization before any body read, the
// size bounds, the per-field local states and differences, the bound
// state, the admission caps under concurrency, expired payloads that
// still count, and the cleanup. The decoder's rules are unit-tested in
// app/internal/metadata/schema_import_decode_test.go.

package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

type importEnv struct {
	*exportEnv
	srv     *Server
	preview string
}

func newImportEnv(t *testing.T) *importEnv {
	t.Helper()
	pool, _ := openServerPool(t)
	initAtrestForOutboxWiring(t)
	cfg := config.Config{StorageBackend: "fs", StorageFSRoot: t.TempDir(), HTTPAddr: "127.0.0.1:0"}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), pool, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.lifecycleCancel)
	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(ts.Close)
	e := &exportEnv{t: t, ctx: context.Background(), pool: pool, url: ts.URL + "/api/v1/fields/export",
		prefix: "xi" + strings.ReplaceAll(uuid.NewString()[:8], "-", ""), ids: map[string]string{}}
	t.Cleanup(e.cleanup)
	// Previews are global state; start and end with none.
	clearPreviews := func() { _, _ = pool.Exec(context.Background(), `DELETE FROM field_schema_import_preview`) }
	clearPreviews()
	t.Cleanup(clearPreviews)
	e.admin = e.principal("fields.admin")
	e.sysAdmin = e.principal("system.admin")
	e.member = e.principal("")
	return &importEnv{exportEnv: e, srv: s, preview: ts.URL + "/api/v1/fields/import/preview"}
}

func (e *importEnv) postPreview(token string, body []byte) exportResp {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.preview, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return exportResp{resp.StatusCode, b}
}

// envelope wraps an export body (with its newline) as a preview request.
func envelope(exportBody []byte) []byte {
	return append(append([]byte(`{"document":`), bytes.TrimSuffix(exportBody, []byte("\n"))...), '}')
}

type previewOut struct {
	PreviewID      string    `json:"preview_id"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	DocumentDigest string    `json:"document_digest"`
	Fields         []struct {
		Code        string   `json:"code"`
		Local       string   `json:"local"`
		LocalID     *string  `json:"local_id"`
		Problem     *string  `json:"problem"`
		Decisions   []string `json:"decisions"`
		Differences []struct {
			Property          string          `json:"property"`
			Local             json.RawMessage `json:"local"`
			Incoming          json.RawMessage `json:"incoming"`
			TermsOnlyIncoming []string        `json:"terms_only_incoming"`
			TermsOnlyLocal    []string        `json:"terms_only_local"`
			TermsChanged      []string        `json:"terms_changed"`
		} `json:"differences"`
	} `json:"fields"`
	UnresolvedTerms []struct{ Field, Term string } `json:"unresolved_terms"`
}

func decodePreview(t *testing.T, r exportResp) previewOut {
	t.Helper()
	if r.status != 200 {
		t.Fatalf("preview status %d: %.400s", r.status, r.body)
	}
	var p previewOut
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// portable builds one portable field as JSON.
func portable(code, typ, label, opts, cond string) string {
	if opts == "" {
		opts = "null"
	}
	if cond == "" {
		cond = "null"
	}
	return fmt.Sprintf(`{"code":%q,"description":"","display_condition":%s,"display_group":"general","display_order":100,`+
		`"edit_tab":null,"label":%q,"open_vocabulary":false,"options":%s,"regexp_filter":null,"required":false,`+
		`"searchable":true,"show_in_advanced_search":true,"show_on_card":false,"show_on_upload":true,"subject_kind":"asset","type":%q}`,
		code, cond, label, opts, typ)
}

func previewDoc(fields ...string) []byte {
	return []byte(`{"document":{"fields":[` + strings.Join(fields, ",") + `],"format":"aa-field-schema/1"}}`)
}

// readRecorder counts body reads; authorization must not read a byte.
type readRecorder struct {
	r     io.Reader
	reads atomic.Int64
}

func (rr *readRecorder) Read(p []byte) (int, error) {
	rr.reads.Add(1)
	return rr.r.Read(p)
}
func (rr *readRecorder) Close() error { return nil }

func TestImportPreview_AuthorizationBeforeBody(t *testing.T) {
	e := newImportEnv(t)
	valid := previewDoc(portable(e.code("a"), "text", "A", "", ""))
	bodies := map[string][]byte{
		"valid":     valid,
		"malformed": []byte(`{"document":`),
		"oversized": bytes.Repeat([]byte("x"), metadata.FieldSchemaImportPreviewMaxBodyBytes+10),
	}
	serve := func(token string, body []byte) (int, []byte, int64) {
		rr := &readRecorder{r: bytes.NewReader(body)}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/fields/import/preview", nil)
		req.Body = rr
		req.ContentLength = -1
		if token != "" {
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
		}
		w := httptest.NewRecorder()
		e.srv.srv.Handler.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes(), rr.reads.Load()
	}
	for _, c := range []struct {
		name, token string
		want        int
	}{{"anonymous", "", 401}, {"member", e.member, 403}} {
		var first []byte
		for name, b := range bodies {
			code, body, reads := serve(c.token, b)
			if code != c.want || reads != 0 {
				t.Fatalf("%s/%s: status %d reads %d, want %d with no read", c.name, name, code, reads, c.want)
			}
			if first == nil {
				first = body
			} else if !bytes.Equal(first, body) {
				t.Fatalf("%s: refusal differs by body", c.name)
			}
		}
	}
	for name, tok := range map[string]string{"fields.admin": e.admin, "system.admin": e.sysAdmin} {
		code, body, _ := serve(tok, valid)
		if code != 200 {
			t.Fatalf("%s: %d %.300s (the raw route must answer; the generated method refuses)", name, code, body)
		}
		_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
	}
	// Oversized with no Content-Length: read through the cap, then 413.
	if code, _, _ := serve(e.admin, bodies["oversized"]); code != 413 {
		t.Fatalf("oversized chunked: %d", code)
	}
	// Oversized by Content-Length: 413 without reading.
	rr := &readRecorder{r: bytes.NewReader(nil)}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fields/import/preview", nil)
	req.Body, req.ContentLength = rr, metadata.FieldSchemaImportPreviewMaxBodyBytes+1
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: e.admin})
	w := httptest.NewRecorder()
	e.srv.srv.Handler.ServeHTTP(w, req)
	if w.Code != 413 || rr.reads.Load() != 0 {
		t.Fatalf("declared oversize: %d reads %d", w.Code, rr.reads.Load())
	}
	if r := e.postPreview(e.admin, []byte(`{"document":`)); r.status != 400 {
		t.Fatalf("malformed: %d %s", r.status, r.body)
	}
}

func TestImportPreview_StatesDifferencesAndBinding(t *testing.T) {
	e := newImportEnv(t)
	same, kind, vocab, gone, mirror, gonemirror, absent := e.code("same"), e.code("kind"), e.code("vocab"),
		e.code("gone"), e.code("mirror"), e.code("gonemirror"), e.code("absent")
	e.field(same, "select", map[string]any{"options": `{"values":[{"value":"a","label":"A","aliases":["alpha"]},{"value":"old","status":"archived"}]}`})
	e.field(kind, "text", nil)
	e.field(vocab, "select", map[string]any{"options": `{"values":[{"value":"wip","label":"WIP"},"legacy"]}`})
	e.field(gone, "text", map[string]any{"status": "deprecated"})
	e.field(mirror, "text", map[string]any{"mirrors_column": "title", "read_only": false})
	e.field(gonemirror, "text", map[string]any{"mirrors_column": "title", "read_only": false, "status": "archived"})

	// The identical one comes from a real export of the local row.
	exp := e.export(e.admin, same)
	if exp.status != 200 {
		t.Fatalf("export: %d %s", exp.status, exp.body)
	}
	var exported struct {
		Fields []json.RawMessage `json:"fields"`
	}
	_ = json.Unmarshal(exp.body, &exported)
	sameJSON := string(exported.Fields[0])

	fields := []string{
		portable(absent, "text", "Absent", "", fmt.Sprintf(`["%s=a","%s~x"]`, same, e.code("nowhere"))),
		portable(gone, "text", "Gone", "", ""),
		portable(gonemirror, "text", "Gone mirror", "", ""),
		portable(kind, "select", "Label "+kind, `[]`, ""),
		portable(mirror, "text", "Label "+mirror, "", ""),
		sameJSON,
		portable(vocab, "select", "Label "+vocab, `[{"children":[],"label":"Work in progress","value":"wip"},{"children":[],"label":null,"value":"review"}]`, ""),
	}
	slices.SortFunc(fields, func(a, b string) int { return strings.Compare(a[9:40], b[9:40]) })
	before := e.snapshot()
	p := decodePreview(t, e.postPreview(e.admin, previewDoc(fields...)))
	if after := e.snapshot(); after != before {
		t.Fatal("a preview changed stored definitions, permissions, defaults or values")
	}

	want := map[string]struct {
		local     string
		decisions []string
	}{
		absent:     {"absent", []string{"create", "skip"}},
		gone:       {"retired", []string{"skip"}},
		gonemirror: {"retired", []string{"skip"}}, // retired wins over mirrored
		kind:       {"different", []string{"keep_local", "skip"}},
		mirror:     {"mirrored", []string{"keep_local", "skip"}},
		same:       {"identical", []string{"use_existing", "skip"}},
		vocab:      {"different", []string{"keep_local", "skip"}},
	}
	for _, f := range p.Fields {
		w := want[f.Code]
		if f.Local != w.local || !slices.Equal(f.Decisions, w.decisions) {
			t.Errorf("%s: local %s decisions %v, want %s %v", f.Code, f.Local, f.Decisions, w.local, w.decisions)
		}
		for _, d := range f.Decisions {
			if d == "extend_vocabulary" {
				t.Errorf("%s offers extend_vocabulary", f.Code)
			}
		}
		if (f.LocalID == nil) != (f.Code == absent) {
			t.Errorf("%s: local_id %v", f.Code, f.LocalID)
		} else if f.LocalID != nil && *f.LocalID != e.ids[f.Code] {
			t.Errorf("%s: local_id %s, want %s", f.Code, *f.LocalID, e.ids[f.Code])
		}
		switch f.Code {
		case kind:
			if !slices.ContainsFunc(f.Differences, func(d struct {
				Property          string          `json:"property"`
				Local             json.RawMessage `json:"local"`
				Incoming          json.RawMessage `json:"incoming"`
				TermsOnlyIncoming []string        `json:"terms_only_incoming"`
				TermsOnlyLocal    []string        `json:"terms_only_local"`
				TermsChanged      []string        `json:"terms_changed"`
			}) bool {
				return d.Property == "type" && string(d.Local) == `"text"` && string(d.Incoming) == `"select"`
			}) {
				t.Errorf("type difference missing: %+v", f.Differences)
			}
		case vocab:
			if len(f.Differences) != 1 || f.Differences[0].Property != "options" ||
				!slices.Equal(f.Differences[0].TermsOnlyIncoming, []string{"review"}) ||
				!slices.Equal(f.Differences[0].TermsOnlyLocal, []string{"legacy"}) ||
				!slices.Equal(f.Differences[0].TermsChanged, []string{"wip"}) {
				t.Errorf("options difference: %+v", f.Differences)
			}
		}
	}
	if len(p.UnresolvedTerms) != 1 || p.UnresolvedTerms[0].Term != e.code("nowhere")+"~x" {
		t.Errorf("unresolved terms %+v", p.UnresolvedTerms)
	}
	if !p.ExpiresAt.Equal(p.CreatedAt.Add(15 * time.Minute)) {
		t.Errorf("expires_at %v created_at %v", p.ExpiresAt, p.CreatedAt)
	}

	// Stored row: canonical document and digest, bound state for every
	// incoming code and direct reference, absences kept.
	var doc []byte
	var digest, state string
	var allowed, bound []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT document, document_digest, state, allowed, bound_state FROM field_schema_import_preview WHERE id = $1`,
		p.PreviewID).Scan(&doc, &digest, &state, &allowed, &bound); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(doc)
	if state != "live" || digest != p.DocumentDigest || digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("stored digest %s state %s", digest, state)
	}
	var b struct {
		Present map[string]string `json:"present"`
		Absent  []string          `json:"absent"`
	}
	_ = json.Unmarshal(bound, &b)
	if !slices.Equal(b.Absent, []string{absent, e.code("nowhere")}) {
		t.Errorf("absent %v", b.Absent)
	}
	for _, c := range []string{same, kind, vocab, gone, mirror, gonemirror} {
		var now string
		_ = e.pool.QueryRow(e.ctx, `SELECT encode(sha256(convert_to(to_jsonb(f)::text,'UTF8')),'hex') FROM field_definition f WHERE code=$1`, c).Scan(&now)
		if b.Present[c] != now {
			t.Errorf("%s: bound digest %q, current %q", c, b.Present[c], now)
		}
	}
}

// More than 200 distinct referenced codes are all bound, none dropped.
func TestImportPreview_ManyReferences(t *testing.T) {
	e := newImportEnv(t)
	local := e.code("ctl")
	e.field(local, "text", nil)
	var fields []string
	refs := map[string]bool{local: true}
	for f := 0; f < 5; f++ {
		terms := []string{fmt.Sprintf("%q", local+"=x")}
		for i := 0; i < 60; i++ {
			c := fmt.Sprintf("%s_m%d_%d", e.prefix, f, i)
			refs[c] = true
			terms = append(terms, fmt.Sprintf("%q", c+"=x"))
		}
		fields = append(fields, portable(fmt.Sprintf("%s_f%d", e.prefix, f), "text", "F", "", "["+strings.Join(terms, ",")+"]"))
	}
	p := decodePreview(t, e.postPreview(e.admin, previewDoc(fields...)))
	if len(p.UnresolvedTerms) != 300 {
		t.Fatalf("unresolved %d, want 300", len(p.UnresolvedTerms))
	}
	var bound []byte
	_ = e.pool.QueryRow(e.ctx, `SELECT bound_state FROM field_schema_import_preview WHERE id=$1`, p.PreviewID).Scan(&bound)
	var b struct {
		Present map[string]string `json:"present"`
		Absent  []string          `json:"absent"`
	}
	_ = json.Unmarshal(bound, &b)
	if len(b.Present) != 1 || len(b.Absent) != 305 || b.Present[local] == "" {
		t.Fatalf("bound present %d absent %d", len(b.Present), len(b.Absent))
	}
}

// The largest export previews; one byte over the request cap is 413.
func TestImportPreview_LargestExport(t *testing.T) {
	e := newImportEnv(t)
	code := e.code("edge")
	e.field(code, "text", map[string]any{"description": ""})
	empty := e.export(e.admin, code)
	fill := 2<<20 - len(empty.body)
	if _, err := e.pool.Exec(e.ctx, `UPDATE field_definition SET description = repeat('a', $2) WHERE code = $1`, code, fill); err != nil {
		t.Fatal(err)
	}
	exp := e.export(e.admin, code)
	if exp.status != 200 || len(exp.body) != 2<<20 {
		t.Fatalf("export %d len %d", exp.status, len(exp.body))
	}
	p := decodePreview(t, e.postPreview(e.admin, envelope(exp.body)))
	sum := sha256.Sum256(exp.body)
	if p.DocumentDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal("digest of the largest export differs from the export body hash")
	}
	if len(p.Fields) != 1 || p.Fields[0].Local != "identical" {
		t.Fatalf("largest export: %+v", p.Fields)
	}
	// Pad the envelope to exactly the cap: accepted (then refused only if
	// the document is too large); one byte more is 413.
	pad := func(n int) []byte {
		b := append([]byte(`{"document":`), bytes.TrimSuffix(exp.body, []byte("\n"))...)
		b = append(b, []byte(`,"source":{"label":"x"}`)...)
		b = append(b, '}')
		return append(b, bytes.Repeat([]byte(" "), n-len(b))...)
	}
	_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
	if r := e.postPreview(e.admin, pad(metadata.FieldSchemaImportPreviewMaxBodyBytes)); r.status != 200 {
		t.Fatalf("at cap: %d %.300s", r.status, r.body)
	}
	if r := e.postPreview(e.admin, pad(metadata.FieldSchemaImportPreviewMaxBodyBytes+1)); r.status != 413 {
		t.Fatalf("cap + 1: %d", r.status)
	}
	// A whitespace-free document over 2 MiB once re-encoded: 422.
	if _, err := e.pool.Exec(e.ctx, `UPDATE field_definition SET description = repeat('a', $2) WHERE code = $1`, code, fill+1); err != nil {
		t.Fatal(err)
	}
	over := []byte(strings.Replace(string(envelope(exp.body)), `"description":"`, `"description":"a`, 1))
	if r := e.postPreview(e.admin, over); r.status != 422 || !strings.Contains(string(r.body), "document_too_large") {
		t.Fatalf("over 2 MiB: %d %.200s", r.status, r.body)
	}
}

// namedAdmin is another fields.admin principal with a unique name. The
// shared principal helper names users by capability, so it can make
// only one per capability.
func (e *importEnv) namedAdmin(tag string) string {
	e.t.Helper()
	var ref int64
	name := e.prefix + "-" + tag
	if err := e.pool.QueryRow(e.ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Import Test', 1) RETURNING ref`, name).Scan(&ref); err != nil {
		e.t.Fatalf("seed user: %v", err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO user_roles (user_ref, role_id) SELECT $1, id FROM roles WHERE name = $2`,
		ref, e.prefix+"-fields-admin"); err != nil {
		e.t.Fatalf("assign role: %v", err)
	}
	token, _, err := auth.NewSessionManager(e.pool).Issue(e.ctx, ref, httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		e.t.Fatalf("issue session: %v", err)
	}
	return token
}

func countPayloads(t *testing.T, e *importEnv) int {
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM field_schema_import_preview WHERE document IS NOT NULL`).Scan(&n)
	return n
}

// waitForAdmissionWaiter reports whether some backend came to wait on
// the admission lock within ten seconds.
func waitForAdmissionWaiter(e *importEnv) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid=1576 AND objid=1`).Scan(&n)
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// runPausedPair starts preview A (paused while holding the admission
// lock) and preview B (queued behind it), releases A, and returns both
// statuses. B must count A's committed row.
func runPausedPair(t *testing.T, e *importEnv, tokA, tokB string, body []byte) (int, int) {
	t.Helper()
	held := make(chan struct{})
	release := make(chan struct{})
	// Always let the paused preview finish, even when an assertion below
	// fails, so a failure never hangs the test.
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	var once sync.Once
	metadata.SetImportPreviewHookForTest(func(stage string) {
		if stage != "locked" {
			return
		}
		first := false
		once.Do(func() { first = true })
		if first {
			close(held)
			<-release
		}
	})
	t.Cleanup(func() { metadata.SetImportPreviewHookForTest(nil) })
	var a, b exportResp
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a = e.postPreview(tokA, body) }()
	<-held
	go func() { defer wg.Done(); b = e.postPreview(tokB, body) }()
	waiting := waitForAdmissionWaiter(e)
	releaseOnce()
	wg.Wait()
	if !waiting {
		t.Fatal("no waiter on the admission lock")
	}
	return a.status, b.status
}

func TestImportPreview_AdmissionCaps(t *testing.T) {
	e := newImportEnv(t)
	body := previewDoc(portable(e.code("a"), "text", "A", "", ""))
	// Sequential actor cap.
	for i := 0; i < 3; i++ {
		if r := e.postPreview(e.admin, body); r.status != 200 {
			t.Fatalf("preview %d: %d", i, r.status)
		}
	}
	if r := e.postPreview(e.admin, body); r.status != 429 || !strings.Contains(string(r.body), "preview_limit") {
		t.Fatalf("4th: %d %s", r.status, r.body)
	}
	// Concurrent actor cap 3 -> 4: one more admin with 2 held.
	_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
	actor := e.namedAdmin("actor")
	for i := 0; i < 2; i++ {
		e.postPreview(actor, body)
	}
	a, b := runPausedPair(t, e, actor, actor, body)
	if a != 200 || b != 429 || countPayloads(t, e) != 3 {
		t.Fatalf("actor 3->4: a %d b %d payloads %d", a, b, countPayloads(t, e))
	}
	// Concurrent global cap 20 -> 21.
	_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
	var actors []string
	for len(actors) < 8 {
		actors = append(actors, e.namedAdmin(fmt.Sprintf("g%d", len(actors))))
	}
	n := 0
	for _, act := range actors[:6] {
		for i := 0; i < 3 && n < 19; i++ {
			if r := e.postPreview(act, body); r.status != 200 {
				t.Fatalf("fill %d: %d", n, r.status)
			}
			n++
		}
	}
	for n < 19 {
		e.postPreview(actors[6], body)
		n++
	}
	a, b = runPausedPair(t, e, actors[6], actors[7], body)
	if a != 200 || b != 429 || countPayloads(t, e) != 20 {
		t.Fatalf("global 20->21: a %d b %d payloads %d", a, b, countPayloads(t, e))
	}
}

func TestImportPreview_ExpiryAndCleanup(t *testing.T) {
	e := newImportEnv(t)
	body := []byte(`{"document":{"fields":[` + portable(e.code("a"), "text", "A", "", "") +
		`],"format":"aa-field-schema/1"},"source":{"label":"Studio B","uri":"https://studio-b.example/f"}}`)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, decodePreview(t, e.postPreview(e.admin, body)).PreviewID)
	}
	// Expired but not yet cleaned: the payloads still count.
	if _, err := e.pool.Exec(e.ctx, `UPDATE field_schema_import_preview SET created_at = now() - interval '1 hour', expires_at = now() - interval '30 minutes'`); err != nil {
		t.Fatal(err)
	}
	if r := e.postPreview(e.admin, body); r.status != 429 {
		t.Fatalf("expired payloads must still count while cleanup is late: %d", r.status)
	}
	sweeper := metadata.NewSchemaImportPreviewSweeper(e.srv.api.metadata, time.Hour, 2)
	// A row another transaction holds is skipped, not waited on.
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `SELECT 1 FROM field_schema_import_preview WHERE id = $1 FOR UPDATE`, ids[0]); err != nil {
		t.Fatal(err)
	}
	compacted, _, err := sweeper.SweepOnce(e.ctx)
	if err != nil || compacted != 2 {
		t.Fatalf("sweep with one row held: %d %v", compacted, err)
	}
	_ = tx.Rollback(e.ctx)
	if compacted, _, err := sweeper.SweepOnce(e.ctx); err != nil || compacted != 1 {
		t.Fatalf("second sweep: %d %v", compacted, err)
	}
	var state, digest string
	var doc, allowed, bound []byte
	var label, uri *string
	var tomb time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT state, document, allowed, bound_state, source_label, source_uri, document_digest, tombstone_until
		FROM field_schema_import_preview WHERE id=$1`, ids[0]).
		Scan(&state, &doc, &allowed, &bound, &label, &uri, &digest, &tomb); err != nil {
		t.Fatal(err)
	}
	// The tombstone keeps id, actor, times and the document digest, and
	// erases the payload and the source claims.
	if state != "expired" || doc != nil || allowed != nil || bound != nil || label != nil || uri != nil ||
		!strings.HasPrefix(digest, "sha256:") || tomb.IsZero() {
		t.Fatalf("tombstone %s doc=%v label=%v uri=%v digest=%q", state, doc != nil, label, uri, digest)
	}
	if r := e.postPreview(e.admin, body); r.status != 200 {
		t.Fatalf("after cleanup a slot is free: %d", r.status)
	}
	// Tombstones past retention are deleted.
	_, _ = e.pool.Exec(e.ctx, `UPDATE field_schema_import_preview SET tombstone_until = now() - interval '1 second' WHERE state = 'expired'`)
	if _, deleted, err := sweeper.SweepOnce(e.ctx); err != nil || deleted != 3 {
		t.Fatalf("tombstone delete: %d %v", deleted, err)
	}
	// The recurring loop sweeps on its ticker.
	_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	go metadata.NewSchemaImportPreviewSweeper(e.srv.api.metadata, 50*time.Millisecond, 500).Run(ctx)
	id := decodePreview(t, e.postPreview(e.admin, body)).PreviewID
	_, _ = e.pool.Exec(e.ctx, `UPDATE field_schema_import_preview SET created_at = now() - interval '1 hour', expires_at = now() - interval '1 minute' WHERE id=$1`, id)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = e.pool.QueryRow(e.ctx, `SELECT state FROM field_schema_import_preview WHERE id=$1`, id).Scan(&state)
		if state == "expired" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the recurring sweep did not compact the expired preview")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Shown local values and their bound digest come from one snapshot,
// even while the row is being rewritten. Every committed version of the
// row is recorded (the initial one included) and the writer is stopped
// and joined BEFORE checking, so every observation must name a recorded
// version and carry exactly that version's digest.
func TestImportPreview_CoherentSnapshot(t *testing.T) {
	e := newImportEnv(t)
	code := e.code("hot")
	e.field(code, "text", nil)
	versions := map[string]string{} // label -> digest of the committed row
	var initialLabel, initialDigest string
	if err := e.pool.QueryRow(e.ctx, `SELECT label, encode(sha256(convert_to(to_jsonb(f)::text,'UTF8')),'hex')
		FROM field_definition f WHERE code=$1`, code).Scan(&initialLabel, &initialDigest); err != nil {
		t.Fatal(err)
	}
	versions[initialLabel] = initialDigest

	type version struct{ label, digest string }
	committed := make(chan version, 100_000)
	writerErr := make(chan error, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			label := fmt.Sprintf("Label %d", i)
			var d string
			if err := e.pool.QueryRow(e.ctx, `UPDATE field_definition f SET label = $2 WHERE code = $1
				RETURNING encode(sha256(convert_to(to_jsonb(f)::text,'UTF8')),'hex')`, code, label).Scan(&d); err != nil {
				writerErr <- err
				return
			}
			committed <- version{label, d}
		}
	}()
	var stopOnce sync.Once
	stopWriter := func() { stopOnce.Do(func() { close(stop); <-done }) }
	defer stopWriter() // a failed assertion below never leaks the writer

	type observation struct{ label, digest string }
	var observed []observation
	body := previewDoc(portable(code, "text", "Incoming label", "", ""))
	for i := 0; i < 15; i++ {
		if _, err := e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`); err != nil {
			t.Fatal(err)
		}
		p := decodePreview(t, e.postPreview(e.admin, body))
		if len(p.Fields) != 1 || p.Fields[0].Local != "different" {
			t.Fatalf("observation %d: %+v", i, p.Fields)
		}
		shown := ""
		for _, d := range p.Fields[0].Differences {
			if d.Property == "label" {
				if err := json.Unmarshal(d.Local, &shown); err != nil {
					t.Fatalf("observation %d: label: %v", i, err)
				}
			}
		}
		if shown == "" {
			t.Fatalf("observation %d: no label difference shown", i)
		}
		var bound []byte
		if err := e.pool.QueryRow(e.ctx, `SELECT bound_state FROM field_schema_import_preview WHERE id=$1`, p.PreviewID).Scan(&bound); err != nil {
			t.Fatal(err)
		}
		var b struct {
			Present map[string]string `json:"present"`
		}
		if err := json.Unmarshal(bound, &b); err != nil {
			t.Fatal(err)
		}
		observed = append(observed, observation{shown, b.Present[code]})
	}
	stopWriter()
	select {
	case err := <-writerErr:
		t.Fatalf("writer: %v", err)
	default:
	}
	close(committed)
	writes := 0
	for v := range committed {
		versions[v.label] = v.digest
		writes++
	}
	if writes == 0 {
		t.Fatal("the writer committed nothing during the observations; the schedule was not concurrent")
	}
	if len(observed) != 15 {
		t.Fatalf("%d observations, want 15", len(observed))
	}
	distinct := map[string]bool{}
	for i, o := range observed {
		want, ok := versions[o.label]
		if !ok {
			t.Fatalf("observation %d shows label %q, which no committed version had", i, o.label)
		}
		if want != o.digest {
			t.Fatalf("observation %d shows label %q with the digest of a different row version", i, o.label)
		}
		distinct[o.label] = true
	}
	t.Logf("compared %d observations over %d concurrent writes (%d distinct versions seen)", len(observed), writes, len(distinct))
}

// rev699 C1: a source label holding U+0000 is a typed 400, not a 500,
// and nothing is written.
func TestImportPreview_SourceLabelNUL(t *testing.T) {
	e := newImportEnv(t)
	before := e.snapshot()
	body := []byte(`{"document":{"fields":[` + portable(e.code("a"), "text", "A", "", "") +
		`],"format":"aa-field-schema/1"},"source":{"label":"Studio\u0000B"}}`)
	r := e.postPreview(e.admin, body)
	var ref struct {
		Reason   string `json:"reason"`
		Problems []struct{ Path, Problem string }
	}
	_ = json.Unmarshal(r.body, &ref)
	if r.status != 400 || ref.Reason != "request_invalid" || len(ref.Problems) == 0 || ref.Problems[0].Path != "$.source.label" {
		t.Fatalf("source label NUL: %d %s", r.status, r.body)
	}
	if n := countPayloads(t, e); n != 0 {
		t.Fatalf("%d preview rows written", n)
	}
	if e.snapshot() != before {
		t.Fatal("definitions or permissions changed")
	}
}
