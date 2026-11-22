// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// #1575: the version reconciler runs its announcer inside the very
// transaction that seals a version (a draft sealed, or a rendition
// successor sealed and made current), after the asset points at it; an
// unchanged rerun runs nothing; an announcer error rolls the seal back.

// seen is what the announcer saw, inside the sealing transaction.
type seen struct {
	asset   uuid.UUID
	current uuid.UUID
	sealed  bool
}

func TestReconcile_AnnouncesEachSealInsideItsTransaction(t *testing.T) {
	rr := newReconcileRig(t)
	var calls []seen
	var fail error
	rr.r.SetAnnouncer(func(ctx context.Context, tx pgx.Tx, assetID uuid.UUID) error {
		var s seen
		s.asset = assetID
		if err := tx.QueryRow(ctx, `SELECT a.current_version_id, v.sealed_at IS NOT NULL
			FROM assets a JOIN asset_versions v ON v.id = a.current_version_id WHERE a.id = $1`, assetID).Scan(&s.current, &s.sealed); err != nil {
			return err
		}
		calls = append(calls, s)
		return fail
	})

	a, h := rr.readyImage(rr.user("rc-announce"))
	rr.ladder(h, "preview", "screen", "hires")
	// A draft sealed: one call, seeing version 1 sealed and current.
	if res := rr.reconcile(h); res.Sealed != 1 {
		t.Fatalf("result %+v", res)
	}
	v1 := rr.versions(a)[0]
	if len(calls) != 1 || calls[0].asset != a || calls[0].current != v1.ID || !calls[0].sealed {
		t.Fatalf("calls %+v, want one for version 1 sealed", calls)
	}
	// An unchanged rerun: no call.
	if res := rr.reconcile(h); res.Unchanged != 1 || len(calls) != 1 {
		t.Fatalf("rerun %+v, %d calls", res, len(calls))
	}
	// A rendition successor: one call, seeing version 2 sealed and current.
	rr.ladder(h, "hires")
	if res := rr.reconcile(h); res.Successors != 1 {
		t.Fatalf("result %+v", res)
	}
	vs := rr.versions(a)
	if len(calls) != 2 || calls[1].current != vs[1].ID || !calls[1].sealed || vs[1].Origin != "rendition" {
		t.Fatalf("calls %+v, want a second for the rendition successor %v", calls, vs[1])
	}

	t.Run("an announcer error rolls the seal back", func(t *testing.T) {
		c, hc := rr.readyImage(rr.user("rc-announce-fail"))
		rr.ladder(hc, "preview", "screen", "hires")
		fail = errors.New("the announcement could not be recorded")
		if _, err := rr.r.Reconcile(rr.ctx, hc); !errors.Is(err, fail) {
			t.Fatalf("Reconcile: %v, want the announcer's error", err)
		}
		if vs := rr.versions(c); len(vs) != 1 || vs[0].SealedAt != nil {
			t.Fatalf("versions %+v, want version 1 still a draft", vs)
		}
		// The retry seals it and announces it.
		fail = nil
		n := len(calls)
		if res := rr.reconcile(hc); res.Sealed != 1 || len(calls) != n+1 || calls[n].asset != c {
			t.Fatalf("retry %+v, calls %+v", res, calls)
		}
	})
}
