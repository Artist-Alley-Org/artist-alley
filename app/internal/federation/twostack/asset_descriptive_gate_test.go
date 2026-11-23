// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 S1 gate: the baseline descriptive and rights members travel from
// A's real sender to B's real inbox, record and read API, on the grant
// path and on a later sealed version, under the peer emission policy
// (assetsend/descriptive.go). B treats them as untrusted declared claims:
// it changes no local definition, value or permission, and needs no
// matching field of its own.
//
// Test-only, labelled where installed: TEST-ONLY ADVERTISEMENT of
// archival-1 (archivalGate) and original-transfer-1 (newCASGate). Neither
// is advertised in production. Definition changes on A and B are fixture
// writes, undone after the test.

package twostack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

var (
	seedRightsDef    = uuid.MustParse("56d71791-189d-429c-b7e4-e837baf91ddb")
	seedStatementDef = uuid.MustParse("6c3e25be-83ed-4762-b636-a1e37cdbb74c")
)

// restoreDef puts a definition on pool back as it is now, after t.
func restoreDef(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	var row []byte
	if err := pool.QueryRow(context.Background(), `SELECT to_jsonb(d) FROM field_definition d WHERE id = $1`, id).Scan(&row); err != nil {
		t.Fatalf("snapshot definition %s: %v", id, err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			UPDATE field_definition d SET code = r.code, status = r.status, read_capability = r.read_capability
			  FROM jsonb_populate_record(NULL::field_definition, $2::jsonb) r WHERE d.id = $1`, id, row); err != nil {
			t.Errorf("restore definition %s: %v", id, err)
		}
	})
}

// fieldsDigest digests every definition and asset value row on pool.
func fieldsDigest(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT concat_ws('|',
		(SELECT md5(string_agg(to_jsonb(d)::text, ',' ORDER BY d.id)) FROM field_definition d),
		(SELECT coalesce(md5(string_agg(to_jsonb(v)::text, ',' ORDER BY v.asset_id, v.field_id)), '') FROM asset_field_value v),
		(SELECT coalesce(md5(string_agg(to_jsonb(c)::text, ',' ORDER BY c.role_id, c.capability_code)), '') FROM role_capabilities c),
		(SELECT coalesce(md5(string_agg(to_jsonb(u)::text, ',' ORDER BY u.user_ref, u.role_id)), '') FROM user_roles u))`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTwoStack_BaselineDescriptiveMembers(t *testing.T) {
	g := archivalGate(t)
	g.runWorkers(g.a)
	g.runWorkers(g.b)
	ctx := context.Background()
	member := g.memberOnB()

	// B's schema differs: it has no rights_uri of its own at all.
	restoreDef(t, g.b.pool, seedRightsDef)
	if _, err := g.b.pool.Exec(ctx, `UPDATE field_definition SET code = 'b_local_' || left(md5(random()::text), 8), status = 'archived' WHERE id = $1`,
		seedRightsDef); err != nil {
		t.Fatal(err)
	}
	beforeB := fieldsDigest(t, g.b.pool)

	const rights = "https://creativecommons.org/licenses/by-sa/4.0/"
	title := `<img src=x onerror="alert(1)"> Harbour at dusk`
	desc := "<script>alert(1)</script>\njavascript:alert(2) https://evil.example/x"
	statement := `<a href="javascript:alert(3)">Studio A</a>`
	describeOnA := func(o original, title, desc, rights, statement string) {
		t.Helper()
		if _, err := g.a.pool.Exec(ctx, `UPDATE assets SET title = $2, description = $3 WHERE id = $1`, o.asset, title, desc); err != nil {
			t.Fatal(err)
		}
		for field, v := range map[uuid.UUID]string{seedRightsDef: rights, seedStatementDef: statement} {
			if v == "" {
				continue
			}
			if _, err := g.a.pool.Exec(ctx, `INSERT INTO asset_field_value (asset_id, field_id, value_text) VALUES ($1, $2, $3)
				ON CONFLICT (asset_id, field_id) DO UPDATE SET value_text = EXCLUDED.value_text`, o.asset, field, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	// wireOnB is the aa:asset member B received for an announcement, as
	// signed by A (B's stored inbox envelope).
	wireOnB := func(ann string) federation.ArchivalAsset {
		t.Helper()
		var raw []byte
		if err := g.b.pool.QueryRow(ctx, `SELECT envelope_json->'aa:asset' FROM federation_inbox WHERE peer_id = $1 AND activity_uri = $2`,
			g.peerAonB, ann).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		m, err := federation.ParseArchivalAsset(raw)
		if err != nil {
			t.Fatalf("B holds a member §17.2 refuses: %v", err)
		}
		return *m
	}
	declared := func(id string) map[string]any {
		t.Helper()
		st, raw := g.getOnB(member, "/federation/received-assets/"+id)
		if st != http.StatusOK {
			t.Fatalf("detail: %d %s", st, raw)
		}
		var d struct {
			Declared    map[string]any `json:"declared"`
			ActivityURI string         `json:"activity_uri"`
			ActorURI    string         `json:"actor_uri"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		return d.Declared
	}

	var o original
	var ann1 string
	t.Run("the grant announces the eligible baseline, as plain claims", func(t *testing.T) {
		o = g.uploadAsset(pngBytes(t)) // a real image, so a version 2 can follow
		g.sealedOnA(o)
		describeOnA(o, title, desc, rights, statement)
		g.grantWith(o, map[string]any{"allow_original": true})
		ann := g.announcementsOnA(o)
		if len(ann) != 1 {
			t.Fatalf("A recorded %d announcements", len(ann))
		}
		ann1 = ann[0]
		g.recordStateOnB(ann1, "complete")

		w := wireOnB(ann1)
		if w.Description == nil || w.Description.Title != title || w.Description.Description != desc || w.Rights != rights ||
			w.RequiredStatement == nil || w.RequiredStatement.Value["none"][0] != statement || w.RequiredStatement.Label["en"][0] != "Attribution" {
			t.Fatalf("the signed member: %+v", w)
		}
		var record []byte
		var actor, signer string
		if err := g.b.pool.QueryRow(ctx, `SELECT record, actor_uri, signer_key_id FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2`,
			g.peerAonB, ann1).Scan(&record, &actor, &signer); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(record), `"rights": "`+rights) && !strings.Contains(string(record), `"rights":"`+rights) {
			t.Fatalf("B's record lacks the rights claim: %s", record)
		}
		if !strings.HasPrefix(actor, g.a.url+"/users/") || signer == "" {
			t.Fatalf("provenance: actor %q signer %q", actor, signer)
		}
		d := declared(g.receivedOnB(ann1))
		desc0, _ := d["description"].(map[string]any)
		rs, _ := d["requiredStatement"].(map[string]any)
		if d["rights"] != rights || desc0["title"] != title || desc0["description"] != desc || rs == nil {
			t.Fatalf("B's declared claims: %v", d)
		}
		if after := fieldsDigest(t, g.b.pool); after != beforeB {
			t.Fatal("receiving changed B's definitions, values or permissions")
		}
	})

	t.Run("a later sealed version carries its own capture", func(t *testing.T) {
		describeOnA(o, "Harbour, version two", "", rights, statement)
		body2 := pngBytes(t)
		hash2, receipt := g.uploadOnA(body2)
		if st := g.api(g.a, http.MethodPost, "/assets/"+o.asset.String()+"/versions", map[string]any{
			"file_hash": hash2, "upload_receipt": receipt, "expected_version": 1, "file_extension": "png"}, nil); st != http.StatusCreated {
			t.Fatalf("version 2 on A: %d", st)
		}
		g.sealedOnA(o)
		var ann2 string
		deadline := time.Now().Add(60 * time.Second)
		for ann2 == "" && time.Now().Before(deadline) {
			if ann := g.announcementsOnA(o); len(ann) == 2 {
				ann2 = ann[1]
			}
			time.Sleep(100 * time.Millisecond)
		}
		if ann2 == "" {
			t.Fatal("version 2 was not announced")
		}
		g.recordStateOnB(ann2, "complete")
		w := wireOnB(ann2)
		if w.Version.Number != 2 || w.Description == nil || w.Description.Title != "Harbour, version two" || w.Description.Description != "" {
			t.Fatalf("version 2's member: %+v", w.Description)
		}
		// Version 1's record on B keeps what was announced with it.
		if w1 := wireOnB(ann1); w1.Description.Title != title {
			t.Fatalf("version 1 changed on B: %+v", w1.Description)
		}
	})

	t.Run("a hidden field on A never reaches the wire", func(t *testing.T) {
		restoreDef(t, g.a.pool, seedRightsDef)
		restoreDef(t, g.a.pool, seedStatementDef)
		if _, err := g.a.pool.Exec(ctx, `UPDATE field_definition SET read_capability = 'system.admin' WHERE id = $1`, seedRightsDef); err != nil {
			t.Fatal(err)
		}
		if _, err := g.a.pool.Exec(ctx, `UPDATE field_definition SET status = 'archived' WHERE id = $1`, seedStatementDef); err != nil {
			t.Fatal(err)
		}
		o2 := g.newOriginal("team")
		g.sealedOnA(o2)
		describeOnA(o2, "Team title", "", rights, "Hidden statement")
		g.grantWith(o2, map[string]any{"allow_original": true})
		ann := g.announcementsOnA(o2)
		if len(ann) != 1 {
			t.Fatalf("A recorded %d announcements", len(ann))
		}
		g.recordStateOnB(ann[0], "complete")
		var env []byte
		if err := g.b.pool.QueryRow(ctx, `SELECT envelope_json::text FROM federation_inbox WHERE peer_id = $1 AND activity_uri = $2`,
			g.peerAonB, ann[0]).Scan(&env); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(env), rights) || strings.Contains(string(env), "Hidden statement") || strings.Contains(string(env), "requiredStatement") {
			t.Fatalf("a hidden value reached B: %s", env)
		}
		if w := wireOnB(ann[0]); w.Description == nil || w.Description.Title != "Team title" {
			t.Fatalf("the ungated title: %+v", w.Description)
		}
	})

	t.Run("a rights URI grants nothing: a view-only share announces nothing", func(t *testing.T) {
		o3 := g.newOriginal("public")
		g.sealedOnA(o3)
		describeOnA(o3, "View only", "", rights, statement)
		g.grantWith(o3, map[string]any{"allow_original": false})
		time.Sleep(500 * time.Millisecond)
		if n := len(g.announcementsOnA(o3)); n != 0 {
			t.Fatalf("A announced %d times under a view-only share", n)
		}
	})
}
