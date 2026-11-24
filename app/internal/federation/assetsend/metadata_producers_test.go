// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 against a live database: the producers of durable metadata work
// (migration 00100's triggers). Each test reads the ledger right after a
// real storage change, committed or rolled back. Skips without
// AA_DB_PASSWORD.

package assetsend_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// queued reports the ledger as (kind, asset) pairs, oldest first, with
// the 'all' rows as uuid.Nil.
func (f *fx) queued() []uuid.UUID {
	f.t.Helper()
	var out []uuid.UUID
	for _, w := range f.ledger() {
		if w.kind == "all" {
			out = append(out, uuid.Nil)
			continue
		}
		out = append(out, *w.asset)
	}
	return out
}

// expectQueued runs change in a transaction (committed unless rollback)
// on an emptied ledger and requires exactly want queued.
func (f *fx) expectQueued(what string, rollback bool, change func(tx pgx.Tx) error, want ...uuid.UUID) {
	f.t.Helper()
	ctx := context.Background()
	f.dropFixtureWork()
	undo := errors.New("roll back")
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if err := change(tx); err != nil {
			return err
		}
		if rollback {
			return undo
		}
		return nil
	})
	if rollback && !errors.Is(err, undo) || !rollback && err != nil {
		f.t.Fatalf("%s: %v", what, err)
	}
	if rollback {
		want = nil
	}
	got := f.queued()
	if len(got) != len(want) {
		f.t.Fatalf("%s: queued %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			f.t.Fatalf("%s: queued %v, want %v", what, got, want)
		}
	}
}

func exec(q string, args ...any) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), q, args...)
		return err
	}
}

// An asset's title, description or eligibility columns queue one
// obligation when they actually change, committed; nothing else does.
func TestMetadataProducers_Assets(t *testing.T) {
	f := newFx(t)
	f.clearWork(t)
	a := f.newAsset("public", true)

	f.expectQueued("a title change", false, exec(`UPDATE assets SET title = 'New title' WHERE id = $1`, a.id), a.id)
	f.expectQueued("a rolled-back title change", true, exec(`UPDATE assets SET title = 'Rolled back' WHERE id = $1`, a.id))
	f.expectQueued("title and description at once", false,
		exec(`UPDATE assets SET title = 'Both', description = 'Both' WHERE id = $1`, a.id), a.id)
	f.expectQueued("the same title again", false, exec(`UPDATE assets SET title = 'Both' WHERE id = $1`, a.id))
	f.expectQueued("flags only", false,
		exec(`UPDATE assets SET mature = NOT mature, ai_provenance = 'assisted', updated_at = now() WHERE id = $1`, a.id))
	f.expectQueued("flags with an unchanged title, as a flags PATCH writes", false,
		exec(`UPDATE assets SET title = title, mature = NOT mature WHERE id = $1`, a.id))
	f.expectQueued("a sensitivity change", false, exec(`UPDATE assets SET sensitivity = 'team' WHERE id = $1`, a.id), a.id)
	f.expectQueued("a deletion", false, exec(`UPDATE assets SET deleted_at = now() WHERE id = $1`, a.id), a.id)
	f.expectQueued("a restore", false, exec(`UPDATE assets SET deleted_at = NULL WHERE id = $1`, a.id), a.id)
	f.expectQueued("an owner written unchanged", false, exec(`UPDATE assets SET owner_user_ref = owner_user_ref WHERE id = $1`, a.id))
	f.expectQueued("an owner change", false, exec(`UPDATE assets SET owner_user_ref = NULL WHERE id = $1`, a.id), a.id)
	f.expectQueued("the owner back", false, exec(`UPDATE assets SET owner_user_ref = $2 WHERE id = $1`, a.id, f.owner), a.id)
	f.expectQueued("an asset type change", false, exec(`UPDATE assets SET asset_type = 2 WHERE id = $1`, a.id), a.id)

	// A new asset queues nothing: its first announcement is its snapshot.
	f.dropFixtureWork()
	f.newAsset("public", false)
	if got := f.queued(); len(got) != 0 {
		t.Fatalf("a new asset queued %v", got)
	}
	// A remote asset queues nothing.
	remote := uuid.New()
	f.exec(`INSERT INTO assets (id, title, asset_type, owner_user_ref, sensitivity, origin_server_id)
		VALUES ($1, 'Remote', 1, $2, 'public', gen_random_uuid())`, remote, f.owner)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, remote) })
	f.expectQueued("a remote asset's title", false, exec(`UPDATE assets SET title = 'Remote 2' WHERE id = $1`, remote))
}

// The two seeded fields' values queue their asset when the value, the
// asset or the field changes, at most two; other fields, a field that
// only shares a seeded code's name, and other value columns queue nothing.
func TestMetadataProducers_FieldValues(t *testing.T) {
	f := newFx(t)
	f.clearWork(t)
	a := f.newAsset("public", true)
	b := f.newAsset("public", true)
	ins := `INSERT INTO asset_field_value (asset_id, field_id, value_text) VALUES ($1, $2, $3)`

	f.expectQueued("a rolled-back rights value", true, exec(ins, a.id, seedRights, goodRights))
	f.expectQueued("a rights value", false, exec(ins, a.id, seedRights, goodRights), a.id)
	f.expectQueued("the same text again", false,
		exec(`UPDATE asset_field_value SET value_text = $3 WHERE asset_id = $1 AND field_id = $2`, a.id, seedRights, goodRights))
	f.expectQueued("another value column only", false,
		exec(`UPDATE asset_field_value SET value_num = 1, set_at = now() WHERE asset_id = $1 AND field_id = $2`, a.id, seedRights))
	f.expectQueued("a changed text", false,
		exec(`UPDATE asset_field_value SET value_text = 'https://example.org/l' WHERE asset_id = $1 AND field_id = $2`, a.id, seedRights), a.id)
	f.expectQueued("a value moved to another asset", false,
		exec(`UPDATE asset_field_value SET asset_id = $3 WHERE asset_id = $1 AND field_id = $2`, a.id, seedRights, b.id), a.id, b.id)
	f.expectQueued("a value moved to the other seeded field", false,
		exec(`UPDATE asset_field_value SET field_id = $3 WHERE asset_id = $1 AND field_id = $2`, b.id, seedRights, seedStatement), b.id)
	f.expectQueued("a statement value", false, exec(ins, a.id, seedStatement, "Statement"), a.id)
	f.expectQueued("a cleared value", false,
		exec(`DELETE FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`, a.id, seedStatement), a.id)

	// A field of the same name, not the seeded one, and an unrelated field.
	other := uuid.New()
	f.exec(`INSERT INTO field_definition (id, code, label, type) VALUES ($1, $2, 'Rights (other)', 'text')`,
		other, "rights_uri_"+strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM field_definition WHERE id = $1`, other) })
	f.expectQueued("an unrelated field's value", false, exec(ins, a.id, other, goodRights))
	f.expectQueued("an unrelated field's value changed", false,
		exec(`UPDATE asset_field_value SET value_text = 'x' WHERE asset_id = $1 AND field_id = $2`, a.id, other))
	f.expectQueued("an unrelated value moved onto a seeded field", false,
		exec(`UPDATE asset_field_value SET field_id = $3 WHERE asset_id = $1 AND field_id = $2`, a.id, other, seedRights), a.id)
}

// A seeded or mirroring definition queues one obligation for every asset
// when an attribute the capture reads changes; labels and other
// definitions queue nothing.
func TestMetadataProducers_Definitions(t *testing.T) {
	f := newFx(t)
	f.clearWork(t)
	all := uuid.Nil
	for _, id := range []uuid.UUID{seedRights, seedStatement, f.mirrorOf("title"), f.mirrorOf("description")} {
		f.restoreDefinition(t, id)
	}
	title := f.mirrorOf("title")
	upd := func(set string, id uuid.UUID) func(tx pgx.Tx) error {
		return exec(`UPDATE field_definition SET `+set+` WHERE id = $1`, id)
	}
	// Each no-op change is undone at once, by another no-op change.
	f.expectQueued("a label", false, upd(`label = label || '~r765'`, seedRights))
	f.expectQueued("the label back", false, upd(`label = left(label, length(label) - 5)`, seedRights))
	f.expectQueued("search and display defaults", false, upd(`searchable = NOT searchable, display_order = display_order + 1, updated_at = now()`, title))
	f.expectQueued("the defaults back", false, upd(`searchable = NOT searchable, display_order = display_order - 1`, title))
	f.expectQueued("a rolled-back privacy change", true, upd(`read_capability = 'system.admin'`, seedRights))
	f.expectQueued("a privacy change", false, upd(`read_capability = 'system.admin'`, seedRights), all)
	f.expectQueued("an archived field", false, upd(`status = 'archived'`, seedStatement), all)
	f.expectQueued("a retyped field", false, upd(`type = 'longtext'`, seedStatement), all)
	f.expectQueued("applies_to", false, upd(`applies_to = '{1}'`, seedRights), all)
	f.expectQueued("a mirror made private", false, upd(`read_capability = 'system.admin'`, title), all)
	f.expectQueued("a mirror's code", false, upd(`code = code || '_x'`, f.mirrorOf("description")), all)

	// A new mirror, and a new unrelated definition.
	f.dropFixtureWork()
	f.extraMirror(t, "title", "")
	if got := f.queued(); len(got) != 1 || got[0] != all {
		t.Fatalf("a new mirror queued %v", got)
	}
	plain := uuid.New()
	f.expectQueued("an unrelated definition", false,
		exec(`INSERT INTO field_definition (id, code, label, type) VALUES ($1, $2, 'Plain', 'text')`,
			plain, "plain_"+strings.ReplaceAll(uuid.NewString()[:8], "-", "")))
	f.expectQueued("an unrelated definition made private", false, upd(`read_capability = 'system.admin'`, plain))
	f.expectQueued("an unrelated definition becoming a mirror", false, upd(`mirrors_column = 'description', type = 'longtext'`, plain), all)
	f.expectQueued("a mirror deleted", false, exec(`DELETE FROM field_definition WHERE id = $1`, plain), all)
}

// Deleting a seeded definition with many values queues ONE obligation for
// every asset, none per value: the cascade's value deletions see their
// definition gone. Rolled back, so the shipped definition stays.
func TestMetadataProducers_ADefinitionCascadeQueuesOneRow(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.clearWork(t)
	const n = 40
	var assets []asset
	for range n {
		a := f.newAsset("public", false)
		f.setText(a, seedRights, goodRights)
		assets = append(assets, a)
	}
	f.dropFixtureWork()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // the deletion is never kept
	if _, err := tx.Exec(ctx, `DELETE FROM field_definition WHERE id = $1`, seedRights); err != nil {
		t.Fatal(err)
	}
	var values, all, perAsset int
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM asset_field_value WHERE field_id = $1),
		(SELECT count(*) FROM federation_metadata_work WHERE kind = 'all'),
		(SELECT count(*) FROM federation_metadata_work WHERE kind = 'asset')`, seedRights).Scan(&values, &all, &perAsset); err != nil {
		t.Fatal(err)
	}
	if values != 0 || all != 1 || perAsset != 0 {
		t.Fatalf("after the cascade: %d values left, %d all rows, %d asset rows; want 0, 1, 0", values, all, perAsset)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var kept int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM asset_field_value WHERE field_id = $1`, seedRights).Scan(&kept)
	if kept < n {
		t.Fatalf("the rollback kept %d values", kept)
	}
	// While the definition exists, an ordinary clear still queues.
	f.expectQueued("an ordinary clear", false,
		exec(`DELETE FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`, assets[0].id, seedRights), assets[0].id)
}

// A peer that becomes enabled, connected and holds all three profiles
// queues one obligation for every asset; other changes queue nothing.
func TestMetadataProducers_Peers(t *testing.T) {
	f := newFx(t)
	f.clearWork(t)
	all := uuid.Nil
	p := f.addPeer() // archival-1 and original-transfer-1 only
	upd := func(set string) func(tx pgx.Tx) error {
		return exec(`UPDATE federation_peers SET `+set+` WHERE id = $1`, p)
	}
	f.expectQueued("a note", false, upd(`notes = 'n', last_seen_at = now()`))
	f.expectQueued("a rolled-back profile", true, upd(`capabilities = '`+allMetadataProfiles+`'::jsonb`))
	f.expectQueued("the third profile", false, upd(`capabilities = '`+allMetadataProfiles+`'::jsonb`), all)
	f.expectQueued("already eligible", false, upd(`notes = 'm', capabilities = '`+allMetadataProfiles+`'::jsonb`))
	f.expectQueued("disabled", false, upd(`enabled = FALSE`))
	f.expectQueued("enabled again", false, upd(`enabled = TRUE`), all)
	f.expectQueued("not connected", false, upd(`status = 'pending_outbound'`))
	f.expectQueued("connected again", false, upd(`status = 'connected'`), all)
	f.expectQueued("a profile dropped", false, upd(`capabilities = '["archival-1","original-transfer-1"]'::jsonb`))
	f.expectQueued("an eligible new peer", false, exec(`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    enabled, status, handshake_by_user_ref, capabilities)
		VALUES ($1, 'receiver', 'k', TRUE, 'connected', $2, $3::jsonb)`, "https://new-"+uuid.NewString()[:8]+".example", f.owner, allMetadataProfiles), all)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE instance_url LIKE 'https://new-%.example' AND handshake_by_user_ref = $1`, f.owner)
	})
}

// An asset share queues its asset(s) when the grant changes, whether or
// not it is live, a regrant included; notes and other kinds queue nothing.
func TestMetadataProducers_Shares(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.clearWork(t)
	a := f.newAsset("public", true)
	b := f.newAsset("public", true)
	var act uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
		VALUES ($1, 'aa:Share', $2, $3, '{}'::jsonb) RETURNING id`, base+"/activities/"+uuid.NewString(), base+"/users/owner", f.owner).Scan(&act); err != nil {
		t.Fatal(err)
	}
	share := uuid.New()
	ins := func(id, object uuid.UUID, kind string) func(tx pgx.Tx) error {
		return exec(`INSERT INTO federation_shares (id, grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id, allow_original)
			VALUES ($1, $2, $3, $4, $5, 'view', $6, $7)`, id, f.owner, kind, object, f.peer, act, kind == "asset")
	}
	upd := func(set string) func(tx pgx.Tx) error {
		return exec(`UPDATE federation_shares SET `+set+` WHERE id = $1`, share)
	}
	f.expectQueued("a rolled-back grant", true, ins(share, a.id, "asset"))
	f.expectQueued("a grant", false, ins(share, a.id, "asset"), a.id)
	f.expectQueued("a note", false, upd(`notes = 'n', updated_at = now()`))
	f.expectQueued("an expiry", false, upd(`expires_at = now() + interval '1 day'`), a.id)
	f.expectQueued("no original", false, upd(`allow_original = FALSE`), a.id)
	f.expectQueued("a user target", false, upd(`target_user_url = 'https://receiver.example/users/u'`), a.id)
	f.expectQueued("moved to another asset", false, exec(`UPDATE federation_shares SET object_id = $2 WHERE id = $1`, share, b.id), a.id, b.id)
	f.expectQueued("a revocation", false, upd(`revoked_at = now()`), b.id)
	f.expectQueued("a regrant of the held asset", false, ins(uuid.New(), b.id, "asset"), b.id)
	f.expectQueued("a deleted share", false, exec(`DELETE FROM federation_shares WHERE id = $1`, share), b.id)
	f.expectQueued("a post share", false, ins(uuid.New(), uuid.New(), "post"))
}

// The ledger holds UUIDs and cursors, never the source text that queued
// the work; and an enqueue that fails fails the change with it.
func TestMetadataProducers_NoValuesAndAFailedEnqueueFailsTheChange(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.clearWork(t)
	a := f.newAsset("public", true)
	const private = "a-distinctive-private-title-2f9c"
	f.dropFixtureWork()
	f.exec(`UPDATE assets SET title = $2, description = $2 WHERE id = $1`, a.id, private)
	f.setText(a, seedStatement, private)
	var rows string
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(string_agg(to_jsonb(w)::text, ','), '') FROM federation_metadata_work w`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == "" || strings.Contains(rows, private) {
		t.Fatalf("the ledger %q", rows)
	}

	// A test-owned constraint, inside the test's own transaction, refuses
	// this asset's work: the title change fails with it. The rollback
	// removes the constraint.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // everything here is undone
	if _, err := tx.Exec(ctx, `ALTER TABLE federation_metadata_work ADD CONSTRAINT r765_refuse CHECK (asset_id IS DISTINCT FROM '`+a.id.String()+`'::uuid) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT change`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE assets SET title = 'Refused' WHERE id = $1`, a.id); err == nil {
		t.Fatal("the change committed without its work")
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT change`); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := tx.QueryRow(ctx, `SELECT title FROM assets WHERE id = $1`, a.id).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != private {
		t.Fatalf("the refused change left title %q", title)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = 'r765_refuse'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the test constraint remains (%d, %v)", left, err)
	}
}
