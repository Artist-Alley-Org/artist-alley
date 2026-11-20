// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Admin HTTP surface for federation_shares — Phase 1.22.C-c.
// Three endpoints: list (with mutually-exclusive filters),
// grant (POST), revoke (DELETE). Gated on system.admin; per-
// object owner check happens at the grant path via the
// ObjectOwnerResolver callback (boot-wired so this package
// doesn't have to import every domain handler's package).
//
// Write-ahead-audit invariant (design proposal §7.2):
// federation.share.granted / .revoked audit rows commit in the
// SAME transaction as the share row + the aa:Share / aa:Unshare
// activity row. The activities writer's WithEmissionFn wraps the
// whole thing.

package shares

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ObjectOwnerResolver returns true when the supplied caller may
// grant shares on the supplied (kind, id). Boot wires an
// implementation that consults the per-domain owner columns
// (posts.author_user_ref, collections.owner_user_ref,
// assets.owner_user_ref). system.admin bypasses this check.
type ObjectOwnerResolver func(ctx context.Context, kind federation.ShareObjectKind, objectID uuid.UUID, caller *auth.Identity) (bool, error)

// PeerLookup returns peer info the share emit envelope needs
// (instance_url for addressing). Boot wires this to
// peer.Registry.ByID.
type PeerLookup func(ctx context.Context, id uuid.UUID) (PeerInfo, error)

// PeerInfo is the subset of peer.Peer the share grant path needs.
type PeerInfo struct {
	ID          uuid.UUID
	InstanceURL string
	Enabled     bool
	Connected   bool
}

// AdminHandler is the openapi-strict adapter for the three
// /admin/federation/shares endpoints + the defederation-preview
// endpoint (1.22.C-d).
type AdminHandler struct {
	registry      *Registry
	activities    *activities.Writer
	auditRec      *audit.Recorder
	resolveOwner  ObjectOwnerResolver
	lookupPeer    PeerLookup
	instanceURLFn func(ctx context.Context) string
	usernameFn    func(ctx context.Context, userRef int64) string

	// 1.22.C-d defederation-preview cross-package deps. Nil-safe;
	// the preview endpoint returns 500 if not wired. Boot calls
	// SetDefederationDeps once.
	pendingHandshakeCounter PendingHandshakeCounter
	suggestionCounter       SuggestionCounter
	peerDisplay             PeerDisplay
}

// NewAdminHandler wires the admin surface. All five callbacks
// are required (no nil-tolerance in production); tests inject
// stubs.
func NewAdminHandler(
	r *Registry,
	writer *activities.Writer,
	auditRec *audit.Recorder,
	resolveOwner ObjectOwnerResolver,
	lookupPeer PeerLookup,
	instanceURLFn func(ctx context.Context) string,
	usernameFn func(ctx context.Context, userRef int64) string,
) *AdminHandler {
	return &AdminHandler{
		registry:      r,
		activities:    writer,
		auditRec:      auditRec,
		resolveOwner:  resolveOwner,
		lookupPeer:    lookupPeer,
		instanceURLFn: instanceURLFn,
		usernameFn:    usernameFn,
	}
}

const (
	capAdmin = "system.admin"
	// capRead gates the shares READ surfaces — the share list and the
	// defederation cascade preview (#356). Granting and revoking a
	// share stay on capAdmin.
	capRead = "federation.read"
)

// --- GET /admin/federation/shares ---------------------------------------

// ListFederationShares — list ACTIVE shares matching a mutually-
// exclusive filter (object / peer / grantor).
func (h *AdminHandler) ListFederationShares(
	ctx context.Context,
	req openapi.ListFederationSharesRequestObject,
) (openapi.ListFederationSharesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListFederationShares401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capRead) && !id.Can(capAdmin) {
		return openapi.ListFederationShares403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: capRead + " capability required"},
		}, nil
	}
	limit := int32(100)
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}

	// Mutually-exclusive filter resolution. Order of precedence:
	// object (most specific) > peer > grantor. At most one filter
	// engages per call.
	switch {
	case req.Params.ObjectKind != nil && req.Params.ObjectId != nil:
		kind := federation.ShareObjectKind(*req.Params.ObjectKind)
		shares, err := h.registry.ListByObject(ctx, kind, uuid.UUID(*req.Params.ObjectId))
		if err != nil {
			if errors.Is(err, ErrObjectKindInvalid) {
				return openapi.ListFederationShares400JSONResponse{
					BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
				}, nil
			}
			return nil, err
		}
		return openapi.ListFederationShares200JSONResponse(toShareList(shares)), nil
	case req.Params.PeerId != nil:
		shares, err := h.registry.ListByPeer(ctx, uuid.UUID(*req.Params.PeerId), limit)
		if err != nil {
			return nil, err
		}
		return openapi.ListFederationShares200JSONResponse(toShareList(shares)), nil
	case req.Params.GrantorUserRef != nil:
		shares, err := h.registry.ListByGrantor(ctx, *req.Params.GrantorUserRef, limit)
		if err != nil {
			return nil, err
		}
		return openapi.ListFederationShares200JSONResponse(toShareList(shares)), nil
	}
	return openapi.ListFederationShares400JSONResponse{
		BadRequestJSONResponse: openapi.BadRequestJSONResponse{
			Error: "exactly one filter required: (object_kind+object_id) | peer_id | grantor_user_ref",
		},
	}, nil
}

// --- POST /admin/federation/shares (grant) ------------------------------

func (h *AdminHandler) GrantFederationShare(
	ctx context.Context,
	req openapi.GrantFederationShareRequestObject,
) (openapi.GrantFederationShareResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.GrantFederationShare401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.GrantFederationShare400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "request body required"},
		}, nil
	}
	// Owner-or-admin permission check.
	objectKind := federation.ShareObjectKind(req.Body.ObjectKind)
	objectID := uuid.UUID(req.Body.ObjectId)
	if !caller.Can(capAdmin) {
		ok, err := h.resolveOwner(ctx, objectKind, objectID, caller)
		if err != nil {
			return nil, err
		}
		if !ok {
			return openapi.GrantFederationShare403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the owner of this object (and not system.admin)"},
			}, nil
		}
	}

	// Peer lookup — confirms peer exists + carries its instance_url
	// into the share's emit envelope.
	peerID := uuid.UUID(req.Body.PeerId)
	peer, err := h.lookupPeer(ctx, peerID)
	if err != nil {
		return openapi.GrantFederationShare404JSONResponse{Error: "peer not found: " + err.Error()}, nil
	}
	if !peer.Connected || !peer.Enabled {
		return openapi.GrantFederationShare400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "peer must be enabled + status=connected to receive shares"},
		}, nil
	}

	scope := federation.ShareScope(req.Body.Scope)
	var targetURL *string
	if req.Body.TargetUserUrl != nil && *req.Body.TargetUserUrl != "" {
		v := *req.Body.TargetUserUrl
		targetURL = &v
	}
	var expiresPgtype *pgtype.Timestamptz
	var expiresPtr *time.Time
	if req.Body.ExpiresAt != nil {
		ts := pgtype.Timestamptz{Time: *req.Body.ExpiresAt, Valid: true}
		expiresPgtype = &ts
		t := *req.Body.ExpiresAt
		expiresPtr = &t
	}
	notes := ""
	if req.Body.Notes != nil {
		notes = *req.Body.Notes
	}
	// #1573: the original-byte grant is explicit and off unless set.
	allowOriginal := req.Body.AllowOriginal != nil && *req.Body.AllowOriginal

	// Pre-validate via InsertInput.Validate() so the catalogue
	// errors surface BEFORE we mint the activity URI.
	in := InsertInput{
		GrantorUserRef: caller.UserRef,
		ObjectKind:     objectKind,
		ObjectID:       objectID,
		PeerID:         peerID,
		TargetUserURL:  targetURL,
		Scope:          scope,
		ExpiresAt:      expiresPgtype,
		Notes:          notes,
		AllowOriginal:  allowOriginal,
	}
	if err := in.Validate(); err != nil {
		return openapi.GrantFederationShare400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	// #1573: the sensitivity cap, at grant time. A collection's member
	// assets are each checked when a file is requested
	// (CanPeerFetchOriginal); an asset is checked here too, so the
	// operator is told now rather than finding every fetch refused.
	if allowOriginal && objectKind == federation.ShareObjectKindAsset {
		row, err := New(h.registry.Pool).GetAssetGrantContext(ctx, pgtype.UUID{Bytes: objectID, Valid: true})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return openapi.GrantFederationShare404JSONResponse{Error: "asset not found"}, nil
			}
			return nil, fmt.Errorf("shares: grant: asset tier: %w", err)
		}
		if !OriginalTierAllowed(row.Sensitivity) {
			return openapi.GrantFederationShare400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{
					Error: "sensitivity_refused: original files can only be shared for public and team assets; this asset is " + row.Sensitivity,
				},
			}, nil
		}
	}

	// Build the actor context for the emit envelope.
	actor := emit.ActorContext{
		UserRef:  caller.UserRef,
		Username: h.usernameFn(ctx, caller.UserRef),
		BaseURL:  h.instanceURLFn(ctx),
	}

	// #1586: settle the grant tuple's unique slot first, then grant.
	//
	// federation_shares_active_uniq_idx holds (grantor, object, peer,
	// recipient) for every row with revoked_at IS NULL, EXPIRED ones
	// included, so an expired share used to block a regrant with 409
	// until the hourly sweeper revoked it, and the 409 body came back
	// zero-valued because the lookup behind it skipped expired rows.
	//
	//   - slot held by a LIVE share: 409 with exactly that row (same
	//     grantor, object, peer and recipient: the row the index matched);
	//   - slot held by an EXPIRED share: retire it the way the sweeper
	//     does (a local aa:Unshare + revoked_at + federation.share.revoked
	//     "expired", committed in its own tx, so the Unshare precedes the
	//     new aa:Share in the activity ledger), then grant. The revoke is
	//     conditional on the row still being expired; if it was closed or
	//     its expiry extended meanwhile, re-read the slot instead;
	//   - slot free: grant.
	//
	// A concurrent grant or the sweeper can change the slot between the
	// read and the INSERT. The INSERT's unique violation sends us round
	// again, where the slot now resolves to the winner's live row (409)
	// or to free. Bounded: a slot that keeps changing is an error, never
	// an empty 409.
	retirer := expiryRetirer{
		registry:      h.registry,
		activities:    h.activities,
		auditRec:      h.auditRec,
		lookupPeer:    h.lookupPeer,
		instanceURLFn: h.instanceURLFn,
		usernameFn:    h.usernameFn,
	}
	const maxSlotAttempts = 3
	for attempt := 1; ; attempt++ {
		held, expired, err := h.registry.unrevokedForTuple(ctx, in)
		switch {
		case err == nil && !expired:
			return openapi.GrantFederationShare409JSONResponse(toShareAPI(held)), nil
		case err == nil && expired:
			retired, err := retirer.retire(ctx, held,
				"auto-revoked on expiry before a regrant",
				map[string]any{"retired_by": "regrant"})
			if err != nil {
				return nil, fmt.Errorf("shares: grant: retire expired share %s: %w", held.ID, err)
			}
			if !retired {
				// Someone else closed it, or its expiry was extended
				// and it is live again: re-read the slot (a live row
				// answers 409 with that row) instead of inserting.
				if attempt >= maxSlotAttempts {
					return nil, fmt.Errorf("shares: grant: the slot for this grant tuple changed on %d consecutive attempts", attempt)
				}
				continue
			}
		case errors.Is(err, ErrShareNotFound):
			// Slot free.
		default:
			return nil, fmt.Errorf("shares: grant: read grant slot: %w", err)
		}

		resp, conflict, err := h.insertGrant(ctx, caller, in, peer, expiresPtr, actor)
		if err != nil {
			return nil, err
		}
		if !conflict {
			return resp, nil
		}
		if attempt >= maxSlotAttempts {
			return nil, fmt.Errorf("shares: grant: the slot for this grant tuple changed on %d consecutive attempts", attempt)
		}
	}
}

// insertGrant runs the write-ahead grant transaction. conflict=true
// means the INSERT hit federation_shares_active_uniq_idx (another row
// took the tuple's slot first); the transaction is rolled back and the
// caller re-reads the slot.
func (h *AdminHandler) insertGrant(
	ctx context.Context,
	caller *auth.Identity,
	in InsertInput,
	peer PeerInfo,
	expiresPtr *time.Time,
	actor emit.ActorContext,
) (openapi.GrantFederationShareResponseObject, bool, error) {
	// Direct-tx flow (NOT WithEmissionFn) because the share row's
	// granted_activity_id FK requires the activity row's id to
	// exist FIRST. Sequence per the design §7.2 write-ahead
	// invariant:
	//
	//   1. Begin tx
	//   2. RecordActivity (aa:Share) — gets activity_id
	//   3. InsertShare with granted_activity_id = activity_id
	//   4. WriteInTx audit event (federation.share.granted)
	//   5. Commit
	//
	// Any failure rolls everything back. After commit we
	// invalidate the per-object cache so the next CanPeerAccess
	// sees the fresh state.

	shareID := uuid.New()
	tx, err := h.registry.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	// #1571: the previous share of this grant tuple, if any, is the
	// grant this one supersedes on the receiver (aa:supersedes). Read
	// inside the grant transaction, after the caller settled the slot:
	// at most one unrevoked row per tuple exists, and it is not this one.
	supersedes, err := previousShareActivityURI(ctx, tx, in)
	if err != nil {
		return nil, false, fmt.Errorf("shares: grant: previous share: %w", err)
	}
	em := emit.Share(actor, emit.ShareRef{
		ShareID:               shareID,
		ObjectKind:            in.ObjectKind,
		ObjectID:              in.ObjectID,
		PeerURL:               peer.InstanceURL,
		TargetUserURL:         derefOrEmpty(in.TargetUserURL),
		Scope:                 in.Scope,
		ExpiresAt:             expiresPtr,
		Notes:                 in.Notes,
		SupersedesActivityURI: supersedes,
		AllowOriginal:         in.AllowOriginal,
	})

	// 1. Activity row → activity_id.
	activityRow, err := h.activities.RecordActivity(ctx, tx, em.Activity)
	if err != nil {
		return nil, false, fmt.Errorf("shares: grant: emit aa:Share: %w", err)
	}
	activityID := activityRow.ID

	// 2. Share row with the real activity_id.
	shareRow, err := New(tx).InsertShare(ctx, InsertShareParams{
		// The id the envelope and the audit already carry (#1586);
		// before, the row got a different, database-generated id.
		ID:                pgtype.UUID{Bytes: shareID, Valid: true},
		GrantorUserRef:    in.GrantorUserRef,
		ObjectKind:        string(in.ObjectKind),
		ObjectID:          pgtype.UUID{Bytes: in.ObjectID, Valid: true},
		PeerID:            pgtype.UUID{Bytes: in.PeerID, Valid: true},
		TargetUserUrl:     in.TargetUserURL,
		Scope:             string(in.Scope),
		ExpiresAt:         pgtype.Timestamptz{Time: timeOrZero(expiresPtr), Valid: expiresPtr != nil},
		Notes:             in.Notes,
		GrantedActivityID: pgtype.UUID{Bytes: activityID, Valid: true},
		AllowOriginal:     in.AllowOriginal,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Lost the slot to a concurrent grant (or a row that
			// expired after the slot was read): roll back and let the
			// caller re-read the slot, which answers with the exact
			// row or retries. Never a 409 without a row (#1586).
			_ = tx.Rollback(ctx)
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("shares: grant: insert: %w", err)
	}

	// #1574: an original-file share of an asset to the peer as a whole
	// also announces the asset's current version to that peer
	// (aa:AssetVersion), in this transaction, after the aa:Share. It is
	// recorded only for a live local public or team asset and a peer that
	// negotiated archival-1 and original-transfer-1 (assetsend.Record);
	// the delivery worker sends it only after this aa:Share reached the
	// peer.
	if in.ObjectKind == federation.ShareObjectKindAsset && in.AllowOriginal && in.TargetUserURL == nil {
		if _, err := assetsend.Record(ctx, tx, h.activities, assetsend.Grant{
			ShareID: shareID, ShareActivityID: activityID, AssetID: in.ObjectID, PeerID: in.PeerID,
			PeerURL: peer.InstanceURL, BaseURL: actor.BaseURL, Username: h.usernameFn,
		}); err != nil {
			return nil, false, fmt.Errorf("shares: grant: %w", err)
		}
	}

	// 3. Audit event (federation.share.granted) — same tx, so
	// it commits/rolls-back atomically with steps 1+2 per the
	// design §7.2 write-ahead invariant.
	actorRef := caller.UserRef
	auditMeta := map[string]any{
		"share_id":       shareID.String(),
		"object_kind":    string(in.ObjectKind),
		"object_id":      in.ObjectID.String(),
		"peer_id":        in.PeerID.String(),
		"scope":          string(in.Scope),
		"allow_original": in.AllowOriginal,
		"correlation_id": activityID.String(),
	}
	if in.TargetUserURL != nil {
		auditMeta["target_user_url"] = *in.TargetUserURL
	}
	if expiresPtr != nil {
		auditMeta["expires_at"] = expiresPtr.UTC().Format(time.RFC3339Nano)
	}
	h.auditRec.WriteInTx(ctx, audit.New(tx), audit.EventFederationShareGranted, nil, &actorRef, auditMeta)

	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	saved := rowToShare(shareRow)
	// Cache invalidation post-commit so the next CanPeerAccess
	// sees the fresh state immediately (cross-process via
	// cache.Registry NOTIFY).
	h.registry.invalidateObject(ctx, saved.ObjectKind, saved.ObjectID)
	return openapi.GrantFederationShare201JSONResponse(toShareAPI(saved)), false, nil
}

// previousShareActivityURI is the aa:Share activity URI of the most
// recent share of in's grant tuple (grantor, object, peer, recipient,
// the federation_shares_active_uniq_idx key), or "" when there is none.
func previousShareActivityURI(ctx context.Context, tx pgx.Tx, in InsertInput) (string, error) {
	var uri string
	err := tx.QueryRow(ctx, `SELECT a.activity_uri
		  FROM federation_shares s
		  JOIN activities a ON a.id = s.granted_activity_id
		 WHERE s.grantor_user_ref = $1 AND s.object_kind = $2 AND s.object_id = $3
		   AND s.peer_id = $4 AND COALESCE(s.target_user_url, '') = $5
		 ORDER BY s.granted_at DESC, s.id DESC
		 LIMIT 1`,
		in.GrantorUserRef, string(in.ObjectKind), in.ObjectID, in.PeerID, derefOrEmpty(in.TargetUserURL)).Scan(&uri)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return uri, err
}

// grantedActivityURI is the aa:Share activity URI a share row was
// granted by: the grant an aa:Unshare of that row revokes (aa:revokes).
func grantedActivityURI(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, share *Share) (string, error) {
	var uri string
	if err := q.QueryRow(ctx, `SELECT activity_uri FROM activities WHERE id = $1`, share.GrantedActivityID).Scan(&uri); err != nil {
		return "", fmt.Errorf("granted activity of share %s: %w", share.ID, err)
	}
	return uri, nil
}

// derefOrEmpty unwraps a string pointer or returns "".
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- DELETE /admin/federation/shares/{id} (revoke) ----------------------

func (h *AdminHandler) RevokeFederationShare(
	ctx context.Context,
	req openapi.RevokeFederationShareRequestObject,
) (openapi.RevokeFederationShareResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RevokeFederationShare401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	shareID := uuid.UUID(req.Id)
	share, err := h.registry.ByID(ctx, shareID)
	if err != nil {
		if errors.Is(err, ErrShareNotFound) {
			return openapi.RevokeFederationShare404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "share not found"},
			}, nil
		}
		return nil, err
	}
	if !caller.Can(capAdmin) && share.GrantorUserRef != caller.UserRef {
		return openapi.RevokeFederationShare403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "only the grantor or system.admin can revoke"},
		}, nil
	}
	if share.RevokedAt.Valid {
		api := toShareAPI(share)
		return openapi.RevokeFederationShare409JSONResponse(api), nil
	}

	// For 1.22.C-c we wire the revoke path simpler than grant:
	// emit aa:Unshare via the activities writer (which records
	// the activity row), THEN flip revoked_at on the share row
	// pointing at the new activity_id, THEN write the audit event.
	// Same atomicity goal — but in a different order, since the
	// flip needs the activity_id.
	peer, err := h.lookupPeer(ctx, share.PeerID)
	if err != nil {
		return nil, fmt.Errorf("shares: revoke: peer lookup: %w", err)
	}
	actor := emit.ActorContext{
		UserRef:  caller.UserRef,
		Username: h.usernameFn(ctx, caller.UserRef),
		BaseURL:  h.instanceURLFn(ctx),
	}
	target := ""
	if share.TargetUserURL != nil {
		target = *share.TargetUserURL
	}
	revokes, err := grantedActivityURI(ctx, h.registry.Pool, share)
	if err != nil {
		return nil, fmt.Errorf("shares: revoke: %w", err)
	}
	em := emit.Unshare(actor, emit.ShareRef{
		ShareID:       share.ID,
		ObjectKind:    share.ObjectKind,
		ObjectID:      share.ObjectID,
		PeerURL:       peer.InstanceURL,
		TargetUserURL: target,
		Scope:         share.Scope,
		Notes:         share.Notes,
	}, revokes)

	tx, err := h.registry.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	activityRow, err := h.activities.RecordActivity(ctx, tx, em.Activity)
	if err != nil {
		return nil, fmt.Errorf("shares: revoke: emit aa:Unshare: %w", err)
	}
	if _, err := New(tx).RevokeShare(ctx, RevokeShareParams{
		ID:                pgtype.UUID{Bytes: share.ID, Valid: true},
		RevokedActivityID: pgtype.UUID{Bytes: activityRow.ID, Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("shares: revoke: mark revoked: %w", err)
	}
	// Audit (same tx for write-ahead-audit).
	actorRef := caller.UserRef
	h.auditRec.WriteInTx(ctx, audit.New(tx), audit.EventFederationShareRevoked,
		nil, &actorRef, map[string]any{
			"share_id":       share.ID.String(),
			"object_kind":    string(share.ObjectKind),
			"object_id":      share.ObjectID.String(),
			"peer_id":        share.PeerID.String(),
			"reason":         "user_revoked",
			"correlation_id": activityRow.ID.String(),
		})
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// Cache invalidation — same key the Insert/Revoke direct
	// path drops. Outside the tx because cache.Registry NOTIFY
	// runs against the pool, not the tx; the cache eviction
	// happens AFTER the row state is committed, which is what
	// we want (no premature flush).
	h.registry.invalidateObject(ctx, share.ObjectKind, share.ObjectID)
	return openapi.RevokeFederationShare204Response{}, nil
}

// --- GET /admin/federation/shares/grant-context (#1573) -----------------

// GetFederationShareGrantContext answers what the grant form shows
// before a share is made: whether allow_original can be set on the
// object and why not, and for an asset its title, tier and its local
// copyright and credit fields. Same permission as granting. The two
// fields are the owner's free text, passed through as typed: they are
// not an ArchivePub rights URI or requiredStatement (spec v1 §17.2.5),
// nothing is derived from them, and they grant or block nothing.
func (h *AdminHandler) GetFederationShareGrantContext(
	ctx context.Context,
	req openapi.GetFederationShareGrantContextRequestObject,
) (openapi.GetFederationShareGrantContextResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.GetFederationShareGrantContext401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	kind := federation.ShareObjectKind(req.Params.ObjectKind)
	if !kind.Valid() || kind == federation.ShareObjectKindUser {
		return openapi.GetFederationShareGrantContext400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: ErrObjectKindInvalid.Error()},
		}, nil
	}
	objectID := uuid.UUID(req.Params.ObjectId)
	if !caller.Can(capAdmin) {
		ok, err := h.resolveOwner(ctx, kind, objectID, caller)
		if err != nil {
			return nil, err
		}
		if !ok {
			return openapi.GetFederationShareGrantContext403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the owner of this object (and not system.admin)"},
			}, nil
		}
	}
	out := openapi.FederationShareGrantContext{
		ObjectKind: openapi.FederationShareGrantContextObjectKind(kind),
		ObjectId:   objectID,
	}
	refuse := func(r openapi.FederationShareGrantContextOriginalRefusal) {
		out.OriginalAllowed = false
		out.OriginalRefusal = &r
	}
	switch kind {
	case federation.ShareObjectKindAsset:
		q := New(h.registry.Pool)
		row, err := q.GetAssetGrantContext(ctx, pgtype.UUID{Bytes: objectID, Valid: true})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return openapi.GetFederationShareGrantContext404JSONResponse{
					NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
				}, nil
			}
			return nil, fmt.Errorf("shares: grant context: asset: %w", err)
		}
		title := row.Title
		out.Title = &title
		tier := openapi.FederationShareGrantContextSensitivity(row.Sensitivity)
		out.Sensitivity = &tier
		if OriginalTierAllowed(row.Sensitivity) {
			out.OriginalAllowed = true
		} else {
			refuse(openapi.FederationShareGrantContextOriginalRefusal("sensitivity_refused"))
		}
		fields, err := q.ListAssetRightsFields(ctx, pgtype.UUID{Bytes: objectID, Valid: true})
		if err != nil {
			return nil, fmt.Errorf("shares: grant context: rights fields: %w", err)
		}
		for _, f := range fields {
			if f.ValueText == nil || (f.ReadCapability != "" && !caller.Can(f.ReadCapability)) {
				continue
			}
			v := *f.ValueText
			switch f.Code {
			case "copyright":
				out.CopyrightNotice = &v
			case "credit":
				out.Credit = &v
			}
		}
	case federation.ShareObjectKindCollection, federation.ShareObjectKindPost:
		if _, ok, err := ObjectOwnerRef(ctx, h.registry.Pool, kind, objectID); err != nil {
			return nil, fmt.Errorf("shares: grant context: %s: %w", kind, err)
		} else if !ok {
			return openapi.GetFederationShareGrantContext404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: string(kind) + " not found"},
			}, nil
		}
		if kind == federation.ShareObjectKindCollection {
			out.OriginalAllowed = true
		} else {
			refuse(openapi.FederationShareGrantContextOriginalRefusal("kind_without_bytes"))
		}
	default:
		refuse(openapi.FederationShareGrantContextOriginalRefusal("kind_without_bytes"))
	}
	return openapi.GetFederationShareGrantContext200JSONResponse(out), nil
}

// --- adapters -----------------------------------------------------------

func toShareAPI(s *Share) openapi.FederationShare {
	out := openapi.FederationShare{
		Id:                uuid.UUID(s.ID),
		GrantorUserRef:    s.GrantorUserRef,
		ObjectKind:        openapi.FederationShareObjectKind(s.ObjectKind),
		ObjectId:          uuid.UUID(s.ObjectID),
		PeerId:            uuid.UUID(s.PeerID),
		Scope:             openapi.FederationShareScope(s.Scope),
		Notes:             s.Notes,
		GrantedActivityId: uuid.UUID(s.GrantedActivityID),
		AllowOriginal:     s.AllowOriginal,
	}
	if s.GrantedAt.Valid {
		out.GrantedAt = s.GrantedAt.Time
	}
	if s.CreatedAt.Valid {
		out.CreatedAt = s.CreatedAt.Time
	}
	if s.UpdatedAt.Valid {
		out.UpdatedAt = s.UpdatedAt.Time
	}
	if s.TargetUserURL != nil {
		v := *s.TargetUserURL
		out.TargetUserUrl = &v
	}
	if s.ExpiresAt.Valid {
		t := s.ExpiresAt.Time
		out.ExpiresAt = &t
	}
	if s.RevokedAt.Valid {
		t := s.RevokedAt.Time
		out.RevokedAt = &t
	}
	if s.RevokedActivityID != nil {
		v := uuid.UUID(*s.RevokedActivityID)
		out.RevokedActivityId = &v
	}
	return out
}

func toShareList(in []Share) openapi.FederationShareList {
	out := openapi.FederationShareList{Items: make([]openapi.FederationShare, len(in))}
	for i := range in {
		out.Items[i] = toShareAPI(&in[i])
	}
	return out
}

// isUniqueViolation detects a Postgres 23505 unique-constraint
// violation. Used to map a duplicate active-share insert to a
// 409 response.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// timeOrZero unwraps a *time.Time or returns the zero time.
func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
