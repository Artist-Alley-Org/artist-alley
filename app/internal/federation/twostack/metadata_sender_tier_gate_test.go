// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 source-tier sender gate: a queued metadata Update recorded while
// the source asset was public is refused by the source's own sender after
// the asset becomes restricted or embargoed, nothing is recorded or sent
// while it stays so, and returning it to public sends the latest metadata
// with no further edit. Each tier runs in both directions, each on a
// fresh cluster, serially (the TEST-ONLY advertisement of newFlowGate is
// process-shared).
//
// What is real: the upload, version, share and import; every metadata
// edit through the source's own API; recording, delivery, refusal and the
// automatic refresh by the instances' own Server.Run; the destination's
// own inbox and API. What is not: the tier change is the TEST-ONLY
// SOURCE-TIER FIXTURE (metadata_sender_tier_helpers_test.go), because no
// API sets an asset's sensitivity. The only other writes are moving
// next_attempt_at of the named held queued rows (holdAll, refuseHeld).
// This is sender refusal: a source-only tier change does not recall what
// the destination already holds.

package twostack_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/google/uuid"
)

func TestTwoStack_MetadataSender_SourceTier(t *testing.T) {
	for _, tier := range []string{"restricted", "embargo"} {
		t.Run(tier, func(t *testing.T) {
			flowDirections(t, func(t *testing.T, g *flowGate) { senderSourceTier(t, g, tier) })
		})
	}
}

// senderSourceTier is one scenario at tier.
func senderSourceTier(t *testing.T, g *flowGate, tier string) {
	const (
		title0, desc0, stmt0 = "Tier title", "Tier description", "Studio T"
		rightsLatest         = "https://creativecommons.org/publicdomain/zero/1.0/"
	)
	s := newSenderGate(t, g, title0, desc0, senderRights, stmt0)
	s.restoreTierLater()
	gets, heads := g.casCounts()
	hist0, held0, cur0, auth0, peers0 := s.history(), s.heldRows(), s.currentJSON(), s.authorityRows(), s.peerState()
	before := s.metadataActivities()
	if hist0 == "" || len(before) == 0 {
		t.Fatalf("%s: no kept history (%d bytes) or source metadata activity (%d) to start from", g.dir, len(hist0), len(before))
	}
	if ti, d, tr, r, st := s.sourceValues(); ti != title0 || d != desc0 || tr != "public" || r != senderRights || st != stmt0 {
		t.Fatalf("%s: the source starts at %q %q %q %q %q", g.dir, ti, d, tr, r, st)
	}
	s.servedOriginal("at the start")
	s.anonymousHidden("at the start")

	// 1. Hold a genuine, marked Update recorded while the asset is public.
	marker := "Tier hold " + uuid.NewString()
	front := &holdFront{match: func(envs []observed) []string { return markedMetadata(envs, s.actor, g.assetURI(s.o), marker) }}
	g.dst.setInboxFront(front.serve)
	t.Cleanup(func() { g.dst.setInboxFront(nil) })
	g.putField(s.o, seedStatementDef, marker)
	hs := s.holdAll(front, before)
	recorded := want(sp(title0), sp(desc0), sp(senderRights), sp(marker))
	var (
		heldURIs []string
		heldIDs  []uuid.UUID
	)
	for _, h := range hs {
		u := g.sourceUpdate(h.uri)
		var c struct {
			Snapshot json.RawMessage `json:"snapshot"`
		}
		if err := json.Unmarshal([]byte(u.capture), &c); err != nil || snapOf(t, c.Snapshot).String() != recorded.String() || u.actor != s.actor {
			t.Fatalf("%s: held %s by %s captured %s (%v), want %s", g.dir, h.uri, u.actor, u.capture, err, recorded)
		}
		heldURIs = append(heldURIs, h.uri)
		heldIDs = append(heldIDs, h.id)
	}
	if _, posts, errs := front.snapshot(); posts == 0 || len(errs) != 0 {
		t.Fatalf("%s: the front held %d requests (errors %v)", g.dir, posts, errs)
	}
	if s.history() != hist0 || s.heldRows() != held0 || s.currentJSON() != cur0 {
		t.Fatalf("%s: a held Update reached the destination's state", g.dir)
	}

	// 2. TEST-ONLY SOURCE-TIER FIXTURE: the source asset becomes tier.
	// Its policy work drains (the held rows aside) and records nothing.
	s.setSourceTier(tier)
	g.settle(s.o, heldIDs...)
	ids := append(append([]string(nil), before...), heldURIs...)
	sort.Strings(ids)
	if now := s.metadataActivities(); !slices.Equal(now, ids) {
		t.Fatalf("%s: becoming %s recorded %v (had %v)", g.dir, tier, now, ids)
	}
	for _, h := range hs {
		if st, _, _ := g.outboxRow(h.id); st != "queued" {
			t.Fatalf("%s: held %s is %q before its release", g.dir, h.uri, st)
		}
	}
	t0 := s.latestPublished(heldURIs)
	s.refuseHeld(hs, fmt.Sprintf("metadata Update: the asset's tier changed (%s, recorded public)", tier))
	failedRows := s.outboxRows(hs)
	if s.history() != hist0 || s.heldRows() != held0 || s.currentJSON() != cur0 || s.authorityRows() != auth0 {
		t.Fatalf("%s: the refused retry changed the destination's state", g.dir)
	}
	if n := s.flagsOnlyAfter(t0); n != 0 {
		t.Fatalf("%s: %d flags-only Updates replaced the refused one", g.dir, n)
	}
	s.servedOriginal("after the refusal")
	s.anonymousHidden("after the refusal")
	if localDigest(t, g.dst) != s.baseline {
		t.Fatalf("%s: the destination's own rows changed after the refusal", g.dir)
	}

	// 3. While the asset is tier, a complete new description through the
	// source's API records and sends nothing about it.
	af := &assetFront{object: g.assetURI(s.o)}
	g.dst.setInboxFront(af.serve)
	titleL, descL := "Tier title, "+tier+" edit", "Tier description, "+tier+" edit"
	stmtL := "Tier latest " + uuid.NewString()
	g.patch(s.o, map[string]any{"title": titleL, "description": descL})
	g.putField(s.o, seedRightsDef, rightsLatest)
	g.putField(s.o, seedStatementDef, stmtL)
	if ti, d, tr, r, st := s.sourceValues(); ti != titleL || d != descL || tr != tier || r != rightsLatest || st != stmtL {
		t.Fatalf("%s: the source reads %q %q %q %q %q after the edits", g.dir, ti, d, tr, r, st)
	}
	g.settle(s.o)
	if now := s.metadataActivities(); !slices.Equal(now, ids) {
		t.Fatalf("%s: edits while %s recorded %v (had %v)", g.dir, tier, now, ids)
	}
	if seen, errs := af.snapshot(); len(seen) != 0 || len(errs) != 0 {
		t.Fatalf("%s: %d envelopes about the asset were sent while %s (errors %v)", g.dir, len(seen), tier, errs)
	}
	if s.history() != hist0 || s.heldRows() != held0 || s.currentJSON() != cur0 || s.authorityRows() != auth0 || s.peerState() != peers0 {
		t.Fatalf("%s: edits while %s reached the destination or changed the peers", g.dir, tier)
	}
	if n := s.flagsOnlyAfter(t0); n != 0 {
		t.Fatalf("%s: %d flags-only Updates while %s", g.dir, n, tier)
	}
	if s.outboxRows(hs) != failedRows {
		t.Fatalf("%s: the refused rows changed while %s", g.dir, tier)
	}

	// 4. Only the tier goes back to public (TEST-ONLY SOURCE-TIER FIXTURE),
	// with no metadata edit: the latest snapshot is sent automatically.
	known := s.metadataActivities()
	after := s.latestPublished(known)
	s.setSourceTier("public")
	latest := want(sp(titleL), sp(descL), sp(rightsLatest), sp(stmtL))
	cur := g.waitCurrent(s.recordID, latest, func(c current) bool {
		return !slices.Contains(known, c.ActivityURI) && c.Published.After(after)
	})
	g.checkUpdate(s.o, cur, 1)
	g.settle(s.o)
	seen, errs := af.snapshot()
	if len(errs) != 0 {
		t.Fatalf("%s: the front could not read requests: %v", g.dir, errs)
	}
	sent := false
	for _, e := range seen {
		if e.Type != "Update" || len(e.Metadata) == 0 || slices.Contains(known, e.ID) {
			t.Fatalf("%s: after the restore the source sent %s %s", g.dir, e.Type, e.ID)
		}
		if e.ID == cur.ActivityURI {
			sent = true
		}
	}
	if !sent {
		t.Fatalf("%s: the current %s was not seen on the wire (%d envelopes)", g.dir, cur.ActivityURI, len(seen))
	}
	if s.outboxRows(hs) != failedRows {
		t.Fatalf("%s: the refused rows changed after the restore", g.dir)
	}
	for _, h := range hs {
		if s.activityRow(h.uri) != h.row {
			t.Fatalf("%s: the refused %s changed after the restore", g.dir, h.uri)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, h.uri); n != 0 {
			t.Fatalf("%s: the refused %s reached the inbox after the restore", g.dir, h.uri)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_received_asset_revisions WHERE peer_id = $1 AND activity_uri = $2`,
			g.peerAonB, h.uri); n != 0 {
			t.Fatalf("%s: the refused %s entered the history after the restore", g.dir, h.uri)
		}
	}
	if n := s.flagsOnlyAfter(t0); n != 0 {
		t.Fatalf("%s: %d flags-only Updates after the restore", g.dir, n)
	}
	if s.heldRows() != held0 || s.authorityRows() != auth0 || s.peerState() != peers0 {
		t.Fatalf("%s: the restore changed the held original, the authority or the peers", g.dir)
	}
	s.servedOriginal("after the restore")
	s.anonymousHidden("after the restore")
	if g2, h2 := g.casCounts(); g2 != gets || h2 != heads {
		t.Fatalf("%s: the metadata flow fetched (%d GETs, %d HEADs more)", g.dir, g2-gets, h2-heads)
	}
	if localDigest(t, g.dst) != s.baseline {
		t.Fatalf("%s: the destination's own rows changed", g.dir)
	}
	g.dst.setInboxFront(nil)
	g.srcRun.stop()
	g.dstRun.stop()
}
