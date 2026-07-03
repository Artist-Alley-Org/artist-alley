// Package federation resolves federated asset canvas URIs for
// the IIIF Presentation API. Phase 1.54.B.
//
// Load-bearing invariant: this package MUST live outside
// app/internal/federation/ so the federation-soak diff stays
// empty. All lookups here are READ-ONLY against the existing
// federation_peers table; no writes, no outbox events, no
// federation-runtime behaviour is touched.
//
// When an asset carries origin_server_id != NULL:
//   - Look up the peer's instance_url from federation_peers by
//     peer_id (matches assets.origin_server_id → federation_peers.id)
//   - Emit canvas ID as <peer.instance_url>/iiif/3/asset/<asset_id>/manifest.json
//   - Emit Image API base as <peer.instance_url>/iiif/3/<asset_id>
//
// The IIIF client (Mirador, UV, etc.) fetches remote canvases
// directly from the peer per ADR 0043 walled-garden semantics.
// Local AA never proxies remote tile bytes.
package federation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Resolver caches peer_id → instance_url mappings. Peer rows change
// rarely (admin action) so a 5-minute in-process cache is safe;
// a peer URL change appears in manifests on the next TTL tick,
// which is fine for what's fundamentally a directory-lookup URL.
type Resolver struct {
	Pool *pgxpool.Pool

	mu      sync.RWMutex
	entries map[uuid.UUID]cachedEntry
	ttl     time.Duration
}

type cachedEntry struct {
	instanceURL string
	fetchedAt   time.Time
}

// DefaultTTL is the fallback cache freshness window when the
// caller doesn't specify one. 5 minutes matches ManifestCache TTL
// so a manifest re-render always sees consistent peer URLs.
const DefaultTTL = 5 * time.Minute

// NewResolver constructs a Resolver with the default TTL.
func NewResolver(pool *pgxpool.Pool) *Resolver {
	return &Resolver{
		Pool:    pool,
		entries: map[uuid.UUID]cachedEntry{},
		ttl:     DefaultTTL,
	}
}

// Resolve returns the peer's instance_url for the given peer_id,
// stripped of trailing slashes. Returns ErrPeerNotFound when the
// peer_id doesn't exist in the local directory (e.g., a federated
// asset whose peer was subsequently deleted — the canvas ID
// silently falls back to the local base per Builder.remoteCanvasBase's
// empty-string return-value contract).
func (r *Resolver) Resolve(ctx context.Context, peerID uuid.UUID) (string, error) {
	r.mu.RLock()
	if e, ok := r.entries[peerID]; ok && time.Since(e.fetchedAt) < r.ttl {
		r.mu.RUnlock()
		return e.instanceURL, nil
	}
	r.mu.RUnlock()

	var raw string
	err := r.Pool.QueryRow(ctx, `
		SELECT instance_url FROM federation_peers WHERE id = $1
	`, peerID).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrPeerNotFound
		}
		return "", err
	}
	stripped := strings.TrimRight(raw, "/")

	r.mu.Lock()
	r.entries[peerID] = cachedEntry{instanceURL: stripped, fetchedAt: time.Now()}
	r.mu.Unlock()
	return stripped, nil
}

// CanvasBaseFor returns the base URL used for canvas / annotation
// IDs on a federated asset. Format: <peer>/iiif/3/asset/<id>/manifest.json
// (the builder appends /canvas/N + /annotation/N per spec).
//
// Returns empty string on lookup failure so the builder falls
// through to the local URL — a broken peer directory shouldn't
// stop a manifest from rendering.
func (r *Resolver) CanvasBaseFor(ctx context.Context, peerID uuid.UUID, assetID uuid.UUID) string {
	base, err := r.Resolve(ctx, peerID)
	if err != nil || base == "" {
		return ""
	}
	return base + "/iiif/3/asset/" + assetID.String() + "/manifest.json"
}

// ImageBaseFor returns the Image API base for a federated asset.
// Format: <peer>/iiif/3/<asset_id>.
func (r *Resolver) ImageBaseFor(ctx context.Context, peerID uuid.UUID, assetID uuid.UUID) string {
	base, err := r.Resolve(ctx, peerID)
	if err != nil || base == "" {
		return ""
	}
	return base + "/iiif/3/" + assetID.String()
}

// ErrPeerNotFound is returned when the peer_id isn't present in
// federation_peers. Handler surfaces silently — a missing peer
// row falls back to the local base URL (via the builder's
// empty-string contract).
var ErrPeerNotFound = errors.New("iiif/federation: peer not found")
