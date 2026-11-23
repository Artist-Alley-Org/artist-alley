// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// GET /api/v1/fields/import/receipts and /receipts/{receipt_id}
// (#1576 S4) through the production router and a real database:
// authorization before the query or path is read, the strict query
// and cursor, keyset paging that neither repeats nor skips across an
// insert, cross-actor reading that grants nothing on apply, honest
// zero-change and historical detail, and no write of any kind.

package http

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

func (e *importEnv) historyURL() string {
	return strings.Replace(e.preview, "/preview", "/receipts", 1)
}

func (e *importEnv) getHistory(token, suffix string) exportResp {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.historyURL()+suffix, nil)
	if err != nil {
		e.t.Fatalf("new request %q: %v", suffix, err)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("GET history: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read history body: %v", err)
	}
	return exportResp{resp.StatusCode, b}
}

type historySummary struct {
	ReceiptID       string    `json:"receipt_id"`
	AppliedAt       time.Time `json:"applied_at"`
	ActorRef        *int64    `json:"actor_ref"`
	Format          string    `json:"format"`
	DocumentDigest  string    `json:"document_digest"`
	DecisionsDigest string    `json:"decisions_digest"`
	SourceLabel     *string   `json:"source_label"`
	SourceURI       *string   `json:"source_uri"`
	Changed         int       `json:"changed"`
	FieldCount      int       `json:"field_count"`
}

type historyPage struct {
	Items      []historySummary `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

type historyRecord struct {
	historySummary
	PreviewID string `json:"preview_id"`
	Fields    []struct {
		Code                string  `json:"code"`
		Decision            string  `json:"decision"`
		Outcome             string  `json:"outcome"`
		FieldID             *string `json:"field_id"`
		IncomingFieldDigest string  `json:"incoming_field_digest"`
	} `json:"fields"`
}

func (e *importEnv) page(token, suffix string) historyPage {
	e.t.Helper()
	r := e.getHistory(token, suffix)
	if r.status != 200 {
		e.t.Fatalf("list %q: %d %s", suffix, r.status, r.body)
	}
	var p historyPage
	if err := json.Unmarshal(r.body, &p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *importEnv) record(token, id string) historyRecord {
	e.t.Helper()
	r := e.getHistory(token, "/"+id)
	if r.status != 200 {
		e.t.Fatalf("detail %s: %d %s", id, r.status, r.body)
	}
	var rec historyRecord
	if err := json.Unmarshal(r.body, &rec); err != nil {
		e.t.Fatal(err)
	}
	return rec
}

// writeState digests every row of every table a history read could
// plausibly touch: whole rows, in a stable order, so any insert, update or
// delete shows. role_capabilities and user_roles are included because a
// read must not change who may do what.
func (e *importEnv) writeState() string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT concat_ws('|',
		(SELECT coalesce(md5(string_agg(to_jsonb(a)::text, ',' ORDER BY a.id)), '') FROM audit_events a),
		(SELECT coalesce(md5(string_agg(to_jsonb(r)::text, ',' ORDER BY r.id)), '') FROM field_schema_import_receipt r),
		(SELECT coalesce(md5(string_agg(to_jsonb(i)::text, ',' ORDER BY i.receipt_id, i.ordinal)), '') FROM field_schema_import_receipt_item i),
		(SELECT coalesce(md5(string_agg(to_jsonb(p)::text, ',' ORDER BY p.id)), '') FROM field_schema_import_preview p),
		(SELECT coalesce(md5(string_agg(to_jsonb(f)::text, ',' ORDER BY f.id)), '') FROM field_definition f),
		(SELECT coalesce(md5(string_agg(to_jsonb(c)::text, ',' ORDER BY c.role_id, c.capability_code)), '') FROM role_capabilities c),
		(SELECT coalesce(md5(string_agg(to_jsonb(u)::text, ',' ORDER BY u.user_ref, u.role_id)), '') FROM user_roles u))`).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func cursorOf(raw string) string { return "v1." + base64.RawURLEncoding.EncodeToString([]byte(raw)) }

func TestImportHistory_AuthorizationBeforeInput(t *testing.T) {
	e := newImportEnv(t)
	listSuffixes := []string{"", "?limit=25", "?limit=0", "?limit=abc", "?cursor=nope", "?limit=1&limit=2", "?bogus=1",
		"?cursor=" + strings.Repeat("A", 4000)}
	detailSuffixes := []string{"/" + uuid.NewString(), "/NOT-A-UUID", "/" + strings.ToUpper(uuid.NewString()), "/" + uuid.NewString() + "?x=1"}
	serve := func(token, suffix string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/fields/import/receipts"+suffix, nil)
		if token != "" {
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
		}
		w := httptest.NewRecorder()
		e.srv.srv.Handler.ServeHTTP(w, req)
		return w.Code
	}
	for _, s := range append(append([]string{}, listSuffixes...), detailSuffixes...) {
		if c := serve("", s); c != 401 {
			t.Errorf("anonymous %q: %d, want 401", s, c)
		}
		if c := serve(e.member, s); c != 403 {
			t.Errorf("member %q: %d, want 403", s, c)
		}
	}
	// Authorized: the raw routes answer, never the generated 500 stub.
	wantAdmin := map[string]int{"": 200, "?limit=25": 200, "?limit=0": 400, "?limit=abc": 400, "?cursor=nope": 400,
		"?limit=1&limit=2": 400, "?bogus=1": 400, "?cursor=" + strings.Repeat("A", 4000): 400,
		detailSuffixes[0]: 404, detailSuffixes[1]: 400, detailSuffixes[2]: 400, detailSuffixes[3]: 400}
	for s, want := range wantAdmin {
		for _, tok := range []string{e.admin, e.sysAdmin} {
			if c := serve(tok, s); c != want {
				t.Errorf("admin %q: %d, want %d", s, c, want)
			}
		}
	}
}

func TestImportHistory_MalformedQueries(t *testing.T) {
	e := newImportEnv(t)
	ts := "2999-01-01T00:00:00.123456Z"
	id := uuid.NewString()
	bad := []string{
		"?limit=", "?limit=-1", "?limit=01", "?limit=101", "?limit=1000", "?limit=1.5", "?limit=+5", "?limit=%201",
		"?cursor=", "?cursor=v2." + strings.TrimPrefix(cursorOf(ts+"|"+id), "v1."), "?cursor=v1.", "?cursor=v1.%21%21",
		"?cursor=" + cursorOf(ts+"|"+id) + "%3D", // padded
		"?cursor=" + cursorOf(ts+"|"+strings.ToUpper(id)),
		"?cursor=" + cursorOf(ts+"|"+id+"x"),
		"?cursor=" + cursorOf(ts),
		"?cursor=" + cursorOf("2999-01-01T00:00:00.123450Z|"+id), // trailing zero: not what the server writes
		"?cursor=" + cursorOf("2999-01-01T00:00:00+00:00|"+id),
		"?cursor=" + cursorOf("2999-13-01T00:00:00Z|"+id),
		"?cursor=" + cursorOf("garbage|"+id),
		"?cursor=" + cursorOf(ts+"|"+id+"|"+id),
		"?cursor=" + cursorOf(ts+"|"+id) + "&cursor=" + cursorOf(ts+"|"+id),
		"?cursor=v1." + strings.Repeat("A", 600),
		"?Limit=5", "?limit=5&x=",
	}
	for _, s := range bad {
		r := e.getHistory(e.admin, s)
		if r.status != 400 || !json.Valid(r.body) {
			t.Errorf("%q: %d %s, want 400", s, r.status, r.body)
		}
	}
	// A well-formed cursor positioned before every receipt is an empty
	// page, not a restart at the first.
	p := e.page(e.admin, "?cursor="+cursorOf("1970-01-01T00:00:00Z|"+id))
	if len(p.Items) != 0 || p.NextCursor != nil {
		t.Fatalf("cursor before everything: %+v", p)
	}
	for _, s := range []string{"?limit=1", "?limit=100", "?limit=99&cursor=" + cursorOf(ts+"|"+id)} {
		if r := e.getHistory(e.admin, s); r.status != 200 {
			t.Errorf("%q: %d %s", s, r.status, r.body)
		}
	}
}

// seedReceipt inserts a bare receipt at a chosen instant; the table has
// no other writer than apply, so paging is tested on exact keys.
func (e *importEnv) seedReceipt(at string, id string) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO field_schema_import_receipt
		(id, preview_id, actor_ref, applied_at, format, document_digest, decisions_digest, decisions, changed)
		VALUES ($1, gen_random_uuid(), NULL, $2::timestamptz, 'aa-field-schema/1', 'sha256:'||repeat('a',64), 'sha256:'||repeat('b',64), '{}', 0)`,
		id, at); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		if _, err := e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_receipt WHERE id=$1`, id); err != nil {
			e.t.Errorf("cleanup receipt %s: %v", id, err)
		}
	})
}

func TestImportHistory_KeysetPaging(t *testing.T) {
	e := newImportEnv(t)
	// Far-future instants sort ahead of every real receipt. Four share
	// one instant, so the id breaks the tie.
	tie := "2999-06-01T00:00:00.654321Z"
	var seeded []string
	for range 4 {
		id := uuid.NewString()
		e.seedReceipt(tie, id)
		seeded = append(seeded, id)
	}
	for _, at := range []string{"2999-06-02T00:00:00Z", "2999-05-31T23:59:59.999999Z"} {
		id := uuid.NewString()
		e.seedReceipt(at, id)
		seeded = append(seeded, id)
	}
	var want []string
	rows, err := e.pool.Query(e.ctx, `SELECT id::text FROM field_schema_import_receipt WHERE id = ANY($1::uuid[]) ORDER BY applied_at DESC, id DESC`, seeded)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		want = append(want, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	first := e.page(e.admin, "?limit=2")
	if len(first.Items) != 2 || first.NextCursor == nil || len(*first.NextCursor) > 512 {
		t.Fatalf("first page: %+v", first)
	}
	got := []string{first.Items[0].ReceiptID, first.Items[1].ReceiptID}
	// The cursor sits inside the tie. Between pages, insert a newer
	// receipt and a same-instant receipt that sorts ahead of the cursor:
	// neither may appear later. One that sorts after it must.
	cursorID := first.Items[1].ReceiptID
	e.seedReceipt("3000-01-01T00:00:00Z", uuid.NewString())
	ahead, behind := "ffffffff-ffff-4fff-bfff-ffffffffffff", "00000000-0000-4000-8000-000000000000"
	if first.Items[1].AppliedAt.Format(time.RFC3339Nano) != tie {
		t.Fatalf("second item is not in the tie: %s", first.Items[1].AppliedAt.Format(time.RFC3339Nano))
	}
	e.seedReceipt(tie, ahead)
	e.seedReceipt(tie, behind)
	cur := *first.NextCursor
	for len(got) < len(want)+1 {
		p := e.page(e.admin, "?limit=2&cursor="+cur)
		for _, it := range p.Items {
			got = append(got, it.ReceiptID)
		}
		if p.NextCursor == nil {
			break
		}
		cur = *p.NextCursor
	}
	// behind sorts last within the tie, ahead of the 05-31 receipt.
	var expect []string
	for _, id := range want {
		expect = append(expect, id)
		if id == want[4] { // the last tie member
			expect = append(expect, behind)
		}
	}
	if strings.Join(got[:len(expect)], ",") != strings.Join(expect, ",") {
		t.Fatalf("paging order (cursor %s):\n got %v\nwant %v", cursorID, got[:len(expect)], expect)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("receipt %s repeated", id)
		}
		seen[id] = true
	}
	if seen[ahead] {
		t.Fatal("a receipt ahead of the cursor appeared on a later page")
	}
}

func TestImportHistory_WalkToEnd(t *testing.T) {
	e := newImportEnv(t)
	for range 3 {
		e.seedReceipt(time.Now().UTC().Format(time.RFC3339Nano), uuid.NewString())
	}
	var total int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM field_schema_import_receipt`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	n, pages, cur := 0, 0, ""
	for {
		suffix := "?limit=1"
		if cur != "" {
			suffix += "&cursor=" + cur
		}
		p := e.page(e.admin, suffix)
		n += len(p.Items)
		pages++
		if p.NextCursor == nil {
			if len(p.Items) != 1 && total > 0 {
				t.Fatalf("final page has %d items", len(p.Items))
			}
			break
		}
		if pages > total+1 {
			t.Fatal("paging never ended")
		}
		cur = *p.NextCursor
	}
	if n != total {
		t.Fatalf("walked %d receipts, table has %d", n, total)
	}
	// The default page is 25.
	if p := e.page(e.admin, ""); len(p.Items) != min(total, 25) {
		t.Fatalf("default page %d of %d", len(p.Items), total)
	}
}

func TestImportHistory_CrossActorDetailAndNoWrites(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	other := e.namedAdmin("reader")
	created, kept, skipped := e.code("created"), e.code("kept"), e.code("skipped")
	e.field(kept, "text", nil)
	envelopeWithSource := fmt.Sprintf(`{"document":{"fields":[%s,%s,%s],"format":"aa-field-schema/1"},"source":{"label":"Partner DAM","uri":"https://example.invalid/schema"}}`,
		portable(created, "text", "C", "", ""), portable(kept, "select", "K", `[]`, ""), portable(skipped, "text", "S", "", ""))
	p := decodePreview(t, e.postPreview(e.admin, []byte(envelopeWithSource)))
	rc := decodeReceipt(t, e.apply(e.admin, p, map[string]string{created: "create", kept: "keep_local", skipped: "skip"}))
	zero := e.previewFields(e.admin, portable(e.code("zero"), "text", "Z", "", ""))
	rz := decodeReceipt(t, e.apply(e.admin, zero, map[string]string{e.code("zero"): "skip"}))
	// A preview the first admin has not applied.
	pending := e.previewFields(e.admin, portable(e.code("pending"), "text", "P", "", ""))

	before := e.writeState()
	// The other admin and a system admin both read the first admin's receipts.
	for _, tok := range []string{other, e.sysAdmin} {
		rec := e.record(tok, rc.ReceiptID)
		if rec.PreviewID != p.PreviewID || rec.Changed != 1 || len(rec.Fields) != 3 ||
			rec.DocumentDigest != p.DocumentDigest || rec.SourceLabel == nil || *rec.SourceLabel != "Partner DAM" ||
			rec.SourceURI == nil || *rec.SourceURI != "https://example.invalid/schema" || rec.ActorRef == nil || rec.Format != "aa-field-schema/1" {
			t.Fatalf("detail: %+v", rec)
		}
		for i, f := range rec.Fields {
			if f.Code != rc.Fields[i].Code || f.Decision != rc.Fields[i].Decision || f.Outcome != rc.Fields[i].Outcome ||
				!strings.HasPrefix(f.IncomingFieldDigest, "sha256:") {
				t.Fatalf("item %d: %+v vs %+v", i, f, rc.Fields[i])
			}
			if (f.FieldID == nil) != (rc.Fields[i].FieldID == nil) || f.FieldID != nil && *f.FieldID != *rc.Fields[i].FieldID {
				t.Fatalf("item %d field id", i)
			}
		}
		rzRec := e.record(tok, rz.ReceiptID)
		if rzRec.Changed != 0 || len(rzRec.Fields) != 1 || rzRec.Fields[0].Outcome != "skipped" || rzRec.SourceLabel != nil {
			t.Fatalf("zero-change detail: %+v", rzRec)
		}
		var found int
		for _, it := range e.page(tok, "?limit=100").Items {
			if it.ReceiptID == rc.ReceiptID {
				found++
				if it.FieldCount != 3 || it.Changed != 1 || it.DecisionsDigest == "" {
					t.Fatalf("summary: %+v", it)
				}
			}
			if it.ReceiptID == rz.ReceiptID {
				found++
				if it.FieldCount != 1 || it.Changed != 0 {
					t.Fatalf("zero summary: %+v", it)
				}
			}
		}
		if found != 2 {
			t.Fatalf("list shows %d of the two receipts", found)
		}
	}
	// No field of the detail response claims a replay.
	if r := e.getHistory(other, "/"+rc.ReceiptID); strings.Contains(string(r.body), "replayed") {
		t.Fatalf("history carries a replay flag: %s", r.body)
	}
	if after := e.writeState(); after != before {
		t.Fatalf("a history read wrote:\n%s\n%s", before, after)
	}
	// Reading grants nothing on apply: the other admin's apply of the
	// first admin's pending preview is still not found, and leaves it.
	if r := e.apply(other, pending, map[string]string{e.code("pending"): "create"}); r.status != 404 || refusalReason(t, r) != "preview_not_found" {
		t.Fatalf("foreign apply: %d %s", r.status, r.body)
	}
	var still int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM field_schema_import_preview WHERE id=$1 AND document IS NOT NULL`, pending.PreviewID).Scan(&still); err != nil {
		t.Fatal(err)
	}
	if still != 1 {
		t.Fatal("the foreign apply consumed the preview")
	}

	// History survives its subjects: the actor goes, the created field
	// goes, the detail still names the historical field id.
	var createdID string
	for _, f := range rc.Fields {
		if f.Code == created {
			createdID = *f.FieldID
		}
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE field_schema_import_receipt SET actor_ref = NULL WHERE id=$1`, rc.ReceiptID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM field_definition WHERE id=$1`, createdID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_receipt WHERE id=$1`, rc.ReceiptID); err != nil {
			t.Errorf("cleanup receipt: %v", err)
		}
	})
	rec := e.record(other, rc.ReceiptID)
	if rec.ActorRef != nil {
		t.Fatalf("actor_ref %v, want null", *rec.ActorRef)
	}
	for _, f := range rec.Fields {
		if f.Code == created && (f.FieldID == nil || *f.FieldID != createdID) {
			t.Fatalf("historical field id lost: %+v", f)
		}
	}
	if r := e.getHistory(other, "/"+uuid.NewString()); r.status != 404 {
		t.Fatalf("unknown receipt: %d", r.status)
	}
}

// rev705 C4: the raw query is parsed with error reporting, so a request
// whose query does not parse is a 400, never a default first page; and
// the cursor must be byte-for-byte the canonical encoding of its
// position (no newline, no padding, no alternative trailing bits).
func TestImportHistory_RawQueryStrictness(t *testing.T) {
	e := newImportEnv(t)
	ts := "2999-01-01T00:00:00.123456Z"
	id := uuid.NewString()
	good := cursorOf(ts + "|" + id)
	body := strings.TrimPrefix(good, "v1.")
	// The same bytes with different unused trailing bits.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, body[len(body)-1])
	alt := "v1." + body[:len(body)-1] + string(alphabet[last+1])
	if b1, _ := base64.RawURLEncoding.DecodeString(body); string(b1) != ts+"|"+id {
		t.Fatal("fixture: canonical cursor does not decode")
	}
	if b2, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(alt, "v1.")); err != nil || string(b2) != ts+"|"+id {
		t.Fatalf("fixture: alternative spelling must decode to the same bytes leniently (%v)", err)
	}
	bad := []string{
		"?cursor=%ZZ",
		"?limit=1;cursor=x",
		"?limit=5&%zz=1",
		"?limit=1;",
		"?cursor=" + good[:10] + "%0A" + good[10:],
		"?cursor=" + good[:10] + "%0D%0A" + good[10:],
		"?cursor=" + alt,
	}
	for _, s := range bad {
		if r := e.getHistory("", s); r.status != 401 {
			t.Errorf("anonymous %q: %d, want 401", s, r.status)
		}
		if r := e.getHistory(e.member, s); r.status != 403 {
			t.Errorf("member %q: %d, want 403", s, r.status)
		}
		if r := e.getHistory(e.admin, s); r.status != 400 || !json.Valid(r.body) {
			t.Errorf("admin %q: %d %s, want 400", s, r.status, r.body)
		}
	}
	if r := e.getHistory(e.admin, "?cursor="+good); r.status != 200 {
		t.Fatalf("canonical cursor: %d %s", r.status, r.body)
	}
}
