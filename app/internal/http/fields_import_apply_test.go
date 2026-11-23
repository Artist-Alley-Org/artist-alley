// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// POST /api/v1/fields/import/apply and the serialized ordinary
// POST /fields (#1576 S2), through the production router and a real
// database: authorization before the body, the four decisions, receipts
// and replay, freshness, atomic rollback, the final-graph rules, and
// the create/import races. Strict envelope cases are unit-tested in
// app/internal/metadata/schema_import_apply_test.go.

package http

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

func (e *importEnv) applyURL() string { return strings.Replace(e.preview, "/preview", "/apply", 1) }

func (e *importEnv) postApply(token string, body []byte) exportResp {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.applyURL(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("POST apply: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return exportResp{resp.StatusCode, b}
}

func (e *importEnv) apply(token string, p previewOut, decisions map[string]string) exportResp {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"preview_id": p.PreviewID, "document_digest": p.DocumentDigest, "decisions": decisions})
	return e.postApply(token, b)
}

func (e *importEnv) previewFields(token string, fields ...string) previewOut {
	e.t.Helper()
	return decodePreview(e.t, e.postPreview(token, previewDoc(fields...)))
}

func (e *importEnv) postField(token string, body string) exportResp {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, strings.Replace(e.preview, "/fields/import/preview", "/fields", 1), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return exportResp{resp.StatusCode, b}
}

type receiptOut struct {
	ReceiptID string `json:"receipt_id"`
	PreviewID string `json:"preview_id"`
	Replayed  bool   `json:"replayed"`
	Changed   int    `json:"changed"`
	Fields    []struct {
		Code, Decision, Outcome string
		FieldID                 *string `json:"field_id"`
	} `json:"fields"`
}

func decodeReceipt(t *testing.T, r exportResp) receiptOut {
	t.Helper()
	if r.status != 200 {
		t.Fatalf("apply status %d: %.500s", r.status, r.body)
	}
	var rc receiptOut
	if err := json.Unmarshal(r.body, &rc); err != nil {
		t.Fatal(err)
	}
	return rc
}

func refusalReason(t *testing.T, r exportResp) string {
	t.Helper()
	var ref struct{ Reason string }
	_ = json.Unmarshal(r.body, &ref)
	return ref.Reason
}

func (e *importEnv) countFields() int {
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM field_definition WHERE code LIKE $1`, e.prefix+"%").Scan(&n)
	return n
}

func (e *importEnv) countReceipts() int {
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM field_schema_import_receipt WHERE actor_ref IN (SELECT ref FROM "user" WHERE username LIKE $1)`, e.prefix+"%").Scan(&n)
	return n
}

func (e *importEnv) rowJSON(code string) string {
	var s string
	_ = e.pool.QueryRow(e.ctx, `SELECT to_jsonb(f)::text FROM field_definition f WHERE code=$1`, code).Scan(&s)
	return s
}

func clearReceipts(e *importEnv) {
	_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_receipt WHERE actor_ref IN (SELECT ref FROM "user" WHERE username LIKE $1)`, e.prefix+"%")
}

// richField sets every portable member to a non-default value.
func richField(code, cond string) string {
	return fmt.Sprintf(`{"code":%q,"description":"Imported description","display_condition":%s,"display_group":"technical","display_order":7,`+
		`"edit_tab":"Technical","label":"Rich label","open_vocabulary":true,"options":[{"children":[{"children":[],"label":null,"value":"uk"}],"label":"Europe","value":"europe"},{"children":[],"label":null,"value":"asia"}],`+
		`"regexp_filter":null,"required":true,"searchable":false,"show_in_advanced_search":false,"show_on_card":true,"show_on_upload":false,"subject_kind":"asset","type":"tree"}`,
		code, cond)
}

func TestImportApply_AuthorizationBeforeBody(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	serve := func(token string, body []byte) (int, int64) {
		rr := &readRecorder{r: bytes.NewReader(body)}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/fields/import/apply", nil)
		req.Body, req.ContentLength = rr, -1
		if token != "" {
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
		}
		w := httptest.NewRecorder()
		e.srv.srv.Handler.ServeHTTP(w, req)
		return w.Code, rr.reads.Load()
	}
	bodies := [][]byte{[]byte(`{"preview_id":"00000000-0000-0000-0000-000000000000"}`), []byte(`{`),
		bytes.Repeat([]byte("x"), metadata.FieldSchemaImportApplyMaxBodyBytes+5)}
	for _, b := range bodies {
		if code, reads := serve("", b); code != 401 || reads != 0 {
			t.Fatalf("anonymous: %d reads %d", code, reads)
		}
		if code, reads := serve(e.member, b); code != 403 || reads != 0 {
			t.Fatalf("member: %d reads %d", code, reads)
		}
	}
	// The raw route answers (the generated method would 500).
	r := e.postApply(e.admin, []byte(`{"preview_id":"6f6c9a7e-5d1f-4c55-9a43-6f0a7d1c9b10","document_digest":"sha256:`+strings.Repeat("0", 64)+`","decisions":{}}`))
	if r.status != 404 || refusalReason(t, r) != "preview_not_found" {
		t.Fatalf("unknown preview: %d %s", r.status, r.body)
	}
	if code, _ := serve(e.admin, bodies[2]); code != 413 {
		t.Fatalf("oversize: %d", code)
	}
}

func TestImportApply_DecisionsOutcomesAndPreservation(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	same, kind, mirror, gone, absent := e.code("same"), e.code("kind"), e.code("mirror"), e.code("gone"), e.code("absent")
	e.field(same, "select", map[string]any{"options": `{"values":[{"value":"a","label":"A","aliases":["alpha"]}]}`})
	e.field(kind, "text", nil)
	e.field(mirror, "text", map[string]any{"mirrors_column": "title", "read_only": false})
	e.field(gone, "text", map[string]any{"status": "archived"})
	exp := e.export(e.admin, same)
	var exported struct {
		Fields []json.RawMessage `json:"fields"`
	}
	_ = json.Unmarshal(exp.body, &exported)
	before := map[string]string{same: e.rowJSON(same), kind: e.rowJSON(kind), mirror: e.rowJSON(mirror), gone: e.rowJSON(gone)}
	var capsBefore string
	_ = e.pool.QueryRow(e.ctx, `SELECT coalesce(string_agg(role_id::text||capability_code, ',' ORDER BY 1),'') FROM role_capabilities`).Scan(&capsBefore)

	incomingAbsent := richField(absent, fmt.Sprintf(`["%s=a","%s~x"]`, same, e.code("nowhere")))
	fields := []string{incomingAbsent, portable(gone, "text", "Gone", "", ""), portable(kind, "select", "Label "+kind, `[]`, ""),
		portable(mirror, "text", "Label "+mirror, "", ""), string(exported.Fields[0])}
	p := e.previewFields(e.admin, fields...)
	rc := decodeReceipt(t, e.apply(e.admin, p, map[string]string{absent: "create", gone: "skip", kind: "keep_local", mirror: "skip", same: "use_existing"}))
	if rc.Replayed || rc.Changed != 1 || len(rc.Fields) != 5 {
		t.Fatalf("receipt %+v", rc)
	}
	want := map[string]string{absent: "created", gone: "skipped", kind: "kept_local", mirror: "skipped", same: "matched"}
	prev := ""
	for _, f := range rc.Fields {
		if f.Code <= prev {
			t.Fatalf("receipt not sorted: %+v", rc.Fields)
		}
		prev = f.Code
		if f.Outcome != want[f.Code] {
			t.Errorf("%s outcome %s, want %s", f.Code, f.Outcome, want[f.Code])
		}
		if f.FieldID == nil && f.Code != absent {
			t.Errorf("%s: no field id", f.Code)
		}
	}
	for c, b := range before {
		if e.rowJSON(c) != b {
			t.Errorf("%s changed", c)
		}
	}
	var capsAfter string
	_ = e.pool.QueryRow(e.ctx, `SELECT coalesce(string_agg(role_id::text||capability_code, ',' ORDER BY 1),'') FROM role_capabilities`).Scan(&capsAfter)
	if capsAfter != capsBefore {
		t.Error("permissions changed")
	}
	// The created row: every portable member survives a real export, and
	// every local setting is the receiver's default.
	re := e.export(e.admin, absent)
	var reexp struct {
		Fields []json.RawMessage `json:"fields"`
	}
	_ = json.Unmarshal(re.body, &reexp)
	var a, b any
	_ = json.Unmarshal(reexp.Fields[0], &a)
	_ = json.Unmarshal([]byte(incomingAbsent), &b)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatalf("re-export differs:\n%s\n%s", ab, bb)
	}
	var appliesTo, extractionSource, extractionMode, status, options string
	var readCap, writeCap, mirrors *string
	var readOnly bool
	var def []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT applies_to::text, read_capability, write_capability, read_only, extraction_source, extraction_mode,
		default_value, status, mirrors_column, options::text FROM field_definition WHERE code=$1`, absent).
		Scan(&appliesTo, &readCap, &writeCap, &readOnly, &extractionSource, &extractionMode, &def, &status, &mirrors, &options); err != nil {
		t.Fatal(err)
	}
	if appliesTo != "{}" || readCap != nil || writeCap != nil || readOnly || extractionSource != "" || extractionMode != "skip_if_set" ||
		def != nil || status != "active" || mirrors != nil || strings.Contains(options, "alias") || strings.Contains(options, "status") {
		t.Fatalf("local defaults: %s %v %v %v %q %s %s %s %v %s", appliesTo, readCap, writeCap, readOnly, extractionSource, extractionMode, def, status, mirrors, options)
	}
	// Provenance, the preview gone, the audit event.
	var items, previews, events int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM field_schema_import_receipt_item WHERE receipt_id=$1`, rc.ReceiptID).Scan(&items)
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM field_schema_import_preview WHERE id=$1`, p.PreviewID).Scan(&previews)
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_events WHERE event_type='metadata.field_schema.imported' AND metadata->>'receipt_id'=$1`, rc.ReceiptID).Scan(&events)
	if items != 5 || previews != 0 || events != 1 {
		t.Fatalf("items %d previews %d events %d", items, previews, events)
	}
	// An all-skip apply is honest: nothing changed, nothing adopted.
	other := e.code("other")
	p2 := e.previewFields(e.admin, portable(other, "text", "O", "", ""))
	rc2 := decodeReceipt(t, e.apply(e.admin, p2, map[string]string{other: "skip"}))
	if rc2.Changed != 0 || rc2.Fields[0].Outcome != "skipped" || e.rowJSON(other) != "" {
		t.Fatalf("all-skip: %+v", rc2)
	}
}

func TestImportApply_LargeDecisionMapAndCap(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	var fields []string
	decisions := map[string]string{}
	for i := 0; i < 100; i++ {
		code := fmt.Sprintf("%s_%03d_%s", e.prefix, i, strings.Repeat("c", 700))
		fields = append(fields, portable(code, "text", "L", "", ""))
		decisions[code] = "skip"
	}
	p := e.previewFields(e.admin, fields...)
	body, _ := json.Marshal(map[string]any{"preview_id": p.PreviewID, "document_digest": p.DocumentDigest, "decisions": decisions})
	if len(body) <= 64<<10 {
		t.Fatalf("decision body %d bytes is not over 64 KiB", len(body))
	}
	rc := decodeReceipt(t, e.postApply(e.admin, body))
	if rc.Changed != 0 || len(rc.Fields) != 100 {
		t.Fatalf("receipt %+v", rc.Changed)
	}
	padded := append(append([]byte{}, body...), bytes.Repeat([]byte(" "), metadata.FieldSchemaImportApplyMaxBodyBytes+1-len(body))...)
	if r := e.postApply(e.admin, padded); r.status != 413 {
		t.Fatalf("cap+1: %d", r.status)
	}
}

func TestImportApply_FreshnessAndRefusals(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	local, ref, a := e.code("local"), e.code("ref"), e.code("a")
	e.field(local, "text", nil)
	body := []string{portable(a, "text", "A", "", fmt.Sprintf(`["%s=x"]`, ref)), portable(local, "text", "Label "+local, "", "")}
	dec := map[string]string{a: "create", local: "use_existing"}
	noWrites := func(name string) {
		t.Helper()
		if e.rowJSON(a) != "" || e.countReceipts() != 0 {
			t.Fatalf("%s wrote something", name)
		}
	}
	other := e.namedAdmin("other")
	p := e.previewFields(e.admin, body...)
	if r := e.apply(other, p, dec); r.status != 404 || refusalReason(t, r) != "preview_not_found" {
		t.Fatalf("foreign actor: %d %s", r.status, r.body)
	}
	tampered := p
	tampered.DocumentDigest = "sha256:" + strings.Repeat("a", 64)
	if r := e.apply(e.admin, tampered, dec); r.status != 409 || refusalReason(t, r) != "digest_mismatch" {
		t.Fatalf("digest tamper: %d %s", r.status, r.body)
	}
	for name, d := range map[string]map[string]string{
		"missing key": {a: "create"}, "extra key": {a: "create", local: "use_existing", e.code("zz"): "skip"},
		"disallowed": {a: "use_existing", local: "use_existing"}, "unknown choice": {a: "adopt", local: "use_existing"},
	} {
		if r := e.apply(e.admin, p, d); r.status != 422 || refusalReason(t, r) != "decision_invalid" {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	noWrites("refusals")
	// Stale: the bound local row changes.
	if _, err := e.pool.Exec(e.ctx, `UPDATE field_definition SET label='Edited' WHERE code=$1`, local); err != nil {
		t.Fatal(err)
	}
	if r := e.apply(e.admin, p, dec); r.status != 409 || refusalReason(t, r) != "preview_stale" || !strings.Contains(string(r.body), local) {
		t.Fatalf("stale edit: %d %s", r.status, r.body)
	}
	noWrites("stale edit")
	// Stale: an absent direct reference appears.
	_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
	p = e.previewFields(e.admin, portable(a, "text", "A", "", fmt.Sprintf(`["%s=x"]`, ref)))
	e.field(ref, "text", map[string]any{"read_only": false})
	if r := e.apply(e.admin, p, map[string]string{a: "create"}); r.status != 409 || !strings.Contains(string(r.body), ref) {
		t.Fatalf("appearance: %d %s", r.status, r.body)
	}
	noWrites("appearance")
	// Expired, tombstone, purged.
	p = e.previewFields(e.admin, portable(a, "text", "A", "", ""))
	_, _ = e.pool.Exec(e.ctx, `UPDATE field_schema_import_preview SET created_at = now()-interval '1 hour', expires_at = now()-interval '1 minute' WHERE id=$1`, p.PreviewID)
	if r := e.apply(e.admin, p, map[string]string{a: "create"}); r.status != 410 {
		t.Fatalf("expired unswept: %d %s", r.status, r.body)
	}
	if _, _, err := metadata.NewSchemaImportPreviewSweeper(e.srv.api.metadata, time.Hour, 500).SweepOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	if r := e.apply(e.admin, p, map[string]string{a: "create"}); r.status != 410 {
		t.Fatalf("tombstone: %d %s", r.status, r.body)
	}
	_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview WHERE id=$1`, p.PreviewID)
	if r := e.apply(e.admin, p, map[string]string{a: "create"}); r.status != 404 {
		t.Fatalf("purged: %d %s", r.status, r.body)
	}
	noWrites("expiry")
}

func TestImportApply_ReplayAndConcurrency(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	a, local := e.code("a"), e.code("local")
	e.field(local, "text", nil)
	p := e.previewFields(e.admin, portable(a, "text", "A", "", ""), portable(local, "text", "Label "+local, "", ""))
	dec := map[string]string{a: "create", local: "use_existing"}
	first := decodeReceipt(t, e.apply(e.admin, p, dec))
	// Local drift and the preview gone: an identical repeat still replays.
	_, _ = e.pool.Exec(e.ctx, `UPDATE field_definition SET label='Drift' WHERE code=$1`, local)
	again := decodeReceipt(t, e.apply(e.admin, p, dec))
	if !again.Replayed || again.ReceiptID != first.ReceiptID || again.Changed != first.Changed || e.countFields() != 2 {
		t.Fatalf("replay %+v vs %+v, fields %d", again, first, e.countFields())
	}
	if r := e.apply(e.admin, p, map[string]string{a: "skip", local: "use_existing"}); r.status != 409 || refusalReason(t, r) != "preview_consumed" {
		t.Fatalf("other decisions: %d %s", r.status, r.body)
	}
	tampered := p
	tampered.DocumentDigest = "sha256:" + strings.Repeat("b", 64)
	if r := e.apply(e.admin, tampered, dec); r.status != 409 || refusalReason(t, r) != "digest_mismatch" {
		t.Fatalf("replay digest: %d %s", r.status, r.body)
	}
	if r := e.apply(e.namedAdmin("x"), p, dec); r.status != 404 {
		t.Fatalf("replay other actor: %d", r.status)
	}
	var events int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_events WHERE event_type='metadata.field_schema.imported' AND metadata->>'receipt_id'=$1`, first.ReceiptID).Scan(&events)
	if events != 1 {
		t.Fatalf("%d audit events, want 1 (replays record none)", events)
	}

	// Two concurrent applies of one preview: one import, one replay.
	b := e.code("b")
	p2 := e.previewFields(e.admin, portable(b, "text", "B", "", ""))
	held, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	var once sync.Once
	metadata.SetImportApplyHookForTest(func(stage string) error {
		if stage == "locked" {
			first := false
			once.Do(func() { first = true })
			if first {
				close(held)
				<-release
			}
		}
		return nil
	})
	t.Cleanup(func() { metadata.SetImportApplyHookForTest(nil) })
	var r1, r2 exportResp
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r1 = e.apply(e.admin, p2, map[string]string{b: "create"}) }()
	<-held
	go func() { defer wg.Done(); r2 = e.apply(e.admin, p2, map[string]string{b: "create"}) }()
	waiting := waitForLock(e, 1173)
	releaseOnce()
	wg.Wait()
	if !waiting {
		t.Fatal("the second apply did not wait on the graph lock")
	}
	c1, c2 := decodeReceipt(t, r1), decodeReceipt(t, r2)
	if c1.ReceiptID != c2.ReceiptID || c1.Replayed == c2.Replayed || c1.Changed != 1 || c2.Changed != 1 {
		t.Fatalf("concurrent: %+v %+v", c1, c2)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM field_definition WHERE code=$1`, b).Scan(&n)
	if n != 1 {
		t.Fatalf("%d rows for %s", n, b)
	}
}

// waitForLock reports whether a backend waits on an advisory lock with
// the given first key within ten seconds.
func waitForLock(e *importEnv, classid int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid=$1`, classid).Scan(&n)
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestImportApply_AtomicRollback(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	a, b := e.code("a"), e.code("b")
	p := e.previewFields(e.admin, portable(a, "text", "A", "", ""), portable(b, "text", "B", "", ""))
	// A later write fails after the first insert.
	metadata.SetImportApplyHookForTest(func(stage string) error {
		if stage == "inserted" {
			return fmt.Errorf("injected failure")
		}
		return nil
	})
	r := e.apply(e.admin, p, map[string]string{a: "create", b: "create"})
	metadata.SetImportApplyHookForTest(nil)
	var state string
	_ = e.pool.QueryRow(e.ctx, `SELECT state FROM field_schema_import_preview WHERE id=$1`, p.PreviewID).Scan(&state)
	if r.status != 500 || e.countFields() != 0 || e.countReceipts() != 0 || state != "live" {
		t.Fatalf("injected failure: %d fields %d receipts %d preview %q", r.status, e.countFields(), e.countReceipts(), state)
	}
	// The same preview then applies cleanly.
	if rc := decodeReceipt(t, e.apply(e.admin, p, map[string]string{a: "create", b: "create"})); rc.Changed != 2 {
		t.Fatalf("retry: %+v", rc)
	}
	// An unstorable code (too long to index) is a typed refusal, rolled back.
	// Hash-derived hex does not compress, so the index tuple really
	// exceeds the btree row limit (a run of one letter would compress).
	var sb strings.Builder
	seed := []byte(e.prefix)
	for sb.Len() < 6000 {
		sum := sha256.Sum256(seed)
		sb.WriteString(hex.EncodeToString(sum[:]))
		seed = sum[:]
	}
	long := e.prefix + "_l" + sb.String()
	p2 := e.previewFields(e.admin, portable(e.code("c"), "text", "C", "", ""), portable(long, "text", "L", "", ""))
	r = e.apply(e.admin, p2, map[string]string{long: "create", e.code("c"): "create"})
	_ = e.pool.QueryRow(e.ctx, `SELECT state FROM field_schema_import_preview WHERE id=$1`, p2.PreviewID).Scan(&state)
	if r.status != 422 || refusalReason(t, r) != "unstorable_value" || e.rowJSON(e.code("c")) != "" || state != "live" {
		t.Fatalf("unstorable: %d %s preview %q", r.status, r.body, state)
	}
}

func TestImportApply_FinalGraph(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	refuse := func(name string, fields []string, dec map[string]string) {
		t.Helper()
		_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
		before := e.countFields()
		r := e.apply(e.admin, e.previewFields(e.admin, fields...), dec)
		if r.status != 422 || refusalReason(t, r) != "graph_refused" || e.countFields() != before {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	x, y := e.code("x"), e.code("y")
	// A two-field cycle, both created; and the same with the roles swapped.
	refuse("cycle", []string{portable(x, "text", "X", "", fmt.Sprintf(`["%s=v"]`, y)), portable(y, "text", "Y", "", fmt.Sprintf(`["%s=v"]`, x))},
		map[string]string{x: "create", y: "create"})
	// A cycle through an existing node whose stored term was unresolved.
	ex, nn := e.code("ex"), e.code("nn")
	e.field(ex, "text", map[string]any{"read_only": false, "display_condition": fmt.Sprintf(`["%s=v"]`, nn)})
	refuse("cycle through existing", []string{portable(nn, "text", "N", "", fmt.Sprintf(`["%s=v"]`, ex))}, map[string]string{nn: "create"})
	// Resolving a stored dependent to a field of the other subject kind.
	dep, ctl := e.code("dep"), e.code("ctl")
	e.field(dep, "text", map[string]any{"read_only": false, "display_condition": fmt.Sprintf(`["%s=v"]`, ctl)})
	coll := strings.Replace(portable(ctl, "text", "C", "", ""), `"subject_kind":"asset"`, `"subject_kind":"collection"`, 1)
	refuse("wrong subject resolution", []string{coll}, map[string]string{ctl: "create"})
	// Archived and mirrored controllers.
	arch, mir := e.code("arch"), e.code("mir")
	e.field(arch, "text", map[string]any{"status": "archived"})
	e.field(mir, "text", map[string]any{"mirrors_column": "title", "read_only": false})
	refuse("archived controller", []string{portable(e.code("p1"), "text", "P", "", fmt.Sprintf(`["%s=v"]`, arch))}, map[string]string{e.code("p1"): "create"})
	refuse("mirrored controller", []string{portable(e.code("p2"), "text", "P", "", fmt.Sprintf(`["%s=v"]`, mir))}, map[string]string{e.code("p2"): "create"})
	// Operator/type: ~ on a boolean controller.
	bo := e.code("bo")
	e.field(bo, "boolean", map[string]any{"read_only": false})
	refuse("operator", []string{portable(e.code("p3"), "text", "P", "", fmt.Sprintf(`["%s~v"]`, bo))}, map[string]string{e.code("p3"): "create"})
	// Unresolved terms are kept verbatim, and a valid resolution later
	// by import is accepted.
	u, missing := e.code("u"), e.code("missing")
	_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
	cond := fmt.Sprintf(`["%s=v","%s~w"]`, missing, ctl)
	decodeReceipt(t, e.apply(e.admin, e.previewFields(e.admin, portable(u, "text", "U", "", cond)), map[string]string{u: "create"}))
	var stored string
	_ = e.pool.QueryRow(e.ctx, `SELECT display_condition::text FROM field_definition WHERE code=$1`, u).Scan(&stored)
	if stored != fmt.Sprintf(`["%s=v", "%s~w"]`, missing, ctl) {
		t.Fatalf("stored condition %s", stored)
	}
	_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
	decodeReceipt(t, e.apply(e.admin, e.previewFields(e.admin, portable(missing, "text", "M", "", "")), map[string]string{missing: "create"}))
}

func TestCreateField_ResolvesImportedConditions(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	a, b := e.code("a"), e.code("b")
	decodeReceipt(t, e.apply(e.admin, e.previewFields(e.admin, portable(a, "text", "A", "", fmt.Sprintf(`["%s=v"]`, b))), map[string]string{a: "create"}))
	r := e.postField(e.admin, fmt.Sprintf(`{"code":%q,"label":"B","type":"text","subject_kind":"collection"}`, b))
	if r.status != 409 || !strings.Contains(string(r.body), "display_condition_conflict") || !strings.Contains(string(r.body), a) || e.rowJSON(b) != "" {
		t.Fatalf("wrong-subject create: %d %s", r.status, r.body)
	}
	if r := e.postField(e.admin, fmt.Sprintf(`{"code":%q,"label":"B","type":"text"}`, b)); r.status != 201 {
		t.Fatalf("valid create: %d %s", r.status, r.body)
	}
	// Ordinary create of an unrelated code is unaffected.
	if r := e.postField(e.admin, fmt.Sprintf(`{"code":%q,"label":"C","type":"text"}`, e.code("c"))); r.status != 201 {
		t.Fatalf("unrelated create: %d", r.status)
	}
}

// The race in both orders. (1) CreateField(B, wrong subject) pauses
// before the locks; an import commits A naming missing B; the create
// then takes the locks, sees A and is refused. (2) CreateField holds the
// locks, paused after its check; the import of A waits on the graph lock
// and, once B exists, is stale.
func TestCreateField_ImportRace(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	run := func(stage string, a, b string) (exportResp, exportResp) {
		held, release := make(chan struct{}), make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		defer releaseOnce()
		var once sync.Once
		metadata.SetCreateFieldHookForTest(func(s string) {
			if s != stage {
				return
			}
			first := false
			once.Do(func() { first = true })
			if first {
				close(held)
				<-release
			}
		})
		defer metadata.SetCreateFieldHookForTest(nil)
		p := e.previewFields(e.admin, portable(a, "text", "A", "", fmt.Sprintf(`["%s=v"]`, b)))
		var create, imp exportResp
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			create = e.postField(e.admin, fmt.Sprintf(`{"code":%q,"label":"B","type":"text","subject_kind":"collection"}`, b))
		}()
		<-held
		if stage == "before_lock" {
			imp = e.apply(e.admin, p, map[string]string{a: "create"})
			releaseOnce()
		} else {
			wg.Add(1)
			go func() { defer wg.Done(); imp = e.apply(e.admin, p, map[string]string{a: "create"}) }()
			if !waitForLock(e, 1173) {
				releaseOnce()
				wg.Wait()
				t.Fatal("the import did not wait on the graph lock held by the create")
			}
			releaseOnce()
		}
		wg.Wait()
		return create, imp
	}
	a1, b1 := e.code("a1"), e.code("b1")
	create, imp := run("before_lock", a1, b1)
	if imp.status != 200 || create.status != 409 || e.rowJSON(b1) != "" {
		t.Fatalf("before_lock: import %d create %d %s", imp.status, create.status, create.body)
	}
	a2, b2 := e.code("a2"), e.code("b2")
	create, imp = run("after_check", a2, b2)
	if create.status != 201 || imp.status != 409 || e.rowJSON(a2) != "" {
		t.Fatalf("after_check: create %d import %d %s", create.status, imp.status, imp.body)
	}
}
