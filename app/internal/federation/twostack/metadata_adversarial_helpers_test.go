// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Helpers of the #1577 receiver adversarial metadata gate
// (metadata_adversarial_gate_test.go).
//
// MANUAL ADVERSARIAL WIRE ENVELOPES: every Update these helpers post is
// built and signed by the test, from the destination's stored genuine
// announcement, with the source's real actor and instance keys held only
// in test memory (never printed, persisted or exported). They are
// receiver-wire evidence, not production source emission.
//
// TEST-ONLY RECEIVER POLICY FAULT FIXTURES: the Authority group changes
// only the exact destination peer row's capabilities/enabled, or the
// exact single live inbound grant row's allow_original / expires_at /
// end columns, and restores every touched column after each case.

package twostack_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// advGate is a flow gate with one genuinely imported, settled asset and
// what the manual envelopes need.
type advGate struct {
	*flowGate
	o         original
	ann       string // the genuine announcement's activity URI
	recordID  string
	companion map[string]json.RawMessage // the announcement's aa:asset, descriptive members removed
	actor     string                     // the source owner's actor URI
	actorPEM  []byte                     // its signing key, in memory only
	instKey   ed25519.PrivateKey         // the source instance key, in memory only
	instKeyID string
	pub       []byte // the source owner's public key (PEM)
	baseline  string // the destination's local digest
	record0   string // the destination's stored original record
	held0     string // its full record, file, pin, variant and object rows
	pins0     int
	gets0     int32
	heads0    int32
	clock     time.Time // the next published time handed out
}

// descriptiveMembers are the companion members an Update with aa:metadata
// must not carry (§18.2).
var descriptiveMembers = []string{"description", "rights", "requiredStatement", "identifiers", "accessionNumber", "iiifManifest"}

const advRights = "https://creativecommons.org/licenses/by/4.0/"

// newAdvGate imports and settles one asset through the real source and
// destination workers, then loads the keys and the companion.
func newAdvGate(t *testing.T, g *flowGate) *advGate {
	t.Helper()
	ctx := context.Background()
	g.srcRun, g.dstRun = g.start(g.src), g.start(g.dst)
	g.localFixture()
	a := &advGate{flowGate: g, baseline: localDigest(t, g.dst)}
	a.o, a.ann, a.recordID = g.importOne("Adversarial baseline", "Imported for real", advRights, "Studio A")
	g.waitCurrent(a.recordID, want(sp("Adversarial baseline"), sp("Imported for real"), sp(advRights), sp("Studio A")), nil)
	g.settle(a.o)

	a.actor = g.src.url + "/users/" + g.owner
	km, err := users.NewHandler(g.src.pool, g.logger, nil).EnsureActorKeyMaterial(ctx, g.src.admin, g.src.url, g.owner)
	if err != nil {
		t.Fatalf("source actor key material: %v", err)
	}
	if a.actorPEM, err = km.DecryptSigningPrivateKey(); err != nil {
		t.Fatalf("source actor key: %v", err)
	}
	id, err := identity.NewManager(g.src.pool, g.logger).Load(ctx)
	if err != nil {
		t.Fatalf("source identity: %v", err)
	}
	a.instKey, a.instKeyID = id.PrivateKey(), g.src.url+"/federation/instance#main-key"
	var pub string
	if err := g.src.pool.QueryRow(ctx, `SELECT signing_public_key_pem FROM "user" WHERE ref = $1`, g.src.admin).Scan(&pub); err != nil || pub == "" {
		t.Fatalf("source public key: %v", err)
	}
	a.pub = []byte(pub)

	var raw []byte
	if err := g.dst.pool.QueryRow(ctx, `SELECT envelope_json->'aa:asset' FROM federation_inbox WHERE peer_id = $1 AND activity_uri = $2`,
		g.peerAonB, a.ann).Scan(&raw); err != nil {
		t.Fatalf("the stored announcement: %v", err)
	}
	if err := json.Unmarshal(raw, &a.companion); err != nil {
		t.Fatal(err)
	}
	for _, k := range descriptiveMembers {
		delete(a.companion, k)
	}
	a.record0 = g.recordRow(a.ann)
	a.held0 = a.heldState()
	a.pins0 = g.countOnB(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, a.o.hash)
	a.servedOriginal("after the import")
	a.gets0, a.heads0 = g.casCounts()
	a.clock = time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	return a
}

// next hands out a published time later than everything before it.
func (a *advGate) next() time.Time {
	a.clock = a.clock.Add(time.Second)
	return a.clock
}

func (a *advGate) newID(suffix string) string {
	return a.src.url + "/activities/" + uuid.NewString() + suffix
}

// upd describes one manual Update. A nil metadata omits aa:metadata (a
// flags-only Update); asset replaces the companion; mutate edits the
// companion copy; extra adds top-level members.
type upd struct {
	id        string
	actor     string
	object    string
	published time.Time
	metadata  json.RawMessage
	noMeta    bool
	mature    *bool
	asset     json.RawMessage
	mutate    func(map[string]json.RawMessage)
	extra     map[string]json.RawMessage
	keyID     string // the envelope signature's key id (default actor#main-key)
}

// build signs u as the source actor and returns its envelope and body.
func (a *advGate) build(u upd) (*federation.Envelope, []byte) {
	a.t.Helper()
	if u.id == "" {
		u.id = a.newID("")
	}
	if u.actor == "" {
		u.actor = a.actor
	}
	if u.object == "" {
		u.object = a.assetURI(a.o)
	}
	if u.published.IsZero() {
		u.published = a.next()
	}
	if u.keyID == "" {
		u.keyID = u.actor + "#main-key"
	}
	asset := u.asset
	if asset == nil {
		c := map[string]json.RawMessage{}
		for k, v := range a.companion {
			c[k] = v
		}
		if u.mature != nil {
			b, _ := json.Marshal(*u.mature)
			c["mature"] = b
		}
		if u.mutate != nil {
			u.mutate(c)
		}
		asset, _ = json.Marshal(c)
	}
	extra := map[string]json.RawMessage{federation.MemberArchivalAsset: asset}
	if !u.noMeta {
		extra[federation.MemberAssetMetadata] = u.metadata
	}
	for k, v := range u.extra {
		extra[k] = v
	}
	env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityUpdate, ID: u.id, Actor: u.actor,
		Object: u.object, Published: u.published, Extra: extra}
	if err := env.Sign(a.actorPEM, u.keyID); err != nil {
		a.t.Fatalf("setup: sign: %v", err)
	}
	body, err := env.Marshal()
	if err != nil {
		a.t.Fatalf("setup: marshal: %v", err)
	}
	return env, body
}

// meta is a four-key snapshot member.
func meta(title, description, rights, statement *string) json.RawMessage {
	b, _ := json.Marshal(want(title, description, rights, statement))
	return b
}

// wireResp is what the destination answered.
type wireResp struct {
	status int
	reason string
	err    error
}

func (r wireResp) String() string { return fmt.Sprintf("%d/%s (%v)", r.status, r.reason, r.err) }

// post delivers body to the destination's inbox over TLS, HTTP-signed
// over exactly these bytes with key under keyID (default: the source
// instance). It never fails the test, so goroutines can use it.
func (a *advGate) post(body []byte, keyID string, key ed25519.PrivateKey) wireResp {
	if keyID == "" {
		keyID, key = a.instKeyID, a.instKey
	}
	req, err := http.NewRequest(http.MethodPost, a.dst.url+"/federation/inbox", bytes.NewReader(body))
	if err != nil {
		return wireResp{err: err}
	}
	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("Host", req.URL.Host)
	if err := httpsig.SignAndAttach(req, body, keyID, key); err != nil {
		return wireResp{err: err}
	}
	resp, err := (&http.Client{Transport: a.tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return wireResp{err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(raw, &out)
	return wireResp{status: resp.StatusCode, reason: out.Reason}
}

// inboxRow reads the destination's inbox row for id; found is false when
// there is none.
func (a *advGate) inboxRow(id string) (status, reason, lastError string, found bool) {
	a.t.Helper()
	var r *string
	err := a.dst.pool.QueryRow(context.Background(), `SELECT status, reject_reason, last_error FROM federation_inbox
		WHERE peer_id = $1 AND activity_uri = $2`, a.peerAonB, id).Scan(&status, &r, &lastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", false
	}
	if err != nil {
		a.t.Fatalf("read the inbox row of %s: %v", id, err)
	}
	if r != nil {
		reason = *r
	}
	return status, reason, lastError, true
}

// settled waits for the inbox row of id to leave pending.
func (a *advGate) settled(id string) (status, reason, lastError string) {
	a.t.Helper()
	a.waitFor("inbox row "+id+" settled", 60*time.Second, func() bool {
		st, _, _, ok := a.inboxRow(id)
		return ok && st != "pending"
	})
	status, reason, lastError, _ = a.inboxRow(id)
	return
}

// counts are the destination's revisions and snapshots of the asset.
func (a *advGate) counts() (revisions, snapshots int) {
	a.t.Helper()
	revisions = a.countOnB(`SELECT count(*) FROM federation_received_asset_revisions WHERE peer_id = $1 AND origin_asset_uri = $2`,
		a.peerAonB, a.assetURI(a.o))
	snapshots = a.countOnB(`SELECT count(*) FROM federation_received_asset_metadata m JOIN federation_received_asset_revisions r
		ON r.id = m.revision_id WHERE r.peer_id = $1 AND r.origin_asset_uri = $2`, a.peerAonB, a.assetURI(a.o))
	return
}

// keptFor counts the destination's revisions of exactly activity id.
func (a *advGate) keptFor(id string) int {
	a.t.Helper()
	return a.countOnB(`SELECT count(*) FROM federation_received_asset_revisions WHERE peer_id = $1 AND activity_uri = $2`, a.peerAonB, id)
}

// current is the record's current_metadata (nil when none is shown).
func (a *advGate) current() *current {
	a.t.Helper()
	st, d := a.detailOnDst(a.recordID)
	if st != http.StatusOK {
		a.t.Fatalf("detail: %d", st)
	}
	return d.CurrentMetadata
}

// declaredFlags is the API's declared mature flag (overlaid by accepted
// flag Updates).
func (a *advGate) declaredMature() bool {
	a.t.Helper()
	st, d := a.detailOnDst(a.recordID)
	if st != http.StatusOK {
		a.t.Fatalf("%s: detail %d while reading the declared flags", a.dir, st)
	}
	var dec struct {
		Mature *bool `json:"mature"`
	}
	if err := json.Unmarshal(d.Declared, &dec); err != nil || dec.Mature == nil {
		a.t.Fatalf("%s: declared %s has no mature flag (%v)", a.dir, d.Declared, err)
	}
	return *dec.Mature
}

// heldState is the destination's full stored rows of the original
// received record, its files, the original's pins, variants and storage
// object, as text. No column of these tables changes on a read.
func (a *advGate) heldState() string {
	a.t.Helper()
	var s string
	if err := a.dst.pool.QueryRow(context.Background(), `SELECT concat_ws(' || ',
		(SELECT to_jsonb(r)::text FROM federation_received_assets r WHERE r.peer_id = $1 AND r.activity_uri = $2),
		(SELECT string_agg(to_jsonb(f)::text, ' | ' ORDER BY f."position") FROM federation_received_asset_files f
		  JOIN federation_received_assets r ON r.id = f.received_asset_id WHERE r.peer_id = $1 AND r.activity_uri = $2),
		(SELECT string_agg(to_jsonb(p)::text, ' | ' ORDER BY p.pin_subject_type, p.pin_subject_id) FROM storage_pins p WHERE p.object_hash = $3),
		(SELECT string_agg(to_jsonb(v)::text, ' | ' ORDER BY v.variant_key) FROM storage_variants v WHERE v.object_hash = $3),
		(SELECT to_jsonb(o)::text FROM storage_objects o WHERE o.hash = $3))`, a.peerAonB, a.ann, a.o.hash).Scan(&s); err != nil {
		a.t.Fatalf("%s: read the held record: %v", a.dir, err)
	}
	if !strings.Contains(s, a.o.hash) {
		a.t.Fatalf("%s: the held state names no original", a.dir)
	}
	return s
}

// servedOriginal requires the destination to serve the uploaded bytes to
// its member (an authorized read; nothing is fetched from the source).
func (a *advGate) servedOriginal(what string) {
	a.t.Helper()
	st, body := a.getOnB(a.member, "/federation/received-assets/"+a.recordID+"/original")
	if st != http.StatusOK || !bytes.Equal(body, a.o.body) {
		a.t.Fatalf("%s: %s the destination served %d (%d bytes, uploaded %d)", a.dir, what, st, len(body), len(a.o.body))
	}
}

// advState is what is observed BEFORE a delivery is sent: the asset's
// revisions and snapshots (counts and every row's exact contents), the
// held original rows, and, when the API may read the record, its current
// snapshot and declared flag.
type advState struct {
	revs, snaps int
	history     string
	held        string
	api         bool
	current     string // the API current_metadata, as JSON
	mature      bool
}

// observe takes an advState; api reads the API too (it must answer 200
// with a current snapshot).
func (a *advGate) observe(api bool) advState {
	a.t.Helper()
	s := advState{held: a.heldState(), api: api}
	s.revs, s.snaps = a.counts()
	if err := a.dst.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(
		    (to_jsonb(r) || jsonb_build_object('metadata', to_jsonb(m)))::text, ' | ' ORDER BY r.id), '')
		  FROM federation_received_asset_revisions r LEFT JOIN federation_received_asset_metadata m ON m.revision_id = r.id
		 WHERE r.peer_id = $1 AND r.origin_asset_uri = $2`, a.peerAonB, a.assetURI(a.o)).Scan(&s.history); err != nil {
		a.t.Fatalf("%s: read the revision history: %v", a.dir, err)
	}
	if api {
		cur := a.current()
		if cur == nil {
			a.t.Fatalf("%s: no current snapshot to observe", a.dir)
		}
		b, _ := json.Marshal(cur)
		s.current, s.mature = string(b), a.declaredMature()
	}
	return s
}

// same requires now to equal before, row for row.
func (a *advGate) same(name string, before advState) {
	a.t.Helper()
	after := a.observe(before.api)
	if after.revs != before.revs || after.snaps != before.snaps {
		a.t.Fatalf("%s: %s: revisions %d->%d, snapshots %d->%d", a.dir, name, before.revs, after.revs, before.snaps, after.snaps)
	}
	if after.history != before.history {
		a.t.Fatalf("%s: %s changed a kept revision or snapshot", a.dir, name)
	}
	if after.held != before.held {
		a.t.Fatalf("%s: %s changed the held record, files, pins or storage rows", a.dir, name)
	}
	if before.api && (after.current != before.current || after.mature != before.mature) {
		a.t.Fatalf("%s: %s changed the current snapshot or the flags", a.dir, name)
	}
	a.invariants(name)
}

// invariants: the destination's own rows, the original record and files,
// pins, and the source's CAS counters are as they were.
func (a *advGate) invariants(what string) {
	a.t.Helper()
	if localDigest(a.t, a.dst) != a.baseline {
		a.t.Fatalf("%s: %s changed the destination's own schema, values or permissions", a.dir, what)
	}
	if a.recordRow(a.ann) != a.record0 || a.heldState() != a.held0 {
		a.t.Fatalf("%s: %s changed the original record, files, pins or storage rows", a.dir, what)
	}
	if n := a.countOnB(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, a.o.hash); n != a.pins0 {
		a.t.Fatalf("%s: %s changed the pins (%d, were %d)", a.dir, what, n, a.pins0)
	}
	if g, h := a.casCounts(); g != a.gets0 || h != a.heads0 {
		a.t.Fatalf("%s: %s fetched (%d GETs, %d HEADs more)", a.dir, what, g-a.gets0, h-a.heads0)
	}
}

// refusedEarly requires, against before (observed BEFORE the delivery
// was sent), an HTTP refusal with exactly status/reason, no inbox row for
// id, no kept revision of it, and nothing else changed.
func (a *advGate) refusedEarly(name, id string, before advState, r wireResp, status int, reason federation.InboxStatus) {
	a.t.Helper()
	if r.err != nil || r.status != status || r.reason != string(reason) {
		a.t.Fatalf("%s: %s answered %s, want %d/%s", a.dir, name, r, status, reason)
	}
	if _, _, _, ok := a.inboxRow(id); ok {
		a.t.Fatalf("%s: %s: an inbox row was stored for a refused delivery", a.dir, name)
	}
	if a.keptFor(id) != 0 {
		a.t.Fatalf("%s: %s: a revision of the refused activity was kept", a.dir, name)
	}
	a.same(name, before)
}

// sendRefused observes the state, THEN sends body (HTTP-signed with key
// under keyID, or the source instance), THEN requires an early refusal.
func (a *advGate) sendRefused(name, id string, body []byte, keyID string, key ed25519.PrivateKey, status int, reason federation.InboxStatus) {
	a.t.Helper()
	before := a.observe(true)
	r := a.post(body, keyID, key)
	a.refusedEarly(name, id, before, r, status, reason)
}

// sendRefusedLate observes the state, THEN sends body, THEN requires a
// terminal asynchronous refusal.
func (a *advGate) sendRefusedLate(name, id string, body []byte, reason federation.InboxStatus, domain string) {
	a.t.Helper()
	before := a.observe(true)
	r := a.post(body, "", nil)
	a.refusedLate(name, id, before, r, reason, domain)
}

// refusedLate requires, against before, 202 and then a terminal rejected
// row with reason and a last_error containing domain, no kept revision,
// and nothing else changed.
func (a *advGate) refusedLate(name, id string, before advState, r wireResp, reason federation.InboxStatus, domain string) {
	a.t.Helper()
	if r.err != nil || r.status != http.StatusAccepted {
		a.t.Fatalf("%s: %s answered %s, want 202", a.dir, name, r)
	}
	st, rs, le := a.settled(id)
	if st != "rejected" || rs != string(reason) || !strings.Contains(le, domain) {
		a.t.Fatalf("%s: %s settled %s/%s (%q), want rejected/%s with %q", a.dir, name, st, rs, le, reason, domain)
	}
	if a.keptFor(id) != 0 {
		a.t.Fatalf("%s: %s: a revision of the refused activity was kept", a.dir, name)
	}
	a.same(name, before)
}

// accepted requires the delivery (202, or 200 for a stored receipt) to be
// kept: a processed row and a revision of id with a snapshot (unless
// flags-only). It returns the kept revision id.
func (a *advGate) accepted(name string, env *federation.Envelope, r wireResp) uuid.UUID {
	a.t.Helper()
	if r.err != nil || (r.status != http.StatusAccepted && r.status != http.StatusOK) {
		a.t.Fatalf("%s: %s answered %s, want 202", a.dir, name, r)
	}
	if st, rs, le := a.settled(env.ID); st != "processed" {
		a.t.Fatalf("%s: %s settled %s/%s (%q)", a.dir, name, st, rs, le)
	}
	var rev uuid.UUID
	if err := a.dst.pool.QueryRow(context.Background(), `SELECT id FROM federation_received_asset_revisions WHERE peer_id = $1 AND activity_uri = $2`,
		a.peerAonB, env.ID).Scan(&rev); err != nil {
		a.t.Fatalf("%s: %s: no revision kept: %v", a.dir, name, err)
	}
	a.invariants(name)
	return rev
}

// checkKept proves, from the receiver alone, that the kept revision rev
// of env is env as signed:
//
//   - the stored inbox envelope verifies with the source owner's public
//     key, and agrees with the sent envelope on id, type, actor, object,
//     published, the signature (algorithm, key, value) and its aa:asset
//     and aa:metadata members;
//   - the kept revision is this peer's, of the asset, an Update by the
//     actor, published at the signed instant (microseconds), bound to the
//     companion's exact version and to the held record's file set;
//   - its metadata row (when the Update carries aa:metadata) is the
//     asset-metadata-1 profile, the complete snapshot, the signed
//     published (seconds, nanoseconds and text), the first origin
//     algorithm, key and signature, and exactly the destination's
//     instance key as recipient;
//   - when asCurrent, the API's current_metadata is exactly this
//     revision, field for field.
func (a *advGate) checkKept(name string, env *federation.Envelope, rev uuid.UUID, asCurrent bool) {
	a.t.Helper()
	ctx := context.Background()
	fail := func(format string, args ...any) {
		a.t.Helper()
		a.t.Fatalf("%s: %s: "+format, append([]any{a.dir, name}, args...)...)
	}
	var stored []byte
	if err := a.dst.pool.QueryRow(ctx, `SELECT envelope_json FROM federation_inbox WHERE peer_id = $1 AND activity_uri = $2`,
		a.peerAonB, env.ID).Scan(&stored); err != nil {
		fail("the stored envelope: %v", err)
	}
	se, err := federation.Unmarshal(stored)
	if err != nil {
		fail("the stored envelope does not parse: %v", err)
	}
	if err := se.Verify(a.pub); err != nil {
		fail("the stored envelope does not verify with the source owner's key: %v", err)
	}
	if se.ID != env.ID || se.Type != env.Type || se.Actor != env.Actor || se.Actor != a.actor || se.Object != env.Object ||
		!se.Published.Equal(env.Published) || se.Signature == nil || env.Signature == nil || se.Signature.Type != env.Signature.Type ||
		se.Signature.PublicKey != env.Signature.PublicKey || se.Signature.Value != env.Signature.Value ||
		!sameJSON(se.Extra[federation.MemberArchivalAsset], env.Extra[federation.MemberArchivalAsset]) {
		fail("the stored envelope is not the one sent")
	}
	md, hasMeta := env.Extra[federation.MemberAssetMetadata]
	if sm, ok := se.Extra[federation.MemberAssetMetadata]; ok != hasMeta || (hasMeta && !sameJSON(sm, md)) {
		fail("the stored aa:metadata is not the one sent")
	}
	var rawPublished struct {
		Published string `json:"published"`
	}
	if err := json.Unmarshal(stored, &rawPublished); err != nil || rawPublished.Published == "" {
		fail("the stored envelope's published: %v", err)
	}
	var version struct {
		Version struct {
			ID string `json:"id"`
		} `json:"version"`
	}
	if err := json.Unmarshal(env.Extra[federation.MemberArchivalAsset], &version); err != nil || version.Version.ID == "" {
		fail("the sent companion's version: %v", err)
	}
	var (
		peerID                                                                uuid.UUID
		assetURI, typ, actor, versionURI, fileSet                             string
		published                                                             time.Time
		hasRow                                                                bool
		profile, publishedText, originAlgo, originKey, originValue, recipient string
		snapshot                                                              []byte
		secs                                                                  int64
		nanos                                                                 int32
	)
	if err := a.dst.pool.QueryRow(ctx, `SELECT r.peer_id, r.origin_asset_uri, r.activity_type, r.actor_uri, r.version_uri, r.file_set_sha256,
		    r.published, m.revision_id IS NOT NULL, COALESCE(m.profile_token, ''), COALESCE(m.snapshot, 'null'::jsonb),
		    COALESCE(m.published_seconds, 0), COALESCE(m.published_nanos, 0), COALESCE(m.published_text, ''),
		    COALESCE(m.origin_signature_algorithm, ''), COALESCE(m.origin_signer_key_id, ''), COALESCE(m.origin_signature_value, ''),
		    COALESCE(m.recipient_signer_key_id, '')
		  FROM federation_received_asset_revisions r LEFT JOIN federation_received_asset_metadata m ON m.revision_id = r.id
		 WHERE r.id = $1 AND r.activity_uri = $2`, rev, env.ID).Scan(&peerID, &assetURI, &typ, &actor, &versionURI, &fileSet,
		&published, &hasRow, &profile, &snapshot, &secs, &nanos, &publishedText, &originAlgo, &originKey, &originValue, &recipient); err != nil {
		fail("the kept revision: %v", err)
	}
	var held string
	err = a.dst.pool.QueryRow(ctx, `SELECT file_set_sha256 FROM federation_received_assets WHERE peer_id = $1 AND origin_asset_uri = $2
		AND version_uri = $3 AND state IN ('complete', 'superseded')`, a.peerAonB, a.assetURI(a.o), versionURI).Scan(&held)
	if err != nil {
		fail("no held record of %s: %v", versionURI, err)
	}
	if peerID != a.peerAonB || assetURI != a.assetURI(a.o) || assetURI != env.Object || typ != string(env.Type) || typ != "Update" ||
		actor != a.actor || versionURI != version.Version.ID || fileSet != held ||
		!published.Equal(env.Published.Truncate(time.Microsecond)) {
		fail("revision %s %s %s %s %s at %s (held %s)", peerID, assetURI, typ, versionURI, fileSet, published.Format(time.RFC3339Nano), held)
	}
	wantRecipient := a.dst.url + "/federation/instance#main-key"
	if hasMeta {
		snapOf(a.t, snapshot)
		if !hasRow || profile != federation.ProfileAssetMetadata1 || !sameJSON(snapshot, md) || secs != env.Published.Unix() ||
			nanos != int32(env.Published.Nanosecond()) || publishedText != rawPublished.Published ||
			originAlgo != string(env.Signature.Type) || originKey != env.Signature.PublicKey || originValue != env.Signature.Value ||
			recipient != wantRecipient {
			fail("kept %s %s %d.%09d %q %s %s %s, sent %s %s", profile, snapshot, secs, nanos, publishedText, originAlgo, originKey,
				recipient, md, env.Published.Format(time.RFC3339Nano))
		}
	} else if hasRow {
		fail("a flags-only Update kept a snapshot")
	}
	if asCurrent {
		cur := a.current()
		if cur == nil || cur.ActivityURI != env.ID || cur.RevisionID != rev.String() || cur.Profile != federation.ProfileAssetMetadata1 ||
			cur.PeerID != a.peerAonB.String() || cur.OriginAssetURI != assetURI || cur.ActorURI != a.actor ||
			cur.VersionURI != versionURI || cur.FileSetSHA256 != fileSet || !sameJSON(cur.Snapshot, snapshot) ||
			!cur.Published.Equal(env.Published) || cur.PublishedSeconds != secs || cur.PublishedNanos != nanos ||
			cur.OriginSignatureAlgo != originAlgo || cur.OriginSignerKeyID != originKey || cur.OriginSignatureValue != originValue ||
			cur.RecipientSignerKeyID != recipient {
			fail("the API current snapshot %+v is not revision %s", cur, rev)
		}
	}
}

// randomKey is an instance key nobody paired.
func randomKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// padMetadata replaces the signed body's compact aa:metadata by the same
// member with spaces inside its opening brace so the raw member is
// exactly size bytes, without re-encoding the body.
func padMetadata(t *testing.T, body []byte, size int) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	member := string(m[federation.MemberAssetMetadata])
	if size < len(member) {
		t.Fatalf("setup: %d bytes is shorter than the member", size)
	}
	padded := "{" + strings.Repeat(" ", size-len(member)) + member[1:]
	old := `"` + federation.MemberAssetMetadata + `":` + member
	if !bytes.Contains(body, []byte(old)) {
		t.Fatal("setup: the compact member is not in the body")
	}
	out := bytes.Replace(body, []byte(old), []byte(`"`+federation.MemberAssetMetadata+`":`+padded), 1)
	env, err := federation.Unmarshal(out)
	if err != nil || len(env.Extra[federation.MemberAssetMetadata]) != size {
		t.Fatalf("setup: the raw member did not survive parsing (%v, %d bytes)", err, len(env.Extra[federation.MemberAssetMetadata]))
	}
	return out
}

// replaceMetadata replaces the compact aa:metadata in body by raw, byte
// for byte (the result may not parse: a duplicate key).
func replaceMetadata(t *testing.T, body []byte, raw string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	old := `"` + federation.MemberAssetMetadata + `":` + string(m[federation.MemberAssetMetadata])
	if !bytes.Contains(body, []byte(old)) {
		t.Fatal("setup: the compact member is not in the body")
	}
	return bytes.Replace(body, []byte(old), []byte(`"`+federation.MemberAssetMetadata+`":`+raw), 1)
}

// policy is one TEST-ONLY RECEIVER POLICY FAULT FIXTURE target: the exact
// destination peer row and the exact single live peer-wide inbound grant
// for the asset, as they are now.
type policy struct {
	a       *advGate
	grantID uuid.UUID
	peer0   string // the peer row, every column but updated_at
	grant0  string // the grant row
}

func (a *advGate) policy() *policy {
	a.t.Helper()
	ctx := context.Background()
	p := &policy{a: a}
	rows, err := a.dst.pool.Query(ctx, `SELECT id FROM federation_inbound_grants WHERE peer_id = $1 AND object_kind = 'asset'
		AND object_uri = $2 AND target_user_uri IS NULL AND state = 'live'`, a.peerAonB, a.assetURI(a.o))
	if err != nil {
		a.t.Fatal(err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			a.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		a.t.Fatal(err)
	}
	if len(ids) != 1 {
		a.t.Fatalf("%s: %d live peer-wide grants for the asset, want exactly 1", a.dir, len(ids))
	}
	p.grantID = ids[0]
	p.peer0, p.grant0 = p.peerRow(), p.grantRow()
	a.t.Cleanup(p.restore)
	return p
}

func (p *policy) peerRow() string {
	p.a.t.Helper()
	var s string
	if err := p.a.dst.pool.QueryRow(context.Background(), `SELECT (to_jsonb(p) - 'updated_at')::text FROM federation_peers p WHERE id = $1`,
		p.a.peerAonB).Scan(&s); err != nil {
		p.a.t.Fatal(err)
	}
	return s
}

func (p *policy) grantRow() string {
	p.a.t.Helper()
	var s string
	if err := p.a.dst.pool.QueryRow(context.Background(), `SELECT to_jsonb(g)::text FROM federation_inbound_grants g WHERE id = $1`,
		p.grantID).Scan(&s); err != nil {
		p.a.t.Fatal(err)
	}
	return s
}

func (p *policy) exec(sql string, args ...any) {
	p.a.t.Helper()
	tag, err := p.a.dst.pool.Exec(context.Background(), sql, args...)
	if err != nil || tag.RowsAffected() != 1 {
		p.a.t.Fatalf("policy fixture: %v (%d rows)", err, tag.RowsAffected())
	}
}

// restore puts the peer's capabilities/enabled and the grant's policy
// columns back exactly, and proves both rows are as they were.
func (p *policy) restore() {
	ctx := context.Background()
	var peer struct {
		Capabilities json.RawMessage `json:"capabilities"`
		Enabled      bool            `json:"enabled"`
	}
	_ = json.Unmarshal([]byte(p.peer0), &peer)
	var grant struct {
		AllowOriginal bool       `json:"allow_original"`
		ExpiresAt     *time.Time `json:"expires_at"`
		State         string     `json:"state"`
		EndReason     *string    `json:"end_reason"`
		EndedBy       *string    `json:"ended_by_activity_uri"`
		EndedAt       *time.Time `json:"ended_at"`
	}
	_ = json.Unmarshal([]byte(p.grant0), &grant)
	_, _ = p.a.dst.pool.Exec(ctx, `UPDATE federation_peers SET capabilities = $2::jsonb, enabled = $3 WHERE id = $1`,
		p.a.peerAonB, string(peer.Capabilities), peer.Enabled)
	_, _ = p.a.dst.pool.Exec(ctx, `UPDATE federation_inbound_grants SET allow_original = $2, expires_at = $3, state = $4,
		end_reason = $5, ended_by_activity_uri = $6, ended_at = $7 WHERE id = $1`,
		p.grantID, grant.AllowOriginal, grant.ExpiresAt, grant.State, grant.EndReason, grant.EndedBy, grant.EndedAt)
}

func (p *policy) restored(what string) {
	p.a.t.Helper()
	p.restore()
	if p.peerRow() != p.peer0 || p.grantRow() != p.grant0 {
		p.a.t.Fatalf("%s: after %s the policy rows are not restored exactly", p.a.dir, what)
	}
}
