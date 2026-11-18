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

	// share-lifecycle-1 members (§16.6, #1571). aa:supersedes names the
	// aa:Share this grant replaces for the same grant tuple (absent on a
	// tuple's first grant); aa:revokes names the aa:Share an aa:Unshare
	// ends (required). Both are activity URIs on the sender's origin.
	MemberSupersedes = "aa:supersedes"
	MemberRevokes    = "aa:revokes"
	// MemberOriginalTransfer is the original-transfer-1 member of an
	// aa:Share (§7.5.7, #1571): true when the origin's share carries the
	// original-file grant. Only sent and accepted with that profile.
	MemberOriginalTransfer = "aa:originalTransfer"

	// ProfileShareLifecycle1 is the share-lifecycle-1 profile token
	// (§16.6, #1571). aa:Share, aa:Unshare and aa:RevokeShare are sent
	// and applied only between peers that both negotiated it.
	ProfileShareLifecycle1 = "share-lifecycle-1"
	// ProfileOriginalTransfer1 is the original-transfer-1 profile token
	// (§7.5.7, §16.5). Reserved and not advertised; it gates the
	// aa:originalTransfer member.
	ProfileOriginalTransfer1 = "original-transfer-1"

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
	// Applied reports whether this receiver applies an activity type
	// for this sender (a share-lifecycle-1 type only with the profile
	// negotiated). Nil means none is.
	Applied func(ActivityType) bool
	// LocalOrigin is the receiver's own origin (scheme://host[:port]).
	// When set, an aa:Share / aa:Unshare must name it as target_peer
	// and any target_user must be a /users/<name> actor on it. Empty
	// skips that check here; the share handler re-checks it against the
	// real base URL before applying anything.
	LocalOrigin string
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
	case ActivityAAShare, ActivityAAUnshare, ActivityAARevokeShare:
		share := env.Type == ActivityAAShare
		if err := validateShareEnvelope(env, mc, share); err != nil {
			return err
		}
		allowed[MemberObjectKind], allowed[MemberTargetPeer], allowed[MemberTargetUser] = true, true, true
		if share {
			allowed[MemberScope], allowed[MemberExpiresAt] = true, true
			allowed[MemberSupersedes], allowed[MemberOriginalTransfer] = true, true
		} else {
			allowed[MemberRevokes] = true
		}
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

// checkOptionalHTTPSURI: m[name], when present, is an absolute https URI.
func checkOptionalHTTPSURI(m map[string]json.RawMessage, name string) error {
	if raw, ok := m[name]; ok {
		return checkHTTPSURI(raw, name)
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

// shareObjectSegment is the first path segment of a shared object's
// URI for each kind (§8.2).
var shareObjectSegment = map[ShareObjectKind]string{
	ShareObjectKindPost:       "posts",
	ShareObjectKindAsset:      "assets",
	ShareObjectKindCollection: "collections",
	ShareObjectKindWorkspace:  "workspaces",
	ShareObjectKindBrandKit:   "brand_kits",
	ShareObjectKindUser:       "users",
}

// validateShareEnvelope checks an applied aa:Share (share=true) or
// aa:Unshare / aa:RevokeShare under share-lifecycle-1 (§7.5.1, §7.5.2,
// §16.6):
//
//   - the §7.5 tuple members (checkShareMembers);
//   - the object is on the sender's own origin, the actor's origin, with
//     the kind's path: a peer can grant only what it is the origin of;
//   - with mc.LocalOrigin, target_peer is this receiver and target_user
//     a /users/<name> actor on it;
//   - aa:supersedes (Share, optional) and aa:revokes (Unshare, required)
//     are activity URIs on the sender's origin, never the envelope's own
//     id;
//   - aa:originalTransfer (Share, optional) is a boolean, needs
//     original-transfer-1 negotiated (ErrProfileNotNegotiated) and an
//     asset or collection share.
func validateShareEnvelope(env *Envelope, mc MemberContext, share bool) error {
	if err := checkShareMembers(env.Extra, share); err != nil {
		return err
	}
	sender := originOf(env.Actor)
	if sender == "" {
		return fmt.Errorf("%w: actor is not an https URI", ErrInvalidMember)
	}
	kind, _ := memberString(env.Extra[MemberObjectKind], MemberObjectKind)
	obj, err := url.Parse(env.Object)
	if err != nil || env.Object == "" || originOf(env.Object) != sender || obj.RawQuery != "" || obj.Fragment != "" {
		return fmt.Errorf("%w: a share's object must be an object on the sender's own origin", ErrInvalidMember)
	}
	segs := strings.Split(strings.Trim(obj.Path, "/"), "/")
	if len(segs) < 2 || segs[0] != shareObjectSegment[ShareObjectKind(kind)] || segs[len(segs)-1] == "" {
		return fmt.Errorf("%w: object is not a %s URI", ErrInvalidMember, kind)
	}
	if mc.LocalOrigin != "" {
		peerURL, _ := memberString(env.Extra[MemberTargetPeer], MemberTargetPeer)
		if originOf(peerURL) != mc.LocalOrigin {
			return fmt.Errorf("%w: %s is not this instance", ErrInvalidMember, MemberTargetPeer)
		}
		if raw, ok := env.Extra[MemberTargetUser]; ok {
			u, _ := memberString(raw, MemberTargetUser)
			if _, ok := LocalUsername(u, mc.LocalOrigin); !ok {
				return fmt.Errorf("%w: %s is not a /users/<name> actor on this instance", ErrInvalidMember, MemberTargetUser)
			}
		}
	}
	ref := func(name string, required bool) error {
		raw, ok := env.Extra[name]
		if !ok {
			if required {
				return fmt.Errorf("%w: %s requires %s", ErrInvalidMember, env.Type, name)
			}
			return nil
		}
		s, err := memberString(raw, name)
		if err != nil {
			return err
		}
		u, perr := url.Parse(s)
		if perr != nil || originOf(s) != sender || u.Fragment != "" || s == env.ID {
			return fmt.Errorf("%w: %s must be another activity URI on the sender's origin", ErrInvalidMember, name)
		}
		return nil
	}
	if share {
		if err := ref(MemberSupersedes, false); err != nil {
			return err
		}
		if raw, ok := env.Extra[MemberOriginalTransfer]; ok {
			if mc.Negotiated == nil || !mc.Negotiated(ProfileOriginalTransfer1) {
				return fmt.Errorf("%w: %s needs %s", ErrProfileNotNegotiated, MemberOriginalTransfer, ProfileOriginalTransfer1)
			}
			var b bool
			if string(raw) == "null" || json.Unmarshal(raw, &b) != nil {
				return fmt.Errorf("%w: %s must be a boolean", ErrInvalidMember, MemberOriginalTransfer)
			}
			if k := ShareObjectKind(kind); b && k != ShareObjectKindAsset && k != ShareObjectKindCollection {
				return fmt.Errorf("%w: %s is only for an asset or collection share", ErrInvalidMember, MemberOriginalTransfer)
			}
		}
		return nil
	}
	return ref(MemberRevokes, true)
}

// LocalUsername returns the name of a /users/<name> actor URI on the
// given origin, and whether uri is one.
func LocalUsername(uri, origin string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || origin == "" || originOf(uri) != origin || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(segs) != 2 || segs[0] != "users" || segs[1] == "" {
		return "", false
	}
	return segs[1], true
}

// OriginOf returns the https origin (scheme://host[:port], host in
// lower case) of an absolute https URI, or "" when it is not one.
func OriginOf(raw string) string { return originOf(raw) }

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	return "https://" + strings.ToLower(u.Host)
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

// WireObject is the envelope `object` for an activity: for a comment
// Create (#1604) and a Like (#1611), the object on the instance that
// holds it (the local payload's target_uri, required, an absolute https
// URI); for every other activity, the activity's own object URI. A
// comment's own URI stays in the sender's local record: on the wire the
// Create's signed `id` identifies the comment, which is what a reply's
// inReplyTo and a future Delete or Undo of it (#1572) refer to. A Like
// without target_uri is of a local object and is never sent.
func WireObject(t ActivityType, objectKind, objectURI string, payload []byte) (string, error) {
	if !(t == ActivityCreate && objectKind == "comment") && t != ActivityLike {
		return objectURI, nil
	}
	var local struct {
		TargetURI string `json:"target_uri"`
	}
	if len(bytes.TrimSpace(payload)) > 0 {
		if err := json.Unmarshal(payload, &local); err != nil {
			return "", fmt.Errorf("%w: %s payload: %v", ErrNotFederated, t, err)
		}
	}
	if !absoluteHTTPS(local.TargetURI) {
		if t == ActivityLike {
			return "", fmt.Errorf("%w: a Like is sent only for an object on another instance (target_uri)", ErrNotFederated)
		}
		return "", fmt.Errorf("%w: a comment Create needs its target's URI (target_uri)", ErrNotFederated)
	}
	return local.TargetURI, nil
}

// WireMembers projects a local activity payload onto the members the
// activity's v1 schema defines (§7.5): none for Like; object_type,
// content and inReplyTo for a comment; the §7.5.1 / §7.5.2 names for
// aa:Share / aa:Unshare. Every other payload member (operator notes,
// local ids, titles kept for notifications) stays local. A type
// Federated refuses, a payload missing a required member, or a member
// whose shape §7.5 refuses (an empty target_peer, a boolean scope) is
// ErrNotFederated, wrapping ErrInvalidMember for the last.
func WireMembers(t ActivityType, objectKind string, payload []byte, negotiated func(token string) bool) (map[string]json.RawMessage, error) {
	if !Federated(t, objectKind) {
		return nil, fmt.Errorf("%w: %s of %q", ErrNotFederated, t, objectKind)
	}
	has := func(token string) bool { return negotiated != nil && negotiated(token) }
	if (t == ActivityAAShare || t == ActivityAAUnshare) && !has(ProfileShareLifecycle1) {
		// §16.3: a profile-gated activity is never sent to a peer
		// without the profile, and its members are never stripped to
		// fit one. The resolver skips such a recipient before a row is
		// queued; a row queued earlier is closed here.
		return nil, fmt.Errorf("%w: %w: %s is sent only with %s", ErrNotFederated, ErrProfileNotNegotiated, t, ProfileShareLifecycle1)
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
		// #1604: inReplyTo is the parent COMMENT of a reply only (the
		// local record's own inReplyTo names the post for a top-level
		// comment and stays local).
		if err := copyAs("in_reply_to_uri", MemberInReplyTo, false); err != nil {
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
			if err := copyAs("supersedes_activity_uri", MemberSupersedes, false); err != nil {
				return nil, err
			}
			// The original-file grant travels only with
			// original-transfer-1 (reserved, not advertised). Without it
			// the member is absent, which means no original: the
			// origin refuses every original fetch from such a peer too.
			var allow bool
			if raw, ok := local["allow_original"]; ok && json.Unmarshal(raw, &allow) == nil && allow && has(ProfileOriginalTransfer1) {
				out[MemberOriginalTransfer] = json.RawMessage("true")
			}
		} else {
			// aa:revokes is REQUIRED: an Unshare that cannot name the
			// grant it ends is refused, never sent without it.
			if raw, ok := local["previous_activity_uri"]; ok {
				var s string
				if json.Unmarshal(raw, &s) == nil && s == "" {
					delete(local, "previous_activity_uri")
				}
			}
			if err := copyAs("previous_activity_uri", MemberRevokes, true); err != nil {
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
		if err == nil {
			err = checkOptionalHTTPSURI(out, MemberSupersedes)
		}
	case ActivityAAUnshare:
		err = checkShareMembers(out, false)
		if err == nil {
			err = checkHTTPSURI(out[MemberRevokes], MemberRevokes)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNotFederated, t, err)
	}
	return out, nil
}
