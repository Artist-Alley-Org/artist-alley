// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetsend_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
)

// #1575: an Update of an asset's mature and aiProvenance flags, and the
// flags an announcement carries fixed when it is recorded.

// changeFlags sets a's flags and records their Updates in one transaction,
// as assets.Handler.UpdateAsset does; fail makes it roll back after.
func (f *fx) changeFlags(a asset, mature bool, ai string, fail error) ([]uuid.UUID, error) {
	f.t.Helper()
	ctx := context.Background()
	var ids []uuid.UUID
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE assets SET mature = $2, ai_provenance = NULLIF($3, '') WHERE id = $1`, a.id, mature, ai); err != nil {
			return err
		}
		var err error
		if ids, err = assetsend.RecordFlagUpdates(ctx, tx, f.writer, assetsend.FlagChange{AssetID: a.id, BaseURL: base,
			Username: func(context.Context, int64) string { return "owner" }}); err != nil {
			return err
		}
		return fail
	})
	return ids, err
}

// flagMember is what Source.FlagUpdateMember answers for Update u to peer.
func (f *fx) flagMember(u withdrawal, peer uuid.UUID) (json.RawMessage, error) {
	return f.src.FlagUpdateMember(context.Background(), u.id, peer, u.objectURI, u.raw)
}

func (f *fx) parsed(raw json.RawMessage) *federation.ArchivalAsset {
	f.t.Helper()
	m, err := federation.ParseArchivalAsset(raw)
	if err != nil {
		f.t.Fatalf("a member §17.2 refuses: %v (%s)", err, raw)
	}
	return m
}

func (f *fx) published(id uuid.UUID) time.Time {
	f.t.Helper()
	var p time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT published_at FROM activities WHERE id = $1`, id).Scan(&p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func TestMember_TheFlagsAreFixedWhenTheAnnouncementIsRecorded(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	g := f.grantAndRecord(a, nil)
	f.shareDelivered(g, "sent")
	before, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, g.payload)
	if err != nil {
		t.Fatal(err)
	}
	if m := f.parsed(before); m.Mature == nil || *m.Mature || m.AIProvenance != "" {
		t.Fatalf("recorded flags: %s", before)
	}
	// The flags change after the announcement was recorded: a redelivery
	// of it sends the same bytes.
	if _, err := f.pool.Exec(context.Background(), `UPDATE assets SET mature = TRUE, ai_provenance = 'generated' WHERE id = $1`, a.id); err != nil {
		t.Fatal(err)
	}
	after, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, g.payload)
	if err != nil || string(after) != string(before) {
		t.Fatalf("a redelivery after a flag change sent %s (%v), want %s", after, err, before)
	}
	// A record from before the flags were recorded reads the live ones.
	var legacy map[string]any
	_ = json.Unmarshal(g.payload, &legacy)
	delete(legacy, "mature")
	delete(legacy, "ai_provenance")
	raw, _ := json.Marshal(legacy)
	live, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, raw)
	if err != nil {
		t.Fatal(err)
	}
	if m := f.parsed(live); m.Mature == nil || !*m.Mature || m.AIProvenance != "generated" {
		t.Fatalf("a legacy record's flags: %s", live)
	}
	// A new announcement records the new flags.
	ids, err := f.sealNext(&a, 2, true, nil)
	if err != nil || len(ids) != 1 {
		t.Fatalf("version 2: %v %v", ids, err)
	}
	var p map[string]any
	_ = json.Unmarshal(f.withdrawalOf(ids[0]).raw, &p)
	if p["mature"] != true || p["ai_provenance"] != "generated" {
		t.Fatalf("version 2's record: %v", p)
	}
}

func TestRecordFlagUpdates_OnlyToPeersToldTheAssetUnderALiveShare(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	sent := f.announceTo(a, f.peer, "sent")
	inflight := f.addPeer()
	gi := f.announceTo(a, inflight, "inflight")
	revoked := f.addPeer()
	gr := f.announceTo(a, revoked, "sent")
	f.revokeShare(gr)
	unbuilt := f.addPeer()
	f.announceTo(a, unbuilt, "queued") // never built: nothing was claimed
	failed := f.addPeer()
	f.announceTo(a, failed, "failed") // built, but never delivered
	unannounced := f.addPeer()
	saved := f.peer
	f.peer = unannounced
	f.grantAndRecord(a, nil) // a live share, its announcement never built
	f.peer = saved

	ids, err := f.changeFlags(a, true, "assisted", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]withdrawal{}
	for _, id := range ids {
		u := f.withdrawalOf(id)
		got[u.str("share_id")] = u
	}
	if len(ids) != 2 || got[sent.share.String()].id == uuid.Nil || got[gi.share.String()].id == uuid.Nil {
		t.Fatalf("Updates %v, want one each to the sent and the in-flight peer", got)
	}
	for _, u := range got {
		if u.typ != string(federation.ActivityUpdate) || u.kind != "asset" || u.localID != a.id.String() ||
			u.payload["version_number"] != float64(1) || u.payload["mature"] != true || u.payload["ai_provenance"] != "assisted" ||
			u.str("sensitivity") != "public" {
			t.Fatalf("an Update's record: %s %s %s %v", u.typ, u.kind, u.localID, u.payload)
		}
	}

	t.Run("a restricted asset's change is told nobody", func(t *testing.T) {
		b := f.newAsset("public", true)
		f.announceTo(b, f.peer, "sent")
		if _, err := f.pool.Exec(context.Background(), `UPDATE assets SET sensitivity = 'restricted' WHERE id = $1`, b.id); err != nil {
			t.Fatal(err)
		}
		if ids, err := f.changeFlags(b, true, "", nil); err != nil || len(ids) != 0 {
			t.Fatalf("Updates of a restricted asset: %v %v", ids, err)
		}
	})
	t.Run("a change that rolls back records nothing", func(t *testing.T) {
		b := f.newAsset("public", true)
		f.announceTo(b, f.peer, "sent")
		if _, err := f.changeFlags(b, true, "", errors.New("the edit failed")); err == nil {
			t.Fatal("the failing edit committed")
		}
		var n int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM activities WHERE activity_type = 'Update' AND object_local_id = $1`,
			b.id.String()).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%d Updates after a rollback (%v)", n, err)
		}
	})
}

func TestRecordFlagUpdates_PublishedFollowsEveryEarlierAnnouncement(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	f.announceTo(a, f.peer, "sent")
	// An announcement of the asset stamped later than now (a clock step).
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	if err := pgx.BeginFunc(context.Background(), f.pool, func(tx pgx.Tx) error {
		em := emit.AssetVersion(emit.ActorContext{UserRef: f.owner, Username: "owner", BaseURL: base},
			emit.AssetVersionRef{AssetID: a.id, VersionID: a.version, ShareID: uuid.New(), ShareActivityID: uuid.New(), PeerURL: "https://x.example"})
		em.Activity.Published = future
		_, err := f.writer.RecordActivity(context.Background(), tx, em.Activity)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	u1, err := f.changeFlags(a, true, "", nil)
	if err != nil || len(u1) != 1 {
		t.Fatalf("first change: %v %v", u1, err)
	}
	if p := f.published(u1[0]); !p.Equal(future.Add(time.Microsecond)) {
		t.Fatalf("published %v, want %v (after the newest announcement)", p, future.Add(time.Microsecond))
	}
	u2, err := f.changeFlags(a, false, "", nil)
	if err != nil || len(u2) != 1 {
		t.Fatalf("second change: %v %v", u2, err)
	}
	if !f.published(u2[0]).After(f.published(u1[0])) {
		t.Fatal("the second Update is not after the first")
	}
}

func TestFlagUpdateMember_FixedWhenRecordedAndRefusedOnDrift(t *testing.T) {
	f := newFx(t)
	// updated is an asset told to f.peer with one Update of its flags.
	updated := func() (asset, grant, withdrawal) {
		t.Helper()
		a := f.newAsset("public", true)
		g := f.announceTo(a, f.peer, "sent")
		ids, err := f.changeFlags(a, true, "generated", nil)
		if err != nil || len(ids) != 1 {
			t.Fatalf("the change: %v %v", ids, err)
		}
		return a, g, f.withdrawalOf(ids[0])
	}
	a, _, u1 := updated()
	raw1, err := f.flagMember(u1, f.peer)
	if err != nil {
		t.Fatal(err)
	}
	m := f.parsed(raw1)
	if m.ID != a.uri || m.Version.Number != 1 || m.Mature == nil || !*m.Mature || m.AIProvenance != "generated" ||
		m.Sensitivity != "public" || len(m.Files) != 1 {
		t.Fatalf("the member: %s", raw1)
	}
	// A later change records its own Update; the first is sent unchanged.
	ids, err := f.changeFlags(a, false, "", nil)
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	if again, err := f.flagMember(u1, f.peer); err != nil || string(again) != string(raw1) {
		t.Fatalf("the first Update after a second change: %s (%v), want %s", again, err, raw1)
	}
	raw2, err := f.flagMember(f.withdrawalOf(ids[0]), f.peer)
	if m2 := f.parsed(raw2); err != nil || m2.Mature == nil || *m2.Mature || m2.AIProvenance != "" {
		t.Fatalf("the second Update: %s %v", raw2, err)
	}

	refused := func(name string, u withdrawal, raw []byte) {
		t.Helper()
		u.raw = raw
		if _, err := f.flagMember(u, f.peer); !errors.Is(err, federation.ErrNotFederated) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	{
		b, _, u := updated()
		if _, err := f.pool.Exec(context.Background(), `UPDATE assets SET sensitivity = 'team' WHERE id = $1`, b.id); err != nil {
			t.Fatal(err)
		}
		refused("the tier changed since", u, u.raw)
	}
	{
		b, _, u := updated()
		if _, err := f.pool.Exec(context.Background(), `UPDATE assets SET sensitivity = 'restricted' WHERE id = $1`, b.id); err != nil {
			t.Fatal(err)
		}
		refused("the asset is restricted now", u, u.raw)
	}
	{
		_, g, u := updated()
		f.revokeShare(g)
		refused("the share was revoked", u, u.raw)
	}
	{
		b, _, u := updated()
		if _, err := f.pool.Exec(context.Background(), `UPDATE assets SET deleted_at = now() WHERE id = $1`, b.id); err != nil {
			t.Fatal(err)
		}
		refused("the asset was deleted", u, u.raw)
	}
	{
		_, _, u := updated()
		var p map[string]any
		_ = json.Unmarshal(u.raw, &p)
		p["version_number"] = 2
		raw, _ := json.Marshal(p)
		refused("a version never claimed for the peer", u, raw)
		delete(p, "mature")
		raw, _ = json.Marshal(p)
		refused("a record without its flags", u, raw)
	}
}

// #1595: a flags Update re-announces the version the peer was told with
// that version's own upload_request base name only; a client_reported
// backfill and an unknown name are omitted, and a retry builds the same
// bytes.
func TestFlagUpdateMember_TheOriginalFilenameComesOnlyFromTheUploadRequest(t *testing.T) {
	f := newFx(t)
	for _, c := range basenameCases {
		t.Run(c.name, func(t *testing.T) {
			a := f.newAssetNamed("public", true, c.basename, c.source)
			f.announceTo(a, f.peer, "sent")
			ids, err := f.changeFlags(a, true, "generated", nil)
			if err != nil || len(ids) != 1 {
				t.Fatalf("the change: %v %v", ids, err)
			}
			u := f.withdrawalOf(ids[0])
			raw, err := f.flagMember(u, f.peer)
			if err != nil {
				t.Fatal(err)
			}
			if m := f.parsed(raw); m.Mature == nil || !*m.Mature || m.AIProvenance != "generated" {
				t.Fatalf("the member: %s", raw)
			}
			f.wantOriginalFilename("the Update", raw, c.want)
			if again, err := f.flagMember(u, f.peer); err != nil || string(again) != string(raw) {
				t.Fatalf("a retry: %v\n%s\nwant\n%s", err, again, raw)
			}
		})
	}
}
