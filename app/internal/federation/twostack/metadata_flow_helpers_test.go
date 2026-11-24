// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Helpers of the #1577 metadata flow gate (metadata_flow_gate_test.go).
// Everything here observes, schedules or arranges local fixtures; the
// positive flow itself is the instances' own handlers and workers.

package twostack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	apphttp "github.com/mscrnt/artist-alley/app/internal/http"
)

// flowGate is one direction of the gate: a is the SOURCE and b the
// DESTINATION, whichever of the cluster's two instances they are.
type flowGate struct {
	*casGate
	dir            string
	src, dst       *instance
	srcRun, dstRun *workers
	member         string // a destination member's token
	owner          string // the source owner's username (its admin)
}

// flowDirections runs body once in each direction, each on a fresh
// cluster: A_to_B (A is the source) and B_to_A (B is).
func flowDirections(t *testing.T, body func(t *testing.T, g *flowGate)) {
	for _, dir := range []string{"A_to_B", "B_to_A"} {
		t.Run(dir, func(t *testing.T) { body(t, newFlowGate(t, dir)) })
	}
}

// newFlowGate pairs a new cluster through the real handshake with
// asset-metadata-1, archival-1 and original-transfer-1 advertised
// (TEST-ONLY ADVERTISEMENT, before the handshake; restored after the
// workers stop, as cleanups run in reverse order) and orients it.
func newFlowGate(t *testing.T, dir string) *flowGate {
	t.Helper()
	prev := append(peer.CapabilitySet(nil), peer.KnownCapabilities...)
	peer.KnownCapabilities = append(peer.KnownCapabilities, peer.PeerCapability(federation.ProfileAssetMetadata1))
	t.Cleanup(func() { peer.KnownCapabilities = prev })
	base := archivalGate(t)
	c := base.cluster
	for _, side := range []struct{ in, other *instance }{{c.a, c.b}, {c.b, c.a}} {
		caps := c.capabilities(side.in, side.other)
		for _, p := range []string{federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1} {
			if !slices.Contains(caps, p) {
				t.Fatalf("%s did not negotiate %s with %s: %v", side.in.name, p, side.other.name, caps)
			}
		}
	}
	g := &flowGate{casGate: base, dir: dir}
	if dir == "B_to_A" {
		// An oriented view: the same two instances, B as the source.
		// Cleanup stays with the cluster that booted them.
		view := &cluster{t: c.t, a: c.b, b: c.a, logger: c.logger, tr: c.tr}
		g.casGate = &casGate{cluster: view, inbound: base.inbound,
			peerAonB: view.peerID(view.b, view.a), peerBonA: view.peerID(view.a, view.b)}
	}
	g.src, g.dst = g.a, g.b
	if g.src.pool == g.dst.pool || g.src.url == g.dst.url || g.src.root == g.dst.root {
		t.Fatal("source and destination are not separate instances")
	}
	var srcDB, dstDB string
	if err := g.src.pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&srcDB); err != nil {
		t.Fatal(err)
	}
	if err := g.dst.pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&dstDB); err != nil {
		t.Fatal(err)
	}
	if srcDB == dstDB || srcDB == "" {
		t.Fatalf("source and destination share database %q", srcDB)
	}
	if g.peerAonB != c.peerID(g.dst, g.src) || g.peerBonA != c.peerID(g.src, g.dst) {
		t.Fatal("the oriented peer ids are not the instances' own rows")
	}
	if err := g.src.pool.QueryRow(context.Background(), `SELECT username FROM "user" WHERE ref = $1`, g.src.admin).Scan(&g.owner); err != nil {
		t.Fatal(err)
	}
	g.member = g.memberOnB()
	t.Logf("%s: source %s (%s, peer row %s), destination %s (%s, peer row %s)", dir, g.src.name, srcDB, g.peerBonA,
		g.dst.name, dstDB, g.peerAonB)
	return g
}

// workers is one Server.Run of an instance, stopped by stop (cancel and
// a bounded wait). stop is idempotent and registered as a cleanup, so it
// runs before the cluster removes its peers.
type workers struct {
	t      *testing.T
	in     *instance
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (g *flowGate) start(in *instance) *workers {
	g.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &workers{t: g.t, in: in, cancel: cancel, done: make(chan struct{})}
	srv := in.srv
	go func() { _ = srv.Run(ctx); close(w.done) }()
	g.t.Cleanup(w.stop)
	return w
}

func (w *workers) stop() {
	w.once.Do(func() {
		w.cancel()
		select {
		case <-w.done:
		case <-time.After(30 * time.Second):
			w.t.Errorf("%s: workers did not stop", w.in.name)
		}
	})
}

// rebuild replaces in's stopped server with a fresh, NOT running one on
// the same database, storage root, URL (the same TLS endpoint) and so
// the same identity. Server.Run is not restartable once shut down.
func (g *flowGate) rebuild(in *instance) {
	g.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for in.pool.Stat().AcquiredConns() != 0 {
		if time.Now().After(deadline) {
			g.t.Fatalf("%s: %d connections still held after the workers stopped", in.name, in.pool.Stat().AcquiredConns())
		}
		time.Sleep(20 * time.Millisecond)
	}
	in.srv.Close()
	s, err := apphttp.New(config.Config{StorageBackend: "fs", StorageFSRoot: in.root, HTTPAddr: "127.0.0.1:0"}, g.logger, in.pool, "twostack")
	if err != nil {
		g.t.Fatalf("%s: New: %v", in.name, err)
	}
	g.t.Cleanup(s.Close)
	in.srv = s
	h := s.Handler()
	in.handler.Store(&h)
}

// srcAPI calls the source's real API as its admin, requiring want.
func (g *flowGate) srcAPI(method, path string, body any, want int) []byte {
	g.t.Helper()
	var raw json.RawMessage
	if st := g.api(g.src, method, path, body, &raw); st != want {
		g.t.Fatalf("%s %s on %s: %d, want %d: %s", method, path, g.src.name, st, want, raw)
	}
	return raw
}

func (g *flowGate) patch(o original, body map[string]any) {
	g.t.Helper()
	g.srcAPI(http.MethodPatch, "/assets/"+o.asset.String(), body, http.StatusOK)
}

func (g *flowGate) putField(o original, field uuid.UUID, v string) {
	g.t.Helper()
	g.srcAPI(http.MethodPut, "/assets/"+o.asset.String()+"/fields/"+field.String(), map[string]any{"value_text": v}, http.StatusOK)
}

func (g *flowGate) clearField(o original, field uuid.UUID) {
	g.t.Helper()
	g.srcAPI(http.MethodDelete, "/assets/"+o.asset.String()+"/fields/"+field.String(), nil, http.StatusNoContent)
}

// mirrorOnSrc is the source's shipped definition mirroring column.
func (g *flowGate) mirrorOnSrc(column string) uuid.UUID {
	g.t.Helper()
	var id uuid.UUID
	if err := g.src.pool.QueryRow(context.Background(), `SELECT id FROM field_definition WHERE mirrors_column = $1 AND code = $1`,
		column).Scan(&id); err != nil {
		g.t.Fatal(err)
	}
	return id
}

func (g *flowGate) assetURI(o original) string { return g.src.url + "/assets/" + o.asset.String() }

// snap is a four-key snapshot as the destination API returns it. Its
// JSON form is compared, so a missing key and a null differ.
type snap struct {
	Title             *string `json:"title"`
	Description       *string `json:"description"`
	Rights            *string `json:"rights"`
	RequiredStatement *struct {
		Label map[string][]string `json:"label"`
		Value map[string][]string `json:"value"`
	} `json:"requiredStatement"`
}

func sp(s string) *string { return &s }

// want builds the expected snapshot ("" title/description/rights/statement
// pointer nil means null).
func want(title, description, rights, statement *string) snap {
	s := snap{Title: title, Description: description, Rights: rights}
	if statement != nil {
		s.RequiredStatement = &struct {
			Label map[string][]string `json:"label"`
			Value map[string][]string `json:"value"`
		}{Label: map[string][]string{"en": {"Attribution"}}, Value: map[string][]string{"none": {*statement}}}
	}
	return s
}

func (s snap) String() string { b, _ := json.Marshal(s); return string(b) }

// current is the destination API's current_metadata of a record.
type current struct {
	Profile              string          `json:"profile"`
	Snapshot             json.RawMessage `json:"snapshot"`
	RevisionID           string          `json:"revision_id"`
	PeerID               string          `json:"peer_id"`
	OriginAssetURI       string          `json:"origin_asset_uri"`
	ActivityURI          string          `json:"activity_uri"`
	ActorURI             string          `json:"actor_uri"`
	VersionURI           string          `json:"version_uri"`
	FileSetSHA256        string          `json:"file_set_sha256"`
	Published            time.Time       `json:"published"`
	PublishedSeconds     int64           `json:"published_seconds"`
	PublishedNanos       int32           `json:"published_nanos"`
	OriginSignerKeyID    string          `json:"origin_signer_key_id"`
	OriginSignatureValue string          `json:"origin_signature_value"`
	RecipientSignerKeyID string          `json:"recipient_signer_key_id"`
	OriginSignatureAlgo  string          `json:"origin_signature_algorithm"`
}

type detail struct {
	Declared        json.RawMessage `json:"declared"`
	ActivityURI     string          `json:"activity_uri"`
	CurrentMetadata *current        `json:"current_metadata"`
	Asset           struct {
		VersionNumber int64 `json:"version_number"`
		Original      struct {
			SHA256 string `json:"sha256"`
		} `json:"original"`
	} `json:"asset"`
}

// detailOnDst reads a record's detail as the destination member.
func (g *flowGate) detailOnDst(recordID string) (int, detail) {
	g.t.Helper()
	st, raw := g.getOnB(g.member, "/federation/received-assets/"+recordID)
	var d detail
	if st == http.StatusOK {
		if err := json.Unmarshal(raw, &d); err != nil {
			g.t.Fatalf("detail: %v %s", err, raw)
		}
	}
	return st, d
}

// snapOf decodes a snapshot, requiring all four keys to be present.
func snapOf(t *testing.T, raw json.RawMessage) snap {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil || len(keys) != 4 {
		t.Fatalf("snapshot %s is not four keys (%v)", raw, err)
	}
	for _, k := range []string{"title", "description", "rights", "requiredStatement"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("snapshot %s lacks %s", raw, k)
		}
	}
	var s snap
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// waitCurrent waits until the record's current snapshot is w (and ok, if
// given, holds), returning it.
func (g *flowGate) waitCurrent(recordID string, w snap, ok func(current) bool) current {
	g.t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	last := "none"
	for time.Now().Before(deadline) {
		st, d := g.detailOnDst(recordID)
		if st == http.StatusOK && d.CurrentMetadata != nil {
			s := snapOf(g.t, d.CurrentMetadata.Snapshot)
			last = s.String() + " from " + d.CurrentMetadata.ActivityURI
			if s.String() == w.String() && (ok == nil || ok(*d.CurrentMetadata)) {
				return *d.CurrentMetadata
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	g.t.Fatalf("%s: the current snapshot never became %s (last %s)", g.dir, w, last)
	return current{}
}

// sourceUpdate is the source's recorded metadata Update for an activity
// URI: its id, capture (as recorded) and published time.
type sourceUpdate struct {
	id        uuid.UUID
	capture   string
	published time.Time
	actor     string
}

func (g *flowGate) sourceUpdate(activityURI string) sourceUpdate {
	g.t.Helper()
	var u sourceUpdate
	if err := g.src.pool.QueryRow(context.Background(), `SELECT id, payload->>'metadata_capture', published_at, actor_uri FROM activities
		WHERE activity_uri = $1 AND activity_type = 'Update' AND payload ? 'metadata_capture'`, activityURI).
		Scan(&u.id, &u.capture, &u.published, &u.actor); err != nil {
		g.t.Fatalf("the source has no metadata Update %s: %v", activityURI, err)
	}
	return u
}

// binding is what checkBinding read: the kept revision, the stored
// signed envelope and the source's ledger row of the activity.
type binding struct {
	revisionID uuid.UUID
	activityID uuid.UUID
	envJSON    []byte
	env        *federation.Envelope
	pub        []byte
}

// checkBinding proves, read only, where a current snapshot came from,
// whether its activity is an initial aa:AssetVersion or an Update:
//
//   - the kept metadata revision is the one the API names, of this peer
//     and activity, bound to EXACTLY the asset's version N and to the
//     file set of the destination's verified (complete or superseded)
//     record of that version;
//   - the destination's stored envelope is that activity (id, type,
//     actor, object), its aa:asset names that version, and it verifies
//     with the source owner's public key;
//   - the source's ledger row is a local activity of that type, by the
//     owner, about this asset, published at the same instant, with an
//     outbox row to this destination;
//   - the kept provenance is the envelope's, and the recipient signer is
//     exactly the destination's instance key.
func (g *flowGate) checkBinding(o original, cur current, versionNumber int64) binding {
	g.t.Helper()
	ctx := context.Background()
	var b binding
	wantActor := g.src.url + "/users/" + g.owner
	versionURI := g.assetURI(o) + "/versions/" + itoa(versionNumber)
	if cur.ActorURI != wantActor || cur.PeerID != g.peerAonB.String() || cur.OriginAssetURI != g.assetURI(o) ||
		cur.Profile != federation.ProfileAssetMetadata1 || cur.VersionURI != versionURI {
		g.t.Fatalf("%s: provenance %+v, want actor %s, peer %s, version %s", g.dir, cur, wantActor, g.peerAonB, versionURI)
	}
	// The kept revision and its metadata row.
	var (
		revType, revVersion, revFileSet, revActor     string
		revPublished                                  time.Time
		secs                                          int64
		nanos                                         int32
		originKey, originAlgo, originValue, recipient string
		kept                                          []byte
	)
	if err := g.dst.pool.QueryRow(ctx, `SELECT r.id, r.activity_type, r.version_uri, r.file_set_sha256, r.actor_uri, r.published,
		    m.snapshot, m.published_seconds, m.published_nanos, m.origin_signer_key_id, m.origin_signature_algorithm,
		    m.origin_signature_value, m.recipient_signer_key_id
		  FROM federation_received_asset_revisions r JOIN federation_received_asset_metadata m ON m.revision_id = r.id
		 WHERE r.peer_id = $1 AND r.activity_uri = $2`, g.peerAonB, cur.ActivityURI).Scan(&b.revisionID, &revType, &revVersion, &revFileSet,
		&revActor, &revPublished, &kept, &secs, &nanos, &originKey, &originAlgo, &originValue, &recipient); err != nil {
		g.t.Fatalf("%s: no kept metadata revision for %s: %v", g.dir, cur.ActivityURI, err)
	}
	if b.revisionID.String() != cur.RevisionID || revVersion != versionURI || revFileSet != cur.FileSetSHA256 || revActor != wantActor ||
		!sameJSON(kept, cur.Snapshot) || secs != cur.PublishedSeconds || nanos != cur.PublishedNanos || originKey != cur.OriginSignerKeyID ||
		originAlgo != cur.OriginSignatureAlgo || originValue != cur.OriginSignatureValue || recipient != cur.RecipientSignerKeyID {
		g.t.Fatalf("%s: the kept revision %s (%s, %s, %s) does not match the API %+v", g.dir, b.revisionID, revType, revVersion, revFileSet, cur)
	}
	if want := g.dst.url + "/federation/instance#main-key"; cur.RecipientSignerKeyID != want {
		g.t.Fatalf("%s: recipient signer %s, want the destination's instance key %s", g.dir, cur.RecipientSignerKeyID, want)
	}
	// The verified record of exactly that version.
	var recordFileSet string
	if err := g.dst.pool.QueryRow(ctx, `SELECT file_set_sha256 FROM federation_received_assets
		WHERE peer_id = $1 AND origin_asset_uri = $2 AND version_uri = $3 AND state IN ('complete', 'superseded')`,
		g.peerAonB, g.assetURI(o), versionURI).Scan(&recordFileSet); err != nil {
		g.t.Fatalf("%s: no verified record of %s: %v", g.dir, versionURI, err)
	}
	if recordFileSet != cur.FileSetSHA256 {
		g.t.Fatalf("%s: bound to file set %s, the record of %s has %s", g.dir, cur.FileSetSHA256, versionURI, recordFileSet)
	}
	// The stored signed envelope.
	if err := g.dst.pool.QueryRow(ctx, `SELECT envelope_json FROM federation_inbox WHERE peer_id = $1 AND activity_uri = $2`,
		g.peerAonB, cur.ActivityURI).Scan(&b.envJSON); err != nil {
		g.t.Fatalf("%s: the destination holds no envelope for %s: %v", g.dir, cur.ActivityURI, err)
	}
	var pub string
	if err := g.src.pool.QueryRow(ctx, `SELECT signing_public_key_pem FROM "user" WHERE ref = $1`, g.src.admin).Scan(&pub); err != nil || pub == "" {
		g.t.Fatalf("the source owner's public key: %v", err)
	}
	b.pub = []byte(pub)
	env, err := federation.Unmarshal(b.envJSON)
	if err != nil {
		g.t.Fatal(err)
	}
	b.env = env
	if err := env.Verify(b.pub); err != nil {
		g.t.Fatalf("%s: the stored envelope does not verify with the source owner's key: %v", g.dir, err)
	}
	if env.ID != cur.ActivityURI || string(env.Type) != revType || env.Actor != wantActor || env.Object != g.assetURI(o) ||
		env.Signature.PublicKey != cur.OriginSignerKeyID || env.Signature.Value != cur.OriginSignatureValue ||
		string(env.Signature.Type) != cur.OriginSignatureAlgo {
		g.t.Fatalf("%s: envelope %s (%s) by %s signed %s, kept %+v", g.dir, env.ID, env.Type, env.Actor, env.Signature.PublicKey, cur)
	}
	if !env.Published.Equal(cur.Published) || env.Published.Unix() != cur.PublishedSeconds || int32(env.Published.Nanosecond()) != cur.PublishedNanos ||
		!env.Published.Equal(revPublished) {
		g.t.Fatalf("%s: published %s, API %s, kept %d.%09d", g.dir, env.Published.Format(time.RFC3339Nano), cur.Published.Format(time.RFC3339Nano),
			cur.PublishedSeconds, cur.PublishedNanos)
	}
	var member struct {
		Version struct {
			ID string `json:"id"`
		} `json:"version"`
	}
	if err := json.Unmarshal(env.Extra[federation.MemberArchivalAsset], &member); err != nil || member.Version.ID != versionURI {
		g.t.Fatalf("%s: the signed aa:asset names %q, want %s (%v)", g.dir, member.Version.ID, versionURI, err)
	}
	// A copy with another aa:asset, not re-signed, does not verify.
	tampered, _ := federation.Unmarshal(b.envJSON)
	tampered.Extra[federation.MemberArchivalAsset] = json.RawMessage(`{"tampered":true}`)
	if tampered.Verify(b.pub) == nil {
		g.t.Fatalf("%s: an altered aa:asset still verifies", g.dir)
	}
	// The source's ledger row and its delivery to this destination.
	var srcType, srcSource, srcKind, srcObject string
	var srcActorRef *int64
	var srcPublished time.Time
	if err := g.src.pool.QueryRow(ctx, `SELECT id, activity_type, source, actor_user_ref, object_kind, object_local_id, published_at
		FROM activities WHERE activity_uri = $1`, cur.ActivityURI).Scan(&b.activityID, &srcType, &srcSource, &srcActorRef, &srcKind, &srcObject,
		&srcPublished); err != nil {
		g.t.Fatalf("%s: the source has no activity %s: %v", g.dir, cur.ActivityURI, err)
	}
	if srcType != revType || srcSource != "local" || srcActorRef == nil || *srcActorRef != g.src.admin || srcKind != "asset" ||
		srcObject != o.asset.String() || !srcPublished.Equal(env.Published) {
		g.t.Fatalf("%s: the source ledger row %s %s by %v about %s/%s at %s", g.dir, srcType, srcSource, srcActorRef, srcKind, srcObject, srcPublished)
	}
	if n := g.countOnSrc(`SELECT count(*) FROM federation_outbox WHERE activity_id = $1 AND peer_id = $2 AND target_user_url IS NULL`,
		b.activityID, g.peerBonA); n != 1 {
		g.t.Fatalf("%s: %d source outbox rows of %s to this destination", g.dir, n, cur.ActivityURI)
	}
	return b
}

// checkUpdate is checkBinding for a metadata Update, which also proves
// that the source's recorded capture, the signed aa:metadata member and
// the kept snapshot are one value of exactly four keys, and that a copy
// with another aa:metadata does not verify.
func (g *flowGate) checkUpdate(o original, cur current, versionNumber int64) sourceUpdate {
	g.t.Helper()
	b := g.checkBinding(o, cur, versionNumber)
	u := g.sourceUpdate(cur.ActivityURI)
	if u.id != b.activityID || string(b.env.Type) != "Update" || u.actor != g.src.url+"/users/"+g.owner {
		g.t.Fatalf("%s: %s is not the source's metadata Update (%s)", g.dir, cur.ActivityURI, b.env.Type)
	}
	var capture struct {
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(u.capture), &capture); err != nil {
		g.t.Fatal(err)
	}
	wire := b.env.Extra[federation.MemberAssetMetadata]
	snapOf(g.t, wire)
	if !sameJSON(wire, capture.Snapshot) || !sameJSON(cur.Snapshot, capture.Snapshot) {
		g.t.Fatalf("%s: wire %s, recorded %s, kept %s", g.dir, wire, capture.Snapshot, cur.Snapshot)
	}
	tampered, _ := federation.Unmarshal(b.envJSON)
	tampered.Extra[federation.MemberAssetMetadata] = json.RawMessage(`{"title":"tampered","description":null,"rights":null,"requiredStatement":null}`)
	if tampered.Verify(b.pub) == nil {
		g.t.Fatalf("%s: an altered aa:metadata still verifies", g.dir)
	}
	return u
}

// checkInitial is checkBinding for the initial snapshot an announcement
// carried: the activity is the source's aa:AssetVersion. No aa:metadata
// member is expected on it.
func (g *flowGate) checkInitial(o original, cur current, versionNumber int64) {
	g.t.Helper()
	b := g.checkBinding(o, cur, versionNumber)
	if string(b.env.Type) != "aa:AssetVersion" {
		g.t.Fatalf("%s: the initial snapshot came from a %s", g.dir, b.env.Type)
	}
}

// checkCurrent checks a current snapshot by what its activity is.
func (g *flowGate) checkCurrent(o original, cur current, versionNumber int64) {
	g.t.Helper()
	var typ string
	if err := g.dst.pool.QueryRow(context.Background(), `SELECT activity_type FROM federation_received_asset_revisions
		WHERE peer_id = $1 AND activity_uri = $2`, g.peerAonB, cur.ActivityURI).Scan(&typ); err != nil {
		g.t.Fatalf("%s: no revision for %s: %v", g.dir, cur.ActivityURI, err)
	}
	if typ == "Update" {
		g.checkUpdate(o, cur, versionNumber)
		return
	}
	g.checkInitial(o, cur, versionNumber)
}

func (g *flowGate) countOnSrc(sql string, args ...any) int {
	g.t.Helper()
	var n int
	if err := g.src.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	return n
}

// settle waits until this asset's work has run its course: no metadata
// work for it (nor any 'all' row) is pending on the source; every
// metadata Update and announcement of it has a source outbox row to this
// destination that is no longer queued, except the held ones; and the
// destination has no pending inbox row about it. Equal-value refreshes
// from the grant or the bootstrap are legitimate; this is where they
// end, so a later identity can be taken as stable.
func (g *flowGate) settle(o original, held ...uuid.UUID) {
	g.t.Helper()
	if held == nil {
		held = []uuid.UUID{}
	}
	g.waitFor("the asset's work settled", 120*time.Second, func() bool {
		work := g.countOnSrc(`SELECT count(*) FROM federation_metadata_work WHERE asset_id = $1 OR kind = 'all'`, o.asset)
		unrouted := g.countOnSrc(`SELECT count(*) FROM activities a WHERE a.object_local_id = $1
			AND (a.activity_type = 'aa:AssetVersion' OR (a.activity_type = 'Update' AND a.payload ? 'metadata_capture'))
			AND NOT EXISTS (SELECT 1 FROM federation_outbox ob WHERE ob.activity_id = a.id AND ob.peer_id = $2)`, o.asset.String(), g.peerBonA)
		queued := g.countOnSrc(`SELECT count(*) FROM federation_outbox ob JOIN activities a ON a.id = ob.activity_id
			WHERE ob.peer_id = $1 AND ob.status = 'queued' AND a.object_local_id = $2 AND NOT (ob.activity_id = ANY ($3))`,
			g.peerBonA, o.asset.String(), held)
		pending := g.countOnB(`SELECT count(*) FROM federation_inbox WHERE peer_id = $1 AND status = 'pending' AND envelope_json->>'object' = $2`,
			g.peerAonB, g.assetURI(o))
		return work == 0 && unrouted == 0 && queued == 0 && pending == 0
	})
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// recordRow is the destination's stored record row for an announcement,
// to show it is never rewritten by metadata.
func (g *flowGate) recordRow(activityURI string) string {
	g.t.Helper()
	var s string
	if err := g.dst.pool.QueryRow(context.Background(), `SELECT r.record::text || '|' || r.activity_uri || '|' || r.actor_uri || '|' ||
		    r.signer_key_id || '|' || (SELECT string_agg(f.object_hash || ':' || f.role, ',' ORDER BY f."position") FROM federation_received_asset_files f
		    WHERE f.received_asset_id = r.id)
		  FROM federation_received_assets r WHERE r.peer_id = $1 AND r.activity_uri = $2`, g.peerAonB, activityURI).Scan(&s); err != nil {
		g.t.Fatal(err)
	}
	return s
}

// localDigest digests the destination's own schema, values and
// permissions: what receiving must never change.
func localDigest(t *testing.T, in *instance) string {
	t.Helper()
	var s string
	q := `SELECT concat_ws('|'`
	for _, tbl := range []string{"user_capability_grants", "user_capability_revokes", "asset_type_acls", "collection_acls", "post_acls",
		"roles", "teams", "team_memberships", "team_parents", "team_closure"} {
		q += `, (SELECT coalesce(md5(string_agg(x::text, ',' ORDER BY x::text)), '') FROM (SELECT to_jsonb(r) AS x FROM ` + tbl + ` r) s)`
	}
	q += `)`
	if err := in.pool.QueryRow(context.Background(), q).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return fieldsDigest(t, in.pool) + "|" + s
}

// localFixture gives the destination a nonempty local schema and value:
// a custom definition and a value of it on a local asset, removed after t.
func (g *flowGate) localFixture() {
	g.t.Helper()
	ctx := context.Background()
	def, asset := uuid.New(), uuid.New()
	code := "flow_local_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO field_definition (id, code, label, type) VALUES ($1, $2, 'Local note', 'text')`, []any{def, code}},
		{`INSERT INTO assets (id, title, asset_type, owner_user_ref, sensitivity) VALUES ($1, 'Local asset', 1, $2, 'public')`, []any{asset, g.dst.admin}},
		{`INSERT INTO asset_field_value (asset_id, field_id, value_text) VALUES ($1, $2, 'local value')`, []any{asset, def}},
	} {
		if _, err := g.dst.pool.Exec(ctx, q.sql, q.args...); err != nil {
			g.t.Fatal(err)
		}
	}
	g.t.Cleanup(func() {
		c := context.Background()
		_, _ = g.dst.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, asset)
		_, _ = g.dst.pool.Exec(c, `DELETE FROM field_definition WHERE id = $1`, def)
	})
}

// casCounts are the source's CAS GETs and HEADs so far.
func (g *flowGate) casCounts() (int32, int32) { return g.src.casGETs.Load(), g.src.casHEADs.Load() }

// waitFor polls cond (a database or wire observation) until it holds.
func (g *flowGate) waitFor(what string, within time.Duration, cond func() bool) {
	g.t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			g.t.Fatalf("%s: %s did not happen within %v", g.dir, what, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// observed is one envelope of an inbox request, as the front saw it.
// Only what the selectors need is decoded; the request is never altered.
type observed struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Actor    string          `json:"actor"`
	Object   string          `json:"object"`
	Metadata json.RawMessage `json:"aa:metadata"`
	Asset    json.RawMessage `json:"aa:asset"`
}

// observeInbox decodes an inbox request body once into its envelopes:
// one envelope, or each of the batch wrapper's.
func observeInbox(body []byte) ([]observed, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, err
	}
	if raw, ok := probe["envelopes"]; ok {
		var batch []observed
		if err := json.Unmarshal(raw, &batch); err != nil {
			return nil, err
		}
		return batch, nil
	}
	var one observed
	if err := json.Unmarshal(body, &one); err != nil {
		return nil, err
	}
	return []observed{one}, nil
}

// markedMetadata selects the ids of the Updates by actor about object
// whose aa:metadata requiredStatement carries exactly marker.
func markedMetadata(envs []observed, actor, object, marker string) []string {
	var out []string
	for _, e := range envs {
		if e.Type != "Update" || e.Actor != actor || e.Object != object || len(e.Metadata) == 0 {
			continue
		}
		var m struct {
			RequiredStatement *struct {
				Value map[string][]string `json:"value"`
			} `json:"requiredStatement"`
		}
		if json.Unmarshal(e.Metadata, &m) != nil || m.RequiredStatement == nil {
			continue
		}
		hit := false
		for _, vs := range m.RequiredStatement.Value {
			if slices.Contains(vs, marker) {
				hit = true
			}
		}
		if hit {
			out = append(out, e.ID)
		}
	}
	return out
}

// versionAnnouncement selects the ids of the aa:AssetVersion envelopes by
// actor about object whose aa:asset names exactly versionURI.
func versionAnnouncement(envs []observed, actor, object, versionURI string) []string {
	var out []string
	for _, e := range envs {
		if e.Type != "aa:AssetVersion" || e.Actor != actor || e.Object != object {
			continue
		}
		var a struct {
			Version struct {
				ID string `json:"id"`
			} `json:"version"`
		}
		if json.Unmarshal(e.Asset, &a) == nil && a.Version.ID == versionURI {
			out = append(out, e.ID)
		}
	}
	return out
}

// holdFront is an inbox front holding (503, the whole original request,
// unaltered) every request in which match selects an envelope, recording
// the selected ids in order; every other request is forwarded with its
// exact bytes. It never fails the test itself: decode errors are kept
// for the test goroutine (errs).
type holdFront struct {
	mu    sync.Mutex
	match func([]observed) []string
	held  []string
	posts int // requests that carried a selected envelope
	errs  []string
}

func (f *holdFront) serve(w http.ResponseWriter, r *http.Request, h http.Handler) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.mu.Lock()
		f.errs = append(f.errs, "read: "+err.Error())
		f.mu.Unlock()
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	envs, err := observeInbox(body)
	var ids []string
	if err == nil {
		ids = f.match(envs)
	}
	f.mu.Lock()
	if err != nil {
		f.errs = append(f.errs, "decode: "+err.Error())
	}
	if len(ids) > 0 {
		f.held = append(f.held, ids...)
		f.posts++
	}
	f.mu.Unlock()
	if len(ids) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	forward(w, r, h, body)
}

// snapshot copies what the front saw, under its lock.
func (f *holdFront) snapshot() (held []string, posts int, errs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.held...), f.posts, append([]string(nil), f.errs...)
}

// forward hands an unmodified request body on to the application.
func forward(w http.ResponseWriter, r *http.Request, h http.Handler, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	h.ServeHTTP(w, r)
}

// outboxRow is the source's outbox row of an activity to the destination.
func (g *flowGate) outboxRow(activityID uuid.UUID) (status string, attempts int, next time.Time) {
	g.t.Helper()
	err := g.src.pool.QueryRow(context.Background(), `SELECT status, attempts, next_attempt_at FROM federation_outbox
		WHERE activity_id = $1 AND peer_id = $2 AND target_user_url IS NULL`, activityID, g.peerBonA).Scan(&status, &attempts, &next)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, time.Time{} // not fanned out yet
	}
	if err != nil {
		g.t.Fatalf("%s: read the outbox row of %s: %v", g.dir, activityID, err)
	}
	return status, attempts, next
}

// schedule moves ONLY the given activity's queued source outbox row's
// next_attempt_at (DETERMINISTIC RETRY SCHEDULING, test-only): never its
// status, payload, signature, actor, id or published.
func (g *flowGate) schedule(activityID uuid.UUID, at string) {
	g.t.Helper()
	tag, err := g.src.pool.Exec(context.Background(), `UPDATE federation_outbox SET next_attempt_at = `+at+`
		WHERE activity_id = $1 AND peer_id = $2 AND target_user_url IS NULL AND status = 'queued'`, activityID, g.peerBonA)
	if err != nil || tag.RowsAffected() != 1 {
		g.t.Fatalf("%s: schedule %s: %v (%d rows)", g.dir, activityID, err, tag.RowsAffected())
	}
}

// activityID is the source's activity id for an activity URI.
func (g *flowGate) activityID(uri string) uuid.UUID {
	g.t.Helper()
	var id uuid.UUID
	if err := g.src.pool.QueryRow(context.Background(), `SELECT id FROM activities WHERE activity_uri = $1`, uri).Scan(&id); err != nil {
		g.t.Fatal(err)
	}
	return id
}
