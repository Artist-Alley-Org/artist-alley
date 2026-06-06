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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation"
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

const capAdmin = "system.admin"

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
	if !id.Can(capAdmin) {
		return openapi.ListFederationShares403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
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
	}
	if err := in.Validate(); err != nil {
		return openapi.GrantFederationShare400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}

	// Build the actor context for the emit envelope.
	actor := emit.ActorContext{
		UserRef:  caller.UserRef,
		Username: h.usernameFn(ctx, caller.UserRef),
		BaseURL:  h.instanceURLFn(ctx),
	}

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

	// Build the emit envelope BEFORE the tx so we have the
	// activity URI for the audit correlation.
	shareID := uuid.New()
	em := emit.Share(actor, emit.ShareRef{
		ShareID:       shareID,
		ObjectKind:    in.ObjectKind,
		ObjectID:      in.ObjectID,
		PeerURL:       peer.InstanceURL,
		TargetUserURL: derefOrEmpty(in.TargetUserURL),
		Scope:         in.Scope,
		ExpiresAt:     expiresPtr,
		Notes:         in.Notes,
	})

	tx, err := h.registry.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// 1. Activity row → activity_id.
	activityRow, err := h.activities.RecordActivity(ctx, tx, em.Activity)
	if err != nil {
		return nil, fmt.Errorf("shares: grant: emit aa:Share: %w", err)
	}
	activityID := activityRow.ID

	// 2. Share row with the real activity_id.
	shareRow, err := New(tx).InsertShare(ctx, InsertShareParams{
		GrantorUserRef:    in.GrantorUserRef,
		ObjectKind:        string(in.ObjectKind),
		ObjectID:          pgtype.UUID{Bytes: in.ObjectID, Valid: true},
		PeerID:            pgtype.UUID{Bytes: in.PeerID, Valid: true},
		TargetUserUrl:     in.TargetUserURL,
		Scope:             string(in.Scope),
		ExpiresAt:         pgtype.Timestamptz{Time: timeOrZero(expiresPtr), Valid: expiresPtr != nil},
		Notes:             in.Notes,
		GrantedActivityID: pgtype.UUID{Bytes: activityID, Valid: true},
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Roll back the tx (defer handles it), then look up
			// the existing share + return 409.
			_ = tx.Rollback(ctx)
			existing := h.findActiveDuplicate(ctx, in)
			if existing != nil {
				return openapi.GrantFederationShare409JSONResponse(toShareAPI(existing)), nil
			}
			return openapi.GrantFederationShare409JSONResponse{}, nil
		}
		return nil, fmt.Errorf("shares: grant: insert: %w", err)
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
		return nil, err
	}
	saved := rowToShare(shareRow)
	// Cache invalidation post-commit so the next CanPeerAccess
	// sees the fresh state immediately (cross-process via
	// cache.Registry NOTIFY).
	h.registry.invalidateObject(ctx, saved.ObjectKind, saved.ObjectID)
	return openapi.GrantFederationShare201JSONResponse(toShareAPI(saved)), nil
}

// derefOrEmpty unwraps a string pointer or returns "".
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// findActiveDuplicate looks up the active share that caused a
// UNIQUE violation. Used to surface the existing share to the
// caller via 409. Best-effort — failing the lookup just means
// we surface a generic 409 without the row body.
func (h *AdminHandler) findActiveDuplicate(ctx context.Context, in InsertInput) *Share {
	target := ""
	if in.TargetUserURL != nil {
		target = *in.TargetUserURL
	}
	existing, err := h.registry.FindActive(ctx, in.ObjectKind, in.ObjectID, in.PeerID, target)
	if err != nil {
		return nil
	}
	return existing
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
	em := emit.Unshare(actor, emit.ShareRef{
		ShareID:       share.ID,
		ObjectKind:    share.ObjectKind,
		ObjectID:      share.ObjectID,
		PeerURL:       peer.InstanceURL,
		TargetUserURL: target,
		Scope:         share.Scope,
		Notes:         share.Notes,
	}, "" /* TODO: lookup original aa:Share activity uri */)

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
