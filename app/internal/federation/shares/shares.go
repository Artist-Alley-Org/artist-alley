// Package shares implements the federation_shares table CRUD +
// the inbox-filter helper per the 1.22.C design proposal.
//
// This file (1.22.C-a) is the skeleton: typed model + bare
// Registry methods around the sqlc queries. The aa:Share /
// aa:Unshare wiring + the inbox-filter decision function +
// audit emission land in 1.22.C-b / -c / -d.
//
// # Caching strategy (deferred to 1.22.C-b)
//
// The inbox-filter hot path will need a per-(object_kind, object_id)
// cache so deeply-shared objects don't pay a DB round-trip per
// inbound activity. Single-key LRU on the active-shares-snapshot
// per object; invalidated on every share write. Implementation
// lands with the Registry's Insert / Revoke methods in 1.22.C-b.

package shares

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// cacheDomainByObject keys the per-object active-shares snapshot
// cache (the inbox-filter hot path read). 5,000-slot LRU sized
// for v1's "thousands of active shares per install" expected
// scale; per-object snapshots are bounded by the share-count for
// that object (typically single-digit). Invalidation hooks live
// in Insert + Revoke + the future RevokeAllByPeer batch.
const cacheDomainByObject = "shares.by_object"

// byObjectSnapshot is the cached active-share set for ONE object,
// keyed by "{kind}:{uuid}". Value-typed so the LRU holds a single
// copy without pointer-aliasing surprises.
type byObjectSnapshot struct {
	Shares []Share
}

// Errors callers may distinguish on.
var (
	// ErrShareNotFound is returned for any lookup miss.
	ErrShareNotFound = errors.New("shares: not found")

	// ErrShareAlreadyRevoked is returned when Revoke is called on
	// a row whose revoked_at is already set. Distinct from
	// ErrShareNotFound so callers can decide: idempotent (treat
	// as success) or surface to the admin ("you already revoked
	// this").
	ErrShareAlreadyRevoked = errors.New("shares: already revoked")

	// ErrObjectKindInvalid is returned when a supplied object_kind
	// isn't in the federation.ShareObjectKind closed catalogue.
	ErrObjectKindInvalid = errors.New("shares: object_kind not in catalogue")

	// ErrScopeInvalid is returned when a supplied scope isn't in
	// the federation.ShareScope closed catalogue.
	ErrScopeInvalid = errors.New("shares: scope not in catalogue")
)

// Share is the in-memory representation of one federation_shares
// row. Public so admin handlers + the future inbox-filter +
// outbox-dispatcher can hold + pass without going through the
// raw sqlc row.
type Share struct {
	ID                  uuid.UUID
	GrantorUserRef      int64
	ObjectKind          federation.ShareObjectKind
	ObjectID            uuid.UUID
	PeerID              uuid.UUID
	TargetUserURL       *string
	Scope               federation.ShareScope
	ExpiresAt           pgtype.Timestamptz
	Notes               string
	GrantedActivityID   uuid.UUID
	GrantedAt           pgtype.Timestamptz
	RevokedAt           pgtype.Timestamptz
	RevokedActivityID   *uuid.UUID
	CreatedAt           pgtype.Timestamptz
	UpdatedAt           pgtype.Timestamptz
}

// Active reports whether the share is currently in effect:
// revoked_at IS NULL AND (expires_at IS NULL OR > now). Used by
// the admin UI and as a defense-in-depth check at call sites
// that bypass the DB filter.
func (s *Share) Active(now pgtype.Timestamptz) bool {
	if s == nil {
		return false
	}
	if s.RevokedAt.Valid {
		return false
	}
	if s.ExpiresAt.Valid && now.Valid && !s.ExpiresAt.Time.After(now.Time) {
		return false
	}
	return true
}

// InsertInput is the typed argument to Registry.Insert. Mirrors
// the columns the InsertShare query accepts.
type InsertInput struct {
	GrantorUserRef    int64
	ObjectKind        federation.ShareObjectKind
	ObjectID          uuid.UUID
	PeerID            uuid.UUID
	TargetUserURL     *string // nil = any user on the peer
	Scope             federation.ShareScope
	ExpiresAt         *pgtype.Timestamptz // nil = no expiry
	Notes             string
	GrantedActivityID uuid.UUID
}

// Validate checks the typed inputs against the closed
// catalogues + the design's "restricted requires target_user_url"
// invariant. Called by Registry.Insert before hitting the DB so
// the catalogue/shape errors surface as 400s without round-trip.
//
// The "restricted requires target_user_url" check is partially
// implementable here (we know the share shape); the full check
// lives at the handler layer where the object's restricted flag
// can be resolved.
func (in InsertInput) Validate() error {
	if !in.ObjectKind.Valid() {
		return ErrObjectKindInvalid
	}
	if !in.Scope.Valid() {
		return ErrScopeInvalid
	}
	return nil
}

// Registry is the package's central state. Constructed once at
// boot; safe for concurrent use.
type Registry struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// byObject is the per-object active-shares snapshot cache
	// (1.22.C-b). Inbox-filter reads it on every inbound
	// activity; CanPeerAccess + the container-fallback lookup
	// both go through this slot. Invalidated on every Insert +
	// Revoke for the affected (kind, id). Nil-safe — registry
	// nil means no cross-process broadcast, every lookup falls
	// through to DB.
	byObject *cache.Cache[byObjectSnapshot]
}

// NewRegistry wires the package. registry can be nil for tests
// that don't want the LISTEN goroutine; production wires it via
// the shared cache.Registry.
func NewRegistry(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Registry {
	r := &Registry{Pool: pool, Logger: logger}
	if registry != nil {
		r.byObject = cache.Register[byObjectSnapshot](registry, cacheDomainByObject, 5_000)
	}
	return r
}

// Insert persists a new share. Caller MUST already have
// emitted the aa:Share activity via WithEmission so the
// grantedActivityID FK points at a real row. Validation
// errors are surfaced before the DB hit.
//
// Idempotency: the active-only unique index on
// (grantor, object, peer, target_user) means a duplicate insert
// surfaces as a UNIQUE violation — caller maps to "share already
// exists" at the HTTP layer.
func (r *Registry) Insert(ctx context.Context, in InsertInput) (*Share, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	var targetURL *string
	if in.TargetUserURL != nil && *in.TargetUserURL != "" {
		v := *in.TargetUserURL
		targetURL = &v
	}
	var expires pgtype.Timestamptz
	if in.ExpiresAt != nil {
		expires = *in.ExpiresAt
	}
	row, err := New(r.Pool).InsertShare(ctx, InsertShareParams{
		GrantorUserRef:    in.GrantorUserRef,
		ObjectKind:        string(in.ObjectKind),
		ObjectID:          pgtype.UUID{Bytes: in.ObjectID, Valid: true},
		PeerID:            pgtype.UUID{Bytes: in.PeerID, Valid: true},
		TargetUserUrl:     targetURL,
		Scope:             string(in.Scope),
		ExpiresAt:         expires,
		Notes:             in.Notes,
		GrantedActivityID: pgtype.UUID{Bytes: in.GrantedActivityID, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	share := rowToShare(row)
	// Cache invariant: any write to (kind, id) drops the cached
	// share-set so the next CanPeerAccess sees the fresh state.
	// Cross-process: cache.Registry NOTIFY ensures federated
	// replicas drop too.
	r.invalidateObject(ctx, share.ObjectKind, share.ObjectID)
	return share, nil
}

// ByID looks up a share by primary key.
func (r *Registry) ByID(ctx context.Context, id uuid.UUID) (*Share, error) {
	row, err := New(r.Pool).GetShareByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrShareNotFound
		}
		return nil, err
	}
	return rowToShare(row), nil
}

// Revoke marks the share revoked + captures the aa:Unshare
// activity UUID. Returns ErrShareAlreadyRevoked when the row
// is already revoked (caller decides whether to treat as
// success or error).
func (r *Registry) Revoke(ctx context.Context, id uuid.UUID, revokedActivityID uuid.UUID) (*Share, error) {
	row, err := New(r.Pool).RevokeShare(ctx, RevokeShareParams{
		ID:                pgtype.UUID{Bytes: id, Valid: true},
		RevokedActivityID: pgtype.UUID{Bytes: revokedActivityID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either id doesn't exist OR already revoked. Disambiguate
			// with a follow-up lookup so the caller gets the right
			// error class.
			existing, err2 := r.ByID(ctx, id)
			if err2 != nil {
				return nil, err
			}
			if existing.RevokedAt.Valid {
				return existing, ErrShareAlreadyRevoked
			}
			return nil, ErrShareNotFound
		}
		return nil, err
	}
	share := rowToShare(row)
	r.invalidateObject(ctx, share.ObjectKind, share.ObjectID)
	return share, nil
}

// ListByObject returns active shares for one object (admin
// "who has access?" view + outbox dispatch source).
func (r *Registry) ListByObject(ctx context.Context, kind federation.ShareObjectKind, objectID uuid.UUID) ([]Share, error) {
	if !kind.Valid() {
		return nil, ErrObjectKindInvalid
	}
	rows, err := New(r.Pool).ListSharesByObject(ctx, ListSharesByObjectParams{
		ObjectKind: string(kind),
		ObjectID:   pgtype.UUID{Bytes: objectID, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	return rowsToShares(rows), nil
}

// ListByPeer returns active shares for one peer (admin per-peer
// outbound view + defederation cascade source).
func (r *Registry) ListByPeer(ctx context.Context, peerID uuid.UUID, limit int32) ([]Share, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := New(r.Pool).ListSharesByPeer(ctx, ListSharesByPeerParams{
		PeerID: pgtype.UUID{Bytes: peerID, Valid: true},
		Limit:  limit,
	})
	if err != nil {
		return nil, err
	}
	return rowsToShares(rows), nil
}

// CountByPeer returns the total active-share count for a peer,
// used by the defederation cascade-preview modal.
func (r *Registry) CountByPeer(ctx context.Context, peerID uuid.UUID) (int64, error) {
	return New(r.Pool).CountSharesByPeer(ctx, pgtype.UUID{Bytes: peerID, Valid: true})
}

// CountsByPeerBreakdown returns the per-object-kind active-share
// counts for one peer. Used by the defederation cascade-preview
// modal to show "12 posts, 23 collections, 8 assets, 4 brand kits"
// per the design proposal §8.5. NOT cached — the preview is an
// admin-clicked one-shot, not a hot path; the per-peer partial
// index keeps it sub-ms.
func (r *Registry) CountsByPeerBreakdown(ctx context.Context, peerID uuid.UUID) (map[federation.ShareObjectKind]int64, error) {
	rows, err := New(r.Pool).GetShareCountsByPeer(ctx, pgtype.UUID{Bytes: peerID, Valid: true})
	if err != nil {
		return nil, err
	}
	out := make(map[federation.ShareObjectKind]int64, len(rows))
	for _, row := range rows {
		out[federation.ShareObjectKind(row.ObjectKind)] = row.ShareCount
	}
	return out, nil
}

// ListByGrantor returns the user's own outbound shares list.
func (r *Registry) ListByGrantor(ctx context.Context, grantorUserRef int64, limit int32) ([]Share, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := New(r.Pool).ListSharesByGrantor(ctx, ListSharesByGrantorParams{
		GrantorUserRef: grantorUserRef,
		Limit:          limit,
	})
	if err != nil {
		return nil, err
	}
	return rowsToShares(rows), nil
}

// FindActive is the inbox-filter direct lookup. Returns the
// highest-scope active share matching (object, peer, target user)
// or ErrShareNotFound if no match. Container resolution
// (collection/workspace fallback) lands in 1.22.C-b alongside
// the decision function.
func (r *Registry) FindActive(
	ctx context.Context,
	kind federation.ShareObjectKind,
	objectID uuid.UUID,
	peerID uuid.UUID,
	targetUserURL string,
) (*Share, error) {
	row, err := New(r.Pool).FindActiveShare(ctx, FindActiveShareParams{
		ObjectKind:    string(kind),
		ObjectID:      pgtype.UUID{Bytes: objectID, Valid: true},
		PeerID:        pgtype.UUID{Bytes: peerID, Valid: true},
		TargetUserUrl: stringPtrOrNil(targetUserURL),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrShareNotFound
		}
		return nil, err
	}
	return rowToShare(row), nil
}

// ListExpiring returns active shares whose expires_at has
// passed. Input to the expiry-sweeper job (Phase 1.22.C-d).
func (r *Registry) ListExpiring(ctx context.Context, limit int32) ([]Share, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := New(r.Pool).ListExpiringShares(ctx, limit)
	if err != nil {
		return nil, err
	}
	return rowsToShares(rows), nil
}

// ListActiveByPeerChunk pulls the next N active shares for a
// peer. Used by the defederation cascade-worker (Phase 1.22.C-d
// job) to process shares in bounded chunks per the reviewer's
// "chunked, never single-tx" rule.
func (r *Registry) ListActiveByPeerChunk(ctx context.Context, peerID uuid.UUID, limit int32) ([]Share, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	rows, err := New(r.Pool).ListActiveSharesByPeerChunk(ctx, ListActiveSharesByPeerChunkParams{
		PeerID: pgtype.UUID{Bytes: peerID, Valid: true},
		Limit:  limit,
	})
	if err != nil {
		return nil, err
	}
	return rowsToShares(rows), nil
}

// --- helpers -------------------------------------------------------------

func rowToShare(r FederationShare) *Share {
	s := &Share{
		ID:                uuid.UUID(r.ID.Bytes),
		GrantorUserRef:    r.GrantorUserRef,
		ObjectKind:        federation.ShareObjectKind(r.ObjectKind),
		ObjectID:          uuid.UUID(r.ObjectID.Bytes),
		PeerID:            uuid.UUID(r.PeerID.Bytes),
		TargetUserURL:     r.TargetUserUrl,
		Scope:             federation.ShareScope(r.Scope),
		ExpiresAt:         r.ExpiresAt,
		Notes:             r.Notes,
		GrantedActivityID: uuid.UUID(r.GrantedActivityID.Bytes),
		GrantedAt:         r.GrantedAt,
		RevokedAt:         r.RevokedAt,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
	if r.RevokedActivityID.Valid {
		id := uuid.UUID(r.RevokedActivityID.Bytes)
		s.RevokedActivityID = &id
	}
	return s
}

func rowsToShares(rows []FederationShare) []Share {
	out := make([]Share, len(rows))
	for i, r := range rows {
		out[i] = *rowToShare(r)
	}
	return out
}

// stringPtrOrNil returns nil for "" (so the SQL OR-NULL fallback
// engages) and a pointer to the string otherwise.
func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
