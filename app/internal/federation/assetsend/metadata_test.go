// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 against a live database: the sender's aa:metadata capture and
// re-check (metadata.go), through CaptureMetadata and
// Source.MetadataMember, with the real seeded definitions and the shipped
// mirror bindings. Nothing here records an activity or sends anything:
// the primitives are not wired to any caller yet. Skips without
// AA_DB_PASSWORD.
//
// Like descriptive_test.go, these tests change the seeded rights
// definitions and the shipped title / description mirror bindings, which
// every test in the database shares; each change is undone in a cleanup.

package assetsend_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
)

const fourNull = `{"title":null,"description":null,"rights":null,"requiredStatement":null}`

func sp(s string) *string { return &s }

// sub is f for subtest t (its helpers fail and clean up on t).
func (f *fx) sub(t *testing.T) *fx {
	c := *f
	c.t = t
	return &c
}

// captureErr captures a's metadata in a transaction that holds a's row,
// as a caller must.
func (f *fx) captureErr(ctx context.Context, a asset) (*assetsend.MetadataCapture, error) {
	var c *assetsend.MetadataCapture
	err := pgx.BeginFunc(context.Background(), f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `SELECT 1 FROM assets WHERE id = $1 FOR UPDATE`, a.id); err != nil {
			return err
		}
		var err error
		c, err = assetsend.CaptureMetadata(ctx, tx, a.id)
		return err
	})
	return c, err
}

func (f *fx) captureNow(a asset) *assetsend.MetadataCapture {
	f.t.Helper()
	c, err := f.captureErr(context.Background(), a)
	if err != nil {
		f.t.Fatalf("capture: %v", err)
	}
	return c
}

// recordOf is c as a local record keeps it.
func (f *fx) recordOf(c *assetsend.MetadataCapture) json.RawMessage {
	f.t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fx) metadataMember(a asset, record json.RawMessage) (json.RawMessage, error) {
	return f.src.MetadataMember(context.Background(), a.id, record)
}

// wire is the standard encoding of a snapshot: what the member must be.
func wire(t *testing.T, s federation.AssetMetadataSnapshot) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func statementOf(v string) *federation.ArchivalLabelValue {
	return &federation.ArchivalLabelValue{Label: map[string][]string{"en": {"Attribution"}}, Value: map[string][]string{"none": {v}}}
}

func wantOmitted(t *testing.T, c *assetsend.MetadataCapture, want map[string]string) {
	t.Helper()
	got := c.Omitted
	if got == nil {
		got = map[string]string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("omitted %v, want %v", got, want)
	}
}

// All four claims, exactly as stored: Unicode, RTL, tab, line feed and
// leading and trailing spaces kept, the statement in its wire shape, and
// a member of exactly the four keys with no local field.
func TestMetadataCapture_AllFourClaimsExactly(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	title := "  Café\t東京 ‏شعار  "
	desc := "line one\n\tline two  \n"
	stmt := " Studio A\n© 2026 "
	f.describe(a, title, desc, goodRights, stmt)

	c := f.captureNow(a)
	want := federation.AssetMetadataSnapshot{Title: &title, Description: &desc, Rights: sp(goodRights), RequiredStatement: statementOf(stmt)}
	if c.V != 1 || !reflect.DeepEqual(c.Snapshot, want) {
		t.Fatalf("capture %+v, want %+v", c, want)
	}
	wantOmitted(t, c, map[string]string{})

	raw, err := f.metadataMember(a, f.recordOf(c))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, wire(t, want)) {
		t.Fatalf("member %s", raw)
	}
	parsed, err := federation.ParseAssetMetadata(raw)
	if err != nil || !reflect.DeepEqual(*parsed, want) {
		t.Fatalf("the member is not the snapshot: %+v %v", parsed, err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(func(yield func(string) bool) {
		for k := range keys {
			if !yield(k) {
				return
			}
		}
	})
	if !slices.Equal(got, []string{"description", "requiredStatement", "rights", "title"}) {
		t.Fatalf("member keys %v", got)
	}
}

// A claim that is absent or withheld is null with a local reason; the
// reasons never reach the member, and four nulls are a valid member.
func TestMetadataCapture_NullsAndLocalReasons(t *testing.T) {
	f := newFx(t)

	mixed := f.newAsset("public", true)
	f.describe(mixed, "Only a title", "", "", "")
	c := f.captureNow(mixed)
	if c.Snapshot.Title == nil || *c.Snapshot.Title != "Only a title" || c.Snapshot.Description != nil || c.Snapshot.Rights != nil ||
		c.Snapshot.RequiredStatement != nil {
		t.Fatalf("mixed capture %+v", c.Snapshot)
	}
	wantOmitted(t, c, map[string]string{"description": "empty", "rights": "empty", "requiredStatement": "empty"})
	raw, err := f.metadataMember(mixed, f.recordOf(c))
	if err != nil || string(raw) != `{"title":"Only a title","description":null,"rights":null,"requiredStatement":null}` {
		t.Fatalf("mixed member %s %v", raw, err)
	}

	empty := f.newAsset("public", true)
	f.describe(empty, "", "", "", "")
	c = f.captureNow(empty)
	wantOmitted(t, c, map[string]string{"title": "empty", "description": "empty", "rights": "empty", "requiredStatement": "empty"})
	raw, err = f.metadataMember(empty, f.recordOf(c))
	if err != nil || string(raw) != fourNull {
		t.Fatalf("four-null member %s %v", raw, err)
	}
	if _, err := federation.ParseAssetMetadata(raw); err != nil {
		t.Fatal(err)
	}

	f.extraMirror(t, "description", "fields.admin")
	gated := f.newAsset("public", true)
	f.describe(gated, "Title", "Withheld body", goodRights, "Statement")
	c = f.captureNow(gated)
	wantOmitted(t, c, map[string]string{"description": "gated"})
	raw, err = f.metadataMember(gated, f.recordOf(c))
	if err != nil || bytes.Contains(raw, []byte("Withheld")) || bytes.Contains(raw, []byte("gated")) || bytes.Contains(raw, []byte("omitted")) {
		t.Fatalf("gated member %s %v", raw, err)
	}
}

// The per-claim limits count bytes, a value at the limit is kept whole, one
// byte over is omitted (never truncated), and a value the wire rules refuse
// is omitted. Invalid UTF-8 cannot reach a capture: the database refuses it.
func TestMetadataCapture_LimitsAndInvalidValues(t *testing.T) {
	f := newFx(t)
	rights := func(n int) string { // a pattern-valid CC URI of n bytes
		v := "https://creativecommons.org/licenses/by/4" + strings.Repeat("0", n-42) + "/"
		if len(v) != n || !federation.ArchivalRightsURIValid(v) {
			t.Fatalf("rights fixture %d bytes", len(v))
		}
		return v
	}
	cases := []struct {
		name                 string
		title, desc, r, stmt string
		member, reason       string // the claim checked; reason "" = kept
	}{
		{"a 1,024-byte multibyte title", strings.Repeat("é", 512), "", "", "", "title", ""},
		{"a 1,025-byte title", strings.Repeat("é", 512) + "x", "", "", "", "title", "over_limit"},
		{"a 4,096-byte description", "", strings.Repeat("d", 4096), "", "", "description", ""},
		{"a 4,097-byte description", "", strings.Repeat("d", 4097), "", "", "description", "over_limit"},
		{"a 2,048-byte rights URI", "", "", rights(2048), "", "rights", ""},
		{"a 2,049-byte rights URI", "", "", rights(2049), "", "rights", "over_limit"},
		{"a 4,096-byte statement", "", "", "", strings.Repeat("s", 4096), "requiredStatement", ""},
		{"a 4,097-byte statement", "", "", "", strings.Repeat("s", 4097), "requiredStatement", "over_limit"},
		{"a control character in the title", "a\x01b", "", "", "", "title", "invalid"},
		{"a control character in the statement", "", "", "", "a\x1bb", "requiredStatement", "invalid"},
		{"a rights URI with a fragment", "", "", goodRights + "#x", "", "rights", "invalid"},
		{"a script URI as rights", "", "", "javascript:alert(1)", "", "rights", "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := f.sub(t)
			a := f.newAsset("public", true)
			f.describe(a, tc.title, tc.desc, tc.r, tc.stmt)
			c := f.captureNow(a)
			if got := c.Omitted[tc.member]; got != tc.reason {
				t.Fatalf("%s omitted as %q, want %q", tc.member, got, tc.reason)
			}
			kept := map[string]bool{
				"title":             c.Snapshot.Title != nil && *c.Snapshot.Title == tc.title,
				"description":       c.Snapshot.Description != nil && *c.Snapshot.Description == tc.desc,
				"rights":            c.Snapshot.Rights != nil && *c.Snapshot.Rights == tc.r,
				"requiredStatement": reflect.DeepEqual(c.Snapshot.RequiredStatement, statementOf(tc.stmt)),
			}[tc.member]
			if kept != (tc.reason == "") {
				t.Fatalf("%s kept=%v: %+v", tc.member, kept, c.Snapshot)
			}
			if raw, err := f.metadataMember(a, f.recordOf(c)); err != nil {
				t.Fatal(err)
			} else if _, err := federation.ParseAssetMetadata(raw); err != nil {
				t.Fatalf("member: %v", err)
			}
		})
	}
	a := f.newAsset("public", true)
	if _, err := f.pool.Exec(context.Background(), `UPDATE assets SET title = $2 WHERE id = $1`, a.id, "bad \xff\xfe"); err == nil {
		t.Fatal("the database stored invalid UTF-8")
	}
}

// The peer emission policy of an announcement holds for a capture: the
// owner's capabilities are not the peer's, and only the seeded rows are
// sources, under every gate.
func TestMetadataCapture_ThePeerPolicy(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	var role uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO roles (name) VALUES ($1) RETURNING id`, "as-role-"+uuid.NewString()[:8]).Scan(&role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM roles WHERE id = $1`, role) })
	f.exec(`INSERT INTO role_capabilities (role_id, capability_code) VALUES ($1, 'system.admin'), ($1, 'fields.admin')`, role)
	f.exec(`INSERT INTO user_roles (user_ref, role_id) VALUES ($1, $2)`, f.owner, role)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM user_roles WHERE user_ref = $1`, f.owner)
	})

	steps := []struct {
		name  string
		setup func(f *fx, t *testing.T)
		want  map[string]string
	}{
		{"the seeded rights field gated by a capability the owner holds", func(f *fx, t *testing.T) {
			f.restoreDefinition(t, seedRights)
			f.exec(`UPDATE field_definition SET read_capability = 'fields.admin' WHERE id = $1`, seedRights)
		}, map[string]string{"rights": "gated"}},
		{"the seeded statement retired", func(f *fx, t *testing.T) {
			f.restoreDefinition(t, seedStatement)
			f.exec(`UPDATE field_definition SET status = 'archived' WHERE id = $1`, seedStatement)
		}, map[string]string{"requiredStatement": "inactive"}},
		{"the seeded rights field retyped", func(f *fx, t *testing.T) {
			f.restoreDefinition(t, seedRights)
			f.exec(`UPDATE field_definition SET type = 'longtext' WHERE id = $1`, seedRights)
		}, map[string]string{"rights": "wrong_type"}},
		{"the seeded statement not for this asset type", func(f *fx, t *testing.T) {
			f.restoreDefinition(t, seedStatement)
			f.exec(`UPDATE field_definition SET applies_to = '{987654}' WHERE id = $1`, seedStatement)
		}, map[string]string{"requiredStatement": "not_applicable"}},
		{"the seeded statement turned into a mirror", func(f *fx, t *testing.T) {
			f.restoreDefinition(t, seedStatement)
			f.exec(`UPDATE field_definition SET mirrors_column = 'description' WHERE id = $1`, seedStatement)
		}, map[string]string{"requiredStatement": "mirrored"}},
		{"another row holds the seeded rights code", func(f *fx, t *testing.T) {
			f.restoreDefinition(t, seedRights)
			f.exec(`UPDATE field_definition SET code = $2 WHERE id = $1`, seedRights, "rights_uri_moved_"+uuid.NewString()[:8])
		}, map[string]string{"rights": "unmapped"}},
		{"the shipped title mirror gated", func(f *fx, t *testing.T) {
			id := f.mirrorOf("title")
			f.restoreDefinition(t, id)
			f.exec(`UPDATE field_definition SET read_capability = 'system.admin' WHERE id = $1`, id)
		}, map[string]string{"title": "gated"}},
		{"another, archived, gated mirror of the description", func(f *fx, t *testing.T) {
			f.extraMirror(t, "description", "fields.admin")
		}, map[string]string{"description": "gated"}},
		{"an ungated extra mirror changes nothing", func(f *fx, t *testing.T) {
			f.extraMirror(t, "title", "")
		}, map[string]string{}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			f := f.sub(t)
			s.setup(f, t)
			a := f.newAsset("team", true)
			stmt := "Statement"
			if s.want["requiredStatement"] == "mirrored" {
				stmt = "" // a mirrored field can hold no value (00044)
			}
			f.describe(a, "Title", "Description", goodRights, stmt)
			c := f.captureNow(a)
			wantOmitted(t, c, s.want)
			held := map[string]bool{
				"title": c.Snapshot.Title != nil, "description": c.Snapshot.Description != nil,
				"rights": c.Snapshot.Rights != nil, "requiredStatement": c.Snapshot.RequiredStatement != nil,
			}
			for member, h := range held {
				if _, withheld := s.want[member]; h == withheld {
					t.Fatalf("%s held=%v with omitted %v", member, h, s.want)
				}
			}
		})
	}
}

// Values valid one by one can exceed the member budget together once the
// standard encoding escapes them. Such a capture clears every claim it held
// (member_over_limit) and keeps the reasons of the claims already omitted.
func TestMetadataCapture_OverTheMemberBudgetClearsEveryClaim(t *testing.T) {
	f := newFx(t)
	title, desc, stmt := strings.Repeat("<", 1024), strings.Repeat("<", 4096), strings.Repeat("&", 4096)
	if federation.ValidateArchivalDescription(federation.ArchivalDescription{Title: title, Description: desc}) != nil ||
		federation.ValidateArchivalRequiredStatement(*statementOf(stmt)) != nil {
		t.Fatal("the values are not valid one by one")
	}
	candidate := wire(t, federation.AssetMetadataSnapshot{Title: &title, Description: &desc, RequiredStatement: statementOf(stmt)})
	if len(candidate) <= federation.AssetMetadataMaxMemberBytes {
		t.Fatalf("candidate %d bytes", len(candidate))
	}
	if _, err := federation.ParseAssetMetadata(candidate); !errors.Is(err, federation.ErrMetadataInvalid) {
		t.Fatalf("the escaped candidate as a member: %v", err)
	}

	a := f.newAsset("public", true)
	f.describe(a, title, desc, "", stmt)
	c := f.captureNow(a)
	if !reflect.DeepEqual(c.Snapshot, federation.AssetMetadataSnapshot{}) {
		t.Fatalf("snapshot %+v", c.Snapshot)
	}
	wantOmitted(t, c, map[string]string{"title": "member_over_limit", "description": "member_over_limit",
		"requiredStatement": "member_over_limit", "rights": "empty"})
	for _, r := range c.Omitted {
		if strings.ContainsAny(r, "<&") {
			t.Fatalf("a reason carries a value: %q", r)
		}
	}
	if raw, err := f.metadataMember(a, f.recordOf(c)); err != nil || string(raw) != fourNull {
		t.Fatalf("member %s %v", raw, err)
	}

	// A claim omitted for its own reason keeps it.
	f.extraMirror(t, "title", "fields.admin")
	b := f.newAsset("public", true)
	f.describe(b, title, desc, goodRights, stmt)
	c = f.captureNow(b)
	wantOmitted(t, c, map[string]string{"title": "gated", "description": "member_over_limit",
		"requiredStatement": "member_over_limit", "rights": "member_over_limit"})
}

// A candidate of exactly the budget is captured whole, its last byte
// included; one byte more clears it. The member survives a later envelope's
// standard encoding unchanged.
func TestMetadataCapture_TheBudgetBoundary(t *testing.T) {
	f := newFx(t)
	title, desc := strings.Repeat("<", 1024), strings.Repeat("<", 4096)
	size := func(stmt string) int {
		return len(wire(t, federation.AssetMetadataSnapshot{Title: &title, Description: &desc, RequiredStatement: statementOf(stmt)}))
	}
	k := federation.AssetMetadataMaxMemberBytes - size("Z")
	stmt := strings.Repeat("a", k) + "Z"
	if size(stmt) != federation.AssetMetadataMaxMemberBytes || len(stmt) > 4096 {
		t.Fatalf("boundary fixture: %d bytes, statement %d", size(stmt), len(stmt))
	}

	a := f.newAsset("public", true)
	f.describe(a, title, desc, "", stmt)
	c := f.captureNow(a)
	if c.Snapshot.RequiredStatement == nil || c.Snapshot.RequiredStatement.Value["none"][0] != stmt ||
		c.Snapshot.Title == nil || *c.Snapshot.Title != title || c.Snapshot.Description == nil || *c.Snapshot.Description != desc {
		t.Fatalf("the boundary capture is not whole: %v", c.Omitted)
	}
	raw, err := f.metadataMember(a, f.recordOf(c))
	if err != nil || len(raw) != federation.AssetMetadataMaxMemberBytes || !bytes.HasSuffix(raw, []byte(`aZ"]}}}`)) {
		t.Fatalf("member %d bytes %v", len(raw), err)
	}
	if _, err := federation.ParseAssetMetadata(raw); err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(map[string]json.RawMessage{"aa:metadata": raw})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal(env, &back); err != nil || !bytes.Equal(back["aa:metadata"], raw) {
		t.Fatalf("the member changed inside an envelope: %v", err)
	}
	if _, err := federation.ParseAssetMetadata(back["aa:metadata"]); err != nil {
		t.Fatal(err)
	}

	over := f.newAsset("public", true)
	f.describe(over, title, desc, "", "a"+stmt)
	c = f.captureNow(over)
	if !reflect.DeepEqual(c.Snapshot, federation.AssetMetadataSnapshot{}) || c.Omitted["requiredStatement"] != "member_over_limit" {
		t.Fatalf("one byte over: %+v %v", c.Snapshot, c.Omitted)
	}
}

// The recorded bytes are what every re-check sends: a later value edit, an
// invalid new value, a value set where null was recorded, the record kept as
// jsonb and a fresh Source change nothing.
func TestMetadataMember_TheRecordedBytesAreFrozen(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	f.describe(a, "Old title", "Old body", "", "Old statement")
	c := f.captureNow(a)
	want := wire(t, c.Snapshot)
	record := f.recordOf(c)
	var stored string
	if err := f.pool.QueryRow(context.Background(), `SELECT $1::jsonb::text`, string(record)).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	f.describe(a, "New title", "", goodRights, "")
	f.setText(a, seedStatement, "new\x01statement")
	fresh := assetsend.Source{Pool: f.pool}
	for name, r := range map[string]json.RawMessage{"the record": record, "the record as jsonb": json.RawMessage(stored)} {
		got, err := fresh.MetadataMember(context.Background(), a.id, r)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %s %v, want %s", name, got, err, want)
		}
	}
	if !bytes.Contains(want, []byte(`"rights":null`)) {
		t.Fatalf("the recorded null: %s", want)
	}
}

// A source no longer shareable refuses the whole member, naming the claim
// and no value. A claim recorded as null is not re-checked, and lifting a
// restriction that withheld a claim at capture does not restore it.
func TestMetadataMember_ARestrictionRefusesTheWholeMember(t *testing.T) {
	f := newFx(t)
	secrets := []string{"Secret title", "Secret body", "Secret statement"}
	steps := []struct {
		name   string
		member string
		change func(f *fx, a asset, t *testing.T)
	}{
		{"rights gated", "rights", func(f *fx, _ asset, t *testing.T) {
			f.restoreDefinition(t, seedRights)
			f.exec(`UPDATE field_definition SET read_capability = 'fields.admin' WHERE id = $1`, seedRights)
		}},
		{"statement retired", "requiredStatement", func(f *fx, _ asset, t *testing.T) {
			f.restoreDefinition(t, seedStatement)
			f.exec(`UPDATE field_definition SET status = 'deprecated' WHERE id = $1`, seedStatement)
		}},
		{"rights retyped", "rights", func(f *fx, _ asset, t *testing.T) {
			f.restoreDefinition(t, seedRights)
			f.exec(`UPDATE field_definition SET type = 'longtext' WHERE id = $1`, seedRights)
		}},
		{"statement no longer applicable", "requiredStatement", func(f *fx, _ asset, t *testing.T) {
			f.restoreDefinition(t, seedStatement)
			f.exec(`UPDATE field_definition SET applies_to = '{987654}' WHERE id = $1`, seedStatement)
		}},
		// A field with values cannot be declared a mirror (00044), so the
		// value is cleared first: the recorded claim stays recorded.
		{"statement value cleared, then turned into a mirror", "requiredStatement", func(f *fx, a asset, t *testing.T) {
			f.restoreDefinition(t, seedStatement)
			f.exec(`DELETE FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`, a.id, seedStatement)
			f.exec(`UPDATE field_definition SET mirrors_column = 'description' WHERE id = $1`, seedStatement)
		}},
		{"the seeded rights code moved", "rights", func(f *fx, _ asset, t *testing.T) {
			f.restoreDefinition(t, seedRights)
			f.exec(`UPDATE field_definition SET code = $2 WHERE id = $1`, seedRights, "rights_uri_moved_"+uuid.NewString()[:8])
		}},
		{"the shipped title mirror gated", "title", func(f *fx, _ asset, t *testing.T) {
			id := f.mirrorOf("title")
			f.restoreDefinition(t, id)
			f.exec(`UPDATE field_definition SET read_capability = 'system.admin' WHERE id = $1`, id)
		}},
		{"a gated description mirror added", "description", func(f *fx, _ asset, t *testing.T) {
			f.extraMirror(t, "description", "fields.admin")
		}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			f := f.sub(t)
			a := f.newAsset("public", true)
			f.describe(a, secrets[0], secrets[1], goodRights, secrets[2])
			c := f.captureNow(a)
			record := f.recordOf(c)
			s.change(f, a, t)
			raw, err := f.metadataMember(a, record)
			if !errors.Is(err, federation.ErrNotFederated) || raw != nil {
				t.Fatalf("not refused: %s %v", raw, err)
			}
			if !strings.Contains(err.Error(), "recorded "+s.member+" ") {
				t.Fatalf("the refusal does not name %s: %v", s.member, err)
			}
			for _, v := range append(secrets, goodRights) {
				if strings.Contains(err.Error(), v) {
					t.Fatalf("the refusal carries a value: %v", err)
				}
			}
		})
	}

	t.Run("a claim recorded as null is not re-checked", func(t *testing.T) {
		f := f.sub(t)
		a := f.newAsset("public", true)
		f.describe(a, "Title", "Body", "", "Statement")
		record := f.recordOf(f.captureNow(a))
		f.restoreDefinition(t, seedRights)
		f.exec(`UPDATE field_definition SET read_capability = 'fields.admin' WHERE id = $1`, seedRights)
		raw, err := f.metadataMember(a, record)
		if err != nil || !bytes.Contains(raw, []byte(`"rights":null`)) {
			t.Fatalf("%s %v", raw, err)
		}
	})

	t.Run("lifting a restriction does not restore a withheld claim", func(t *testing.T) {
		f := f.sub(t)
		f.restoreDefinition(t, seedRights)
		f.exec(`UPDATE field_definition SET read_capability = 'fields.admin' WHERE id = $1`, seedRights)
		a := f.newAsset("public", true)
		f.describe(a, "Title", "Body", goodRights, "Statement")
		c := f.captureNow(a)
		if c.Snapshot.Rights != nil || c.Omitted["rights"] != "gated" {
			t.Fatalf("capture %+v %v", c.Snapshot, c.Omitted)
		}
		f.exec(`UPDATE field_definition SET read_capability = NULL WHERE id = $1`, seedRights)
		raw, err := f.metadataMember(a, f.recordOf(c))
		if err != nil || !bytes.Contains(raw, []byte(`"rights":null`)) || bytes.Contains(raw, []byte(goodRights)) {
			t.Fatalf("%s %v", raw, err)
		}
	})
}

// Anything but a valid capture of this version is refused for good, without
// its values; a missing asset is refused; a read error is an error, never an
// empty member.
func TestMetadataMember_FailsClosed(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	f.describe(a, "Secret title", "", goodRights, "")
	good := f.captureNow(a)
	snap := string(wire(t, good.Snapshot))
	records := map[string]string{
		"an unknown version":             `{"v":2,"snapshot":` + snap + `}`,
		"no version":                     `{"snapshot":` + snap + `}`,
		"no snapshot":                    `{"v":1}`,
		"broken JSON":                    `{"v":1,"snapshot":` + snap,
		"a trailing value":               `{"v":1,"snapshot":` + snap + `}{}`,
		"an unknown field":               `{"v":1,"snapshot":` + snap + `,"extra":true}`,
		"a non-string reason":            `{"v":1,"snapshot":` + snap + `,"omitted":{"rights":1}}`,
		"a snapshot without rights":      `{"v":1,"snapshot":{"title":"Secret title","description":null,"requiredStatement":null}}`,
		"a statement of the wrong shape": `{"v":1,"snapshot":{"title":null,"description":null,"rights":null,"requiredStatement":"Secret title"}}`,
		"an oversized snapshot": `{"v":1,"snapshot":{"title":"` + strings.Repeat("x", 40000) +
			`","description":null,"rights":null,"requiredStatement":null}}`,
		"a snapshot that is not an object": `{"v":1,"snapshot":"Secret title"}`,
	}
	for name, r := range records {
		raw, err := f.metadataMember(a, json.RawMessage(r))
		if !errors.Is(err, federation.ErrNotFederated) || raw != nil {
			t.Errorf("%s: %s %v", name, raw, err)
			continue
		}
		if strings.Contains(err.Error(), "Secret") || strings.Contains(err.Error(), "xxxx") {
			t.Errorf("%s: the refusal carries a value: %v", name, err)
		}
	}

	record := f.recordOf(good)
	if raw, err := f.src.MetadataMember(context.Background(), uuid.New(), record); !errors.Is(err, federation.ErrNotFederated) || raw != nil ||
		!strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("a missing asset: %s %v", raw, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if raw, err := f.src.MetadataMember(ctx, a.id, record); err == nil || errors.Is(err, federation.ErrNotFederated) || raw != nil {
		t.Fatalf("a read error: %s %v", raw, err)
	}

	if c, err := f.captureErr(context.Background(), asset{id: uuid.New()}); !errors.Is(err, pgx.ErrNoRows) || c != nil {
		t.Fatalf("capturing a missing asset: %+v %v", c, err)
	}
	if c, err := f.captureErr(ctx, a); err == nil || c != nil {
		t.Fatalf("a capture read error: %+v %v", c, err)
	}
}

// A capture and a re-check write nothing and change neither their input nor
// the capture; a capture reads the caller's transaction, and a rolled-back
// change leaves no trace.
func TestMetadataCapture_ReadsOnlyInTheCallersTransaction(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	counts := func() string {
		t.Helper()
		var activities, outbox, values int
		if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activities), (SELECT count(*) FROM federation_outbox),
			(SELECT count(*) FROM asset_field_value)`).Scan(&activities, &outbox, &values); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(activities, outbox, values)
	}
	a := f.newAsset("public", true)
	f.describe(a, "Title", "Body", goodRights, "Statement")
	before := counts()
	c := f.captureNow(a)
	keep, _ := json.Marshal(c)
	record := f.recordOf(c)
	input := bytes.Clone(record)
	if _, err := f.metadataMember(a, record); err != nil {
		t.Fatal(err)
	}
	if after := counts(); after != before {
		t.Fatalf("rows changed: %s -> %s", before, after)
	}
	if !bytes.Equal(record, input) {
		t.Fatal("the record was modified")
	}
	if now, _ := json.Marshal(c); !bytes.Equal(now, keep) {
		t.Fatal("the capture was modified")
	}

	f.restoreDefinition(t, seedRights)
	rollback := errors.New("roll back")
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			`SELECT 1 FROM assets WHERE id = $1 FOR UPDATE`,
			`UPDATE assets SET title = 'Inside the transaction' WHERE id = $1`,
			`UPDATE field_definition SET read_capability = 'fields.admin' WHERE id = '` + seedRights.String() + `' AND $1::uuid IS NOT NULL`,
		} {
			if _, err := tx.Exec(ctx, q, a.id); err != nil {
				return err
			}
		}
		c, err := assetsend.CaptureMetadata(ctx, tx, a.id)
		if err != nil {
			return err
		}
		if c.Snapshot.Title == nil || *c.Snapshot.Title != "Inside the transaction" || c.Snapshot.Rights != nil || c.Omitted["rights"] != "gated" {
			return fmt.Errorf("the capture did not read its transaction: %+v %v", c.Snapshot, c.Omitted)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	c = f.captureNow(a)
	if c.Snapshot.Title == nil || *c.Snapshot.Title != "Title" || c.Snapshot.Rights == nil || *c.Snapshot.Rights != goodRights {
		t.Fatalf("after the rollback: %+v %v", c.Snapshot, c.Omitted)
	}
}

// A recorded capture is exactly one strict JSON value: nothing after it but
// whitespace, no repeated name, and exactly the lower-case keys v,
// snapshot and an optional object of reasons. Anything else is refused
// for good, without its values; trailing whitespace is accepted.
func TestMetadataMember_TheRecordIsOneStrictValue(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	f.describe(a, "Secret title", "", goodRights, "")
	c := f.captureNow(a)
	valid := string(f.recordOf(c))
	snap := string(wire(t, c.Snapshot))
	want := wire(t, c.Snapshot)

	for name, r := range map[string]string{
		"trailing spaces and newlines": valid + " \n\t\r ",
		"leading whitespace":           "\n  " + valid,
	} {
		raw, err := f.metadataMember(a, json.RawMessage(r))
		if err != nil || !bytes.Equal(raw, want) {
			t.Errorf("%s: %s %v", name, raw, err)
		}
	}

	refusedRecords := map[string]string{
		"an unmatched ]":                      valid + "]",
		"an unmatched }":                      valid + "}",
		"whitespace, then an unmatched ]":     valid + " \n]",
		"a trailing true":                     valid + "true",
		"a trailing string":                   valid + `"x"`,
		"a trailing object":                   valid + `{}`,
		"trailing invalid bytes":              valid + "\xff\xfe",
		"trailing text":                       valid + "Secret",
		"v twice, 2 then 1":                   `{"v":2,"v":1,"snapshot":` + snap + `}`,
		"v twice, 1 then 2":                   `{"v":1,"v":2,"snapshot":` + snap + `}`,
		"snapshot twice, different values":    `{"v":1,"snapshot":` + snap + `,"snapshot":` + fourNull + `}`,
		"omitted twice":                       `{"v":1,"snapshot":` + snap + `,"omitted":{},"omitted":{"title":"empty"}}`,
		"a mis-cased V":                       `{"V":1,"snapshot":` + snap + `}`,
		"a mis-cased Snapshot":                `{"v":1,"Snapshot":` + snap + `}`,
		"a mis-cased Omitted":                 `{"v":1,"snapshot":` + snap + `,"Omitted":{}}`,
		"omitted as null":                     `{"v":1,"snapshot":` + snap + `,"omitted":null}`,
		"v as a string":                       `{"v":"1","snapshot":` + snap + `}`,
		"v as a fraction":                     `{"v":1.0,"snapshot":` + snap + `}`,
		"a repeated name inside the snapshot": `{"v":1,"snapshot":{"title":"Secret title","title":null,"description":null,"rights":null,"requiredStatement":null}}`,
		"a top-level array":                   `[` + valid + `]`,
		"a top-level null":                    `null`,
	}
	for name, r := range refusedRecords {
		raw, err := f.metadataMember(a, json.RawMessage(r))
		if !errors.Is(err, federation.ErrNotFederated) || raw != nil {
			t.Errorf("%s: not refused: %s %v", name, raw, err)
			continue
		}
		if strings.Contains(err.Error(), "Secret") || strings.Contains(err.Error(), "creativecommons") {
			t.Errorf("%s: the refusal carries a value: %v", name, err)
		}
	}

	// The size bound is the recorded snapshot's own bytes: escaped or padded
	// past 32,768, it is refused even though its canonical form is smaller.
	esc := strings.Repeat(`\u003c`, 1024)  // title: 1,024 '<' as 6,144 bytes
	desc := strings.Repeat(`\u0064`, 4096) // description: 4,096 'd' as 24,576 bytes
	big := `{"title":"` + esc + `","description":"` + desc + `","rights":null,"requiredStatement":{"label":{"en":["Attribution"]},"value":{"none":["` +
		strings.Repeat("s", 2100) + `"]}}}`
	canon, err := federation.Canonicalize([]byte(big))
	if err != nil || len(big) <= federation.AssetMetadataMaxMemberBytes || len(canon) > federation.AssetMetadataMaxMemberBytes {
		t.Fatalf("fixture: raw %d, canonical %d, %v", len(big), len(canon), err)
	}
	padded := `{"title":null,` + strings.Repeat(" ", federation.AssetMetadataMaxMemberBytes) + `"description":null,"rights":null,"requiredStatement":null}`
	for name, s := range map[string]string{"escaped past the bound": big, "padded past the bound": padded} {
		raw, err := f.metadataMember(a, json.RawMessage(`{"v":1,"snapshot":`+s+`}`))
		if !errors.Is(err, federation.ErrNotFederated) || raw != nil {
			t.Errorf("%s: not refused: %d bytes %v", name, len(raw), err)
		}
	}
}

// A valid four-null capture names no source, so nothing is read: it is
// returned as recorded whatever the asset id, even by a Source with no
// pool or a cancelled context. This is source privacy only; it says
// nothing about the asset, a grant, a profile or a tier, which a driver
// checks for a clear as for any other Update.
func TestMetadataMember_AFourNullRecordReadsNothing(t *testing.T) {
	record := json.RawMessage(`{"v":1,"snapshot":` + fourNull + `,"omitted":{"title":"empty","rights":"gated"}}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, c := range map[string]context.Context{"a live context": context.Background(), "a cancelled context": ctx} {
		raw, err := (assetsend.Source{}).MetadataMember(c, uuid.New(), record)
		if err != nil || string(raw) != fourNull {
			t.Fatalf("%s: %s %v", name, raw, err)
		}
	}
}
