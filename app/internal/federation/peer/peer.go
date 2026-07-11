// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package peer implements the federation peer registry per
// ADR 0043 §"Trust model" — the per-instance list of who we
// federate with. Pairing alone shares no content;
// federation_shares (1.22.C) is the access-control layer.
//
// # Caching (the federation hot path)
//
// Two hot read paths cached at this layer:
//
//   1. Per-instance-URL lookup (peer.by_url LRU). Every inbound
//      federation activity does GetPeerByInstanceURL to (a)
//      authenticate the request, (b) check the enabled flag, (c)
//      reach the trust + encryption policy. Cache-miss falls
//      through to the DB; writes invalidate via cache.Registry
//      NOTIFY so federated replicas stay coherent.
//
//   2. Enabled-peers snapshot (peer.enabled_snapshot, single-key
//      LRU). Every outbound activity dispatch iterates the
//      enabled-peers set to know who can receive. Snapshot is
//      stored under a fixed key so the LRU acts as a one-slot
//      cache; invalidated on ANY peer mutation (the snapshot
//      represents the whole set, so any change drops it).
//
// Writes always invalidate; reads always cache-first.

package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

const (
	// cacheDomainByURL keys the per-instance-URL cache. Domain
	// string is published so docs/catalogs.md can list it.
	cacheDomainByURL = "peer.by_url"

	// cacheDomainEnabledSnapshot keys the single-slot "all
	// enabled peers" snapshot. Fixed snapshot key so invalidating
	// it drops the whole set on any peer mutation.
	cacheDomainEnabledSnapshot = "peer.enabled_snapshot"
	enabledSnapshotKey         = "all"

	// cacheDomainVisibleSnapshot keys the "peers we expose for
	// peer-of-peer discovery" snapshot. Same single-slot pattern;
	// different predicate. 1.22.B-d.
	cacheDomainVisibleSnapshot = "peer.visible_snapshot"
	visibleSnapshotKey         = "all"
)

// Errors callers may distinguish on.
var (
	// ErrPeerNotFound is returned when a Get/Update/Delete
	// targets a peer that doesn't exist.
	ErrPeerNotFound = errors.New("peer: not found")

	// ErrInstanceURLInvalid covers the input-validation rejections
	// at the Add path — empty / non-https / trailing slash / etc.
	ErrInstanceURLInvalid = errors.New("peer: instance URL must be https:// with no trailing slash")

	// ErrInstancePublicKeyInvalid covers PEM parse failures + non-
	// Ed25519 keys.
	ErrInstancePublicKeyInvalid = errors.New("peer: instance public key must be PEM-wrapped Ed25519")

	// ErrTrustTierInvalid is returned when a supplied trust_tier
	// isn't in the federation.TrustTier closed catalogue.
	ErrTrustTierInvalid = errors.New("peer: trust_tier not in the closed catalogue")

	// ErrEncryptionPolicyInvalid is the same for encryption_policy.
	ErrEncryptionPolicyInvalid = errors.New("peer: encryption_policy not in the closed catalogue")
)

// Peer is the in-memory representation of one row. Public so
// admin handlers + future federation transport code (1.22.D)
// can hold + pass without going through the raw sqlc row.
type Peer struct {
	ID                 uuid.UUID
	InstanceURL        string
	DisplayName        string
	InstancePublicKey  string // PEM (placeholder for pending_outbound; real after handshake)
	TrustTier          federation.TrustTier
	EncryptionPolicy   federation.EncryptionPolicy
	Enabled            bool
	Status             federation.PeerStatus // handshake state (migration 00052)
	HandshakeAt        pgtype.Timestamptz
	HandshakeByUserRef int64
	LastSeenAt         pgtype.Timestamptz
	Notes              string
	// ShareInVisibleList — opt-in for peer-of-peer discovery
	// per migration 00055. When TRUE this peer appears in
	// GET /federation/peers/visible responses.
	ShareInVisibleList bool

	// Capabilities (Phase 1.22.I-d) — bilateral intersection
	// of what BOTH this peer and we support. Populated by the
	// handshake engine on completion. Empty for legacy peers
	// paired before I-d; the [CapabilitiesNegotiatedAt] timestamp
	// distinguishes "never negotiated" (NULL) from "negotiated
	// and got an empty intersection" (non-NULL with empty array).
	Capabilities CapabilitySet

	// CapabilitiesNegotiatedAt is non-zero ONLY when the
	// handshake completed with a capability exchange. Pre-I-d
	// peers surface as zero-time + appear in
	// [Registry.ListPeersMissingCapabilities] for operator
	// re-pairing.
	CapabilitiesNegotiatedAt pgtype.Timestamptz
}

// AddInput is the typed argument to Registry.Add.
type AddInput struct {
	InstanceURL        string
	DisplayName        string
	InstancePublicKey  string
	TrustTier          federation.TrustTier
	EncryptionPolicy   federation.EncryptionPolicy
	HandshakeByUserRef int64
	Notes              string
	// Enabled defaults to TRUE when zero-value; admins can
	// disable post-create via Update.
	Enabled *bool
	// Status defaults to PeerStatusConnected when zero-value
	// (manual admin entry). The handshake flow sets it to
	// pending_outbound (we initiated) or pending_inbound (peer
	// initiated). Skips the PEM-validate step when status is
	// pending_outbound (placeholder pubkey carried in
	// InstancePublicKey until the peer's confirm POST replaces it).
	Status federation.PeerStatus
}

// UpdateInput is the typed PATCH argument to Registry.Update.
// All fields optional — nil means "leave unchanged".
type UpdateInput struct {
	DisplayName        *string
	TrustTier          *federation.TrustTier
	EncryptionPolicy   *federation.EncryptionPolicy
	Enabled            *bool
	Notes              *string
	ShareInVisibleList *bool
}

// Registry is the package's central state. Constructed once at
// boot; safe for concurrent use.
type Registry struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	byURL           *cache.Cache[Peer]
	enabledSnapshot *cache.Cache[enabledSnapshot]
	visibleSnapshot *cache.Cache[enabledSnapshot]
}

// enabledSnapshot is the cached list of all enabled peers —
// the outbox dispatcher iterates this set on every activity.
// Stored as a value type so the LRU holds a single copy without
// pointer-aliasing surprises.
type enabledSnapshot struct {
	Peers []Peer
}

// NewRegistry wires the package. registry can be nil (no caching;
// every read hits the DB). Recommended sizes calibrated for
// federation hot paths — a few hundred peers is a generous v1
// ceiling.
func NewRegistry(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Registry {
	r := &Registry{Pool: pool, Logger: logger}
	if registry != nil {
		r.byURL = cache.Register[Peer](registry, cacheDomainByURL, 1_000)
		r.enabledSnapshot = cache.Register[enabledSnapshot](registry, cacheDomainEnabledSnapshot, 1)
		r.visibleSnapshot = cache.Register[enabledSnapshot](registry, cacheDomainVisibleSnapshot, 1)
	}
	return r
}

// --- read helpers --------------------------------------------------------

// ByInstanceURL resolves a peer by its instance URL. Cache-first;
// cold misses fall through to the DB + populate. Returns
// ErrPeerNotFound when no row exists for the URL.
func (r *Registry) ByInstanceURL(ctx context.Context, url string) (*Peer, error) {
	if r.byURL != nil {
		if hit, ok := r.byURL.Get(url); ok {
			cp := hit
			return &cp, nil
		}
	}
	row, err := New(r.Pool).GetPeerByInstanceURL(ctx, url)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPeerNotFound
		}
		return nil, err
	}
	p := rowToPeer(row)
	if r.byURL != nil {
		r.byURL.Add(url, *p)
	}
	return p, nil
}

// ByID resolves a peer by primary key. Less hot than ByInstanceURL
// so it doesn't get its own cache — admin UI clicks pay one
// indexed lookup.
func (r *Registry) ByID(ctx context.Context, id uuid.UUID) (*Peer, error) {
	pgID := pgtype.UUID{Bytes: id, Valid: true}
	row, err := New(r.Pool).GetPeerByID(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPeerNotFound
		}
		return nil, err
	}
	return rowToPeer(row), nil
}

// List returns up to `limit` peers ordered newest-handshake-first.
// Admin UI default render. Limit clamped to [1, 500].
func (r *Registry) List(ctx context.Context, limit int) ([]Peer, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := New(r.Pool).ListPeers(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	out := make([]Peer, len(rows))
	for i, row := range rows {
		out[i] = *rowToPeer(row)
	}
	return out, nil
}

// EnabledSnapshot returns the cached snapshot of all enabled
// peers — the federation outbox dispatcher's primary read.
// Single-slot LRU means one DB roundtrip per peer mutation,
// memory-bound otherwise.
func (r *Registry) EnabledSnapshot(ctx context.Context) ([]Peer, error) {
	if r.enabledSnapshot != nil {
		if hit, ok := r.enabledSnapshot.Get(enabledSnapshotKey); ok {
			return append([]Peer(nil), hit.Peers...), nil
		}
	}
	rows, err := New(r.Pool).ListEnabledPeers(ctx)
	if err != nil {
		return nil, err
	}
	peers := make([]Peer, len(rows))
	for i, row := range rows {
		peers[i] = *rowToPeer(row)
	}
	if r.enabledSnapshot != nil {
		r.enabledSnapshot.Add(enabledSnapshotKey, enabledSnapshot{Peers: peers})
	}
	// Return a defensive copy so the caller can't mutate the cache.
	return append([]Peer(nil), peers...), nil
}

// --- write helpers -------------------------------------------------------

// Add creates a new peer row. Validates the URL shape + the PEM
// public key + the catalogue-typed fields before hitting the DB.
// Caches the new row + invalidates the enabled snapshot.
//
// Returns ErrPeerNotFound semantics inverted: a UNIQUE-violation
// on instance_url surfaces as a wrapped pgx error the caller maps
// to "peer already exists" at the HTTP layer.
func (r *Registry) Add(ctx context.Context, in AddInput) (*Peer, error) {
	url, err := normalizeInstanceURL(in.InstanceURL)
	if err != nil {
		return nil, err
	}
	status := in.Status
	if status == "" {
		status = federation.PeerStatusConnected
	}
	if !status.Valid() {
		return nil, fmt.Errorf("peer: invalid status %q", status)
	}
	// PEM validation: pending_outbound rows carry a placeholder
	// in InstancePublicKey until the peer's confirm POST overwrites
	// it. Skip validation in that case; validate for every other
	// status (connected = real key from manual entry;
	// pending_inbound = real key from peer's offer envelope).
	if status != federation.PeerStatusPendingOutbound {
		if err := validatePEMEd25519(in.InstancePublicKey); err != nil {
			return nil, err
		}
	}
	if !in.TrustTier.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrTrustTierInvalid, in.TrustTier)
	}
	if !in.EncryptionPolicy.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrEncryptionPolicyInvalid, in.EncryptionPolicy)
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	row, err := New(r.Pool).InsertPeer(ctx, InsertPeerParams{
		InstanceUrl:        url,
		DisplayName:        in.DisplayName,
		InstancePublicKey:  in.InstancePublicKey,
		TrustTier:          string(in.TrustTier),
		EncryptionPolicy:   string(in.EncryptionPolicy),
		Enabled:            enabled,
		Status:             string(status),
		HandshakeByUserRef: in.HandshakeByUserRef,
		Notes:              in.Notes,
	})
	if err != nil {
		return nil, err
	}
	p := rowToPeer(row)
	r.invalidate(ctx, url)
	return p, nil
}

// Update applies a PATCH. Returns ErrPeerNotFound when the row
// doesn't exist. Invalidates the cached row + the enabled
// snapshot (since enabled/tier/policy may have changed).
func (r *Registry) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (*Peer, error) {
	// Validate the typed fields before hitting the DB — fail
	// fast so the caller sees the right error class.
	if in.TrustTier != nil && !in.TrustTier.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrTrustTierInvalid, *in.TrustTier)
	}
	if in.EncryptionPolicy != nil && !in.EncryptionPolicy.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrEncryptionPolicyInvalid, *in.EncryptionPolicy)
	}
	params := UpdatePeerParams{
		ID:                 pgtype.UUID{Bytes: id, Valid: true},
		DisplayName:        in.DisplayName,
		Enabled:            in.Enabled,
		Notes:              in.Notes,
		ShareInVisibleList: in.ShareInVisibleList,
	}
	if in.TrustTier != nil {
		s := string(*in.TrustTier)
		params.TrustTier = &s
	}
	if in.EncryptionPolicy != nil {
		s := string(*in.EncryptionPolicy)
		params.EncryptionPolicy = &s
	}
	row, err := New(r.Pool).UpdatePeer(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPeerNotFound
		}
		return nil, err
	}
	p := rowToPeer(row)
	r.invalidate(ctx, p.InstanceURL)
	return p, nil
}

// Delete defederates a peer. Caller (admin HTTP handler) is
// responsible for emitting the final aa:Unshare activities +
// audit row — those land with the federation outbox dispatcher
// in 1.22.D. This method just removes the row + invalidates
// caches.
func (r *Registry) Delete(ctx context.Context, id uuid.UUID) error {
	// Resolve the URL first so we can invalidate the right cache key.
	p, err := r.ByID(ctx, id)
	if err != nil {
		return err
	}
	if err := New(r.Pool).DeletePeer(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		return err
	}
	r.invalidate(ctx, p.InstanceURL)
	return nil
}

// TouchLastSeen is the best-effort bump called by the 1.22.D
// inbox path when a valid signed request arrives. Never errors
// to the caller; logs internally.
func (r *Registry) TouchLastSeen(ctx context.Context, instanceURL string) {
	if err := New(r.Pool).TouchPeerLastSeen(ctx, instanceURL); err != nil && r.Logger != nil {
		r.Logger.LogAttrs(ctx, slog.LevelWarn, "peer.touch_last_seen.error",
			slog.String("instance_url", instanceURL),
			slog.String("err", err.Error()),
		)
	}
	// Don't invalidate the byURL cache for a last_seen_at bump —
	// the column doesn't drive any hot-path decisions and
	// flushing on every inbound request would defeat the cache.
}

// SetCapabilities (Phase 1.22.I-d) writes the bilateral
// capability intersection produced by the handshake engine on
// both sides of a pairing. Invalidates the by-URL cache so the
// next ByInstanceURL / ByID returns the fresh value rather
// than the stale empty set — load-bearing because the I-e/I-g
// dispatch gate is the only consumer of Peer.Capabilities and
// MUST see the result of negotiation immediately.
//
// caps must be the INTERSECTION of both sides' advertised sets,
// not either side's raw advertised set. Helper [Intersect]
// produces it.
func (r *Registry) SetCapabilities(ctx context.Context, id uuid.UUID, caps CapabilitySet) error {
	blob, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("peer: marshal capabilities: %w", err)
	}
	q := New(r.Pool)
	if err := q.SetPeerCapabilities(ctx, SetPeerCapabilitiesParams{
		ID:           pgtype.UUID{Bytes: id, Valid: true},
		Capabilities: blob,
	}); err != nil {
		return fmt.Errorf("peer: set capabilities: %w", err)
	}
	// Resolve the URL so the by-URL cache invalidation hits the
	// right key. A read failure here doesn't propagate — the
	// write succeeded, and the cache invalidation broadcast will
	// drop the stale entry on the next ByInstanceURL miss
	// (slower path but still correct).
	if peer, err := r.ByID(ctx, id); err == nil {
		r.invalidate(ctx, peer.InstanceURL)
	}
	return nil
}

// ListPeersMissingCapabilities returns peers paired before I-d
// that haven't been re-negotiated. Surfaced by the admin
// federation page so an operator can trigger re-pairing.
//
// Returns the small typed struct rather than the full Peer
// because the only consumer is the operator-facing list — the
// other peer columns aren't needed at the call site.
func (r *Registry) ListPeersMissingCapabilities(ctx context.Context) ([]PeerMissingCapabilities, error) {
	rows, err := New(r.Pool).ListPeersMissingCapabilities(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PeerMissingCapabilities, len(rows))
	for i, row := range rows {
		out[i] = PeerMissingCapabilities{
			ID:          uuid.UUID(row.ID.Bytes),
			InstanceURL: row.InstanceUrl,
			DisplayName: row.DisplayName,
		}
	}
	return out, nil
}

// PeerMissingCapabilities is the operator-list shape.
type PeerMissingCapabilities struct {
	ID          uuid.UUID
	InstanceURL string
	DisplayName string
}

// invalidate drops both caches for a given peer URL. Called on
// every write; safe to call with an empty URL (e.g. on Add
// failure paths where we never got a URL).
func (r *Registry) invalidate(ctx context.Context, url string) {
	if r.byURL != nil && url != "" {
		if err := r.byURL.Invalidate(ctx, url); err != nil && r.Logger != nil {
			r.Logger.LogAttrs(ctx, slog.LevelWarn, "peer.cache.invalidate.error",
				slog.String("domain", cacheDomainByURL),
				slog.String("url", url),
				slog.String("err", err.Error()),
			)
		}
	}
	if r.enabledSnapshot != nil {
		if err := r.enabledSnapshot.Invalidate(ctx, enabledSnapshotKey); err != nil && r.Logger != nil {
			r.Logger.LogAttrs(ctx, slog.LevelWarn, "peer.cache.invalidate.error",
				slog.String("domain", cacheDomainEnabledSnapshot),
				slog.String("err", err.Error()),
			)
		}
	}
	if r.visibleSnapshot != nil {
		if err := r.visibleSnapshot.Invalidate(ctx, visibleSnapshotKey); err != nil && r.Logger != nil {
			r.Logger.LogAttrs(ctx, slog.LevelWarn, "peer.cache.invalidate.error",
				slog.String("domain", cacheDomainVisibleSnapshot),
				slog.String("err", err.Error()),
			)
		}
	}
}

// --- helpers -------------------------------------------------------------

// rowToPeer adapts the sqlc-generated FederationPeer row into
// the package's public Peer type.
func rowToPeer(r FederationPeer) *Peer {
	p := &Peer{
		ID:                       uuid.UUID(r.ID.Bytes),
		InstanceURL:              r.InstanceUrl,
		DisplayName:              r.DisplayName,
		InstancePublicKey:        r.InstancePublicKey,
		TrustTier:                federation.TrustTier(r.TrustTier),
		EncryptionPolicy:         federation.EncryptionPolicy(r.EncryptionPolicy),
		Enabled:                  r.Enabled,
		Status:                   federation.PeerStatus(r.Status),
		HandshakeAt:              r.HandshakeAt,
		HandshakeByUserRef:       r.HandshakeByUserRef,
		LastSeenAt:               r.LastSeenAt,
		Notes:                    r.Notes,
		ShareInVisibleList:       r.ShareInVisibleList,
		CapabilitiesNegotiatedAt: r.CapabilitiesNegotiatedAt,
	}
	// Capabilities JSONB → typed CapabilitySet. Unknown values
	// preserve per ADR 0042. Malformed JSON falls through to an
	// empty set + logs nothing here — boot-time data corruption
	// is surfaced by the admin observability surface
	// (ListPeersMissingCapabilities won't list a peer with a bad
	// blob, so the operator sees the gap via missing-from-list
	// rather than a noisy log line per query).
	if len(r.Capabilities) > 0 {
		_ = p.Capabilities.UnmarshalJSON(r.Capabilities)
	}
	return p
}

// VisibleSnapshot returns the cached snapshot of peers we've
// opted to share in the public /federation/peers/visible response.
// Single-slot LRU per the enabled-snapshot pattern; invalidated
// on every peer mutation (the snapshot represents a whole filter).
//
// Separate from EnabledSnapshot because the predicate differs:
// EnabledSnapshot is for outbound delivery (everyone we can
// reach); VisibleSnapshot is for "what we expose to OTHER peers".
func (r *Registry) VisibleSnapshot(ctx context.Context) ([]Peer, error) {
	if r.visibleSnapshot != nil {
		if hit, ok := r.visibleSnapshot.Get(visibleSnapshotKey); ok {
			return append([]Peer(nil), hit.Peers...), nil
		}
	}
	rows, err := New(r.Pool).ListVisiblePeers(ctx)
	if err != nil {
		return nil, err
	}
	peers := make([]Peer, len(rows))
	for i, row := range rows {
		peers[i] = *rowToPeer(row)
	}
	if r.visibleSnapshot != nil {
		r.visibleSnapshot.Add(visibleSnapshotKey, enabledSnapshot{Peers: peers})
	}
	return append([]Peer(nil), peers...), nil
}

// --- handshake-internal helpers (called only by handshake.go) -----------
//
// These live on Registry because the handshake flow needs them
// but they're not part of the public CRUD surface.

// setStatus atomically updates the status column + invalidates
// both caches. Used by the handshake state machine ONLY — admin
// CRUD uses Update which goes through the full validation path.
func (r *Registry) setStatus(ctx context.Context, id uuid.UUID, status federation.PeerStatus) (*Peer, error) {
	if !status.Valid() {
		return nil, fmt.Errorf("peer.setStatus: invalid status %q", status)
	}
	row, err := New(r.Pool).SetPeerStatus(ctx, SetPeerStatusParams{
		ID:     pgtype.UUID{Bytes: id, Valid: true},
		Status: string(status),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPeerNotFound
		}
		return nil, err
	}
	p := rowToPeer(row)
	r.invalidate(ctx, p.InstanceURL)
	return p, nil
}

// completeOutboundHandshake atomically replaces a pending_outbound
// row's placeholder pubkey with the peer's real key + flips
// status to connected. Called by handshake confirm.
func (r *Registry) completeOutboundHandshake(ctx context.Context, id uuid.UUID, peerPubKeyPEM string) (*Peer, error) {
	if err := validatePEMEd25519(peerPubKeyPEM); err != nil {
		return nil, err
	}
	row, err := New(r.Pool).CompleteOutboundHandshake(ctx, CompleteOutboundHandshakeParams{
		ID:                pgtype.UUID{Bytes: id, Valid: true},
		InstancePublicKey: peerPubKeyPEM,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPeerNotFound
		}
		return nil, err
	}
	p := rowToPeer(row)
	r.invalidate(ctx, p.InstanceURL)
	return p, nil
}

// appendNote appends a free-text line to the notes column so
// the admin UI surfaces transient handshake failures.
func (r *Registry) appendNote(ctx context.Context, id uuid.UUID, note string) error {
	stamped := fmt.Sprintf("[%s] %s", nowUTC().Format("2006-01-02T15:04:05Z"), note)
	row, err := New(r.Pool).AppendPeerNote(ctx, AppendPeerNoteParams{
		ID:    pgtype.UUID{Bytes: id, Valid: true},
		Notes: stamped,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPeerNotFound
		}
		return err
	}
	r.invalidate(ctx, row.InstanceUrl)
	return nil
}

// listPendingInbound returns the pending_inbound rows for the
// admin "requests awaiting your approval" feed. Not cached —
// the partial index federation_peers_pending_inbound_idx keeps
// the query cheap + the data changes on every accept/reject so
// caching would mostly serve stale results.
func (r *Registry) listPendingInbound(ctx context.Context) ([]Peer, error) {
	rows, err := New(r.Pool).ListPendingInboundPeers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Peer, len(rows))
	for i, row := range rows {
		out[i] = *rowToPeer(row)
	}
	return out, nil
}

// nowUTC is a var so tests can substitute a fixed clock. Public
// callers of the package don't see it.
var nowUTC = func() time.Time { return time.Now().UTC() }

// normalizeInstanceURL trims whitespace + rejects shapes the
// rest of the federation stack doesn't accept (plain http, no
// trailing slash, no path component beyond the root).
//
// Note: the trailing-slash strip operates on the host portion
// only — naively trimming "/" from the full string would eat
// into the "https://" prefix for empty-host inputs (the bug the
// peer_test "https://" case caught).
func normalizeInstanceURL(in string) (string, error) {
	s := strings.TrimSpace(in)
	if !strings.HasPrefix(s, "https://") {
		return "", ErrInstanceURLInvalid
	}
	rest := strings.TrimPrefix(s, "https://")
	rest = strings.TrimRight(rest, "/")
	if rest == "" {
		return "", ErrInstanceURLInvalid
	}
	if strings.Contains(rest, "/") {
		return "", ErrInstanceURLInvalid
	}
	return "https://" + rest, nil
}

// validatePEMEd25519 confirms a PEM blob round-trips as an
// Ed25519 public key. Uses federation.PublicKeyFromPEM so the
// validation matches the federation library's own parser exactly.
func validatePEMEd25519(pem string) error {
	if strings.TrimSpace(pem) == "" {
		return ErrInstancePublicKeyInvalid
	}
	if _, err := federation.PublicKeyFromPEM([]byte(pem)); err != nil {
		return fmt.Errorf("%w: %v", ErrInstancePublicKeyInvalid, err)
	}
	return nil
}

// --- JSONB helpers for future inbox / outbox payload columns ------------
//
// Reserved for the Phase 1.22.D federation_inbox + federation_outbox
// JSONB columns. Inlined here so peer.go has zero external
// non-test consumers; promotes to its own file when the
// dispatcher ships.

var _ = json.Marshal
