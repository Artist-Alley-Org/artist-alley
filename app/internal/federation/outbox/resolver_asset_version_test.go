// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package outbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
)

// #1574: an aa:AssetVersion is addressed by the original-file share it
// rides on: that share's peer only, the share read live, and only to a
// peer with archival-1 and original-transfer-1.

const bothAssetProfiles = `["share-lifecycle-1","archival-1","original-transfer-1"]`

func (f *shareFixture) assetShare(t *testing.T, asset uuid.UUID, mut func(*shares.InsertInput)) *shares.Share {
	t.Helper()
	in := shares.InsertInput{
		GrantorUserRef: f.ref, ObjectKind: federation.ShareObjectKindAsset, ObjectID: asset,
		PeerID: f.peer, Scope: federation.ShareScopeView, GrantedActivityID: f.activity(t, "aa:Share"), AllowOriginal: true,
	}
	if mut != nil {
		mut(&in)
	}
	s, err := f.svc.Insert(context.Background(), in)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM federation_shares WHERE id = $1`, s.ID) })
	return s
}

func assetVersionInput(asset, share uuid.UUID, tier outbox.Sensitivity) outbox.Input {
	return outbox.Input{Verb: string(federation.ActivityAAAssetVersion), TargetKind: "asset", TargetID: asset,
		ShareID: share, Visibility: outbox.VisibilityPrivate, Sensitivity: tier}
}

func TestResolver_AssetVersion_GoesToItsSharesPeerOnly(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	f.negotiate(t, f.peer, bothAssetProfiles)
	f.negotiate(t, f.other, bothAssetProfiles)
	asset := uuid.New()
	s := f.assetShare(t, asset, nil)
	// The other peer's own original share of the same asset does not
	// make it a recipient of this one.
	f.assetShare(t, asset, func(in *shares.InsertInput) { in.PeerID = f.other })

	for _, tier := range []outbox.Sensitivity{outbox.SensitivityPublic, outbox.SensitivityTeam} {
		got, err := f.r.Resolve(ctx, assetVersionInput(asset, s.ID, tier))
		if err != nil {
			t.Fatal(err)
		}
		onlyPeer(t, "aa:AssetVersion at "+string(tier), got, f.peer)
		if got.Recipients[0].TargetUserURL != "" {
			t.Fatalf("addressed to %q, want the peer as a whole", got.Recipients[0].TargetUserURL)
		}
	}
	// Never at a tier that cannot be sent in plaintext: nobody.
	for _, tier := range []outbox.Sensitivity{outbox.SensitivityRestricted, outbox.SensitivityEmbargo, outbox.SensitivityUnresolved} {
		got, err := f.r.Resolve(ctx, assetVersionInput(asset, s.ID, tier))
		if err != nil {
			t.Fatal(err)
		}
		if got.Skipped == outbox.SkippedNone || len(got.Recipients) != 0 {
			t.Fatalf("at %s: %+v, want skipped with nobody", tier, got)
		}
	}
}

func TestResolver_AssetVersion_ProfilesAndGrantReadLive(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	var calls []skipCall
	f.r.SetEmissionSkippedForPeer(func(_ context.Context, p uuid.UUID, reason outbox.SkippedReason, verb string) {
		calls = append(calls, skipCall{p, reason, verb})
	})
	asset := uuid.New()
	s := f.assetShare(t, asset, nil)

	// Without both profiles: skipped and audited for that peer.
	for _, caps := range []string{`[]`, `["share-lifecycle-1","archival-1"]`, `["share-lifecycle-1","original-transfer-1"]`} {
		f.negotiate(t, f.peer, caps)
		got, err := f.r.Resolve(ctx, assetVersionInput(asset, s.ID, outbox.SensitivityPublic))
		if err != nil {
			t.Fatal(err)
		}
		if got.Skipped != outbox.SkippedProfileNotNegotiated || len(got.Recipients) != 0 {
			t.Fatalf("with %s: %+v, want profile_not_negotiated", caps, got)
		}
	}
	if len(calls) != 3 || calls[0] != (skipCall{f.peer, outbox.SkippedProfileNotNegotiated, string(federation.ActivityAAAssetVersion)}) {
		t.Fatalf("per-peer audit calls %+v", calls)
	}
	f.negotiate(t, f.peer, bothAssetProfiles)

	past := time.Now().Add(-time.Hour)
	cases := []struct {
		name  string
		share func() uuid.UUID
		asset uuid.UUID
	}{
		{"another asset's share", func() uuid.UUID { return s.ID }, uuid.New()},
		{"a share without the original", func() uuid.UUID {
			return f.assetShare(t, uuid.New(), func(in *shares.InsertInput) { in.AllowOriginal = false }).ID
		}, uuid.Nil},
		{"a share to one recipient", func() uuid.UUID {
			u := "https://" + randHex(4) + ".example/users/bob"
			return f.assetShare(t, uuid.New(), func(in *shares.InsertInput) { in.TargetUserURL = &u }).ID
		}, uuid.Nil},
		{"an expired share", func() uuid.UUID {
			return f.assetShare(t, uuid.New(), func(in *shares.InsertInput) {
				in.ExpiresAt = &pgtype.Timestamptz{Time: past, Valid: true}
			}).ID
		}, uuid.Nil},
		{"a revoked share", func() uuid.UUID {
			r := f.assetShare(t, uuid.New(), nil)
			if _, err := f.svc.Revoke(ctx, r.ID, f.activity(t, "aa:Unshare")); err != nil {
				t.Fatal(err)
			}
			return r.ID
		}, uuid.Nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := c.share()
			target := c.asset
			if target == uuid.Nil {
				var obj uuid.UUID
				if err := f.pool.QueryRow(ctx, `SELECT object_id FROM federation_shares WHERE id = $1`, id).Scan(&obj); err != nil {
					t.Fatal(err)
				}
				target = obj
			}
			got, err := f.r.Resolve(ctx, assetVersionInput(target, id, outbox.SensitivityPublic))
			if err != nil {
				t.Fatal(err)
			}
			noRecipients(t, c.name, got)
		})
	}

	// A disabled peer, no share id, a share that is gone.
	if _, err := f.pool.Exec(ctx, `UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peer); err != nil {
		t.Fatal(err)
	}
	got, err := f.r.Resolve(ctx, assetVersionInput(asset, s.ID, outbox.SensitivityPublic))
	if err != nil {
		t.Fatal(err)
	}
	noRecipients(t, "a disabled peer", got)
	for _, id := range []uuid.UUID{uuid.Nil, uuid.New()} {
		got, err := f.r.Resolve(ctx, assetVersionInput(asset, id, outbox.SensitivityPublic))
		if err != nil || got.Skipped != outbox.SkippedShareNotFound {
			t.Fatalf("share %v: %+v %v, want share_not_found", id, got, err)
		}
	}
}

// #1575: an asset's withdrawal goes to the one peer its emitter named
// (a peer the asset was announced to), read live, with both profiles. It
// needs no share: a revoked, expired or absent share does not stop it.

func assetWithdrawalInput(asset, peer uuid.UUID) outbox.Input {
	return outbox.Input{Verb: string(federation.ActivityDelete), TargetKind: "asset", TargetID: asset,
		TargetPeerID: peer, Visibility: outbox.VisibilityPrivate, Sensitivity: outbox.SensitivityPublic}
}

func TestResolver_AssetWithdrawal_GoesToTheNamedPeerOnly(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	var calls []skipCall
	f.r.SetEmissionSkippedForPeer(func(_ context.Context, p uuid.UUID, reason outbox.SkippedReason, verb string) {
		calls = append(calls, skipCall{p, reason, verb})
	})
	f.negotiate(t, f.peer, bothAssetProfiles)
	f.negotiate(t, f.other, bothAssetProfiles)
	asset := uuid.New()
	// The other peer holds a live original share of the same asset; it
	// was not named, so it is not a recipient.
	f.assetShare(t, asset, func(in *shares.InsertInput) { in.PeerID = f.other })
	got, err := f.r.Resolve(ctx, assetWithdrawalInput(asset, f.peer))
	if err != nil {
		t.Fatal(err)
	}
	onlyPeer(t, "withdrawal with no share", got, f.peer)

	// A revoked share to the named peer does not stop it.
	r := f.assetShare(t, asset, nil)
	if _, err := f.svc.Revoke(ctx, r.ID, f.activity(t, "aa:Unshare")); err != nil {
		t.Fatal(err)
	}
	got, err = f.r.Resolve(ctx, assetWithdrawalInput(asset, f.peer))
	if err != nil {
		t.Fatal(err)
	}
	onlyPeer(t, "withdrawal after the share was revoked", got, f.peer)

	// Nobody named, or a peer that is gone.
	for _, p := range []uuid.UUID{uuid.Nil, uuid.New()} {
		got, err := f.r.Resolve(ctx, assetWithdrawalInput(asset, p))
		if err != nil {
			t.Fatal(err)
		}
		noRecipients(t, "peer "+p.String(), got)
	}
	// Without both profiles: skipped and audited for that peer.
	for _, caps := range []string{`[]`, `["share-lifecycle-1","archival-1"]`, `["share-lifecycle-1","original-transfer-1"]`} {
		f.negotiate(t, f.peer, caps)
		got, err := f.r.Resolve(ctx, assetWithdrawalInput(asset, f.peer))
		if err != nil {
			t.Fatal(err)
		}
		if got.Skipped != outbox.SkippedProfileNotNegotiated || len(got.Recipients) != 0 {
			t.Fatalf("with %s: %+v, want profile_not_negotiated", caps, got)
		}
	}
	if len(calls) != 3 || calls[0] != (skipCall{f.peer, outbox.SkippedProfileNotNegotiated, string(federation.ActivityDelete)}) {
		t.Fatalf("per-peer audit calls %+v", calls)
	}
	// A disabled peer.
	f.negotiate(t, f.peer, bothAssetProfiles)
	if _, err := f.pool.Exec(ctx, `UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peer); err != nil {
		t.Fatal(err)
	}
	got, err = f.r.Resolve(ctx, assetWithdrawalInput(asset, f.peer))
	if err != nil {
		t.Fatal(err)
	}
	noRecipients(t, "a disabled peer", got)
}
