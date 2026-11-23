// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetingest_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

// #1577 (spec v1 §18.4): an asset-metadata-1 Update's snapshot, kept with
// its revision, and the asset's current snapshot derived from what is
// kept. These are domain tests through Service.ApplyFlagUpdate with
// envelopes built here: they assume the inbox verified the signature, and
// are not a signed two-instance gate.

const allProfiles = `["share-lifecycle-1", "archival-1", "original-transfer-1", "asset-metadata-1"]`

const mdSecret = "MD-SECRET-9c1e"

// metaFx is fx whose peer also negotiated asset-metadata-1.
func metaFx(t *testing.T) *fx {
	t.Helper()
	f := newFx(t)
	f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peerID, allProfiles)
	return f
}

func (f *fx) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), q, args...); err != nil {
		f.t.Fatal(err)
	}
}

// mdSign gives e a declared signature of its actor's #main-key with a
// fresh 64-byte value (the domain trusts the inbox verified it).
func mdSign(e *federation.Envelope) {
	v := make([]byte, ed25519.SignatureSize)
	_, _ = rand.Read(v)
	e.Signature = &federation.Signature{Type: federation.SignatureAlgEd25519, PublicKey: e.Actor + "#main-key",
		Value: base64.RawURLEncoding.EncodeToString(v)}
}

// mdMember is a raw aa:metadata member of the four raw JSON values.
func mdMember(title, description, rights, statement string) json.RawMessage {
	return json.RawMessage(`{"title":` + title + `,"description":` + description + `,"rights":` + rights +
		`,"requiredStatement":` + statement + `}`)
}

func mdQ(s string) string { b, _ := json.Marshal(s); return string(b) }

const mdCC = "https://creativecommons.org/licenses/by/4.0/"
const mdStmt = `{"label":{"en":["Attribution"]},"value":{"none":["Studio A"]}}`

// metaUpdate is the origin's signed Update of a naming version n with
// files, carrying member as aa:metadata, published at p.
func (f *fx) metaUpdate(a *asset, n int, files []map[string]any, member json.RawMessage, p time.Time) *federation.Envelope {
	f.t.Helper()
	e := f.flagUpdate(a, n, files, false, "", p)
	e.Extra[federation.MemberAssetMetadata] = member
	mdSign(e)
	return e
}

// mdAnnounceBase is when the signed announcements here are published:
// before every Update these tests send, so an announcement's initial
// snapshot (#1577) is older than the Updates that follow it.
var mdAnnounceBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// signedV is announceV as a peer that negotiated asset-metadata-1 sends
// it: published at mdAnnounceBase + n seconds and signed by its actor, so
// its initial snapshot can be kept.
func (f *fx) signedV(a *asset, n int, files []map[string]any) *federation.Envelope {
	e := f.announceV(a, n, files)
	e.Published = mdAnnounceBase.Add(time.Duration(n) * time.Second)
	mdSign(e)
	return e
}

func (f *fx) snapshots(a *asset) int {
	return f.count(`SELECT count(*) FROM federation_received_asset_metadata m
		JOIN federation_received_asset_revisions r ON r.id = m.revision_id
		WHERE r.peer_id = $1 AND r.origin_asset_uri = $2`, f.peerID, a.uri)
}

func (f *fx) currentMeta(a *asset) *assetingest.MetadataRevision {
	f.t.Helper()
	m, err := f.ingest.CurrentMetadata(context.Background(), f.peerID, a.uri)
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fx) currentTitle(a *asset) string {
	f.t.Helper()
	m := f.currentMeta(a)
	switch {
	case m == nil:
		return "<none>"
	case m.Snapshot.Title == nil:
		return "<null>"
	}
	return *m.Snapshot.Title
}

// completeV1 is a granted asset whose version 1 is complete here.
func (f *fx) completeV1() *asset {
	f.t.Helper()
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	if v1, err := f.receive(f.signedV(a, 1, a.files)); err != nil || v1.State != assetingest.StateComplete {
		f.t.Fatalf("version 1: %+v %v", v1, err)
	}
	return a
}

// localState is what an Update must never change here.
func (f *fx) localState() string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT concat_ws(' ',
		(SELECT count(*) FROM federation_received_assets), (SELECT count(*) FROM federation_received_asset_files),
		(SELECT count(*) FROM storage_pins), (SELECT count(*) FROM field_definition),
		(SELECT count(*) FROM asset_field_value), (SELECT count(*) FROM assets),
		(SELECT string_agg(id::text || state, ',' ORDER BY id) FROM federation_received_assets WHERE peer_id = $1))`,
		f.peerID).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func TestMetadataUpdate_KeptWithItsRevision(t *testing.T) {
	f := metaFx(t)
	ctx := context.Background()
	a := f.completeV1()
	before := f.localState()
	fetches := f.origin.getCount(a.hash[0])
	p := time.Date(2026, 11, 23, 10, 0, 0, 123456789, time.UTC)
	member := mdMember(mdQ("<b>Harbour</b> at dusk"), mdQ("Oil on board.\n"), mdQ(mdCC), mdStmt)
	u := f.metaUpdate(a, 1, a.files, member, p)
	got, err := f.applyUpdate(u)
	if err != nil || got.RevisionID == uuid.Nil || got.Duplicate {
		t.Fatalf("a metadata Update: %+v %v", got, err)
	}
	// Version 1's announcement kept its initial snapshot; the Update adds one.
	if f.revisions(a) != 2 || f.snapshots(a) != 2 {
		t.Fatalf("%d revisions and %d snapshots, want 2 and 2", f.revisions(a), f.snapshots(a))
	}
	var snap []byte
	var secs int64
	var nanos int32
	var text, origin, algo, value, recipient string
	if err := f.pool.QueryRow(ctx, `SELECT snapshot, published_seconds, published_nanos, published_text, origin_signer_key_id,
		    origin_signature_algorithm, origin_signature_value, recipient_signer_key_id
		  FROM federation_received_asset_metadata WHERE revision_id = $1`, got.RevisionID).
		Scan(&snap, &secs, &nanos, &text, &origin, &algo, &value, &recipient); err != nil {
		t.Fatal(err)
	}
	var want, have any
	_ = json.Unmarshal(member, &want)
	_ = json.Unmarshal(snap, &have)
	if !jsonEqual(want, have) {
		t.Fatalf("snapshot kept as %s", snap)
	}
	if secs != p.Unix() || nanos != 123456789 || text != "2026-11-23T10:00:00.123456789Z" {
		t.Fatalf("published kept as %d.%09d %q", secs, nanos, text)
	}
	// The origin's declared signature and this instance's recipient identity
	// are kept apart, each under its own name.
	if origin != u.Actor+"#main-key" || algo != "Ed25519" || value != u.Signature.Value {
		t.Fatalf("origin signature kept as %q %q %q", origin, algo, value)
	}
	if recipient != f.signer.KeyID || recipient == origin || !strings.HasPrefix(recipient, localBase) {
		t.Fatalf("recipient kept as %q (origin %q)", recipient, origin)
	}

	m := f.currentMeta(a)
	if m == nil || m.RevisionID != got.RevisionID || m.Snapshot.Title == nil || *m.Snapshot.Title != "<b>Harbour</b> at dusk" ||
		*m.Snapshot.Description != "Oil on board.\n" || *m.Snapshot.Rights != mdCC || m.Snapshot.RequiredStatement.Value["none"][0] != "Studio A" {
		t.Fatalf("the current snapshot: %+v", m)
	}
	if !m.Published.Equal(p) || m.Published.Nanosecond() != 123456789 || m.ActivityURI != u.ID || m.ActorURI != u.Actor ||
		m.VersionURI != f.versionURI(a, 1) || m.OriginSignerKeyID != origin || m.RecipientSignerKeyID != recipient || m.PeerID != f.peerID {
		t.Fatalf("the current snapshot's provenance: %+v", m)
	}
	// Four nulls are a current snapshot, distinct from none.
	clear := f.metaUpdate(a, 1, a.files, mdMember(`null`, `null`, `null`, `null`), p.Add(time.Second))
	if _, err := f.applyUpdate(clear); err != nil {
		t.Fatal(err)
	}
	if m := f.currentMeta(a); m == nil || m.Snapshot.Title != nil || m.Snapshot.Description != nil || m.Snapshot.Rights != nil || m.Snapshot.RequiredStatement != nil {
		t.Fatalf("after a clear-all: %+v", m)
	}
	// A newer flags-only Update contributes no snapshot and clears nothing.
	if _, err := f.applyUpdate(f.flagUpdate(a, 1, a.files, true, "", p.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	if m := f.currentMeta(a); m == nil || m.ActivityURI != clear.ID {
		t.Fatalf("a flags-only Update changed the current snapshot: %+v", m)
	}
	if f.snapshots(a) != 3 || f.revisions(a) != 4 {
		t.Fatalf("%d snapshots and %d revisions", f.snapshots(a), f.revisions(a))
	}
	// Nothing local changed: no record, file, pin, field, value or asset,
	// and nothing was fetched.
	if after := f.localState(); after != before || f.origin.getCount(a.hash[0]) != fetches {
		t.Fatalf("local state changed:\n%s\n%s", before, after)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE federation_received_asset_metadata SET snapshot = '{}'::jsonb WHERE revision_id = $1`, got.RevisionID); err == nil {
		t.Fatal("a snapshot was rewritten")
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestMetadataUpdate_RefusalsKeepNothing(t *testing.T) {
	f := metaFx(t)
	a := f.completeV1()
	p := time.Now().UTC().Add(time.Second)
	valid := func() *federation.Envelope {
		return f.metaUpdate(a, 1, a.files, mdMember(mdQ("t"), `null`, `null`, `null`), p)
	}
	withMember := func(m string) func() *federation.Envelope {
		return func() *federation.Envelope {
			e := valid()
			e.Extra[federation.MemberAssetMetadata] = json.RawMessage(m)
			return e
		}
	}
	withdrawn := f.newAsset(1)
	f.grant(f.peerID, withdrawn.uri, nil)
	if v, err := f.receive(f.signedV(withdrawn, 1, withdrawn.files)); err != nil || v.State != assetingest.StateComplete {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := f.withdraw(f.deleteOf(withdrawn, f.tombstone(withdrawn, ""))); err != nil {
		t.Fatal(err)
	}
	ungranted := f.newAsset(1)
	cases := []struct {
		name   string
		target *asset
		env    func() *federation.Envelope
		reason string
	}{
		{"an empty title", a, withMember(string(mdMember(`""`, `null`, `null`, `null`))), assetingest.ReasonMetadataInvalid},
		{"a missing key", a, withMember(`{"title":null,"description":null,"rights":null}`), assetingest.ReasonMetadataInvalid},
		{"a repeated key", a, withMember(`{"title":"a","title":"b","description":null,"rights":null,"requiredStatement":null}`), assetingest.ReasonMetadataInvalid},
		{"a control character", a, withMember(string(mdMember(mdQ("a\rb"), `null`, `null`, `null`))), assetingest.ReasonMetadataInvalid},
		{"over the raw bound", a, withMember("{" + strings.Repeat(" ", 32768) + `"title":null,"description":null,"rights":null,"requiredStatement":null}`), assetingest.ReasonMetadataInvalid},
		{"a secret in an invalid rights value", a, withMember(string(mdMember(`null`, `null`, mdQ("https://"+mdSecret+".example/"), `null`))), assetingest.ReasonMetadataInvalid},
		{"a descriptive companion member", a, func() *federation.Envelope {
			e := valid()
			var m map[string]any
			_ = json.Unmarshal(e.Extra[federation.MemberArchivalAsset], &m)
			m["rights"] = mdCC
			e.Extra[federation.MemberArchivalAsset], _ = json.Marshal(m)
			return e
		}, assetingest.ReasonMetadataInvalid},
		{"no signature", a, func() *federation.Envelope { e := valid(); e.Signature = nil; return e }, casfetch.ReasonInvalidRequest},
		{"a signature of another key", a, func() *federation.Envelope {
			e := valid()
			e.Signature.PublicKey = f.origin.srv.URL + "/users/mallory#main-key"
			return e
		}, casfetch.ReasonInvalidRequest},
		{"a short signature value", a, func() *federation.Envelope { e := valid(); e.Signature.Value = "c2ln"; return e }, casfetch.ReasonInvalidRequest},
		{"a restricted member", a, func() *federation.Envelope {
			e := valid()
			var m map[string]any
			_ = json.Unmarshal(e.Extra[federation.MemberArchivalAsset], &m)
			m["sensitivity"] = "restricted"
			e.Extra[federation.MemberArchivalAsset], _ = json.Marshal(m)
			return e
		}, casfetch.ReasonSensitivity},
		{"an actor on another origin", a, func() *federation.Envelope {
			e := valid()
			e.Actor = "https://elsewhere.example/users/alice"
			mdSign(e)
			return e
		}, casfetch.ReasonForeignOrigin},
		{"a version held here with other bytes", a, func() *federation.Envelope {
			other, _ := f.originals(1)
			return f.metaUpdate(a, 1, other, mdMember(mdQ("t"), `null`, `null`, `null`), p)
		}, assetingest.ReasonVersionConflict},
		{"no grant", ungranted, func() *federation.Envelope {
			return f.metaUpdate(ungranted, 1, ungranted.files, mdMember(mdQ("t"), `null`, `null`, `null`), p)
		}, casfetch.ReasonNoInboundGrant},
		{"a withdrawn asset", withdrawn, func() *federation.Envelope {
			return f.metaUpdate(withdrawn, 1, withdrawn.files, mdMember(mdQ("t"), `null`, `null`, `null`), p)
		}, assetingest.ReasonWithdrawn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			revs, snaps := f.revisions(tc.target), f.snapshots(tc.target)
			_, err := f.applyUpdate(tc.env())
			wantReason(t, err, tc.reason)
			if err != nil && strings.Contains(err.Error(), mdSecret) {
				t.Fatalf("the refusal carries the claim: %v", err)
			}
			if f.revisions(tc.target) != revs || f.snapshots(tc.target) != snaps {
				t.Fatal("a refused metadata Update kept a revision or a snapshot")
			}
		})
	}
	// Profiles: each of the three missing from the stored intersection.
	for _, missing := range []string{"asset-metadata-1", "archival-1", "original-transfer-1"} {
		t.Run("without "+missing, func(t *testing.T) {
			caps := strings.Replace(allProfiles, `, "`+missing+`"`, "", 1)
			f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peerID, caps)
			defer f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peerID, allProfiles)
			revs := f.revisions(a)
			_, err := f.applyUpdate(valid())
			wantReason(t, err, casfetch.ReasonProfile)
			if f.revisions(a) != revs {
				t.Fatal("kept without the profile")
			}
		})
	}
	t.Run("a disabled peer", func(t *testing.T) {
		f.exec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peerID)
		defer f.exec(`UPDATE federation_peers SET enabled = TRUE WHERE id = $1`, f.peerID)
		_, err := f.applyUpdate(valid())
		wantReason(t, err, casfetch.ReasonPeerUnavailable)
	})
	// The flags-only Update is unchanged: no new token, no signature needed.
	t.Run("a flags-only Update needs neither", func(t *testing.T) {
		f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peerID, bothProfiles)
		defer f.exec(`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peerID, allProfiles)
		if _, err := f.applyUpdate(f.flagUpdate(a, 1, a.files, true, "", p)); err != nil {
			t.Fatalf("a flags-only Update: %v", err)
		}
	})
}

// hookedPeers runs after once ByID has answered: the stored peer row it
// read is then changed before the admitting transaction starts.
type hookedPeers struct {
	inner peers
	after func()
}

func (h hookedPeers) ByID(ctx context.Context, id uuid.UUID) (casfetch.Peer, error) {
	p, err := h.inner.ByID(ctx, id)
	if h.after != nil {
		h.after()
	}
	return p, err
}

// TestMetadataUpdate_PeerReadAgainUnderTheLocks: the peer is validated from
// a read outside the transaction; a change committed after that read is
// still honoured, because the admitting transaction reads the row again
// FOR SHARE. The flags-only path keeps its existing behaviour.
func TestMetadataUpdate_PeerReadAgainUnderTheLocks(t *testing.T) {
	f := metaFx(t)
	a := f.completeV1()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name, change, restore, reason string
	}{
		{"asset-metadata-1 dropped", `UPDATE federation_peers SET capabilities = '` + bothProfiles + `'::jsonb WHERE id = $1`,
			`UPDATE federation_peers SET capabilities = '` + allProfiles + `'::jsonb WHERE id = $1`, casfetch.ReasonProfile},
		{"disabled", `UPDATE federation_peers SET enabled = FALSE WHERE id = $1`,
			`UPDATE federation_peers SET enabled = TRUE WHERE id = $1`, casfetch.ReasonPeerUnavailable},
		{"disconnected", `UPDATE federation_peers SET status = 'pending_outbound' WHERE id = $1`,
			`UPDATE federation_peers SET status = 'connected' WHERE id = $1`, casfetch.ReasonPeerUnavailable},
		{"moved", `UPDATE federation_peers SET instance_url = 'https://moved.example' WHERE id = $1`,
			`UPDATE federation_peers SET instance_url = '` + f.origin.srv.URL + `' WHERE id = $1`, casfetch.ReasonPeerUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validated := make(chan struct{})
			proceed := make(chan struct{})
			var once sync.Once
			svc := assetingest.New(assetingest.Deps{Pool: f.pool, Storage: f.svc, Grants: f.grants, Fetcher: f.client,
				Audit: audit.NewRecorder(f.pool, logger), Logger: logger,
				Peers: hookedPeers{inner: peers{pool: f.pool}, after: func() {
					once.Do(func() { close(validated); <-proceed })
				}}})
			e := f.metaUpdate(a, 1, a.files, mdMember(mdQ("t"), `null`, `null`, `null`), time.Now().UTC().Add(time.Second))
			revs := f.revisions(a)
			done := make(chan error, 1)
			go func() {
				_, err := svc.ApplyFlagUpdate(context.Background(), assetingest.Announcement{PeerID: f.peerID, Envelope: e, Signer: f.signer})
				done <- err
			}()
			<-validated
			f.exec(tc.change, f.peerID)
			defer f.exec(tc.restore, f.peerID)
			close(proceed)
			wantReason(t, <-done, tc.reason)
			if f.revisions(a) != revs {
				t.Fatal("kept after the peer changed")
			}
		})
	}
}

func TestMetadataUpdate_ReplayAndConflicts(t *testing.T) {
	f := metaFx(t)
	ctx := context.Background()
	a := f.completeV1()
	p := time.Date(2026, 11, 23, 11, 0, 0, 500, time.UTC)
	member := mdMember(mdQ("Harbour"), `null`, mdQ(mdCC), mdStmt)
	u := f.metaUpdate(a, 1, a.files, member, p)
	first, err := f.applyUpdate(u)
	if err != nil {
		t.Fatal(err)
	}
	firstSig := u.Signature.Value
	dup := func(name string, e *federation.Envelope, signer ...casfetch.Signer) {
		t.Helper()
		sg := f.signer
		if len(signer) > 0 {
			sg = signer[0]
		}
		got, err := f.ingest.ApplyFlagUpdate(ctx, assetingest.Announcement{PeerID: f.peerID, Envelope: e, Signer: sg})
		if err != nil || !got.Duplicate || got.RevisionID != first.RevisionID {
			t.Fatalf("%s: %+v %v, want the first revision as a duplicate", name, got, err)
		}
	}
	dup("the same Update", u)
	reordered := *u
	reordered.Extra = map[string]json.RawMessage{federation.MemberArchivalAsset: u.Extra[federation.MemberArchivalAsset],
		federation.MemberAssetMetadata: json.RawMessage(` {"requiredStatement": {"value":{"none":["Studio A"]},"label":{"en":["Attribution"]}},` +
			"\n" + ` "rights":` + mdQ(mdCC) + `, "description" : null, "title":"Harbour"} `)}
	dup("member order and whitespace", &reordered)
	resigned := reordered
	mdSign(&resigned)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other := casfetch.InstanceSigner(&outbox.IdentitySigner{PrivateKey: otherPriv, KeyURL: localBase + "/users/bob#main-key"})
	dup("re-signed, for another recipient", &resigned, other)
	offset := *u
	offset.Published = p.In(time.FixedZone("x", 5*3600+30*60))
	dup("the same instant at another offset", &offset)
	// A restarted service decides from the database alone.
	restarted := assetingest.New(assetingest.Deps{Pool: f.pool, Storage: f.svc, Peers: peers{pool: f.pool}, Grants: f.grants, Fetcher: f.client})
	if got, err := restarted.ApplyFlagUpdate(ctx, assetingest.Announcement{PeerID: f.peerID, Envelope: u, Signer: f.signer}); err != nil ||
		!got.Duplicate || got.RevisionID != first.RevisionID {
		t.Fatalf("after a restart: %+v %v", got, err)
	}
	var sig, recipient string
	if err := f.pool.QueryRow(ctx, `SELECT origin_signature_value, recipient_signer_key_id FROM federation_received_asset_metadata
		WHERE revision_id = $1`, first.RevisionID).Scan(&sig, &recipient); err != nil || sig != firstSig || recipient != f.signer.KeyID {
		t.Fatalf("the first provenance was replaced: %q %q (%v)", sig, recipient, err)
	}
	if f.snapshots(a) != 2 || f.revisions(a) != 2 {
		t.Fatalf("duplicates kept rows: %d snapshots, %d revisions", f.snapshots(a), f.revisions(a))
	}

	b := f.newAsset(1)
	f.grant(f.peerID, b.uri, nil)
	conflicts := []struct {
		name string
		env  func() *federation.Envelope
	}{
		{"another snapshot", func() *federation.Envelope {
			return f.metaUpdate(a, 1, a.files, mdMember(mdQ("Other"), `null`, mdQ(mdCC), mdStmt), p)
		}},
		{"a null where a claim was", func() *federation.Envelope {
			return f.metaUpdate(a, 1, a.files, mdMember(`null`, `null`, mdQ(mdCC), mdStmt), p)
		}},
		{"a claim where a null was", func() *federation.Envelope {
			return f.metaUpdate(a, 1, a.files, mdMember(mdQ("Harbour"), mdQ("added"), mdQ(mdCC), mdStmt), p)
		}},
		{"other flags", func() *federation.Envelope {
			e := f.metaUpdate(a, 1, a.files, member, p)
			var m map[string]any
			_ = json.Unmarshal(e.Extra[federation.MemberArchivalAsset], &m)
			m["mature"] = true
			e.Extra[federation.MemberArchivalAsset], _ = json.Marshal(m)
			return e
		}},
		{"other files", func() *federation.Envelope {
			files, _ := f.originals(1)
			return f.metaUpdate(a, 1, files, member, p)
		}},
		{"another object", func() *federation.Envelope { return f.metaUpdate(b, 1, b.files, member, p) }},
		{"another actor", func() *federation.Envelope {
			e := f.metaUpdate(a, 1, a.files, member, p)
			e.Actor = f.origin.srv.URL + "/users/bob"
			mdSign(e)
			return e
		}},
		{"one nanosecond later, in the same microsecond", func() *federation.Envelope {
			return f.metaUpdate(a, 1, a.files, member, p.Add(time.Nanosecond))
		}},
		{"without its metadata", func() *federation.Envelope { return f.flagUpdate(a, 1, a.files, false, "", p) }},
	}
	for _, tc := range conflicts {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.env()
			e.ID = u.ID
			revsA, revsB := f.revisions(a), f.revisions(b)
			_, err := f.applyUpdate(e)
			wantReason(t, err, assetingest.ReasonActivityConflict)
			if f.revisions(a) != revsA || f.revisions(b) != revsB || f.snapshots(a) != 2 {
				t.Fatal("a conflicting Update was kept")
			}
		})
	}
	t.Run("metadata added under a flags-only id", func(t *testing.T) {
		flags := f.flagUpdate(a, 1, a.files, false, "", p.Add(time.Minute))
		if _, err := f.applyUpdate(flags); err != nil {
			t.Fatal(err)
		}
		e := f.metaUpdate(a, 1, a.files, member, p.Add(time.Minute))
		e.ID = flags.ID
		_, err := f.applyUpdate(e)
		wantReason(t, err, assetingest.ReasonActivityConflict)
		if again, err := f.applyUpdate(flags); err != nil || !again.Duplicate {
			t.Fatalf("the flags-only Update again: %+v %v", again, err)
		}
	})
	// The reverse: an announcement under an id kept as an Update.
	for name, id := range map[string]string{"a metadata Update's id": u.ID, "a flags-only Update's id": ""} {
		t.Run("an announcement under "+name, func(t *testing.T) {
			if id == "" {
				flags := f.flagUpdate(b, 1, b.files, false, "", p)
				if _, err := f.applyUpdate(flags); err != nil {
					t.Fatal(err)
				}
				id = flags.ID
			}
			target := f.newAsset(1)
			f.grant(f.peerID, target.uri, nil)
			e := f.signedV(target, 1, target.files)
			e.ID = id
			_, err := f.receive(e)
			wantReason(t, err, assetingest.ReasonActivityConflict)
			if n := f.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2`, f.peerID, id); n != 0 {
				t.Fatalf("%d records under an Update's id", n)
			}
			if f.revisions(target) != 0 {
				t.Fatal("a revision was kept under an Update's id")
			}
		})
	}
}

func TestMetadataUpdate_ConcurrentDeliveriesKeepOne(t *testing.T) {
	f := metaFx(t)
	a := f.completeV1()
	u := f.metaUpdate(a, 1, a.files, mdMember(mdQ("once"), `null`, `null`, `null`), time.Now().UTC().Add(time.Second))
	const n = 6
	var wg sync.WaitGroup
	results := make([]assetingest.FlagUpdate, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.applyUpdate(u)
		}(i)
	}
	close(start)
	wg.Wait()
	fresh := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil || results[i].RevisionID != results[0].RevisionID {
			t.Fatalf("delivery %d: %+v %v", i, results[i], errs[i])
		}
		if !results[i].Duplicate {
			fresh++
		}
	}
	if fresh != 1 || f.snapshots(a) != 2 || f.revisions(a) != 2 {
		t.Fatalf("%d admissions, %d snapshots, %d revisions; want 1, 2, 2", fresh, f.snapshots(a), f.revisions(a))
	}
}

func TestMetadataUpdate_OrderIsFullPrecision(t *testing.T) {
	f := metaFx(t)
	t.Run("nanoseconds within one microsecond", func(t *testing.T) {
		a := f.completeV1()
		base := time.Date(2026, 11, 23, 12, 0, 0, 1000, time.UTC)
		later := f.metaUpdate(a, 1, a.files, mdMember(mdQ("two"), `null`, `null`, `null`), base.Add(2*time.Nanosecond))
		earlier := f.metaUpdate(a, 1, a.files, mdMember(mdQ("one"), `null`, `null`, `null`), base.Add(time.Nanosecond))
		// The URIs order the other way, so only the nanoseconds can pick
		// the later one.
		later.ID = f.origin.srv.URL + "/activities/" + a.id.String() + "-a"
		earlier.ID = f.origin.srv.URL + "/activities/" + a.id.String() + "-z"
		for _, e := range []*federation.Envelope{later, earlier} {
			if _, err := f.applyUpdate(e); err != nil {
				t.Fatal(err)
			}
		}
		// The revisions' timestamptz cannot tell them apart; the snapshots can.
		if n := f.count(`SELECT count(DISTINCT published) FROM federation_received_asset_revisions
			WHERE peer_id = $1 AND origin_asset_uri = $2 AND activity_type = 'Update'`, f.peerID, a.uri); n != 1 {
			t.Fatalf("%d distinct revision timestamps, want 1 (microsecond precision)", n)
		}
		if got := f.currentTitle(a); got != "two" {
			t.Fatalf("current %q, want the later by one nanosecond", got)
		}
	})
	t.Run("a year past 2262", func(t *testing.T) {
		a := f.completeV1()
		far := time.Date(2300, 1, 2, 3, 4, 5, 6, time.UTC)
		if _, err := f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ("now"), `null`, `null`, `null`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))); err != nil {
			t.Fatal(err)
		}
		if _, err := f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ("future"), `null`, `null`, `null`), far)); err != nil {
			t.Fatal(err)
		}
		m := f.currentMeta(a)
		if m == nil || *m.Snapshot.Title != "future" || !m.Published.Equal(far) || m.PublishedSeconds != far.Unix() || m.PublishedNanos != 6 {
			t.Fatalf("the far-future snapshot: %+v", m)
		}
	})
	t.Run("equal times: the activity URI, byte by byte", func(t *testing.T) {
		a := f.completeV1()
		at := time.Date(2026, 11, 23, 13, 0, 0, 7, time.UTC)
		for _, suffix := range []string{"tie-B", "tie-a", "tie-_", "tie-A"} {
			e := f.metaUpdate(a, 1, a.files, mdMember(mdQ(suffix), `null`, `null`, `null`), at)
			e.ID = f.origin.srv.URL + "/activities/" + a.id.String() + "-" + suffix
			if _, err := f.applyUpdate(e); err != nil {
				t.Fatal(err)
			}
		}
		// Bytes: 'A' 0x41 < 'B' 0x42 < '_' 0x5f < 'a' 0x61, so "tie-a" is the
		// greatest, whatever a linguistic collation would say.
		if got := f.currentTitle(a); got != "tie-a" {
			t.Fatalf("current %q, want tie-a", got)
		}
	})
}

func TestMetadataUpdate_CurrentOnlyWithAVerifiedVersion(t *testing.T) {
	f := metaFx(t)
	p := time.Now().UTC().Add(time.Second)
	t.Run("kept before its version; current once it completes", func(t *testing.T) {
		a := f.newAsset(1)
		f.grant(f.peerID, a.uri, nil)
		if _, err := f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ("early"), `null`, `null`, `null`), p)); err != nil {
			t.Fatal(err)
		}
		if f.snapshots(a) != 1 || f.currentTitle(a) != "<none>" {
			t.Fatalf("before the version: %d snapshots, current %q", f.snapshots(a), f.currentTitle(a))
		}
		var kept []byte
		_ = f.pool.QueryRow(context.Background(), `SELECT to_jsonb(m)::text FROM federation_received_asset_metadata m
			JOIN federation_received_asset_revisions r ON r.id = m.revision_id WHERE r.origin_asset_uri = $1 AND r.activity_type = 'Update'`, a.uri).Scan(&kept)
		// The version announced and pending (not fetched yet): still not current.
		sched, err := f.schedule(f.signedV(a, 1, a.files))
		if err != nil || sched.State != assetingest.StatePending {
			t.Fatalf("version 1 scheduled: %+v %v", sched, err)
		}
		if got := f.currentTitle(a); got != "<none>" {
			t.Fatalf("a pending version made %q current", got)
		}
		if v1, err := f.process(sched.RecordID); err != nil || v1.State != assetingest.StateComplete {
			t.Fatalf("version 1: %+v %v", v1, err)
		}
		if got := f.currentTitle(a); got != "early" {
			t.Fatalf("after the version completed: %q", got)
		}
		var now []byte
		_ = f.pool.QueryRow(context.Background(), `SELECT to_jsonb(m)::text FROM federation_received_asset_metadata m
			JOIN federation_received_asset_revisions r ON r.id = m.revision_id WHERE r.origin_asset_uri = $1 AND r.activity_type = 'Update'`, a.uri).Scan(&now)
		if string(kept) != string(now) {
			t.Fatal("activation rewrote the kept snapshot")
		}
	})
	t.Run("a version that fails its fixity never makes it current", func(t *testing.T) {
		a := f.newAsset(1)
		f.grant(f.peerID, a.uri, nil)
		if _, err := f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ("bad"), `null`, `null`, `null`), p)); err != nil {
			t.Fatal(err)
		}
		f.origin.setMode(a.hash[0], "tamper")
		out, _ := f.receive(f.signedV(a, 1, a.files))
		if out.State != assetingest.StateRejected {
			t.Fatalf("the tampered version: %+v", out)
		}
		if got := f.currentTitle(a); got != "<none>" {
			t.Fatalf("a rejected version made %q current", got)
		}
	})
	t.Run("superseded stays eligible; a pending newer one hides nothing", func(t *testing.T) {
		a := f.completeV1()
		if _, err := f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ("v1"), `null`, `null`, `null`), p)); err != nil {
			t.Fatal(err)
		}
		files2, _ := f.originals(1)
		if v2, err := f.receive(f.signedV(a, 2, files2)); err != nil || v2.State != assetingest.StateComplete {
			t.Fatalf("version 2: %+v %v", v2, err)
		}
		if f.chain(a) != "1:superseded 2:complete" {
			t.Fatalf("chain %s", f.chain(a))
		}
		// Version 2's own initial snapshot (four nulls) is older than the
		// version 1 Update, so it clears nothing; the files served stay
		// version 2's.
		if got := f.currentTitle(a); got != "v1" {
			t.Fatalf("after version 2: %q", got)
		}
		if _, err := f.applyUpdate(f.metaUpdate(a, 2, files2, mdMember(mdQ("v2"), `null`, `null`, `null`), p.Add(time.Second))); err != nil {
			t.Fatal(err)
		}
		if got := f.currentTitle(a); got != "v2" {
			t.Fatalf("after the version 2 snapshot: %q", got)
		}
		files3, _ := f.originals(1)
		if _, err := f.applyUpdate(f.metaUpdate(a, 3, files3, mdMember(mdQ("v3"), `null`, `null`, `null`), p.Add(2*time.Second))); err != nil {
			t.Fatal(err)
		}
		if got := f.currentTitle(a); got != "v2" {
			t.Fatalf("a snapshot of a version not held became current: %q", got)
		}
		if f.chain(a) != "1:superseded 2:complete" {
			t.Fatalf("a metadata Update changed the chain: %s", f.chain(a))
		}
	})
	t.Run("a withdrawal hides the current snapshot and keeps the history", func(t *testing.T) {
		a := f.completeV1()
		if _, err := f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ("kept"), `null`, `null`, `null`), p)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.withdraw(f.deleteOf(a, f.tombstone(a, ""))); err != nil {
			t.Fatal(err)
		}
		if got := f.currentTitle(a); got != "<none>" || f.snapshots(a) != 2 {
			t.Fatalf("after the withdrawal: %q, %d snapshots", got, f.snapshots(a))
		}
	})
}

// TestMetadataUpdate_SnapshotAndRevisionAreOneAdmission: a snapshot insert
// that fails (a test-only trigger) rolls the flags revision back too.
func TestMetadataUpdate_SnapshotAndRevisionAreOneAdmission(t *testing.T) {
	f := metaFx(t)
	a := f.completeV1()
	marker := "FAIL-" + uuid.NewString()[:8]
	fn := "md_fail_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	f.exec(`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.snapshot->>'title' = '` + marker + `' THEN RAISE EXCEPTION 'test: refuse this snapshot'; END IF; RETURN NEW; END; $$`)
	f.exec(`CREATE TRIGGER ` + fn + ` BEFORE INSERT ON federation_received_asset_metadata FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fn+` ON federation_received_asset_metadata`)
		_, _ = f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)
	})
	revs, snapsBefore := f.revisions(a), f.snapshots(a)
	_, err := f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ(marker), `null`, `null`, `null`), time.Now().UTC().Add(time.Second)))
	if err == nil || !assetingest.Retryable(err) {
		t.Fatalf("a failed snapshot insert: %v", err)
	}
	if f.revisions(a) != revs || f.snapshots(a) != snapsBefore {
		t.Fatalf("a revision without its snapshot was kept: %d -> %d revisions", revs, f.revisions(a))
	}
	// The same trigger lets another snapshot through: the refusal was this one.
	if _, err := f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ("fine"), `null`, `null`, `null`), time.Now().UTC().Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	if f.snapshots(a) != snapsBefore+1 || f.revisions(a) != revs+1 {
		t.Fatal("the control Update was not kept")
	}
}

// TestMetadataUpdate_SnapshotOnlyOnTheRevisionItInserted: a snapshot is
// attached only to the revision this delivery's own INSERT wrote. A
// test-only trigger turns one activity's INSERT into a different row
// written by a nested INSERT (same transaction, same received_at) and
// suppresses the outer one, which then affects no row. This is a
// controlled invariant test, not an observed production race.
func TestMetadataUpdate_SnapshotOnlyOnTheRevisionItInserted(t *testing.T) {
	f := metaFx(t)
	a := f.completeV1()
	p := time.Now().UTC().Add(time.Second)
	target := f.metaUpdate(a, 1, a.files, mdMember(mdQ("diverted"), `null`, `null`, `null`), p)
	fn := "md_divert_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	f.exec(`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF pg_trigger_depth() = 1 AND NEW.activity_uri = '` + target.ID + `' THEN
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
	revs, snaps := f.revisions(a), f.snapshots(a)
	got, err := f.applyUpdate(target)
	wantReason(t, err, assetingest.ReasonActivityConflict)
	if got.RevisionID != uuid.Nil {
		t.Fatalf("a revision id was returned for a refused delivery: %+v", got)
	}
	if f.revisions(a) != revs || f.snapshots(a) != snaps {
		t.Fatalf("kept %d revisions and %d snapshots, want %d and %d: the nested row and any snapshot must roll back",
			f.revisions(a), f.snapshots(a), revs, snaps)
	}
	if n := f.count(`SELECT count(*) FROM federation_received_asset_revisions WHERE activity_uri = $1`, target.ID); n != 0 {
		t.Fatalf("%d rows under the diverted activity", n)
	}
	if after := f.localState(); after != before {
		t.Fatalf("local state changed:\n%s\n%s", before, after)
	}
	// Another activity is untouched by the trigger and still admitted.
	control := f.metaUpdate(a, 1, a.files, mdMember(mdQ("control"), `null`, `null`, `null`), p.Add(time.Second))
	if c, err := f.applyUpdate(control); err != nil || c.Duplicate || c.RevisionID == uuid.Nil {
		t.Fatalf("the control activity: %+v %v", c, err)
	}
	if f.revisions(a) != revs+1 || f.snapshots(a) != snaps+1 || f.currentTitle(a) != "control" {
		t.Fatalf("the control activity was not kept (%d revisions, %d snapshots, current %q)", f.revisions(a), f.snapshots(a), f.currentTitle(a))
	}
}

// TestMetadataUpdate_GrantRequiredForFirstAdmission: with every profile
// negotiated, a first metadata admission needs a live original grant.
func TestMetadataUpdate_GrantRequiredForFirstAdmission(t *testing.T) {
	f := metaFx(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		grant func(a *asset)
	}{
		{"a view-only grant without the original", func(a *asset) {
			if _, err := f.grants.ApplyShare(ctx, inboundgrants.Share{
				PeerID: f.peerID, ActivityURI: f.origin.srv.URL + "/activities/share-" + uuid.NewString(),
				GrantorActorURI: f.origin.srv.URL + "/users/alice", ObjectKind: federation.ShareObjectKindAsset,
				ObjectURI: a.uri, Scope: federation.ShareScopeView, AllowOriginal: false,
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a revoked grant", func(a *asset) { f.revoke(f.peerID, a.uri, f.grant(f.peerID, a.uri, nil)) }},
		{"a grant expired on arrival", func(a *asset) {
			var past time.Time
			if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp() - interval '1 second'`).Scan(&past); err != nil {
				t.Fatal(err)
			}
			f.grant(f.peerID, a.uri, &past)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := f.newAsset(1)
			tc.grant(a)
			before := f.localState()
			_, err := f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ("t"), `null`, `null`, `null`), time.Now().UTC().Add(time.Second)))
			wantReason(t, err, casfetch.ReasonNoInboundGrant)
			if f.revisions(a) != 0 || f.snapshots(a) != 0 {
				t.Fatalf("kept %d revisions and %d snapshots without a live original grant", f.revisions(a), f.snapshots(a))
			}
			if f.origin.getCount(a.hash[0]) != 0 {
				t.Fatal("a metadata Update fetched a file")
			}
			if after := f.localState(); after != before {
				t.Fatalf("local state changed:\n%s\n%s", before, after)
			}
		})
	}
}

// TestMetadataUpdate_AfterRevocation: an activity accepted before a
// revocation is still the same duplicate, which adds and changes nothing;
// a different activity under the revoked grant is refused. (Whether a kept
// snapshot may still be shown is not this storage's decision.)
func TestMetadataUpdate_AfterRevocation(t *testing.T) {
	f := metaFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	share := f.grant(f.peerID, a.uri, nil)
	if v1, err := f.receive(f.signedV(a, 1, a.files)); err != nil || v1.State != assetingest.StateComplete {
		t.Fatalf("version 1: %+v %v", v1, err)
	}
	u := f.metaUpdate(a, 1, a.files, mdMember(mdQ("before"), `null`, `null`, `null`), time.Now().UTC().Add(time.Second))
	first, err := f.applyUpdate(u)
	if err != nil {
		t.Fatal(err)
	}
	firstSig := u.Signature.Value
	f.revoke(f.peerID, a.uri, share)
	before := f.localState()
	fetches := f.origin.getCount(a.hash[0])
	revs, snaps := f.revisions(a), f.snapshots(a)

	again := *u
	mdSign(&again)
	for name, e := range map[string]*federation.Envelope{"the same delivery": u, "the same activity re-signed": &again} {
		got, err := f.applyUpdate(e)
		if err != nil || !got.Duplicate || got.RevisionID != first.RevisionID {
			t.Fatalf("%s after the revocation: %+v %v", name, got, err)
		}
	}
	var sig string
	if err := f.pool.QueryRow(ctx, `SELECT origin_signature_value FROM federation_received_asset_metadata WHERE revision_id = $1`,
		first.RevisionID).Scan(&sig); err != nil || sig != firstSig {
		t.Fatalf("the first signature changed: %q (%v)", sig, err)
	}
	_, err = f.applyUpdate(f.metaUpdate(a, 1, a.files, mdMember(mdQ("after"), `null`, `null`, `null`), time.Now().UTC().Add(2*time.Second)))
	wantReason(t, err, casfetch.ReasonNoInboundGrant)
	if f.revisions(a) != revs || f.snapshots(a) != snaps || f.origin.getCount(a.hash[0]) != fetches {
		t.Fatal("a delivery after the revocation added a row or fetched")
	}
	if after := f.localState(); after != before {
		t.Fatalf("local state changed:\n%s\n%s", before, after)
	}
}
