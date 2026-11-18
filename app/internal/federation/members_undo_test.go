// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package federation_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// #1572: the only Undo v1 applies is an Undo of a Like. Its object is
// the actor's own Like id on the actor's origin; object_type is
// required and must be "Like"; nothing else is admitted, at receipt or
// after decryption.
func TestValidateMembers_UndoOfLike(t *testing.T) {
	applied := federation.MemberContext{Applied: func(federation.ActivityType) bool { return true }}
	const actor = "https://b.example/users/bob"
	undo := func(object string, extra map[string]string) *federation.Envelope {
		env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityUndo,
			ID: "https://b.example/activities/undo1", Actor: actor, Object: object, Extra: map[string]json.RawMessage{}}
		for k, v := range extra {
			env.Extra[k] = json.RawMessage(v)
		}
		return env
	}
	like := "https://b.example/activities/like1"

	if err := federation.ValidateMembers(undo(like, map[string]string{"object_type": `"Like"`}), applied); err != nil {
		t.Fatalf("Undo of a Like refused: %v", err)
	}
	for name, c := range map[string]struct {
		env  *federation.Envelope
		want error
	}{
		"Undo of a Follow": {undo(like, map[string]string{"object_type": `"Follow"`}), federation.ErrUnsupportedActivity},
		"Undo of a Block":  {undo(like, map[string]string{"object_type": `"Block"`}), federation.ErrUnsupportedActivity},
		"Undo of a Create": {undo(like, map[string]string{"object_type": `"Create"`}), federation.ErrUnsupportedActivity},
		"Undo of a Follow, foreign object (verb decides first)": {undo("https://c.example/activities/f", map[string]string{"object_type": `"Follow"`}), federation.ErrUnsupportedActivity},
		"no object_type":         {undo(like, nil), federation.ErrInvalidMember},
		"object_type not string": {undo(like, map[string]string{"object_type": `7`}), federation.ErrInvalidMember},
		"object on another origin": {undo("https://c.example/activities/like1", map[string]string{"object_type": `"Like"`}),
			federation.ErrInvalidMember},
		"object on the receiver": {undo("https://a.example/posts/p1", map[string]string{"object_type": `"Like"`}), federation.ErrInvalidMember},
		"plain-http object":      {undo("http://b.example/activities/like1", map[string]string{"object_type": `"Like"`}), federation.ErrInvalidMember},
		"relative object":        {undo("/activities/like1", map[string]string{"object_type": `"Like"`}), federation.ErrInvalidMember},
		"object with a query":    {undo(like+"?x=1", map[string]string{"object_type": `"Like"`}), federation.ErrInvalidMember},
		"object with a fragment": {undo(like+"#f", map[string]string{"object_type": `"Like"`}), federation.ErrInvalidMember},
		"object is its own id":   {undo("https://b.example/activities/undo1", map[string]string{"object_type": `"Like"`}), federation.ErrInvalidMember},
		"no object":              {undo("", map[string]string{"object_type": `"Like"`}), federation.ErrInvalidMember},
		"an extra member":        {undo(like, map[string]string{"object_type": `"Like"`, "content": `"x"`}), federation.ErrUnknownField},
	} {
		if err := federation.ValidateMembers(c.env, applied); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}

	// A receiver that does not apply Undo answers every Undo
	// unsupported_activity, whatever it carries.
	none := federation.MemberContext{}
	if err := federation.ValidateMembers(undo(like, map[string]string{"object_type": `"Like"`}), none); !errors.Is(err, federation.ErrUnsupportedActivity) {
		t.Errorf("unapplied Undo: %v", err)
	}

	// Encrypted: the clear object is checked at receipt (object_type is
	// sealed and checked after decryption); no member may be in clear.
	sealed := func(object string) *federation.Envelope {
		env := undo(object, nil)
		env.Extra = nil
		env.Encryption = &federation.EncryptionBlock{}
		return env
	}
	if err := federation.ValidateMembers(sealed(like), applied); err != nil {
		t.Errorf("sealed Undo of an own activity refused at receipt: %v", err)
	}
	if err := federation.ValidateMembers(sealed("https://c.example/activities/like1"), applied); !errors.Is(err, federation.ErrInvalidMember) {
		t.Errorf("sealed Undo of a foreign object: %v, want invalid_member at receipt", err)
	}
	clear := undo(like, map[string]string{"object_type": `"Like"`})
	clear.Encryption = &federation.EncryptionBlock{}
	if err := federation.ValidateMembers(clear, applied); !errors.Is(err, federation.ErrInvalidMember) {
		t.Errorf("object_type in clear beside encryption: %v", err)
	}
}

// What the sender sends for an Undo of a Like is exactly what the
// receiver admits: object = the Like id, object_type "Like", nothing
// else from the local payload.
func TestUndoWire_RoundTripsThroughValidateMembers(t *testing.T) {
	likeID := "https://b.example/activities/like1"
	payload := []byte(`{"target_post_id":"p","target_type":"Like"}`)
	if !federation.Federated(federation.ActivityUndo, "activity", payload) {
		t.Fatal("an Undo of a Like is not federated")
	}
	for _, other := range []string{`{"target_type":"Follow"}`, `{"target_type":"Block"}`, `{}`, ``, `not json`} {
		if federation.Federated(federation.ActivityUndo, "activity", []byte(other)) {
			t.Errorf("Undo with payload %q federated", other)
		}
	}
	obj, err := federation.WireObject(federation.ActivityUndo, "activity", likeID, payload)
	if err != nil || obj != likeID {
		t.Fatalf("wire object %q, %v; want the Like id", obj, err)
	}
	if _, err := federation.WireObject(federation.ActivityUndo, "activity", "urn:aa:legacy-like:x", payload); !errors.Is(err, federation.ErrNotFederated) {
		t.Errorf("an Undo naming a non-https id: %v, want not federated", err)
	}
	wire, err := federation.WireMembers(federation.ActivityUndo, "activity", payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != 1 || string(wire[federation.MemberObjectType]) != `"Like"` {
		t.Fatalf("wire members %v; want only object_type \"Like\"", wire)
	}
	env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityUndo,
		ID: "https://b.example/activities/undo1", Actor: "https://b.example/users/bob", Object: obj, Extra: wire}
	if err := federation.ValidateMembers(env, federation.MemberContext{Applied: func(federation.ActivityType) bool { return true }}); err != nil {
		t.Errorf("the receiver refuses what the sender sends: %v", err)
	}
}
