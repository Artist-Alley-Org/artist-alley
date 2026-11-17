// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package federation_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

// #1571: aa:Share / aa:Unshare under share-lifecycle-1.

const (
	shareSender   = "https://a.example"
	shareReceiver = "https://b.example"
	shareAsset    = shareSender + "/assets/6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11"
)

func shareEnv(typ federation.ActivityType, members map[string]any) *federation.Envelope {
	extra := map[string]json.RawMessage{}
	for k, v := range members {
		if v == nil {
			continue
		}
		b, _ := json.Marshal(v)
		extra[k] = b
	}
	return &federation.Envelope{
		Context: federation.ContextV1, Type: typ, ID: shareSender + "/activities/g2",
		Actor: shareSender + "/users/alice", Object: shareAsset, Extra: extra,
	}
}

func shareMembers(over map[string]any) map[string]any {
	m := map[string]any{"object_kind": "asset", "target_peer": shareReceiver, "scope": "view"}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func unshareMembers(over map[string]any) map[string]any {
	m := map[string]any{"object_kind": "asset", "target_peer": shareReceiver, "aa:revokes": shareSender + "/activities/g1"}
	for k, v := range over {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return m
}

func shareCtx(tokens ...string) federation.MemberContext {
	return federation.MemberContext{
		Negotiated: func(t string) bool {
			for _, x := range tokens {
				if x == t {
					return true
				}
			}
			return false
		},
		Applied:     func(federation.ActivityType) bool { return true },
		LocalOrigin: shareReceiver,
	}
}

func TestValidateMembers_ShareLifecycle(t *testing.T) {
	lc := shareCtx(federation.ProfileShareLifecycle1)
	both := shareCtx(federation.ProfileShareLifecycle1, federation.ProfileOriginalTransfer1)
	cases := []struct {
		name string
		env  *federation.Envelope
		mc   federation.MemberContext
		want error // nil = accepted
	}{
		{"first grant", shareEnv(federation.ActivityAAShare, shareMembers(nil)), lc, nil},
		{"recipient on this instance", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"target_user": shareReceiver + "/users/bob"})), lc, nil},
		{"supersedes on the sender's origin", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"aa:supersedes": shareSender + "/activities/g1"})), lc, nil},
		{"original with the profile", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"aa:originalTransfer": true})), both, nil},
		{"unshare naming its grant", shareEnv(federation.ActivityAAUnshare, unshareMembers(nil)), lc, nil},
		{"revokeshare alias", shareEnv(federation.ActivityAARevokeShare, unshareMembers(nil)), lc, nil},

		{"recipient on another instance", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"target_user": "https://c.example/users/bob"})), lc, federation.ErrInvalidMember},
		{"recipient not a user actor", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"target_user": shareReceiver + "/groups/bob"})), lc, federation.ErrInvalidMember},
		{"target_peer is another instance", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"target_peer": "https://c.example"})), lc, federation.ErrInvalidMember},
		{"edit is not a scope", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"scope": "edit"})), lc, federation.ErrInvalidMember},
		{"forged reference on another origin", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"aa:supersedes": "https://c.example/activities/g1"})), lc, federation.ErrInvalidMember},
		{"self reference", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"aa:supersedes": shareSender + "/activities/g2"})), lc, federation.ErrInvalidMember},
		{"reference not a string", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"aa:supersedes": 7})), lc, federation.ErrInvalidMember},
		{"original without original-transfer-1", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"aa:originalTransfer": true})), lc, federation.ErrProfileNotNegotiated},
		{"original not a boolean", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"aa:originalTransfer": "yes"})), both, federation.ErrInvalidMember},
		{"original on a post", func() *federation.Envelope {
			e := shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"object_kind": "post", "aa:originalTransfer": true}))
			e.Object = shareSender + "/posts/6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11"
			return e
		}(), both, federation.ErrInvalidMember},
		{"unshare without aa:revokes", shareEnv(federation.ActivityAAUnshare, unshareMembers(map[string]any{"aa:revokes": nil})), lc, federation.ErrInvalidMember},
		{"unshare with a scope", shareEnv(federation.ActivityAAUnshare, unshareMembers(map[string]any{"scope": "view"})), lc, federation.ErrUnknownField},
		{"unshare with aa:supersedes", shareEnv(federation.ActivityAAUnshare, unshareMembers(map[string]any{"aa:supersedes": shareSender + "/activities/g0"})), lc, federation.ErrUnknownField},
		{"share with aa:revokes", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"aa:revokes": shareSender + "/activities/g0"})), lc, federation.ErrUnknownField},
		{"no profile: unsupported, members not judged", shareEnv(federation.ActivityAAShare, shareMembers(map[string]any{"aa:supersedes": 7})),
			federation.MemberContext{Applied: func(federation.ActivityType) bool { return false }}, federation.ErrUnsupportedActivity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := federation.ValidateMembers(c.env, c.mc)
			if c.want == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
		})
	}
}

// A peer can grant only what it is the origin of: an object on another
// origin, or a path that is not the kind's, is refused.
func TestValidateMembers_ShareObjectMustBeTheSendersOwn(t *testing.T) {
	lc := shareCtx(federation.ProfileShareLifecycle1)
	for name, object := range map[string]string{
		"third instance's asset": "https://c.example/assets/6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11",
		"the receiver's asset":   shareReceiver + "/assets/6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11",
		"wrong kind path":        shareSender + "/posts/6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11",
		"plain http":             "http://a.example/assets/6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11",
		"no object":              "",
	} {
		e := shareEnv(federation.ActivityAAShare, shareMembers(nil))
		e.Object = object
		if err := federation.ValidateMembers(e, lc); !errors.Is(err, federation.ErrInvalidMember) {
			t.Errorf("%s: err %v, want invalid_member", name, err)
		}
	}
}

func TestWireMembers_ShareLifecyclePerRecipient(t *testing.T) {
	share := []byte(`{"share_id":"s","object_kind":"asset","object_id":"o","object_uri":"u","scope":"view",` +
		`"peer_url":"https://b.example","supersedes_activity_uri":"https://a.example/activities/g1","allow_original":true,"notes":"private"}`)
	unshare := []byte(`{"share_id":"s","object_kind":"asset","object_id":"o","object_uri":"u","peer_url":"https://b.example",` +
		`"previous_activity_uri":"https://a.example/activities/g1"}`)
	has := func(tokens ...string) func(string) bool {
		return func(t string) bool {
			for _, x := range tokens {
				if x == t {
					return true
				}
			}
			return false
		}
	}

	// Older peer (no profile): nothing is sent, neither verb.
	for _, c := range []struct {
		typ     federation.ActivityType
		payload []byte
	}{{federation.ActivityAAShare, share}, {federation.ActivityAAUnshare, unshare}} {
		for _, neg := range []func(string) bool{nil, has(federation.ProfileArchival1, federation.ProfileOriginalTransfer1)} {
			if got, err := federation.WireMembers(c.typ, "asset", c.payload, neg); !errors.Is(err, federation.ErrNotFederated) ||
				!errors.Is(err, federation.ErrProfileNotNegotiated) {
				t.Errorf("%s without %s: members %v, err %v", c.typ, federation.ProfileShareLifecycle1, got, err)
			}
		}
	}

	// share-lifecycle-1 only: references, no original member.
	got, err := federation.WireMembers(federation.ActivityAAShare, "asset", share, has(federation.ProfileShareLifecycle1))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[federation.MemberOriginalTransfer]; ok {
		t.Error("aa:originalTransfer sent to a peer without original-transfer-1")
	}
	if string(got[federation.MemberSupersedes]) != `"https://a.example/activities/g1"` {
		t.Errorf("aa:supersedes = %s", got[federation.MemberSupersedes])
	}
	if _, ok := got["notes"]; ok {
		t.Error("operator notes on the wire")
	}

	// Both profiles: the original grant travels.
	got, err = federation.WireMembers(federation.ActivityAAShare, "asset", share, has(federation.ProfileShareLifecycle1, federation.ProfileOriginalTransfer1))
	if err != nil {
		t.Fatal(err)
	}
	if string(got[federation.MemberOriginalTransfer]) != "true" {
		t.Errorf("aa:originalTransfer = %s, want true", got[federation.MemberOriginalTransfer])
	}

	// Unshare: aa:revokes is required; an empty one is refused, never sent.
	got, err = federation.WireMembers(federation.ActivityAAUnshare, "asset", unshare, has(federation.ProfileShareLifecycle1))
	if err != nil || string(got[federation.MemberRevokes]) != `"https://a.example/activities/g1"` {
		t.Fatalf("unshare members %v, err %v", got, err)
	}
	for _, bad := range [][]byte{
		[]byte(`{"object_kind":"asset","peer_url":"https://b.example","previous_activity_uri":""}`),
		[]byte(`{"object_kind":"asset","peer_url":"https://b.example"}`),
		[]byte(`{"object_kind":"asset","peer_url":"https://b.example","previous_activity_uri":"/activities/g1"}`),
	} {
		if got, err := federation.WireMembers(federation.ActivityAAUnshare, "asset", bad, has(federation.ProfileShareLifecycle1)); !errors.Is(err, federation.ErrNotFederated) {
			t.Errorf("%s: members %v, err %v; want refused", bad, got, err)
		}
	}
}

// What the sender projects, the receiver accepts: one contract on both
// sides of the wire.
func TestWireMembers_ShareRoundTripsThroughValidateMembers(t *testing.T) {
	both := func(t string) bool {
		return t == federation.ProfileShareLifecycle1 || t == federation.ProfileOriginalTransfer1
	}
	for _, c := range []struct {
		typ     federation.ActivityType
		payload string
	}{
		{federation.ActivityAAShare, `{"object_kind":"asset","scope":"remix","peer_url":"https://b.example","target_user_url":"https://b.example/users/bob",` +
			`"expires_at":"2027-01-01T00:00:00Z","supersedes_activity_uri":"https://a.example/activities/g1","allow_original":true}`},
		{federation.ActivityAAUnshare, `{"object_kind":"asset","peer_url":"https://b.example","target_user_url":"https://b.example/users/bob",` +
			`"previous_activity_uri":"https://a.example/activities/g1"}`},
	} {
		wire, err := federation.WireMembers(c.typ, "asset", []byte(c.payload), both)
		if err != nil {
			t.Fatal(err)
		}
		env := &federation.Envelope{Context: federation.ContextV1, Type: c.typ, ID: shareSender + "/activities/g2",
			Actor: shareSender + "/users/alice", Object: shareAsset, Extra: wire}
		if err := federation.ValidateMembers(env, shareCtx(federation.ProfileShareLifecycle1, federation.ProfileOriginalTransfer1)); err != nil {
			t.Errorf("%s: the receiver refuses what the sender sends: %v", c.typ, err)
		}
	}
}

// The token is one string on both sides: the profile the federation
// package gates on is the capability the handshake advertises.
func TestShareLifecycleToken_IsAdvertisedAndMatches(t *testing.T) {
	if string(peer.CapShareLifecycle1) != federation.ProfileShareLifecycle1 {
		t.Fatalf("peer token %q != federation token %q", peer.CapShareLifecycle1, federation.ProfileShareLifecycle1)
	}
	if !peer.KnownCapabilities.Has(peer.CapShareLifecycle1) {
		t.Fatal("share-lifecycle-1 is implemented on both sides and must be advertised")
	}
	if string(peer.CapOriginalTransfer1) != federation.ProfileOriginalTransfer1 {
		t.Fatalf("peer token %q != federation token %q", peer.CapOriginalTransfer1, federation.ProfileOriginalTransfer1)
	}
	if peer.KnownCapabilities.Has(peer.CapOriginalTransfer1) {
		t.Fatal("original-transfer-1 must stay unadvertised until #1574")
	}
}
