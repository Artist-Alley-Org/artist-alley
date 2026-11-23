// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// rev701 corrections for #1576 S2: expiry rechecked after blocking work,
// receipt items that impose no code-length limit, an audit event for
// every committed receipt, the read-set row locks against row-only
// writers, and resolved-edge rules beside unresolved terms.

package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

// holdRow opens a transaction holding one row lock and reports its
// backend pid. release ends it (commit when commit is true).
func holdRow(t *testing.T, e *importEnv, query string, args ...any) (pid int, release func(commit bool)) {
	t.Helper()
	tx, err := e.pool.BeginTx(e.ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, query, args...); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func(commit bool) {
		once.Do(func() {
			if commit {
				_ = tx.Commit(context.Background())
			} else {
				_ = tx.Rollback(context.Background())
			}
		})
	}
	t.Cleanup(func() { release(false) })
	return pid, release
}

// blockedBy reports, within ten seconds, whether n backends wait on pid.
func blockedBy(e *importEnv, pid, n int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var c int
		_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&c)
		if c >= n {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// waitPastExpiry blocks until the database clock is past the preview's
// expiry, bounded by the expiry itself plus five seconds.
func waitPastExpiry(t *testing.T, e *importEnv, previewID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var past bool
		if err := e.pool.QueryRow(e.ctx, `SELECT clock_timestamp() > expires_at FROM field_schema_import_preview WHERE id=$1`, previewID).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the preview never expired")
}

func (e *importEnv) auditCount(receiptID string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_events WHERE event_type='metadata.field_schema.imported' AND metadata->>'receipt_id'=$1`, receiptID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// C1: a preview that expires while the apply waits on a row lock must
// not apply. The wait is confirmed BEFORE expiry, and the deadline
// passes DURING it.
func TestImportApply_ExpiresDuringRowWait(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	ctl, a := e.code("ctl"), e.code("a")
	e.field(ctl, "text", map[string]any{"read_only": false})
	p := e.previewFields(e.admin, portable(a, "text", "A", "", fmt.Sprintf(`["%s=v"]`, ctl)))
	if _, err := e.pool.Exec(e.ctx, `UPDATE field_schema_import_preview SET created_at = clock_timestamp() - interval '1 minute',
		expires_at = clock_timestamp() + interval '1500 milliseconds' WHERE id=$1`, p.PreviewID); err != nil {
		t.Fatal(err)
	}
	pid, release := holdRow(t, e, `SELECT 1 FROM field_definition WHERE code=$1 FOR UPDATE`, ctl)
	var r exportResp
	done := make(chan struct{})
	go func() { defer close(done); r = e.apply(e.admin, p, map[string]string{a: "create"}) }()
	if !blockedBy(e, pid, 1) {
		release(false)
		<-done
		t.Fatal("the apply never waited on the held row")
	}
	var notYet bool
	_ = e.pool.QueryRow(e.ctx, `SELECT clock_timestamp() < expires_at FROM field_schema_import_preview WHERE id=$1`, p.PreviewID).Scan(&notYet)
	if !notYet {
		release(false)
		<-done
		t.Fatal("inconclusive: the preview had expired before the wait was confirmed")
	}
	waitPastExpiry(t, e, p.PreviewID)
	release(true)
	<-done
	if r.status != 410 || refusalReason(t, r) != "preview_expired" || e.rowJSON(a) != "" || e.countReceipts() != 0 {
		t.Fatalf("expired during the wait: %d %s, row %q receipts %d", r.status, r.body, e.rowJSON(a), e.countReceipts())
	}
}

// C1: the same while waiting on the preview row itself.
func TestImportApply_ExpiresDuringPreviewWait(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	a := e.code("a")
	p := e.previewFields(e.admin, portable(a, "text", "A", "", ""))
	if _, err := e.pool.Exec(e.ctx, `UPDATE field_schema_import_preview SET created_at = clock_timestamp() - interval '1 minute',
		expires_at = clock_timestamp() + interval '1500 milliseconds' WHERE id=$1`, p.PreviewID); err != nil {
		t.Fatal(err)
	}
	pid, release := holdRow(t, e, `SELECT 1 FROM field_schema_import_preview WHERE id=$1 FOR UPDATE`, p.PreviewID)
	var r exportResp
	done := make(chan struct{})
	go func() { defer close(done); r = e.apply(e.admin, p, map[string]string{a: "create"}) }()
	if !blockedBy(e, pid, 1) {
		release(false)
		<-done
		t.Fatal("the apply never waited on the preview row")
	}
	waitPastExpiry(t, e, p.PreviewID)
	release(true)
	<-done
	if r.status != 410 || e.rowJSON(a) != "" || e.countReceipts() != 0 {
		t.Fatalf("expired during the preview wait: %d %s", r.status, r.body)
	}
}

// C1: 410 within 24 hours of expiry, 404 after it, whatever cleanup did.
func TestImportApply_ExpiryRetentionHorizon(t *testing.T) {
	e := newImportEnv(t)
	a := e.code("a")
	p := e.previewFields(e.admin, portable(a, "text", "A", "", ""))
	// Not swept, expired 25 hours ago: beyond the horizon.
	if _, err := e.pool.Exec(e.ctx, `UPDATE field_schema_import_preview SET created_at = now() - interval '26 hours', expires_at = now() - interval '25 hours' WHERE id=$1`, p.PreviewID); err != nil {
		t.Fatal(err)
	}
	if r := e.apply(e.admin, p, map[string]string{a: "create"}); r.status != 404 {
		t.Fatalf("unswept, beyond 24 h: %d %s", r.status, r.body)
	}
	// A tombstone kept past its horizon is 404 too.
	_, _ = e.pool.Exec(e.ctx, `UPDATE field_schema_import_preview SET state='expired', document=NULL, allowed=NULL, bound_state=NULL,
		source_label=NULL, source_uri=NULL, tombstone_until = expires_at + interval '24 hours' WHERE id=$1`, p.PreviewID)
	if r := e.apply(e.admin, p, map[string]string{a: "create"}); r.status != 404 {
		t.Fatalf("tombstone beyond 24 h: %d %s", r.status, r.body)
	}
	// Within 24 hours: 410.
	_, _ = e.pool.Exec(e.ctx, `UPDATE field_schema_import_preview SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour',
		tombstone_until = now() + interval '23 hours' WHERE id=$1`, p.PreviewID)
	if r := e.apply(e.admin, p, map[string]string{a: "create"}); r.status != 410 {
		t.Fatalf("tombstone within 24 h: %d %s", r.status, r.body)
	}
}

// incompressibleCode is a format-valid code too long to index.
func incompressibleCode(e *importEnv) string {
	var sb strings.Builder
	h := []byte(e.prefix)
	for sb.Len() < 6000 {
		sum := sha256Sum(h)
		sb.WriteString(sum)
		h = []byte(sum)
	}
	return e.prefix + "_z" + sb.String()
}

// C2: skipping a field whose code is too long to index must work; only
// creating it is unstorable.
func TestImportApply_LongCodeSkip(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	short, long := e.code("short"), incompressibleCode(e)
	p := e.previewFields(e.admin, portable(short, "text", "S", "", ""), portable(long, "text", "L", "", ""))
	dec := map[string]string{short: "create", long: "skip"}
	rc := decodeReceipt(t, e.apply(e.admin, p, dec))
	if rc.Changed != 1 || len(rc.Fields) != 2 || rc.Fields[0].Code != short || rc.Fields[0].Outcome != "created" ||
		rc.Fields[1].Code != long || rc.Fields[1].Outcome != "skipped" {
		t.Fatalf("receipt %+v", rc)
	}
	again := decodeReceipt(t, e.apply(e.admin, p, dec))
	if !again.Replayed || again.ReceiptID != rc.ReceiptID || len(again.Fields) != 2 || again.Fields[1].Code != long {
		t.Fatalf("replay %+v", again)
	}
	// Creating the long code is still a typed refusal with full rollback.
	other := e.code("other")
	p2 := e.previewFields(e.admin, portable(other, "text", "O", "", ""), portable(long, "text", "L", "", ""))
	var state string
	r := e.apply(e.admin, p2, map[string]string{other: "create", long: "create"})
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM field_schema_import_preview WHERE id=$1`, p2.PreviewID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if r.status != 422 || refusalReason(t, r) != "unstorable_value" || e.rowJSON(other) != "" || state != "live" {
		t.Fatalf("long create: %d %s preview %q", r.status, r.body, state)
	}
	var receipts int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM field_schema_import_receipt WHERE preview_id=$1`, p2.PreviewID).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("receipt written for a refused apply: %d %v", receipts, err)
	}
}

// C3: every newly committed receipt has one audit event, changed 0
// included; a replay adds none.
func TestImportApply_AuditEveryReceipt(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	same, kept, skipped := e.code("same"), e.code("kept"), e.code("skipped")
	e.field(same, "text", map[string]any{"read_only": false})
	e.field(kept, "text", map[string]any{"read_only": false})
	exp := e.export(e.admin, same)
	matchedOnly := e.previewFields(e.admin, string(exportedFields(t, exp)[0]))
	rc1 := decodeReceipt(t, e.apply(e.admin, matchedOnly, map[string]string{same: "use_existing"}))
	keepSkip := e.previewFields(e.admin, portable(kept, "select", "K", "[]", ""), portable(skipped, "text", "S", "", ""))
	rc2 := decodeReceipt(t, e.apply(e.admin, keepSkip, map[string]string{kept: "keep_local", skipped: "skip"}))
	if rc1.Changed != 0 || rc1.Fields[0].Outcome != "matched" || rc2.Changed != 0 ||
		rc2.Fields[0].Outcome != "kept_local" || rc2.Fields[1].Outcome != "skipped" {
		t.Fatalf("receipts %+v %+v", rc1, rc2)
	}
	for _, id := range []string{rc1.ReceiptID, rc2.ReceiptID} {
		if n := e.auditCount(id); n != 1 {
			t.Fatalf("receipt %s: %d audit events, want 1", id, n)
		}
	}
	decodeReceipt(t, e.apply(e.admin, matchedOnly, map[string]string{same: "use_existing"}))
	if n := e.auditCount(rc1.ReceiptID); n != 1 {
		t.Fatalf("a replay added an audit event: %d", n)
	}
}

func exportedFields(t *testing.T, r exportResp) [][]byte {
	t.Helper()
	if r.status != 200 {
		t.Fatalf("export %d %s", r.status, r.body)
	}
	var doc struct {
		Fields []jsonRaw `json:"fields"`
	}
	if err := jsonUnmarshal(r.body, &doc); err != nil {
		t.Fatal(err)
	}
	out := make([][]byte, len(doc.Fields))
	for i, f := range doc.Fields {
		out[i] = f
	}
	return out
}

func (e *importEnv) sendField(token, method, id, body string) exportResp {
	e.t.Helper()
	url := strings.Replace(e.preview, "/fields/import/preview", "/fields/"+id, 1)
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
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

// C4: while an apply holds its read set, an archive of a resolved
// controller and an applicability edit of an EXISTING dependent (not an
// incoming code) wait for its commit, and then apply as later drift.
func TestImportApply_ReadSetBlocksRowWriters(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	dep, ctl, miss := e.code("dep"), e.code("ctl"), e.code("miss")
	e.field(ctl, "select", map[string]any{"read_only": false, "options": `{"values":["x"]}`})
	e.field(dep, "text", map[string]any{"read_only": false, "applies_to": []int64{},
		"display_condition": fmt.Sprintf(`["%s=x","%s=v"]`, ctl, miss)})
	p := e.previewFields(e.admin, portable(miss, "text", "M", "", ""))
	held, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	var once sync.Once
	metadata.SetImportApplyHookForTest(func(stage string) error {
		if stage == "rows_locked" {
			once.Do(func() { close(held); <-release })
		}
		return nil
	})
	t.Cleanup(func() { metadata.SetImportApplyHookForTest(nil) })
	var ar exportResp
	applyDone := make(chan struct{})
	go func() { defer close(applyDone); ar = e.apply(e.admin, p, map[string]string{miss: "create"}) }()
	<-held
	var finished atomic.Int32
	var archive, patch exportResp
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		archive = e.sendField(e.admin, http.MethodDelete, e.ids[ctl], "")
		finished.Add(1)
	}()
	go func() {
		defer wg.Done()
		patch = e.sendField(e.admin, http.MethodPatch, e.ids[dep], `{"applies_to":[7]}`)
		finished.Add(1)
	}()
	var applyPID int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && applyPID == 0 {
		_ = e.pool.QueryRow(e.ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted AND classid=1173 LIMIT 1`).Scan(&applyPID)
		time.Sleep(10 * time.Millisecond)
	}
	if applyPID == 0 || !blockedBy(e, applyPID, 2) || finished.Load() != 0 {
		releaseOnce()
		wg.Wait()
		<-applyDone
		t.Fatalf("row writers did not wait on the apply (pid %d, finished %d)", applyPID, finished.Load())
	}
	releaseOnce()
	<-applyDone
	wg.Wait()
	if ar.status != 200 || archive.status != 204 && archive.status != 200 || patch.status != 200 {
		t.Fatalf("apply %d archive %d %s patch %d %s", ar.status, archive.status, archive.body, patch.status, patch.body)
	}
}

// C4: the same for the ordinary create resolving a stored dependent.
func TestCreateField_ReadSetBlocksRowWriters(t *testing.T) {
	e := newImportEnv(t)
	dep, ctl, miss := e.code("dep"), e.code("ctl"), e.code("miss")
	e.field(ctl, "select", map[string]any{"read_only": false, "options": `{"values":["x"]}`})
	e.field(dep, "text", map[string]any{"read_only": false, "display_condition": fmt.Sprintf(`["%s=x","%s=v"]`, ctl, miss)})
	held, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	var once sync.Once
	metadata.SetCreateFieldHookForTest(func(stage string) {
		if stage == "rows_locked" {
			once.Do(func() { close(held); <-release })
		}
	})
	t.Cleanup(func() { metadata.SetCreateFieldHookForTest(nil) })
	var cr exportResp
	createDone := make(chan struct{})
	go func() {
		defer close(createDone)
		cr = e.postField(e.admin, fmt.Sprintf(`{"code":%q,"label":"M","type":"text"}`, miss))
	}()
	<-held
	var finished atomic.Int32
	var archive exportResp
	go func() { archive = e.sendField(e.admin, http.MethodDelete, e.ids[ctl], ""); finished.Add(1) }()
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && pid == 0 {
		_ = e.pool.QueryRow(e.ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted AND classid=1173 LIMIT 1`).Scan(&pid)
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 || !blockedBy(e, pid, 1) || finished.Load() != 0 {
		releaseOnce()
		<-createDone
		t.Fatalf("archive did not wait on the create (pid %d)", pid)
	}
	releaseOnce()
	<-createDone
	for finished.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if cr.status != 201 || archive.status >= 300 {
		t.Fatalf("create %d %s archive %d %s", cr.status, cr.body, archive.status, archive.body)
	}
}

// C4: writer first. A bound row's uncommitted change blocks the apply;
// once it commits, the apply sees the fresh row and is stale.
func TestImportApply_WriterFirstIsStale(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	local, a := e.code("local"), e.code("a")
	e.field(local, "text", map[string]any{"read_only": false})
	p := e.previewFields(e.admin, portable(a, "text", "A", "", ""), portable(local, "text", "Label "+local, "", ""))
	pid, release := holdRow(t, e, `UPDATE field_definition SET label = 'Changed by the writer' WHERE code=$1`, local)
	var r exportResp
	done := make(chan struct{})
	go func() {
		defer close(done)
		r = e.apply(e.admin, p, map[string]string{a: "create", local: "use_existing"})
	}()
	if !blockedBy(e, pid, 1) {
		release(false)
		<-done
		t.Fatal("the apply did not wait on the writer")
	}
	release(true)
	<-done
	if r.status != 409 || refusalReason(t, r) != "preview_stale" || e.rowJSON(a) != "" || e.countReceipts() != 0 {
		t.Fatalf("writer first: %d %s", r.status, r.body)
	}
}

// C4: resolved edges keep every rule even beside an unresolved term.
func TestImportApply_ResolvedEdgesBesideMissing(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	k1, k2, k3 := e.code("k1"), e.code("k2"), e.code("k3")
	e.field(k1, "text", map[string]any{"read_only": false, "applies_to": []int64{1}})
	e.field(k2, "text", map[string]any{"read_only": false, "applies_to": []int64{2}})
	e.field(k3, "text", map[string]any{"read_only": false})
	miss := e.code("zmissing")
	refuse := func(name, cond string) {
		t.Helper()
		_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
		d := e.code("d" + name)
		r := e.apply(e.admin, e.previewFields(e.admin, portable(d, "text", "D", "", cond)), map[string]string{d: "create"})
		if r.status != 422 || refusalReason(t, r) != "graph_refused" || e.rowJSON(d) != "" {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	refuse("nway", fmt.Sprintf(`["%s=a","%s=b","%s=c"]`, k1, k2, miss))
	refuse("contradiction", fmt.Sprintf(`["%s=a","%s=b","%s=c"]`, k3, k3, miss))
}

// C4: cycles in a second ordering permutation (dependencies against the
// code order) and along it, both valid ascending documents.
func TestImportApply_CyclePermutations(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	c1, c2, c3 := e.code("c1"), e.code("c2"), e.code("c3")
	for name, conds := range map[string][3]string{
		"against order": {c3, c1, c2}, // c1 -> c3 -> c2 -> c1
		"along order":   {c2, c3, c1}, // c1 -> c2 -> c3 -> c1
	} {
		_, _ = e.pool.Exec(e.ctx, `DELETE FROM field_schema_import_preview`)
		p := e.previewFields(e.admin,
			portable(c1, "text", "1", "", fmt.Sprintf(`["%s=v"]`, conds[0])),
			portable(c2, "text", "2", "", fmt.Sprintf(`["%s=v"]`, conds[1])),
			portable(c3, "text", "3", "", fmt.Sprintf(`["%s=v"]`, conds[2])))
		r := e.apply(e.admin, p, map[string]string{c1: "create", c2: "create", c3: "create"})
		if r.status != 422 || refusalReason(t, r) != "graph_refused" || e.countFields() != 0 {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
}

// C4: a non-null regexp_filter survives create and re-export.
func TestImportApply_RegexpRoundTrip(t *testing.T) {
	e := newImportEnv(t)
	t.Cleanup(func() { clearReceipts(e) })
	code := e.code("shot")
	in := strings.Replace(portable(code, "text", "Shot", "", ""), `"regexp_filter":null`, `"regexp_filter":"^[A-Z]{2}[0-9]+$"`, 1)
	decodeReceipt(t, e.apply(e.admin, e.previewFields(e.admin, in), map[string]string{code: "create"}))
	got := exportedFields(t, e.export(e.admin, code))[0]
	var a, b any
	if err := jsonUnmarshal(got, &a); err != nil {
		t.Fatal(err)
	}
	if err := jsonUnmarshal([]byte(in), &b); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("regexp round trip:\n%s\n%s", got, in)
	}
}

type jsonRaw = json.RawMessage

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func sha256Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
