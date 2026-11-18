// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package federation_test

import (
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// #1611: a Like's wire object is the liked object on the instance that
// holds it; a Like without one is of a local object and is never sent.
func TestWireObject_Like(t *testing.T) {
	local := "https://b.example/posts/p1"
	target := "https://a.example/posts/p1"
	got, err := federation.WireObject(federation.ActivityLike, "post", target,
		[]byte(`{"post_title":"t","target_uri":"`+target+`","target_peer_id":"00000000-0000-0000-0000-000000000001"}`))
	if err != nil || got != target {
		t.Fatalf("remote Like object %q, %v; want %q", got, err, target)
	}
	for _, bad := range []string{
		`{"post_title":"t"}`,
		`{"target_uri":"/posts/p1"}`,
		`{"target_uri":"http://a.example/posts/p1"}`,
		`not json`,
	} {
		if got, err := federation.WireObject(federation.ActivityLike, "post", local, []byte(bad)); !errors.Is(err, federation.ErrNotFederated) {
			t.Errorf("%s: object %q, err %v; want not federated", bad, got, err)
		}
	}
	// A Like still carries no activity members.
	wire, err := federation.WireMembers(federation.ActivityLike, "post",
		[]byte(`{"post_title":"t","target_uri":"`+target+`","target_peer_id":"x"}`), nil)
	if err != nil || len(wire) != 0 {
		t.Fatalf("Like members %v, %v; want none", wire, err)
	}
}
