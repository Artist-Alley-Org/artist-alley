// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetingest_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

// #1577 (spec v1 §18.4): an aa:AssetVersion accepted while asset-metadata-1
// is negotiated keeps an initial snapshot of its baseline claims, from the
// original delivery, with its record and revision. Domain tests: the
// signatures are declared random values and the caller is assumed to have
// verified them; this is not the signed two-instance gate.

// initialV is announceV (version n of a, with files) carrying the given
// archival-1 descriptive members, signed, published at p.
func (f *fx) initialV(a *asset, n int, files []map[string]any, extra map[string]any, p time.Time) *federation.Envelope {
	f.t.Helper()
	pred := ""
	if n > 1 {
		pred = f.versionURI(a, n-1)
	}
	m := a.memberAt(f.versionURI(a, n), n, pred, files)
	for k, v := range extra {
		m[k] = v
	}
	e := f.envelope(a, m)
	e.Published = p
	mdSign(e)
	return e
}

// snapshotOf returns the kept snapshot of the activity, its provenance as
// one comparable string, and whether there is one.
func (f *fx) snapshotOf(activity string) (snapshot []byte, row string, ok bool) {
	f.t.Helper()
	err := f.pool.QueryRow(context.Background(), `SELECT m.snapshot::text, to_jsonb(m)::text
		  FROM federation_received_asset_metadata m JOIN federation_received_asset_revisions r ON r.id = m.revision_id
		 WHERE r.peer_id = $1 AND r.activity_uri = $2`, f.peerID, activity).Scan(&snapshot, &row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false
	}
	if err != nil {
		f.t.Fatalf("reading the snapshot of %s: %v", activity, err)
	}
	return snapshot, row, true
}

func (f *fx) records(a *asset) int {
	return f.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND origin_asset_uri = $2`, f.peerID, a.uri)
}

func (f *fx) pinCount(a *asset) int {
	return f.count(`SELECT count(*) FROM storage_pins WHERE object_hash = ANY($1)`, a.hash)
}

// otherSigner is another local identity than the fixture's.
func otherSigner() casfetch.Signer {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	return casfetch.InstanceSigner(&outbox.IdentitySigner{PrivateKey: priv, KeyURL: localBase + "/users/bob#main-key"})
}

var mdStatement = map[string]any{"label": map[string]any{"en": []string{"Attribution"}}, "value": map[string]any{"none": []string{"Studio A"}}}

func TestInitialSnapshot_KeptAtScheduleAndCurrentOnceVerified(t *testing.T) {
	f := metaFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	p := time.Date(2026, 11, 23, 9, 0, 0, 987654321, time.UTC)
	e := f.initialV(a, 1, a.files, map[string]any{
		"description": map[string]any{"title": "<b>Harbour</b>", "description": "Oil on board.\n", "creator": []string{"A. Painter"},
			"subject": []string{"sea"}, "date": "1901", "language": "en"},
		"rights": mdCC, "requiredStatement": mdStatement,
		"identifiers":     []map[string]string{{"scheme": "local", "value": "INV-1"}},
		"accessionNumber": "A-1", "iiifManifest": f.origin.srv.URL + "/iiif/" + a.id.String() + "/manifest.json",
	}, p)
	sched, err := f.schedule(e)
	if err != nil || sched.State != assetingest.StatePending {
		t.Fatalf("scheduled: %+v %v", sched, err)
	}
	snap, before, ok := f.snapshotOf(e.ID)
	if !ok || f.records(a) != 1 || f.revisions(a) != 1 || f.snapshots(a) != 1 {
		t.Fatalf("after Schedule: snapshot %v, %d records, %d revisions, %d snapshots", ok, f.records(a), f.revisions(a), f.snapshots(a))
	}
	var got map[string]any
	_ = json.Unmarshal(snap, &got)
	want := map[string]any{"title": "<b>Harbour</b>", "description": "Oil on board.\n", "rights": mdCC,
		"requiredStatement": map[string]any{"label": map[string]any{"en": []any{"Attribution"}}, "value": map[string]any{"none": []any{"Studio A"}}}}
	if !jsonEqual(got, want) {
		t.Fatalf("initial snapshot %s: only the four baseline claims, exactly", snap)
	}
	var secs int64
	var nanos int32
	var origin, value, recipient string
	if err := f.pool.QueryRow(ctx, `SELECT published_seconds, published_nanos, origin_signer_key_id, origin_signature_value, recipient_signer_key_id
		  FROM federation_received_asset_metadata m JOIN federation_received_asset_revisions r ON r.id = m.revision_id
		 WHERE r.activity_uri = $1`, e.ID).Scan(&secs, &nanos, &origin, &value, &recipient); err != nil {
		t.Fatal(err)
	}
	if secs != p.Unix() || nanos != 987654321 || origin != e.Actor+"#main-key" || value != e.Signature.Value || recipient != f.signer.KeyID {
		t.Fatalf("provenance %d.%09d %q %q %q", secs, nanos, origin, value, recipient)
	}
	if m := f.currentMeta(a); m != nil {
		t.Fatalf("a pending version's snapshot is current: %+v", m)
	}
	if out, err := f.process(sched.RecordID); err != nil || out.State != assetingest.StateComplete {
		t.Fatalf("processed: %+v %v", out, err)
	}
	m := f.currentMeta(a)
	if m == nil || m.ActivityURI != e.ID || *m.Snapshot.Title != "<b>Harbour</b>" || !m.Published.Equal(p) {
		t.Fatalf("after verification: %+v", m)
	}
	// Verification rewrote nothing and added no revision; a restarted
	// service derives the same current snapshot from the database.
	if _, after, _ := f.snapshotOf(e.ID); after != before || f.revisions(a) != 1 {
		t.Fatalf("verification changed the snapshot or added a revision:\n%s\n%s", before, after)
	}
	restarted := assetingest.New(assetingest.Deps{Pool: f.pool, Storage: f.svc, Peers: peers{pool: f.pool}, Grants: f.grants, Fetcher: f.client})
	if r, err := restarted.CurrentMetadata(ctx, f.peerID, a.uri); err != nil || r == nil || r.RevisionID != m.RevisionID {
		t.Fatalf("after a restart: %+v %v", r, err)
	}
}

func TestInitialSnapshot_BaselineShapes(t *testing.T) {
	f := metaFx(t)
	p := time.Now().UTC()
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  string
	}{
		{"no descriptive claim: four nulls", nil, `{"title":null,"description":null,"rights":null,"requiredStatement":null}`},
		{"other DCMI terms only: four nulls", map[string]any{"description": map[string]any{"creator": []string{"A"}, "date": "1901"}},
			`{"title":null,"description":null,"rights":null,"requiredStatement":null}`},
		{"a title only", map[string]any{"description": map[string]any{"title": "  spaced  "}},
			`{"title":"  spaced  ","description":null,"rights":null,"requiredStatement":null}`},
		{"a statement in several languages", map[string]any{"requiredStatement": map[string]any{
			"label": map[string]any{"en": []string{"Credit"}, "fr": []string{"Crédit"}}, "value": map[string]any{"none": []string{"<i>A</i>", "B"}}}},
			`{"title":null,"description":null,"rights":null,"requiredStatement":{"label":{"en":["Credit"],"fr":["Crédit"]},"value":{"none":["<i>A</i>","B"]}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := f.newAsset(1)
			f.grant(f.peerID, a.uri, nil)
			e := f.initialV(a, 1, a.files, tc.extra, p)
			if out, err := f.receive(e); err != nil || out.State != assetingest.StateComplete {
				t.Fatalf("%+v %v", out, err)
			}
			snap, _, ok := f.snapshotOf(e.ID)
			var got, want any
			_ = json.Unmarshal(snap, &got)
			_ = json.Unmarshal([]byte(tc.want), &want)
			if !ok || !jsonEqual(got, want) {
				t.Fatalf("snapshot %s, want %s", snap, tc.want)
			}
			// Four nulls are a current snapshot, not its absence.
			if m := f.currentMeta(a); m == nil || m.ActivityURI != e.ID {
				t.Fatalf("current: %+v", m)
			}
		})
	}
}

// Claims archival-1 accepts are kept as accepted, although an aa:metadata
// Update carrying them would be refused (§18.2's bounds are an Update
// member's, not a baseline's).
func TestInitialSnapshot_LegacyClaimsBeyondUpdateBoundsAreKept(t *testing.T) {
	f := metaFx(t)
	long := make([]string, 9)
	for i := range long {
		long[i] = strings.Repeat(string(rune('a'+i)), 4096)
	}
	longRights := "https://creativecommons.org/licenses/" + strings.Repeat("b", 3000) + "/4.0/"
	for _, tc := range []struct {
		name  string
		extra map[string]any
		check func(t *testing.T, m *assetingest.MetadataRevision)
	}{
		{"nine 4,096-byte statement values", map[string]any{"requiredStatement": map[string]any{
			"label": map[string]any{"en": []string{"Attribution"}}, "value": map[string]any{"none": long}}},
			func(t *testing.T, m *assetingest.MetadataRevision) {
				if got := m.Snapshot.RequiredStatement.Value["none"]; !reflect.DeepEqual(got, long) {
					t.Fatalf("the statement was not kept exactly (%d values)", len(got))
				}
				if got := m.Snapshot.RequiredStatement.Label["en"]; !reflect.DeepEqual(got, []string{"Attribution"}) {
					t.Fatalf("the label was not kept exactly: %q", got)
				}
			}},
		{"a 3,000-byte rights URI", map[string]any{"rights": longRights},
			func(t *testing.T, m *assetingest.MetadataRevision) {
				if m.Snapshot.Rights == nil || *m.Snapshot.Rights != longRights {
					t.Fatal("the rights URI was not kept exactly")
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := f.newAsset(1)
			f.grant(f.peerID, a.uri, nil)
			e := f.initialV(a, 1, a.files, tc.extra, time.Now().UTC())
			if _, err := federation.ParseArchivalAsset(e.Extra[federation.MemberArchivalAsset]); err != nil {
				t.Fatalf("setup: archival-1 refuses the member: %v", err)
			}
			if out, err := f.receive(e); err != nil || out.State != assetingest.StateComplete {
				t.Fatalf("%+v %v", out, err)
			}
			snap, _, ok := f.snapshotOf(e.ID)
			if !ok {
				t.Fatal("no initial snapshot kept")
			}
			// The branch under test: beyond §18.2's member bounds.
			if tc.name == "nine 4,096-byte statement values" && len(snap) <= federation.AssetMetadataMaxMemberBytes {
				t.Fatalf("setup: the derived snapshot is %d bytes, not over %d", len(snap), federation.AssetMetadataMaxMemberBytes)
			}
			if tc.name == "a 3,000-byte rights URI" && len(longRights) <= federation.AssetMetadataMaxRightsBytes {
				t.Fatalf("setup: the rights URI is %d bytes, not over %d", len(longRights), federation.AssetMetadataMaxRightsBytes)
			}
			// As an Update member the same snapshot is refused.
			if _, err := federation.ParseAssetMetadata(snap); !errors.Is(err, federation.ErrMetadataInvalid) {
				t.Fatalf("setup: the derived snapshot (%d bytes) passes the Update rules: %v", len(snap), err)
			}
			m := f.currentMeta(a)
			if m == nil || m.ActivityURI != e.ID {
				t.Fatalf("current: %+v", m)
			}
			tc.check(t, m)
		})
	}
}

func TestInitialSnapshot_OrderWithUpdatesAndSuccessors(t *testing.T) {
	f := metaFx(t)
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	t0 := time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC)
	title := func(s string) map[string]any { return map[string]any{"description": map[string]any{"title": s}} }
	if out, err := f.receive(f.initialV(a, 1, a.files, title("A"), t0)); err != nil || out.State != assetingest.StateComplete {
		t.Fatalf("v1: %+v %v", out, err)
	}
	if got := f.currentTitle(a); got != "A" {
		t.Fatalf("after v1: %q", got)
	}
	if _, err := f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ("B"), `null`, `null`, `null`), t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if got := f.currentTitle(a); got != "B" {
		t.Fatalf("after the Update: %q", got)
	}
	// A successor announced later, still pending, replaces nothing.
	files2, _ := f.originals(1)
	v2 := f.initialV(a, 2, files2, title("C"), t0.Add(2*time.Minute))
	sched, err := f.schedule(v2)
	if err != nil || sched.State != assetingest.StatePending {
		t.Fatalf("v2 scheduled: %+v %v", sched, err)
	}
	if got := f.currentTitle(a); got != "B" {
		t.Fatalf("a pending successor's snapshot became current: %q", got)
	}
	if out, err := f.process(sched.RecordID); err != nil || out.State != assetingest.StateComplete {
		t.Fatalf("v2: %+v %v", out, err)
	}
	if got := f.currentTitle(a); got != "C" {
		t.Fatalf("after v2 completed: %q", got)
	}
	// An older reannouncement (a regrant) keeps its snapshot but resets
	// nothing newer; a flags-only Update adds no snapshot.
	snaps := f.snapshots(a)
	if out, err := f.receive(f.initialV(a, 2, files2, title("old"), t0.Add(90*time.Second))); err != nil || !out.Duplicate {
		t.Fatalf("an older reannouncement: %+v %v", out, err)
	}
	if _, err := f.applyUpdate(f.flagUpdate(a, 2, files2, true, "", t0.Add(3*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if got := f.currentTitle(a); got != "C" || f.snapshots(a) != snaps+1 {
		t.Fatalf("current %q, %d snapshots (want C, %d)", got, f.snapshots(a), snaps+1)
	}
}

func TestInitialSnapshot_DuplicatesAndReannouncements(t *testing.T) {
	f := metaFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	t0 := time.Date(2026, 11, 23, 15, 0, 0, 1000, time.UTC)
	v1 := f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "first"}}, t0)
	if out, err := f.receive(v1); err != nil || out.State != assetingest.StateComplete {
		t.Fatalf("v1: %+v %v", out, err)
	}
	_, firstRow, _ := f.snapshotOf(v1.ID)
	counts := func() string {
		return strings.Join([]string{f.localState(), string(rune('0' + f.revisions(a))), string(rune('0' + f.snapshots(a))),
			string(rune('0' + f.pinCount(a))), string(rune('0' + int(f.origin.getCount(a.hash[0]))))}, "|")
	}
	before := counts()
	resigned := *v1
	mdSign(&resigned)
	for name, e := range map[string]*federation.Envelope{"the same delivery": v1, "re-signed": &resigned} {
		if out, err := f.receive(e); err != nil || !out.Duplicate {
			t.Fatalf("%s: %+v %v", name, out, err)
		}
	}
	if _, row, _ := f.snapshotOf(v1.ID); row != firstRow || counts() != before {
		t.Fatalf("a duplicate changed something:\n%s\n%s", before, counts())
	}

	// A new activity announcing the held version (a regrant): a new
	// baseline, no fetch, no record of its own.
	re := f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "regrant"}}, t0.Add(time.Minute))
	fetches := f.origin.getCount(a.hash[0])
	if out, err := f.receive(re); err != nil || !out.Duplicate || out.State != assetingest.StateComplete {
		t.Fatalf("a reannouncement: %+v %v", out, err)
	}
	if f.origin.getCount(a.hash[0]) != fetches || f.records(a) != 1 {
		t.Fatal("a reannouncement fetched or wrote a record")
	}
	_, reRow, ok := f.snapshotOf(re.ID)
	if !ok || f.currentTitle(a) != "regrant" {
		t.Fatalf("the reannouncement's snapshot: %v, current %q", ok, f.currentTitle(a))
	}
	// The same reannouncement again: nothing more. Under its id, other
	// content, a publication one nanosecond later or another recipient is
	// not the same activity.
	revs, snaps := f.revisions(a), f.snapshots(a)
	reResigned := *re
	mdSign(&reResigned)
	for name, e := range map[string]*federation.Envelope{"again": re, "re-signed": &reResigned} {
		if out, err := f.receive(e); err != nil || !out.Duplicate {
			t.Fatalf("the reannouncement %s: %+v %v", name, out, err)
		}
	}
	if _, row, _ := f.snapshotOf(re.ID); row != reRow || f.revisions(a) != revs || f.snapshots(a) != snaps {
		t.Fatal("a repeated reannouncement changed its snapshot or added rows")
	}
	for _, tc := range []struct {
		name   string
		env    func() *federation.Envelope
		signer *casfetch.Signer
		reason string
	}{
		{"other descriptive content", func() *federation.Envelope {
			e := f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "changed"}}, re.Published)
			e.ID = re.ID
			return e
		}, nil, assetingest.ReasonActivityConflict},
		{"published one nanosecond later", func() *federation.Envelope {
			e := f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "regrant"}}, re.Published.Add(time.Nanosecond))
			e.ID = re.ID
			return e
		}, nil, assetingest.ReasonActivityConflict},
		{"another recipient", func() *federation.Envelope { e := *re; return &e },
			func() *casfetch.Signer { s := otherSigner(); return &s }(), casfetch.ReasonForeignSigner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sg := f.signer
			if tc.signer != nil {
				sg = *tc.signer
			}
			_, err := f.ingest.Schedule(ctx, assetingest.Announcement{PeerID: f.peerID, Envelope: tc.env(), Signer: sg})
			wantReason(t, err, tc.reason)
			if _, row, _ := f.snapshotOf(re.ID); row != reRow || f.revisions(a) != revs || f.snapshots(a) != snaps {
				t.Fatal("a conflicting reannouncement changed something")
			}
		})
	}
	t.Run("a published one nanosecond later under the first announcement's id", func(t *testing.T) {
		e := *v1
		e.Published = v1.Published.Add(time.Nanosecond)
		_, err := f.receive(&e)
		wantReason(t, err, assetingest.ReasonActivityConflict)
		if _, row, _ := f.snapshotOf(v1.ID); row != firstRow {
			t.Fatal("the first snapshot changed")
		}
	})
}

func TestInitialSnapshot_NoBackfill(t *testing.T) {
	f := newFx(t) // archival-1 and original-transfer-1 only
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	v1 := f.announceV(a, 1, a.files)
	if out, err := f.receive(v1); err != nil || out.State != assetingest.StateComplete {
		t.Fatalf("legacy v1: %+v %v", out, err)
	}
	re := f.announceV(a, 1, a.files) // a legacy reannouncement: a revision, no record of its own
	if out, err := f.receive(re); err != nil || !out.Duplicate {
		t.Fatalf("legacy reannouncement: %+v %v", out, err)
	}
	if f.snapshots(a) != 0 || f.revisions(a) != 2 {
		t.Fatalf("legacy: %d snapshots, %d revisions", f.snapshots(a), f.revisions(a))
	}
	// The peer negotiates asset-metadata-1 later; the same activities,
	// delivered again (unsigned, as first sent), and a restart, add nothing.
	f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peerID, allProfiles)
	restarted := assetingest.New(assetingest.Deps{Pool: f.pool, Storage: f.svc, Peers: peers{pool: f.pool}, Grants: f.grants, Fetcher: f.client})
	for _, svc := range []*assetingest.Service{f.ingest, restarted} {
		for name, e := range map[string]*federation.Envelope{"v1": v1, "the reannouncement": re} {
			if out, err := svc.Receive(context.Background(), assetingest.Announcement{PeerID: f.peerID, Envelope: e, Signer: f.signer}); err != nil || !out.Duplicate {
				t.Fatalf("%s again after negotiation: %+v %v", name, out, err)
			}
		}
	}
	if f.snapshots(a) != 0 || f.revisions(a) != 2 || f.currentMeta(a) != nil {
		t.Fatalf("a legacy activity gained a snapshot: %d snapshots, %d revisions", f.snapshots(a), f.revisions(a))
	}
	// A genuinely new activity after negotiation does.
	fresh := f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "new"}}, time.Now().UTC())
	if out, err := f.receive(fresh); err != nil || !out.Duplicate {
		t.Fatalf("a new reannouncement: %+v %v", out, err)
	}
	if _, _, ok := f.snapshotOf(fresh.ID); !ok || f.currentTitle(a) != "new" {
		t.Fatal("a new activity after negotiation kept no snapshot")
	}
}

func TestInitialSnapshot_Refusals(t *testing.T) {
	f := metaFx(t)
	t.Run("without asset-metadata-1 it is a legacy announcement", func(t *testing.T) {
		f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peerID, bothProfiles)
		defer f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peerID, allProfiles)
		a := f.newAsset(1)
		f.grant(f.peerID, a.uri, nil)
		if out, err := f.receive(f.announceV(a, 1, a.files)); err != nil || out.State != assetingest.StateComplete {
			t.Fatalf("%+v %v", out, err)
		}
		if f.snapshots(a) != 0 || f.revisions(a) != 1 {
			t.Fatalf("%d snapshots, %d revisions", f.snapshots(a), f.revisions(a))
		}
	})
	for _, tc := range []struct {
		name   string
		mutate func(e *federation.Envelope)
		reason string
	}{
		{"no signature", func(e *federation.Envelope) { e.Signature = nil }, casfetch.ReasonInvalidRequest},
		{"another key", func(e *federation.Envelope) { e.Signature.PublicKey = e.Actor + "-other#main-key" }, casfetch.ReasonInvalidRequest},
		{"a short value", func(e *federation.Envelope) { e.Signature.Value = "c2ln" }, casfetch.ReasonInvalidRequest},
		{"no published", func(e *federation.Envelope) { e.Published = time.Time{} }, casfetch.ReasonInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := f.newAsset(1)
			f.grant(f.peerID, a.uri, nil)
			e := f.initialV(a, 1, a.files, nil, time.Now().UTC())
			tc.mutate(e)
			before := f.localState()
			_, err := f.receive(e)
			wantReason(t, err, tc.reason)
			if f.records(a) != 0 || f.revisions(a) != 0 || f.snapshots(a) != 0 || f.origin.getCount(a.hash[0]) != 0 {
				t.Fatal("a refused announcement kept or fetched something")
			}
			if after := f.localState(); after != before {
				t.Fatalf("local state changed:\n%s\n%s", before, after)
			}
		})
	}
	t.Run("a disabled peer", func(t *testing.T) {
		f.exec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peerID)
		defer f.exec(`UPDATE federation_peers SET enabled = TRUE WHERE id = $1`, f.peerID)
		a := f.newAsset(1)
		_, err := f.receive(f.initialV(a, 1, a.files, nil, time.Now().UTC()))
		wantReason(t, err, casfetch.ReasonPeerUnavailable)
	})
	// The profile dropped after the peer was read, before the binding
	// transaction: the capture is refused, never downgraded to legacy.
	for _, tc := range []struct{ name, change, restore, reason string }{
		{"asset-metadata-1 dropped while deciding", `UPDATE federation_peers SET capabilities = '` + bothProfiles + `'::jsonb WHERE id = $1`,
			`UPDATE federation_peers SET capabilities = '` + allProfiles + `'::jsonb WHERE id = $1`, casfetch.ReasonProfile},
		{"disconnected while deciding", `UPDATE federation_peers SET status = 'pending_outbound' WHERE id = $1`,
			`UPDATE federation_peers SET status = 'connected' WHERE id = $1`, casfetch.ReasonPeerUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := f.newAsset(1)
			f.grant(f.peerID, a.uri, nil)
			validated, proceed := make(chan struct{}), make(chan struct{})
			var once sync.Once
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			svc := assetingest.New(assetingest.Deps{Pool: f.pool, Storage: f.svc, Grants: f.grants, Fetcher: f.client,
				Audit: audit.NewRecorder(f.pool, logger), Logger: logger,
				Peers: hookedPeers{inner: peers{pool: f.pool}, after: func() { once.Do(func() { close(validated); <-proceed }) }}})
			e := f.initialV(a, 1, a.files, nil, time.Now().UTC())
			done := make(chan error, 1)
			go func() {
				_, err := svc.Schedule(context.Background(), assetingest.Announcement{PeerID: f.peerID, Envelope: e, Signer: f.signer})
				done <- err
			}()
			<-validated
			f.exec(tc.change, f.peerID)
			defer f.exec(tc.restore, f.peerID)
			close(proceed)
			wantReason(t, <-done, tc.reason)
			if f.records(a) != 0 || f.revisions(a) != 0 || f.snapshots(a) != 0 {
				t.Fatal("kept after the peer changed")
			}
		})
	}
}

func TestInitialSnapshot_GrantPolicy(t *testing.T) {
	f := metaFx(t).withWake()
	ctx := context.Background()
	t.Run("before the grant: kept pending, current only after grant and verification", func(t *testing.T) {
		a := f.newAsset(1)
		e := f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "late"}}, time.Now().UTC())
		out, err := f.receive(e)
		wantReason(t, err, assetingest.ReasonAwaitingGrant)
		if _, _, ok := f.snapshotOf(e.ID); !ok || f.currentMeta(a) != nil || f.origin.getCount(a.hash[0]) != 0 {
			t.Fatalf("waiting: snapshot kept %v, current %+v, fetches %d", ok, f.currentMeta(a), f.origin.getCount(a.hash[0]))
		}
		f.grant(f.peerID, a.uri, nil)
		res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor())
		if err != nil || res.State != assetingest.StateComplete || f.currentTitle(a) != "late" {
			t.Fatalf("after the grant: %+v %v, current %q", res, err, f.currentTitle(a))
		}
	})
	for _, tc := range []struct {
		name   string
		setup  func(a *asset)
		reason string
	}{
		{"a view-only grant", func(a *asset) {
			if _, err := f.grants.ApplyShare(ctx, inboundgrants.Share{
				PeerID: f.peerID, ActivityURI: f.origin.srv.URL + "/activities/share-" + uuid.NewString(),
				GrantorActorURI: f.origin.srv.URL + "/users/alice", ObjectKind: federation.ShareObjectKindAsset,
				ObjectURI: a.uri, Scope: federation.ShareScopeView,
			}); err != nil {
				t.Fatal(err)
			}
		}, casfetch.ReasonNoInboundGrant},
		{"a revoked grant", func(a *asset) { f.revoke(f.peerID, a.uri, f.grant(f.peerID, a.uri, nil)) }, casfetch.ReasonNoInboundGrant},
		{"a direct grant expired on arrival", func(a *asset) {
			var past time.Time
			if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp() - interval '1 second'`).Scan(&past); err != nil {
				t.Fatal(err)
			}
			f.grant(f.peerID, a.uri, &past)
		}, casfetch.ReasonNoInboundGrant},
		{"a withdrawn asset", func(a *asset) {
			f.grant(f.peerID, a.uri, nil)
			if _, err := f.withdraw(f.deleteOf(a, f.tombstone(a, ""))); err != nil {
				t.Fatal(err)
			}
		}, assetingest.ReasonWithdrawn},
	} {
		t.Run(tc.name+": rejected, no revision or snapshot", func(t *testing.T) {
			a := f.newAsset(1)
			tc.setup(a)
			e := f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "no"}}, time.Now().UTC())
			_, err := f.receive(e)
			wantReason(t, err, tc.reason)
			if f.revisions(a) != 0 || f.snapshots(a) != 0 || f.origin.getCount(a.hash[0]) != 0 {
				t.Fatalf("%d revisions, %d snapshots, %d fetches", f.revisions(a), f.snapshots(a), f.origin.getCount(a.hash[0]))
			}
			// A known decision is a rejected record, never a wait.
			if n := f.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2
				AND state = 'rejected' AND reason = $3`, f.peerID, e.ID, tc.reason); n != 1 || f.records(a) != 1 {
				t.Fatalf("%d rejected %s records (of %d), want the one", n, tc.reason, f.records(a))
			}
		})
	}
	t.Run("a version held with other bytes: refused, nothing kept", func(t *testing.T) {
		a := f.newAsset(1)
		f.grant(f.peerID, a.uri, nil)
		if out, err := f.receive(f.initialV(a, 1, a.files, nil, time.Now().UTC())); err != nil || out.State != assetingest.StateComplete {
			t.Fatalf("v1: %+v %v", out, err)
		}
		revs, snaps := f.revisions(a), f.snapshots(a)
		other, _ := f.originals(1)
		_, err := f.receive(f.initialV(a, 1, other, nil, time.Now().UTC()))
		wantReason(t, err, assetingest.ReasonVersionConflict)
		if f.revisions(a) != revs || f.snapshots(a) != snaps {
			t.Fatal("a conflicting version kept a revision or snapshot")
		}
	})
	t.Run("after a revocation: a new reannouncement is refused, the old one stays a duplicate", func(t *testing.T) {
		a := f.newAsset(1)
		share := f.grant(f.peerID, a.uri, nil)
		v1 := f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "kept"}}, time.Now().UTC())
		if out, err := f.receive(v1); err != nil || out.State != assetingest.StateComplete {
			t.Fatalf("v1: %+v %v", out, err)
		}
		f.revoke(f.peerID, a.uri, share)
		before := f.localState()
		revs, snaps, fetches := f.revisions(a), f.snapshots(a), f.origin.getCount(a.hash[0])
		_, err := f.receive(f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "novel"}}, time.Now().UTC().Add(time.Second)))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		if out, err := f.receive(v1); err != nil || !out.Duplicate {
			t.Fatalf("the historical duplicate: %+v %v", out, err)
		}
		if f.revisions(a) != revs || f.snapshots(a) != snaps || f.origin.getCount(a.hash[0]) != fetches || f.currentTitle(a) != "kept" {
			t.Fatalf("after the revocation: %d/%d revisions, %d/%d snapshots, current %q", f.revisions(a), revs, f.snapshots(a), snaps, f.currentTitle(a))
		}
		if after := f.localState(); after != before {
			t.Fatalf("local state changed:\n%s\n%s", before, after)
		}
	})
	t.Run("legacy: without the profile a reannouncement needs no grant", func(t *testing.T) {
		g := newFx(t)
		a := g.newAsset(1)
		share := g.grant(g.peerID, a.uri, nil)
		if out, err := g.receive(g.announceV(a, 1, a.files)); err != nil || out.State != assetingest.StateComplete {
			t.Fatalf("v1: %+v %v", out, err)
		}
		g.revoke(g.peerID, a.uri, share)
		if out, err := g.receive(g.announceV(a, 1, a.files)); err != nil || !out.Duplicate || g.revisions(a) != 2 {
			t.Fatalf("a legacy reannouncement after revocation: %+v %v (%d revisions)", out, err, g.revisions(a))
		}
	})
}

func TestInitialSnapshot_RecordRevisionAndSnapshotAreOne(t *testing.T) {
	f := metaFx(t)
	marker := "FAIL-" + uuid.NewString()[:8]
	fn := "md_init_fail_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	f.exec(`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.snapshot->>'title' = '` + marker + `' THEN RAISE EXCEPTION 'test: refuse this snapshot'; END IF; RETURN NEW; END; $$`)
	f.exec(`CREATE TRIGGER ` + fn + ` BEFORE INSERT ON federation_received_asset_metadata FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fn+` ON federation_received_asset_metadata`)
		_, _ = f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)
	})
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	before := f.localState()
	_, err := f.receive(f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": marker}}, time.Now().UTC()))
	if err == nil || !assetingest.Retryable(err) {
		t.Fatalf("a failed initial snapshot insert: %v", err)
	}
	if f.records(a) != 0 || f.revisions(a) != 0 || f.snapshots(a) != 0 || f.origin.getCount(a.hash[0]) != 0 || f.pinCount(a) != 0 {
		t.Fatalf("partial admission: %d records, %d revisions, %d snapshots", f.records(a), f.revisions(a), f.snapshots(a))
	}
	if after := f.localState(); after != before {
		t.Fatalf("local state changed:\n%s\n%s", before, after)
	}
	// The same trigger lets another announcement through.
	if out, err := f.receive(f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "fine"}}, time.Now().UTC())); err != nil ||
		out.State != assetingest.StateComplete || f.currentTitle(a) != "fine" {
		t.Fatalf("the control announcement: %+v %v", out, err)
	}
}

// TestInitialSnapshot_OnlyOnTheRevisionItInserted: a new announcement
// under asset-metadata-1 is admitted only with its initial snapshot. A
// test-only trigger turns this activity's revision INSERT into a different
// row written by a nested INSERT (same transaction, same received_at) and
// suppresses the outer one, which then affects no row: the whole
// admission is refused and rolled back. A controlled invariant test, not
// an observed production race.
func TestInitialSnapshot_OnlyOnTheRevisionItInserted(t *testing.T) {
	f := metaFx(t)
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	e := f.initialV(a, 1, a.files, map[string]any{"description": map[string]any{"title": "diverted"}}, time.Now().UTC())
	fn := "md_init_divert_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	f.exec(`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF pg_trigger_depth() = 1 AND NEW.activity_uri = '` + e.ID + `' THEN
				NEW.mature := NOT NEW.mature;
				NEW.record := jsonb_set(NEW.record, '{mature}', to_jsonb(NEW.mature));
				INSERT INTO federation_received_asset_revisions SELECT (NEW).*;
				RETURN NULL;
			END IF;
			RETURN NEW;
		END; $$`)
	f.exec(`CREATE TRIGGER ` + fn + ` BEFORE INSERT ON federation_received_asset_revisions FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fn+` ON federation_received_asset_revisions`)
		_, _ = f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)
	})
	before := f.localState()
	out, err := f.schedule(e)
	t.Logf("outcome %+v err %v: %d records, %d revisions, %d snapshots", out, err, f.records(a), f.revisions(a), f.snapshots(a))
	wantReason(t, err, assetingest.ReasonActivityConflict)
	if out.RecordID != uuid.Nil {
		t.Fatalf("a record id was returned for a refused announcement: %+v", out)
	}
	if f.records(a) != 0 || f.revisions(a) != 0 || f.snapshots(a) != 0 || f.pinCount(a) != 0 || f.origin.getCount(a.hash[0]) != 0 {
		t.Fatalf("not rolled back: %d records, %d revisions, %d snapshots, %d pins", f.records(a), f.revisions(a), f.snapshots(a), f.pinCount(a))
	}
	if after := f.localState(); after != before {
		t.Fatalf("local state changed:\n%s\n%s", before, after)
	}
	// Another announcement is untouched by the trigger and admitted with its snapshot.
	b := f.newAsset(1)
	f.grant(f.peerID, b.uri, nil)
	control := f.initialV(b, 1, b.files, map[string]any{"description": map[string]any{"title": "control"}}, time.Now().UTC())
	if c, err := f.schedule(control); err != nil || c.State != assetingest.StatePending || c.RecordID == uuid.Nil {
		t.Fatalf("the control announcement: %+v %v", c, err)
	}
	if _, _, ok := f.snapshotOf(control.ID); !ok || f.revisions(b) != 1 {
		t.Fatal("the control announcement kept no snapshot")
	}
}
