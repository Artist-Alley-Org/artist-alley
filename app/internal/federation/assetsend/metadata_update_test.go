// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 against a live database: a metadata Update to one peer
// (metadata_update.go), recorded through RecordMetadataUpdate in a
// transaction holding the asset row and built through
// Source.MetadataUpdateMembers. The delivery itself, through the real
// worker, is in outbox/delivery_metadata_update_test.go. Skips without
// AA_DB_PASSWORD.

package assetsend_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
)

const allMetadataProfiles = `["share-lifecycle-1","archival-1","original-transfer-1","asset-metadata-1"]`

// withMetadataProfile gives f's peer all three profiles, as they are put
// back after t.
func (f *fx) withMetadataProfile(t *testing.T) {
	t.Helper()
	f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peer, allMetadataProfiles)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `UPDATE federation_peers SET enabled = TRUE, status = 'connected', capabilities = $2::jsonb
			WHERE id = $1`, f.peer, allMetadataProfiles)
	})
}

// recordMetadataTo records a metadata Update of a to peer in one
// transaction holding a's row, as a caller must; fail rolls it back.
func (f *fx) recordMetadataTo(a asset, peer uuid.UUID, username string, fail error) (uuid.UUID, error) {
	ctx := context.Background()
	var id uuid.UUID
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM assets WHERE id = $1 FOR UPDATE`, a.id); err != nil {
			return err
		}
		var err error
		if id, err = assetsend.RecordMetadataUpdate(ctx, tx, f.writer, assetsend.MetadataChange{AssetID: a.id, PeerID: peer, BaseURL: base,
			Username: func(context.Context, int64) string { return username }}); err != nil {
			return err
		}
		return fail
	})
	return id, err
}

func (f *fx) recordMetadata(a asset) uuid.UUID {
	f.t.Helper()
	id, err := f.recordMetadataTo(a, f.peer, "owner", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

type metaUpdate struct {
	id        uuid.UUID
	objectURI string
	raw       []byte
	payload   map[string]any
	published time.Time
}

func (f *fx) metaUpdate(id uuid.UUID) metaUpdate {
	f.t.Helper()
	u := metaUpdate{id: id}
	if err := f.pool.QueryRow(context.Background(), `SELECT object_uri, payload, published_at FROM activities WHERE id = $1`, id).
		Scan(&u.objectURI, &u.raw, &u.published); err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(u.raw, &u.payload); err != nil {
		f.t.Fatal(err)
	}
	return u
}

func (f *fx) metaMembers(u metaUpdate) (json.RawMessage, json.RawMessage, error) {
	return f.src.MetadataUpdateMembers(context.Background(), u.id, f.peer, u.objectURI, u.raw)
}

// updates counts the asset's local Update activities.
func (f *fx) updates(a asset) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM activities
		WHERE source = 'local' AND activity_type = 'Update' AND object_kind = 'asset' AND object_local_id = $1`, a.id.String()).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// eligible is a public sealed asset described as given, granted to f's
// peer with its announcement recorded (not dispatched) and its aa:Share
// sent.
func (f *fx) eligible(title, description, rights, statement string) (asset, grant) {
	f.t.Helper()
	a := f.newAsset("public", true)
	f.describe(a, title, description, rights, statement)
	g := f.grantAndRecord(a, nil)
	if g.announcement == uuid.Nil {
		f.t.Fatal("nothing announced")
	}
	f.shareDelivered(g, "sent")
	return a, g
}

// told builds g's announcement (its claims recorded, as the delivery
// worker does) and marks its outbox row to the peer sent, so a flags
// Update reaches the peer too.
func (f *fx) told(g grant) {
	f.t.Helper()
	if _, err := f.member(g); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, 'sent')`, g.announcement, f.peer)
}

// An eligible peer gets one Update naming the version it was announced,
// even one not dispatched yet and never claimed. Its local record holds
// the capture as one string and no wire member; the members are aa:asset
// without a descriptive member, and the recorded aa:metadata.
func TestMetadataUpdate_RecordsForAnEligiblePeer(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	a := f.newAsset("public", true)
	f.describe(a, "Harbour at dawn", "Body", goodRights, "Studio A")
	g := f.grantAndRecord(a, nil) // the aa:Share not delivered yet
	var claims, outbox int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM federation_asset_announcement_claims WHERE asset_id = $1),
		(SELECT count(*) FROM federation_outbox WHERE activity_id = $2)`, a.id, g.announcement).Scan(&claims, &outbox); err != nil || claims != 0 || outbox != 0 {
		t.Fatalf("fixture: %d claims, %d outbox rows, %v", claims, outbox, err)
	}

	id := f.recordMetadata(a)
	if id == uuid.Nil {
		t.Fatal("nothing recorded")
	}
	u := f.metaUpdate(id)
	if u.objectURI != base+"/assets/"+a.id.String() {
		t.Fatalf("object %s", u.objectURI)
	}
	want := map[string]bool{"share_id": true, "share_activity_id": true, "asset_id": true, "version_number": true, "sensitivity": true,
		"mature": true, "ai_provenance": true, "version_id": true, "metadata_capture": true}
	for k := range u.payload {
		if !want[k] {
			t.Errorf("the local record holds %q", k)
		}
	}
	if u.payload["share_id"] != g.share.String() || u.payload["version_id"] != a.version.String() || u.payload["version_number"] != float64(1) {
		t.Fatalf("record %v", u.payload)
	}
	capture, ok := u.payload["metadata_capture"].(string)
	if !ok {
		t.Fatalf("the capture is %T, not a string", u.payload["metadata_capture"])
	}
	var c assetsend.MetadataCapture
	if err := json.Unmarshal([]byte(capture), &c); err != nil || c.V != 1 || c.Snapshot.Title == nil || *c.Snapshot.Title != "Harbour at dawn" {
		t.Fatalf("capture %s %v", capture, err)
	}

	// The aa:Share is not delivered yet: the delivery waits.
	if _, _, err := f.metaMembers(u); !errors.Is(err, assetsend.ErrNotReady) {
		t.Fatalf("before the aa:Share: %v", err)
	}
	f.shareDelivered(g, "sent")
	asset, metadata, err := f.metaMembers(u)
	if err != nil {
		t.Fatal(err)
	}
	m := f.parsed(asset)
	if m.ID != u.objectURI || m.Version.Number != 1 || m.Description != nil || m.Rights != "" || m.RequiredStatement != nil {
		t.Fatalf("aa:asset %+v", m)
	}
	if !bytes.Equal(metadata, wire(t, c.Snapshot)) {
		t.Fatalf("aa:metadata %s", metadata)
	}
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM federation_asset_announcement_claims WHERE asset_id = $1`, a.id).Scan(&claims)
	}); err != nil || claims != 0 {
		t.Fatalf("claims after the build: %d %v", claims, err)
	}
}

// Nothing is recorded unless every condition holds now; another peer is
// never reached.
func TestMetadataUpdate_NothingForAnIneligiblePeer(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	steps := []struct {
		name   string
		break_ func(f *fx, a asset, g grant) (peer uuid.UUID, username string)
	}{
		{"the peer without asset-metadata-1", func(f *fx, _ asset, _ grant) (uuid.UUID, string) {
			f.exec(`UPDATE federation_peers SET capabilities = capabilities - 'asset-metadata-1' WHERE id = $1`, f.peer)
			return f.peer, "owner"
		}},
		{"the peer without archival-1", func(f *fx, _ asset, _ grant) (uuid.UUID, string) {
			f.exec(`UPDATE federation_peers SET capabilities = capabilities - 'archival-1' WHERE id = $1`, f.peer)
			return f.peer, "owner"
		}},
		{"the peer without original-transfer-1", func(f *fx, _ asset, _ grant) (uuid.UUID, string) {
			f.exec(`UPDATE federation_peers SET capabilities = capabilities - 'original-transfer-1' WHERE id = $1`, f.peer)
			return f.peer, "owner"
		}},
		{"the peer disabled", func(f *fx, _ asset, _ grant) (uuid.UUID, string) {
			f.exec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peer)
			return f.peer, "owner"
		}},
		{"the peer not connected", func(f *fx, _ asset, _ grant) (uuid.UUID, string) {
			f.exec(`UPDATE federation_peers SET status = 'pending_inbound' WHERE id = $1`, f.peer)
			return f.peer, "owner"
		}},
		{"the share revoked", func(f *fx, _ asset, g grant) (uuid.UUID, string) {
			f.exec(`UPDATE federation_shares SET revoked_at = now() WHERE id = $1`, g.share)
			return f.peer, "owner"
		}},
		{"the share expired", func(f *fx, _ asset, g grant) (uuid.UUID, string) {
			f.exec(`UPDATE federation_shares SET expires_at = now() - interval '1 hour' WHERE id = $1`, g.share)
			return f.peer, "owner"
		}},
		{"the share to one user", func(f *fx, _ asset, g grant) (uuid.UUID, string) {
			f.exec(`UPDATE federation_shares SET target_user_url = 'https://receiver.example/users/bob' WHERE id = $1`, g.share)
			return f.peer, "owner"
		}},
		{"a view-only share", func(f *fx, _ asset, g grant) (uuid.UUID, string) {
			f.exec(`UPDATE federation_shares SET allow_original = FALSE WHERE id = $1`, g.share)
			return f.peer, "owner"
		}},
		{"the asset deleted", func(f *fx, a asset, _ grant) (uuid.UUID, string) {
			f.exec(`UPDATE assets SET deleted_at = now() WHERE id = $1`, a.id)
			return f.peer, "owner"
		}},
		{"the asset restricted", func(f *fx, a asset, _ grant) (uuid.UUID, string) {
			f.exec(`UPDATE assets SET sensitivity = 'restricted' WHERE id = $1`, a.id)
			return f.peer, "owner"
		}},
		{"the owner without a username", func(f *fx, _ asset, _ grant) (uuid.UUID, string) { return f.peer, "" }},
		{"the announcement failed", func(f *fx, _ asset, g grant) (uuid.UUID, string) {
			f.exec(`INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, 'failed')`, g.announcement, f.peer)
			return f.peer, "owner"
		}},
		{"the announcement refused", func(f *fx, _ asset, g grant) (uuid.UUID, string) {
			f.exec(`INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, 'refused')`, g.announcement, f.peer)
			return f.peer, "owner"
		}},
		{"another peer, granted nothing", func(f *fx, _ asset, _ grant) (uuid.UUID, string) {
			p := f.addPeer()
			f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, p, allMetadataProfiles)
			return p, "owner"
		}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			f := f.sub(t)
			f.withMetadataProfile(t)
			a, g := f.eligible("Title", "Body", goodRights, "Statement")
			peer, username := s.break_(f, a, g)
			id, err := f.recordMetadataTo(a, peer, username, nil)
			if err != nil || id != uuid.Nil || f.updates(a) != 0 {
				t.Fatalf("recorded %s (%d Updates) %v", id, f.updates(a), err)
			}
		})
	}
	t.Run("a draft version is never named", func(t *testing.T) {
		f := f.sub(t)
		f.withMetadataProfile(t)
		a := f.newAsset("public", false)
		g := f.grantAndRecord(a, nil) // the draft's announcement waits for its seal
		f.shareDelivered(g, "sent")
		if id, err := f.recordMetadataTo(a, f.peer, "owner", nil); err != nil || id != uuid.Nil {
			t.Fatalf("recorded %s %v", id, err)
		}
	})
}

// The highest sealed version usable for the peer is named; a later draft
// is not.
func TestMetadataUpdate_NamesTheHighestUsableSealedVersion(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	a, _ := f.eligible("Title", "", "", "")
	v1 := a.version
	if ids, err := f.sealNext(&a, 2, true, nil); err != nil || len(ids) == 0 {
		t.Fatalf("version 2: %v %v", ids, err)
	}
	v2 := a.version
	if _, err := f.sealNext(&a, 3, false, nil); err != nil {
		t.Fatal(err)
	}
	u := f.metaUpdate(f.recordMetadata(a))
	if u.payload["version_number"] != float64(2) || u.payload["version_id"] != v2.String() || v1 == v2 {
		t.Fatalf("named %v %v", u.payload["version_number"], u.payload["version_id"])
	}
	asset, _, err := f.metaMembers(u)
	if err != nil || f.parsed(asset).Version.Number != 2 {
		t.Fatalf("aa:asset %s %v", asset, err)
	}
}

// A metadata Update is ordered after every earlier announcement and
// Update of the asset, flags Updates included, and a rollback leaves no
// activity. A flags Update carries no capture.
func TestMetadataUpdate_PublishedAfterEverythingAndRolledBackWhole(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	a, g := f.eligible("Title", "", "", "")
	f.told(g)
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	f.exec(`UPDATE activities SET published_at = $2 WHERE id = $1`, g.announcement, future)

	first := f.metaUpdate(f.recordMetadata(a))
	if !first.published.After(future) {
		t.Fatalf("published %s, not after %s", first.published, future)
	}
	flags, err := f.changeFlags(a, true, "generated", nil)
	if err != nil || len(flags) != 1 {
		t.Fatalf("flags Update: %v %v", flags, err)
	}
	flagUpdate := f.metaUpdate(flags[0])
	if _, ok := flagUpdate.payload["metadata_capture"]; ok || !flagUpdate.published.After(first.published) {
		t.Fatalf("flags Update %v at %s", flagUpdate.payload, flagUpdate.published)
	}
	second := f.metaUpdate(f.recordMetadata(a))
	if !second.published.After(flagUpdate.published) || second.payload["mature"] != true || second.payload["ai_provenance"] != "generated" {
		t.Fatalf("second %v at %s", second.payload, second.published)
	}

	before := f.updates(a)
	var outbox int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM federation_outbox`).Scan(&outbox)
	rollback := errors.New("roll back")
	if id, err := f.recordMetadataTo(a, f.peer, "owner", rollback); !errors.Is(err, rollback) || id == uuid.Nil {
		t.Fatalf("the rolled-back call: %s %v", id, err)
	}
	var outboxAfter int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM federation_outbox`).Scan(&outboxAfter)
	if f.updates(a) != before || outboxAfter != outbox {
		t.Fatalf("a rollback left %d Updates (was %d), %d outbox rows (was %d)", f.updates(a), before, outboxAfter, outbox)
	}
}

// The capture comes back from the activity row exactly as captured, at
// the member budget's boundary, and every later delivery, from a fresh
// Source after value edits, a flags change and a new version, sends the
// same members.
func TestMetadataUpdate_TheCaptureSurvivesTheActivityRowExactly(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	title, desc := strings.Repeat("<", 1024), strings.Repeat("<", 4096)
	size := func(stmt string) int {
		return len(wire(t, federation.AssetMetadataSnapshot{Title: &title, Description: &desc, RequiredStatement: statementOf(stmt)}))
	}
	stmt := strings.Repeat("a", federation.AssetMetadataMaxMemberBytes-size("Z")) + "Z"
	a, _ := f.eligible(title, desc, "", stmt)
	u := f.metaUpdate(f.recordMetadata(a))

	// The string in the stored row is the capture's encoding, byte for byte.
	var stored string
	if err := f.pool.QueryRow(ctx, `SELECT payload->>'metadata_capture' FROM activities WHERE id = $1`, u.id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var now []byte
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		c, err := assetsend.CaptureMetadata(ctx, tx, a.id)
		if err == nil {
			now, err = json.Marshal(c)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stored != string(now) {
		t.Fatal("the stored capture is not the capture's encoding")
	}
	asset, metadata, err := f.metaMembers(u)
	if err != nil || len(metadata) != federation.AssetMetadataMaxMemberBytes || !bytes.HasSuffix(metadata, []byte(`aZ"]}}}`)) {
		t.Fatalf("member %d bytes %v", len(metadata), err)
	}

	f.describe(a, "New title", "New body", goodRights, "New statement")
	if _, err := f.changeFlags(a, true, "assisted", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sealNext(&a, 2, true, nil); err != nil {
		t.Fatal(err)
	}
	asset2, metadata2, err := (assetsend.Source{Pool: f.pool}).MetadataUpdateMembers(ctx, u.id, f.peer, u.objectURI, f.metaUpdate(u.id).raw)
	if err != nil || !bytes.Equal(asset2, asset) || !bytes.Equal(metadata2, metadata) {
		t.Fatalf("a later delivery changed: %v", err)
	}
	if m := f.parsed(asset2); m.Version.Number != 1 || m.Mature == nil || *m.Mature {
		t.Fatalf("aa:asset %+v", m)
	}
}

// Every delivery check applies to a four-null clear as to any Update: the
// snapshot's own re-check reads nothing for it, so these are what stop it.
func TestMetadataUpdate_EveryCheckAppliesToAClear(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	var other int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Other', 1) RETURNING ref`,
		"as-other-"+uuid.NewString()[:8]).Scan(&other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, other) })

	cleared := func(f *fx) (asset, grant, metaUpdate) {
		a, g := f.eligible("", "", "", "")
		u := f.metaUpdate(f.recordMetadata(a))
		if _, metadata, err := f.metaMembers(u); err != nil || string(metadata) != fourNull {
			f.t.Fatalf("baseline: %s %v", metadata, err)
		}
		return a, g, u
	}
	steps := []struct {
		name   string
		change func(f *fx, a asset, g grant, u *metaUpdate)
	}{
		{"the share revoked", func(f *fx, _ asset, g grant, _ *metaUpdate) {
			f.exec(`UPDATE federation_shares SET revoked_at = now() WHERE id = $1`, g.share)
		}},
		{"the share expired", func(f *fx, _ asset, g grant, _ *metaUpdate) {
			f.exec(`UPDATE federation_shares SET expires_at = now() - interval '1 hour' WHERE id = $1`, g.share)
		}},
		{"the share to one user", func(f *fx, _ asset, g grant, _ *metaUpdate) {
			f.exec(`UPDATE federation_shares SET target_user_url = 'https://receiver.example/users/bob' WHERE id = $1`, g.share)
		}},
		{"the peer disabled", func(f *fx, _ asset, _ grant, _ *metaUpdate) {
			f.exec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peer)
		}},
		{"the peer dropped asset-metadata-1", func(f *fx, _ asset, _ grant, _ *metaUpdate) {
			f.exec(`UPDATE federation_peers SET capabilities = capabilities - 'asset-metadata-1' WHERE id = $1`, f.peer)
		}},
		{"the peer dropped archival-1", func(f *fx, _ asset, _ grant, _ *metaUpdate) {
			f.exec(`UPDATE federation_peers SET capabilities = capabilities - 'archival-1' WHERE id = $1`, f.peer)
		}},
		{"another owner", func(f *fx, a asset, _ grant, _ *metaUpdate) {
			f.exec(`UPDATE assets SET owner_user_ref = $2 WHERE id = $1`, a.id, other)
		}},
		{"another tier", func(f *fx, a asset, _ grant, _ *metaUpdate) {
			f.exec(`UPDATE assets SET sensitivity = 'team' WHERE id = $1`, a.id)
		}},
		{"the asset deleted", func(f *fx, a asset, _ grant, _ *metaUpdate) {
			f.exec(`UPDATE assets SET deleted_at = now() WHERE id = $1`, a.id)
		}},
		{"another version id", func(_ *fx, _ asset, _ grant, u *metaUpdate) {
			u.payload["version_id"] = uuid.NewString()
			u.raw, _ = json.Marshal(u.payload)
		}},
		{"another version number", func(_ *fx, _ asset, _ grant, u *metaUpdate) {
			u.payload["version_number"] = 7
			u.raw, _ = json.Marshal(u.payload)
		}},
		{"another share activity", func(_ *fx, _ asset, _ grant, u *metaUpdate) {
			u.payload["share_activity_id"] = uuid.NewString()
			u.raw, _ = json.Marshal(u.payload)
		}},
		{"another activity", func(_ *fx, _ asset, g grant, u *metaUpdate) { u.id = g.announcement }},
		{"another object", func(_ *fx, _ asset, _ grant, u *metaUpdate) {
			u.objectURI = base + "/assets/" + uuid.NewString()
		}},
		{"no capture", func(_ *fx, _ asset, _ grant, u *metaUpdate) {
			delete(u.payload, "metadata_capture")
			u.raw, _ = json.Marshal(u.payload)
		}},
		{"a capture that is not a string", func(_ *fx, _ asset, _ grant, u *metaUpdate) {
			u.payload["metadata_capture"] = map[string]any{"v": 1}
			u.raw, _ = json.Marshal(u.payload)
		}},
		{"a malformed capture", func(_ *fx, _ asset, _ grant, u *metaUpdate) {
			u.payload["metadata_capture"] = `{"v":1,"snapshot":` + fourNull + `}]`
			u.raw, _ = json.Marshal(u.payload)
		}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			f := f.sub(t)
			f.withMetadataProfile(t)
			a, g, u := cleared(f)
			s.change(f, a, g, &u)
			asset, metadata, err := f.metaMembers(u)
			if !errors.Is(err, federation.ErrNotFederated) || asset != nil || metadata != nil {
				t.Fatalf("not refused: %s %s %v", asset, metadata, err)
			}
		})
	}
	t.Run("the aa:Share not delivered yet waits", func(t *testing.T) {
		f := f.sub(t)
		f.withMetadataProfile(t)
		_, g, u := cleared(f)
		f.shareDelivered(g, "queued")
		if _, _, err := f.metaMembers(u); !errors.Is(err, assetsend.ErrNotReady) || errors.Is(err, federation.ErrNotFederated) {
			t.Fatalf("%v", err)
		}
	})
}

// A value edit, even to an invalid value, sends the recorded claims; a
// restriction of a recorded claim's source refuses the whole activity,
// naming no value.
func TestMetadataUpdate_PrivacyRefusesTheWholeActivity(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	a, _ := f.eligible("Secret title", "Secret body", goodRights, "Secret statement")
	u := f.metaUpdate(f.recordMetadata(a))
	_, before, err := f.metaMembers(u)
	if err != nil {
		t.Fatal(err)
	}
	f.describe(a, "Edited", "", "", "")
	f.setText(a, seedRights, "not a rights URI")
	if _, after, err := f.metaMembers(u); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("a value edit changed the member: %s %v", after, err)
	}
	f.restoreDefinition(t, seedRights)
	f.exec(`UPDATE field_definition SET read_capability = 'fields.admin' WHERE id = $1`, seedRights)
	asset, metadata, err := f.metaMembers(u)
	if !errors.Is(err, federation.ErrNotFederated) || asset != nil || metadata != nil {
		t.Fatalf("not refused: %v", err)
	}
	for _, v := range []string{"Secret", goodRights} {
		if strings.Contains(err.Error(), v) {
			t.Fatalf("the refusal carries a value: %v", err)
		}
	}
}

// rowTracer records, while on, the rows each statement returned (the
// command tag's count) on a pool of its own.
type rowTracer struct {
	mu   sync.Mutex
	on   bool
	rows []int64
}

func (r *rowTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (r *rowTracer) TraceQueryEnd(_ context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.on && d.Err == nil && strings.HasPrefix(d.CommandTag.String(), "SELECT") {
		r.rows = append(r.rows, d.CommandTag.RowsAffected())
	}
}

func (f *fx) tracedPool(tr *rowTracer) *pgxpool.Pool {
	f.t.Helper()
	cfg, err := pgxpool.ParseConfig(fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable pool_max_conns=4",
		env("AA_DB_HOST", "postgres"), env("AA_DB_PORT", "5432"), env("AA_DB_USER", "artist_alley"),
		os.Getenv("AA_DB_PASSWORD"), env("AA_DB_NAME", "artist_alley")))
	if err != nil {
		f.t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(pool.Close)
	return pool
}

// grantPeer grants a to peer under a new share with its own aa:Share,
// recording the announcement only when announce is set.
func (f *fx) grantPeer(a asset, peer uuid.UUID, announce bool) grant {
	f.t.Helper()
	ctx := context.Background()
	g := grant{share: uuid.New()}
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
			VALUES ($1, 'aa:Share', $2, $3, '{}'::jsonb) RETURNING id`, base+"/activities/"+uuid.NewString(),
			base+"/users/owner", f.owner).Scan(&g.shareActivity); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federation_shares (id, grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id, allow_original)
			VALUES ($1, $2, 'asset', $3, $4, 'view', $5, TRUE)`, g.share, f.owner, a.id, peer, g.shareActivity); err != nil {
			return err
		}
		if !announce {
			return nil
		}
		id, err := assetsend.Record(ctx, tx, f.writer, assetsend.Grant{ShareID: g.share, ShareActivityID: g.shareActivity,
			AssetID: a.id, PeerID: peer, PeerURL: "https://receiver.example", BaseURL: base,
			Username: func(context.Context, int64) string { return "owner" }})
		g.announcement = id
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, 'sent')`, g.shareActivity, peer); err != nil {
		f.t.Fatal(err)
	}
	return g
}

// C1: the version is chosen in one statement returning at most one row,
// whatever number of versions is usable for the peer.
func TestMetadataUpdate_TheVersionIsChosenByABoundedQuery(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)
	a, _ := f.eligible("Title", "", "", "")
	for n := int64(2); n <= 4; n++ {
		if ids, err := f.sealNext(&a, n, true, nil); err != nil || len(ids) == 0 {
			t.Fatalf("version %d: %v %v", n, ids, err)
		}
	}
	tr := &rowTracer{}
	pool := f.tracedPool(tr)
	var id uuid.UUID
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM assets WHERE id = $1 FOR UPDATE`, a.id); err != nil {
			return err
		}
		tr.mu.Lock()
		tr.on = true
		tr.mu.Unlock()
		var err error
		id, err = assetsend.RecordMetadataUpdate(ctx, tx, f.writer, assetsend.MetadataChange{AssetID: a.id, PeerID: f.peer, BaseURL: base,
			Username: func(context.Context, int64) string { return "owner" }})
		tr.mu.Lock()
		tr.on = false
		tr.mu.Unlock()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if id == uuid.Nil {
		t.Fatal("nothing recorded")
	}
	if u := f.metaUpdate(id); u.payload["version_number"] != float64(4) || u.payload["version_id"] != a.version.String() {
		t.Fatalf("named %v", u.payload["version_number"])
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.rows) == 0 {
		t.Fatal("no statement traced")
	}
	for i, n := range tr.rows {
		if n > 1 {
			t.Errorf("statement %d returned %d rows to the recorder (rows %v)", i, n, tr.rows)
		}
	}
}

// C1: the version named is the highest usable for THIS peer, not the
// asset's.
func TestMetadataUpdate_ThePeersOwnHighestVersion(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	a, g := f.eligible("Title", "", "", "")
	// Version 2 is sealed while the peer's share is expired, so it is not
	// announced to the peer; another peer is granted (and announced) it.
	f.exec(`UPDATE federation_shares SET expires_at = now() - interval '1 hour' WHERE id = $1`, g.share)
	if _, err := f.sealNext(&a, 2, true, nil); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE federation_shares SET expires_at = NULL WHERE id = $1`, g.share)
	other := f.addPeer()
	if og := f.grantPeer(a, other, true); og.announcement == uuid.Nil {
		t.Fatal("the other peer was not announced version 2")
	}
	u := f.metaUpdate(f.recordMetadata(a))
	if u.payload["version_number"] != float64(1) {
		t.Fatalf("named %v, want the peer's own version 1", u.payload["version_number"])
	}
}

// C1: the remaining record-time refusals and the bindings a pending
// announcement needs.
func TestMetadataUpdate_TheBindingRules(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.withMetadataProfile(t)

	t.Run("a remote asset", func(t *testing.T) {
		f := f.sub(t)
		// A versioned local asset cannot become remote (#1596), so the
		// asset is remote from the start, with a live grant to the peer.
		a := asset{id: uuid.New()}
		f.exec(`INSERT INTO assets (id, title, asset_type, owner_user_ref, sensitivity, origin_server_id)
			VALUES ($1, 'Remote', 1, $2, 'public', gen_random_uuid())`, a.id, f.owner)
		t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, a.id) })
		f.grantPeer(a, f.peer, false)
		if id, err := f.recordMetadataTo(a, f.peer, "owner", nil); err != nil || id != uuid.Nil {
			t.Fatalf("recorded %s %v", id, err)
		}
	})
	t.Run("an asset with no owner", func(t *testing.T) {
		f := f.sub(t)
		a, _ := f.eligible("Title", "", "", "")
		f.exec(`UPDATE assets SET owner_user_ref = NULL WHERE id = $1`, a.id)
		if id, err := f.recordMetadataTo(a, f.peer, "owner", nil); err != nil || id != uuid.Nil {
			t.Fatalf("recorded %s %v", id, err)
		}
	})
	// Pending is the announcement's durable dispatch work (migration
	// 00101), never the progress cursor (rev796). The cursor is moved past
	// the announcement as a fixture (restored after the test): only to show
	// that it no longer decides. The actual dispatcher schedule is tested in
	// pending_dispatch_test.go.
	cursorPast := func(t *testing.T, f *fx) {
		t.Helper()
		var later uuid.UUID
		if err := f.pool.QueryRow(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
			VALUES ($1, 'Like', $2, $3, '{}'::jsonb) RETURNING id`, base+"/activities/"+uuid.NewString(), base+"/users/owner", f.owner).Scan(&later); err != nil {
			t.Fatal(err)
		}
		var prev *uuid.UUID
		var had bool
		if err := f.pool.QueryRow(ctx, `SELECT TRUE, last_dispatched_activity_id FROM federation_dispatch_state WHERE id = 1`).Scan(&had, &prev); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			c := context.Background()
			if had {
				_, _ = f.pool.Exec(c, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = $1 WHERE id = 1`, prev)
			} else {
				_, _ = f.pool.Exec(c, `DELETE FROM federation_dispatch_state WHERE id = 1`)
			}
		})
		f.exec(`INSERT INTO federation_dispatch_state (id, last_dispatched_activity_id) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET last_dispatched_activity_id = EXCLUDED.last_dispatched_activity_id`, later)
	}
	pendingWork := func(t *testing.T, f *fx, id uuid.UUID) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM federation_dispatch_work WHERE activity_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Run("a pending announcement behind the dispatch cursor", func(t *testing.T) {
		f := f.sub(t)
		a, g := f.eligible("Title", "", "", "")
		cursorPast(t, f)
		if n := pendingWork(t, f, g.announcement); n != 1 {
			t.Fatalf("the announcement has %d dispatch work rows, want 1", n)
		}
		id, err := f.recordMetadataTo(a, f.peer, "owner", nil)
		if err != nil || id == uuid.Nil {
			t.Fatalf("not recorded: %s %v", id, err)
		}
		if u := f.metaUpdate(id); u.payload["version_id"] != a.version.String() || u.payload["version_number"] != float64(1) ||
			u.payload["share_id"] != g.share.String() {
			t.Fatalf("named %v (%v) on %v", u.payload["version_id"], u.payload["version_number"], u.payload["share_id"])
		}
	})
	// The fixture DELETE of exactly the announcement's work row models the
	// dispatcher acknowledging it with no row for the peer (a skip).
	for _, behind := range []bool{true, false} {
		name := "an acknowledged announcement with no outbox row, the cursor not moved by the test"
		if behind {
			name = "an acknowledged announcement with no outbox row, behind the cursor"
		}
		t.Run(name, func(t *testing.T) {
			f := f.sub(t)
			a, g := f.eligible("Title", "", "", "")
			if behind {
				cursorPast(t, f)
			}
			f.exec(`DELETE FROM federation_dispatch_work WHERE activity_id = $1`, g.announcement)
			if n := pendingWork(t, f, g.announcement); n != 0 {
				t.Fatalf("the announcement still has %d dispatch work rows", n)
			}
			if id, err := f.recordMetadataTo(a, f.peer, "owner", nil); err != nil || id != uuid.Nil {
				t.Fatalf("recorded %s %v", id, err)
			}
		})
	}
	t.Run("a queued announcement whose share became targeted, with a fresh grant", func(t *testing.T) {
		f := f.sub(t)
		a, g := f.eligible("Title", "", "", "")
		f.exec(`INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, 'queued')`, g.announcement, f.peer)
		f.exec(`UPDATE federation_shares SET target_user_url = 'https://receiver.example/users/bob' WHERE id = $1`, g.share)
		f.grantPeer(a, f.peer, false) // peer-wide and original, announcing nothing
		if id, err := f.recordMetadataTo(a, f.peer, "owner", nil); err != nil || id != uuid.Nil {
			t.Fatalf("recorded %s %v on the targeted share's announcement", id, err)
		}
	})
	t.Run("a sent version under an ended share, with a fresh grant", func(t *testing.T) {
		f := f.sub(t)
		a, g := f.eligible("Title", "", "", "")
		f.exec(`INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, 'sent')`, g.announcement, f.peer)
		f.exec(`UPDATE federation_shares SET revoked_at = now() WHERE id = $1`, g.share)
		fresh := f.grantPeer(a, f.peer, false)
		id, err := f.recordMetadataTo(a, f.peer, "owner", nil)
		if err != nil || id == uuid.Nil {
			t.Fatalf("not recorded: %v", err)
		}
		if u := f.metaUpdate(id); u.payload["version_number"] != float64(1) || u.payload["share_id"] != fresh.share.String() {
			t.Fatalf("named %v on %v", u.payload["version_number"], u.payload["share_id"])
		}
	})
}

// C1: a recorded activity whose actor, object kind or object id is gone
// is refused for good, a four-null clear included; a read error is still
// an error.
func TestMetadataUpdate_AMissingActivityIdentityIsRefused(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	for _, column := range []string{"actor_user_ref", "object_kind", "object_local_id"} {
		for name, title := range map[string]string{"ordinary": "Title", "four-null": ""} {
			t.Run(column+", "+name, func(t *testing.T) {
				f := f.sub(t)
				a, _ := f.eligible(title, "", "", "")
				u := f.metaUpdate(f.recordMetadata(a))
				var actor *int64
				var kind, local *string
				if err := f.pool.QueryRow(context.Background(), `SELECT actor_user_ref, object_kind, object_local_id FROM activities WHERE id = $1`, u.id).
					Scan(&actor, &kind, &local); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = f.pool.Exec(context.Background(), `UPDATE activities SET actor_user_ref = $2, object_kind = $3, object_local_id = $4 WHERE id = $1`,
						u.id, actor, kind, local)
				})
				f.exec(`UPDATE activities SET `+column+` = NULL WHERE id = $1`, u.id)
				asset, metadata, err := f.metaMembers(u)
				if !errors.Is(err, federation.ErrNotFederated) || asset != nil || metadata != nil {
					t.Fatalf("not refused: %v", err)
				}
			})
		}
	}
	t.Run("a cancelled context", func(t *testing.T) {
		f := f.sub(t)
		a, _ := f.eligible("Title", "", "", "")
		u := f.metaUpdate(f.recordMetadata(a))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		asset, metadata, err := f.src.MetadataUpdateMembers(ctx, u.id, f.peer, u.objectURI, u.raw)
		if err == nil || errors.Is(err, federation.ErrNotFederated) || asset != nil || metadata != nil {
			t.Fatalf("a read error: %v", err)
		}
	})
}

// C1: a peer missing a profile is refused as profile_not_negotiated,
// naming the token, a four-null clear included; a disabled peer is a
// plain refusal.
func TestMetadataUpdate_AMissingProfileKeepsItsReason(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	for _, token := range []string{federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1} {
		for name, title := range map[string]string{"ordinary": "Secret title", "four-null": ""} {
			t.Run(token+", "+name, func(t *testing.T) {
				f := f.sub(t)
				f.withMetadataProfile(t)
				a, _ := f.eligible(title, "", "", "")
				u := f.metaUpdate(f.recordMetadata(a))
				f.exec(`UPDATE federation_peers SET capabilities = capabilities - $2::text WHERE id = $1`, f.peer, token)
				asset, metadata, err := f.metaMembers(u)
				if !errors.Is(err, federation.ErrNotFederated) || !errors.Is(err, federation.ErrProfileNotNegotiated) || asset != nil || metadata != nil {
					t.Fatalf("%v", err)
				}
				if !strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "Secret") {
					t.Fatalf("the reason: %v", err)
				}
			})
		}
	}
	t.Run("a disabled peer", func(t *testing.T) {
		f := f.sub(t)
		f.withMetadataProfile(t)
		a, _ := f.eligible("Title", "", "", "")
		u := f.metaUpdate(f.recordMetadata(a))
		f.exec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peer)
		_, _, err := f.metaMembers(u)
		if !errors.Is(err, federation.ErrNotFederated) || errors.Is(err, federation.ErrProfileNotNegotiated) {
			t.Fatalf("%v", err)
		}
	})
}

// #1595: a metadata Update's aa:asset names the original only from the
// recorded sealed version's upload_request base name; a client_reported
// backfill and an unknown name are omitted, and a retry builds the same
// bytes.
func TestMetadataUpdate_TheOriginalFilenameComesOnlyFromTheUploadRequest(t *testing.T) {
	f := newFx(t)
	f.withMetadataProfile(t)
	for _, c := range basenameCases {
		t.Run(c.name, func(t *testing.T) {
			a := f.newAssetNamed("public", true, c.basename, c.source)
			f.describe(a, "Harbour at dawn", "Body", goodRights, "Studio A")
			g := f.grantAndRecord(a, nil)
			f.shareDelivered(g, "sent")
			id := f.recordMetadata(a)
			if id == uuid.Nil {
				t.Fatal("nothing recorded")
			}
			u := f.metaUpdate(id)
			asset, metadata, err := f.metaMembers(u)
			if err != nil {
				t.Fatal(err)
			}
			f.wantOriginalFilename("the metadata Update", asset, c.want)
			again, againMeta, err := f.metaMembers(u)
			if err != nil || !bytes.Equal(again, asset) || !bytes.Equal(againMeta, metadata) {
				t.Fatalf("a retry: %v", err)
			}
		})
	}
}
