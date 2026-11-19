// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Recipient resolver per the 1.22.D design proposal §3.2.
// Phase 1.22.D-b-2.
//
// Given an activity (verb + target object + author + visibility),
// the resolver returns the set of (peer, target_user_url)
// recipients the outbox should fan out to. The dispatcher
// inserts one federation_outbox row per resolved Recipient.
//
// The explicit-share recipient set (also used by the public tier)
// is NOT cached: every resolution reads the authoritative rows,
// federation_shares JOIN federation_peers, through the partial index
// federation_shares_delivery_idx (object_kind, object_id) WHERE
// revoked_at IS NULL. The answer is a security decision (which peers
// may receive an activity), so it must be fail-closed against every
// way it can change:
//
//   - a grant must be seen by the very next new activity;
//   - a revoke, a natural expiry (expires_at passes; nothing writes
//     a row), and a peer being disabled or leaving `connected` must
//     stop the very next new activity, in every process.
//
// An earlier per-object LRU (`federation_shares.by_object`) had no
// TTL and its invalidation hook had no production caller, so a share
// granted after an object's first activity was never seen and a
// revoked peer kept receiving until the process restarted (#1233,
// live-reproduced). Expiry and peer state change without any write
// event a cache could hook, so the fix is to not cache this answer.
// The cost is one indexed read per dispatched activity whose tier is
// explicit-share or public.
//
// The followers tier still has a `federation_follows.by_actor` cache,
// but followers() resolves nothing yet (TODO(1.22.D-b-7)) and
// InvalidateFollowsByActor has no production caller either. Wiring
// the followers tier MUST give that lookup the same coherence
// (or drop its cache) before it returns real recipients.
//
// Sender-side emission refusal per §3.9 / §5.5 addition 2:
// when target object's sensitivity tier is restricted or
// embargo, the resolver returns an EmissionSkipped result
// (with a reason) instead of a Recipient set. The dispatcher
// emits the federation.emission.skipped audit event + advances
// the cursor past the activity. NO retroactive dispatch when
// 1.22.I ships.

package outbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// Visibility is the five-tier visibility model of the
// posts.visibility / collections.visibility CHECK constraints
// (migration 00008 added `public`, #414 / #1176).
//
// Every tier the schema allows MUST have a case in Resolve: an
// unhandled value is a resolver error, and the dispatcher rolls the
// whole batch back on a resolver error without advancing its cursor,
// so one unhandled activity stalls every activity queued behind it
// (#1233).
type Visibility string

const (
	VisibilityPrivate       Visibility = "private"
	VisibilityOrgOnly       Visibility = "org-only"
	VisibilityFollowers     Visibility = "followers"
	VisibilityExplicitShare Visibility = "explicit-share"
	// VisibilityPublic is LOCAL read visibility: anyone who can
	// reach this instance may read the object. It is NOT a
	// federation audience. See the public case in Resolve.
	VisibilityPublic Visibility = "public"
)

// Sensitivity is the four-tier sensitivity model per ADR 0020.
// Only assets carry a sensitivity column; boot derives a post's
// tier from its attached assets (http.outboxSensitivityLookup).
type Sensitivity string

const (
	SensitivityPublic     Sensitivity = "public"
	SensitivityTeam       Sensitivity = "team"
	SensitivityRestricted Sensitivity = "restricted"
	SensitivityEmbargo    Sensitivity = "embargo"

	// SensitivityUnresolved is what the dispatcher records when it
	// could not learn an object's tier: no lookup wired, a lookup
	// error, or a kind the lookup does not cover (#1525). It is
	// not one of the four tiers and is never stored on an object.
	// It requires encryption exactly like restricted, so an
	// unknown tier is never delivered in plaintext.
	SensitivityUnresolved Sensitivity = "unresolved"
)

// Recipient is one resolved outbox-row destination.
type Recipient struct {
	PeerID         uuid.UUID
	TargetUserURL  string // empty = broadcast (rare)
	Scope          string // view / comment / annotate / remix per ADR 0020
	EncryptionTier string // none / nacl-box (current tier per peer)
	// Sensitivity, when set, is a tier this recipient's row must be sent
	// at no less strictly than: an Undo inherits the tier its Like was
	// queued at for that recipient (#1572). The dispatcher stamps the
	// stricter of it and the activity's own tier.
	Sensitivity Sensitivity
}

// Result is the resolver's typed return.
type Result struct {
	Recipients []Recipient
	// Skipped is non-empty when the activity was emission-
	// refused. The dispatcher emits federation.emission.skipped
	// audit with this reason instead of inserting outbox rows.
	Skipped SkippedReason
}

// SkippedReason mirrors spec §12.3.
type SkippedReason string

const (
	SkippedNone                              SkippedReason = ""
	SkippedEncryptionRequiredButNotSupported SkippedReason = "encryption_required_but_not_supported"
	SkippedRecipientSetEmpty                 SkippedReason = "recipient_set_empty"
	SkippedDefederationInProgress            SkippedReason = "defederation_in_progress"
	// SkippedActivityNotFederated: the activity type is not one v1
	// federates (federation.Federated, spec §12.3, #1580).
	SkippedActivityNotFederated SkippedReason = "activity_not_federated"
	// SkippedProfileNotNegotiated: the activity is profile-gated (an
	// aa:Share / aa:Unshare needs share-lifecycle-1, #1571) and the
	// recipient peer's stored intersection lacks the profile. Nothing is
	// queued for that peer (spec §12.3, §16.3).
	SkippedProfileNotNegotiated SkippedReason = "profile_not_negotiated"
	// SkippedShareNotFound: a share activity whose share row no longer
	// exists (its peer was removed, which cascades the row), or that
	// names none. There is nobody to address.
	SkippedShareNotFound SkippedReason = "share_not_found"
	// SkippedNoReceivedGrant: a Like of another instance's object whose
	// named owning peer is not an enabled, connected peer of that
	// object's origin, or under whose grants (federation_inbound_grants)
	// no live, unexpired grant covers the liking user at view scope or
	// above (#1611). Nothing is queued.
	SkippedNoReceivedGrant SkippedReason = "no_received_grant"

	// 1.22.I-d per-recipient capability gate. Emitted by
	// federation.emission.skipped when a recipient peer hasn't
	// negotiated the required capability at handshake time.
	//
	// Today only [SkippedCapabilityMissingE2E] fires (the gate
	// is [peer.CapabilitySet.SupportsE2E], a triple-AND on
	// e2e-encrypted + nacl-box + x25519). The two granular
	// reasons are reserved for future fine-grained gates
	// (e.g. a peer that advertises e2e + x25519 but not nacl-box
	// because they only support a different envelope construction).
	SkippedCapabilityMissingE2E     SkippedReason = "capability_missing_e2e_encrypted"
	SkippedCapabilityMissingNaClBox SkippedReason = "capability_missing_nacl_box"
	SkippedCapabilityMissingX25519  SkippedReason = "capability_missing_x25519"
)

// Input is the resolver's typed argument. The dispatcher
// extracts these from the activities row before calling.
type Input struct {
	// The activity verb (Like / Create / aa:Share / etc.).
	// The resolver uses it to short-circuit verbs that never
	// federate (e.g. local-only verbs).
	Verb string

	// Target object the activity is about. The
	// (TargetKind, TargetID) pair keys the live (uncached)
	// federation_shares lookup for the explicit-share and public
	// tiers.
	TargetKind string
	TargetID   uuid.UUID

	// ShareID is the federation_shares row an aa:Share / aa:Unshare is
	// about (its payload's share_id). A share activity is addressed to
	// exactly that share's peer and recipient, never to every peer that
	// happens to hold a share of the same object (#1571).
	ShareID uuid.UUID

	// TargetURI is a comment Create's or a Like's target_uri, the
	// object on the instance that holds it, and RecipientActor the
	// activity's first `to` (the target's author there). A comment is
	// addressed to exactly the target's origin peer (#1604); a Like to
	// exactly TargetPeerID, the owning peer its emitter named, under a
	// live received grant (#1611). Either on a local object has no other
	// instance to go to.
	TargetURI      string
	TargetPeerID   uuid.UUID
	RecipientActor string

	// PriorActivityURI is the id of the earlier activity a follow-up
	// refers to, and PriorType that activity's type (#1572): for an Undo
	// its object and payload target_type (a Like), for a Delete or an
	// Update of a comment the payload's create_activity_uri (a Create). A
	// follow-up goes exactly where that activity went.
	PriorActivityURI string
	PriorType        string

	// Tx, when set, is what an Undo's recipients are read through: the
	// dispatcher's batch transaction, so an Undo sees the delivery rows
	// its Like was queued in earlier in the same, not yet committed,
	// batch (#1572). Nil reads through the pool.
	Tx Querier

	// AuthorRef + AuthorURI describe the local actor. The
	// follows.by_actor cache uses AuthorRef for follower-tier
	// resolution.
	AuthorRef int64
	AuthorURI string

	// Visibility + Sensitivity drive the resolution flow.
	// See the §3.2 visibility resolver for the per-tier
	// behaviour.
	Visibility  Visibility
	Sensitivity Sensitivity

	// RequiresEncryption (Phase 1.22.I-d) drives the per-recipient
	// capability gate. When true, the resolver consults
	// [Resolver.peerSupportsEncryption] for each recipient + drops
	// those that haven't negotiated e2e support. Dormant in
	// production traffic at 1.22.I-d (no caller sets it true yet);
	// 1.22.I-e flips the flag when restricted/embargo sensitivity
	// requires envelope encryption. Scenario 08 exercises the gate
	// directly via the synthetic-injection path.
	RequiresEncryption bool
}

// EncryptionSupported reports whether the local instance can
// encrypt outbound envelopes per ADR 0020. Production boot
// answers whether the at-rest master key that unwraps per-user
// X25519 keys is loaded (#1525). It is an instance-level
// question only: whether a given delivery is encrypted, sent in
// plaintext or refused is decided per recipient by the delivery
// Worker (ChoosePathFor over the peer's negotiated capability
// and the recipient key).
type EncryptionSupported func(ctx context.Context) bool

// PeerSupportsEncryption (Phase 1.22.I-d) returns whether a peer
// has negotiated end-to-end encryption support during the
// handshake. Boot wires it to a closure over the peer registry's
// ByID + the resulting Peer.Capabilities.SupportsE2E. nil-safe at
// the Resolver — when unwired the gate stays dormant + every
// recipient passes through.
//
// Lives as a typed callback rather than a direct peer-package
// import so the import edge stays one-directional (peer/handshake
// doesn't depend on outbox; outbox doesn't import peer).
type PeerSupportsEncryption func(ctx context.Context, peerID uuid.UUID) bool

// EmissionSkippedForPeerHook (Phase 1.22.I-d) is the audit
// callback for per-recipient capability skips. Wired to
// [audit.Recorder.FederationEmissionSkippedForPeer] at boot.
// nil-safe — when unwired the gate still drops recipients
// silently (the production-skip path keeps working; only the
// audit row is missing).
type EmissionSkippedForPeerHook func(ctx context.Context, peerID uuid.UUID, reason SkippedReason, verb string)

// Resolver turns an activity into its recipient set. Share-based
// tiers read the database on every call (see the file comment).
type Resolver struct {
	pool                *pgxpool.Pool
	followsByActor      *cache.Cache[[]Recipient]
	encryptionSupported EncryptionSupported

	// 1.22.I-d per-recipient capability gate. Both nil-safe.
	peerSupportsEncryption PeerSupportsEncryption
	emissionSkippedForPeer EmissionSkippedForPeerHook

	// receivedGrant answers whether a live grant this instance received
	// from peerID covers actorURI's use of objectURI at view scope
	// (#1611). nil refuses every Like of another instance's object.
	receivedGrant ReceivedGrantLookup
}

// Querier is the read surface priorRecipients needs: a pool or a
// transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ReceivedGrantLookup reports whether this instance holds, from peerID,
// a live grant covering actorURI (a local user) on objectURI at view
// scope or above. A refusal is (false, nil); an error is a lookup
// failure, which never authorises. Boot wires it to
// inboundgrants.Store.Allows.
type ReceivedGrantLookup func(ctx context.Context, peerID uuid.UUID, actorURI, objectURI string) (bool, error)

// SetReceivedGrants installs the received-grant check for Likes of
// another instance's objects (#1611).
func (r *Resolver) SetReceivedGrants(f ReceivedGrantLookup) { r.receivedGrant = f }

const (
	cacheDomainFollowsByActor = "federation_follows.by_actor"
)

// NewResolver wires the resolver. The follows cap is per the design
// proposal (5000 entries).
func NewResolver(pool *pgxpool.Pool, reg *cache.Registry, encSupported EncryptionSupported) *Resolver {
	if encSupported == nil {
		// Default: encryption NOT supported (1.22.I gate).
		encSupported = func(context.Context) bool { return false }
	}
	return &Resolver{
		pool:                pool,
		followsByActor:      cache.Register[[]Recipient](reg, cacheDomainFollowsByActor, 5000),
		encryptionSupported: encSupported,
	}
}

// SetPeerSupportsEncryption wires the per-recipient capability
// gate's lookup. Call once at boot AFTER the peer registry is
// constructed. Idempotent; nil-safe (passing nil disables the gate).
func (r *Resolver) SetPeerSupportsEncryption(f PeerSupportsEncryption) {
	r.peerSupportsEncryption = f
}

// SetEmissionSkippedForPeer wires the per-recipient audit hook
// that fires when the capability gate drops a recipient. Call
// once at boot. Idempotent; nil-safe.
func (r *Resolver) SetEmissionSkippedForPeer(f EmissionSkippedForPeerHook) {
	r.emissionSkippedForPeer = f
}

// InvalidateFollowsByActor is the hook the follows path is meant to
// call on Accept(Follow) / Undo(Follow) commits. It has no production
// caller yet (the followers tier is unwired, TODO(1.22.D-b-7)).
func (r *Resolver) InvalidateFollowsByActor(ctx context.Context, actorRef int64) error {
	if r == nil {
		return nil
	}
	return r.followsByActor.Invalidate(ctx, strconv.FormatInt(actorRef, 10))
}

// Resolve returns the recipient set for an activity per §3.2.
func (r *Resolver) Resolve(ctx context.Context, in Input) (Result, error) {
	// §3.9 sender-side emission refusal: restricted / embargo
	// (and unresolved, #1525) objects require encrypted delivery.
	// When this instance cannot encrypt at all we refuse to
	// enqueue + the dispatcher emits federation.emission.skipped
	// + advances cursor. When it can, the rows are enqueued with
	// their tier and the delivery Worker encrypts per recipient
	// or refuses that recipient (never plaintext).
	if (in.Sensitivity == SensitivityRestricted || in.Sensitivity == SensitivityEmbargo ||
		in.Sensitivity == SensitivityUnresolved) &&
		!r.encryptionSupported(ctx) {
		return Result{Skipped: SkippedEncryptionRequiredButNotSupported}, nil
	}

	// Share lifecycle (#1571): addressed by the share row, never by the
	// object's visibility tier. The share row IS the audience: an
	// explicit grant its owner (or system.admin) made to that one peer
	// and recipient, checked when it was made. Visibility is a local
	// read tier, and only posts have one (an asset or collection
	// resolves to private), so gating on it would drop every asset
	// share; and an aa:Unshare that is not delivered leaves a live grant
	// at the peer. The sensitivity refusal above still applies to both.
	if isShareVerb(in.Verb) {
		return r.shareRecipient(ctx, in)
	}
	if in.Verb == string(federation.ActivityCreate) && in.TargetKind == "comment" {
		return r.commentRecipient(ctx, in)
	}
	// A Like goes to the owner of the liked object only (#1611), never
	// by the visibility of a local object: a Like of a local object has
	// no other instance to go to.
	if in.Verb == string(federation.ActivityLike) {
		return r.likeRecipient(ctx, in)
	}
	// A follow-up (an Undo of a Like, a Delete or an Update of a comment)
	// goes where the activity it refers to went (#1572), never by a
	// visibility tier.
	switch federation.ActivityType(in.Verb) {
	case federation.ActivityUndo, federation.ActivityDelete, federation.ActivityUpdate:
		return r.priorRecipients(ctx, in)
	}

	switch in.Visibility {
	case VisibilityPrivate, VisibilityOrgOnly:
		// Zero recipients — local-only. Emit skipped audit
		// so operators can confirm a "why didn't this
		// federate?" investigation matches a known reason.
		return Result{Skipped: SkippedRecipientSetEmpty}, nil

	case VisibilityFollowers:
		recipients, err := r.followers(ctx, in.AuthorRef)
		if err != nil {
			return Result{}, err
		}
		recipients = r.applyCapabilityGate(ctx, in, recipients)
		if len(recipients) == 0 {
			return Result{Skipped: SkippedRecipientSetEmpty}, nil
		}
		return Result{Recipients: recipients}, nil

	case VisibilityExplicitShare, VisibilityPublic:
		// `public` federates EXACTLY like `explicit-share`: only to
		// recipients holding a live explicit share of this object
		// (unrevoked, unexpired, on an enabled + connected peer),
		// and to nobody else (#1233).
		//
		// Public read visibility says who may read the object on
		// THIS instance. It is not a grant to any peer, and it is
		// never a reason to broadcast to every paired peer: pairing
		// is not consent to receive a studio's content. So a public
		// object with no live share resolves to an empty set, which
		// skips the activity and lets the dispatcher advance its
		// cursor, instead of the resolver error it used to be.
		recipients, err := r.explicitShares(ctx, in.TargetKind, in.TargetID)
		if err != nil {
			return Result{}, err
		}
		recipients = r.applyCapabilityGate(ctx, in, recipients)
		if len(recipients) == 0 {
			return Result{Skipped: SkippedRecipientSetEmpty}, nil
		}
		return Result{Recipients: recipients}, nil
	}

	// A value outside the schema's CHECK (or a tier added there
	// without a case above) stays an error: guessing an audience for
	// an unknown tier could leak content.
	return Result{}, fmt.Errorf("unsupported visibility: %q", in.Visibility)
}

func isShareVerb(v string) bool {
	switch federation.ActivityType(v) {
	case federation.ActivityAAShare, federation.ActivityAAUnshare, federation.ActivityAARevokeShare:
		return true
	}
	return false
}

// shareRecipient resolves a share activity to its share row's peer and
// recipient, read live: the peer enabled and connected, and
// share-lifecycle-1 in its stored intersection, or the recipient is
// skipped with profile_not_negotiated (audited per peer). The share row
// is read whether or not it has been revoked since: the aa:Unshare needs
// it, and an aa:Share followed by its aa:Unshare is applied in order by
// the receiver from their references.
func (r *Resolver) shareRecipient(ctx context.Context, in Input) (Result, error) {
	if in.ShareID == (uuid.UUID{}) {
		// Not an error: a resolver error rolls the whole batch back and
		// stalls every activity behind it (#1233). Nobody to address.
		return Result{Skipped: SkippedShareNotFound}, nil
	}
	var (
		peerID      pgtype.UUID
		targetURL   string
		scope       string
		encryption  string
		usable      bool
		hasLifecyle bool
	)
	err := r.pool.QueryRow(ctx, `
		SELECT s.peer_id, COALESCE(s.target_user_url, ''), s.scope, p.encryption_policy,
		       (p.enabled AND p.status = 'connected'),
		       p.capabilities ? $2
		  FROM federation_shares s
		  JOIN federation_peers p ON p.id = s.peer_id
		 WHERE s.id = $1
	`, in.ShareID, federation.ProfileShareLifecycle1).Scan(&peerID, &targetURL, &scope, &encryption, &usable, &hasLifecyle)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{Skipped: SkippedShareNotFound}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("resolve share recipient: %w", err)
	}
	if !usable {
		return Result{Skipped: SkippedRecipientSetEmpty}, nil
	}
	id := uuid.UUID(peerID.Bytes)
	if !hasLifecyle {
		if r.emissionSkippedForPeer != nil {
			r.emissionSkippedForPeer(ctx, id, SkippedProfileNotNegotiated, in.Verb)
		}
		return Result{Skipped: SkippedProfileNotNegotiated}, nil
	}
	recipients := r.applyCapabilityGate(ctx, in, []Recipient{{
		PeerID: id, TargetUserURL: targetURL, Scope: scope, EncryptionTier: encryption,
	}})
	if len(recipients) == 0 {
		return Result{Skipped: SkippedRecipientSetEmpty}, nil
	}
	return Result{Recipients: recipients}, nil
}

// commentRecipient addresses a comment Create to the paired peer that
// is the origin of its target (#1604, spec v1 §7.5.5): enabled and
// connected, matched by origin, never by visibility (a comment has no
// visibility tier of its own). The receiver decides, from its own
// shares, whether the comment is admitted (#1570). A target on this
// instance, or on no paired peer, has nobody to send to.
func (r *Resolver) commentRecipient(ctx context.Context, in Input) (Result, error) {
	target := federation.OriginOf(in.TargetURI)
	if target == "" {
		return Result{Skipped: SkippedRecipientSetEmpty}, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT id, instance_url, encryption_policy FROM federation_peers
		WHERE enabled = TRUE AND status = 'connected'`)
	if err != nil {
		return Result{}, fmt.Errorf("resolve comment recipient: %w", err)
	}
	defer rows.Close()
	var out []Recipient
	for rows.Next() {
		var id pgtype.UUID
		var instanceURL, enc string
		if err := rows.Scan(&id, &instanceURL, &enc); err != nil {
			return Result{}, err
		}
		if federation.OriginOf(instanceURL) != target {
			continue
		}
		rec := Recipient{PeerID: uuid.UUID(id.Bytes), EncryptionTier: enc}
		if federation.OriginOf(in.RecipientActor) == target {
			rec.TargetUserURL = in.RecipientActor
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	out = r.applyCapabilityGate(ctx, in, out)
	if len(out) == 0 {
		return Result{Skipped: SkippedRecipientSetEmpty}, nil
	}
	return Result{Recipients: out}, nil
}

// likeRecipient addresses a Like of another instance's object (#1611,
// spec v1 §7.5.6) to exactly the peer its emitter named as the owner,
// and only when all of these hold, checked before anything is queued:
//
//   - the Like names a target (target_uri) and an owning peer
//     (target_peer_id); a Like of a local object has neither and stays
//     local;
//   - that peer is enabled and connected, and its instance is the
//     target's origin (a peer is never inferred from the target's
//     host, and a named peer of another origin is refused);
//   - a live, unexpired grant received from that peer covers the liking
//     user on that object at view scope or above (federation_inbound_
//     grants, #1571): a user-specific grant covers only its user, and an
//     ended, superseded, expired or disabled-peer grant covers nobody.
//
// The owner re-authorises the Like against its own shares on receipt
// (#1570); this check keeps a Like from being sent where no grant says
// the object was shared, so nothing about a private object leaves.
func (r *Resolver) likeRecipient(ctx context.Context, in Input) (Result, error) {
	origin := federation.OriginOf(in.TargetURI)
	if origin == "" || in.TargetPeerID == (uuid.UUID{}) {
		return Result{Skipped: SkippedRecipientSetEmpty}, nil
	}
	var instanceURL, enc string
	var usable bool
	err := r.pool.QueryRow(ctx, `SELECT instance_url, encryption_policy, (enabled AND status = 'connected')
		FROM federation_peers WHERE id = $1`, in.TargetPeerID).Scan(&instanceURL, &enc, &usable)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{Skipped: SkippedNoReceivedGrant}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("resolve like recipient: %w", err)
	}
	if !usable || federation.OriginOf(instanceURL) != origin || r.receivedGrant == nil {
		return Result{Skipped: SkippedNoReceivedGrant}, nil
	}
	ok, err := r.receivedGrant(ctx, in.TargetPeerID, in.AuthorURI, in.TargetURI)
	if err != nil {
		return Result{}, fmt.Errorf("resolve like recipient: received grant: %w", err)
	}
	if !ok {
		return Result{Skipped: SkippedNoReceivedGrant}, nil
	}
	rec := Recipient{PeerID: in.TargetPeerID, EncryptionTier: enc}
	if federation.OriginOf(in.RecipientActor) == origin {
		rec.TargetUserURL = in.RecipientActor
	}
	out := r.applyCapabilityGate(ctx, in, []Recipient{rec})
	if len(out) == 0 {
		return Result{Skipped: SkippedRecipientSetEmpty}, nil
	}
	return Result{Recipients: out}, nil
}

// priorRecipients addresses a follow-up (#1572) to exactly the
// recipients of the earlier activity it refers to: an Undo of a Like
// (spec v1 §7.5.4) to that Like's, a Delete or an Update of a comment
// (§7.5.5) to that comment's Create's. An Update carries content, so
// keeping the Create's tier means it is sent encrypted or not at all,
// exactly like the comment it edits. They are the federation_outbox rows of that
// activity, which must be this instance's own, of that type, by the same
// actor (a Create of a comment for a Delete). Each recipient keeps the
// peer, the target user (whose key an encrypted original was sealed to)
// and the tier the original's row was queued at, so a retraction is
// never sent less protected than what it withdraws. A row that was
// refused or cancelled carried nothing to anyone and is not followed; a
// peer that is no longer enabled and connected is not sent to. No
// received grant or share is needed: the retraction withdraws the
// actor's own contribution.
//
// An original that was never queued for anyone (a Like or a comment of a
// local object, or one refused before queueing) has nobody to send the
// retraction to: the result is recipient_set_empty, audited, never a
// broadcast. Outbox rows are kept, not pruned (they go only with their
// activity or their peer), so the original's recipients are still on
// record when the retraction is dispatched; in.Tx makes the rows of an
// original queued earlier in the same batch visible.
func (r *Resolver) priorRecipients(ctx context.Context, in Input) (Result, error) {
	switch {
	case in.Verb == string(federation.ActivityUndo) && in.PriorType == string(federation.ActivityLike):
	case (in.Verb == string(federation.ActivityDelete) || in.Verb == string(federation.ActivityUpdate)) &&
		in.TargetKind == "comment" && in.PriorType == string(federation.ActivityCreate):
	default:
		return Result{Skipped: SkippedActivityNotFederated}, nil
	}
	if in.PriorActivityURI == "" || in.AuthorURI == "" {
		return Result{Skipped: SkippedRecipientSetEmpty}, nil
	}
	var q Querier = r.pool
	if in.Tx != nil {
		q = in.Tx
	}
	rows, err := q.Query(ctx, `
		SELECT DISTINCT o.peer_id, COALESCE(o.target_user_url, ''), COALESCE(o.sensitivity, ''),
		       p.encryption_policy, (p.enabled AND p.status = 'connected')
		  FROM activities a
		  JOIN federation_outbox o ON o.activity_id = a.id
		  JOIN federation_peers p ON p.id = o.peer_id
		 WHERE a.activity_uri = $1
		   AND a.activity_type = $3
		   AND (a.activity_type <> 'Create' OR a.object_kind = 'comment')
		   AND a.source = 'local'
		   AND a.actor_uri = $2
		   AND o.status IN ('queued', 'sent', 'failed')
		 ORDER BY 1, 2
	`, in.PriorActivityURI, in.AuthorURI, in.PriorType)
	if err != nil {
		return Result{}, fmt.Errorf("resolve follow-up recipients: %w", err)
	}
	defer rows.Close()
	var out []Recipient
	for rows.Next() {
		var id pgtype.UUID
		var targetURL, sensitivity, enc string
		var usable bool
		if err := rows.Scan(&id, &targetURL, &sensitivity, &enc, &usable); err != nil {
			return Result{}, err
		}
		if !usable {
			continue
		}
		rec := Recipient{PeerID: uuid.UUID(id.Bytes), TargetUserURL: targetURL, EncryptionTier: enc,
			Sensitivity: SensitivityUnresolved}
		if sensitivity != "" {
			rec.Sensitivity = Sensitivity(sensitivity)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	out = r.applyCapabilityGate(ctx, in, out)
	if len(out) == 0 {
		return Result{Skipped: SkippedRecipientSetEmpty}, nil
	}
	return Result{Recipients: out}, nil
}

// applyCapabilityGate (Phase 1.22.I-d) filters out recipients
// whose peer hasn't negotiated end-to-end encryption support when
// the activity requires it. Dormant when [Input.RequiresEncryption]
// is false (the default — 1.22.I-e flips it). Dormant when
// [peerSupportsEncryption] hook is unwired (the boot configuration
// with cap-checking disabled). Dropped recipients fire the
// per-peer audit via [emissionSkippedForPeer] when wired;
// otherwise drop silently — the I-g sender refusal pattern is the
// load-bearing decision, the audit is observability.
func (r *Resolver) applyCapabilityGate(ctx context.Context, in Input, recipients []Recipient) []Recipient {
	if !in.RequiresEncryption || r.peerSupportsEncryption == nil {
		return recipients
	}
	out := make([]Recipient, 0, len(recipients))
	for _, rec := range recipients {
		if r.peerSupportsEncryption(ctx, rec.PeerID) {
			out = append(out, rec)
			continue
		}
		if r.emissionSkippedForPeer != nil {
			r.emissionSkippedForPeer(ctx, rec.PeerID, SkippedCapabilityMissingE2E, in.Verb)
		}
	}
	return out
}

// followers reads + caches the follower-share recipients for
// the given actor. Keyed on actor user_ref.
//
// TODO(1.22.D-b-7): wire follower resolution against
// object_kind='user' federation_shares rows once the user
// table grows a uuid column (the share row stores
// object_id UUID; the user table is currently keyed on
// bigint ref only). Until then this returns empty, which
// the resolver maps to SkippedRecipientSetEmpty. The demo
// + the explicit-share visibility tier are unaffected;
// the followers tier just doesn't federate yet.
//
// The cache wiring is in place so this is a one-line query
// swap when the schema lands.
func (r *Resolver) followers(ctx context.Context, actorRef int64) ([]Recipient, error) {
	if actorRef == 0 {
		return nil, nil
	}
	key := strconv.FormatInt(actorRef, 10)
	if cached, ok := r.followsByActor.Get(key); ok {
		return cached, nil
	}
	out := []Recipient(nil)
	r.followsByActor.Add(key, out)
	return out, nil
}

// explicitShares reads the LIVE per-object share recipients for
// (kind, id) from the database on every call: unrevoked, unexpired
// (against NOW() at resolution time) and on an enabled, connected
// peer. Deliberately uncached (see the file comment): a cached answer
// would outlive a revoke, an expiry or a peer disable. Hit by the
// explicit-share and public visibility tiers.
func (r *Resolver) explicitShares(ctx context.Context, kind string, id uuid.UUID) ([]Recipient, error) {
	if kind == "" || id == (uuid.UUID{}) {
		return nil, nil
	}

	rows, err := r.pool.Query(ctx, `
		SELECT s.peer_id, COALESCE(s.target_user_url, ''), s.scope, p.encryption_policy
		FROM federation_shares s
		JOIN federation_peers p ON p.id = s.peer_id
		WHERE s.object_kind = $1
		  AND s.object_id   = $2
		  AND s.revoked_at IS NULL
		  AND (s.expires_at IS NULL OR s.expires_at > NOW())
		  AND p.enabled = TRUE
		  AND p.status = 'connected'
	`, kind, id)
	if err != nil {
		return nil, fmt.Errorf("resolve explicit shares: %w", err)
	}
	defer rows.Close()

	var out []Recipient
	for rows.Next() {
		var (
			peerID         pgtype.UUID
			targetURL      string
			scope          string
			encryptionTier string
		)
		if err := rows.Scan(&peerID, &targetURL, &scope, &encryptionTier); err != nil {
			return nil, err
		}
		out = append(out, Recipient{
			PeerID:         uuid.UUID(peerID.Bytes),
			TargetUserURL:  targetURL,
			Scope:          scope,
			EncryptionTier: encryptionTier,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ErrInvalidInput is returned for malformed Input shapes that
// indicate a programmer error rather than a runtime issue.
var ErrInvalidInput = errors.New("outbox.resolver: invalid input")
