// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 (spec v1 §18.4): the current asset-metadata-1 snapshot on a
// received record's detail, current_metadata.
//
// Layer: a live database holding the rows the asset worker keeps for an
// accepted metadata Update or announcement (a revision and its metadata
// row), written here directly; the production Handler.GetReceivedAsset
// reads them, and the generated response writer
// (VisitGetReceivedAssetResponse) puts the answer on the wire, decoded
// here as generic JSON. These are not signed wire tests: no envelope is
// signed, received or admitted. The HTTP route reuses this handler
// unchanged (http.apiServer.GetReceivedAsset delegates to it). Skips
// without AA_DB_PASSWORD, like the rest of the package.

package receivedassets_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/receivedassets"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

var (
	fileSet0 = strings.Repeat("0", 64) // f.insert's file set (version 1)
	fileSet1 = strings.Repeat("1", 64) // f.supersede's file set (version 2)
	t0       = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
)

const (
	recipientKey = localBase + "/actor#main-key"
	allProfiles  = `["share-lifecycle-1","archival-1","original-transfer-1","asset-metadata-1"]`
	noMetadata   = `["share-lifecycle-1","archival-1","original-transfer-1"]`
	clearAll     = `{"title":null,"description":null,"rights":null,"requiredStatement":null}`
)

func v1(r record) string { return r.object + "/versions/1" }
func v2(r record) string { return r.object + "/versions/2" }

// kept is one stored snapshot, as written by keep.
type kept struct {
	revision   uuid.UUID
	activity   string
	actor      string
	at         time.Time
	version    string
	fileSet    string
	snapshot   string
	receivedAt time.Time
}

// keep stores snapshot for r's asset as the asset worker keeps an
// accepted one: an immutable revision of type typ (Update, or
// aa:AssetVersion for an announcement's initial snapshot) naming
// version and fileSet, and its metadata row with the signed published to
// the nanosecond, the declared origin signature and this instance's
// recipient identity.
func (f *fx) keep(r record, typ, name, version, fileSet string, at time.Time, snapshot string) kept {
	f.t.Helper()
	ctx := context.Background()
	k := kept{activity: r.peer.url + "/activities/" + name, actor: r.peer.url + "/users/alice", at: at,
		version: version, fileSet: fileSet, snapshot: snapshot}
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO federation_received_asset_revisions
			(peer_id, origin_asset_uri, activity_uri, activity_type, actor_uri, published, version_uri, file_set_sha256, mature, ai_provenance, record)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, FALSE, NULL, '{}'::jsonb) RETURNING id`,
			r.peer.id, r.object, k.activity, typ, k.actor, at, version, fileSet).Scan(&k.revision); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO federation_received_asset_metadata
			(revision_id, profile_token, snapshot, published_seconds, published_nanos, published_text,
			 origin_signer_key_id, origin_signature_algorithm, origin_signature_value, recipient_signer_key_id)
			VALUES ($1, 'asset-metadata-1', $2::jsonb, $3, $4, $5, $6, 'Ed25519', $7, $8) RETURNING received_at`,
			k.revision, snapshot, at.Unix(), at.Nanosecond(), at.Format(time.RFC3339Nano),
			k.actor+"#main-key", "sig-"+name, recipientKey).Scan(&k.receivedAt)
	}); err != nil {
		f.t.Fatalf("keep %s: %v", name, err)
	}
	return k
}

// flagsOnly stores a flags-only Update revision (no snapshot).
func (f *fx) flagsOnly(r record, name string, at time.Time, mature bool) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO federation_received_asset_revisions
		(peer_id, origin_asset_uri, activity_uri, activity_type, actor_uri, published, version_uri, file_set_sha256, mature, ai_provenance, record)
		VALUES ($1, $2, $3, 'Update', $4, $5, $6, $7, $8, NULL, '{}'::jsonb)`,
		r.peer.id, r.object, r.peer.url+"/activities/"+name, r.peer.url+"/users/alice", at, v1(r), fileSet0, mature); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) profiles(p peer, caps string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, p.id, caps); err != nil {
		f.t.Fatal(err)
	}
}

// extraPeer is a connected peer of its own, removed with the test.
func (f *fx) extraPeer() peer {
	f.t.Helper()
	p := f.newPeer()
	f.cleanup = append(f.cleanup, func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE id = $1`, p.id)
	})
	f.profiles(p, allProfiles)
	return p
}

// wire is a detail answer as the generated response writer sends it.
type wire struct {
	status int
	header http.Header
	body   map[string]any
}

func (f *fx) wireOf(h *receivedassets.Handler, ctx context.Context, id uuid.UUID) wire {
	f.t.Helper()
	resp, err := h.GetReceivedAsset(ctx, openapi.GetReceivedAssetRequestObject{Id: id})
	if err != nil {
		f.t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := resp.VisitGetReceivedAssetResponse(rec); err != nil {
		f.t.Fatal(err)
	}
	w := wire{status: rec.Code, header: rec.Header()}
	dec := json.NewDecoder(rec.Body)
	dec.UseNumber()
	if err := dec.Decode(&w.body); err != nil {
		f.t.Fatal(err)
	}
	return w
}

func (f *fx) detail(caller int64, r record) wire { return f.wireOf(f.h, as(caller), r.id) }

func decodeJSON(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stable is the detail without current_metadata: what existed before it.
func stable(w wire) map[string]any {
	m := map[string]any{}
	for k, v := range w.body {
		if k != "current_metadata" {
			m[k] = v
		}
	}
	return m
}

var currentKeys = []string{"activity_uri", "actor_uri", "file_set_sha256", "origin_asset_uri", "origin_signature_algorithm",
	"origin_signature_value", "origin_signer_key_id", "peer_id", "profile", "published", "published_nanos", "published_seconds",
	"received_at", "recipient_signer_key_id", "revision_id", "snapshot", "version_uri"}

// wantCurrent fails unless w is a 200 whose current_metadata is k, for
// r's asset, with every provenance field exact.
func (f *fx) wantCurrent(name string, w wire, r record, k kept) {
	t := f.t
	t.Helper()
	if w.status != http.StatusOK || w.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%s: status %d, Cache-Control %q", name, w.status, w.header.Get("Cache-Control"))
	}
	raw, ok := w.body["current_metadata"]
	if !ok {
		t.Fatalf("%s: current_metadata omitted, want the snapshot of %s", name, k.activity)
	}
	cm, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s: current_metadata is %T", name, raw)
	}
	keys := make([]string, 0, len(cm))
	for key := range cm {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, currentKeys) {
		t.Fatalf("%s: current_metadata keys %v", name, keys)
	}
	str := func(key string) string { s, _ := cm[key].(string); return s }
	num := func(key string) string { n, _ := cm[key].(json.Number); return n.String() }
	for key, want := range map[string]string{
		"profile": "asset-metadata-1", "revision_id": k.revision.String(), "peer_id": r.peer.id.String(),
		"origin_asset_uri": r.object, "activity_uri": k.activity, "actor_uri": k.actor, "version_uri": k.version,
		"file_set_sha256": k.fileSet, "origin_signer_key_id": k.actor + "#main-key", "origin_signature_algorithm": "Ed25519",
		"origin_signature_value": "sig-" + k.activity[strings.LastIndex(k.activity, "/")+1:], "recipient_signer_key_id": recipientKey,
		"published_seconds": strconv.FormatInt(k.at.Unix(), 10), "published_nanos": strconv.Itoa(k.at.Nanosecond()),
	} {
		got := str(key)
		if key == "published_seconds" || key == "published_nanos" {
			got = num(key)
		}
		if got != want {
			t.Errorf("%s: %s = %q, want %q", name, key, got, want)
		}
	}
	if str("origin_signer_key_id") == str("recipient_signer_key_id") {
		t.Errorf("%s: the origin and recipient identities are the same", name)
	}
	if p, err := time.Parse(time.RFC3339Nano, str("published")); err != nil || !p.Equal(k.at) {
		t.Errorf("%s: published %q (%v), want %s", name, str("published"), err, k.at.Format(time.RFC3339Nano))
	}
	if p, err := time.Parse(time.RFC3339Nano, str("received_at")); err != nil || !p.Equal(k.receivedAt) {
		t.Errorf("%s: received_at %q (%v), want %s", name, str("received_at"), err, k.receivedAt)
	}
	if want := decodeJSON(t, k.snapshot); !reflect.DeepEqual(cm["snapshot"], want) {
		t.Errorf("%s: snapshot\n got %s\nwant %s", name, mustJSON(t, cm["snapshot"]), mustJSON(t, want))
	}
}

func (f *fx) wantNoCurrent(name string, w wire) {
	f.t.Helper()
	if w.status != http.StatusOK {
		f.t.Fatalf("%s: status %d, want 200", name, w.status)
	}
	if cm, ok := w.body["current_metadata"]; ok {
		f.t.Fatalf("%s: current_metadata %s, want it omitted", name, mustJSON(f.t, cm))
	}
}

// wantNotFound: the same 404 as a missing record, with nothing else.
func (f *fx) wantNotFound(name string, w wire) {
	f.t.Helper()
	if w.status != http.StatusNotFound || !reflect.DeepEqual(w.body, map[string]any{"error": "received asset not found"}) {
		f.t.Errorf("%s: %d %s, want the plain 404", name, w.status, mustJSON(f.t, w.body))
	}
}

// A. A record whose snapshots were never kept (no profile at admission,
// or kept before #1577) has no current_metadata. Its own claims, a
// flags-only revision and the profiles negotiated now synthesize none.
func TestCurrentMetadata_LegacyRecordHasNone(t *testing.T) {
	f := newFx(t)
	r := f.complete(f.peerA, "public")
	f.grant(r, false, "", "")
	before := f.detail(f.u1, r)
	f.wantNoCurrent("legacy", before)
	if d := before.body["declared"].(map[string]any); d["rights"] != "http://rightsstatements.org/vocab/InC/1.0/" ||
		!reflect.DeepEqual(d["description"], map[string]any{"title": "Harbour at dusk"}) {
		t.Fatalf("the legacy claims are not declared: %v", d)
	}
	if before.body["original_available"] != false {
		t.Fatal("a view-only grant reports the original available")
	}
	f.flagsOnly(r, "flags-"+uuid.NewString(), t0, false)
	f.profiles(f.peerA, allProfiles)
	after := f.detail(f.u1, r)
	f.wantNoCurrent("after a flags-only revision and the profiles", after)
	if !reflect.DeepEqual(stable(after), stable(before)) {
		t.Fatalf("the detail changed:\n got %s\nwant %s", mustJSON(t, stable(after)), mustJSON(t, stable(before)))
	}
}

// B and G. A snapshot is its own typed object beside the original claims:
// literal markup, URI-looking text and Unicode exactly as kept, the
// timestamps to the nanosecond, and origin and recipient provenance kept
// apart. The record, its files, its declared claims and the caller's
// permissions are unchanged. A kept initial snapshot beyond an Update
// member's wire bounds (§18.4) reads back whole.
func TestCurrentMetadata_SnapshotBesideTheOriginalClaims(t *testing.T) {
	f := newFx(t)
	f.profiles(f.peerA, allProfiles)
	r := f.complete(f.peerA, "public")
	f.grant(r, false, "", "")
	before := f.detail(f.u1, r)
	f.wantNoCurrent("before any snapshot", before)

	literal := mustJSON(t, map[string]any{
		"title":       "<script>alert(\"x\")</script> Café 東京 ‮🎨",
		"description": "line one\n\tjavascript:alert(1) <a href=\"https://evil.example/\">here</a> & {{x}}",
		"rights":      "https://creativecommons.org/licenses/by/4.0/",
		"requiredStatement": map[string]any{
			"label": map[string]any{"en": []string{"Attribution"}, "fr": []string{"Attribution", "Deuxième"}},
			"value": map[string]any{"none": []string{"Studio A <b>bold</b>", "https://evil.example/?q=<x>&y=1"}},
		},
	})
	at := t0.Add(123456789 * time.Nanosecond)
	k := f.keep(r, "Update", "meta-"+uuid.NewString(), v1(r), fileSet0, at, literal)
	got := f.detail(f.u1, r)
	f.wantCurrent("literal snapshot", got, r, k)
	if !reflect.DeepEqual(stable(got), stable(before)) {
		t.Fatalf("the original claims, record or files changed:\n got %s\nwant %s", mustJSON(t, stable(got)), mustJSON(t, stable(before)))
	}
	if got.body["original_available"] != false {
		t.Fatal("a snapshot changed the caller's original-file permission")
	}

	// G: initial snapshots kept from an announcement under archival-1's
	// rules that an aa:metadata member could not carry (§18.4), read back
	// whole. Each is checked legacy-valid and member-invalid first.
	//
	// 1. rights longer than the member's 2,048 bytes, the rest well within
	// both: archival-1 bounds rights only by its pattern. (A URI with a
	// fragment does not match it.) The whole member is under 32,768 bytes,
	// so only the rights bound refuses it.
	if federation.ArchivalRightsURIValid("https://creativecommons.org/licenses/by/4.0/#a") {
		t.Fatal("archival-1 accepts a rights URI with a fragment")
	}
	longRights := "https://creativecommons.org/licenses/by/4" + strings.Repeat(".0", 1100) + "/"
	k1, raw1 := f.legacyInitial(r, "initial-rights-"+uuid.NewString(), at.Add(time.Second), "Harbour at dusk", "A short description.", longRights, nil)
	if len(longRights) <= federation.AssetMetadataMaxRightsBytes || len(raw1) >= federation.AssetMetadataMaxMemberBytes {
		t.Fatalf("rights %d bytes, member %d bytes", len(longRights), len(raw1))
	}
	f.wantCurrent("legacy rights beyond the member's rights bound", f.detail(f.u1, r), r, k1)

	// 2. a requiredStatement of nine 4,096-byte texts under one tag, each
	// text and the list within archival-1's limits, with a title and a
	// description at their common maximum: over the member's 32,768 raw
	// bytes. The texts differ, so order and every byte are compared.
	texts := make([]string, 9)
	for i := range texts {
		p := fmt.Sprintf("part %d of 9: ", i+1)
		texts[i] = p + strings.Repeat(string(rune('a'+i)), 4096-len(p))
	}
	rs := &federation.ArchivalLabelValue{Label: map[string][]string{"en": {"Credit"}}, Value: map[string][]string{"en": texts}}
	k2, raw2 := f.legacyInitial(r, "initial-statement-"+uuid.NewString(), at.Add(2*time.Second),
		strings.Repeat("T", federation.AssetMetadataMaxTitleBytes), strings.Repeat("d", federation.AssetMetadataMaxDescriptionBytes),
		"https://creativecommons.org/licenses/by/4.0/", rs)
	if len(raw2) <= federation.AssetMetadataMaxMemberBytes {
		t.Fatalf("member %d bytes, want more than %d", len(raw2), federation.AssetMetadataMaxMemberBytes)
	}
	for _, x := range texts {
		if len(x) != 4096 {
			t.Fatalf("a text of %d bytes", len(x))
		}
	}
	got2 := f.detail(f.u1, r)
	f.wantCurrent("a legacy requiredStatement beyond the member's raw bound", got2, r, k2)
	value := got2.body["current_metadata"].(map[string]any)["snapshot"].(map[string]any)["requiredStatement"].(map[string]any)["value"].(map[string]any)["en"].([]any)
	if len(value) != len(texts) {
		t.Fatalf("%d texts, want %d", len(value), len(texts))
	}
	for i, v := range value {
		if v != texts[i] {
			t.Fatalf("text %d changed: %d bytes", i, len(v.(string)))
		}
	}
}

// legacyInitial keeps an announcement's initial snapshot (§18.4) for r,
// as a synthetic stored row: it is not a signed announcement or an
// admission test. Before writing, it checks that every claim is one
// archival-1 accepts (ValidateArchivalDescription, ArchivalRightsURIValid,
// ValidateArchivalRequiredStatement) and that the same snapshot is NOT a
// valid aa:metadata member (ErrMetadataInvalid): the case §18.4 keeps
// whole, under the record's rules. It returns the kept row and the
// snapshot's serialized bytes.
func (f *fx) legacyInitial(r record, name string, at time.Time, title, description, rights string, rs *federation.ArchivalLabelValue) (kept, string) {
	t := f.t
	t.Helper()
	if err := federation.ValidateArchivalDescription(federation.ArchivalDescription{Title: title, Description: description}); err != nil {
		t.Fatalf("%s: not a description archival-1 accepts: %v", name, err)
	}
	if !federation.ArchivalRightsURIValid(rights) {
		t.Fatalf("%s: not a rights URI archival-1 accepts", name)
	}
	snap := map[string]any{"title": title, "description": description, "rights": rights, "requiredStatement": nil}
	if rs != nil {
		if err := federation.ValidateArchivalRequiredStatement(*rs); err != nil {
			t.Fatalf("%s: not a requiredStatement archival-1 accepts: %v", name, err)
		}
		snap["requiredStatement"] = rs
	}
	raw := mustJSON(t, snap)
	if _, err := federation.ParseAssetMetadata(json.RawMessage(raw)); !errors.Is(err, federation.ErrMetadataInvalid) {
		t.Fatalf("%s: as an aa:metadata member: %v, want ErrMetadataInvalid", name, err)
	}
	return f.keep(r, "aa:AssetVersion", name, v1(r), fileSet0, at, raw), raw
}

// C. Four nulls are a snapshot that clears every claim: present, never
// omitted, and no fallback to the original claims. A later flags-only
// revision changes the flags only. Order is the signed published to the
// nanosecond, then the activity URI byte by byte, whatever arrived when.
func TestCurrentMetadata_ClearAndOrder(t *testing.T) {
	f := newFx(t)
	f.profiles(f.peerA, allProfiles)
	r := f.complete(f.peerA, "public")
	f.grant(r, true, "", "")
	values := func(title string) string {
		return mustJSON(t, map[string]any{"title": title, "description": nil, "rights": "https://creativecommons.org/licenses/by/4.0/", "requiredStatement": nil})
	}
	p := "o-" + uuid.NewString()[:8] + "-"
	k1 := f.keep(r, "Update", p+"m", v1(r), fileSet0, t0.Add(time.Second), values("one"))
	f.wantCurrent("first", f.detail(f.u1, r), r, k1)
	f.keep(r, "Update", p+"z", v1(r), fileSet0, t0, values("older, arriving later"))
	f.wantCurrent("an older arrival", f.detail(f.u1, r), r, k1)

	clear := f.keep(r, "Update", p+"b", v1(r), fileSet0, t0.Add(2*time.Second), clearAll)
	got := f.detail(f.u1, r)
	f.wantCurrent("the clear", got, r, clear)
	if s := got.body["current_metadata"].(map[string]any)["snapshot"].(map[string]any); len(s) != 4 ||
		s["title"] != nil || s["description"] != nil || s["rights"] != nil || s["requiredStatement"] != nil {
		t.Fatalf("the clear is not four explicit nulls: %v", s)
	}
	if d := got.body["declared"].(map[string]any); d["rights"] != "http://rightsstatements.org/vocab/InC/1.0/" {
		t.Fatalf("the clear changed the original claims: %v", d)
	}
	f.flagsOnly(r, p+"flags", t0.Add(3*time.Second), true)
	got = f.detail(f.u1, r)
	f.wantCurrent("after a later flags-only revision", got, r, clear)
	if got.body["declared"].(map[string]any)["mature"] != true {
		t.Fatal("the flags-only revision's flags are not shown")
	}

	// The same instant: the greater activity URI, compared byte by byte
	// ("a" 0x61 sorts after "Z" 0x5A; a linguistic collation would not).
	f.keep(r, "Update", p+"Z", v1(r), fileSet0, t0.Add(4*time.Second), values("upper"))
	lower := f.keep(r, "Update", p+"a", v1(r), fileSet0, t0.Add(4*time.Second), values("lower"))
	f.wantCurrent("the byte-greater URI", f.detail(f.u1, r), r, lower)
	// One nanosecond later wins, whatever its URI.
	nano := f.keep(r, "Update", p+"0", v1(r), fileSet0, t0.Add(4*time.Second+time.Nanosecond), values("nano"))
	f.wantCurrent("one nanosecond later", f.detail(f.u1, r), r, nano)
}

// D. A snapshot naming a version not held, pending, rejected or held
// with another file set is kept but not current; the version's verified
// arrival makes it current. The record's files are never chosen by a
// snapshot: a PUBLIC superseded version's snapshot is current on the
// current version's detail, with its own binding.
func TestCurrentMetadata_OnlyAHeldVerifiedVersionActivates(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.profiles(f.peerA, allProfiles)
	r := f.complete(f.peerA, "public")
	f.grant(r, true, "", "")
	snap := func(title string) string {
		return mustJSON(t, map[string]any{"title": title, "description": nil, "rights": nil, "requiredStatement": nil})
	}
	p := "b-" + uuid.NewString()[:8] + "-"
	k1 := f.keep(r, "Update", p+"1", v1(r), fileSet0, t0, snap("held"))
	f.wantCurrent("held version 1", f.detail(f.u1, r), r, k1)
	k2 := f.keep(r, "Update", p+"2", v2(r), fileSet1, t0.Add(time.Second), snap("version 2, not held"))
	f.wantCurrent("a version not held", f.detail(f.u1, r), r, k1)
	f.keep(r, "Update", p+"3", v1(r), strings.Repeat("2", 64), t0.Add(2*time.Second), snap("another file set"))
	f.wantCurrent("another file set", f.detail(f.u1, r), r, k1)

	// Version 2 announced, then rejected.
	pending := uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO federation_received_assets
		(id, peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
		 file_set_sha256, record, sensitivity, signer_key_id, predecessor_version_uri)
		SELECT $1, peer_id, $2, actor_uri, origin_asset_uri, origin_asset_id, $3, 2, $4, record, sensitivity, 'k', version_uri
		  FROM federation_received_assets WHERE id = $5`, pending, r.peer.url+"/activities/"+uuid.NewString(), v2(r), fileSet1, r.id); err != nil {
		t.Fatal(err)
	}
	f.wantCurrent("version 2 pending", f.detail(f.u1, r), r, k1)
	if _, err := f.pool.Exec(ctx, `UPDATE federation_received_assets SET state = 'rejected', reason = 'fixity_mismatch' WHERE id = $1`, pending); err != nil {
		t.Fatal(err)
	}
	f.wantCurrent("version 2 rejected", f.detail(f.u1, r), r, k1)

	// Version 2 verified: its snapshot is current, on its own detail.
	next := f.supersede(r, "public")
	got := f.detail(f.u1, next)
	f.wantCurrent("version 2 verified", got, next, k2)
	f.wantNotFound("version 1's detail", f.detail(f.u1, r))

	// A newer snapshot bound to version 1, now superseded and public: it is
	// current at the asset level, and the detail stays version 2's.
	k4 := f.keep(r, "Update", p+"4", v1(r), fileSet0, t0.Add(3*time.Second), snap("bound to version 1"))
	got4 := f.detail(f.u1, next)
	f.wantCurrent("a public superseded binding", got4, next, k4)
	if !reflect.DeepEqual(stable(got4), stable(got)) {
		t.Fatal("a snapshot bound to version 1 changed version 2's detail")
	}
	files := got4.body["files"].([]any)
	if a := got4.body["asset"].(map[string]any); a["version_uri"] != v2(r) || len(files) != 1 ||
		files[0].(map[string]any)["sha256"] != next.hash {
		t.Fatalf("the detail's version or files: %v %v", a, files)
	}
}

// E. A snapshot is shown only when its own version is held here as
// PUBLIC: the current version's tier does not authorize claims kept with
// a team version. The newest candidate is chosen first; when its binding
// is not public, current_metadata is omitted, and an older public
// snapshot is not shown in its place. A PUBLIC superseded binding stays
// shown.
func TestCurrentMetadata_PrivateBindingIsNotShown(t *testing.T) {
	f := newFx(t)
	f.profiles(f.peerA, allProfiles)
	snap := func(title string) string {
		return mustJSON(t, map[string]any{"title": title, "description": title, "rights": nil, "requiredStatement": nil})
	}
	p := "e-" + uuid.NewString()[:8] + "-"

	// Version 1 team, version 2 public.
	team := f.complete(f.peerA, "team")
	f.grant(team, true, "", "")
	pub := f.supersede(team, "public")
	old := f.keep(team, "Update", p+"old", v2(team), fileSet1, t0, snap("public, older"))
	before := f.detail(f.u1, pub)
	f.wantCurrent("the public binding", before, pub, old)
	f.keep(team, "Update", p+"new", v1(team), fileSet0, t0.Add(time.Second), snap("team, newer"))
	after := f.detail(f.u1, pub)
	f.wantNoCurrent("the newest snapshot is bound to the team version", after)
	if !reflect.DeepEqual(stable(after), stable(before)) {
		t.Fatal("the hidden snapshot changed the rest of the detail")
	}

	// A team record is not shown at all, snapshot or not.
	other := f.complete(f.peerA, "team")
	f.grant(other, true, "", "")
	f.keep(other, "Update", p+"team", v1(other), fileSet0, t0, snap("team record"))
	f.wantNotFound("a team record", f.detail(f.u1, other))

	// Version 1 public and superseded: its newer snapshot is shown.
	w1 := f.complete(f.peerA, "public")
	f.grant(w1, true, "", "")
	w2 := f.supersede(w1, "public")
	f.keep(w1, "Update", p+"w-old", v2(w1), fileSet1, t0, snap("version 2"))
	newer := f.keep(w1, "Update", p+"w-new", v1(w1), fileSet0, t0.Add(time.Second), snap("public superseded"))
	f.wantCurrent("a public superseded binding", f.detail(f.u1, w2), w2, newer)
}

// F. current_metadata never widens who sees a record: every refusal is
// the old 404 with nothing else, an authorized withdrawal is only the
// notice, a view-only grant shows the snapshot without the original, a
// revocation after a read hides it and a regrant shows it again, and a
// dropped profile neither erases nor invents a snapshot.
func TestCurrentMetadata_ReadPolicyStillDecides(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.profiles(f.peerA, allProfiles)
	f.profiles(f.peerX, allProfiles)
	snap := mustJSON(t, map[string]any{"title": "kept", "description": nil, "rights": nil, "requiredStatement": nil})
	keepOn := func(r record) kept {
		return f.keep(r, "Update", "f-"+uuid.NewString(), v1(r), fileSet0, t0, snap)
	}
	past := time.Now().Add(-time.Hour)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	type refusal struct {
		caller int64
		mk     func() record
	}
	refusals := map[string]refusal{
		"anonymous": {0, func() record { r := f.complete(f.peerA, "public"); f.grant(r, true, "", ""); return r }},
		"targeted to another user": {f.u2, func() record {
			r := f.complete(f.peerA, "public")
			f.grant(r, true, localBase+"/users/"+f.u1Name, "")
			return r
		}},
		"another peer's grant": {f.u1, func() record { r := f.complete(f.peerA, "public"); f.grantFrom(f.peerX, r); return r }},
		"no grant":             {f.u1, func() record { return f.complete(f.peerA, "public") }},
		"targeted, unbound": {f.u1, func() record {
			r := f.complete(f.peerA, "public")
			f.grant(r, true, localBase+"/users/"+f.u1Name, "")
			exec(`UPDATE federation_inbound_grants SET recipient_user_ref = NULL WHERE object_uri = $1`, r.object)
			return r
		}},
		"pending": {f.u1, func() record {
			r := record{id: uuid.New(), peer: f.peerA, hash: strings.Repeat("cd", 32), body: []byte("x")}
			r.object = f.peerA.url + "/assets/" + uuid.NewString()
			f.insert(r, "public", "pending")
			f.grant(r, true, "", "")
			return r
		}},
		"rejected": {f.u1, func() record {
			r := record{id: uuid.New(), peer: f.peerA, hash: strings.Repeat("ce", 32), body: []byte("x")}
			r.object = f.peerA.url + "/assets/" + uuid.NewString()
			f.insert(r, "public", "rejected")
			f.grant(r, true, "", "")
			return r
		}},
		"superseded": {f.u1, func() record {
			r := f.complete(f.peerA, "public")
			f.grant(r, true, "", "")
			f.supersede(r, "public")
			return r
		}},
		"team": {f.u1, func() record { r := f.complete(f.peerA, "team"); f.grant(r, true, "", ""); return r }},
		"revoked": {f.u1, func() record {
			r := f.complete(f.peerA, "public")
			f.revoke(r, f.grant(r, true, "", ""))
			return r
		}},
		"expired": {f.u1, func() record {
			r := f.complete(f.peerA, "public")
			f.grant(r, true, "", "")
			exec(`UPDATE federation_inbound_grants SET expires_at = $2 WHERE object_uri = $1`, r.object, past)
			return r
		}},
		"peer disabled": {f.u1, func() record {
			p := f.extraPeer()
			r := f.complete(p, "public")
			f.grant(r, true, "", "")
			exec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, p.id)
			return r
		}},
		"peer disconnected": {f.u1, func() record {
			p := f.extraPeer()
			r := f.complete(p, "public")
			f.grant(r, true, "", "")
			exec(`UPDATE federation_peers SET status = 'pending_inbound' WHERE id = $1`, p.id)
			return r
		}},
		"withdrawn, caller could not view": {f.u2, func() record {
			r := f.complete(f.peerA, "public")
			f.grant(r, true, localBase+"/users/"+f.u1Name, "")
			f.withdraw(r, nil)
			return r
		}},
	}
	for name, c := range refusals {
		r := c.mk()
		keepOn(r)
		f.wantNotFound(name, f.detail(c.caller, r))
	}

	// An authorized withdrawal: the notice, and nothing of the record.
	wd := f.complete(f.peerA, "public")
	f.grant(wd, true, "", "")
	keepOn(wd)
	f.withdraw(wd, nil)
	w := f.detail(f.u1, wd)
	if w.status != http.StatusGone {
		t.Fatalf("withdrawn: %d", w.status)
	}
	for _, key := range []string{"current_metadata", "declared", "files", "asset", "activity_uri"} {
		if _, ok := w.body[key]; ok {
			t.Errorf("the withdrawal notice carries %s", key)
		}
	}

	// View only: the snapshot without the original.
	vo := f.complete(f.peerA, "public")
	share := f.grant(vo, false, "", "")
	k := keepOn(vo)
	got := f.detail(f.u1, vo)
	f.wantCurrent("view only", got, vo, k)
	if got.body["original_available"] != false {
		t.Fatal("a view-only grant reports the original available")
	}
	orig, err := f.h.DownloadReceivedAssetOriginal(as(f.u1), openapi.DownloadReceivedAssetOriginalRequestObject{Id: vo.id})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := orig.(openapi.DownloadReceivedAssetOriginal404JSONResponse); !ok {
		t.Fatalf("a view-only grant downloads the original: %T", orig)
	}

	// Revoked after a successful read, then granted again.
	f.revoke(vo, share)
	f.wantNotFound("revoked after a read", f.detail(f.u1, vo))
	f.grant(vo, false, "", "")
	f.wantCurrent("granted again", f.detail(f.u1, vo), vo, k)

	// The profile dropped on renegotiation: the kept snapshot stays as it
	// was, and a record without one gains none.
	f.profiles(f.peerA, noMetadata)
	f.wantCurrent("after the profile was dropped", f.detail(f.u1, vo), vo, k)
	bare := f.complete(f.peerA, "public")
	f.grant(bare, false, "", "")
	f.wantNoCurrent("no snapshot, the profile dropped", f.detail(f.u1, bare))
}

// tracer records every statement a traced pool runs and calls after once,
// when the statement reading the current snapshot completes: a
// synthetic interleaving between the candidate read and its recheck.
type tracer struct {
	mu    sync.Mutex
	sql   []string
	after func()
	fired bool
}

type sqlKey struct{}

func (tr *tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	tr.sql = append(tr.sql, d.SQL)
	tr.mu.Unlock()
	return context.WithValue(ctx, sqlKey{}, d.SQL)
}

func (tr *tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	s, _ := ctx.Value(sqlKey{}).(string)
	tr.mu.Lock()
	fn := tr.after
	if !strings.Contains(s, "FROM federation_received_asset_metadata m") || fn == nil || tr.fired {
		fn = nil
	} else {
		tr.fired = true
	}
	tr.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (tr *tracer) reset(after func()) {
	tr.mu.Lock()
	tr.sql, tr.after, tr.fired = nil, after, false
	tr.mu.Unlock()
}

// traced is a Handler on a pool of its own, traced by tr.
func (f *fx) traced(tr *tracer) *receivedassets.Handler {
	f.t.Helper()
	cfg, err := pgxpool.ParseConfig(fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable pool_max_conns=4",
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
	return &receivedassets.Handler{Pool: pool, Storage: f.svc}
}

// The candidate is rechecked after it is read: a record the caller can no
// longer view, or an asset withdrawn meanwhile, answers the same 404, and
// a failed recheck is an error, never a 200 without the snapshot.
func TestCurrentMetadata_RecheckAfterTheCandidateRead(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.profiles(f.peerA, allProfiles)
	tr := &tracer{}
	h := f.traced(tr)
	snap := mustJSON(t, map[string]any{"title": "kept", "description": nil, "rights": nil, "requiredStatement": nil})
	exec := func(q string, args ...any) {
		if _, err := f.pool.Exec(ctx, q, args...); err != nil {
			t.Error(err)
		}
	}
	setup := func() (record, string) {
		r := f.complete(f.peerA, "public")
		share := f.grant(r, true, "", "")
		k := f.keep(r, "Update", "r-"+uuid.NewString(), v1(r), fileSet0, t0, snap)
		tr.reset(nil)
		f.wantCurrent("before", f.wireOf(h, as(f.u1), r.id), r, k)
		return r, share
	}
	cases := map[string]func(r record, share string){
		"grant revoked": func(r record, share string) { f.revoke(r, share) },
		"tombstone kept, record not yet withdrawn": func(r record, _ string) {
			exec(`INSERT INTO federation_received_tombstones (peer_id, origin_asset_uri, activity_uri, actor_uri, tombstone, deleted, last_version)
				VALUES ($1, $2, $3, $4, $5::jsonb, '2026-11-21T09:00:00Z', $6)`, r.peer.id, r.object, r.peer.url+"/activities/"+uuid.NewString(),
				r.peer.url+"/users/alice", fmt.Sprintf(`{"id":%q,"formerType":"aa:Asset"}`, r.object), v1(r))
		},
		"withdrawn": func(r record, _ string) { f.withdraw(r, nil) },
		"peer disabled": func(r record, _ string) {
			exec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, r.peer.id)
		},
	}
	for name, change := range cases {
		r, share := setup()
		tr.reset(func() { change(r, share) })
		f.wantNotFound(name+", during the read", f.wireOf(h, as(f.u1), r.id))
		if !tr.fired {
			t.Fatalf("%s: the candidate was never read", name)
		}
		exec(`UPDATE federation_peers SET enabled = TRUE, status = 'connected' WHERE id = $1`, f.peerA.id)
	}

	// The recheck fails: an error, not an answer.
	r, _ := setup()
	cctx, cancel := context.WithCancel(as(f.u1))
	defer cancel()
	tr.reset(cancel)
	resp, err := h.GetReceivedAsset(cctx, openapi.GetReceivedAssetRequestObject{Id: r.id})
	if !tr.fired {
		t.Fatal("the candidate was never read")
	}
	if err == nil || !strings.HasPrefix(err.Error(), "receivedassets: ") {
		t.Fatalf("a failed recheck answered %T, error %v", resp, err)
	}
}

// H. Reading the detail writes nothing: every statement the handler runs
// is a SELECT, and the asset's kept rows, its records, files, pins,
// grants and peer are unchanged after present, omitted, refused and
// withdrawn reads.
func TestCurrentMetadata_ReadsWriteNothing(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.profiles(f.peerA, allProfiles)
	tr := &tracer{}
	h := f.traced(tr)
	snap := mustJSON(t, map[string]any{"title": "kept", "description": "d", "rights": nil, "requiredStatement": nil})
	shown := f.complete(f.peerA, "public")
	f.grant(shown, true, "", "")
	f.keep(shown, "Update", "h-"+uuid.NewString(), v1(shown), fileSet0, t0, snap)
	hidden := f.complete(f.peerA, "team")
	f.grant(hidden, true, "", "")
	f.keep(hidden, "Update", "h-"+uuid.NewString(), v1(hidden), fileSet0, t0, snap)
	gone := f.complete(f.peerA, "public")
	f.grant(gone, true, "", "")
	f.keep(gone, "Update", "h-"+uuid.NewString(), v1(gone), fileSet0, t0, snap)
	f.withdraw(gone, nil)
	none := f.complete(f.peerA, "public")
	f.grant(none, true, "", "")

	fingerprint := func() string {
		t.Helper()
		var out string
		if err := f.pool.QueryRow(ctx, `SELECT concat_ws('|',
			(SELECT md5(coalesce(string_agg(row_to_json(m)::text, ',' ORDER BY m.revision_id), '')) FROM federation_received_asset_metadata m
			   JOIN federation_received_asset_revisions v ON v.id = m.revision_id WHERE v.peer_id = $1),
			(SELECT md5(coalesce(string_agg(row_to_json(v)::text, ',' ORDER BY v.id), '')) FROM federation_received_asset_revisions v WHERE v.peer_id = $1),
			(SELECT md5(coalesce(string_agg(row_to_json(a)::text, ',' ORDER BY a.id), '')) FROM federation_received_assets a WHERE a.peer_id = $1),
			(SELECT md5(coalesce(string_agg(row_to_json(x)::text, ',' ORDER BY x.received_asset_id, x.position), '')) FROM federation_received_asset_files x
			   JOIN federation_received_assets a ON a.id = x.received_asset_id WHERE a.peer_id = $1),
			(SELECT md5(coalesce(string_agg(row_to_json(s)::text, ',' ORDER BY s.pin_subject_id, s.object_hash), '')) FROM storage_pins s
			   WHERE s.pin_subject_type = 'federation_received_asset' AND s.pin_subject_id IN (SELECT id::text FROM federation_received_assets WHERE peer_id = $1)),
			(SELECT md5(coalesce(string_agg(row_to_json(g)::text, ',' ORDER BY g.id), '')) FROM federation_inbound_grants g WHERE g.peer_id = $1),
			(SELECT md5(coalesce(string_agg(row_to_json(t)::text, ','), '')) FROM federation_received_tombstones t WHERE t.peer_id = $1),
			(SELECT md5(row_to_json(p)::text) FROM federation_peers p WHERE p.id = $1))`, f.peerA.id).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := fingerprint()
	tr.reset(nil)
	for _, c := range []struct {
		caller int64
		r      record
	}{{f.u1, shown}, {f.u2, shown}, {0, shown}, {f.u1, hidden}, {f.u1, gone}, {f.u1, none}} {
		f.wireOf(h, as(c.caller), c.r.id)
	}
	tr.mu.Lock()
	stmts := slices.Clone(tr.sql)
	tr.mu.Unlock()
	if len(stmts) == 0 {
		t.Fatal("no statement was traced")
	}
	for _, s := range stmts {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "SELECT") {
			t.Errorf("the detail ran a statement that is not a SELECT: %s", s)
		}
	}
	if after := fingerprint(); after != before {
		t.Fatalf("reads changed the kept rows:\n before %s\n after  %s", before, after)
	}
}
