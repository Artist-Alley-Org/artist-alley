// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
	// Add / Remove (AP §6.6 / §6.7) — used for collection
	// membership: Add(object→target=collection) means "add the
	// object to the collection"; Remove is the inverse. Added in
	// migration 00050 alongside the collections handler wiring.
	ActivityAdd    ActivityType = "Add"
	ActivityRemove ActivityType = "Remove"

	// Custom artist-alley activities — the load-bearing extension
	// set. Every `aa:*` activity MUST gate on a current
	// federation_shares row (Phase 1.22.C).
	ActivityAAShare   ActivityType = "aa:Share"
	ActivityAAUnshare ActivityType = "aa:Unshare"
	// ActivityAARevokeShare is RESERVED per the 1.22.C design
	// proposal §12.5 #3. v1 implementations MUST treat any inbound
	// aa:RevokeShare as an aa:Unshare (forward-compat parsing).
	// Distinct semantic (scope-narrow-without-gap) ships in a
	// future moderation phase. The constant is present so the
	// catalogue is forward-compatible; the inbox dispatcher maps
	// it to the Unshare handler.
	ActivityAARevokeShare        ActivityType = "aa:RevokeShare"
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
	ActivityAdd:                  {},
	ActivityRemove:               {},
	ActivityAAShare:              {},
	ActivityAAUnshare:            {},
	ActivityAARevokeShare:        {},
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

// --- Phase 1.22.I-c per-user encryption-key advertisement ------------

// PropEncryptionPublicKey is the envelope-extra property name
// that carries the sender's current X25519 public key on every
// outbound envelope. Receivers parse + persist via
// federation/remote.Handler.SetEncryptionKey so I-e's outbox
// encryption + I-f's inbox decryption have a known recipient key
// to dispatch against.
//
// The field is OPTIONAL — pre-1.22.I-c peers won't emit it, and
// post-1.22.I-c senders may omit it for system-generated activities
// without an attributable actor (rare). The I-g sender-refusal
// flow handles the recipient-side "no key yet" case.
const PropEncryptionPublicKey = "aa:encryptionPublicKey"

// TypeX25519PublicKey is the discriminator inside the
// aa:encryptionPublicKey block. Single value in v1; the
// discriminator exists so a future Hybrid PQ KEM algorithm
// (Curve25519 + ML-KEM-768 envelope) can land as a sibling type
// without breaking parsers.
const TypeX25519PublicKey = "aa:X25519PublicKey"

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

// ObjectVisibility is the 4-tier closed catalogue for object
// visibility per the 1.22.C design proposal §1. Mirrors the
// CHECK constraint added in migration 00056 on posts.visibility
// + collections.visibility (and any future shareable object).
//
//	private        — author + local admins only (no federation)
//	org-only       — local users with ACL access (no federation)
//	followers      — federated via Accept(Follow) per-user gates
//	explicit-share — federated via per-recipient federation_shares
//
// `public` is reserved for a future public-fediverse phase; v1
// implementations MUST reject it at the write API per the
// reviewer's locked-in answer (§12.5 #5 of the proposal). The
// constant ObjectVisibilityPublic is INTENTIONALLY absent.
type ObjectVisibility string

const (
	ObjectVisibilityPrivate       ObjectVisibility = "private"
	ObjectVisibilityOrgOnly       ObjectVisibility = "org-only"
	ObjectVisibilityFollowers     ObjectVisibility = "followers"
	ObjectVisibilityExplicitShare ObjectVisibility = "explicit-share"
)

// Valid reports whether v is in the closed catalogue. `public` is
// NOT considered valid at v1 (reserved value).
func (v ObjectVisibility) Valid() bool {
	switch v {
	case ObjectVisibilityPrivate, ObjectVisibilityOrgOnly,
		ObjectVisibilityFollowers, ObjectVisibilityExplicitShare:
		return true
	}
	return false
}

// IsFederated reports whether the tier flows content across the
// federation boundary. private + org-only stay local.
func (v ObjectVisibility) IsFederated() bool {
	return v == ObjectVisibilityFollowers || v == ObjectVisibilityExplicitShare
}

// ShareScope is the 4-rung ladder of access scopes per the
// 1.22.C design proposal §1 + §12.5 #1. Mirrors the CHECK
// constraint on federation_shares.scope.
//
// Ordering: view < comment < annotate < remix.
//
//	view     — read-only; can Like / Announce
//	comment  — view + can Create(Note) (comments)
//	annotate — comment + can aa:Annotation (whiteboards, text annotations)
//	remix    — annotate + can incorporate shared assets into
//	           own posts/collections/workspaces/brand_kits on
//	           recipient instance. Original NEVER modified.
//
// Future fifth scope `edit` reserved for cross-instance edit of
// the original (lands when that becomes a real ask). Not v1.
type ShareScope string

const (
	ShareScopeView     ShareScope = "view"
	ShareScopeComment  ShareScope = "comment"
	ShareScopeAnnotate ShareScope = "annotate"
	ShareScopeRemix    ShareScope = "remix"
)

// Valid reports whether s is in the closed catalogue.
func (s ShareScope) Valid() bool {
	switch s {
	case ShareScopeView, ShareScopeComment, ShareScopeAnnotate, ShareScopeRemix:
		return true
	}
	return false
}

// Rank returns an integer for ordered comparison: view=1 < comment=2
// < annotate=3 < remix=4. Zero on invalid (the caller should
// check Valid first; Rank is the comparison helper).
func (s ShareScope) Rank() int {
	switch s {
	case ShareScopeView:
		return 1
	case ShareScopeComment:
		return 2
	case ShareScopeAnnotate:
		return 3
	case ShareScopeRemix:
		return 4
	}
	return 0
}

// Covers reports whether s is sufficient for a required scope.
// Used by the inbox filter: e.g. an `annotate` share covers a
// `comment` activity, but a `view` share does NOT cover a
// `comment` activity.
func (s ShareScope) Covers(required ShareScope) bool {
	return s.Rank() >= required.Rank() && s.Rank() > 0 && required.Rank() > 0
}

// ShareObjectKind is the closed catalogue of shareable object
// kinds per the 1.22.C design proposal §2.2. Mirrors the CHECK
// constraint on federation_shares.object_kind.
//
//	asset, post, collection — current shareable objects
//	workspace, brand_kit    — future containers (tables land later;
//	                           enum value present now so the
//	                           federation_shares schema is
//	                           forward-compatible)
//	user                    — followers: a share row with
//	                           object_kind=user IS the follower
//	                           relationship (no separate followers
//	                           table).
type ShareObjectKind string

const (
	ShareObjectKindAsset      ShareObjectKind = "asset"
	ShareObjectKindPost       ShareObjectKind = "post"
	ShareObjectKindCollection ShareObjectKind = "collection"
	ShareObjectKindWorkspace  ShareObjectKind = "workspace"
	ShareObjectKindBrandKit   ShareObjectKind = "brand_kit"
	ShareObjectKindUser       ShareObjectKind = "user"
)

// Valid reports whether k is in the closed catalogue.
func (k ShareObjectKind) Valid() bool {
	switch k {
	case ShareObjectKindAsset, ShareObjectKindPost, ShareObjectKindCollection,
		ShareObjectKindWorkspace, ShareObjectKindBrandKit, ShareObjectKindUser:
		return true
	}
	return false
}

// PublishStatus is the state machine for publishing THIS instance
// to a federation directory per migration 00054 + the publish
// flow in docs/spec/federation-directory/v1.md §"POST /v1/register".
//
//	not_published    — fresh row; we've never tried to be listed
//	pending_dns      — challenge issued; operator must add the
//	                   TXT record we showed them
//	pending_register — DNS visible (or operator clicked anyway);
//	                   /v1/register POST is in flight
//	listed           — directory accepted us; publish_listing_id
//	                   populated
//	failed           — any step failed; publish_last_error populated
type PublishStatus string

const (
	PublishStatusNotPublished    PublishStatus = "not_published"
	PublishStatusPendingDNS      PublishStatus = "pending_dns"
	PublishStatusPendingRegister PublishStatus = "pending_register"
	PublishStatusListed          PublishStatus = "listed"
	PublishStatusFailed          PublishStatus = "failed"
)

// Valid reports whether s is in the closed catalogue.
func (s PublishStatus) Valid() bool {
	switch s {
	case PublishStatusNotPublished, PublishStatusPendingDNS,
		PublishStatusPendingRegister, PublishStatusListed, PublishStatusFailed:
		return true
	}
	return false
}

// PeerStatus is the handshake state machine for a federation_peers
// row per migration 00052 + docs/spec/federation/v1.md §11.
// Mirrors the CHECK constraint added in that migration.
type PeerStatus string

const (
	// PeerStatusPendingOutbound — we initiated the handshake;
	// awaiting the peer's admin confirmation.
	PeerStatusPendingOutbound PeerStatus = "pending_outbound"

	// PeerStatusPendingInbound — peer initiated; awaiting OUR
	// admin's manual review + accept (TOFU + explicit confirm).
	PeerStatusPendingInbound PeerStatus = "pending_inbound"

	// PeerStatusConnected — both sides confirmed; full federation
	// traffic allowed (gated additionally by enabled + tier +
	// federation_shares).
	PeerStatusConnected PeerStatus = "connected"
)

// KnownPeerStatuses is the closed catalogue.
var KnownPeerStatuses = map[PeerStatus]struct{}{
	PeerStatusPendingOutbound: {},
	PeerStatusPendingInbound:  {},
	PeerStatusConnected:       {},
}

// Valid reports whether s is in the closed catalogue.
func (s PeerStatus) Valid() bool {
	_, ok := KnownPeerStatuses[s]
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

// AtLeast is an alias for Covers (see above). Kept as a separate
// method name because some early callers used .AtLeast(view) and
// the alternative spelling reads more naturally in some contexts
// ("does this share grant at-least view?").
func (s ShareScope) AtLeast(other ShareScope) bool {
	return s.Covers(other)
}

// KnownShareScopes mirrors the four-entry catalogue as a map for
// the .Valid lookup. Kept compatible with earlier code that
// iterated this set + with the vocab_test invariants.
var KnownShareScopes = map[ShareScope]struct{}{
	ShareScopeView:     {},
	ShareScopeComment:  {},
	ShareScopeAnnotate: {},
	ShareScopeRemix:    {},
}

// ShareScopeOrdered is the elevation-ordered list — earlier
// entries grant strictly less than later ones. `view` < `comment`
// < `annotate` < `remix`. Used by access-check helpers + by the
// vocab_test drift detector.
var ShareScopeOrdered = []ShareScope{
	ShareScopeView,
	ShareScopeComment,
	ShareScopeAnnotate,
	ShareScopeRemix,
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
	InboxStatusUnknownActor           InboxStatus = "unknown_actor"            // actor does not resolve
	InboxStatusUnknownPeer            InboxStatus = "unknown_peer"             // sender instance not in federation_peers
	InboxStatusPeerDisabled           InboxStatus = "peer_disabled"            // peer known but disabled
	InboxStatusUnknownObject          InboxStatus = "unknown_object"           // object URL does not resolve to a local row (wrong host OR unknown UUID)
	InboxStatusUnsharedObject         InboxStatus = "unshared_object"          // local row exists but no current federation_shares grant
	InboxStatusEnvelopeSigMissing     InboxStatus = "envelope_sig_missing"     // structural: signature field absent/malformed (distinct from SigInvalid which is crypto failure with present-but-bad)
	InboxStatusEncryptionRequired     InboxStatus = "encryption_required"      // SENDER violated a MUST-encrypt rule (inverse of EncryptionNotSupported)
	InboxStatusEncryptionNotSupported InboxStatus = "encryption_not_supported" // RECEIVER hasn't shipped 1.22.I X25519 decode yet (inverse of EncryptionRequired)
	InboxStatusPlaintextTypeMismatch  InboxStatus = "plaintext_type_mismatch"  // decrypted type ≠ envelope type
	InboxStatusDecryptFailed          InboxStatus = "decrypt_failed"           // 1.22.I-f: walked every retained receiver key + every attempt's nacl/box.Open returned !ok — tamper, corruption, or sender used a recipient key version we've fully aged out
	InboxStatusStaleRequest           InboxStatus = "stale_request"            // HTTP-Sig date out of window
	InboxStatusReplay                 InboxStatus = "replay"                   // activity id already seen
	InboxStatusError                  InboxStatus = "error"                    // internal error during processing
)

// KnownInboxStatuses is the closed catalogue (used by the
// migration's CHECK + by exhaustive switches that need to assert
// no value escaped).
var KnownInboxStatuses = map[InboxStatus]struct{}{
	InboxStatusPending:                {},
	InboxStatusProcessed:              {},
	InboxStatusInvalidContext:         {},
	InboxStatusUnsigned:               {},
	InboxStatusUnsupportedAlgorithm:   {},
	InboxStatusSigMalformed:           {},
	InboxStatusSigInvalid:             {},
	InboxStatusUnknownKey:             {},
	InboxStatusUnknownField:           {},
	InboxStatusInvalidType:            {},
	InboxStatusInvalidActor:           {},
	InboxStatusInvalidObject:          {},
	InboxStatusInvalidPublished:       {},
	InboxStatusUnknownActor:           {},
	InboxStatusUnknownPeer:            {},
	InboxStatusPeerDisabled:           {},
	InboxStatusUnknownObject:          {},
	InboxStatusUnsharedObject:         {},
	InboxStatusEnvelopeSigMissing:     {},
	InboxStatusEncryptionRequired:     {},
	InboxStatusEncryptionNotSupported: {},
	InboxStatusPlaintextTypeMismatch:  {},
	InboxStatusDecryptFailed:          {},
	InboxStatusStaleRequest:           {},
	InboxStatusReplay:                 {},
	InboxStatusError:                  {},
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
