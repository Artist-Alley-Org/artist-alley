// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 S1 against a live database: the baseline descriptive and rights
// members an aa:AssetVersion carries (descriptive.go): what is captured
// when an announcement is recorded, under the peer emission policy; what
// every delivery sends; and the refusal when a recorded member's source is
// no longer shareable. Skips without AA_DB_PASSWORD.
//
// These tests change the seeded rights definitions and the shipped
// title / description mirror bindings, which every test in the database
// shares; each change is undone in a cleanup.

package assetsend_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// The seeded definitions of migration 00097.
var (
	seedRights    = uuid.MustParse("56d71791-189d-429c-b7e4-e837baf91ddb")
	seedStatement = uuid.MustParse("6c3e25be-83ed-4762-b636-a1e37cdbb74c")
)

const goodRights = "https://creativecommons.org/licenses/by/4.0/"

// setText sets a's value of field id.
func (f *fx) setText(a asset, field uuid.UUID, v string) {
	f.t.Helper()
	f.exec(`INSERT INTO asset_field_value (asset_id, field_id, value_text) VALUES ($1, $2, $3)
		ON CONFLICT (asset_id, field_id) DO UPDATE SET value_text = EXCLUDED.value_text`, a.id, field, v)
}

// describe sets a's columns and its seeded rights values.
func (f *fx) describe(a asset, title, description, rights, statement string) {
	f.t.Helper()
	f.exec(`UPDATE assets SET title = $2, description = $3 WHERE id = $1`, a.id, title, description)
	if rights != "" {
		f.setText(a, seedRights, rights)
	}
	if statement != "" {
		f.setText(a, seedStatement, statement)
	}
}

// restoreDefinition puts definition id back as it is now, after t.
func (f *fx) restoreDefinition(t *testing.T, id uuid.UUID) {
	t.Helper()
	var row []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT to_jsonb(d) FROM field_definition d WHERE id = $1`, id).Scan(&row); err != nil {
		t.Fatalf("snapshot definition %s: %v", id, err)
	}
	t.Cleanup(func() {
		if _, err := f.pool.Exec(context.Background(), `
			UPDATE field_definition d SET code = r.code, type = r.type, status = r.status, read_capability = r.read_capability,
			       applies_to = r.applies_to, mirrors_column = r.mirrors_column, subject_kind = r.subject_kind
			  FROM jsonb_populate_record(NULL::field_definition, $2::jsonb) r
			 WHERE d.id = $1`, id, row); err != nil {
			t.Errorf("restore definition %s: %v", id, err)
		}
	})
}

// mirrorOf is the id of the shipped definition mirroring column.
func (f *fx) mirrorOf(column string) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM field_definition WHERE code = $1 AND mirrors_column = $1`, column).Scan(&id); err != nil {
		f.t.Fatalf("the shipped %s mirror: %v", column, err)
	}
	return id
}

// extraMirror adds another definition mirroring column, under another
// code, gated by readCap ("" ungated); removed after t.
func (f *fx) extraMirror(t *testing.T, column, readCap string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	typ := "text"
	if column == "description" {
		typ = "longtext"
	}
	f.exec(`INSERT INTO field_definition (id, code, label, type, mirrors_column, read_capability, status)
		VALUES ($1, $2, 'Alternate', $3, $4, NULLIF($5, ''), 'archived')`, id, "alt_"+column+"_"+strings.ReplaceAll(uuid.NewString()[:8], "-", ""), typ, column, readCap)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM field_definition WHERE id = $1`, id) })
	return id
}

// snapshot is the announcement's recorded descriptive member.
func (f *fx) snapshot(g grant) map[string]any {
	f.t.Helper()
	var p struct {
		D map[string]any `json:"descriptive"`
	}
	if err := json.Unmarshal(g.payload, &p); err != nil {
		f.t.Fatal(err)
	}
	return p.D
}

func omitted(s map[string]any) map[string]any {
	o, _ := s["omitted"].(map[string]any)
	return o
}

// announcedNow records and delivers a grant of a, returning it.
func (f *fx) announcedNow(a asset) grant {
	f.t.Helper()
	g := f.grantAndRecord(a, nil)
	if g.announcement == uuid.Nil {
		f.t.Fatal("nothing announced")
	}
	f.shareDelivered(g, "sent")
	return g
}

func TestDescriptive_SendsTheEligibleBaselineAsPlainText(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	title := `<img src=x onerror=alert(1)> Harbour`
	desc := "<script>alert(1)</script>\njavascript:alert(1) https://example.invalid/x"
	statement := `<b>Photo</b>: Studio A, javascript:void(0)`
	f.describe(a, title, desc, goodRights, statement)
	g := f.announcedNow(a)

	snap := f.snapshot(g)
	if snap["v"] != float64(1) || snap["title"] != title || snap["description"] != desc || snap["rights"] != goodRights ||
		snap["required_statement"] != statement || len(omitted(snap)) != 0 {
		t.Fatalf("snapshot: %v", snap)
	}
	m, err := f.member(g)
	if err != nil {
		t.Fatal(err)
	}
	if m.Description == nil || m.Description.Title != title || m.Description.Description != desc ||
		m.Description.Creator != nil || m.Description.Subject != nil || m.Description.Date != "" || m.Description.Language != "" {
		t.Fatalf("description: %+v", m.Description)
	}
	if m.Rights != goodRights || m.RequiredStatement == nil ||
		strings.Join(m.RequiredStatement.Label["en"], "|") != "Attribution" || len(m.RequiredStatement.Label) != 1 ||
		strings.Join(m.RequiredStatement.Value["none"], "|") != statement || len(m.RequiredStatement.Value) != 1 {
		t.Fatalf("rights: %q %+v", m.Rights, m.RequiredStatement)
	}
	// Nothing else is invented.
	if m.Identifiers != nil || m.AccessionNumber != "" || m.IIIFManifest != "" {
		t.Fatalf("unexpected members: %+v", m)
	}
}

func TestDescriptive_OmitsWithAReasonAndNeverTruncates(t *testing.T) {
	f := newFx(t)
	cases := []struct {
		name                           string
		title, desc, rights, statement string
		want                           map[string]string
	}{
		{"nothing set", "", "", "", "", map[string]string{"title": "empty", "description": "empty", "rights": "empty", "requiredStatement": "empty"}},
		{"a rights URI the wire refuses", "T", "", "http://evil.example/licence", "", map[string]string{"description": "empty", "rights": "invalid", "requiredStatement": "empty"}},
		{"a javascript rights value", "T", "", "javascript:alert(1)", "", map[string]string{"description": "empty", "rights": "invalid", "requiredStatement": "empty"}},
		{"an over-limit title", strings.Repeat("a", federation.ArchivalMaxTitleBytes+1), "D", "", "", map[string]string{"title": "over_limit", "rights": "empty", "requiredStatement": "empty"}},
		{"an over-limit description", "T", strings.Repeat("d", federation.ArchivalMaxLongText+1), "", "", map[string]string{"description": "over_limit", "rights": "empty", "requiredStatement": "empty"}},
		{"a control character", "T\x01", "", "", "S\x02", map[string]string{"title": "invalid", "description": "empty", "rights": "empty", "requiredStatement": "invalid"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := f.newAsset("public", true)
			f.describe(a, c.title, c.desc, c.rights, c.statement)
			g := f.announcedNow(a)
			snap := f.snapshot(g)
			got := map[string]string{}
			for k, v := range omitted(snap) {
				got[k] = v.(string)
			}
			if len(got) != len(c.want) {
				t.Fatalf("omitted %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("omitted %v, want %v", got, c.want)
				}
			}
			for k := range c.want {
				key := map[string]string{"title": "title", "description": "description", "rights": "rights", "requiredStatement": "required_statement"}[k]
				if _, ok := snap[key]; ok {
					t.Fatalf("%s omitted but recorded: %v", k, snap)
				}
			}
			raw, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, g.payload)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			_ = json.Unmarshal(raw, &wire)
			for k := range c.want {
				if k == "title" || k == "description" {
					if d, ok := wire["description"].(map[string]any); ok {
						if _, has := d[k]; has {
							t.Fatalf("omitted %s sent: %s", k, raw)
						}
						if len(d) == 0 {
							t.Fatalf("an empty description object: %s", raw)
						}
					}
					continue
				}
				if _, has := wire[k]; has {
					t.Fatalf("omitted %s sent: %s", k, raw)
				}
			}
			// Never a truncated or repaired value on the wire.
			if strings.Contains(string(raw), strings.Repeat("a", 64)) || strings.Contains(string(raw), strings.Repeat("d", 64)) {
				t.Fatalf("a truncated value was sent: %.200s", raw)
			}
		})
	}
}

func TestDescriptive_ThePeerPolicy(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()

	// The sharing owner holds everything: a global system.admin and
	// fields.admin role, and the same as a team-scoped grant. A peer
	// inherits none of it.
	var role, team uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO roles (name) VALUES ($1) RETURNING id`, "as-role-"+uuid.NewString()[:8]).Scan(&role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM roles WHERE id = $1`, role) })
	f.exec(`INSERT INTO role_capabilities (role_id, capability_code) VALUES ($1, 'system.admin'), ($1, 'fields.admin')`, role)
	f.exec(`INSERT INTO user_roles (user_ref, role_id) VALUES ($1, $2)`, f.owner, role)
	if err := f.pool.QueryRow(ctx, `INSERT INTO teams (name, slug) VALUES ($1, $1) RETURNING id`, "as-team-"+uuid.NewString()[:8]).Scan(&team); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, team) })
	f.exec(`INSERT INTO user_roles (user_ref, role_id, team_id) VALUES ($1, $2, $3)`, f.owner, role, team)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM user_roles WHERE user_ref = $1`, f.owner)
	})

	type step struct {
		name  string
		setup func(t *testing.T)
		want  map[string]string // omitted member -> reason
	}
	steps := []step{
		{"the seeded rights field gated by a capability the owner holds", func(t *testing.T) {
			f.restoreDefinition(t, seedRights)
			f.exec(`UPDATE field_definition SET read_capability = 'fields.admin' WHERE id = $1`, seedRights)
		}, map[string]string{"rights": "gated"}},
		{"the seeded statement retired", func(t *testing.T) {
			f.restoreDefinition(t, seedStatement)
			f.exec(`UPDATE field_definition SET status = 'archived' WHERE id = $1`, seedStatement)
		}, map[string]string{"requiredStatement": "inactive"}},
		{"the seeded rights field retyped", func(t *testing.T) {
			f.restoreDefinition(t, seedRights)
			f.exec(`UPDATE field_definition SET type = 'longtext' WHERE id = $1`, seedRights)
		}, map[string]string{"rights": "wrong_type"}},
		{"the seeded statement not for this asset type", func(t *testing.T) {
			f.restoreDefinition(t, seedStatement)
			f.exec(`UPDATE field_definition SET applies_to = '{987654}' WHERE id = $1`, seedStatement)
		}, map[string]string{"requiredStatement": "not_applicable"}},
		{"the shipped title mirror gated", func(t *testing.T) {
			id := f.mirrorOf("title")
			f.restoreDefinition(t, id)
			f.exec(`UPDATE field_definition SET read_capability = 'system.admin' WHERE id = $1`, id)
		}, map[string]string{"title": "gated"}},
		{"another, archived, gated mirror of the description", func(t *testing.T) {
			f.extraMirror(t, "description", "fields.admin")
		}, map[string]string{"description": "gated"}},
		{"an ungated extra mirror changes nothing", func(t *testing.T) {
			f.extraMirror(t, "title", "")
		}, map[string]string{}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			s.setup(t)
			a := f.newAsset("team", true)
			f.describe(a, "Title", "Description", goodRights, "Statement")
			g := f.announcedNow(a)
			snap := f.snapshot(g)
			got := map[string]string{}
			for k, v := range omitted(snap) {
				got[k] = v.(string)
			}
			if len(got) != len(s.want) {
				t.Fatalf("omitted %v, want %v", got, s.want)
			}
			for k, v := range s.want {
				if got[k] != v {
					t.Fatalf("omitted %v, want %v", got, s.want)
				}
			}
			m, err := f.member(g)
			if err != nil {
				t.Fatal(err)
			}
			if s.want["rights"] != "" && m.Rights != "" {
				t.Fatalf("gated rights sent: %q", m.Rights)
			}
			if s.want["title"] != "" && m.Description != nil && m.Description.Title != "" {
				t.Fatalf("gated title sent: %q", m.Description.Title)
			}
			if s.want["description"] != "" && m.Description != nil && m.Description.Description != "" {
				t.Fatalf("gated description sent: %q", m.Description.Description)
			}
			if s.want["requiredStatement"] != "" && m.RequiredStatement != nil {
				t.Fatalf("withheld statement sent")
			}
		})
	}
}

// A seeded definition an operator turned into a mirror of a column is no
// longer a source (and, being a mirror, can hold no value of its own).
func TestDescriptive_AMirroredSeedIsNotASource(t *testing.T) {
	f := newFx(t)
	f.restoreDefinition(t, seedStatement)
	f.exec(`UPDATE field_definition SET mirrors_column = 'description' WHERE id = $1`, seedStatement)
	a := f.newAsset("public", true)
	f.describe(a, "Title", "Body", goodRights, "")
	g := f.announcedNow(a)
	snap := f.snapshot(g)
	if r := omitted(snap)["requiredStatement"]; r != "mirrored" {
		t.Fatalf("omitted %v, want requiredStatement mirrored", snap)
	}
	// The column it mirrors is ungated, so the description itself is sent.
	if m, err := f.member(g); err != nil || m.Description == nil || m.Description.Description != "Body" || m.RequiredStatement != nil {
		t.Fatalf("member: %+v %v", m, err)
	}
}

// An operator's own field is never adopted: only the row with the seeded
// id AND code is a source. Without it the member is omitted, unmapped.
func TestDescriptive_AnotherFieldIsNeverAdopted(t *testing.T) {
	f := newFx(t)
	f.restoreDefinition(t, seedRights)
	// The seeded row keeps its id but no longer carries the seeded code,
	// as when another row held that code first.
	f.exec(`UPDATE field_definition SET code = $2 WHERE id = $1`, seedRights, "rights_uri_moved_"+uuid.NewString()[:8])
	other := uuid.New()
	f.exec(`INSERT INTO field_definition (id, code, label, type) VALUES ($1, 'rights_uri', 'Operator rights', 'text')`, other)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM field_definition WHERE id = $1`, other) })

	a := f.newAsset("public", true)
	f.setText(a, other, goodRights)
	f.setText(a, seedRights, goodRights)
	g := f.announcedNow(a)
	if r := omitted(f.snapshot(g))["rights"]; r != "unmapped" {
		t.Fatalf("rights omitted as %v, want unmapped", r)
	}
	m, err := f.member(g)
	if err != nil || m.Rights != "" {
		t.Fatalf("an operator's rights_uri was adopted: %q %v", m.Rights, err)
	}
	// The operator's field and value are untouched.
	var code, value string
	if err := f.pool.QueryRow(context.Background(), `SELECT d.code, v.value_text FROM field_definition d
		JOIN asset_field_value v ON v.field_id = d.id AND v.asset_id = $2 WHERE d.id = $1`, other, a.id).Scan(&code, &value); err != nil ||
		code != "rights_uri" || value != goodRights {
		t.Fatalf("the operator's field: %q %q %v", code, value, err)
	}
}

// Every delivery sends the recorded bytes: a later edit, or a value set
// after an omission, changes nothing; a record from before #1577 sends no
// descriptive member at all, whatever the asset holds now.
func TestDescriptive_TheRecordedBytesAreSentOnEveryDelivery(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	f.describe(a, "First title", "", goodRights, "")
	g := f.announcedNow(a)
	first, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, g.payload)
	if err != nil {
		t.Fatal(err)
	}
	f.describe(a, "Second title", "Now described", "https://rightsstatements.org/vocab/InC/1.0/", "Now required")
	again, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, g.payload)
	if err != nil || string(again) != string(first) {
		t.Fatalf("a redelivery after edits:\n%s\nwant\n%s (%v)", again, first, err)
	}
	if !strings.Contains(string(first), "First title") || strings.Contains(string(again), "Now described") ||
		strings.Contains(string(again), "Now required") || strings.Contains(string(again), "rightsstatements") {
		t.Fatalf("the redelivery is not the recorded one: %s", again)
	}

	// A new announcement (the next sealed version) captures the values
	// of its own time.
	ids, err := f.sealNext(&a, 2, true, nil)
	if err != nil || len(ids) != 1 {
		t.Fatalf("seal: %v %v", ids, err)
	}
	m2, _ := f.memberOf(ids[0])
	if m2.Description == nil || m2.Description.Title != "Second title" || m2.Rights != "https://rightsstatements.org/vocab/InC/1.0/" {
		t.Fatalf("a new announcement: %+v", m2)
	}

	// A record from before #1577 (no snapshot) sends none.
	var legacy []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT (payload - 'descriptive')::text FROM activities WHERE id = $1`, g.announcement).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	raw, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"description"`, `"rights"`, `"requiredStatement"`} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("a legacy record sent %s: %s", k, raw)
		}
	}
	// An unknown snapshot version is refused, not guessed at.
	var future map[string]any
	_ = json.Unmarshal(g.payload, &future)
	future["descriptive"] = map[string]any{"v": 2, "title": "x"}
	fb, _ := json.Marshal(future)
	if _, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, fb); !errors.Is(err, federation.ErrNotFederated) {
		t.Fatalf("snapshot v2: %v, want refused", err)
	}
}

// Recorded bytes never override current privacy: a source no longer
// shareable refuses the whole activity, naming no value.
func TestDescriptive_ARestrictionAfterRecordingRefusesTheActivity(t *testing.T) {
	f := newFx(t)
	secret := "Embargoed-statement-" + uuid.NewString()[:8]
	cases := []struct {
		name     string
		restrict func(t *testing.T)
	}{
		{"the seeded rights field gated", func(t *testing.T) {
			f.restoreDefinition(t, seedRights)
			f.exec(`UPDATE field_definition SET read_capability = 'system.admin' WHERE id = $1`, seedRights)
		}},
		{"the seeded statement retired", func(t *testing.T) {
			f.restoreDefinition(t, seedStatement)
			f.exec(`UPDATE field_definition SET status = 'deprecated' WHERE id = $1`, seedStatement)
		}},
		{"the seeded statement no longer for this type", func(t *testing.T) {
			f.restoreDefinition(t, seedStatement)
			f.exec(`UPDATE field_definition SET applies_to = '{987654}' WHERE id = $1`, seedStatement)
		}},
		{"a new gated mirror of the title", func(t *testing.T) { f.extraMirror(t, "title", "system.admin") }},
		{"the shipped description mirror gated", func(t *testing.T) {
			id := f.mirrorOf("description")
			f.restoreDefinition(t, id)
			f.exec(`UPDATE field_definition SET read_capability = 'fields.admin' WHERE id = $1`, id)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := f.newAsset("public", true)
			f.describe(a, "Title", "Description", goodRights, secret)
			g := f.announcedNow(a)
			if _, err := f.member(g); err != nil {
				t.Fatalf("before: %v", err)
			}
			c.restrict(t)
			_, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, g.payload)
			if !errors.Is(err, federation.ErrNotFederated) {
				t.Fatalf("after: %v, want the activity refused", err)
			}
			for _, v := range []string{secret, goodRights, "Title", "Description"} {
				if strings.Contains(err.Error(), v) {
					t.Fatalf("the refusal names a value: %v", err)
				}
			}
		})
	}
	t.Run("an edit of a value is not a restriction", func(t *testing.T) {
		a := f.newAsset("public", true)
		f.describe(a, "Title", "", goodRights, "")
		g := f.announcedNow(a)
		f.setText(a, seedRights, "not even a URI")
		if _, err := f.member(g); err != nil {
			t.Fatalf("a value edit refused the activity: %v", err)
		}
	})
}

// The capture is one statement inside the recording transaction: a gate
// committed before it is honoured; one still uncommitted is not seen, and
// once it commits the next delivery refuses. A title change, which updates
// the asset row the recording holds, commits wholly before or after.
func TestDescriptive_CaptureAndRestrictionAreOrdered(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.restoreDefinition(t, seedRights)

	t.Run("a gate in flight is not seen, then refuses the delivery", func(t *testing.T) {
		a := f.newAsset("public", true)
		f.describe(a, "Title", "", goodRights, "")
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, `UPDATE field_definition SET read_capability = 'system.admin' WHERE id = $1`, seedRights); err != nil {
			t.Fatal(err)
		}
		done := make(chan grant, 1)
		go func() { g, _ := f.grantAndRecordErr(a, nil); done <- g }()
		var g grant
		select {
		case g = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the recording waited on the uncommitted gate")
		}
		f.shareDelivered(g, "sent")
		if f.snapshot(g)["rights"] != goodRights {
			t.Fatalf("captured: %v", f.snapshot(g))
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := f.src.Member(ctx, g.announcement, f.peer, g.objectURI, g.payload); !errors.Is(err, federation.ErrNotFederated) {
			t.Fatalf("after the gate committed: %v", err)
		}
		f.exec(`UPDATE field_definition SET read_capability = NULL WHERE id = $1`, seedRights)
	})

	t.Run("a committed gate is honoured at capture", func(t *testing.T) {
		a := f.newAsset("public", true)
		f.describe(a, "Title", "", goodRights, "")
		f.exec(`UPDATE field_definition SET read_capability = 'system.admin' WHERE id = $1`, seedRights)
		g := f.announcedNow(a)
		if r := omitted(f.snapshot(g))["rights"]; r != "gated" {
			t.Fatalf("omitted %v", f.snapshot(g))
		}
		f.exec(`UPDATE field_definition SET read_capability = NULL WHERE id = $1`, seedRights)
	})

	t.Run("a title change commits wholly before the capture", func(t *testing.T) {
		a := f.newAsset("public", true)
		f.describe(a, "Old title", "Old description", "", "")
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, `UPDATE assets SET title = 'New title', description = 'New description' WHERE id = $1`, a.id); err != nil {
			t.Fatal(err)
		}
		done := make(chan grant, 1)
		go func() { g, _ := f.grantAndRecordErr(a, nil); done <- g }()
		// The recording waits for the asset row.
		deadline := time.Now().Add(10 * time.Second)
		for {
			var n int
			_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%FOR SHARE OF a%'`).Scan(&n)
			if n > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the recording never waited for the asset row")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		g := <-done
		if s := f.snapshot(g); s["title"] != "New title" || s["description"] != "New description" {
			t.Fatalf("captured %v", s)
		}
	})
}

// A sealed later version is announced with the descriptive members of its
// own recording, on the catch-up path.
func TestDescriptive_ASealedVersionCarriesItsOwnCapture(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	f.describe(a, "Version one", "", goodRights, "Statement")
	f.announcedNow(a)
	f.describe(a, "Version two", "", "", "")
	ids, err := f.sealNext(&a, 2, true, nil)
	if err != nil || len(ids) != 1 {
		t.Fatalf("seal: %v %v", ids, err)
	}
	m, raw := f.memberOf(ids[0])
	if m.Version.Number != 2 || m.Description == nil || m.Description.Title != "Version two" || m.Rights != goodRights ||
		m.RequiredStatement == nil {
		t.Fatalf("the sealed version's member: %s", raw)
	}
}

// An Update (§17.2.8) stays mature and aiProvenance only, whatever the
// asset describes.
func TestDescriptive_AnUpdateCarriesNoDescriptiveMember(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	f.describe(a, "Title", "Description", goodRights, "Statement")
	f.announceTo(a, f.peer, "sent")
	ids, err := f.changeFlags(a, true, "assisted", nil)
	if err != nil || len(ids) != 1 {
		t.Fatalf("the change: %v %v", ids, err)
	}
	u := f.withdrawalOf(ids[0])
	raw, err := f.flagMember(u, f.peer)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"description", "rights", "requiredStatement", "identifiers", "accessionNumber", "iiifManifest"} {
		if _, ok := wire[k]; ok {
			t.Fatalf("the Update carries %s: %s", k, raw)
		}
	}
	if _, ok := u.payload["descriptive"]; ok {
		t.Fatalf("the Update's record carries a snapshot: %v", u.payload)
	}
}

// pgx import kept for the Tx type used by helpers in this package.
var _ pgx.Tx
