// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetingest_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
)

// #1575 (spec v1 §17.2.8): an origin's Update of its asset's mature and
// aiProvenance flags, and the revisions every accepted aa:asset keeps.

// flagUpdate is the origin's Update of a's flags naming version n with
// files, published at p.
func (f *fx) flagUpdate(a *asset, n int, files []map[string]any, mature bool, ai string, p time.Time) *federation.Envelope {
	f.t.Helper()
	pred := ""
	if n > 1 {
		pred = f.versionURI(a, n-1)
	}
	m := a.memberAt(f.versionURI(a, n), n, pred, files)
	m["mature"] = mature
	if ai != "" {
		m["aiProvenance"] = ai
	}
	e := f.envelope(a, m)
	e.Type, e.Published = federation.ActivityUpdate, p
	return e
}

func (f *fx) applyUpdate(e *federation.Envelope) (assetingest.FlagUpdate, error) {
	return f.ingest.ApplyFlagUpdate(context.Background(), assetingest.Announcement{PeerID: f.peerID, Envelope: e, Signer: f.signer})
}

func (f *fx) revisions(a *asset) int {
	return f.count(`SELECT count(*) FROM federation_received_asset_revisions WHERE peer_id = $1 AND origin_asset_uri = $2`, f.peerID, a.uri)
}

func TestFlagUpdate_IsKeptOnceAndFailsClosed(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	e1 := f.announceV(a, 1, a.files)
	if v1, err := f.receive(e1); err != nil || v1.State != assetingest.StateComplete {
		t.Fatalf("version 1: %+v %v", v1, err)
	}
	if n := f.revisions(a); n != 1 {
		t.Fatalf("%d revisions after version 1, want its own", n)
	}
	later := time.Now().UTC().Add(time.Second)
	u := f.flagUpdate(a, 1, a.files, true, "generated", later)
	got, err := f.applyUpdate(u)
	if err != nil || got.RevisionID == uuid.Nil || got.Duplicate {
		t.Fatalf("the Update: %+v %v", got, err)
	}
	var mature bool
	var ai, typ string
	if err := f.pool.QueryRow(ctx, `SELECT mature, ai_provenance, activity_type FROM federation_received_asset_revisions WHERE id = $1`,
		got.RevisionID).Scan(&mature, &ai, &typ); err != nil || !mature || ai != "generated" || typ != "Update" {
		t.Fatalf("the revision: %v %q %q (%v)", mature, ai, typ, err)
	}
	if again, err := f.applyUpdate(u); err != nil || !again.Duplicate || again.RevisionID != got.RevisionID {
		t.Fatalf("the same Update again: %+v %v", again, err)
	}
	// The record itself is untouched: an Update changes no record.
	if st, _ := f.state(f.current(a)); st != assetingest.StateComplete || f.chain(a) != "1:complete" {
		t.Fatalf("the record after an Update: %s %s", st, f.chain(a))
	}

	for _, tc := range []struct {
		name   string
		env    func() *federation.Envelope
		reason string
	}{
		{"its id with another payload", func() *federation.Envelope {
			e := f.flagUpdate(a, 1, a.files, false, "", later)
			e.ID = u.ID
			return e
		}, assetingest.ReasonActivityConflict},
		{"the id of an announcement", func() *federation.Envelope {
			e := f.flagUpdate(a, 1, a.files, false, "", later)
			e.ID = e1.ID
			return e
		}, assetingest.ReasonActivityConflict},
		{"a version held here named with other bytes", func() *federation.Envelope {
			other, _ := f.originals(1)
			return f.flagUpdate(a, 1, other, false, "", later)
		}, assetingest.ReasonVersionConflict},
		{"a restricted member", func() *federation.Envelope {
			e := f.flagUpdate(a, 1, a.files, false, "", later)
			var m map[string]any
			_ = json.Unmarshal(e.Extra[federation.MemberArchivalAsset], &m)
			m["sensitivity"] = "restricted"
			e.Extra[federation.MemberArchivalAsset], _ = json.Marshal(m)
			return e
		}, casfetch.ReasonSensitivity},
	} {
		n := f.revisions(a)
		_, err := f.applyUpdate(tc.env())
		wantReason(t, err, tc.reason)
		if f.revisions(a) != n {
			t.Fatalf("%s: a refused Update was kept", tc.name)
		}
	}

	t.Run("no grant for the asset: refused, nothing kept", func(t *testing.T) {
		b := f.newAsset(1)
		_, err := f.applyUpdate(f.flagUpdate(b, 1, b.files, true, "", later))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		if f.revisions(b) != 0 {
			t.Fatal("kept without a grant")
		}
	})
	t.Run("before the version it names: kept, and the version still arrives", func(t *testing.T) {
		c := f.newAsset(1)
		f.grant(f.peerID, c.uri, nil)
		if _, err := f.applyUpdate(f.flagUpdate(c, 1, c.files, true, "assisted", later)); err != nil {
			t.Fatalf("an Update before its version: %v", err)
		}
		if v1, err := f.receive(f.announceV(c, 1, c.files)); err != nil || v1.State != assetingest.StateComplete {
			t.Fatalf("version 1 after the Update: %+v %v", v1, err)
		}
		if n := f.revisions(c); n != 2 {
			t.Fatalf("%d revisions, want the Update's and version 1's", n)
		}
	})
	t.Run("after the origin withdrew the asset: refused, nothing kept", func(t *testing.T) {
		if _, err := f.withdraw(f.deleteOf(a, f.tombstone(a, ""))); err != nil {
			t.Fatal(err)
		}
		n := f.revisions(a)
		_, err := f.applyUpdate(f.flagUpdate(a, 1, a.files, false, "", later.Add(time.Second)))
		wantReason(t, err, assetingest.ReasonWithdrawn)
		if f.revisions(a) != n {
			t.Fatal("an Update after the withdrawal was kept")
		}
		if again, err := f.applyUpdate(u); err != nil || !again.Duplicate {
			t.Fatalf("a replay of the Update kept before the withdrawal: %+v %v", again, err)
		}
	})
	t.Run("a revision is immutable", func(t *testing.T) {
		if _, err := f.pool.Exec(ctx, `UPDATE federation_received_asset_revisions SET mature = FALSE WHERE id = $1`, got.RevisionID); err == nil {
			t.Fatal("a revision was rewritten")
		}
	})
}

// An aa:AssetVersion leaves a revision only when it is accepted: a new
// version, or a version held here announced again under a new activity.
func TestFlagUpdate_OnlyAcceptedAnnouncementsLeaveARevision(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	if v3, err := f.receive(f.announceV(a, 3, a.files)); err != nil || v3.State != assetingest.StateComplete {
		t.Fatalf("version 3: %+v %v", v3, err)
	}
	files2, _ := f.originals(1)
	_, err := f.receive(f.announceV(a, 2, files2)) // older than the first held: stale
	wantReason(t, err, assetingest.ReasonVersionStale)
	if n := f.revisions(a); n != 1 {
		t.Fatalf("%d revisions after a stale announcement, want 1", n)
	}
	// Version 3 again, under a new activity (a regrant): a duplicate here,
	// and a revision of its own; the same activity again adds none.
	again := f.announceV(a, 3, a.files)
	for i := 0; i < 2; i++ {
		if res, err := f.receive(again); err != nil || !res.Duplicate {
			t.Fatalf("version 3 again: %+v %v", res, err)
		}
	}
	if n := f.revisions(a); n != 2 {
		t.Fatalf("%d revisions after a regrant's announcement, want 2", n)
	}
}
