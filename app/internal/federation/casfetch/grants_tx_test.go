// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package casfetch_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
)

// #1571: the inbound grant is asked again INSIDE the recording
// transaction (AllowsTx), after the last check outside it. A revoke that
// lands after the final check is still refused at the commit, and
// nothing is recorded; the same for the reuse pin.
func TestFetch_InboundGrantRecheckedInTheCommit(t *testing.T) {
	f := newFx(t)
	casfetch.SetAfterFinalCheck(f.client, func() { f.grants.set(false) })
	_, err := f.client.Fetch(context.Background(), f.req())
	wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
	var e *casfetch.Error
	if !errors.As(err, &e) || e.OriginReason != casfetch.ReasonNoInboundGrant {
		t.Fatalf("cause %+v, want no_inbound_grant from the in-commit check", e)
	}
	if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 0 || f.ingestPins() != 0 {
		t.Fatalf("recorded after an in-commit refusal: objects=%d pins=%d", n, f.ingestPins())
	}
}

func TestFetch_InboundGrantRecheckedInTheReusePin(t *testing.T) {
	f := newFx(t)
	f.holdLocally()
	casfetch.SetBeforeReusePin(f.client, func() { f.grants.set(false) })
	_, err := f.client.Fetch(context.Background(), f.req())
	wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
	if f.ingestPins() != 0 {
		t.Fatal("pinned after the grant ended")
	}
}
