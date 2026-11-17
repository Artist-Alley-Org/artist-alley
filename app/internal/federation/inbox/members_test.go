// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1580: the inbox accepts only the members spec v1 defines for an
// activity. Every envelope here is signed by the fixture's actor and
// posted through the real handler, so each refusal is the one a peer
// would see.

package inbox_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// memberEnvelope builds a signed envelope of type t carrying extra
// top-level members.
func (fx *fixture) memberEnvelope(t federation.ActivityType, extra map[string]any) []byte {
	fx.t.Helper()
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      string(t),
		"id":        fx.peerURL + "/activities/" + uuid.NewString(),
		"actor":     fx.peerURL + "/users/alice",
		"published": time.Now().UTC().Format(time.RFC3339),
		"object":    fx.peerURL + "/posts/" + uuid.NewString(),
		"signature": map[string]string{
			"type":      "Ed25519",
			"publicKey": fx.peerURL + "/users/alice#main-key",
			"value":     "AAAAAAAAAAAA",
		},
	}
	for k, v := range extra {
		env[k] = v
	}
	b, err := json.Marshal(env)
	if err != nil {
		fx.t.Fatal(err)
	}
	return fx.signEnvelope(b)
}

func validEncKey() map[string]any {
	return map[string]any{
		"type":            "aa:X25519PublicKey",
		"publicKeyBase64": base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"version":         1,
	}
}

func comment(extra map[string]any) map[string]any {
	m := map[string]any{"object_type": "Note", "content": "Nice lighting."}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// refused posts env and asserts the reason, the status and that
// nothing was accepted.
func (fx *fixture) refused(env []byte, status int, reason federation.InboxStatus) {
	fx.t.Helper()
	rr := fx.postSingle(env)
	if rr.Code != status {
		fx.t.Errorf("status %d, want %d; body %s", rr.Code, status, rr.Body.String())
	}
	assertReasonInBody(fx.t, rr.Body.Bytes(), reason)
	if n := fx.countRows(activityIDOf(fx.t, env)); n != 0 {
		fx.t.Errorf("a refused envelope left %d inbox rows; it is refused before storage", n)
	}
	if e := fx.rejects.last(); e == nil || e.reason != reason {
		fx.t.Errorf("audit hook got %+v, want %s", e, reason)
	}
}

func TestInboxMembers_PlaintextRefusals(t *testing.T) {
	cases := []struct {
		name   string
		typ    federation.ActivityType
		extra  map[string]any
		status int
		reason federation.InboxStatus
	}{
		{"like_unknown_member", federation.ActivityLike, map[string]any{"post_title": "local only"}, http.StatusBadRequest, federation.InboxStatusUnknownField},
		{"like_display_hint", federation.ActivityLike, map[string]any{"actorDisplayName": "Alice"}, http.StatusBadRequest, federation.InboxStatusUnknownField},
		{"like_avatar_hint", federation.ActivityLike, map[string]any{"actorAvatarUrl": "https://x.example/a.png"}, http.StatusBadRequest, federation.InboxStatusUnknownField},
		{"comment_unknown_member", federation.ActivityCreate, comment(map[string]any{"comment_depth": 1}), http.StatusBadRequest, federation.InboxStatusUnknownField},
		{"comment_without_content", federation.ActivityCreate, map[string]any{"object_type": "Note"}, http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"comment_blank_content", federation.ActivityCreate, comment(map[string]any{"content": "   "}), http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"comment_content_too_long", federation.ActivityCreate, comment(map[string]any{"content": strings.Repeat("é", 10001)}), http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"comment_content_not_string", federation.ActivityCreate, comment(map[string]any{"content": 7}), http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"comment_without_object_type", federation.ActivityCreate, map[string]any{"content": "hi"}, http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"comment_reply_not_https_uri", federation.ActivityCreate, comment(map[string]any{"inReplyTo": "not a uri"}), http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"enc_key_bad_version", federation.ActivityLike, map[string]any{"aa:encryptionPublicKey": map[string]any{"type": "aa:X25519PublicKey", "publicKeyBase64": base64.StdEncoding.EncodeToString(make([]byte, 32)), "version": 0}}, http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"enc_key_short", federation.ActivityLike, map[string]any{"aa:encryptionPublicKey": map[string]any{"type": "aa:X25519PublicKey", "publicKeyBase64": "AAAA", "version": 1}}, http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"enc_key_extra_member", federation.ActivityLike, map[string]any{"aa:encryptionPublicKey": map[string]any{"type": "aa:X25519PublicKey", "publicKeyBase64": base64.StdEncoding.EncodeToString(make([]byte, 32)), "version": 1, "note": "x"}}, http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"archival_member_unnegotiated", federation.ActivityLike, map[string]any{"aa:asset": map[string]any{}}, http.StatusUnprocessableEntity, federation.InboxStatusProfileNotNegotiated},
		{"provenance_unnegotiated", federation.ActivityAAProvenance, nil, http.StatusUnprocessableEntity, federation.InboxStatusProfileNotNegotiated},
		{"clear_member_beside_encryption", federation.ActivityLike, map[string]any{
			"content": "in clear",
			"encryption": map[string]any{
				"algorithm": "nacl-box-v1", "sender_key_id": "https://x.example/users/alice#encryption-key", "sender_key_version": 1,
				"recipient_key_id": "https://local.example/users/bob#encryption-key", "recipient_key_version": 1,
				"nonce": base64.StdEncoding.EncodeToString(make([]byte, 24)), "ciphertext": base64.StdEncoding.EncodeToString(make([]byte, 32)),
			},
		}, http.StatusBadRequest, federation.InboxStatusInvalidMember},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.refused(fx.memberEnvelope(c.typ, c.extra), c.status, c.reason)
		})
	}
}

// A JSON object may not repeat a member name: the signed bytes and the
// decoded fields could otherwise say different things.
func TestInboxMembers_DuplicateMemberRefused(t *testing.T) {
	fx := newFixture(t)
	env := fx.memberEnvelope(federation.ActivityLike, nil)
	var head struct {
		Object string `json:"object"`
	}
	_ = json.Unmarshal(env, &head)
	dup := strings.Replace(string(env), `"object":`, `"object":"`+head.Object+`","object":`, 1)
	fx.refused([]byte(dup), http.StatusBadRequest, federation.InboxStatusInvalidMember)
}

// A Create that is not a comment (a direct message, a post) is not
// applied as one: it is recorded as unsupported_activity.
func TestInboxMembers_NonCommentCreateIsUnsupported(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"direct_message": {"object_type": "Note", "content": "psst", "directMessage": true},
		"post":           {"object_type": "aa:Post", "title": "New work", "visibility": "public"},
		"collection":     {"object_type": "Collection", "name": "Refs"},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t)
			env := fx.memberEnvelope(federation.ActivityCreate, extra)
			rr := fx.postSingle(env)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422; body %s", rr.Code, rr.Body.String())
			}
			assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnsupportedActivity)
			fx.assertStoredUnsupported(activityIDOf(t, env), env)
		})
	}
}

// What the v1 contract allows is accepted: a Like with no activity
// members, a comment with object_type, content and inReplyTo, and the
// inline encryption key on either.
func TestInboxMembers_ConformingEnvelopesAccepted(t *testing.T) {
	for name, c := range map[string]struct {
		typ   federation.ActivityType
		extra map[string]any
	}{
		"like":              {federation.ActivityLike, nil},
		"like_with_enc_key": {federation.ActivityLike, map[string]any{"aa:encryptionPublicKey": validEncKey()}},
		"comment":           {federation.ActivityCreate, comment(nil)},
		"comment_reply":     {federation.ActivityCreate, comment(map[string]any{"inReplyTo": "https://local.example/comments/" + uuid.NewString(), "aa:encryptionPublicKey": validEncKey()})},
		"comment_max_len":   {federation.ActivityCreate, comment(map[string]any{"content": strings.Repeat("é", 10000)})},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t)
			rr := fx.postSingle(fx.memberEnvelope(c.typ, c.extra))
			if rr.Code != http.StatusAccepted {
				t.Fatalf("status %d, want 202; body %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// The batch endpoint applies the same rule per envelope.
func TestInboxMembers_BatchRefusesPerEnvelope(t *testing.T) {
	fx := newFixture(t)
	good := fx.memberEnvelope(federation.ActivityLike, nil)
	bad := fx.memberEnvelope(federation.ActivityLike, map[string]any{"post_title": "local only"})
	res := fx.postBatch(good, bad)
	if len(res) != 2 {
		t.Fatalf("results %+v", res)
	}
	byID := map[string]batchResultT{}
	for _, r := range res {
		byID[r.ActivityURI] = r
	}
	if r := byID[activityIDOf(t, good)]; r.Status != "accepted" {
		t.Errorf("conforming envelope: %+v", r)
	}
	if r := byID[activityIDOf(t, bad)]; r.Status != "rejected" || r.Reason != string(federation.InboxStatusUnknownField) {
		t.Errorf("unknown member: %+v, want rejected unknown_field", r)
	}
}
