// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package federation_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// #1572: the only Delete v1 applies is a Delete of a comment. Its object
// is the comment's Create id on the actor's origin; object_type is
// required and must be "Note"; nothing else is admitted, at receipt or
// after decryption.
func TestValidateMembers_DeleteOfComment(t *testing.T) {
	applied := federation.MemberContext{Applied: func(federation.ActivityType) bool { return true }}
	del := func(object string, extra map[string]string) *federation.Envelope {
		env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityDelete,
			ID: "https://b.example/activities/del1", Actor: "https://b.example/users/bob", Object: object,
			Extra: map[string]json.RawMessage{}}
		for k, v := range extra {
			env.Extra[k] = json.RawMessage(v)
		}
		return env
	}
	create := "https://b.example/activities/create1"
	if err := federation.ValidateMembers(del(create, map[string]string{"object_type": `"Note"`}), applied); err != nil {
		t.Fatalf("Delete of a comment refused: %v", err)
	}
	for name, c := range map[string]struct {
		env  *federation.Envelope
		want error
	}{
		"Delete of a post":                 {del(create, map[string]string{"object_type": `"aa:Post"`}), federation.ErrUnsupportedActivity},
		"Delete of an asset":               {del(create, map[string]string{"object_type": `"aa:Asset"`}), federation.ErrUnsupportedActivity},
		"Delete of a Tombstone":            {del(create, map[string]string{"object_type": `"Tombstone"`}), federation.ErrUnsupportedActivity},
		"Delete of a post, foreign object": {del("https://c.example/x", map[string]string{"object_type": `"aa:Post"`}), federation.ErrUnsupportedActivity},
		"no object_type":                   {del(create, nil), federation.ErrInvalidMember},
		"object on another origin":         {del("https://c.example/activities/create1", map[string]string{"object_type": `"Note"`}), federation.ErrInvalidMember},
		"object on the receiver":           {del("https://a.example/comments/c1", map[string]string{"object_type": `"Note"`}), federation.ErrInvalidMember},
		"object with a query":              {del(create+"?a=1", map[string]string{"object_type": `"Note"`}), federation.ErrInvalidMember},
		"object is its own id":             {del("https://b.example/activities/del1", map[string]string{"object_type": `"Note"`}), federation.ErrInvalidMember},
		"no object":                        {del("", map[string]string{"object_type": `"Note"`}), federation.ErrInvalidMember},
		"an extra member":                  {del(create, map[string]string{"object_type": `"Note"`, "content": `"x"`}), federation.ErrUnknownField},
	} {
		if err := federation.ValidateMembers(c.env, applied); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	sealed := func(object string) *federation.Envelope {
		env := del(object, nil)
		env.Extra = nil
		env.Encryption = &federation.EncryptionBlock{}
		return env
	}
	if err := federation.ValidateMembers(sealed(create), applied); err != nil {
		t.Errorf("sealed Delete of an own Create refused at receipt: %v", err)
	}
	if err := federation.ValidateMembers(sealed("https://c.example/activities/create1"), applied); !errors.Is(err, federation.ErrInvalidMember) {
		t.Errorf("sealed Delete of a foreign object: %v, want invalid_member at receipt", err)
	}
}

// What the sender sends for an author's Delete of a comment is exactly
// what the receiver admits: object = the comment's Create id, object_type
// "Note", nothing else from the local payload.
func TestDeleteWire_RoundTripsThroughValidateMembers(t *testing.T) {
	create := "https://b.example/activities/create1"
	payload := []byte(`{"object_type":"Note","target_post_id":"p","create_activity_uri":"` + create + `"}`)
	if !federation.Federated(federation.ActivityDelete, "comment", payload) {
		t.Fatal("an author's Delete of a comment is not federated")
	}
	for _, other := range []string{`{"object_type":"Note","target_post_id":"p"}`, `{"create_activity_uri":""}`, ``} {
		if federation.Federated(federation.ActivityDelete, "comment", []byte(other)) {
			t.Errorf("Delete with payload %q federated", other)
		}
	}
	obj, err := federation.WireObject(federation.ActivityDelete, "comment", "https://b.example/comments/c1", payload)
	if err != nil || obj != create {
		t.Fatalf("wire object %q, %v; want the Create id, not the local comment URI", obj, err)
	}
	wire, err := federation.WireMembers(federation.ActivityDelete, "comment", payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != 1 || string(wire[federation.MemberObjectType]) != `"Note"` {
		t.Fatalf("wire members %v; want only object_type \"Note\"", wire)
	}
	env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityDelete,
		ID: "https://b.example/activities/del1", Actor: "https://b.example/users/bob", Object: obj, Extra: wire}
	if err := federation.ValidateMembers(env, federation.MemberContext{Applied: func(federation.ActivityType) bool { return true }}); err != nil {
		t.Errorf("the receiver refuses what the sender sends: %v", err)
	}
}
