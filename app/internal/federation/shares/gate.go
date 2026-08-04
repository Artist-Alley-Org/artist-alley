// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Inbox-filter decision function + per-object cache + container
// resolution per the 1.22.C design proposal §5. Phase 1.22.C-b.
//
// # The gate
//
// CanPeerAccess is the load-bearing function the 1.22.D inbox
// dispatcher calls per inbound activity. It returns an
// AccessDecision carrying:
//
//   - Allowed bool                  — yes/no
//   - Reason  RejectReason          — when Allowed=false, the
//                                     §5.3 taxonomy code
//   - Share   *Share                — the matching active share,
//                                     when Allowed=true (so the
//                                     caller can record share_id
//                                     in the activity row)
//
// # Caching
//
// Per-object active-share snapshot cache (peer.shares.by_object
// LRU). Hot path: a single inbound activity targeting object X
// hits the cache once + iterates the in-memory set for the
// per-peer/per-user/per-scope match.
//
// Cache key: "{kind}:{uuid}". Cache value: the slice of active
// Share rows for that object.
//
// Invalidation: every Registry.Insert + Registry.Revoke drops
// the affected object's cache slot. Cross-process via cache.Registry
// NOTIFY so federated replicas stay coherent.
//
// # Container resolution
//
// Per the design §3: a share on a Collection grants the scope
// on every asset in it transitively. Implementation: when a
// direct lookup for an Asset misses, we resolve containing
// Collection IDs (via collection_resources JOIN, single SQL
// query) + try the cached share-sets for each.
//
// # The transitive grant is bounded by what the grantor owned (#893)
//
// A container share confers scope on a member ONLY if the share's
// grantor could have shared that member directly — they own it,
// or they hold system.admin, which is the same pair of conditions
// AdminHandler.GrantFederationShare enforces on the grant path.
// Both questions route through one owner map; see owner.go.
//
// Without that bound the JOIN carried no constraint on who owns
// the member: the grant check protected the CONTAINER and nothing
// re-checked the CONTENTS. "Share the container" therefore meant
// "grant a federated peer scope over any object that happens to be
// in it" — and a peer, unlike a local reader, keeps its own copy of
// that decision.
//
// The reachability precondition already exists, contrary to the
// issue's framing: collections.AddCollectionResource authorises
// against the COLLECTION (canMutateCollection) and never against
// the asset, so any collection owner can already put an asset they
// do not own into a collection they do. What #882 adds is the
// affordance, not the possibility.
//
// # Caching, and what is deliberately NOT cached
//
// The per-object LRU below holds active share ROWS keyed
// "{kind}:{uuid}". The #893 constraint is evaluated AFTER that
// read, against member ownership loaded fresh per call, so the
// key still identifies exactly one value and needs no new
// invalidation trigger. Folding the member-ownership answer into
// the snapshot would break that: the same collection's cached
// entry would have to mean different things for different
// members, and a membership change (which touches no share row,
// so fires no invalidation) would leave a peer holding a stale
// grant. The extra per-call lookup is the price of that
// coherence, and it is paid only on the container-fallback path,
// which already costs a query.
//
// Workspaces deferred — the table doesn't exist yet; when it
// lands the lookup gets a third fallback step in this same
// function without changing the public surface. It will need the
// same member-ownership bound.

package shares

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// RejectReason is the closed taxonomy of "why was this activity
// rejected" codes per the 1.22.C design proposal §5.3. Surfaced
// in federation.activity.rejected audit events + returned to
// inbox callers so the dispatcher can render an admin-friendly
// message.
type RejectReason string

const (
	// RejectReasonNoShareRow — no active share for this object × peer pair.
	RejectReasonNoShareRow RejectReason = "no_share_row"

	// RejectReasonWrongUser — share exists but target_user_url
	// doesn't match the requesting user (the share is for someone
	// else on the same peer).
	RejectReasonWrongUser RejectReason = "wrong_user"

	// RejectReasonExpired — share row's expires_at is in the past.
	// Inbox-filter check fires per request even if the expiry
	// sweeper (1.22.C-d) hasn't yet processed the row.
	RejectReasonExpired RejectReason = "expired"

	// RejectReasonRevoked — share row's revoked_at is set.
	// Belt-and-suspenders: the partial indexes filter revoked rows
	// out so this should never fire from the FindActiveShare path;
	// guard remains for code reading via FindByID etc.
	RejectReasonRevoked RejectReason = "revoked"

	// RejectReasonInsufficientScope — share grants a lower scope
	// than the activity requires (e.g. share=view but activity
	// wants comment).
	RejectReasonInsufficientScope RejectReason = "insufficient_scope"

	// RejectReasonPeerDisabled — the federation_peers.enabled is
	// false; admin has kill-switched the peer.
	RejectReasonPeerDisabled RejectReason = "peer_disabled"

	// RejectReasonPeerNotConnected — the federation_peers.status
	// is not 'connected' (e.g. pending_inbound, pending_outbound).
	// Inbox only accepts from fully-paired peers.
	RejectReasonPeerNotConnected RejectReason = "peer_not_connected"

	// RejectReasonGrantorNotOwner — a container share covering
	// this object exists and matches the peer/user/scope, but its
	// grantor neither owns the object nor holds system.admin, so
	// the share confers nothing on it (#893). The most diagnostic
	// reason in the taxonomy: it says "the grant you are relying
	// on was never the grantor's to make", which is what an
	// operator needs to see rather than a bare no_share_row.
	RejectReasonGrantorNotOwner RejectReason = "grantor_not_owner"
)

// AccessDecision is the typed result of CanPeerAccess.
type AccessDecision struct {
	Allowed bool
	Reason  RejectReason // populated when Allowed=false
	Share   *Share       // populated when Allowed=true
}

// AccessRequest is the typed argument shape — five fields the
// inbox dispatcher passes per activity.
type AccessRequest struct {
	PeerID        uuid.UUID
	UserURL       string // requesting user's actor URL on the peer; "" if not user-scoped
	ObjectKind    federation.ShareObjectKind
	ObjectID      uuid.UUID
	RequiredScope federation.ShareScope

	// PeerEnabled + PeerConnected are passed in by the inbox
	// dispatcher (which has the peer row already, no need for us
	// to re-fetch). When EITHER is false we short-circuit to the
	// matching reject reason without touching the shares cache.
	PeerEnabled   bool
	PeerConnected bool
}

// CanPeerAccess returns the gate decision per the design §5.1.
// Resolution order:
//
//  1. Peer-level gates (enabled + connected) — short-circuit.
//  2. Required scope must be valid; bad scope is an internal
//     bug, not a peer issue. Return error.
//  3. Direct share lookup against (kind, id, peer, user, scope).
//  4. Container fallback for assets: any covering Collection
//     share that satisfies (peer, user, scope).
//  5. (Future: workspace fallback for collections — deferred
//     until the workspaces table exists.)
//
// Returns the FIRST allowing share found per the §5.1 ordering
// (specific user > broadcast; higher scope > lower scope). When
// nothing matches, returns RejectReasonNoShareRow (or a more
// specific reason if we found a share that ALMOST matched —
// wrong user / insufficient scope / expired).
func (r *Registry) CanPeerAccess(ctx context.Context, req AccessRequest) (AccessDecision, error) {
	if !req.RequiredScope.Valid() {
		return AccessDecision{}, fmt.Errorf("shares.CanPeerAccess: invalid required scope %q", req.RequiredScope)
	}
	if !req.ObjectKind.Valid() {
		return AccessDecision{}, fmt.Errorf("shares.CanPeerAccess: invalid object kind %q", req.ObjectKind)
	}

	// Peer-level short-circuits.
	if !req.PeerEnabled {
		return AccessDecision{Allowed: false, Reason: RejectReasonPeerDisabled}, nil
	}
	if !req.PeerConnected {
		return AccessDecision{Allowed: false, Reason: RejectReasonPeerNotConnected}, nil
	}

	// Direct lookup — covers the common case.
	direct, directReason, err := r.matchSharesForObject(ctx, req.ObjectKind, req.ObjectID, req)
	if err != nil {
		return AccessDecision{}, err
	}
	if direct != nil {
		return AccessDecision{Allowed: true, Share: direct}, nil
	}

	// Container fallback for assets: check any covering collection.
	if req.ObjectKind == federation.ShareObjectKindAsset {
		containerHit, containerReason, err := r.matchContainingCollectionShares(ctx, req.ObjectID, req)
		if err != nil {
			return AccessDecision{}, err
		}
		if containerHit != nil {
			return AccessDecision{Allowed: true, Share: containerHit}, nil
		}
		// Container reason beats direct reason when more
		// specific (e.g. container had insufficient_scope while
		// direct had no_share_row).
		if reasonSpecificity(containerReason) > reasonSpecificity(directReason) {
			directReason = containerReason
		}
	}

	// Nothing matched.
	if directReason == "" {
		directReason = RejectReasonNoShareRow
	}
	return AccessDecision{Allowed: false, Reason: directReason}, nil
}

// matchSharesForObject finds the best matching active share for
// (kind, id) given the requester. Returns nil + the "best-effort"
// reject reason when no match (so the caller can distinguish
// "no share at all" from "share exists but wrong user / wrong
// scope / expired").
func (r *Registry) matchSharesForObject(
	ctx context.Context,
	kind federation.ShareObjectKind,
	objectID uuid.UUID,
	req AccessRequest,
) (*Share, RejectReason, error) {
	candidates, err := r.activeSharesByObject(ctx, kind, objectID)
	if err != nil {
		return nil, "", err
	}
	if len(candidates) == 0 {
		return nil, RejectReasonNoShareRow, nil
	}
	return pickBestMatch(candidates, req)
}

// matchContainingCollectionShares is the container fallback for
// asset lookups. Resolves the asset's collection memberships +
// checks each — but only counts a container share whose grantor
// could have shared this asset directly (#893).
func (r *Registry) matchContainingCollectionShares(
	ctx context.Context,
	assetID uuid.UUID,
	req AccessRequest,
) (*Share, RejectReason, error) {
	containerIDs, err := New(r.Pool).FindContainingCollections(ctx, pgtype.UUID{Bytes: assetID, Valid: true})
	if err != nil {
		return nil, "", err
	}
	if len(containerIDs) == 0 {
		return nil, RejectReasonNoShareRow, nil
	}

	// Who owns the member? One lookup for the whole call, read
	// fresh (never cached — see the caching note in this file's
	// header). No resolvable owner is a denial, not an error:
	// nothing can be transitively re-shared on behalf of an owner
	// we cannot name.
	memberOwnerRef, ownerKnown, err := ObjectOwnerRef(ctx, r.Pool, federation.ShareObjectKindAsset, assetID)
	if err != nil {
		return nil, "", err
	}
	if !ownerKnown {
		return nil, RejectReasonGrantorNotOwner, nil
	}
	adminSeen := make(map[int64]bool, 1)

	// Best reason across all containers — if ANY container grants
	// access, return the matching share. If none do, return the
	// most-specific failure reason we saw.
	bestReason := RejectReasonNoShareRow
	for _, cid := range containerIDs {
		candidates, err := r.activeSharesByObject(ctx, federation.ShareObjectKindCollection, uuid.UUID(cid.Bytes))
		if err != nil {
			return nil, "", err
		}
		if len(candidates) == 0 {
			bestReason = bumpReason(bestReason, RejectReasonNoShareRow)
			continue
		}
		// Filter BEFORE pickBestMatch, not after: picking first
		// would let one unauthorised grantor's higher-ranked share
		// mask a lower-ranked share the owner themselves granted
		// on the same collection.
		kept := candidates[:0]
		for i := range candidates {
			authorized, err := r.grantorAuthority(ctx, candidates[i].GrantorUserRef, memberOwnerRef, adminSeen)
			if err != nil {
				return nil, "", err
			}
			if authorized {
				kept = append(kept, candidates[i])
			}
		}
		if len(kept) == 0 {
			// Shares exist on this container; none of them was the
			// grantor's to extend over this member.
			bestReason = bumpReason(bestReason, RejectReasonGrantorNotOwner)
			continue
		}
		share, reason, err := pickBestMatch(kept, req)
		if err != nil {
			return nil, "", err
		}
		if share != nil {
			return share, "", nil
		}
		bestReason = bumpReason(bestReason, reason)
	}
	return nil, bestReason, nil
}

// pickBestMatch iterates the active candidates for a single
// object + returns the highest-priority matching share. Match
// rules per the design §5.1:
//   - target_user_url NULL (broadcast) OR == req.UserURL
//   - scope sufficient (share.Scope.Covers(req.RequiredScope))
//   - revoked_at IS NULL (already filtered by the SQL caller)
//   - expires_at IS NULL OR in the future (we re-check here for
//     defense-in-depth — cache may be milliseconds stale)
//
// Priority when multiple match:
//   - Specific user beats broadcast
//   - Higher scope beats lower
//   - Among ties, deterministic by share UUID (stable test outcome)
//
// Returns the best match + zero reason on success. Returns nil +
// the most-specific failure reason on no-match (so the caller
// can surface "wrong_user" vs "insufficient_scope" vs "expired"
// instead of the catch-all "no_share_row").
func pickBestMatch(candidates []Share, req AccessRequest) (*Share, RejectReason, error) {
	var best *Share
	bestReason := RejectReasonNoShareRow
	now := nowFn()
	for i := range candidates {
		c := &candidates[i]
		if !c.Active(now) {
			// Cache might hold a row that ticked over expiry
			// between the load + this check; mark it expired so
			// the reason taxonomy is correct.
			if c.ExpiresAt.Valid {
				bestReason = bumpReason(bestReason, RejectReasonExpired)
			} else if c.RevokedAt.Valid {
				bestReason = bumpReason(bestReason, RejectReasonRevoked)
			}
			continue
		}
		// User match.
		userMatch := false
		if c.TargetUserURL == nil || *c.TargetUserURL == "" {
			userMatch = true // broadcast — any user on the peer
		} else if req.UserURL != "" && *c.TargetUserURL == req.UserURL {
			userMatch = true
		}
		if !userMatch {
			bestReason = bumpReason(bestReason, RejectReasonWrongUser)
			continue
		}
		// Scope match.
		if !c.Scope.Covers(req.RequiredScope) {
			bestReason = bumpReason(bestReason, RejectReasonInsufficientScope)
			continue
		}
		// This share matches. Prefer specific over broadcast +
		// higher scope over lower.
		if best == nil || rankMatch(c) > rankMatch(best) {
			best = c
		}
	}
	if best != nil {
		return best, "", nil
	}
	return nil, bestReason, nil
}

// rankMatch is the tiebreak key for pickBestMatch:
//
//	specific-user shares score above broadcast (10),
//	plus the scope rank (1-4),
//	plus a uuid-derived constant for deterministic ties.
func rankMatch(s *Share) int {
	score := s.Scope.Rank()
	if s.TargetUserURL != nil && *s.TargetUserURL != "" {
		score += 10
	}
	return score
}

// reasonSpecificity orders reject reasons by how diagnostic they
// are. "We found a share but wrong scope" is more specific than
// "we found nothing." Used to bubble the best reason out of the
// container-fallback path.
func reasonSpecificity(r RejectReason) int {
	switch r {
	case RejectReasonGrantorNotOwner:
		// Top of the ladder: a matching share was found and
		// deliberately not honoured. Nothing else in the taxonomy
		// tells the operator that.
		return 5
	case RejectReasonInsufficientScope:
		return 4
	case RejectReasonWrongUser:
		return 3
	case RejectReasonExpired:
		return 2
	case RejectReasonRevoked:
		return 2
	case RejectReasonNoShareRow:
		return 1
	}
	return 0
}

// bumpReason replaces curr with next if next is more specific.
func bumpReason(curr, next RejectReason) RejectReason {
	if reasonSpecificity(next) > reasonSpecificity(curr) {
		return next
	}
	return curr
}

// --- per-object cache ----------------------------------------------------

// activeSharesByObject returns the cached or freshly-loaded set
// of active shares for (kind, id). Single-key LRU per object.
func (r *Registry) activeSharesByObject(ctx context.Context, kind federation.ShareObjectKind, id uuid.UUID) ([]Share, error) {
	key := cacheKey(kind, id)
	if r.byObject != nil {
		if hit, ok := r.byObject.Get(key); ok {
			return append([]Share(nil), hit.Shares...), nil
		}
	}
	rows, err := New(r.Pool).ListActiveSharesByObject(ctx, ListActiveSharesByObjectParams{
		ObjectKind: string(kind),
		ObjectID:   pgtype.UUID{Bytes: id, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	shares := rowsToShares(rows)
	if r.byObject != nil {
		r.byObject.Add(key, byObjectSnapshot{Shares: shares})
	}
	return append([]Share(nil), shares...), nil
}

// invalidateObject drops the cached share-set for one object.
// Called from Insert + Revoke for the affected object.
func (r *Registry) invalidateObject(ctx context.Context, kind federation.ShareObjectKind, id uuid.UUID) {
	if r.byObject == nil {
		return
	}
	if err := r.byObject.Invalidate(ctx, cacheKey(kind, id)); err != nil && r.Logger != nil {
		r.Logger.LogAttrs(ctx, slog.LevelWarn, "shares.cache.invalidate.error",
			slog.String("err", err.Error()),
		)
	}
}

// cacheKey is the cache slot key. Stable string formatting so
// federated replicas using NOTIFY broadcast the same key.
func cacheKey(kind federation.ShareObjectKind, id uuid.UUID) string {
	return string(kind) + ":" + id.String()
}

// --- aa:RevokeShare normalization ----------------------------------------

// NormalizeInboundActivityType maps an incoming activity type to
// its v1 dispatcher equivalent per the design §12.5 #3. v1
// implementations MUST treat any inbound aa:RevokeShare as an
// aa:Unshare; this helper centralizes that mapping so future
// changes (when aa:RevokeShare gets its own semantic) only
// touch this function.
func NormalizeInboundActivityType(t federation.ActivityType) federation.ActivityType {
	if t == federation.ActivityAARevokeShare {
		return federation.ActivityAAUnshare
	}
	return t
}

// --- activity-to-scope mapping -------------------------------------------

// ActivityRequiredScope returns the minimum share scope required
// to admit an inbound activity per the design §5.4. Returns
// ShareScopeView as a safe default for verbs the caller hasn't
// modeled yet — the gate will allow the activity if a view-or-
// better share exists, which is the most permissive interpretation;
// callers that need stricter handling for unknown verbs check
// the returned ok=false flag.
func ActivityRequiredScope(t federation.ActivityType) (federation.ShareScope, bool) {
	switch t {
	case federation.ActivityLike, federation.ActivityAnnounce:
		return federation.ShareScopeView, true
	case federation.ActivityCreate:
		// Create defaults to "comment" because the most common
		// Create payload is a Note (comment). Whiteboard/annotation
		// Create payloads should go through ActivityAAAnnotation
		// which maps to annotate.
		return federation.ShareScopeComment, true
	case federation.ActivityAAAnnotation:
		return federation.ShareScopeAnnotate, true
	case federation.ActivityAAWorkflowTransition,
		federation.ActivityAAApprove,
		federation.ActivityAARequestChanges,
		federation.ActivityAAMarkReviewed:
		return federation.ShareScopeRemix, true
	case federation.ActivityDelete, federation.ActivityUpdate:
		// Recipient deleting/updating their OWN derivative work
		// only needs view on the original. Origin-side
		// Update/Delete of the original object always passes the
		// gate from the source (since the source IS the grantor).
		return federation.ShareScopeView, true
	case federation.ActivityUndo:
		// Undoing a prior activity inherits the prior's scope;
		// view is the safe minimum (you can always undo your own
		// Like).
		return federation.ShareScopeView, true
	case federation.ActivityFollow, federation.ActivityAccept, federation.ActivityReject:
		// Follow flow is handled outside the share gate (see
		// §9.3 of the design proposal). Returning view+ok=false
		// signals the dispatcher to skip the share check for
		// these verbs entirely.
		return federation.ShareScopeView, false
	case federation.ActivityBlock:
		// Block has no recipients per AP §6.9; share gate not
		// relevant.
		return federation.ShareScopeView, false
	case federation.ActivityAdd, federation.ActivityRemove:
		// Collection membership changes — gated against the TARGET
		// COLLECTION at remix scope.
		//
		// #893 note: the original rationale was "emitted by the
		// collection owner", and that is all this still checks —
		// an inbound Add carrying a FOREIGN object is authorised
		// by the collection's remix share and by nothing about the
		// object. The local equivalent has the same shape
		// (collections.AddCollectionResource authorises against
		// the collection, never the asset), so this is not a
		// federation-only gap and closing it belongs with #882
		// rather than here. The READ side is covered: a member the
		// grantor doesn't own confers no scope no matter how it
		// got into the collection.
		return federation.ShareScopeRemix, true
	case federation.ActivityAAShare, federation.ActivityAAUnshare, federation.ActivityAARevokeShare:
		// Share-flow activities are gated differently (the share
		// IS the grant). Dispatcher handles separately.
		return federation.ShareScopeView, false
	}
	// Unknown verb — return false so the dispatcher decides.
	return federation.ShareScopeView, false
}

// --- helpers -------------------------------------------------------------

// nowFn is a package-level var so tests can freeze the clock. In
// production it's time.Now; tests override per case.
var nowFn = defaultNow

func defaultNow() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now(), Valid: true}
}

var _ = errors.New // import-keepalive for any future sentinel-error addition
