// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 sender gate (spec v1 §18.5, §18.6 in part): the SOURCE refuses to
// send a queued metadata Update once the receiving peer no longer holds a
// required profile, or once the asset was withdrawn; and it sends the
// latest metadata again on its own when the profiles come back. Both
// directions, each scenario on a fresh cluster, serial.
//
// Everything sent is produced by the source's own handlers and
// Server.Run and handled by the destination's own workers; the tests only
// hold or count inbox requests (never altered), move next_attempt_at of
// the held queued rows (DETERMINISTIC RETRY SCHEDULING), and use the
// real admin renegotiation route with a TEST-ONLY shared in-process
// advertisement (not an independent upgrade). Profiles stay unadvertised
// in production. Not covered here: sender sensitivity/embargo, failed
// fetch activation, concurrent source recorders, the live UI.

package twostack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

const senderRights = "https://creativecommons.org/licenses/by/4.0/"

// latestPublished is the latest published time among the source's
// activities of uris.
func (s *senderGate) latestPublished(uris []string) time.Time {
	s.t.Helper()
	var t0 time.Time
	if err := s.src.pool.QueryRow(context.Background(), `SELECT coalesce(max(published_at), 'epoch'::timestamptz) FROM activities
		WHERE activity_uri = ANY ($1)`, uris).Scan(&t0); err != nil {
		s.t.Fatal(err)
	}
	return t0
}

// senderRenegotiation is one scenario: a queued metadata Update is
// refused after a real renegotiation drops token, an edit made while it
// is missing sends nothing, and restoring it sends the latest metadata
// with no further edit. allNull runs it with a four-null capture.
func senderRenegotiation(t *testing.T, g *flowGate, token string, allNull bool) {
	var s *senderGate
	if allNull {
		s = newSenderGate(t, g, "Null title", "", "", "")
		mirror := g.mirrorOnSrc("title")
		s.restoreDefLater(mirror)
		g.srcAPI(http.MethodPatch, "/fields/"+mirror.String(), map[string]any{"read_capability": "system.admin"}, http.StatusOK)
		g.waitCurrent(s.recordID, want(nil, nil, nil, nil), nil)
		g.settle(s.o)
	} else {
		s = newSenderGate(t, g, "Sender title", "Sender description", senderRights, "Studio A")
	}
	gets, heads := g.casCounts()
	hist0, held0, cur0 := s.history(), s.heldRows(), s.currentJSON()
	before := s.metadataActivities()

	// Hold a freshly recorded Update of this asset.
	marker := "Sender hold " + uuid.NewString()
	front := &holdFront{match: func(envs []observed) []string {
		if allNull {
			return allNullMetadata(envs, s.actor, g.assetURI(s.o))
		}
		return markedMetadata(envs, s.actor, g.assetURI(s.o), marker)
	}}
	g.dst.setInboxFront(front.serve)
	t.Cleanup(func() { g.dst.setInboxFront(nil) })
	if allNull {
		g.patch(s.o, map[string]any{"title": "Null title, edited"})
	} else {
		g.putField(s.o, seedStatementDef, marker)
	}
	hs := s.holdAll(front, before)
	var heldURIs []string
	for _, h := range hs {
		u := g.sourceUpdate(h.uri)
		if allNull {
			var c struct {
				Snapshot json.RawMessage `json:"snapshot"`
			}
			if err := json.Unmarshal([]byte(u.capture), &c); err != nil || snapOf(t, c.Snapshot).String() != want(nil, nil, nil, nil).String() {
				t.Fatalf("%s: held %s is not a four-null capture: %s (%v)", g.dir, h.uri, u.capture, err)
			}
		} else if !strings.Contains(u.capture, marker) {
			t.Fatalf("%s: held %s does not carry the marker", g.dir, h.uri)
		}
		heldURIs = append(heldURIs, h.uri)
	}
	if _, _, errs := front.snapshot(); len(errs) != 0 {
		t.Fatalf("%s: the front could not read requests: %v", g.dir, errs)
	}
	if s.history() != hist0 || s.heldRows() != held0 || s.currentJSON() != cur0 {
		t.Fatalf("%s: a held Update reached the destination's state", g.dir)
	}

	// Real renegotiation without token.
	s.renegotiate(without(s.full, token))
	hist1, held1, cur1 := s.history(), s.heldRows(), s.currentJSON()
	t0 := s.latestPublished(heldURIs)
	s.refuseHeld(hs, "metadata Update: the peer does not hold "+token)
	if s.history() != hist1 || s.heldRows() != held1 || s.currentJSON() != cur1 {
		t.Fatalf("%s: the refused retry changed the destination's state", g.dir)
	}
	if n := s.flagsOnlyAfter(t0); n != 0 {
		t.Fatalf("%s: %d flags-only Updates replaced the refused one", g.dir, n)
	}

	// An edit while the profile is missing records and sends nothing.
	ids := s.metadataActivities()
	missing := "While missing " + uuid.NewString()
	latest := want(sp("Sender title"), sp("Sender description"), sp(senderRights), sp(missing))
	if allNull {
		g.patch(s.o, map[string]any{"title": "Null title, while missing"})
		latest = want(nil, nil, nil, nil)
	} else {
		g.putField(s.o, seedStatementDef, missing)
	}
	g.settle(s.o)
	if now := s.metadataActivities(); !slices.Equal(now, ids) {
		t.Fatalf("%s: an edit while %s is missing recorded %v (had %v)", g.dir, token, now, ids)
	}
	if s.history() != hist1 || s.heldRows() != held1 || s.currentJSON() != cur1 {
		t.Fatalf("%s: an edit while %s is missing reached the destination", g.dir, token)
	}

	// Restored: the latest metadata is sent with no further edit.
	s.renegotiate(s.full)
	known := append(append([]string(nil), ids...), heldURIs...)
	after := s.latestPublished(known)
	cur := g.waitCurrent(s.recordID, latest, func(c current) bool {
		return !slices.Contains(known, c.ActivityURI) && c.Published.After(after)
	})
	g.checkUpdate(s.o, cur, 1)
	for _, h := range hs {
		if st, _, _ := g.outboxRow(h.id); st != "failed" {
			t.Fatalf("%s: the refused %s is %q after the restore", g.dir, h.uri, st)
		}
		if s.activityRow(h.uri) != h.row {
			t.Fatalf("%s: the refused %s changed after the restore", g.dir, h.uri)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_asset_revisions WHERE peer_id = $1 AND activity_uri = $2`,
			g.peerAonB, h.uri); n != 0 {
			t.Fatalf("%s: the refused %s entered the history", g.dir, h.uri)
		}
	}
	if g2, h2 := g.casCounts(); g2 != gets || h2 != heads {
		t.Fatalf("%s: the metadata flow fetched (%d GETs, %d HEADs more)", g.dir, g2-gets, h2-heads)
	}
	if s.heldRows() != held0 {
		t.Fatalf("%s: the held original rows changed", g.dir)
	}
	s.servedOriginal("after the restore")
	if localDigest(t, g.dst) != s.baseline {
		t.Fatalf("%s: the destination's own rows changed", g.dir)
	}
	g.dst.setInboxFront(nil)
	g.srcRun.stop()
	g.dstRun.stop()
}

func TestTwoStack_MetadataSender_Renegotiation(t *testing.T) {
	for _, token := range []string{federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1} {
		t.Run("without "+token, func(t *testing.T) {
			flowDirections(t, func(t *testing.T, g *flowGate) { senderRenegotiation(t, g, token, false) })
		})
	}
	t.Run("a four-null capture, without "+federation.ProfileAssetMetadata1, func(t *testing.T) {
		flowDirections(t, func(t *testing.T, g *flowGate) { senderRenegotiation(t, g, federation.ProfileAssetMetadata1, true) })
	})
}

// A real withdrawal overtakes a queued metadata Update: the tombstone is
// delivered, and the Update's retry is refused at the source.
func TestTwoStack_MetadataSender_QueuedWithdrawal(t *testing.T) {
	flowDirections(t, func(t *testing.T, g *flowGate) {
		ctx := context.Background()
		s := newSenderGate(t, g, "Withdrawn title", "Withdrawn description", senderRights, "Studio W")
		record0, hist0 := g.recordRow(s.ann), s.history()
		gets, heads := g.casCounts()
		before := s.metadataActivities()

		marker := "Withdrawal hold " + uuid.NewString()
		front := &holdFront{match: func(envs []observed) []string { return markedMetadata(envs, s.actor, g.assetURI(s.o), marker) }}
		g.dst.setInboxFront(front.serve)
		t.Cleanup(func() { g.dst.setInboxFront(nil) })
		g.putField(s.o, seedStatementDef, marker)
		hs := s.holdAll(front, before)
		var heldURIs []string
		for _, h := range hs {
			if u := g.sourceUpdate(h.uri); !strings.Contains(u.capture, marker) {
				t.Fatalf("%s: held %s does not carry the marker", g.dir, h.uri)
			}
			heldURIs = append(heldURIs, h.uri)
		}

		// The real withdrawal, with a local reason that must not travel.
		sentinel := "operator-sentinel-" + uuid.NewString()
		g.deleteOnA(s.o, sentinel)
		g.recordStateOnB(s.ann, "withdrawn/withdrawn")
		wd, _ := g.withdrawalOnA(s.o)
		g.outboxStatusOnA(wd, "sent")
		for _, p := range []string{"/federation/received-assets/" + s.recordID, "/federation/received-assets/" + s.recordID + "/original"} {
			if st, body := g.getOnB(g.member, p); st != http.StatusGone || bytes.Contains(body, []byte(sentinel)) {
				t.Fatalf("%s: member %s: %d", g.dir, p, st)
			}
			if st, _ := g.getOnB("", p); st != http.StatusNotFound {
				t.Fatalf("%s: anonymous %s: %d", g.dir, p, st)
			}
		}
		if n := g.countOnB(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = $1`,
			s.recordID); n != 0 {
			t.Fatalf("%s: %d record pins after the withdrawal", g.dir, n)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_tombstones WHERE peer_id = $1 AND origin_asset_uri = $2`,
			g.peerAonB, g.assetURI(s.o)); n != 1 {
			t.Fatalf("%s: %d tombstones, want 1", g.dir, n)
		}
		// The signed tombstone as delivered: the source owner's signature,
		// the asset and its announced version and original, no local reason.
		var envJSON, tombText []byte
		if err := g.dst.pool.QueryRow(ctx, `SELECT envelope_json FROM federation_inbox WHERE peer_id = $1 AND activity_type = 'Delete'
			AND envelope_json->>'object' = $2`, g.peerAonB, g.assetURI(s.o)).Scan(&envJSON); err != nil {
			t.Fatalf("%s: the delivered Delete: %v", g.dir, err)
		}
		if err := g.dst.pool.QueryRow(ctx, `SELECT coalesce(string_agg(to_jsonb(t)::text, ' | '), '') FROM federation_received_tombstones t
			WHERE t.peer_id = $1 AND t.origin_asset_uri = $2`, g.peerAonB, g.assetURI(s.o)).Scan(&tombText); err != nil {
			t.Fatal(err)
		}
		var pub string
		if err := g.src.pool.QueryRow(ctx, `SELECT signing_public_key_pem FROM "user" WHERE ref = $1`, g.src.admin).Scan(&pub); err != nil || pub == "" {
			t.Fatalf("the source owner's public key: %v", err)
		}
		env, err := federation.Unmarshal(envJSON)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.Verify([]byte(pub)); err != nil || env.Actor != s.actor || env.Object != g.assetURI(s.o) {
			t.Fatalf("%s: the delivered Delete by %s about %s does not verify: %v", g.dir, env.Actor, env.Object, err)
		}
		tomb := env.Extra[federation.MemberArchivalTombstone]
		if !bytes.Contains(tomb, []byte(g.assetURI(s.o)+"/versions/1")) || !bytes.Contains(tomb, []byte(s.o.hash)) {
			t.Fatalf("%s: the tombstone does not name the delivered version and original: %s", g.dir, tomb)
		}
		if bytes.Contains(envJSON, []byte(sentinel)) || bytes.Contains(tombText, []byte(sentinel)) {
			t.Fatalf("%s: the local delete reason reached the destination", g.dir)
		}
		// Immutable claims and history survive the withdrawal.
		if g.recordRow(s.ann) != record0 || s.history() != hist0 {
			t.Fatalf("%s: the withdrawal rewrote the original claims or the metadata history", g.dir)
		}

		// Release the held retry against the withdrawn state.
		withdrawn, histW := s.heldRows(), s.history()
		t0 := s.latestPublished(heldURIs)
		s.refuseHeld(hs, "metadata Update: the asset is deleted or not local")
		if s.heldRows() != withdrawn || s.history() != histW {
			t.Fatalf("%s: the refused retry changed the withdrawn state", g.dir)
		}
		if n := s.flagsOnlyAfter(t0); n != 0 {
			t.Fatalf("%s: %d flags-only Updates replaced the refused one", g.dir, n)
		}
		for _, p := range []string{"/federation/received-assets/" + s.recordID, "/federation/received-assets/" + s.recordID + "/original"} {
			if st, _ := g.getOnB(g.member, p); st != http.StatusGone {
				t.Fatalf("%s: member %s after the refusal: %d", g.dir, p, st)
			}
			if st, _ := g.getOnB("", p); st != http.StatusNotFound {
				t.Fatalf("%s: anonymous %s after the refusal: %d", g.dir, p, st)
			}
		}
		if g2, h2 := g.casCounts(); g2 != gets || h2 != heads {
			t.Fatalf("%s: fetched after the withdrawal (%d GETs, %d HEADs more)", g.dir, g2-gets, h2-heads)
		}
		if localDigest(t, g.dst) != s.baseline {
			t.Fatalf("%s: the destination's own rows changed", g.dir)
		}
		g.dst.setInboxFront(nil)
		g.srcRun.stop()
		g.dstRun.stop()
	})
}
