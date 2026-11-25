// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 gate (spec v1 §18.6, in part): asset metadata flows between two
// production instances, in BOTH directions, through their own handlers
// and workers. A source edit queues durable work in its own transaction
// (migration 00100); the source's Server.Run records a signed metadata
// Update and delivers it; the destination's inbox, dispatcher and asset
// worker keep it; its API shows it as current_metadata, beside the
// original claims, which never change.
//
// Each direction is a fresh cluster: A_to_B has A as the source, B_to_A
// has B. Test-only, labelled where installed:
//
//   - TEST-ONLY ADVERTISEMENT of asset-metadata-1, archival-1 and
//     original-transfer-1, before the real handshake (newFlowGate). None
//     is advertised in production.
//   - DETERMINISTIC RETRY SCHEDULING: only the next_attempt_at of one
//     named, queued source outbox row (flowGate.schedule).
//   - Inbox fronts on the destination that hold (503) or count requests
//     and forward everything else unmodified.
//   - BeforeVersion only: a TEST-ONLY SOURCE CAS FRONT answering 503 to
//     the GET of version 2's original, and TEST-ONLY RECEIVER RETRY
//     SCHEDULING of only that pending record's next_attempt_at
//     (metadata_failed_fetch_helpers_test.go).
//
// Nothing here records, captures, builds, signs, dispatches or ingests an
// activity itself. This is not the complete §18.6 gate (see the evidence
// matrix): no wrong peer/actor/origin/profile, grant/tier, malformed or
// oversized wire, replay/conflict/tie, rejected (fixity-failed) fetch,
// queued-withdrawal or live-UI case is proven here (BeforeVersion proves a
// transient failed fetch and its retry), and the profile stays
// unadvertised.

package twostack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// importOne uploads, seals, describes (title, description and, when
// set, the seeded rights and statement through their routes) and grants
// a new source asset, and waits for the destination's record.
func (g *flowGate) importOne(title, desc, rights, statement string) (original, string, string) {
	g.t.Helper()
	o := g.uploadAsset(pngBytes(g.t))
	g.sealedOnA(o)
	g.patch(o, map[string]any{"title": title, "description": desc})
	if rights != "" {
		g.putField(o, seedRightsDef, rights)
	}
	if statement != "" {
		g.putField(o, seedStatementDef, statement)
	}
	g.grantWith(o, map[string]any{"allow_original": true})
	ann := g.announcementsOnA(o)
	if len(ann) != 1 {
		g.t.Fatalf("%s: the source recorded %d announcements, want 1", g.dir, len(ann))
	}
	g.recordStateOnB(ann[0], "complete")
	return o, ann[0], g.receivedOnB(ann[0])
}

// 1. The lifecycle: import, edits through every write route, clears, a
// batch across two assets, a new version without a regrant, withdrawal.
func TestTwoStack_MetadataFlow_Lifecycle(t *testing.T) {
	flowDirections(t, func(t *testing.T, g *flowGate) {
		ctx := context.Background()
		g.srcRun, g.dstRun = g.start(g.src), g.start(g.dst)
		g.localFixture()
		baseline := localDigest(t, g.dst)
		descMirror := g.mirrorOnSrc("description")

		title0 := `<em>Harbour</em> & "dusk"`
		desc0 := "Line one\n<script>alert(1)</script>"
		rights0 := "https://creativecommons.org/licenses/by/4.0/"
		stmt0 := `<a href="javascript:alert(2)">Studio A</a>`
		o, ann, id := g.importOne(title0, desc0, rights0, stmt0)
		var o2 original
		var id2 string

		var declared0 []byte
		var record0 string
		var cur0, cur1 current
		t.Run("imported with its baseline claims and a current snapshot", func(t *testing.T) {
			st, d := g.detailOnDst(id)
			if st != http.StatusOK || d.Asset.Original.SHA256 != o.hash || d.ActivityURI != ann {
				t.Fatalf("detail %d %+v", st, d)
			}
			if st, body := g.getOnB(g.member, "/federation/received-assets/"+id+"/original"); st != http.StatusOK || !bytes.Equal(body, o.body) {
				t.Fatalf("original %d (%d bytes)", st, len(body))
			}
			for _, p := range []string{"/federation/received-assets/" + id, "/federation/received-assets/" + id + "/original"} {
				if st, _ := g.getOnB("", p); st != http.StatusNotFound {
					t.Fatalf("anonymous %s: %d", p, st)
				}
			}
			if !strings.Contains(string(d.Declared), rights0) || !bytes.Contains(d.Declared, []byte(`"title"`)) {
				t.Fatalf("declared %s", d.Declared)
			}
			declared0, record0 = d.Declared, g.recordRow(ann)
			cur0 = g.waitCurrent(id, want(sp(title0), sp(desc0), sp(rights0), sp(stmt0)), nil)
			// The announcement's initial snapshot, or a grant-time refresh
			// (a real Update): either way bound to exactly version 1.
			g.checkCurrent(o, cur0, 1)
			if localDigest(t, g.dst) != baseline {
				t.Fatal("receiving changed the destination's own schema, values or permissions")
			}
		})

		t.Run("edits through PATCH, the mirrored description and the seeded values", func(t *testing.T) {
			gets, heads := g.casCounts()
			pins := g.countOnB(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, o.hash)
			title1, desc1 := "Harbour, revised", "Revised <b>description</b> via its mirror"
			rights1, stmt1 := "https://rightsstatements.org/vocab/InC/1.0/", "Studio B"
			g.patch(o, map[string]any{"title": title1, "description": "interim"})
			g.putField(o, descMirror, desc1)
			g.putField(o, seedRightsDef, rights1)
			g.putField(o, seedStatementDef, stmt1)
			var srcDesc string
			if err := g.src.pool.QueryRow(ctx, `SELECT description FROM assets WHERE id = $1`, o.asset).Scan(&srcDesc); err != nil || srcDesc != desc1 {
				t.Fatalf("the mirrored PUT left description %q (%v)", srcDesc, err)
			}
			cur1 = g.waitCurrent(id, want(sp(title1), sp(desc1), sp(rights1), sp(stmt1)), func(c current) bool {
				return c.ActivityURI != ann && c.Published.After(cur0.Published)
			})
			g.checkUpdate(o, cur1, 1)
			if cur1.ActivityURI == cur0.ActivityURI || cur1.RevisionID == cur0.RevisionID {
				t.Fatal("the edit reused an earlier identity")
			}
			_, d := g.detailOnDst(id)
			if !sameJSON(d.Declared, declared0) || g.recordRow(ann) != record0 {
				t.Fatalf("the original claims changed: %s", d.Declared)
			}
			if g2, h2 := g.casCounts(); g2 != gets || h2 != heads {
				t.Fatalf("metadata edits fetched the original (%d GETs, %d HEADs more)", g2-gets, h2-heads)
			}
			if n := g.countOnB(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, o.hash); n != pins {
				t.Fatalf("pins %d, were %d", n, pins)
			}
			if localDigest(t, g.dst) != baseline {
				t.Fatal("the edit changed the destination's own rows")
			}
		})

		t.Run("explicit clears are nulls, not missing keys", func(t *testing.T) {
			g.patch(o, map[string]any{"description": ""})
			g.clearField(o, seedRightsDef)
			g.clearField(o, seedStatementDef)
			cur := g.waitCurrent(id, want(sp("Harbour, revised"), nil, nil, nil), func(c current) bool { return c.Published.After(cur1.Published) })
			g.checkUpdate(o, cur, 1)
			for _, k := range []string{`"description":null`, `"rights":null`, `"requiredStatement":null`} {
				if !bytes.Contains(bytes.ReplaceAll(cur.Snapshot, []byte(" "), nil), []byte(k)) {
					t.Fatalf("the clear %s lacks %s", cur.Snapshot, k)
				}
			}
		})

		t.Run("a batch overwrites the seeded rights on two shared assets", func(t *testing.T) {
			var ann2 string
			o2, ann2, id2 = g.importOne("Second harbour", "", "", "")
			_ = ann2
			rightsB := "https://creativecommons.org/publicdomain/zero/1.0/"
			var p struct {
				Token  string `json:"token"`
				Counts struct {
					Eligible    int `json:"eligible"`
					WouldChange int `json:"would_change"`
				} `json:"counts"`
			}
			raw := g.srcAPI(http.MethodPost, "/batch/asset-fields/preview", map[string]any{"mode": "overwrite", "field_id": seedRightsDef,
				"value": map[string]any{"value_text": rightsB}, "selection": []map[string]any{{"kind": "asset", "id": o.asset}, {"kind": "asset", "id": o2.asset}}},
				http.StatusOK)
			if err := json.Unmarshal(raw, &p); err != nil || p.Token == "" || p.Counts.Eligible != 2 || p.Counts.WouldChange != 2 {
				t.Fatalf("preview %s", raw)
			}
			var a struct {
				Outcome struct {
					Changed int `json:"changed"`
				} `json:"outcome_counts"`
			}
			raw = g.srcAPI(http.MethodPost, "/batch/asset-fields/apply", map[string]any{"token": p.Token, "reason": "Gate: relicense both",
				"confirm_count": 2}, http.StatusOK)
			if err := json.Unmarshal(raw, &a); err != nil || a.Outcome.Changed != 2 {
				t.Fatalf("apply %s", raw)
			}
			c1 := g.waitCurrent(id, want(sp("Harbour, revised"), nil, sp(rightsB), nil), nil)
			c2 := g.waitCurrent(id2, want(sp("Second harbour"), nil, sp(rightsB), nil), nil)
			g.checkUpdate(o, c1, 1)
			g.checkUpdate(o2, c2, 1)
		})

		var annV2, idV2 string
		var body2 []byte
		t.Run("a new version is announced without a regrant", func(t *testing.T) {
			body2 = pngBytes(t)
			hash2, receipt := g.uploadOnA(body2)
			g.srcAPI(http.MethodPost, "/assets/"+o.asset.String()+"/versions", map[string]any{"file_hash": hash2, "upload_receipt": receipt,
				"expected_version": 1, "file_extension": "png"}, http.StatusCreated)
			g.waitFor("version 2 announced", 120*time.Second, func() bool { return len(g.announcementsOnA(o)) == 2 })
			annV2 = g.announcementsOnA(o)[1]
			g.recordStateOnB(annV2, "complete")
			idV2 = g.receivedOnB(annV2)
			g.recordStateOnB(ann, "superseded")
			st, d := g.detailOnDst(idV2)
			if st != http.StatusOK || d.Asset.VersionNumber != 2 || d.Asset.Original.SHA256 != hash2 {
				t.Fatalf("version 2's record: %d %+v", st, d)
			}
			if st, b := g.getOnB(g.member, "/federation/received-assets/"+idV2+"/original"); st != http.StatusOK || !bytes.Equal(b, body2) {
				t.Fatalf("version 2's original: %d", st)
			}
			v2 := g.assetURI(o) + "/versions/2"
			cur := g.waitCurrent(idV2, want(sp("Harbour, revised"), nil, sp("https://creativecommons.org/publicdomain/zero/1.0/"), nil),
				func(c current) bool { return c.VersionURI == v2 })
			g.checkCurrent(o, cur, 2) // exact version 2, its file set and provenance
			t.Logf("%s: after version 2 the current snapshot is bound to %s (from %s)", g.dir, cur.VersionURI, cur.ActivityURI)
			if g.recordRow(ann) != record0 {
				t.Fatal("version 1's record changed")
			}
		})

		t.Run("withdrawal ends access; nothing resurrects it", func(t *testing.T) {
			g.deleteOnA(o, "gate withdrawal")
			g.recordStateOnB(annV2, "withdrawn/withdrawn")
			for _, rid := range []string{id, idV2} {
				g.waitFor("record "+rid+" withdrawn", 60*time.Second, func() bool {
					st, _ := g.getOnB(g.member, "/federation/received-assets/"+rid)
					return st == http.StatusGone
				})
				if st, _ := g.getOnB(g.member, "/federation/received-assets/"+rid+"/original"); st != http.StatusGone {
					t.Fatalf("member original of %s: %d", rid, st)
				}
				for _, p := range []string{"/federation/received-assets/" + rid, "/federation/received-assets/" + rid + "/original"} {
					if st, _ := g.getOnB("", p); st != http.StatusNotFound {
						t.Fatalf("anonymous %s: %d", p, st)
					}
				}
				if n := g.countOnB(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = $1`, rid); n != 0 {
					t.Fatalf("%d pins on %s after the withdrawal", n, rid)
				}
			}
			if g.recordRow(ann) != record0 {
				t.Fatal("the withdrawal rewrote version 1's record")
			}
			// The other asset keeps its record and current metadata.
			if st, d := g.detailOnDst(id2); st != http.StatusOK || d.CurrentMetadata == nil {
				t.Fatalf("the other asset: %d", st)
			}
			if localDigest(t, g.dst) != baseline {
				t.Fatal("the destination's own rows changed")
			}
		})
	})
}

// 2. An edit made after the announcement was recorded, while the source
// is stopped, survives a real restart: the announcement keeps what was
// recorded, and the later Update carries the edit.
func TestTwoStack_MetadataFlow_QueuedEditSurvivesRestart(t *testing.T) {
	flowDirections(t, func(t *testing.T, g *flowGate) {
		ctx := context.Background()
		g.srcRun, g.dstRun = g.start(g.src), g.start(g.dst)
		o := g.uploadAsset(pngBytes(t))
		g.sealedOnA(o)
		g.srcRun.stop()
		g.rebuild(g.src) // a fresh server, not running

		g.patch(o, map[string]any{"title": "Recorded title", "description": "Recorded description"})
		g.putField(o, seedRightsDef, "https://creativecommons.org/licenses/by/4.0/")
		g.grantWith(o, map[string]any{"allow_original": true})
		ann := g.announcementsOnA(o)
		if len(ann) != 1 {
			t.Fatalf("the stopped source recorded %d announcements", len(ann))
		}
		annID := g.activityID(ann[0])
		payload := func() string {
			var s string
			if err := g.src.pool.QueryRow(ctx, `SELECT payload::text FROM activities WHERE id = $1`, annID).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		recorded := payload()
		if !strings.Contains(recorded, "Recorded title") {
			t.Fatalf("the announcement did not record the title: %s", recorded)
		}
		g.patch(o, map[string]any{"title": "Edited while stopped", "description": "Edited description"})
		g.putField(o, seedRightsDef, "https://rightsstatements.org/vocab/InC/1.0/")
		var work int
		if err := g.src.pool.QueryRow(ctx, `SELECT count(*) FROM federation_metadata_work WHERE asset_id = $1`, o.asset).Scan(&work); err != nil || work == 0 {
			t.Fatalf("no durable work for the edit (%d, %v)", work, err)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_inbox WHERE peer_id = $1 AND envelope_json->>'object' = $2`, g.peerAonB, g.assetURI(o)); n != 0 {
			t.Fatalf("the destination received %d activities from a stopped source", n)
		}
		if payload() != recorded {
			t.Fatal("the edit changed the recorded announcement")
		}

		g.srcRun = g.start(g.src)
		g.recordStateOnB(ann[0], "complete")
		id := g.receivedOnB(ann[0])
		_, d := g.detailOnDst(id)
		if !strings.Contains(string(d.Declared), "Recorded title") || strings.Contains(string(d.Declared), "Edited while stopped") ||
			!strings.Contains(string(d.Declared), "creativecommons.org/licenses/by/4.0") {
			t.Fatalf("declared %s", d.Declared)
		}
		cur := g.waitCurrent(id, want(sp("Edited while stopped"), sp("Edited description"), sp("https://rightsstatements.org/vocab/InC/1.0/"), nil),
			func(c current) bool { return c.ActivityURI != ann[0] })
		g.checkUpdate(o, cur, 1)
		if payload() != recorded {
			t.Fatal("delivery changed the recorded announcement")
		}
	})
}

// 3. A snapshot captured before a privacy restriction is never delivered
// after it: its retry is refused, and a new Update clears the claim.
func TestTwoStack_MetadataFlow_PrivacyRefusesCapturedSnapshot(t *testing.T) {
	flowDirections(t, func(t *testing.T, g *flowGate) {
		ctx := context.Background()
		g.srcRun, g.dstRun = g.start(g.src), g.start(g.dst)
		rights := "https://creativecommons.org/licenses/by/4.0/"
		o, _, id := g.importOne("Public title", "", rights, "")
		g.waitCurrent(id, want(sp("Public title"), nil, sp(rights), nil), nil)
		g.settle(o)
		baseline := localDigest(t, g.dst)
		actor := g.src.url + "/users/" + g.owner

		// The private marker is the free-text required statement: a rights
		// value must be an allowed rights URI, and the capture sends any
		// other as null (rev769 first focused run: the marker never left).
		marker := "Private statement " + uuid.NewString()
		front := &holdFront{match: func(envs []observed) []string { return markedMetadata(envs, actor, g.assetURI(o), marker) }}
		g.dst.setInboxFront(front.serve)
		t.Cleanup(func() { g.dst.setInboxFront(nil) })

		g.putField(o, seedStatementDef, marker)
		g.waitFor("the marked Update's first attempt", 120*time.Second, func() bool { h, _, _ := front.snapshot(); return len(h) > 0 })
		held, _, _ := front.snapshot()
		marked := held[0]
		u := g.sourceUpdate(marked)
		if !strings.Contains(u.capture, marker) || u.actor != actor {
			t.Fatalf("the held activity is not the source's marked capture: %s by %s", u.capture, u.actor)
		}
		g.waitFor("the attempt recorded on the source", 60*time.Second, func() bool {
			st, n, _ := g.outboxRow(u.id)
			return st == "queued" && n >= 1
		})
		g.schedule(u.id, "now() + interval '1 hour'")

		restoreDef(t, g.src.pool, seedStatementDef)
		g.srcAPI(http.MethodPatch, "/fields/"+seedStatementDef.String(), map[string]any{"read_capability": "system.admin"}, http.StatusOK)
		isHeld := func(uri string) bool { h, _, _ := front.snapshot(); return slices.Contains(h, uri) }
		// The restricted claim is null; its public sibling stays.
		clear := want(sp("Public title"), nil, sp(rights), nil)
		cur := g.waitCurrent(id, clear, func(c current) bool { return !isHeld(c.ActivityURI) && c.Published.After(u.published) })
		g.checkUpdate(o, cur, 1)

		// Every legitimate obligation runs its course (the held rows apart);
		// the settled current snapshot is a clear that no marked activity
		// ever became.
		held, _, _ = front.snapshot()
		var heldIDs []uuid.UUID
		for _, uri := range held {
			heldIDs = append(heldIDs, g.activityID(uri))
		}
		g.settle(o, heldIDs...)
		st, d := g.detailOnDst(id)
		if st != http.StatusOK || d.CurrentMetadata == nil || isHeld(d.CurrentMetadata.ActivityURI) ||
			snapOf(t, d.CurrentMetadata.Snapshot).String() != clear.String() {
			t.Fatalf("the settled current snapshot: %d %+v", st, d.CurrentMetadata)
		}
		settled := *d.CurrentMetadata
		g.checkUpdate(o, settled, 1)
		for _, uri := range held {
			if n := g.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, uri); n != 0 {
				t.Fatalf("a marked activity %s reached the destination's inbox", uri)
			}
		}

		// Release: a new front still holds (and counts) anything marked;
		// ONLY the old row is made due.
		after := &holdFront{match: front.match}
		g.dst.setInboxFront(after.serve)
		g.schedule(u.id, "now()")
		g.waitFor("the old row refused terminally", 120*time.Second, func() bool { st, _, _ := g.outboxRow(u.id); return st == "failed" })
		var lastError string
		if err := g.src.pool.QueryRow(ctx, `SELECT last_error FROM federation_outbox WHERE activity_id = $1 AND peer_id = $2`,
			u.id, g.peerBonA).Scan(&lastError); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(lastError, "aa:metadata: the recorded requiredStatement is no longer shareable with the peer") {
			t.Fatalf("the old row failed for another reason: %q", lastError)
		}
		if _, posts, _ := after.snapshot(); posts != 0 {
			t.Fatalf("%d requests carried the private value after the restriction", posts)
		}
		if again := g.sourceUpdate(marked); again.id != u.id || again.capture != u.capture || !again.published.Equal(u.published) {
			t.Fatal("the old activity's capture or identity changed")
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_asset_revisions WHERE peer_id = $1 AND activity_uri = $2`, g.peerAonB, marked); n != 0 {
			t.Fatal("the destination kept a revision of the refused activity")
		}
		if n := g.countOnSrc(`SELECT count(*) FROM activities WHERE activity_type = 'Update' AND object_local_id = $1
			AND NOT payload ? 'metadata_capture' AND published_at > $2`, o.asset.String(), u.published); n != 0 {
			t.Fatalf("%d flags-only Updates appeared after the refused one", n)
		}
		if _, d := g.detailOnDst(id); d.CurrentMetadata == nil || d.CurrentMetadata.ActivityURI != settled.ActivityURI {
			t.Fatal("the current snapshot moved after the refusal")
		}
		for _, f := range []*holdFront{front, after} {
			if _, _, errs := f.snapshot(); len(errs) != 0 {
				t.Fatalf("the front could not read requests: %v", errs)
			}
		}
		if localDigest(t, g.dst) != baseline {
			t.Fatal("the destination's own rows changed")
		}
	})
}

// 4. A metadata Update naming a version the destination does not hold
// yet is kept pending, and projects once that version arrives. Version
// 2's first fetch fails (the source answers its GET 503: TEST-ONLY
// SOURCE CAS FRONT); the record stays pending, nothing of version 2 is
// published and the pending Update stays hidden. After the front is
// removed, the same record's retry (made due by TEST-ONLY RECEIVER RETRY
// SCHEDULING, metadata_failed_fetch_helpers_test.go) is claimed by the
// running worker, completes, and activates that same pending revision.
func TestTwoStack_MetadataFlow_BeforeVersion(t *testing.T) {
	flowDirections(t, func(t *testing.T, g *flowGate) {
		ctx := context.Background()
		g.srcRun, g.dstRun = g.start(g.src), g.start(g.dst)
		g.localFixture()
		local0 := localDigest(t, g.dst)
		rights := "https://creativecommons.org/licenses/by/4.0/"
		o, ann1, id1 := g.importOne("Version one title", "", rights, "")
		g.waitCurrent(id1, want(sp("Version one title"), nil, sp(rights), nil), nil)
		// Grant and bootstrap refreshes of equal value are legitimate: let
		// them end before the visible snapshot is taken as stable.
		g.settle(o)
		st, d := g.detailOnDst(id1)
		if st != http.StatusOK || d.CurrentMetadata == nil {
			t.Fatalf("version 1's current snapshot: %d", st)
		}
		cur0 := *d.CurrentMetadata
		g.checkCurrent(o, cur0, 1)

		actor, v2 := g.src.url+"/users/"+g.owner, g.assetURI(o)+"/versions/2"
		front := &holdFront{match: func(envs []observed) []string { return versionAnnouncement(envs, actor, g.assetURI(o), v2) }}
		g.dst.setInboxFront(front.serve)
		t.Cleanup(func() { g.dst.setInboxFront(nil) })

		body2 := pngBytes(t)
		hash2, receipt := g.uploadOnA(body2)
		g.srcAPI(http.MethodPost, "/assets/"+o.asset.String()+"/versions", map[string]any{"file_hash": hash2, "upload_receipt": receipt,
			"expected_version": 1, "file_extension": "png"}, http.StatusCreated)
		g.waitFor("version 2's announcement held", 120*time.Second, func() bool { h, _, _ := front.snapshot(); return len(h) > 0 })
		held, _, _ := front.snapshot()
		annV2 := held[0]
		if a := g.announcementsOnA(o); len(a) != 2 || a[1] != annV2 {
			t.Fatalf("held %s, announcements %v", annV2, a)
		}
		annV2ID := g.activityID(annV2)
		g.waitFor("its attempt recorded", 60*time.Second, func() bool { st, n, _ := g.outboxRow(annV2ID); return st == "queued" && n >= 1 })
		g.schedule(annV2ID, "now() + interval '1 hour'")

		gets, heads := g.casCounts()
		g.patch(o, map[string]any{"title": "Named version two"})
		var update string
		var pendingRev uuid.UUID
		var pendingVersion, pendingFileSet string
		var pendingSnap []byte
		g.waitFor("the Update naming version 2 kept pending", 120*time.Second, func() bool {
			if update == "" {
				err := g.src.pool.QueryRow(ctx, `SELECT activity_uri FROM activities WHERE activity_type = 'Update' AND object_local_id = $1
					AND payload->>'metadata_capture' LIKE '%Named version two%' ORDER BY published_at DESC LIMIT 1`, o.asset.String()).Scan(&update)
				if errors.Is(err, pgx.ErrNoRows) {
					return false
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			err := g.dst.pool.QueryRow(ctx, `SELECT r.id, r.version_uri, r.file_set_sha256, m.snapshot
				FROM federation_received_asset_revisions r JOIN federation_received_asset_metadata m ON m.revision_id = r.id
				WHERE r.peer_id = $1 AND r.activity_uri = $2`, g.peerAonB, update).Scan(&pendingRev, &pendingVersion, &pendingFileSet, &pendingSnap)
			if errors.Is(err, pgx.ErrNoRows) {
				return false
			}
			if err != nil {
				t.Fatal(err)
			}
			return true
		})
		if pendingVersion != v2 || snapOf(t, pendingSnap).String() != want(sp("Named version two"), nil, sp(rights), nil).String() {
			t.Fatalf("the pending revision names %s with %s", pendingVersion, pendingSnap)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND version_uri = $2`, g.peerAonB, v2); n != 0 {
			t.Fatal("version 2 reached the destination while held")
		}
		g.settle(o, annV2ID)
		if _, d := g.detailOnDst(id1); d.CurrentMetadata == nil || d.CurrentMetadata.ActivityURI != cur0.ActivityURI {
			t.Fatalf("the pending Update replaced the visible snapshot: %+v", d.CurrentMetadata)
		}
		if g2, h2 := g.casCounts(); g2 != gets || h2 != heads {
			t.Fatalf("the pending Update fetched (%d GETs, %d HEADs)", g2-gets, h2-heads)
		}

		// Baselines before version 2's announcement is released. view
		// only reads (it starts nothing).
		view := &senderGate{flowGate: g, o: o, ann: ann1, recordID: id1}
		held1, stable1, claims1 := view.heldRows(), g.recordStable(ann1), g.recordRow(ann1)
		// The complete kept history and version 1's complete current
		// projection, taken after the settle above (the held announcement
		// aside), both nonempty and naming the current and pending Updates.
		history0, current1 := g.fullHistory(o), view.currentJSON()
		if history0[cur0.ActivityURI] == "" || history0[update] == "" {
			t.Fatalf("the history before the release lacks the current %s or the pending %s", cur0.ActivityURI, update)
		}
		auth0, peers0 := view.authorityRows(), view.peerState()
		pendingRow, updateRow, updateEnv := g.revisionRow(pendingRev), view.activityRow(update), g.inboxEnvelope(update)
		if st, _, _ := g.outboxRow(g.activityID(update)); st != "sent" {
			t.Fatalf("the Update naming version 2 is %q at the source, not sent", st)
		}
		if hash2 == o.hash || g.countOnB(`SELECT count(*) FROM storage_objects WHERE hash = $1`, hash2) != 0 ||
			g.countOnB(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, hash2) != 0 {
			t.Fatalf("version 2's original %s is not new to the destination", hash2)
		}
		if local0 != localDigest(t, g.dst) {
			t.Fatal("the destination's own rows changed before the release")
		}

		// The release, with version 2's GET failing (TEST-ONLY SOURCE CAS
		// FRONT) and a counter for any redelivery of the pending Update.
		casFront := &casFailFront{hash: hash2}
		g.src.setFront(casFront.serve)
		t.Cleanup(func() { g.src.setFront(nil) })
		redelivered := newCountFront([]string{update})
		g.dst.setInboxFront(redelivered.serve)
		g.schedule(annV2ID, "now()")
		fa := g.waitFailedFetch(casFront, annV2, v2, hash2)
		if fa.attempts != 1 {
			t.Fatalf("version 2's first failure is attempt %d", fa.attempts)
		}
		parked := g.parkRetry(fa, g.assetURI(o), v2)
		if _, heads := casFront.seen(); !slices.Contains(heads, http.StatusOK) {
			t.Fatalf("the source's signed HEAD of version 2 did not succeed: %v", heads)
		}
		g.waitFor("version 2's announcement sent", 60*time.Second, func() bool { st, _, _ := g.outboxRow(annV2ID); return st == "sent" })
		v2ID := fa.id.String()

		// After the failed fetch: version 1 stays current and served;
		// nothing of version 2 is published or held; the pending Update is
		// kept as it was and stays hidden.
		if view.heldRows() != held1 || g.recordStable(ann1) != stable1 || g.recordRow(ann1) != claims1 {
			t.Fatal("the failed fetch changed version 1's record, files, pins or storage")
		}
		if _, d := g.detailOnDst(id1); d.CurrentMetadata == nil || d.CurrentMetadata.ActivityURI != cur0.ActivityURI ||
			d.CurrentMetadata.RevisionID != cur0.RevisionID {
			t.Fatalf("version 1's current snapshot changed after the failed fetch: %+v", d.CurrentMetadata)
		}
		view.servedOriginal("after the failed fetch")
		view.anonymousHidden("after the failed fetch")
		for _, p := range []string{"/federation/received-assets/" + v2ID, "/federation/received-assets/" + v2ID + "/original"} {
			if st, _ := g.getOnB(g.member, p); st != http.StatusNotFound {
				t.Fatalf("member %s while version 2 is pending: %d", p, st)
			}
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1`, fa.id); n == 0 {
			t.Fatal("version 2's record names no file")
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1
			AND (state = 'verified' OR verified_at IS NOT NULL)`, fa.id); n != 0 {
			t.Fatalf("%d of version 2's files verified after the failed fetch", n)
		}
		if g.countOnB(`SELECT count(*) FROM storage_objects WHERE hash = $1`, hash2) != 0 ||
			g.countOnB(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, hash2) != 0 {
			t.Fatal("the failed fetch left version 2's bytes or a pin behind")
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_transfer_events WHERE received_asset_id = $1 AND outcome = 'success'`, fa.id); n != 0 {
			t.Fatalf("%d success events for version 2 after the failed fetch", n)
		}
		if g.revisionRow(pendingRev) != pendingRow || view.activityRow(update) != updateRow || g.inboxEnvelope(update) != updateEnv {
			t.Fatal("the failed fetch changed the pending Update's revision, source activity or stored envelope")
		}
		if view.authorityRows() != auth0 || view.peerState() != peers0 || localDigest(t, g.dst) != local0 {
			t.Fatal("the failed fetch changed the shares, grants, peers or the destination's own rows")
		}
		// The whole kept history at the failure: every earlier revision
		// still there with an identical row, and only version 2's own
		// announcement revision added (kept when it was bound, before its
		// fetch); version 1's complete current projection unchanged.
		g.settle(o)
		history1 := g.fullHistory(o)
		if history1[annV2] == "" {
			t.Fatalf("version 2's announcement revision %s was not kept before its fetch", annV2)
		}
		before := map[string]string{}
		for uri, row := range history1 {
			if uri != annV2 {
				before[uri] = row
			}
		}
		g.sameHistory("at the failed fetch", history0, before)
		if view.currentJSON() != current1 {
			t.Fatal("version 1's current projection changed at the failed fetch")
		}

		// Recovery: the real handler serves again; the same record, made
		// due, is claimed and completed by the running worker.
		g.src.setFront(nil)
		getsBefore, headsBefore := g.casCounts()
		g.dueRetry(fa, g.assetURI(o), v2, parked)
		g.recordStateOnB(annV2, "complete")
		g.recordStateOnB(ann1, "superseded")
		idV2 := g.receivedOnB(annV2)
		if idV2 != v2ID {
			t.Fatalf("version 2 completed as %s, not the record that failed (%s)", idV2, v2ID)
		}
		var attempts int32
		if err := g.dst.pool.QueryRow(ctx, `SELECT attempts FROM federation_received_assets WHERE id = $1 AND completed_at IS NOT NULL`,
			fa.id).Scan(&attempts); err != nil || attempts <= fa.attempts {
			t.Fatalf("version 2 completed after %d attempts (failed at %d): %v", attempts, fa.attempts, err)
		}
		if g2, h2 := g.casCounts(); g2 <= getsBefore || h2 <= headsBefore {
			t.Fatalf("the retry made no real GET or HEAD (%d, %d more)", g2-getsBefore, h2-headsBefore)
		}
		if g.countOnB(`SELECT count(*) FROM federation_transfer_events WHERE received_asset_id = $1 AND event_type = 'transfer'
			AND outcome = 'warning' AND reason = 'http_status' AND record_state_after = 'pending' AND attempt = $2`, fa.id, fa.attempts) != 1 {
			t.Fatal("version 2's failed attempt is no longer in the ledger")
		}
		if g.countOnB(`SELECT count(*) FROM federation_transfer_events WHERE received_asset_id = $1 AND event_type = 'fixity-check'
			AND outcome = 'success' AND object_hash = $2 AND attempt = $3 AND record_state_after = 'complete'`, fa.id, hash2, attempts) < 1 ||
			g.countOnB(`SELECT count(*) FROM federation_transfer_events WHERE received_asset_id = $1 AND event_type = 'ingestion'
			AND outcome = 'success' AND attempt = $2 AND record_state_after = 'complete'`, fa.id, attempts) != 1 {
			t.Fatalf("version 2's successful attempt %d is not in the ledger", attempts)
		}
		if g.countOnB(`SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_received_asset'
			AND pin_subject_id = $2`, hash2, v2ID) != 1 ||
			g.countOnB(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, hash2) != 1 {
			t.Fatal("version 2's original is not held by exactly its record's pin")
		}
		cur := g.waitCurrent(idV2, want(sp("Named version two"), nil, sp(rights), nil), func(c current) bool { return c.ActivityURI == update })
		g.checkUpdate(o, cur, 2)
		if cur.RevisionID != pendingRev.String() || cur.FileSetSHA256 != pendingFileSet || cur.VersionURI != pendingVersion {
			t.Fatalf("the projected snapshot %+v is not the pending revision %s (%s, %s)", cur, pendingRev, pendingVersion, pendingFileSet)
		}
		if st, b := g.getOnB(g.member, "/federation/received-assets/"+idV2+"/original"); st != http.StatusOK || !bytes.Equal(b, body2) {
			t.Fatalf("version 2's original: %d", st)
		}
		if _, _, errs := front.snapshot(); len(errs) != 0 {
			t.Fatalf("the front could not read requests: %v", errs)
		}

		// The retry activated the Update the destination already held: no
		// redelivery; the whole kept history exactly as at the failure (no
		// revision added, removed or rewritten); nothing the Update was
		// built from rewritten; version 1's claims, files and pins kept.
		g.settle(o)
		if hits, errs := redelivered.result(); hits != 0 || len(errs) != 0 {
			t.Fatalf("%d requests redelivered the pending Update (errors %v)", hits, errs)
		}
		g.sameHistory("across the retry", history1, g.fullHistory(o))
		if g.revisionRow(pendingRev) != pendingRow || view.activityRow(update) != updateRow || g.inboxEnvelope(update) != updateEnv {
			t.Fatal("the retry changed the pending Update's revision, source activity or stored envelope")
		}
		if g.recordStable(ann1) != stable1 || g.recordRow(ann1) != claims1 {
			t.Fatal("version 1's claims, files or pins changed beyond its supersession")
		}
		if view.authorityRows() != auth0 || view.peerState() != peers0 || localDigest(t, g.dst) != local0 {
			t.Fatal("the retry changed the shares, grants, peers or the destination's own rows")
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND origin_asset_uri = $2 AND state = 'pending'`,
			g.peerAonB, g.assetURI(o)); n != 0 {
			t.Fatalf("%d records of the asset still pending", n)
		}
		g.dst.setInboxFront(nil)
		t.Logf("%s: pending Update %s (revision %s) projected over %s after record %s failed at attempt %d and completed at %d",
			g.dir, update, pendingRev, cur.VersionURI, v2ID, fa.attempts, attempts)
	})
}

// Selector regressions (UNIT-ONLY: synthetic JSON inputs to the fronts'
// selectors; no envelope here is signed, sent or evidence of the flow).
func TestMetadataFlowSelectors(t *testing.T) {
	const actor, object = "https://src.example/users/owner", "https://src.example/assets/a1"
	const marker = "Private statement 1234"
	update := func(id, actor, object, statement, title string) string {
		rs := "null"
		if statement != "" {
			rs = `{"label":{"en":["Attribution"]},"value":{"none":["` + statement + `"]}}`
		}
		return `{"id":"` + id + `","type":"Update","actor":"` + actor + `","object":"` + object +
			`","aa:metadata":{"title":"` + title + `","description":null,"rights":null,"requiredStatement":` + rs + `}}`
	}
	batch := func(envs ...string) []byte { return []byte(`{"envelopes":[` + strings.Join(envs, ",") + `]}`) }
	marked := func(body []byte) []string {
		envs, err := observeInbox(body)
		if err != nil {
			t.Fatal(err)
		}
		return markedMetadata(envs, actor, object, marker)
	}
	cases := []struct {
		name string
		body []byte
		want []string
	}{
		{"unrelated first, marked second", batch(`{"id":"like-1","type":"Like","actor":"`+actor+`","object":"`+object+`"}`,
			update("m-1", actor, object, marker, "t")), []string{"m-1"}},
		{"several marked among benign companions", batch(update("m-1", actor, object, marker, "t"), update("b-1", actor, object, "Studio", "t"),
			update("m-2", actor, object, marker, "t")), []string{"m-1", "m-2"}},
		{"no matching metadata", batch(update("b-1", actor, object, "Studio", "t"),
			`{"id":"v-1","type":"aa:AssetVersion","actor":"`+actor+`","object":"`+object+`","aa:asset":{"version":{"id":"`+object+`/versions/1"}}}`), nil},
		{"the marker only in non-target envelopes", batch(
			`{"id":"n-1","type":"Create","actor":"`+actor+`","object":"`+object+`","content":"`+marker+`"}`,
			update("o-1", "https://other.example/users/x", object, marker, "t"),
			update("o-2", actor, "https://src.example/assets/other", marker, "t"),
			update("o-3", actor, object, "", marker)), nil},
		{"a single marked envelope", []byte(update("m-1", actor, object, marker, "t")), []string{"m-1"}},
	}
	for _, c := range cases {
		if got := marked(c.body); !slices.Equal(got, c.want) {
			t.Errorf("%s: selected %v, want %v", c.name, got, c.want)
		}
	}

	// The version front: only this asset's exact version 2 announcement.
	ann := func(id, object, version string) string {
		return `{"id":"` + id + `","type":"aa:AssetVersion","actor":"` + actor + `","object":"` + object +
			`","aa:asset":{"version":{"id":"` + version + `"}}}`
	}
	envs, err := observeInbox(batch(ann("v1", object, object+"/versions/1"), ann("x2", "https://src.example/assets/other",
		"https://src.example/assets/other/versions/2"), update("u", actor, object, "", "t"), ann("v2", object, object+"/versions/2")))
	if err != nil {
		t.Fatal(err)
	}
	if got := versionAnnouncement(envs, actor, object, object+"/versions/2"); !slices.Equal(got, []string{"v2"}) {
		t.Errorf("version selector: %v", got)
	}

	// The front holds the whole original request when anything matches,
	// and forwards the exact bytes otherwise.
	front := &holdFront{match: func(envs []observed) []string { return markedMetadata(envs, actor, object, marker) }}
	var forwarded []byte
	var length int64
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, _ = io.ReadAll(r.Body)
		length = r.ContentLength
		w.WriteHeader(http.StatusAccepted)
	})
	for _, c := range []struct {
		body []byte
		code int
	}{{cases[0].body, http.StatusServiceUnavailable}, {cases[2].body, http.StatusAccepted}} {
		forwarded, length = nil, 0
		rec := httptest.NewRecorder()
		front.serve(rec, httptest.NewRequest(http.MethodPost, "/federation/inbox", bytes.NewReader(c.body)), app)
		if rec.Code != c.code {
			t.Fatalf("front answered %d, want %d", rec.Code, c.code)
		}
		if c.code == http.StatusAccepted && (!bytes.Equal(forwarded, c.body) || length != int64(len(c.body))) {
			t.Fatalf("forwarded %d bytes (length %d), want the exact %d", len(forwarded), length, len(c.body))
		}
		if c.code == http.StatusServiceUnavailable && forwarded != nil {
			t.Fatal("a held request reached the application")
		}
	}
	if held, posts, errs := front.snapshot(); !slices.Equal(held, []string{"m-1"}) || posts != 1 || len(errs) != 0 {
		t.Fatalf("the front recorded %v, %d posts, errors %v", held, posts, errs)
	}
}
