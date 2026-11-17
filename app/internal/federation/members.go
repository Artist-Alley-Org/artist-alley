// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Envelope members: what a v1 envelope of each activity may carry, on
// both sides of the wire (#1580, spec v1 §3.2, §7.5, §16).
//
// Receiving: Unmarshal is structural (it refuses duplicate member
// names and keeps every member outside the §3 base set in Extra);
// ValidateMembers then decides, for the activity's type and the
// sender's negotiated profiles, whether each of those members is
// allowed and well formed. Nothing is accepted that this file does not
// define.
//
// Sending: WireMembers projects the local activity payload (the audit
// record, which may hold operator-only data and local ids) onto the
// members the activity's v1 schema defines. Activity types v1 does not
// federate, and projections that do not satisfy §7.5, are refused with
// ErrNotFederated, never sent.

package federation

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Member names.
const (
	MemberObjectType    = "object_type"
	MemberContent       = "content"
	MemberInReplyTo     = "inReplyTo"
	MemberDirectMessage = "directMessage"
	MemberObjectKind    = "object_kind"
	MemberTargetPeer    = "target_peer"
	MemberTargetUser    = "target_user"
	MemberScope         = "scope"
	MemberExpiresAt     = "expires_at"

	// CommentObjectType is the object_type of a comment Create.
	CommentObjectType = "Note"

	// CommentContentMaxRunes bounds a comment's content, the same
	// bound as a local comment (openapi.yaml CommentCreate.body).
	CommentContentMaxRunes = 10000
)

var (
	// ErrInvalidMember: a member the schema defines is missing,
	// repeated, or has the wrong shape or value (inbox reason
	// invalid_member).
	ErrInvalidMember = errors.New("federation: invalid envelope member")

	// ErrUnsupportedActivity: an activity this receiver does not apply
	// (inbox reason unsupported_activity), including a Create that is
	// not a comment.
	ErrUnsupportedActivity = errors.New("federation: activity is not applied by this receiver")

	// ErrNotFederated: the sender does not federate this activity in
	// v1, or its local payload lacks a member the wire schema needs.
	ErrNotFederated = errors.New("federation: activity_not_federated")
)

// MemberContext is what ValidateMembers needs to know about the sender
// and the receiver.
type MemberContext struct {
	// Negotiated reports whether a profile token is in the stored,
	// negotiated capability intersection with the sending peer. Nil
	// means none is.
	Negotiated func(token string) bool
	// Applied reports whether this receiver applies an activity type.
	// Nil means none is.
	Applied func(ActivityType) bool
}

// ValidateMembers checks env's non-base members against the v1
// schema of its activity type. In order:
//
//  1. With an encryption block, no activity member may be in clear;
//     the sealed payload is checked again after decryption (with
//     Encryption cleared and Extra restored).
//  2. archival-1 members and gated types need the negotiated profile
//     (ErrProfileNotNegotiated) and must match it (ErrArchivalInvalid).
//  3. A type the receiver does not apply is ErrUnsupportedActivity;
//     so is a Create that is not a comment.
//  4. Every member must be defined for the type (ErrUnknownField) and
//     well formed (ErrInvalidMember).
func ValidateMembers(env *Envelope, mc MemberContext) error {
	negotiated := mc.Negotiated != nil && mc.Negotiated(ProfileArchival1)
	applied := func(t ActivityType) bool { return mc.Applied != nil && mc.Applied(t) }

	if env.Encryption != nil {
		if names := sortedNames(env.Extra); len(names) > 0 {
			return fmt.Errorf("%w: %s in clear beside an encryption block", ErrInvalidMember, quoteMember(names[0]))
		}
		if err := ValidateArchivalMembers(env, negotiated); err != nil {
			return err
		}
		if !applied(env.Type) {
			return fmt.Errorf("%w: %s", ErrUnsupportedActivity, env.Type)
		}
		return nil
	}

	if err := ValidateArchivalMembers(env, negotiated); err != nil {
		return err
	}
	if !applied(env.Type) {
		return fmt.Errorf("%w: %s", ErrUnsupportedActivity, env.Type)
	}

	allowed := map[string]bool{PropEncryptionPublicKey: true}
	switch env.Type {
	case ActivityLike:
		if env.Object == "" {
			return fmt.Errorf("%w: Like requires object", ErrInvalidMember)
		}
	case ActivityCreate:
		if err := validateCommentCreate(env); err != nil {
			return err
		}
		allowed[MemberObjectType], allowed[MemberContent], allowed[MemberInReplyTo] = true, true, true
	default:
		// An applied type with no activity members defined carries
		// none: only the base set is allowed. A handler that needs
		// members lands with their schema here.
	}
	for _, name := range sortedNames(env.Extra) {
		if !allowed[name] {
			return fmt.Errorf("%w: %s is not a %s member", ErrUnknownField, quoteMember(name), env.Type)
		}
	}
	if raw, ok := env.Extra[PropEncryptionPublicKey]; ok {
		if err := validateEncryptionPublicKey(raw); err != nil {
			return err
		}
	}
	return nil
}

// validateCommentCreate: the only Create v1 applies is a comment,
// object_type "Note", without directMessage. Any other Create (a post,
// a collection, a direct message) is unsupported; a comment needs a
// non-blank content of at most CommentContentMaxRunes and an optional
// absolute https inReplyTo.
func validateCommentCreate(env *Envelope) error {
	if _, dm := env.Extra[MemberDirectMessage]; dm {
		return fmt.Errorf("%w: a direct-message Create is not federated in v1", ErrUnsupportedActivity)
	}
	raw, ok := env.Extra[MemberObjectType]
	if !ok {
		return fmt.Errorf("%w: Create requires %s", ErrInvalidMember, MemberObjectType)
	}
	ot, err := memberString(raw, MemberObjectType)
	if err != nil {
		return err
	}
	if ot != CommentObjectType {
		return fmt.Errorf("%w: Create of %s", ErrUnsupportedActivity, quoteMember(ot))
	}
	if env.Object == "" {
		return fmt.Errorf("%w: Create requires object", ErrInvalidMember)
	}
	return checkCommentMembers(env.Extra)
}

// checkCommentMembers checks a comment's content (required: a
// non-blank string of at most CommentContentMaxRunes code points) and
// inReplyTo (optional: an absolute https URI). The receiver and the
// sender (WireMembers) both use it, so one definition holds for both.
func checkCommentMembers(m map[string]json.RawMessage) error {
	raw, ok := m[MemberContent]
	if !ok {
		return fmt.Errorf("%w: a comment requires %s", ErrInvalidMember, MemberContent)
	}
	content, err := memberString(raw, MemberContent)
	if err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("%w: %s is blank", ErrInvalidMember, MemberContent)
	}
	if n := utf8.RuneCountInString(content); n > CommentContentMaxRunes {
		return fmt.Errorf("%w: %s is %d characters, more than %d", ErrInvalidMember, MemberContent, n, CommentContentMaxRunes)
	}
	if raw, ok := m[MemberInReplyTo]; ok {
		if err := checkHTTPSURI(raw, MemberInReplyTo); err != nil {
			return err
		}
	}
	return nil
}

// checkHTTPSURI: a JSON string holding an absolute https URI without
// userinfo.
func checkHTTPSURI(raw json.RawMessage, name string) error {
	s, err := memberString(raw, name)
	if err != nil {
		return err
	}
	if !absoluteHTTPS(s) {
		return fmt.Errorf("%w: %s is not an absolute https URI", ErrInvalidMember, name)
	}
	return nil
}

// checkHTTPSOrigin: a JSON string holding an https origin, the shape
// of an instance URL (no userinfo, path, query or fragment).
func checkHTTPSOrigin(raw json.RawMessage, name string) error {
	s, err := memberString(raw, name)
	if err != nil {
		return err
	}
	u, perr := url.Parse(s)
	if perr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || strings.ContainsAny(s, "?#") || strings.Trim(u.Path, "/") != "" {
		return fmt.Errorf("%w: %s is not an https instance URL", ErrInvalidMember, name)
	}
	return nil
}

// checkShareMembers checks the §7.5.1 / §7.5.2 members of an aa:Share
// (withScope) or aa:Unshare.
func checkShareMembers(m map[string]json.RawMessage, withScope bool) error {
	kind, err := memberString(m[MemberObjectKind], MemberObjectKind)
	if err != nil {
		return err
	}
	if !ShareObjectKind(kind).Valid() {
		return fmt.Errorf("%w: %s %s is not a shareable kind", ErrInvalidMember, MemberObjectKind, quoteMember(kind))
	}
	if err := checkHTTPSOrigin(m[MemberTargetPeer], MemberTargetPeer); err != nil {
		return err
	}
	if raw, ok := m[MemberTargetUser]; ok {
		if err := checkHTTPSURI(raw, MemberTargetUser); err != nil {
			return err
		}
	}
	if withScope {
		scope, err := memberString(m[MemberScope], MemberScope)
		if err != nil {
			return err
		}
		if !ShareScope(scope).Valid() {
			return fmt.Errorf("%w: %s %s is not view, comment, annotate or remix", ErrInvalidMember, MemberScope, quoteMember(scope))
		}
		if raw, ok := m[MemberExpiresAt]; ok {
			v, err := memberString(raw, MemberExpiresAt)
			if err != nil {
				return err
			}
			if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
				return fmt.Errorf("%w: %s is not an RFC 3339 time", ErrInvalidMember, MemberExpiresAt)
			}
		}
	}
	return nil
}

// validateEncryptionPublicKey: exactly {type, publicKeyBase64,
// version}, type aa:X25519PublicKey, a 32-byte key in standard base64
// and a version from 1.
func validateEncryptionPublicKey(raw json.RawMessage) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return fmt.Errorf("%w: %s is not an object", ErrInvalidMember, PropEncryptionPublicKey)
	}
	for _, k := range sortedNames(m) {
		switch k {
		case "type", "publicKeyBase64", "version":
		default:
			return fmt.Errorf("%w: %s has the unknown member %s", ErrInvalidMember, PropEncryptionPublicKey, quoteMember(k))
		}
	}
	typ, err := memberString(m["type"], PropEncryptionPublicKey+".type")
	if err != nil {
		return err
	}
	if typ != TypeX25519PublicKey {
		return fmt.Errorf("%w: %s.type is not %s", ErrInvalidMember, PropEncryptionPublicKey, TypeX25519PublicKey)
	}
	key, err := memberString(m["publicKeyBase64"], PropEncryptionPublicKey+".publicKeyBase64")
	if err != nil {
		return err
	}
	if b, err := base64.StdEncoding.DecodeString(key); err != nil || len(b) != 32 {
		return fmt.Errorf("%w: %s.publicKeyBase64 is not a 32-byte key", ErrInvalidMember, PropEncryptionPublicKey)
	}
	var version int32
	if raw, ok := m["version"]; !ok || string(raw) == "null" || json.Unmarshal(raw, &version) != nil || version < 1 {
		return fmt.Errorf("%w: %s.version must be an integer from 1", ErrInvalidMember, PropEncryptionPublicKey)
	}
	return nil
}

// memberString decodes a member that must be a JSON string (null is
// not a string).
func memberString(raw json.RawMessage, name string) (string, error) {
	var s string
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &s) != nil {
		return "", fmt.Errorf("%w: %s must be a string", ErrInvalidMember, name)
	}
	return s, nil
}

func absoluteHTTPS(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

func sortedNames(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// quoteMember quotes a peer-supplied name for an error, bounded.
func quoteMember(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strconv.Quote(s)
}

// CheckDuplicateMembers refuses a JSON document in which any object,
// at any depth, repeats a member name (ErrInvalidMember). Go's decoder
// keeps the last value, a canonicaliser may refuse the document, and
// another implementation may keep the first, so a repeated name lets
// the signed bytes and the decoded fields disagree.
func CheckDuplicateMembers(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := walkJSONValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after the JSON value", ErrInvalidMember)
	}
	return nil
}

func walkJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMember, err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			nameTok, err := dec.Token()
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidMember, err)
			}
			name, _ := nameTok.(string)
			if seen[name] {
				return fmt.Errorf("%w: duplicate member %s", ErrInvalidMember, quoteMember(name))
			}
			seen[name] = true
			if err := walkJSONValue(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walkJSONValue(dec); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil { // closing delimiter
		return fmt.Errorf("%w: %v", ErrInvalidMember, err)
	}
	return nil
}

// --- sending --------------------------------------------------------------

// Federated reports whether the sender federates an activity of this
// type about an object of this kind in v1: Like, a comment Create, and
// aa:Share / aa:Unshare. Everything else stays local until a receiver
// applies it (spec v1 §7.6, §12.3 activity_not_federated).
func Federated(t ActivityType, objectKind string) bool {
	switch t {
	case ActivityLike, ActivityAAShare, ActivityAAUnshare:
		return true
	case ActivityCreate:
		return objectKind == "comment"
	}
	return false
}

// WireMembers projects a local activity payload onto the members the
// activity's v1 schema defines (§7.5): none for Like; object_type,
// content and inReplyTo for a comment; the §7.5.1 / §7.5.2 names for
// aa:Share / aa:Unshare. Every other payload member (operator notes,
// local ids, titles kept for notifications) stays local. A type
// Federated refuses, a payload missing a required member, or a member
// whose shape §7.5 refuses (an empty target_peer, a boolean scope) is
// ErrNotFederated, wrapping ErrInvalidMember for the last.
func WireMembers(t ActivityType, objectKind string, payload []byte) (map[string]json.RawMessage, error) {
	if !Federated(t, objectKind) {
		return nil, fmt.Errorf("%w: %s of %q", ErrNotFederated, t, objectKind)
	}
	local := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(payload)) > 0 && string(bytes.TrimSpace(payload)) != "null" {
		if err := json.Unmarshal(payload, &local); err != nil {
			return nil, fmt.Errorf("%w: payload is not an object: %v", ErrNotFederated, err)
		}
	}
	out := map[string]json.RawMessage{}
	copyAs := func(from, to string, required bool) error {
		raw, ok := local[from]
		if !ok || string(raw) == "null" {
			if required {
				return fmt.Errorf("%w: %s payload lacks %s", ErrNotFederated, t, from)
			}
			return nil
		}
		out[to] = raw
		return nil
	}
	switch t {
	case ActivityLike:
		// No activity members: the liked object is the envelope object.
	case ActivityCreate:
		out[MemberObjectType] = json.RawMessage(strconv.Quote(CommentObjectType))
		if err := copyAs(MemberContent, MemberContent, true); err != nil {
			return nil, err
		}
		if err := copyAs(MemberInReplyTo, MemberInReplyTo, false); err != nil {
			return nil, err
		}
	case ActivityAAShare, ActivityAAUnshare:
		if err := copyAs("object_kind", MemberObjectKind, true); err != nil {
			return nil, err
		}
		if err := copyAs("peer_url", MemberTargetPeer, true); err != nil {
			return nil, err
		}
		if err := copyAs("target_user_url", MemberTargetUser, false); err != nil {
			return nil, err
		}
		if t == ActivityAAShare {
			if err := copyAs("scope", MemberScope, true); err != nil {
				return nil, err
			}
			if err := copyAs("expires_at", MemberExpiresAt, false); err != nil {
				return nil, err
			}
		}
	}
	// The projection must satisfy §7.5 before anything is signed or
	// sealed: a malformed local payload is refused, never sent.
	var err error
	switch t {
	case ActivityCreate:
		err = checkCommentMembers(out)
	case ActivityAAShare:
		err = checkShareMembers(out, true)
	case ActivityAAUnshare:
		err = checkShareMembers(out, false)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNotFederated, t, err)
	}
	return out, nil
}
