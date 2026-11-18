// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package federation_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// #1572: the only Update v1 applies is an edit of a comment: object_type
// "Note", the comment's Create id on the actor's origin, and a content
// valid as a comment's; no inReplyTo.
func TestValidateMembers_UpdateOfComment(t *testing.T) {
	applied := federation.MemberContext{Applied: func(federation.ActivityType) bool { return true }}
	upd := func(object string, extra map[string]string) *federation.Envelope {
		env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityUpdate,
			ID: "https://b.example/activities/upd1", Actor: "https://b.example/users/bob", Object: object,
			Extra: map[string]json.RawMessage{}}
		for k, v := range extra {
			env.Extra[k] = json.RawMessage(v)
		}
		return env
	}
	create := "https://b.example/activities/create1"
	if err := federation.ValidateMembers(upd(create, map[string]string{"object_type": `"Note"`, "content": `"new"`}), applied); err != nil {
		t.Fatalf("Update of a comment refused: %v", err)
	}
	for name, c := range map[string]struct {
		env  *federation.Envelope
		want error
	}{
		"Update of a post":         {upd(create, map[string]string{"object_type": `"aa:Post"`, "content": `"x"`}), federation.ErrUnsupportedActivity},
		"Update of a profile":      {upd(create, map[string]string{"object_type": `"Person"`}), federation.ErrUnsupportedActivity},
		"no object_type":           {upd(create, map[string]string{"content": `"x"`}), federation.ErrInvalidMember},
		"no content":               {upd(create, map[string]string{"object_type": `"Note"`}), federation.ErrInvalidMember},
		"blank content":            {upd(create, map[string]string{"object_type": `"Note"`, "content": `"  "`}), federation.ErrInvalidMember},
		"content too long":         {upd(create, map[string]string{"object_type": `"Note"`, "content": `"` + strings.Repeat("a", 10001) + `"`}), federation.ErrInvalidMember},
		"inReplyTo":                {upd(create, map[string]string{"object_type": `"Note"`, "content": `"x"`, "inReplyTo": `"https://b.example/activities/p"`}), federation.ErrUnknownField},
		"object on another origin": {upd("https://c.example/activities/create1", map[string]string{"object_type": `"Note"`, "content": `"x"`}), federation.ErrInvalidMember},
		"object is its own id":     {upd("https://b.example/activities/upd1", map[string]string{"object_type": `"Note"`, "content": `"x"`}), federation.ErrInvalidMember},
	} {
		if err := federation.ValidateMembers(c.env, applied); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

// What the sender sends for an author's edit is what the receiver admits:
// object = the comment's Create id, object_type "Note" and content only.
func TestUpdateWire_RoundTripsThroughValidateMembers(t *testing.T) {
	create := "https://b.example/activities/create1"
	payload := []byte(`{"object_type":"Note","content":"new text","target_post_id":"p","create_activity_uri":"` + create + `"}`)
	if !federation.Federated(federation.ActivityUpdate, "comment", payload) {
		t.Fatal("an author's edit of a comment is not federated")
	}
	for _, kind := range []string{"post", "user", "collection"} {
		if federation.Federated(federation.ActivityUpdate, kind, payload) {
			t.Errorf("an Update of a %s federated", kind)
		}
	}
	obj, err := federation.WireObject(federation.ActivityUpdate, "comment", "https://b.example/comments/c1", payload)
	if err != nil || obj != create {
		t.Fatalf("wire object %q, %v; want the Create id", obj, err)
	}
	wire, err := federation.WireMembers(federation.ActivityUpdate, "comment", payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != 2 || string(wire[federation.MemberObjectType]) != `"Note"` || string(wire[federation.MemberContent]) != `"new text"` {
		t.Fatalf("wire members %v; want object_type and content only", wire)
	}
	if _, err := federation.WireMembers(federation.ActivityUpdate, "comment",
		[]byte(`{"object_type":"Note","content":"  ","create_activity_uri":"`+create+`"}`), nil); !errors.Is(err, federation.ErrNotFederated) {
		t.Errorf("a blank edit is sent: %v", err)
	}
	env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityUpdate,
		ID: "https://b.example/activities/upd1", Actor: "https://b.example/users/bob", Object: obj, Extra: wire}
	if err := federation.ValidateMembers(env, federation.MemberContext{Applied: func(federation.ActivityType) bool { return true }}); err != nil {
		t.Errorf("the receiver refuses what the sender sends: %v", err)
	}
}
