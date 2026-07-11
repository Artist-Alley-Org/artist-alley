// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package p2p implements peer-of-peer discovery per ADR 0043's
// trust-laundered social-graph suggestions. Each connected peer
// opted to share its visible-peer list with us; we periodically
// fetch those lists, dedup against our own federation_peers,
// surface the remainder as "Suggested peers (via Studio B)".
//
// # Trust model
//
// A suggestion is NOT a trust statement. We trust Studio B; Studio
// B trusts Studio C; Studio C appears as a suggestion. That is
// just a hint — the operator clicking "Pair" on a suggestion
// still kicks off the existing handshake from 1.22.B-b, which IS
// the trust verification.
//
// # Caching
//
// Two layers:
//
//   1. In-process per-source LRU keyed by source_peer_id. Cold
//      reads fall through to federation_peer_suggestions.
//   2. Cross-process via cache.Registry NOTIFY so federated
//      replicas drop the cache in lockstep when an admin runs
//      a refresh.

package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

const (
	cacheDomainBySource = "p2p.by_source"

	httpTimeout  = 15 * time.Second
	maxBodyBytes = 1 * 1024 * 1024 // 1 MB cap on /visible responses
)

// Suggestion is the in-memory shape of one cached suggestion.
type Suggestion struct {
	ID                    uuid.UUID
	SourcePeerID          uuid.UUID
	SuggestedURL          string
	SuggestedDisplayName  string
	SuggestedPublicKey    string
	SuggestedFingerprint  string
	CachedAt              pgtype.Timestamptz

	// Source — joined-in by the registry so the admin UI can
	// render "via Studio B" provenance without a second fetch.
	SourceDisplayName string
	SourceURL         string
}

// Errors callers may distinguish on.
var (
	ErrSourceNotFound = errors.New("p2p: source peer not found")
	ErrSourceUnreachable = errors.New("p2p: could not fetch /federation/peers/visible from source")
)

// Registry owns the suggestions cache + the per-source fetcher.
type Registry struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Peers  *peer.Registry

	bySource *cache.Cache[bySourceSnapshot]
}

// bySourceSnapshot is the cached list of suggestions from ONE
// source peer.
type bySourceSnapshot struct {
	SourceID    uuid.UUID
	Suggestions []Suggestion
}

// NewRegistry wires the package.
func NewRegistry(pool *pgxpool.Pool, logger *slog.Logger, peerReg *peer.Registry, cacheReg *cache.Registry) *Registry {
	r := &Registry{Pool: pool, Logger: logger, Peers: peerReg}
	if cacheReg != nil {
		r.bySource = cache.Register[bySourceSnapshot](cacheReg, cacheDomainBySource, 200)
	}
	return r
}

// --- read helpers --------------------------------------------------------

// ListSuggestions returns up to `limit` cached suggestions across
// ALL sources, deduplicated against our own federation_peers list
// (so a suggested URL we already federate with is hidden — the
// admin only sees genuinely-new pairing candidates).
//
// The dedup happens at query time rather than via FK because our
// own peer set changes independently of suggestions.
func (r *Registry) ListSuggestions(ctx context.Context, limit int32) ([]Suggestion, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := New(r.Pool).ListPeerSuggestions(ctx, limit)
	if err != nil {
		return nil, err
	}
	// Build a lookup of our own peer URLs for dedup.
	ownPeers, err := r.Peers.List(ctx, 500)
	if err != nil {
		return nil, err
	}
	ownURLs := make(map[string]struct{}, len(ownPeers))
	for _, p := range ownPeers {
		ownURLs[p.InstanceURL] = struct{}{}
	}
	// Also build a source-peer lookup so we can join provenance
	// onto each suggestion row without N+1 queries.
	sourceByID := make(map[uuid.UUID]peer.Peer, len(ownPeers))
	for _, p := range ownPeers {
		sourceByID[p.ID] = p
	}
	out := make([]Suggestion, 0, len(rows))
	for _, row := range rows {
		s := *rowToSuggestion(row)
		if _, ours := ownURLs[s.SuggestedURL]; ours {
			continue
		}
		if src, ok := sourceByID[s.SourcePeerID]; ok {
			s.SourceDisplayName = src.DisplayName
			s.SourceURL = src.InstanceURL
		}
		out = append(out, s)
	}
	return out, nil
}

// --- refresh orchestration ----------------------------------------------

// Client is the HTTP fetcher for /federation/peers/visible.
// Stateless; safe for concurrent use.
type Client struct {
	HTTP *http.Client
}

// NewClient wires the default HTTP client.
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: httpTimeout}}
}

// visibleResponse is the on-wire shape of /federation/peers/visible
// (mirrored from the public handler — we don't import server types).
type visibleResponse struct {
	Peers []visiblePeer `json:"peers"`
}

type visiblePeer struct {
	InstanceURL       string `json:"instance_url"`
	DisplayName       string `json:"display_name"`
	InstancePublicKey string `json:"instance_public_key"`
	Fingerprint       string `json:"fingerprint"`
}

// RefreshFromSource fetches /federation/peers/visible from one of
// our connected peers and persists the result as suggestions.
// Returns the count of suggestions persisted.
//
// Errors: ErrSourceNotFound if the source row vanished between
// list + fetch; ErrSourceUnreachable on any HTTP failure.
func (c *Client) RefreshFromSource(ctx context.Context, r *Registry, source *peer.Peer) (int, error) {
	url := strings.TrimRight(source.InstanceURL, "/") + "/federation/peers/visible"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrSourceUnreachable, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrSourceUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return 0, fmt.Errorf("%w: HTTP %s: %s", ErrSourceUnreachable, resp.Status, strings.TrimSpace(string(body)))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrSourceUnreachable, err)
	}
	var vr visibleResponse
	if err := json.Unmarshal(raw, &vr); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrSourceUnreachable, err)
	}
	// Persist + prune in one transaction.
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := New(tx)
	keepURLs := make([]string, 0, len(vr.Peers))
	for _, p := range vr.Peers {
		if p.InstanceURL == "" || p.InstancePublicKey == "" {
			continue
		}
		keepURLs = append(keepURLs, p.InstanceURL)
		if err := q.UpsertPeerSuggestion(ctx, UpsertPeerSuggestionParams{
			SourcePeerID:         pgtype.UUID{Bytes: source.ID, Valid: true},
			SuggestedUrl:         p.InstanceURL,
			SuggestedDisplayName: p.DisplayName,
			SuggestedPublicKey:   p.InstancePublicKey,
			SuggestedFingerprint: p.Fingerprint,
		}); err != nil {
			return 0, err
		}
	}
	keepJSON, _ := json.Marshal(keepURLs)
	if err := q.DeleteSuggestionsBySourceNotIn(ctx, DeleteSuggestionsBySourceNotInParams{
		SourcePeerID: pgtype.UUID{Bytes: source.ID, Valid: true},
		KeepUrls:     keepJSON,
	}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	r.invalidate(source.ID)
	return len(keepURLs), nil
}

// RefreshAll walks all connected+enabled peers, calls
// RefreshFromSource on each. Returns a per-source summary so the
// admin UI can render "N from Studio B; 0 from Studio C
// (unreachable)" feedback.
func (c *Client) RefreshAll(ctx context.Context, r *Registry) []RefreshOutcome {
	peers, err := r.Peers.EnabledSnapshot(ctx)
	if err != nil {
		return []RefreshOutcome{{Error: err.Error()}}
	}
	outcomes := make([]RefreshOutcome, 0, len(peers))
	for i := range peers {
		p := peers[i]
		count, err := c.RefreshFromSource(ctx, r, &p)
		o := RefreshOutcome{
			SourcePeerID:      p.ID,
			SourceDisplayName: p.DisplayName,
			SourceURL:         p.InstanceURL,
			Count:             count,
		}
		if err != nil {
			o.Error = err.Error()
		}
		outcomes = append(outcomes, o)
	}
	return outcomes
}

// RefreshOutcome is one row of the admin's "refresh feedback".
type RefreshOutcome struct {
	SourcePeerID      uuid.UUID
	SourceDisplayName string
	SourceURL         string
	Count             int
	Error             string
}

// --- internal -----------------------------------------------------------

func (r *Registry) invalidate(sourceID uuid.UUID) {
	if r.bySource == nil {
		return
	}
	if err := r.bySource.Invalidate(context.Background(), sourceID.String()); err != nil && r.Logger != nil {
		r.Logger.LogAttrs(context.Background(), slog.LevelWarn, "p2p.cache.invalidate.error",
			slog.String("source_id", sourceID.String()),
			slog.String("err", err.Error()),
		)
	}
}

func rowToSuggestion(r FederationPeerSuggestion) *Suggestion {
	return &Suggestion{
		ID:                   uuid.UUID(r.ID.Bytes),
		SourcePeerID:         uuid.UUID(r.SourcePeerID.Bytes),
		SuggestedURL:         r.SuggestedUrl,
		SuggestedDisplayName: r.SuggestedDisplayName,
		SuggestedPublicKey:   r.SuggestedPublicKey,
		SuggestedFingerprint: r.SuggestedFingerprint,
		CachedAt:             r.CachedAt,
	}
}

// keep federation import alive for any future addition that
// references PeerStatus / TrustTier from this file.
var _ = federation.PeerStatusConnected
