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
// Two caches front the SQL JOINs per the §3.6 caching
// requirement:
//
//   shares.by_object  — keyed on (object_kind, object_id);
//                       hits the explicit-share visibility
//                       tier's hot path.
//   follows.by_actor  — keyed on (actor_user_ref); hits the
//                       followers visibility tier's hot path.
//                       Without this, every post by a popular
//                       local user does a federation_shares
//                       scan per dispatch — N+1 surface.
//
// Both caches:
//   - Use the project's cache.Registry pattern (LRU + cross-
//     process NOTIFY).
//   - Invalidate on the relevant write-side event (the share
//     or follow grant/revoke commit fires the NOTIFY).
//   - Are bounded (5000 + 5000 entries) so memory pressure
//     stays predictable in the walled-garden v1 scale.
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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// Visibility is the four-tier visibility model per the v1
// schema constraint on posts.visibility.
type Visibility string

const (
	VisibilityPrivate       Visibility = "private"
	VisibilityOrgOnly       Visibility = "org-only"
	VisibilityFollowers     Visibility = "followers"
	VisibilityExplicitShare Visibility = "explicit-share"
)

// Sensitivity is the four-tier sensitivity model per ADR 0020.
// Schema columns aren't applied to posts yet (pre-MVP) — the
// resolver treats all sensitivity values it sees as
// SensitivityPublic until the schema lands. When restricted /
// embargo columns ship, the resolver flips to refusing emission
// per §3.9.
type Sensitivity string

const (
	SensitivityPublic     Sensitivity = "public"
	SensitivityTeam       Sensitivity = "team"
	SensitivityRestricted Sensitivity = "restricted"
	SensitivityEmbargo    Sensitivity = "embargo"
)

// Recipient is one resolved outbox-row destination.
type Recipient struct {
	PeerID         uuid.UUID
	TargetUserURL  string // empty = broadcast (rare)
	Scope          string // view / comment / annotate / remix per ADR 0020
	EncryptionTier string // none / nacl-box (current tier per peer)
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
	// (TargetKind, TargetID) pair drives the
	// shares.by_object cache lookup for explicit-share rows.
	TargetKind string
	TargetID   uuid.UUID

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
// encrypt outbound envelopes per ADR 0020. v1.0 returns false
// (X25519 keypair-per-user ships in 1.22.I); v1.0+1 returns
// true. Wired as a field on the Resolver so 1.22.I flips one
// boolean instead of touching every call site.
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

// Resolver fronts the recipient-resolution SQL with the two
// caches per §3.6.
type Resolver struct {
	pool                *pgxpool.Pool
	sharesByObject      *cache.Cache[[]Recipient]
	followsByActor      *cache.Cache[[]Recipient]
	encryptionSupported EncryptionSupported

	// 1.22.I-d per-recipient capability gate. Both nil-safe.
	peerSupportsEncryption PeerSupportsEncryption
	emissionSkippedForPeer EmissionSkippedForPeerHook
}

const (
	cacheDomainSharesByObject = "federation_shares.by_object"
	cacheDomainFollowsByActor = "federation_follows.by_actor"
)

// NewResolver wires the resolver. cap sizes are per the
// design proposal — 5000 entries each.
func NewResolver(pool *pgxpool.Pool, reg *cache.Registry, encSupported EncryptionSupported) *Resolver {
	if encSupported == nil {
		// Default: encryption NOT supported (1.22.I gate).
		encSupported = func(context.Context) bool { return false }
	}
	return &Resolver{
		pool:                pool,
		sharesByObject:      cache.Register[[]Recipient](reg, cacheDomainSharesByObject, 5000),
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

// InvalidateSharesByObject is the cross-package hook the
// shares package calls on aa:Share / aa:Unshare commits +
// the share-expiry-sweeper firing. Drops the cached recipient
// set for the object so the next emission rebuilds from DB.
func (r *Resolver) InvalidateSharesByObject(ctx context.Context, objectKind string, objectID uuid.UUID) error {
	if r == nil {
		return nil
	}
	return r.sharesByObject.Invalidate(ctx, sharesKey(objectKind, objectID))
}

// InvalidateFollowsByActor is the cross-package hook the
// follows path calls on Accept(Follow) / Undo(Follow)
// commits.
func (r *Resolver) InvalidateFollowsByActor(ctx context.Context, actorRef int64) error {
	if r == nil {
		return nil
	}
	return r.followsByActor.Invalidate(ctx, strconv.FormatInt(actorRef, 10))
}

// Resolve returns the recipient set for an activity per §3.2.
func (r *Resolver) Resolve(ctx context.Context, in Input) (Result, error) {
	// §3.9 sender-side emission refusal: restricted / embargo
	// objects require encrypted delivery. Until 1.22.I lights
	// up X25519 we refuse to enqueue + the dispatcher emits
	// federation.emission.skipped + advances cursor.
	if (in.Sensitivity == SensitivityRestricted || in.Sensitivity == SensitivityEmbargo) &&
		!r.encryptionSupported(ctx) {
		return Result{Skipped: SkippedEncryptionRequiredButNotSupported}, nil
	}

	// Verb short-circuits: verbs that never federate at all
	// drop here. Currently empty — every verb in the v1
	// catalogue federates somewhere. Kept as a hook so future
	// local-only verbs can opt out without touching callers.

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

	case VisibilityExplicitShare:
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

	return Result{}, fmt.Errorf("unsupported visibility: %q", in.Visibility)
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

// explicitShares reads + caches the per-object share rows for
// (kind, id). Hit by the explicit-share visibility tier;
// also feeds the §5.1 containment expansion (assets inside
// shared collections) when the dispatcher walks parents.
func (r *Resolver) explicitShares(ctx context.Context, kind string, id uuid.UUID) ([]Recipient, error) {
	if kind == "" || id == (uuid.UUID{}) {
		return nil, nil
	}
	key := sharesKey(kind, id)
	if cached, ok := r.sharesByObject.Get(key); ok {
		return cached, nil
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
	r.sharesByObject.Add(key, out)
	return out, nil
}

func sharesKey(kind string, id uuid.UUID) string {
	return kind + ":" + id.String()
}

// ErrInvalidInput is returned for malformed Input shapes that
// indicate a programmer error rather than a runtime issue.
var ErrInvalidInput = errors.New("outbox.resolver: invalid input")
