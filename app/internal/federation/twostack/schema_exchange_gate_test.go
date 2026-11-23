// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1576 S3: the two-instance field-schema adoption gate.
//
// Two current-code servers from the real production router (apphttp.New)
// on their own migrated databases (AA_DB_NAME_A, AA_DB_NAME_B), over TLS.
// The schema travels only by the operator's HTTP chain: A's export
// response bytes become B's preview document, and B's operator applies
// explicit decisions. SQL is used only for local-only fixture state, the
// preservation snapshots and assertions; it never stands in for an
// import step.
//
// Scope: current staged code, not a published binary. It does not
// exercise federation profiles or the activity stream, and proves
// nothing about #1580/#1582 or the release gate.

package twostack_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

type xresp struct {
	status int
	body   []byte
}

// call sends exact bytes with a chosen credential ("" = anonymous) and
// fails on any transport or read error.
func (c *cluster) call(in *instance, token, method, path string, body []byte) xresp {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, in.url+"/api/v1"+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: c.tr, Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		c.t.Fatalf("%s %s %s: %v", in.name, method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("%s %s %s: read: %v", in.name, method, path, err)
	}
	return xresp{resp.StatusCode, raw}
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %.300s: %v", b, err)
	}
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// principal makes an approved user on one instance with an API token,
// and optionally one directly granted capability.
func principal(t *testing.T, in *instance, prefix, tag, capability string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	var ref int64
	if err := in.pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Schema Gate', 1) RETURNING ref`,
		prefix+"-"+tag).Scan(&ref); err != nil {
		t.Fatalf("%s user: %v", in.name, err)
	}
	if capability != "" {
		if _, err := in.pool.Exec(ctx, `INSERT INTO user_capability_grants (user_ref, capability_code) VALUES ($1, $2)`, ref, capability); err != nil {
			t.Fatalf("%s grant: %v", in.name, err)
		}
	}
	tok, _ := auth.NewAPIToken()
	if _, err := in.pool.Exec(ctx, `INSERT INTO api_tokens (user_ref, name, token_hash) VALUES ($1, 'schema gate', $2)`, ref, auth.HashAPIToken(tok)); err != nil {
		t.Fatalf("%s token: %v", in.name, err)
	}
	return ref, tok
}

// localState is everything the fixture scope owns on one instance, by
// section: whole definition rows by code, default overrides, complete
// value rows, and the permission tables. Errors are fatal.
type localState struct {
	defs      map[string]string
	overrides string
	values    string
	perms     string
}

func snapshot(t *testing.T, in *instance, prefix string) localState {
	t.Helper()
	ctx := context.Background()
	q := func(sql string) string {
		var s string
		if err := in.pool.QueryRow(ctx, sql, prefix+"%").Scan(&s); err != nil {
			t.Fatalf("%s snapshot: %v (%s)", in.name, err, sql)
		}
		return s
	}
	st := localState{defs: map[string]string{}}
	rows, err := in.pool.Query(ctx, `SELECT code, to_jsonb(f)::text FROM field_definition f WHERE code LIKE $1`, prefix+"%")
	if err != nil {
		t.Fatalf("%s snapshot defs: %v", in.name, err)
	}
	for rows.Next() {
		var c, j string
		if err := rows.Scan(&c, &j); err != nil {
			t.Fatalf("%s snapshot defs: %v", in.name, err)
		}
		st.defs[c] = j
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s snapshot defs: %v", in.name, err)
	}
	rows.Close()
	st.overrides = q(`SELECT coalesce(string_agg(to_jsonb(o)::text, E'\n' ORDER BY o.field_id, o.team_id), '') FROM field_default_override o JOIN field_definition f ON f.id=o.field_id WHERE f.code LIKE $1`)
	st.values = q(`SELECT coalesce(string_agg(to_jsonb(v)::text, E'\n' ORDER BY v.asset_id, v.field_id), '') FROM asset_field_value v JOIN field_definition f ON f.id=v.field_id WHERE f.code LIKE $1`)
	st.perms = strings.Join([]string{
		q(`SELECT coalesce(string_agg(role_id::text||' '||capability_code, ',' ORDER BY 1), '') FROM role_capabilities WHERE $1 <> ''`),
		q(`SELECT coalesce(string_agg(g.user_ref::text||' '||g.capability_code, ',' ORDER BY 1), '') FROM user_capability_grants g WHERE $1 <> ''`),
		q(`SELECT coalesce(string_agg(user_ref::text||' '||role_id::text, ',' ORDER BY 1), '') FROM user_roles WHERE $1 <> ''`),
	}, "|")
	return st
}

// sameExcept fails unless got equals want in every section, with the
// definitions allowed to differ only as listed: codes in added must be
// new in got, codes in changed may differ, everything else identical.
func sameExcept(t *testing.T, in *instance, where string, want, got localState, added, changed []string) {
	t.Helper()
	if got.values != want.values {
		t.Fatalf("%s: %s values changed:\nbefore %s\nafter  %s", where, in.name, want.values, got.values)
	}
	if got.overrides != want.overrides {
		t.Fatalf("%s: %s default overrides changed", where, in.name)
	}
	if got.perms != want.perms {
		t.Fatalf("%s: %s permissions changed", where, in.name)
	}
	skip := map[string]bool{}
	for _, c := range changed {
		skip[c] = true
	}
	for _, c := range added {
		if _, had := want.defs[c]; had {
			t.Fatalf("%s: %s %s was expected to be new", where, in.name, c)
		}
		if _, has := got.defs[c]; !has {
			t.Fatalf("%s: %s %s was expected to be created", where, in.name, c)
		}
		skip[c] = true
	}
	for c, row := range want.defs {
		if skip[c] {
			continue
		}
		if got.defs[c] != row {
			t.Fatalf("%s: %s definition %s changed", where, in.name, c)
		}
	}
	if len(got.defs) != len(want.defs)+len(added) {
		t.Fatalf("%s: %s has %d definitions, want %d", where, in.name, len(got.defs), len(want.defs)+len(added))
	}
}

func count(t *testing.T, in *instance, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := in.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s count: %v (%s)", in.name, err, sql)
	}
	return n
}

// createField creates a definition through the ordinary field API, then
// sets the PATCH-only portable properties, and returns its id.
func (c *cluster) createField(in *instance, create, patch string) string {
	c.t.Helper()
	r := c.call(in, in.token, http.MethodPost, "/fields", []byte(create))
	if r.status != 201 {
		c.t.Fatalf("%s create %s: %d %s", in.name, create, r.status, r.body)
	}
	var f struct {
		ID string `json:"id"`
	}
	mustJSON(c.t, r.body, &f)
	if patch != "" {
		if p := c.call(in, in.token, http.MethodPatch, "/fields/"+f.ID, []byte(patch)); p.status != 200 {
			c.t.Fatalf("%s patch %s: %d %s", in.name, patch, p.status, p.body)
		}
	}
	return f.ID
}

func (c *cluster) export(in *instance, token string, codes ...string) xresp {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"codes": codes})
	return c.call(in, token, http.MethodPost, "/fields/export", b)
}

// previewOf wraps exact export bytes (newline dropped inside the
// envelope) with an optional source claim.
func previewOf(export []byte, source string) []byte {
	b := append([]byte(`{"document":`), bytes.TrimSuffix(export, []byte("\n"))...)
	if source != "" {
		b = append(b, []byte(`,"source":`+source)...)
	}
	return append(b, '}')
}

type previewDoc struct {
	PreviewID      string `json:"preview_id"`
	DocumentDigest string `json:"document_digest"`
	Fields         []struct {
		Code        string   `json:"code"`
		Local       string   `json:"local"`
		LocalID     *string  `json:"local_id"`
		Decisions   []string `json:"decisions"`
		Differences []struct {
			Property          string   `json:"property"`
			TermsOnlyIncoming []string `json:"terms_only_incoming"`
			TermsOnlyLocal    []string `json:"terms_only_local"`
			TermsChanged      []string `json:"terms_changed"`
		} `json:"differences"`
	} `json:"fields"`
	UnresolvedTerms []struct{ Field, Term string } `json:"unresolved_terms"`
}

type receiptDoc struct {
	ReceiptID       string `json:"receipt_id"`
	DocumentDigest  string `json:"document_digest"`
	DecisionsDigest string `json:"decisions_digest"`
	Replayed        bool   `json:"replayed"`
	Changed         int    `json:"changed"`
	Fields          []struct {
		Code, Decision, Outcome string
		FieldID                 *string `json:"field_id"`
	} `json:"fields"`
}

func applyBody(p previewDoc, decisions map[string]string) []byte {
	b, _ := json.Marshal(map[string]any{"preview_id": p.PreviewID, "document_digest": p.DocumentDigest, "decisions": decisions})
	return b
}

func reasonOf(t *testing.T, b []byte) string {
	t.Helper()
	var r struct {
		Reason string `json:"reason"`
	}
	mustJSON(t, b, &r)
	return r.Reason
}

// gateCleanup removes everything a run owns on one instance: its
// assets (values cascade), receipts (items cascade), previews, fields,
// team, tokens, grants, sessions and users. Every statement's error is
// reported, and the scope is checked empty afterwards. Audit rows stay
// (an append-only log, in disposable databases).
func gateCleanup(t *testing.T, in *instance, prefix string, assets []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	like := prefix + "%"
	users := `SELECT ref FROM "user" WHERE username LIKE $1`
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM assets WHERE id = ANY($1::uuid[])`, []any{assets}},
		{`DELETE FROM field_schema_import_receipt WHERE actor_ref IN (` + users + `) OR actor_ref = $2`, []any{like, in.admin}},
		{`DELETE FROM field_schema_import_preview WHERE actor_ref IN (` + users + `) OR actor_ref = $2`, []any{like, in.admin}},
		{`UPDATE field_definition SET deprecated_replacement_id = NULL WHERE code LIKE $1`, []any{like}},
		{`DELETE FROM field_definition WHERE code LIKE $1`, []any{like}},
		{`DELETE FROM teams WHERE slug LIKE $1`, []any{like}},
		{`DELETE FROM api_tokens WHERE user_ref IN (` + users + `)`, []any{like}},
		{`DELETE FROM user_capability_grants WHERE user_ref IN (` + users + `)`, []any{like}},
		{`DELETE FROM sessions WHERE user_ref IN (` + users + `)`, []any{like}},
		{`DELETE FROM "user" WHERE username LIKE $1`, []any{like}},
	} {
		if _, err := in.pool.Exec(ctx, st.sql, st.args...); err != nil {
			t.Errorf("%s cleanup %q: %v", in.name, st.sql, err)
		}
	}
	var left int
	if err := in.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM assets WHERE id = ANY($2::uuid[]))
		+ (SELECT count(*) FROM field_definition WHERE code LIKE $1)
		+ (SELECT count(*) FROM "user" WHERE username LIKE $1)
		+ (SELECT count(*) FROM teams WHERE slug LIKE $1)
		+ (SELECT count(*) FROM field_schema_import_preview WHERE actor_ref = $3)
		+ (SELECT count(*) FROM field_schema_import_receipt WHERE actor_ref = $3)`, like, assets, in.admin).Scan(&left); err != nil {
		t.Errorf("%s cleanup check: %v", in.name, err)
	} else if left != 0 {
		t.Errorf("%s cleanup left %d scoped rows", in.name, left)
	} else {
		t.Logf("%s cleanup: scope empty", in.name)
	}
}

func TestTwoStack_FieldSchemaExchange(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	A, B := c.a, c.b
	pfx := "s3" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	code := func(s string) string { return pfx + "_" + s }
	var assetsA, assetsB []string
	t.Cleanup(func() { gateCleanup(t, A, pfx, assetsA); gateCleanup(t, B, pfx, assetsB) })

	// Isolation: two databases, neither the general one; two URLs.
	var dbA, dbB string
	if err := A.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbA); err != nil {
		t.Fatal(err)
	}
	if err := B.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbB); err != nil {
		t.Fatal(err)
	}
	if dbA == dbB || A.url == B.url || A.root == B.root {
		t.Fatalf("instances are not independent: %s %s %s %s", dbA, dbB, A.url, B.url)
	}
	t.Logf("A db=%s url=%s; B db=%s url=%s", dbA, A.url, dbB, B.url)

	hidden, match, tree, shot, kind, opts := code("ctl_hidden"), code("match"), code("tree"), code("shot"), code("kind"), code("opts")

	// ----- A: the source schema, through A's ordinary field API -----
	c.createField(A, fmt.Sprintf(`{"code":%q,"label":"Hidden controller","type":"select","options":{"values":["yes","no"]}}`, hidden), "")
	c.createField(A, fmt.Sprintf(`{"code":%q,"label":"Match","type":"select","options":{"values":[{"value":"a","label":"A"},"b"]}}`, match), "")
	treeIDA := c.createField(A, fmt.Sprintf(`{"code":%q,"label":"Region","type":"tree","description":"Where","display_group":"place","display_order":5,`+
		`"options":{"values":[{"value":"europe","label":"Europe","children":[{"value":"uk","label":"United Kingdom"}]},"asia"]}}`, tree),
		`{"show_on_card":true,"edit_tab":"Place"}`)
	c.createField(A, fmt.Sprintf(`{"code":%q,"label":"Shot","type":"text","required":true}`, shot),
		fmt.Sprintf(`{"regexp_filter":"^[A-Z]{2}[0-9]+$","edit_tab":"Production","show_on_upload":false,"display_condition":["%s=a","%s=yes"]}`, match, hidden))
	c.createField(A, fmt.Sprintf(`{"code":%q,"label":"Kind","type":"text"}`, kind), "")
	c.createField(A, fmt.Sprintf(`{"code":%q,"label":"Opts","type":"select","options":{"values":["a","b","c"]}}`, opts), "")

	// ----- B: matching, conflicting, and local-only settings -----
	matchIDB := c.createField(B, fmt.Sprintf(`{"code":%q,"label":"Match","type":"select","options":{"values":[{"value":"a","label":"A"},"b"]}}`, match), "")
	kindIDB := c.createField(B, fmt.Sprintf(`{"code":%q,"label":"Kind","type":"select","options":{"values":["k"]}}`, kind), "")
	optsIDB := c.createField(B, fmt.Sprintf(`{"code":%q,"label":"Opts","type":"select","options":{"values":[{"value":"a","label":"Alpha"},"x"]}}`, opts), "")
	// Local-only state on B the ordinary API does not set conveniently.
	var teamID string
	if err := B.pool.QueryRow(ctx, `INSERT INTO teams (slug, name) VALUES ($1, 'Schema gate team') RETURNING id::text`, pfx+"-team").Scan(&teamID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE field_definition SET read_capability='assets.read', write_capability='fields.admin', read_only=true, applies_to='{3,5}',
		   extraction_source='exif:Make', extraction_mode='replace', default_value='{"kind":"literal","value_text":"a"}',
		   options='{"values":[{"value":"a","label":"A","aliases":["alpha"]},"b",{"value":"old","status":"archived","replaced_by":"a"}]}'
		 WHERE id = $1`,
		`INSERT INTO field_default_override (field_id, team_id, default_value) VALUES ($1, '` + teamID + `', '{"kind":"literal","value_text":"b"}')`,
	} {
		if _, err := B.pool.Exec(ctx, stmt, matchIDB); err != nil {
			t.Fatalf("B local fixture: %v", err)
		}
	}
	if _, err := B.pool.Exec(ctx, `UPDATE field_definition SET read_capability='assets.read', status='active' WHERE id=$1`, optsIDB); err != nil {
		t.Fatal(err)
	}
	_, memberTok := principal(t, B, pfx, "member", "")
	_, otherAdminTok := principal(t, B, pfx, "admin2", "fields.admin")

	// An owned B asset holding values on pre-existing B definitions: the
	// archived local term on match, and values on the two conflicts.
	assetB := uuid.NewString()
	assetsB = append(assetsB, assetB)
	if _, err := B.pool.Exec(ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref) VALUES ($1, 'Schema gate asset', 1, $2)`, assetB, B.admin); err != nil {
		t.Fatalf("B asset: %v", err)
	}
	for _, v := range []struct{ field, text string }{{matchIDB, "old"}, {kindIDB, "k"}, {optsIDB, "x"}} {
		if _, err := B.pool.Exec(ctx, `INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by, set_by_user_ref) VALUES ($1, $2, $3, 'manual', $4)`,
			assetB, v.field, v.text, B.admin); err != nil {
			t.Fatalf("B value: %v", err)
		}
	}
	var valuesSeeded string
	if err := B.pool.QueryRow(ctx, `SELECT string_agg(f.code||'='||v.value_text, ',' ORDER BY f.code) FROM asset_field_value v JOIN field_definition f ON f.id=v.field_id
		WHERE v.asset_id=$1`, assetB).Scan(&valuesSeeded); err != nil {
		t.Fatal(err)
	}
	if valuesSeeded != fmt.Sprintf("%s=k,%s=old,%s=x", kind, match, opts) {
		t.Fatalf("value fixture: %q", valuesSeeded)
	}

	beforeA := snapshot(t, A, pfx)
	beforeB := snapshot(t, B, pfx)
	if strings.Count(beforeB.values, "\n")+1 != 3 || len(beforeB.defs) != 3 || beforeB.overrides == "" {
		t.Fatalf("B snapshot does not hold the fixture: %d defs, values %q", len(beforeB.defs), beforeB.values)
	}

	// ----- 1. A exports an explicit selection -----
	exp := c.export(A, A.token, kind, match, opts, shot, tree)
	if exp.status != 200 {
		t.Fatalf("A export: %d %s", exp.status, exp.body)
	}
	for _, forbidden := range []string{`"id"`, "applies_to", "read_capability", "write_capability", "read_only", "extraction", "default", "status", "aliases", treeIDA} {
		if bytes.Contains(exp.body, []byte(forbidden)) {
			t.Fatalf("A export carries %q", forbidden)
		}
	}

	// ----- 5a. trust: refusals across the pair, before any preview -----
	pv := previewOf(exp.body, `{"label":"Studio A drop","uri":"https://studio-a.example/fields"}`)
	for name, tok := range map[string]string{"anonymous": "", "A's admin token": A.token} {
		for _, body := range [][]byte{pv, []byte(`{"document":`)} {
			if r := c.call(B, tok, http.MethodPost, "/fields/import/preview", body); r.status != 401 {
				t.Fatalf("%s preview on B: %d", name, r.status)
			}
			if r := c.call(B, tok, http.MethodPost, "/fields/import/apply", body); r.status != 401 {
				t.Fatalf("%s apply on B: %d", name, r.status)
			}
		}
	}
	var memberBodies [][]byte
	for _, body := range [][]byte{pv, []byte(`{"document":`)} {
		r := c.call(B, memberTok, http.MethodPost, "/fields/import/preview", body)
		if r.status != 403 {
			t.Fatalf("member preview on B: %d", r.status)
		}
		memberBodies = append(memberBodies, r.body)
	}
	if !bytes.Equal(memberBodies[0], memberBodies[1]) || bytes.Contains(memberBodies[0], []byte(pfx)) {
		t.Fatal("member refusal depends on the body or names a code")
	}
	// A document edited to carry a sender-local member, a malformed one,
	// and an oversized request are refused.
	tampered := bytes.Replace(pv, []byte(`"code":"`+tree+`"`), []byte(`"read_capability":"x","code":"`+tree+`"`), 1)
	if r := c.call(B, B.token, http.MethodPost, "/fields/import/preview", tampered); r.status != 422 || reasonOf(t, r.body) != "document_invalid" {
		t.Fatalf("sender-local member: %d %s", r.status, r.body)
	}
	if r := c.call(B, B.token, http.MethodPost, "/fields/import/preview", pv[:len(pv)/2]); r.status != 400 {
		t.Fatalf("malformed: %d", r.status)
	}
	if r := c.call(B, B.token, http.MethodPost, "/fields/import/preview", append(append([]byte{}, pv...), bytes.Repeat([]byte(" "), 2_162_689)...)); r.status != 413 {
		t.Fatalf("oversized: %d", r.status)
	}
	sameExcept(t, B, "refused requests", beforeB, snapshot(t, B, pfx), nil, nil)
	if count(t, B, `SELECT count(*) FROM field_schema_import_preview WHERE actor_ref=$1`, B.admin) != 0 {
		t.Fatal("refused requests recorded a preview")
	}

	// ----- 2/3. B previews A's exact bytes -----
	r := c.call(B, B.token, http.MethodPost, "/fields/import/preview", pv)
	if r.status != 200 {
		t.Fatalf("B preview: %d %s", r.status, r.body)
	}
	var p previewDoc
	mustJSON(t, r.body, &p)
	if p.DocumentDigest != hexSum(exp.body) {
		t.Fatalf("preview digest %s, A export body %s", p.DocumentDigest, hexSum(exp.body))
	}
	states := map[string]string{}
	for _, f := range p.Fields {
		if _, dup := states[f.Code]; dup {
			t.Fatalf("preview lists %s twice", f.Code)
		}
		states[f.Code] = f.Local + " " + strings.Join(f.Decisions, ",")
	}
	if len(p.Fields) != 5 || len(states) != 5 {
		t.Fatalf("preview has %d fields, want 5", len(p.Fields))
	}
	wantStates := map[string]string{
		kind: "different keep_local,skip", match: "identical use_existing,skip", opts: "different keep_local,skip",
		shot: "absent create,skip", tree: "absent create,skip",
	}
	for k, v := range wantStates {
		if states[k] != v {
			t.Errorf("%s: %q, want %q", k, states[k], v)
		}
	}
	for _, f := range p.Fields {
		props := []string{}
		for _, d := range f.Differences {
			props = append(props, d.Property)
		}
		switch f.Code {
		case kind:
			// text versus a local select: the type, and options (null
			// versus the local terms).
			if strings.Join(props, ",") != "options,type" {
				t.Fatalf("kind differences %v, want options,type", props)
			}
		case opts:
			if strings.Join(props, ",") != "options" {
				t.Fatalf("opts differences %v, want exactly options", props)
			}
			d := f.Differences[0]
			if strings.Join(d.TermsOnlyIncoming, ",") != "b,c" || strings.Join(d.TermsOnlyLocal, ",") != "x" || strings.Join(d.TermsChanged, ",") != "a" {
				t.Fatalf("opts term diff %+v", d)
			}
		default:
			if len(props) != 0 {
				t.Fatalf("%s differences %v, want none", f.Code, props)
			}
		}
	}
	if len(p.UnresolvedTerms) != 1 || p.UnresolvedTerms[0].Field != shot || p.UnresolvedTerms[0].Term != hidden+"=yes" {
		t.Fatalf("unresolved %+v", p.UnresolvedTerms)
	}
	sameExcept(t, B, "preview", beforeB, snapshot(t, B, pfx), nil, nil)

	// B's ordinary member cannot apply either, for a valid map of an
	// existing preview or a malformed body: the same 403, no code named.
	validMap := map[string]string{kind: "keep_local", match: "use_existing", opts: "skip", shot: "create", tree: "create"}
	m1 := c.call(B, memberTok, http.MethodPost, "/fields/import/apply", applyBody(p, validMap))
	m2 := c.call(B, memberTok, http.MethodPost, "/fields/import/apply", []byte(`{"preview_id":`))
	if m1.status != 403 || m2.status != 403 || !bytes.Equal(m1.body, m2.body) || bytes.Contains(m1.body, []byte(pfx)) {
		t.Fatalf("member apply: %d %s / %d %s", m1.status, m1.body, m2.status, m2.body)
	}
	if count(t, B, `SELECT count(*) FROM field_schema_import_receipt WHERE preview_id=$1`, p.PreviewID) != 0 {
		t.Fatal("a member apply wrote a receipt")
	}

	// Incomplete decisions: refused atomically; the same preview stays.
	incomplete := map[string]string{kind: "keep_local", match: "use_existing", opts: "skip", shot: "create"}
	if r := c.call(B, B.token, http.MethodPost, "/fields/import/apply", applyBody(p, incomplete)); r.status != 422 || reasonOf(t, r.body) != "decision_invalid" {
		t.Fatalf("incomplete: %d %s", r.status, r.body)
	}
	// Another authorized B operator cannot see this preview: same as unknown.
	foreign := c.call(B, otherAdminTok, http.MethodPost, "/fields/import/apply", applyBody(p, incomplete))
	unknown := c.call(B, otherAdminTok, http.MethodPost, "/fields/import/apply", applyBody(previewDoc{PreviewID: uuid.NewString(), DocumentDigest: p.DocumentDigest}, incomplete))
	if foreign.status != 404 || !bytes.Equal(foreign.body, unknown.body) {
		t.Fatalf("foreign preview: %d %s vs %s", foreign.status, foreign.body, unknown.body)
	}
	sameExcept(t, B, "refused applies", beforeB, snapshot(t, B, pfx), nil, nil)
	if count(t, B, `SELECT count(*) FROM field_schema_import_receipt WHERE preview_id=$1`, p.PreviewID) != 0 {
		t.Fatal("refused applies wrote a receipt")
	}

	// The explicit valid map.
	decisions := map[string]string{kind: "keep_local", match: "use_existing", opts: "skip", shot: "create", tree: "create"}
	ar := c.call(B, B.token, http.MethodPost, "/fields/import/apply", applyBody(p, decisions))
	if ar.status != 200 {
		t.Fatalf("B apply: %d %s", ar.status, ar.body)
	}
	var rc receiptDoc
	mustJSON(t, ar.body, &rc)
	wantOutcome := map[string]string{kind: "kept_local", match: "matched", opts: "skipped", shot: "created", tree: "created"}
	ids := map[string]string{}
	if len(rc.Fields) != 5 {
		t.Fatalf("receipt has %d fields, want 5", len(rc.Fields))
	}
	for _, f := range rc.Fields {
		if _, dup := ids[f.Code]; dup {
			t.Fatalf("receipt lists %s twice", f.Code)
		}
		if f.Outcome != wantOutcome[f.Code] || f.Decision != decisions[f.Code] || f.FieldID == nil {
			t.Errorf("%s: %s/%s id %v", f.Code, f.Decision, f.Outcome, f.FieldID)
		} else {
			ids[f.Code] = *f.FieldID
		}
	}
	if rc.Replayed || rc.Changed != 2 || ids[match] != matchIDB || ids[kind] != kindIDB || ids[opts] != optsIDB {
		t.Fatalf("receipt %+v", rc)
	}
	// Provenance: B's actor, A's document digest, the decisions digest,
	// the operator's untrusted claims.
	var actor int64
	var docDig, decDig, label, uri string
	if err := B.pool.QueryRow(ctx, `SELECT actor_ref, document_digest, decisions_digest, source_label, source_uri FROM field_schema_import_receipt WHERE id=$1`,
		rc.ReceiptID).Scan(&actor, &docDig, &decDig, &label, &uri); err != nil {
		t.Fatal(err)
	}
	sortedDec, _ := json.Marshal(decisions)
	if actor != B.admin || docDig != hexSum(exp.body) || decDig != hexSum(sortedDec) || label != "Studio A drop" || uri != "https://studio-a.example/fields" {
		t.Fatalf("provenance %d %s %s %q %q", actor, docDig, decDig, label, uri)
	}
	if n := count(t, B, `SELECT count(*) FROM field_schema_import_receipt_item WHERE receipt_id=$1`, rc.ReceiptID); n != 5 {
		t.Fatalf("%d items", n)
	}
	if n := count(t, B, `SELECT count(*) FROM audit_events WHERE event_type='metadata.field_schema.imported' AND metadata->>'receipt_id'=$1`, rc.ReceiptID); n != 1 {
		t.Fatalf("%d audit events", n)
	}
	// Receiver-owned settings on the created rows.
	for _, cd := range []string{shot, tree} {
		var applies, src, mode, status string
		var rd, wr, mirror *string
		var ro bool
		var def []byte
		if err := B.pool.QueryRow(ctx, `SELECT applies_to::text, read_capability, write_capability, read_only, extraction_source, extraction_mode, default_value, status, mirrors_column
			FROM field_definition WHERE code=$1`, cd).Scan(&applies, &rd, &wr, &ro, &src, &mode, &def, &status, &mirror); err != nil {
			t.Fatal(err)
		}
		if applies != "{}" || rd != nil || wr != nil || ro || src != "" || mode != "skip_if_set" || def != nil || status != "active" || mirror != nil {
			t.Fatalf("%s local defaults: %s %v %v %v %q %s %s %s %v", cd, applies, rd, wr, ro, src, mode, def, status, mirror)
		}
	}
	// Preservation: every pre-existing B row in scope, overrides, values
	// and permissions are byte-identical; only the two new rows differ.
	afterB := snapshot(t, B, pfx)
	for _, cd := range []string{match, kind, opts} {
		if afterB.defs[cd] != beforeB.defs[cd] {
			t.Fatalf("B's %s changed", cd)
		}
	}
	sameExcept(t, B, "apply", beforeB, afterB, []string{shot, tree}, nil)
	sameExcept(t, A, "B's apply", beforeA, snapshot(t, A, pfx), nil, nil)

	// The adopted subset is the same schema on both sides, byte for byte.
	subsetA := c.export(A, A.token, match, shot, tree)
	subsetB := c.export(B, B.token, match, shot, tree)
	if subsetA.status != 200 || subsetB.status != 200 || !bytes.Equal(subsetA.body, subsetB.body) {
		t.Fatalf("adopted subset differs:\nA %s\nB %s", subsetA.body, subsetB.body)
	}
	t.Logf("adopted subset digest %s (A and B)", hexSum(subsetB.body))

	// Replay: same receipt, nothing new.
	again := c.call(B, B.token, http.MethodPost, "/fields/import/apply", applyBody(p, decisions))
	var rc2 receiptDoc
	mustJSON(t, again.body, &rc2)
	if again.status != 200 || !rc2.Replayed || rc2.ReceiptID != rc.ReceiptID ||
		count(t, B, `SELECT count(*) FROM field_schema_import_receipt_item WHERE receipt_id=$1`, rc.ReceiptID) != 5 ||
		count(t, B, `SELECT count(*) FROM audit_events WHERE event_type='metadata.field_schema.imported' AND metadata->>'receipt_id'=$1`, rc.ReceiptID) != 1 ||
		false {
		t.Fatalf("replay: %d %s", again.status, again.body)
	}
	sameExcept(t, B, "replay", afterB, snapshot(t, B, pfx), nil, nil)

	// ----- 5b. stale: an ordinary B edit after a preview -----
	r = c.call(B, B.token, http.MethodPost, "/fields/import/preview", previewOf(exp.body, ""))
	if r.status != 200 {
		t.Fatalf("stale preview: %d %s", r.status, r.body)
	}
	var ps previewDoc
	mustJSON(t, r.body, &ps)
	if p := c.call(B, B.token, http.MethodPatch, "/fields/"+kindIDB, []byte(`{"label":"Kind, edited on B"}`)); p.status != 200 {
		t.Fatalf("B edit: %d", p.status)
	}
	staleDec := map[string]string{kind: "keep_local", match: "use_existing", opts: "skip", shot: "skip", tree: "skip"}
	if r := c.call(B, B.token, http.MethodPost, "/fields/import/apply", applyBody(ps, staleDec)); r.status != 409 || reasonOf(t, r.body) != "preview_stale" ||
		count(t, B, `SELECT count(*) FROM field_schema_import_receipt WHERE preview_id=$1`, ps.PreviewID) != 0 {
		t.Fatalf("stale: %d %s", r.status, r.body)
	}
	// The only change is the intended local label edit; values are intact.
	afterStale := snapshot(t, B, pfx)
	sameExcept(t, B, "stale", afterB, afterStale, nil, []string{kind})
	if !strings.Contains(afterStale.defs[kind], `"label": "Kind, edited on B"`) {
		t.Fatalf("the local edit is not the stored label: %s", afterStale.defs[kind])
	}

	// ----- 4. an updated source schema needs another review -----
	if p := c.call(A, A.token, http.MethodPatch, "/fields/"+treeIDA, []byte(`{"label":"Region (revised)"}`)); p.status != 200 {
		t.Fatalf("A edit: %d %s", p.status, p.body)
	}
	sameExcept(t, B, "A's source edit", afterStale, snapshot(t, B, pfx), nil, nil)
	exp2 := c.export(A, A.token, tree)
	if exp2.status != 200 {
		t.Fatalf("A re-export: %d", exp2.status)
	}
	r = c.call(B, B.token, http.MethodPost, "/fields/import/preview", previewOf(exp2.body, ""))
	var p2 previewDoc
	mustJSON(t, r.body, &p2)
	if r.status != 200 || len(p2.Fields) != 1 || p2.Fields[0].Code != tree || p2.Fields[0].Local != "different" ||
		len(p2.Fields[0].Differences) != 1 || p2.Fields[0].Differences[0].Property != "label" {
		t.Fatalf("updated preview: %d %s", r.status, r.body)
	}
	var rc3 receiptDoc
	r = c.call(B, B.token, http.MethodPost, "/fields/import/apply", applyBody(p2, map[string]string{tree: "keep_local"}))
	mustJSON(t, r.body, &rc3)
	if r.status != 200 || rc3.Changed != 0 || len(rc3.Fields) != 1 || rc3.Fields[0].Outcome != "kept_local" {
		t.Fatalf("keep_local of the update: %d %s", r.status, r.body)
	}
	sameExcept(t, B, "keep_local of the update", afterStale, snapshot(t, B, pfx), nil, nil)

	// The omitted controller, adopted later, resolves the preserved term.
	exp3 := c.export(A, A.token, hidden)
	if exp3.status != 200 {
		t.Fatalf("A controller export: %d", exp3.status)
	}
	r = c.call(B, B.token, http.MethodPost, "/fields/import/preview", previewOf(exp3.body, ""))
	if r.status != 200 {
		t.Fatalf("controller preview: %d %s", r.status, r.body)
	}
	var p3 previewDoc
	mustJSON(t, r.body, &p3)
	var rc4 receiptDoc
	r = c.call(B, B.token, http.MethodPost, "/fields/import/apply", applyBody(p3, map[string]string{hidden: "create"}))
	mustJSON(t, r.body, &rc4)
	if r.status != 200 || rc4.Changed != 1 || len(rc4.Fields) != 1 || rc4.Fields[0].Outcome != "created" {
		t.Fatalf("controller adoption: %d %s", r.status, r.body)
	}
	afterController := snapshot(t, B, pfx)
	sameExcept(t, B, "controller adoption", afterStale, afterController, []string{hidden}, nil)
	if !strings.Contains(afterController.defs[shot], fmt.Sprintf(`"display_condition": ["%s=a", "%s=yes"]`, match, hidden)) {
		t.Fatalf("shot's condition is not the preserved terms: %s", afterController.defs[shot])
	}
	e1, e2 := c.export(A, A.token, hidden, shot), c.export(B, B.token, hidden, shot)
	if e1.status != 200 || e2.status != 200 || !bytes.Equal(e1.body, e2.body) {
		t.Fatalf("after resolution the subset differs: %d %d\n%s\n%s", e1.status, e2.status, e1.body, e2.body)
	}

	// ----- the reverse direction: B -> A, compact -----
	bOnly := code("b_only")
	c.createField(B, fmt.Sprintf(`{"code":%q,"label":"From B","type":"multi_select","options":{"values":["one",{"value":"two","label":"Two"}]}}`, bOnly),
		`{"show_in_advanced_search":false,"edit_tab":"B"}`)
	// A local value on A, so the reverse direction has values to keep.
	assetA := uuid.NewString()
	assetsA = append(assetsA, assetA)
	matchIDA := ""
	if err := A.pool.QueryRow(ctx, `SELECT id::text FROM field_definition WHERE code=$1`, match).Scan(&matchIDA); err != nil {
		t.Fatal(err)
	}
	if _, err := A.pool.Exec(ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref) VALUES ($1, 'Schema gate asset A', 1, $2)`, assetA, A.admin); err != nil {
		t.Fatalf("A asset: %v", err)
	}
	if _, err := A.pool.Exec(ctx, `INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by, set_by_user_ref) VALUES ($1, $2, 'b', 'manual', $3)`,
		assetA, matchIDA, A.admin); err != nil {
		t.Fatalf("A value: %v", err)
	}
	beforeA2 := snapshot(t, A, pfx)
	beforeB2 := snapshot(t, B, pfx)
	if beforeA2.values == "" {
		t.Fatal("A snapshot holds no values")
	}
	eb := c.export(B, B.token, bOnly)
	if eb.status != 200 {
		t.Fatalf("B export: %d", eb.status)
	}
	r = c.call(A, A.token, http.MethodPost, "/fields/import/preview", previewOf(eb.body, `{"label":"Studio B"}`))
	var pa previewDoc
	mustJSON(t, r.body, &pa)
	if r.status != 200 || len(pa.Fields) != 1 || pa.Fields[0].Local != "absent" || pa.DocumentDigest != hexSum(eb.body) {
		t.Fatalf("A preview: %d %s", r.status, r.body)
	}
	var rca receiptDoc
	r = c.call(A, A.token, http.MethodPost, "/fields/import/apply", applyBody(pa, map[string]string{bOnly: "create"}))
	mustJSON(t, r.body, &rca)
	if r.status != 200 || rca.Changed != 1 || len(rca.Fields) != 1 || rca.Fields[0].Outcome != "created" {
		t.Fatalf("A apply: %d %s", r.status, r.body)
	}
	ea := c.export(A, A.token, bOnly)
	if ea.status != 200 || !bytes.Equal(ea.body, eb.body) {
		t.Fatalf("B->A subset differs: %d\n%s\n%s", ea.status, ea.body, eb.body)
	}
	// Only the new definition differs on A; every A row, value, override
	// and permission is unchanged, and B is unchanged by A's import.
	sameExcept(t, A, "B->A apply", beforeA2, snapshot(t, A, pfx), []string{bOnly}, nil)
	sameExcept(t, B, "B->A apply", beforeB2, snapshot(t, B, pfx), nil, nil)
}
