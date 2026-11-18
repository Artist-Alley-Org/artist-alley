// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package federation_test

import (
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// #1604: a comment Create's wire object is its target, not the
// comment's local URI.
func TestWireObject(t *testing.T) {
	commentURI := "https://b.example/comments/c1"
	target := "https://a.example/posts/p1"
	got, err := federation.WireObject(federation.ActivityCreate, "comment", commentURI,
		[]byte(`{"object_type":"Note","content":"c","target_uri":"`+target+`"}`))
	if err != nil || got != target {
		t.Fatalf("comment Create object %q, %v; want the target %q", got, err, target)
	}
	for _, bad := range []string{
		`{"object_type":"Note","content":"c"}`,
		`{"object_type":"Note","content":"c","target_uri":"/posts/p1"}`,
		`{"object_type":"Note","content":"c","target_uri":"http://a.example/posts/p1"}`,
		`not json`,
	} {
		if got, err := federation.WireObject(federation.ActivityCreate, "comment", commentURI, []byte(bad)); !errors.Is(err, federation.ErrNotFederated) {
			t.Errorf("%s: object %q, err %v; want refused", bad, got, err)
		}
	}
	// Every other activity keeps its own object (a Like takes its
	// target's, #1611: TestWireObject_Like).
	for _, c := range []struct {
		typ  federation.ActivityType
		kind string
	}{{federation.ActivityAAShare, "post"}, {federation.ActivityCreate, "post"}} {
		if got, err := federation.WireObject(c.typ, c.kind, "https://a.example/posts/p1", []byte(`{"target_uri":"https://x.example/y"}`)); err != nil || got != "https://a.example/posts/p1" {
			t.Errorf("%s/%s: %q, %v", c.typ, c.kind, got, err)
		}
	}
}

// What the sender projects for a top-level comment and a reply, the
// receiver's member check accepts: object = target, inReplyTo only on
// the reply.
func TestCommentWire_RoundTripsThroughValidateMembers(t *testing.T) {
	target := "https://a.example/posts/p1"
	for name, payload := range map[string]string{
		"top-level": `{"object_type":"Note","inReplyTo":"` + target + `","content":"c","target_uri":"` + target + `"}`,
		"reply":     `{"object_type":"Note","content":"c","target_uri":"` + target + `","in_reply_to_uri":"https://b.example/activities/top"}`,
	} {
		wire, err := federation.WireMembers(federation.ActivityCreate, "comment", []byte(payload), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, hasReply := wire[federation.MemberInReplyTo]
		if hasReply != (name == "reply") {
			t.Errorf("%s: inReplyTo present = %v", name, hasReply)
		}
		obj, err := federation.WireObject(federation.ActivityCreate, "comment", "https://b.example/comments/c1", []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityCreate,
			ID: "https://b.example/activities/" + name, Actor: "https://b.example/users/bob", Object: obj, Extra: wire}
		if err := federation.ValidateMembers(env, federation.MemberContext{Applied: func(federation.ActivityType) bool { return true }}); err != nil {
			t.Errorf("%s: the receiver refuses what the sender sends: %v", name, err)
		}
		if env.Object != target {
			t.Errorf("%s: wire object %q", name, env.Object)
		}
	}
}
