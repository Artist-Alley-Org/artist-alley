// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// POST /api/v1/fields/export (#1576, ADR 0083) through the production
// router: the real server from New(), a real session cookie, and the
// stored rows in the test database.
//
// What is asserted:
//   - authorization before any stored schema is read: 401 and 403 bodies
//     are byte-identical whether or not the selected codes exist;
//   - the exact whitelist, at every level of the serialized object, and
//     that no sentinel from an excluded column reaches the wire;
//   - every portable type, options with labels, children and retired
//     terms, verbatim display-condition terms (one naming a missing
//     field), and explicit nulls;
//   - deterministic, sorted, de-duplicated output;
//   - unknown, retired and malformed selections refuse the whole request;
//   - the request (64 KiB, 100 codes) and export (2 MiB) bounds;
//   - nothing the endpoint can reach is changed by calling it.

package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/config"
)

// The versioned whitelist, spelled out here rather than read from the
// generated type, so a member added to the DTO fails this test.
var (
	exportTopKeys   = []string{"fields", "format"}
	exportFieldKeys = []string{
		"code", "description", "display_condition", "display_group", "display_order",
		"edit_tab", "label", "open_vocabulary", "options", "regexp_filter", "required",
		"searchable", "show_in_advanced_search", "show_on_card", "show_on_upload",
		"subject_kind", "type",
	}
	exportOptionKeys = []string{"children", "label", "value"}
)

type exportEnv struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	url    string
	prefix string
	ids    map[string]string // code -> id, for the sentinel scan
	// cookies by principal
	admin, sysAdmin, member string
}

func newExportEnv(t *testing.T) *exportEnv {
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
		prefix: "xp" + strings.ReplaceAll(uuid.NewString()[:8], "-", ""), ids: map[string]string{}}
	t.Cleanup(e.cleanup)
	e.admin = e.principal("fields.admin")
	e.sysAdmin = e.principal("system.admin")
	e.member = e.principal("")
	return e
}

func (e *exportEnv) code(s string) string { return e.prefix + "_" + s }

// principal creates an approved user holding exactly cap (none when
// empty) through a role of its own, and returns a session token.
func (e *exportEnv) principal(capability string) string {
	e.t.Helper()
	var ref int64
	name := e.prefix + "-" + strings.ReplaceAll(capability, ".", "-")
	if err := e.pool.QueryRow(e.ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Export Test', 1) RETURNING ref`,
		name).Scan(&ref); err != nil {
		e.t.Fatalf("seed user: %v", err)
	}
	if capability != "" {
		var role pgtype.UUID
		if err := e.pool.QueryRow(e.ctx, `INSERT INTO roles (name) VALUES ($1) RETURNING id`, name).Scan(&role); err != nil {
			e.t.Fatalf("seed role: %v", err)
		}
		if _, err := e.pool.Exec(e.ctx,
			`INSERT INTO role_capabilities (role_id, capability_code) VALUES ($1, $2)`, role, capability); err != nil {
			e.t.Fatalf("attach %s: %v", capability, err)
		}
		if _, err := e.pool.Exec(e.ctx,
			`INSERT INTO user_roles (user_ref, role_id) VALUES ($1, $2)`, ref, role); err != nil {
			e.t.Fatalf("assign role: %v", err)
		}
	}
	token, _, err := auth.NewSessionManager(e.pool).Issue(e.ctx, ref, httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		e.t.Fatalf("issue session: %v", err)
	}
	return token
}

func (e *exportEnv) cleanup() {
	ctx := context.Background()
	like := e.prefix + "%"
	_, _ = e.pool.Exec(ctx, `UPDATE field_definition SET deprecated_replacement_id = NULL WHERE code LIKE $1`, like)
	_, _ = e.pool.Exec(ctx, `DELETE FROM field_definition WHERE code LIKE $1`, like)
	_, _ = e.pool.Exec(ctx, `DELETE FROM sessions WHERE user_ref IN (SELECT ref FROM "user" WHERE username LIKE $1)`, like)
	_, _ = e.pool.Exec(ctx, `DELETE FROM user_roles WHERE user_ref IN (SELECT ref FROM "user" WHERE username LIKE $1)`, like)
	_, _ = e.pool.Exec(ctx, `DELETE FROM "user" WHERE username LIKE $1`, like)
	_, _ = e.pool.Exec(ctx, `DELETE FROM role_capabilities WHERE role_id IN (SELECT id FROM roles WHERE name LIKE $1)`, like)
	_, _ = e.pool.Exec(ctx, `DELETE FROM roles WHERE name LIKE $1`, like)
}

// field inserts one definition. cols overrides the defaults; every
// excluded column gets a sentinel unless the case says otherwise.
func (e *exportEnv) field(code, typ string, cols map[string]any) string {
	e.t.Helper()
	row := map[string]any{
		"code": code, "label": "Label " + code, "type": typ,
		// Sentinels in the columns that must never travel.
		"applies_to": []int64{424242, 434343}, "write_capability": "sentinel.write",
		"read_only": true, "extraction_source": "exif:SentinelTag", "extraction_mode": "replace",
	}
	for k, v := range cols {
		row[k] = v
	}
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]any, 0, len(keys))
	ph := make([]string, 0, len(keys))
	for i, k := range keys {
		args = append(args, row[k])
		ph = append(ph, fmt.Sprintf("$%d", i+1))
	}
	var id string
	q := `INSERT INTO field_definition (` + strings.Join(keys, ", ") + `) VALUES (` + strings.Join(ph, ", ") + `) RETURNING id::text`
	if err := e.pool.QueryRow(e.ctx, q, args...).Scan(&id); err != nil {
		e.t.Fatalf("seed field %s: %v", code, err)
	}
	e.ids[code] = id
	return id
}

type exportResp struct {
	status int
	body   []byte
}

func (e *exportEnv) post(token string, body []byte) exportResp {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return exportResp{resp.StatusCode, b}
}

func (e *exportEnv) export(token string, codes ...string) exportResp {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"codes": codes})
	return e.post(token, b)
}

// snapshot is every row the endpoint could conceivably change, as text.
func (e *exportEnv) snapshot() string {
	e.t.Helper()
	var defs, perms, defaults string
	if err := e.pool.QueryRow(e.ctx,
		`SELECT coalesce(string_agg(to_jsonb(f)::text, E'\n' ORDER BY f.code), '') FROM field_definition f`).Scan(&defs); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx,
		`SELECT coalesce(string_agg(role_id::text || ' ' || capability_code, E'\n' ORDER BY role_id, capability_code), '') FROM role_capabilities`).Scan(&perms); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx,
		`SELECT coalesce(string_agg(to_jsonb(o)::text, E'\n' ORDER BY o.field_id, o.team_id), '') FROM field_default_override o`).Scan(&defaults); err != nil {
		e.t.Fatal(err)
	}
	var values int64
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM asset_field_value`).Scan(&values); err != nil {
		e.t.Fatal(err)
	}
	return fmt.Sprintf("%s\n--\n%s\n--\n%s\n--\n%d", defs, perms, defaults, values)
}

func decodeObject(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// assertWhitelist walks the whole serialized document and requires the
// exact key set at each level, options and their children included.
func assertWhitelist(t *testing.T, body []byte) {
	t.Helper()
	top := decodeObject(t, body)
	if got := keysOf(top); !slices.Equal(got, exportTopKeys) {
		t.Fatalf("top-level keys %v, want exactly %v", got, exportTopKeys)
	}
	var fields []map[string]json.RawMessage
	if err := json.Unmarshal(top["fields"], &fields); err != nil {
		t.Fatalf("fields: %v", err)
	}
	var walk func(where string, raw json.RawMessage)
	walk = func(where string, raw json.RawMessage) {
		var opts []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &opts); err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		if opts == nil {
			t.Fatalf("%s is null, want an array", where)
		}
		for i, o := range opts {
			if got := keysOf(o); !slices.Equal(got, exportOptionKeys) {
				t.Errorf("%s[%d] keys %v, want exactly %v", where, i, got, exportOptionKeys)
			}
			walk(fmt.Sprintf("%s[%d].children", where, i), o["children"])
		}
	}
	for _, f := range fields {
		if got := keysOf(f); !slices.Equal(got, exportFieldKeys) {
			t.Errorf("field %s keys %v, want exactly %v", f["code"], got, exportFieldKeys)
		}
		if string(f["options"]) != "null" {
			walk(fmt.Sprintf("field %s options", f["code"]), f["options"])
		}
	}
}

type refusal struct {
	Error  string   `json:"error"`
	Reason string   `json:"reason"`
	Codes  []string `json:"codes"`
}

func mustRefusal(t *testing.T, r exportResp, reason string, codes []string) {
	t.Helper()
	if r.status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422; body %s", r.status, r.body)
	}
	var got refusal
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatalf("decode refusal %s: %v", r.body, err)
	}
	if got.Reason != reason || !slices.Equal(got.Codes, codes) || got.Error == "" {
		t.Fatalf("refusal %+v, want reason %s codes %v", got, reason, codes)
	}
	if strings.Contains(string(r.body), `"fields"`) {
		t.Fatalf("a refusal carried export content: %s", r.body)
	}
}

func TestExportFieldSchema_ProductionRouter(t *testing.T) {
	e := newExportEnv(t)

	// One definition per portable type, plus the vocabulary shapes.
	ctrl := e.code("ctrl")
	e.field(ctrl, "select", map[string]any{"options": `{"values":["commission","personal"]}`})
	for _, typ := range []string{"text", "longtext", "rich_text", "number", "boolean", "date", "datetime", "reference"} {
		e.field(e.code("t_"+typ), typ, nil)
	}
	sel := e.code("sel")
	selID := e.field(sel, "select", map[string]any{
		"description": "Colour space", "required": true, "searchable": false,
		"display_group": "technical", "display_order": 7, "open_vocabulary": true,
		"show_on_card": true, "show_in_advanced_search": false, "show_on_upload": false,
		"edit_tab": "Technical", "regexp_filter": "^[a-z_]+$",
		// Two terms: one names a field in this export, one a field that
		// does not exist anywhere. Both travel verbatim.
		"display_condition": fmt.Sprintf(`["%s=commission", "%s~urgent"]`, ctrl, e.code("absent")),
		"options": `{"values":["srgb",{"value":"linear","label":"Linear","aliases":["sentinelalias"]},` +
			`{"value":"old_space","label":"Old","status":"deprecated","replaced_by":"srgb"},` +
			`{"value":"gone","status":"archived","replaced_by":"srgb"}],"sentinel_rest_key":{"min":1}}`,
	})
	multi := e.code("multi")
	e.field(multi, "multi_select", map[string]any{"options": `{"values":[]}`})
	tree := e.code("tree")
	e.field(tree, "tree", map[string]any{
		"subject_kind": "collection", "applies_to": []int64{}, "extraction_source": "",
		"deprecated_replacement_id": selID,
		"options": `{"values":[{"value":"europe","label":"Europe","children":[` +
			`{"value":"uk","label":"United Kingdom","aliases":["sentinelgb"],"children":[{"value":"london"}]},` +
			`{"value":"atlantis","status":"archived","children":[{"value":"lost_city","label":"Lost"}]}]},` +
			`{"value":"asia","status":"deprecated","replaced_by":"europe"}]}`,
	})
	noVals := e.code("novals")
	e.field(noVals, "select", map[string]any{"options": `{}`})
	gated := e.code("gated")
	e.field(gated, "text", map[string]any{"read_capability": "sentinel.read", "default_value": `{"value": "sentinel-default"}`})
	dep := e.code("dep")
	e.field(dep, "text", map[string]any{"status": "deprecated"})
	arc := e.code("arc")
	e.field(arc, "text", map[string]any{"status": "archived"})
	badVals := e.code("badvals")
	e.field(badVals, "select", map[string]any{"options": `{"values": 5}`})
	dupVals := e.code("dupvals")
	e.field(dupVals, "tree", map[string]any{"options": `{"values":[{"value":"a","children":[{"value":"a"}]}]}`})
	big := e.code("big")
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO field_definition (code, label, type, options) VALUES ($1, 'Big', 'select',
		jsonb_build_object('values', (SELECT jsonb_agg(jsonb_build_object('value', 'v' || g || repeat('x', 60), 'label', 'Term ' || g))
		  FROM generate_series(1, 25000) g)))`, big); err != nil {
		t.Fatalf("seed big: %v", err)
	}

	portable := []string{ctrl, sel, multi, tree, noVals, gated}
	for _, typ := range []string{"text", "longtext", "rich_text", "number", "boolean", "date", "datetime", "reference"} {
		portable = append(portable, e.code("t_"+typ))
	}
	before := e.snapshot()

	t.Run("authorization precedes the schema", func(t *testing.T) {
		for _, c := range []struct {
			name, token string
			want        int
		}{{"anonymous", "", 401}, {"member without fields.admin", e.member, 403}} {
			existing := e.export(c.token, sel)
			missing := e.export(c.token, e.code("no_such_field"))
			retired := e.export(c.token, dep)
			if existing.status != c.want || missing.status != c.want || retired.status != c.want {
				t.Fatalf("%s: statuses %d/%d/%d, want %d", c.name, existing.status, missing.status, retired.status, c.want)
			}
			if !bytes.Equal(existing.body, missing.body) || !bytes.Equal(existing.body, retired.body) {
				t.Fatalf("%s: body differs by selection, an existence oracle:\n%s\n%s\n%s", c.name, existing.body, missing.body, retired.body)
			}
			if strings.Contains(string(existing.body), e.prefix) {
				t.Fatalf("%s: refusal names a selected code: %s", c.name, existing.body)
			}
		}
		for name, tok := range map[string]string{"fields.admin": e.admin, "system.admin": e.sysAdmin} {
			if r := e.export(tok, sel); r.status != 200 {
				t.Fatalf("%s: status %d body %s", name, r.status, r.body)
			}
		}
	})

	t.Run("whitelist, values and nulls", func(t *testing.T) {
		r := e.export(e.admin, portable...)
		if r.status != 200 {
			t.Fatalf("status %d body %s", r.status, r.body)
		}
		assertWhitelist(t, r.body)

		raw := string(r.body)
		for _, s := range []string{"sentinel", "424242", "434343", "exif:", "replace", "aliases", "replaced_by",
			"status", "deprecated", "archived", "old_space", "gone", "asia", "atlantis", "lost_city",
			"applies_to", "read_capability", "write_capability", "read_only", "mirrors_column",
			"created_at", "updated_at", "origin_server_id", "extraction", "default", `"id"`} {
			if strings.Contains(raw, s) {
				t.Errorf("export contains excluded %q", s)
			}
		}
		for code, id := range e.ids {
			if strings.Contains(raw, id) {
				t.Errorf("export contains the local id of %s", code)
			}
		}

		var doc struct {
			Format string                       `json:"format"`
			Fields []map[string]json.RawMessage `json:"fields"`
		}
		if err := json.Unmarshal(r.body, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Format != "aa-field-schema/1" {
			t.Fatalf("format %q", doc.Format)
		}
		gotCodes := []string{}
		byCode := map[string]map[string]json.RawMessage{}
		for _, f := range doc.Fields {
			var c string
			_ = json.Unmarshal(f["code"], &c)
			gotCodes = append(gotCodes, c)
			byCode[c] = f
		}
		want := slices.Clone(portable)
		slices.Sort(want)
		if !slices.Equal(gotCodes, want) {
			t.Fatalf("codes %v, want sorted %v", gotCodes, want)
		}

		// The fully populated select, member by member.
		wantSel := map[string]string{
			"code": `"` + sel + `"`, "label": `"Label ` + sel + `"`, "description": `"Colour space"`,
			"type": `"select"`, "subject_kind": `"asset"`, "required": "true", "searchable": "false",
			"display_group": `"technical"`, "display_order": "7", "regexp_filter": `"^[a-z_]+$"`,
			"display_condition": fmt.Sprintf(`["%s=commission","%s~urgent"]`, ctrl, e.code("absent")),
			"show_on_card":      "true", "show_in_advanced_search": "false", "show_on_upload": "false",
			"edit_tab": `"Technical"`, "open_vocabulary": "true",
			"options": `[{"children":[],"label":null,"value":"srgb"},{"children":[],"label":"Linear","value":"linear"}]`,
		}
		for k, v := range wantSel {
			if got := string(byCode[sel][k]); got != v {
				t.Errorf("%s.%s = %s, want %s", sel, k, got, v)
			}
		}
		// Retired branches are left out with their subtree; the
		// collection subject kind travels.
		if got, want := string(byCode[tree]["options"]),
			`[{"children":[{"children":[{"children":[],"label":null,"value":"london"}],"label":"United Kingdom","value":"uk"}],"label":"Europe","value":"europe"}]`; got != want {
			t.Errorf("tree options %s, want %s", got, want)
		}
		if got := string(byCode[tree]["subject_kind"]); got != `"collection"` {
			t.Errorf("tree subject_kind %s", got)
		}
		// Explicit nulls and empties: a vocabulary type with no terms is
		// an empty array, every other type is null.
		for code, want := range map[string]string{multi: "[]", noVals: "[]", gated: "null", e.code("t_reference"): "null"} {
			if got := string(byCode[code]["options"]); got != want {
				t.Errorf("%s options %s, want %s", code, got, want)
			}
		}
		for _, k := range []string{"regexp_filter", "display_condition", "edit_tab"} {
			if got := string(byCode[gated][k]); got != "null" {
				t.Errorf("%s.%s = %s, want null", gated, k, got)
			}
		}
		if got := string(byCode[gated]["description"]); got != `""` {
			t.Errorf("empty description %s, want \"\"", got)
		}
		for _, typ := range []string{"text", "longtext", "rich_text", "number", "boolean", "date", "datetime", "reference"} {
			if got := string(byCode[e.code("t_"+typ)]["type"]); got != `"`+typ+`"` {
				t.Errorf("type %s exported as %s", typ, got)
			}
		}
	})

	t.Run("deterministic and de-duplicated", func(t *testing.T) {
		a := e.export(e.admin, tree, sel, sel, ctrl, tree)
		b := e.export(e.admin, ctrl, tree, sel)
		c := e.export(e.admin, sel, ctrl, tree)
		if a.status != 200 || b.status != 200 || c.status != 200 {
			t.Fatalf("statuses %d %d %d", a.status, b.status, c.status)
		}
		if !bytes.Equal(a.body, b.body) || !bytes.Equal(b.body, c.body) {
			t.Fatalf("same selection, different bytes:\n%s\n%s", a.body, b.body)
		}
		var doc struct {
			Fields []struct{ Code string } `json:"fields"`
		}
		_ = json.Unmarshal(a.body, &doc)
		if len(doc.Fields) != 3 {
			t.Fatalf("%d fields, want 3 after folding duplicates", len(doc.Fields))
		}
	})

	t.Run("unknown, retired and malformed refuse the whole request", func(t *testing.T) {
		z, y := e.code("zz_missing"), e.code("aa_missing")
		mustRefusal(t, e.export(e.admin, sel, z, y), "unknown_codes", []string{y, z})
		// Unknown is reported before retired.
		mustRefusal(t, e.export(e.admin, dep, z), "unknown_codes", []string{z})
		mustRefusal(t, e.export(e.admin, sel, dep, arc), "retired_codes", []string{arc, dep})
		mustRefusal(t, e.export(e.admin, sel, dupVals, badVals), "malformed_definition", []string{badVals, dupVals})
		mustRefusal(t, e.export(e.admin, big), "export_too_large", []string{})
	})

	t.Run("request bounds", func(t *testing.T) {
		for name, body := range map[string]string{
			"empty list":     `{"codes":[]}`,
			"no codes":       `{}`,
			"null codes":     `{"codes":null}`,
			"bad syntax":     `{"codes":["Bad Code"]}`,
			"leading digit":  `{"codes":["1abc"]}`,
			"padded":         `{"codes":[" ` + sel + `"]}`,
			"not json":       `codes=` + sel,
			"wrong type":     `{"codes":"` + sel + `"}`,
			"101 duplicates": `{"codes":[` + strings.TrimSuffix(strings.Repeat(`"`+sel+`",`, 101), ",") + `]}`,
		} {
			if r := e.post(e.admin, []byte(body)); r.status != 400 {
				t.Errorf("%s: status %d, want 400; body %s", name, r.status, r.body)
			}
		}
		hundred := make([]string, 100)
		for i := range hundred {
			hundred[i] = sel
		}
		if r := e.export(e.admin, hundred...); r.status != 200 {
			t.Errorf("100 entries: status %d body %s", r.status, r.body)
		}

		// 64 KiB exactly reaches the handler; one byte more does not, for
		// any caller.
		pad := func(n int) []byte {
			b := []byte(`{"codes":["` + sel + `"],"pad":"`)
			b = append(b, bytes.Repeat([]byte("x"), n-len(b)-2)...)
			return append(b, '"', '}')
		}
		if r := e.post(e.admin, pad(64<<10)); r.status != 200 {
			t.Errorf("64 KiB body: status %d body %.200s", r.status, r.body)
		}
		for _, tok := range []string{e.admin, ""} {
			r := e.post(tok, pad(64<<10+1))
			if r.status != http.StatusRequestEntityTooLarge || !strings.Contains(string(r.body), `"error"`) {
				t.Errorf("64 KiB + 1: status %d body %s", r.status, r.body)
			}
		}
	})

	if after := e.snapshot(); after != before {
		t.Fatalf("the export changed stored state")
	}
}

// The response bound is enforced on the encoded export, so a selection
// just under it succeeds: the cap refuses, it never truncates.
func TestExportFieldSchema_UnderTheResponseBound(t *testing.T) {
	e := newExportEnv(t)
	code := e.code("near")
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO field_definition (code, label, type, options) VALUES ($1, 'Near', 'select',
		jsonb_build_object('values', (SELECT jsonb_agg(jsonb_build_object('value', 'v' || g || repeat('x', 60)))
		  FROM generate_series(1, 18000) g)))`, code); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := e.export(e.admin, code)
	if r.status != 200 {
		t.Fatalf("status %d body %.300s", r.status, r.body)
	}
	if len(r.body) < 1<<20 || len(r.body) > 2<<20 {
		t.Fatalf("body is %d bytes; the fixture should sit between 1 and 2 MiB", len(r.body))
	}
	var doc struct {
		Fields []struct {
			Options []struct{ Value string } `json:"options"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(r.body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Fields) != 1 || len(doc.Fields[0].Options) != 18000 {
		t.Fatalf("exported %d terms, want all 18000", len(doc.Fields[0].Options))
	}
	if !reflect.DeepEqual(doc.Fields[0].Options[17999].Value, "v18000"+strings.Repeat("x", 60)) {
		t.Fatalf("last term %q", doc.Fields[0].Options[17999].Value)
	}
}

// The 2 MiB bound is on the response BODY, byte for byte: a body of
// exactly 2 MiB is served, and one byte more is the typed refusal with no
// fields. The size is set with an ASCII description, which JSON encodes
// one byte per character, so the length is exact rather than estimated.
func TestExportFieldSchema_ExactResponseBound(t *testing.T) {
	e := newExportEnv(t)
	code := e.code("edge")
	e.field(code, "text", map[string]any{"description": ""})
	setDescription := func(n int) {
		t.Helper()
		if _, err := e.pool.Exec(e.ctx, `UPDATE field_definition SET description = repeat('a', $2) WHERE code = $1`,
			code, n); err != nil {
			t.Fatalf("set description: %v", err)
		}
	}

	empty := e.export(e.admin, code)
	if empty.status != 200 || !bytes.HasSuffix(empty.body, []byte("}\n")) {
		t.Fatalf("baseline: status %d, body %q", empty.status, empty.body)
	}
	fill := 2<<20 - len(empty.body)

	setDescription(fill)
	at := e.export(e.admin, code)
	if at.status != 200 {
		t.Fatalf("exactly 2 MiB: status %d, body %.300s", at.status, at.body)
	}
	if len(at.body) != 2<<20 {
		t.Fatalf("exactly 2 MiB: body is %d bytes, want %d", len(at.body), 2<<20)
	}

	setDescription(fill + 1)
	over := e.export(e.admin, code)
	mustRefusal(t, over, "export_too_large", []string{})
}
