// Typed catalogues for the federation protocol per ADR 0042:
// activity types, object types, collection types, trust tiers,
// encryption policies, share scopes, inbox / outbox status codes.
//
// Every catalogue here is mirrored in the docs/catalogs.md
// meta-index. Database CHECK constraints in the migration
// (00048_federation_*.sql when 1.22.B + 1.22.C land) mirror the
// same string sets — drift between this file and those migrations
// is a code-review block.
//
// The full vocabulary surface is documented in
// docs/spec/federation/v1.md §7.

package federation

// ContextV1 is the exact string the envelope's `@context` field
// MUST equal. Parser does string equality, not JSON-LD context
// resolution.
const ContextV1 = "https://artist-alley.org/protocol/v1"

// ActivityType is the typed catalogue for the `type` field on a
// federation envelope. Standard ActivityStreams 2.0 types are
// carried through; custom artist-alley types are prefixed `aa:`.
type ActivityType string

const (
	// Standard ActivityStreams 2.0 activities (per W3C AS2 Vocabulary).
	ActivityCreate   ActivityType = "Create"
	ActivityUpdate   ActivityType = "Update"
	ActivityDelete   ActivityType = "Delete"
	ActivityFollow   ActivityType = "Follow"
	ActivityAccept   ActivityType = "Accept"
	ActivityReject   ActivityType = "Reject"
	ActivityUndo     ActivityType = "Undo"
	ActivityLike     ActivityType = "Like"
	ActivityAnnounce ActivityType = "Announce" // repost / boost
	ActivityBlock    ActivityType = "Block"

	// Custom artist-alley activities — the load-bearing extension
	// set. Every `aa:*` activity MUST gate on a current
	// federation_shares row (Phase 1.22.C).
	ActivityAAShare              ActivityType = "aa:Share"
	ActivityAAUnshare            ActivityType = "aa:Unshare"
	ActivityAAApprove            ActivityType = "aa:Approve"
	ActivityAARequestChanges     ActivityType = "aa:RequestChanges"
	ActivityAAMarkReviewed       ActivityType = "aa:MarkReviewed"
	ActivityAAAnnotation         ActivityType = "aa:Annotation"
	ActivityAAWorkflowTransition ActivityType = "aa:WorkflowTransition"
	ActivityAAAssetVersion       ActivityType = "aa:AssetVersion"
	ActivityAASubscribe          ActivityType = "aa:Subscribe"
	ActivityAAMention            ActivityType = "aa:Mention"
)

// KnownActivityTypes is the closed set the parser accepts. Any
// other `type` value is rejected with InboxStatusInvalidType.
//
// The set is exposed as a map (not a slice) so membership checks
// are O(1) and the order in which we declare types above doesn't
// leak into runtime behaviour.
var KnownActivityTypes = map[ActivityType]struct{}{
	ActivityCreate:               {},
	ActivityUpdate:               {},
	ActivityDelete:               {},
	ActivityFollow:               {},
	ActivityAccept:               {},
	ActivityReject:               {},
	ActivityUndo:                 {},
	ActivityLike:                 {},
	ActivityAnnounce:             {},
	ActivityBlock:                {},
	ActivityAAShare:              {},
	ActivityAAUnshare:            {},
	ActivityAAApprove:            {},
	ActivityAARequestChanges:     {},
	ActivityAAMarkReviewed:       {},
	ActivityAAAnnotation:         {},
	ActivityAAWorkflowTransition: {},
	ActivityAAAssetVersion:       {},
	ActivityAASubscribe:          {},
	ActivityAAMention:            {},
}

// Valid reports whether t is in the closed catalogue.
func (t ActivityType) Valid() bool {
	_, ok := KnownActivityTypes[t]
	return ok
}

// ObjectType is the typed catalogue for an `Object`'s `type`
// field. Standard AS2 types pass through; custom artist-alley
// types are prefixed `aa:`.
type ObjectType string

const (
	// Standard AS2 objects.
	ObjectNote              ObjectType = "Note"
	ObjectImage             ObjectType = "Image"
	ObjectVideo             ObjectType = "Video"
	ObjectDocument          ObjectType = "Document"
	ObjectCollection        ObjectType = "Collection"
	ObjectOrderedCollection ObjectType = "OrderedCollection"

	// Custom artist-alley objects.
	ObjectAAAsset      ObjectType = "aa:Asset"
	ObjectAAPost       ObjectType = "aa:Post"
	ObjectAAWorkspace  ObjectType = "aa:Workspace"
	ObjectAABrandKit   ObjectType = "aa:BrandKit"
	ObjectAACollection ObjectType = "aa:Collection"
)

// KnownObjectTypes is the closed object-type catalogue. Same
// semantics as KnownActivityTypes.
var KnownObjectTypes = map[ObjectType]struct{}{
	ObjectNote:              {},
	ObjectImage:             {},
	ObjectVideo:             {},
	ObjectDocument:          {},
	ObjectCollection:        {},
	ObjectOrderedCollection: {},
	ObjectAAAsset:           {},
	ObjectAAPost:            {},
	ObjectAAWorkspace:       {},
	ObjectAABrandKit:        {},
	ObjectAACollection:      {},
}

// Valid reports whether t is in the closed catalogue.
func (t ObjectType) Valid() bool {
	_, ok := KnownObjectTypes[t]
	return ok
}

// SignatureAlgorithm is the typed catalogue for the
// `signature.type` field on envelopes. v1 allows exactly one
// algorithm; the typed constant exists so a future Phase 1.22.K
// (key rotation, possibly with additional algorithms) can extend
// it without an interface{} escape hatch.
type SignatureAlgorithm string

const (
	SignatureAlgEd25519 SignatureAlgorithm = "Ed25519"
)

// Valid reports whether s is in the closed catalogue.
func (s SignatureAlgorithm) Valid() bool { return s == SignatureAlgEd25519 }

// EncryptionAlgorithm is the typed catalogue for the
// `encrypted.alg` field in NaCl-box envelopes.
type EncryptionAlgorithm string

const (
	EncryptionAlgNaClBox EncryptionAlgorithm = "nacl-box"
)

// Valid reports whether a is in the closed catalogue.
func (a EncryptionAlgorithm) Valid() bool { return a == EncryptionAlgNaClBox }

// ObjectKind is the typed catalogue for the `object_kind`
// discriminator on aa:Share / aa:Unshare and the
// federation_shares table. Mirrors the CHECK constraint in
// migration 00049_federation_shares.sql (Phase 1.22.C; not yet
// written).
type ObjectKind string

const (
	ObjectKindPost       ObjectKind = "post"
	ObjectKindCollection ObjectKind = "collection"
	ObjectKindWorkspace  ObjectKind = "workspace"
	ObjectKindBrandKit   ObjectKind = "brand_kit"
	ObjectKindAsset      ObjectKind = "asset"
	ObjectKindUser       ObjectKind = "user"
)

// KnownObjectKinds is the closed object-kind catalogue.
var KnownObjectKinds = map[ObjectKind]struct{}{
	ObjectKindPost:       {},
	ObjectKindCollection: {},
	ObjectKindWorkspace:  {},
	ObjectKindBrandKit:   {},
	ObjectKindAsset:      {},
	ObjectKindUser:       {},
}

// Valid reports whether k is in the closed catalogue.
func (k ObjectKind) Valid() bool {
	_, ok := KnownObjectKinds[k]
	return ok
}

// TrustTier describes what a paired peer link can carry. Mirrors
// the CHECK on federation_peers.trust_tier (Phase 1.22.B).
type TrustTier string

const (
	// TrustConnected is the standard tier. Activities about
	// explicitly-shared objects flow in both directions; no
	// content is shared by default.
	TrustConnected TrustTier = "connected"

	// TrustDirectoryListed adds opt-in visibility in the
	// artist-alley.org curated directory (Phase 1.22.H).
	TrustDirectoryListed TrustTier = "directory-listed"

	// TrustAutoSync is per-peer opt-in for instances inside a
	// single trust domain (HQ ↔ remote studio under the same
	// corporate roof). Allows saved auto-share policies (Phase
	// 1.22.J). Never the default.
	TrustAutoSync TrustTier = "auto-sync"
)

// KnownTrustTiers is the closed catalogue.
var KnownTrustTiers = map[TrustTier]struct{}{
	TrustConnected:       {},
	TrustDirectoryListed: {},
	TrustAutoSync:        {},
}

// Valid reports whether t is in the closed catalogue.
func (t TrustTier) Valid() bool {
	_, ok := KnownTrustTiers[t]
	return ok
}

// EncryptionPolicy mirrors federation_peers.encryption_policy.
// Orthogonal to TrustTier — controls whether activities over a
// peer link MUST use NaCl-box envelopes for restricted / embargo
// content (per ADR 0020 sensitivity tiers).
type EncryptionPolicy string

const (
	// EncryptionPlaintext sends plain envelopes for public /
	// team-tier content. Restricted / embargo content still
	// MUST be encrypted (per the sensitivity-tier rules in
	// docs/spec/federation/v1.md §6.5).
	EncryptionPlaintext EncryptionPolicy = "plaintext"

	// EncryptionE2E forces NaCl-box envelopes for all
	// restricted / embargo content regardless of any per-
	// activity override.
	EncryptionE2E EncryptionPolicy = "e2e-encrypted"
)

// KnownEncryptionPolicies is the closed catalogue.
var KnownEncryptionPolicies = map[EncryptionPolicy]struct{}{
	EncryptionPlaintext: {},
	EncryptionE2E:       {},
}

// Valid reports whether p is in the closed catalogue.
func (p EncryptionPolicy) Valid() bool {
	_, ok := KnownEncryptionPolicies[p]
	return ok
}

// ShareScope describes what a recipient peer can DO with a shared
// object. Mirrors federation_shares.scope (Phase 1.22.C).
type ShareScope string

const (
	ShareScopeView     ShareScope = "view"
	ShareScopeComment  ShareScope = "comment"
	ShareScopeAnnotate ShareScope = "annotate"
	ShareScopeEdit     ShareScope = "edit"
)

// KnownShareScopes is the closed catalogue. The order in slices
// derived from this map is implementation-defined; consumers that
// need a stable order use ShareScopeOrdered below.
var KnownShareScopes = map[ShareScope]struct{}{
	ShareScopeView:     {},
	ShareScopeComment:  {},
	ShareScopeAnnotate: {},
	ShareScopeEdit:     {},
}

// ShareScopeOrdered is the elevation-ordered list — earlier
// entries grant strictly less than later ones. `view` ⊂ `comment`
// ⊂ `annotate` ⊂ `edit`. Used by access-check helpers in 1.22.C.
var ShareScopeOrdered = []ShareScope{
	ShareScopeView,
	ShareScopeComment,
	ShareScopeAnnotate,
	ShareScopeEdit,
}

// Valid reports whether s is in the closed catalogue.
func (s ShareScope) Valid() bool {
	_, ok := KnownShareScopes[s]
	return ok
}

// AtLeast reports whether s grants at least the access level of
// other. `edit.AtLeast(view)` is true; `view.AtLeast(edit)` is
// false; an unknown scope is never sufficient.
func (s ShareScope) AtLeast(other ShareScope) bool {
	sRank, sOK := scopeRank(s)
	oRank, oOK := scopeRank(other)
	if !sOK || !oOK {
		return false
	}
	return sRank >= oRank
}

func scopeRank(s ShareScope) (int, bool) {
	for i, ord := range ShareScopeOrdered {
		if ord == s {
			return i, true
		}
	}
	return -1, false
}

// InboxStatus is the typed result of an inbox-side parse + admit
// decision. Maps to federation_inbox.status (Phase 1.22.D) AND
// is the reason code surfaced in audit events
// (federation.activity.received / .rejected per ADR 0033).
//
// Names mirror the reason-code list in docs/spec/federation/v1.md
// §12.1 — adding a new value here without updating the spec is a
// code-review block.
type InboxStatus string

const (
	// Admit paths.
	InboxStatusPending   InboxStatus = "pending"   // received, awaiting dispatch
	InboxStatusProcessed InboxStatus = "processed" // dispatched to domain handler

	// Reject reasons — wire layer.
	InboxStatusInvalidContext       InboxStatus = "invalid_context"       // @context != ContextV1
	InboxStatusUnsigned             InboxStatus = "unsigned"              // missing signature field
	InboxStatusUnsupportedAlgorithm InboxStatus = "unsupported_algorithm" // signature.type not in allowlist
	InboxStatusSigMalformed         InboxStatus = "sig_malformed"         // wrong length / undecodable
	InboxStatusSigInvalid           InboxStatus = "sig_invalid"           // verify failed
	InboxStatusUnknownKey           InboxStatus = "unknown_key"           // publicKey URL not a known actor key
	InboxStatusUnknownField         InboxStatus = "unknown_field"         // strict-parse caught extra field
	InboxStatusInvalidType          InboxStatus = "invalid_type"          // type not in ActivityType catalogue
	InboxStatusInvalidActor         InboxStatus = "invalid_actor"         // actor URI malformed
	InboxStatusInvalidObject        InboxStatus = "invalid_object"        // object URI malformed
	InboxStatusInvalidPublished     InboxStatus = "invalid_published"     // published not RFC 3339

	// Reject reasons — semantic layer (require state to evaluate).
	InboxStatusUnknownActor        InboxStatus = "unknown_actor"          // actor does not resolve
	InboxStatusUnknownPeer         InboxStatus = "unknown_peer"           // sender instance not in federation_peers
	InboxStatusPeerDisabled        InboxStatus = "peer_disabled"          // peer known but disabled
	InboxStatusUnsharedObject      InboxStatus = "unshared_object"        // no current federation_shares row
	InboxStatusEncryptionRequired  InboxStatus = "encryption_required"    // plaintext over e2e-only peer link
	InboxStatusPlaintextTypeMismatch InboxStatus = "plaintext_type_mismatch" // decrypted type ≠ envelope type
	InboxStatusStaleRequest        InboxStatus = "stale_request"          // HTTP-Sig date out of window
	InboxStatusReplay              InboxStatus = "replay"                 // activity id already seen
	InboxStatusError               InboxStatus = "error"                  // internal error during processing
)

// KnownInboxStatuses is the closed catalogue (used by the
// migration's CHECK + by exhaustive switches that need to assert
// no value escaped).
var KnownInboxStatuses = map[InboxStatus]struct{}{
	InboxStatusPending:               {},
	InboxStatusProcessed:             {},
	InboxStatusInvalidContext:        {},
	InboxStatusUnsigned:              {},
	InboxStatusUnsupportedAlgorithm:  {},
	InboxStatusSigMalformed:          {},
	InboxStatusSigInvalid:            {},
	InboxStatusUnknownKey:            {},
	InboxStatusUnknownField:          {},
	InboxStatusInvalidType:           {},
	InboxStatusInvalidActor:          {},
	InboxStatusInvalidObject:         {},
	InboxStatusInvalidPublished:      {},
	InboxStatusUnknownActor:          {},
	InboxStatusUnknownPeer:           {},
	InboxStatusPeerDisabled:          {},
	InboxStatusUnsharedObject:        {},
	InboxStatusEncryptionRequired:    {},
	InboxStatusPlaintextTypeMismatch: {},
	InboxStatusStaleRequest:          {},
	InboxStatusReplay:                {},
	InboxStatusError:                 {},
}

// IsReject reports whether s is one of the rejection statuses (vs
// pending/processed). Useful for "should we audit this as a
// federation.activity.rejected event?" checks.
func (s InboxStatus) IsReject() bool {
	switch s {
	case InboxStatusPending, InboxStatusProcessed:
		return false
	}
	_, known := KnownInboxStatuses[s]
	return known
}

// OutboxStatus is the typed result of an outbox-side dispatch
// attempt. Maps to federation_outbox.status (Phase 1.22.D).
type OutboxStatus string

const (
	OutboxStatusQueued    OutboxStatus = "queued"
	OutboxStatusSent      OutboxStatus = "sent"
	OutboxStatusFailed    OutboxStatus = "failed"
	OutboxStatusCancelled OutboxStatus = "cancelled"
)

// KnownOutboxStatuses is the closed catalogue.
var KnownOutboxStatuses = map[OutboxStatus]struct{}{
	OutboxStatusQueued:    {},
	OutboxStatusSent:      {},
	OutboxStatusFailed:    {},
	OutboxStatusCancelled: {},
}

// Valid reports whether s is in the closed catalogue.
func (s OutboxStatus) Valid() bool {
	_, ok := KnownOutboxStatuses[s]
	return ok
}
