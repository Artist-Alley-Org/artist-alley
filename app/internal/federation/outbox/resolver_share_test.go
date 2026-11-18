// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package outbox_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
)

// #1571: an aa:Share / aa:Unshare is addressed by its share row: that
// share's peer and recipient only, live state read now, and only when
// the peer negotiated share-lifecycle-1.

type skipCall struct {
	peer   uuid.UUID
	reason outbox.SkippedReason
	verb   string
}

func (f *shareFixture) negotiate(t *testing.T, peerID uuid.UUID, caps string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, peerID, caps); err != nil {
		t.Fatal(err)
	}
}

func (f *shareFixture) shareInput(verb federation.ActivityType, shareID uuid.UUID, vis outbox.Visibility) outbox.Input {
	return outbox.Input{Verb: string(verb), TargetKind: "post", TargetID: f.post, ShareID: shareID,
		AuthorRef: f.ref, Visibility: vis, Sensitivity: outbox.SensitivityPublic}
}

func TestResolver_ShareActivity_OlderPeerIsSkippedAndAudited(t *testing.T) {
	f := newShareFixture(t)
	var calls []skipCall
	f.r.SetEmissionSkippedForPeer(func(_ context.Context, p uuid.UUID, reason outbox.SkippedReason, verb string) {
		calls = append(calls, skipCall{p, reason, verb})
	})
	s := f.grant(t, nil)
	// The fixture peer negotiated nothing: a pre-#1571 peer.
	for _, verb := range []federation.ActivityType{federation.ActivityAAShare, federation.ActivityAAUnshare} {
		got, err := f.r.Resolve(context.Background(), f.shareInput(verb, s.ID, outbox.VisibilityPublic))
		if err != nil {
			t.Fatal(err)
		}
		if got.Skipped != outbox.SkippedProfileNotNegotiated || len(got.Recipients) != 0 {
			t.Fatalf("%s to an older peer: %+v, want profile_not_negotiated and nobody", verb, got)
		}
	}
	if len(calls) != 2 || calls[0] != (skipCall{f.peer, outbox.SkippedProfileNotNegotiated, string(federation.ActivityAAShare)}) ||
		calls[1].reason != outbox.SkippedProfileNotNegotiated {
		t.Fatalf("per-peer audit calls %+v", calls)
	}
	// Other profiles do not count.
	f.negotiate(t, f.peer, `["archival-1","nacl-box"]`)
	if got, _ := f.r.Resolve(context.Background(), f.shareInput(federation.ActivityAAShare, s.ID, outbox.VisibilityPublic)); got.Skipped != outbox.SkippedProfileNotNegotiated {
		t.Fatalf("without share-lifecycle-1: %+v", got)
	}
}

func TestResolver_ShareActivity_GoesOnlyToItsSharesPeer(t *testing.T) {
	f := newShareFixture(t)
	f.negotiate(t, f.peer, `["share-lifecycle-1"]`)
	f.negotiate(t, f.other, `["share-lifecycle-1"]`)
	s := f.grant(t, nil)
	// The other peer holds its own live share of the same object; it must
	// not receive this share's grant or revoke.
	other, err := f.svc.Insert(context.Background(), shares.InsertInput{
		GrantorUserRef: f.ref, ObjectKind: federation.ShareObjectKindPost, ObjectID: f.post,
		PeerID: f.other, Scope: federation.ShareScopeRemix, GrantedActivityID: f.activity(t, "aa:Share"),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.r.Resolve(context.Background(), f.shareInput(federation.ActivityAAShare, s.ID, outbox.VisibilityPublic))
	if err != nil {
		t.Fatal(err)
	}
	onlyPeer(t, "aa:Share", got, f.peer)
	if got.Recipients[0].Scope != string(federation.ShareScopeView) {
		t.Errorf("scope %q, want the share's own", got.Recipients[0].Scope)
	}
	got, _ = f.r.Resolve(context.Background(), f.shareInput(federation.ActivityAAShare, other.ID, outbox.VisibilityPublic))
	onlyPeer(t, "the other share", got, f.other)

	// Revoked: the aa:Unshare still reaches the share's peer (and only
	// it), whatever the object's visibility now is.
	if _, err := f.svc.Revoke(context.Background(), s.ID, f.activity(t, "aa:Unshare")); err != nil {
		t.Fatal(err)
	}
	for _, vis := range []outbox.Visibility{outbox.VisibilityPublic, outbox.VisibilityPrivate, outbox.VisibilityOrgOnly} {
		got, err := f.r.Resolve(context.Background(), f.shareInput(federation.ActivityAAUnshare, s.ID, vis))
		if err != nil {
			t.Fatal(err)
		}
		onlyPeer(t, "aa:Unshare at "+string(vis), got, f.peer)
	}
}

func TestResolver_ShareActivity_PeerStateAndMissingShare(t *testing.T) {
	f := newShareFixture(t)
	f.negotiate(t, f.peer, `["share-lifecycle-1"]`)
	s := f.grant(t, nil)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peer); err != nil {
		t.Fatal(err)
	}
	got, err := f.r.Resolve(ctx, f.shareInput(federation.ActivityAAShare, s.ID, outbox.VisibilityPublic))
	if err != nil {
		t.Fatal(err)
	}
	noRecipients(t, "disabled peer", got)
	// No share id, or a share that is gone: skipped, never a resolver
	// error (which would stall the dispatch batch).
	for _, id := range []uuid.UUID{{}, uuid.New()} {
		got, err := f.r.Resolve(ctx, f.shareInput(federation.ActivityAAShare, id, outbox.VisibilityPublic))
		if err != nil || got.Skipped != outbox.SkippedShareNotFound {
			t.Fatalf("share %s: %+v %v", id, got, err)
		}
	}
	// A share of an object kind without a visibility tier (an asset
	// resolves to private) is still addressed by its row.
	if _, err := f.pool.Exec(ctx, `UPDATE federation_peers SET enabled = TRUE WHERE id = $1`, f.peer); err != nil {
		t.Fatal(err)
	}
	in := f.shareInput(federation.ActivityAAShare, s.ID, outbox.VisibilityPrivate)
	in.TargetKind = "asset"
	got, err = f.r.Resolve(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	onlyPeer(t, "share at private visibility", got, f.peer)
}
