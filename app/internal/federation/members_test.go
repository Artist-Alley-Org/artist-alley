// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1580: the v1 member contract (members.go). The conformance vectors
// under testdata/conformance/members are implementation-agnostic; the
// rest pins this implementation's sender projection and its meeting
// with v0.11.1 (testdata/compat/v0.11.1).

package federation_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// memberReason maps a parse or member refusal to its §12.1 reason, so
// the vectors name reasons rather than Go sentinels.
func memberReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, federation.ErrInvalidContext):
		return string(federation.InboxStatusInvalidContext)
	case errors.Is(err, federation.ErrProfileNotNegotiated):
		return string(federation.InboxStatusProfileNotNegotiated)
	case errors.Is(err, federation.ErrArchivalInvalid):
		return string(federation.InboxStatusArchivalInvalid)
	case errors.Is(err, federation.ErrUnsupportedActivity):
		return string(federation.InboxStatusUnsupportedActivity)
	case errors.Is(err, federation.ErrUnknownField):
		return string(federation.InboxStatusUnknownField)
	case errors.Is(err, federation.ErrInvalidMember):
		return string(federation.InboxStatusInvalidMember)
	}
	return "unexpected: " + err.Error()
}

func memberCtx(negotiated, applied []string) federation.MemberContext {
	return federation.MemberContext{
		Negotiated: func(tok string) bool { return slices.Contains(negotiated, tok) },
		Applied:    func(t federation.ActivityType) bool { return slices.Contains(applied, string(t)) },
	}
}

// checkMembers is the receiver's member path: structural parse, then
// ValidateMembers.
func checkMembers(raw []byte, mc federation.MemberContext) error {
	env, err := federation.Unmarshal(raw)
	if err != nil {
		return err
	}
	return federation.ValidateMembers(env, mc)
}

func TestConformance_Members(t *testing.T) {
	dir := filepath.Join(conformanceDir, "members")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		n++
		t.Run(strings.TrimSuffix(e.Name(), ".json"), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var v struct {
				Negotiated     []string        `json:"negotiated"`
				Applied        []string        `json:"applied"`
				Envelope       json.RawMessage `json:"envelope"`
				ExpectedReason string          `json:"expected_reason"`
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			got := memberReason(checkMembers(v.Envelope, memberCtx(v.Negotiated, v.Applied)))
			if got != v.ExpectedReason {
				t.Errorf("reason %q, want %q", got, v.ExpectedReason)
			}
		})
	}
	if n < 20 {
		t.Fatalf("only %d member vectors", n)
	}
}

func TestValidateMembers_Negotiated(t *testing.T) {
	like := []byte(`{"@context":"https://artist-alley.org/protocol/v1","type":"Like","id":"https://a.example/activities/1",
		"actor":"https://a.example/users/alice","published":"2026-11-17T10:00:00Z","object":"https://b.example/posts/1",
		"aa:asset":{}}`)
	applied := []string{"Like", "Create"}
	if got := memberReason(checkMembers(like, memberCtx(nil, applied))); got != "profile_not_negotiated" {
		t.Errorf("unnegotiated: %q", got)
	}
	// Negotiated, the member is held to the profile: an asset record
	// on a Like is archival_invalid, never accepted silently.
	if got := memberReason(checkMembers(like, memberCtx([]string{federation.ProfileArchival1}, applied))); got != "archival_invalid" {
		t.Errorf("negotiated: %q", got)
	}
}

func TestCheckDuplicateMembers(t *testing.T) {
	for doc, wantErr := range map[string]bool{
		`{"a":1,"b":{"c":[{"d":1},{"d":2}]}}`: false,
		`{"a":1,"a":1}`:                       true,
		`{"a":{"b":1,"b":2}}`:                 true,
		`{"a":[{"x":1,"x":1}]}`:               true,
		`{"a":1} {"b":2}`:                     true,
		`{"a":`:                               true,
	} {
		err := federation.CheckDuplicateMembers([]byte(doc))
		if (err != nil) != wantErr || (err != nil && !errors.Is(err, federation.ErrInvalidMember)) {
			t.Errorf("%s: err %v, want error %v", doc, err, wantErr)
		}
	}
}

// lifecycleOnly: the recipient negotiated share-lifecycle-1 and nothing
// else.
func lifecycleOnly(token string) bool { return token == federation.ProfileShareLifecycle1 }

func wireNames(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Every type the emitters produce is either projected onto its v1
// members or refused as not federated; nothing passes through as is.
func TestWireMembers(t *testing.T) {
	share := `{"share_id":"s","object_kind":"post","object_id":"o","object_uri":"u","scope":"view","peer_url":"https://b.example",` +
		`"target_user_url":"https://b.example/users/bob","expires_at":"2026-12-31T00:00:00Z","notes":"private","notes_private":true,` +
		`"previous_activity_uri":"https://a.example/activities/g1"}`
	cases := []struct {
		typ     federation.ActivityType
		kind    string
		payload string
		want    []string // nil with wantErr
		wantErr bool
	}{
		{federation.ActivityLike, "post", `{"post_title":"t"}`, []string{}, false},
		{federation.ActivityCreate, "comment", `{"object_type":"Note","inReplyTo":"https://a.example/posts/1","content":"c","comment_depth":1,"target_post_id":"p","target_post_title":"t"}`,
			[]string{"content", "inReplyTo", "object_type"}, false},
		{federation.ActivityCreate, "comment", `{"object_type":"Note","content":"top-level"}`, []string{"content", "object_type"}, false},
		{federation.ActivityAAShare, "post", share, []string{"expires_at", "object_kind", "scope", "target_peer", "target_user"}, false},
		{federation.ActivityAAUnshare, "post", share, []string{"aa:revokes", "object_kind", "target_peer", "target_user"}, false},
		// Required member missing from the local payload.
		{federation.ActivityCreate, "comment", `{"object_type":"Note"}`, nil, true},
		{federation.ActivityAAShare, "post", `{"object_kind":"post","scope":"view"}`, nil, true},
		// Not federated in v1.
		{federation.ActivityCreate, "post", `{"object_type":"aa:Post","title":"t","visibility":"public"}`, nil, true},
		{federation.ActivityCreate, "message", `{"object_type":"Note","content":"dm","directMessage":true}`, nil, true},
		{federation.ActivityCreate, "collection", `{"object_type":"Collection","name":"n"}`, nil, true},
		{federation.ActivityUpdate, "post", `{"title":"t"}`, nil, true},
		{federation.ActivityUpdate, "user", `{"display_name":"d"}`, nil, true},
		{federation.ActivityDelete, "comment", `{"target_post_id":"p"}`, nil, true},
		{federation.ActivityUndo, "activity", `{"target_post_id":"p"}`, nil, true},
		{federation.ActivityAdd, "collection", `{"target_name":"n"}`, nil, true},
		{federation.ActivityRemove, "collection", `{"target_name":"n"}`, nil, true},
		{federation.ActivityFollow, "user", ``, nil, true},
		{federation.ActivityAccept, "activity", ``, nil, true},
		{federation.ActivityBlock, "user", `{"target_user_ref":1}`, nil, true},
		{federation.ActivityAAAnnotation, "comment", `{"annotation_kind":"k"}`, nil, true},
	}
	// Malformed local payloads (#1580 C1): the projection is checked
	// against §7.5 before anything is signed or sealed; a bad value is
	// refused, never sent.
	base := map[string]any{"share_id": "s", "object_kind": "post", "object_id": "o", "object_uri": "u",
		"scope": "view", "peer_url": "https://b.example", "target_user_url": "https://b.example/users/bob",
		"expires_at": "2026-12-31T00:00:00Z", "notes": "private", "notes_private": true,
		"previous_activity_uri": "https://a.example/activities/g1"}
	shareWith := func(k string, v any) string {
		m := map[string]any{}
		for kk, vv := range base {
			m[kk] = vv
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	for _, bad := range []struct {
		typ     federation.ActivityType
		payload string
	}{
		{federation.ActivityCreate, `{"object_type":"Note","content":""}`},
		{federation.ActivityCreate, `{"object_type":"Note","content":"   "}`},
		{federation.ActivityCreate, `{"object_type":"Note","content":7}`},
		{federation.ActivityCreate, `{"object_type":"Note","content":"` + strings.Repeat("é", 10001) + `"}`},
		{federation.ActivityCreate, `{"object_type":"Note","content":"ok","inReplyTo":"/comments/1"}`},
		{federation.ActivityCreate, `{"object_type":"Note","content":"ok","inReplyTo":42}`},
		{federation.ActivityAAShare, shareWith("object_kind", "spaceship")},
		{federation.ActivityAAShare, shareWith("object_kind", 3)},
		{federation.ActivityAAShare, shareWith("peer_url", "")},
		{federation.ActivityAAShare, shareWith("peer_url", "http://b.example")},
		{federation.ActivityAAShare, shareWith("peer_url", "https://b.example/inbox")},
		{federation.ActivityAAShare, shareWith("target_user_url", "bob")},
		{federation.ActivityAAShare, shareWith("scope", true)},
		{federation.ActivityAAShare, shareWith("scope", "edit")},
		{federation.ActivityAAShare, shareWith("expires_at", "tomorrow")},
		{federation.ActivityAAShare, shareWith("expires_at", 1767225600)},
		{federation.ActivityAAUnshare, shareWith("object_kind", "")},
		{federation.ActivityAAUnshare, shareWith("peer_url", "https://user@b.example")},
	} {
		if got, err := federation.WireMembers(bad.typ, map[federation.ActivityType]string{
			federation.ActivityCreate: "comment", federation.ActivityAAShare: "post", federation.ActivityAAUnshare: "post",
		}[bad.typ], []byte(bad.payload), lifecycleOnly); !errors.Is(err, federation.ErrNotFederated) || !errors.Is(err, federation.ErrInvalidMember) {
			t.Errorf("%s %.80s: members %v, err %v; want ErrNotFederated wrapping ErrInvalidMember", bad.typ, bad.payload, wireNames(got), err)
		}
	}

	for _, c := range cases {
		got, err := federation.WireMembers(c.typ, c.kind, []byte(c.payload), lifecycleOnly)
		if c.wantErr {
			if !errors.Is(err, federation.ErrNotFederated) {
				t.Errorf("%s/%s: err %v, want ErrNotFederated", c.typ, c.kind, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s/%s: %v", c.typ, c.kind, err)
			continue
		}
		if names := wireNames(got); !reflect.DeepEqual(names, c.want) {
			t.Errorf("%s/%s: members %v, want %v", c.typ, c.kind, names, c.want)
		}
		if b, _ := json.Marshal(got); strings.Contains(string(b), "private") {
			t.Errorf("%s/%s: private value on the wire: %s", c.typ, c.kind, b)
		}
	}
}

// --- v0.11.1 (testdata/compat/v0.11.1) ------------------------------------

const compatDir = "testdata/compat/v0.11.1"

func readCompat(t *testing.T, pattern string) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(compatDir, pattern))
	if err != nil || len(files) == 0 {
		t.Fatalf("no %s goldens (%v)", pattern, err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = b
	}
	return out
}

// Old sender to this receiver: what v0.11.1 put on the wire is
// refused. Its placeholder signature is malformed (#1526 refuses it at
// stage 10b), and even signed, its members would not pass (#1580).
func TestCompatV0111_SenderBytesRefused(t *testing.T) {
	for name, raw := range readCompat(t, "sender_*.json") {
		t.Run(name, func(t *testing.T) {
			var g struct {
				Envelope        json.RawMessage `json:"envelope"`
				CurrentReceiver struct {
					Signature string `json:"signature"`
					Members   string `json:"members"`
				} `json:"current_receiver"`
			}
			if err := json.Unmarshal(raw, &g); err != nil {
				t.Fatal(err)
			}
			env, err := federation.Unmarshal(g.Envelope)
			if err != nil {
				t.Fatalf("structural parse: %v", err)
			}
			_, pubPEM, _ := freshKeypair(t)
			if err := env.Verify(pubPEM); !errors.Is(err, federation.ErrSigMalformed) {
				t.Errorf("placeholder signature: %v, want ErrSigMalformed (%s)", err, g.CurrentReceiver.Signature)
			}
			got := memberReason(federation.ValidateMembers(env, memberCtx(nil, []string{"Like", "Create"})))
			if got != g.CurrentReceiver.Members {
				t.Errorf("members: %q, want %q", got, g.CurrentReceiver.Members)
			}
		})
	}
}

// This sender to an old receiver: the members sent for each local
// payload are exactly the golden ones, and they include what the
// v0.11.1 receiver reads. A v0.11.1 receiver keeps unknown members and
// checks the signature for shape only (its envelope.go Unmarshal and
// the #1526 changelog), so a Like and a comment are accepted there; its
// comment handler needs a non-blank content (handler_social.go
// extractCommentPayload at c3f6ef2e). Modelled from that source, not
// run against the old binary (#1582 runs both releases).
func TestCompatV0111_CurrentSenderMeetsOldReceiver(t *testing.T) {
	for name, raw := range readCompat(t, "current_sender_*.json") {
		t.Run(name, func(t *testing.T) {
			var g struct {
				ActivityType string                     `json:"activity_type"`
				ObjectKind   string                     `json:"object_kind"`
				LocalPayload json.RawMessage            `json:"local_payload"`
				WireMembers  map[string]json.RawMessage `json:"wire_members"`
				Sent         *bool                      `json:"sent"`
				Receiver     struct {
					Reads   []string `json:"reads"`
					Applies bool     `json:"applies"`
				} `json:"v0_11_1_receiver"`
			}
			if err := json.Unmarshal(raw, &g); err != nil {
				t.Fatal(err)
			}
			// A v0.11.1 peer never negotiated any profile.
			got, err := federation.WireMembers(federation.ActivityType(g.ActivityType), g.ObjectKind, g.LocalPayload, nil)
			if g.Sent != nil && !*g.Sent {
				// #1571: a profile-gated activity is not sent at all.
				if !errors.Is(err, federation.ErrNotFederated) || !errors.Is(err, federation.ErrProfileNotNegotiated) {
					t.Fatalf("%s to a v0.11.1 peer: members %v, err %v; want refused, profile_not_negotiated", g.ActivityType, wireNames(got), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(t, got, g.WireMembers) {
				gb, _ := json.Marshal(got)
				t.Errorf("wire members %s, want the golden %s", gb, mustMarshal(g.WireMembers))
			}
			// v0.11.1 (c3f6ef2e) inbox/handler_stub.go BuildRegistry:
			// real handlers for Like and Create (CommentHandler, which
			// needs a non-blank content); every other verb is a stub
			// that marks the row processed without applying it.
			v0111Applied := map[string]bool{"Like": true, "Create": true}
			applies := v0111Applied[g.ActivityType]
			for _, k := range g.Receiver.Reads {
				var s string
				if err := json.Unmarshal(got[k], &s); err != nil || strings.TrimSpace(s) == "" {
					if k == federation.MemberContent {
						applies = false
						t.Errorf("the v0.11.1 receiver needs a non-blank %s", k)
					}
				}
			}
			if applies != g.Receiver.Applies {
				t.Errorf("v0.11.1 applies %s: model says %v, golden says %v", g.ActivityType, applies, g.Receiver.Applies)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b map[string]json.RawMessage) bool {
	t.Helper()
	var x, y any
	_ = json.Unmarshal(mustMarshal(a), &x)
	_ = json.Unmarshal(mustMarshal(b), &y)
	return reflect.DeepEqual(x, y)
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
