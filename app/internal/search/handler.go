// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/vector"
)

// Service is the boot-time integration point. Wraps the Engine +
// Cache + Counter so the HTTP handler needs one dependency.
type Service struct {
	engine  *Engine
	cache   *Cache
	counter *Counter
	// vector is the fetcher for pgvector-backed similarity anchor
	// lookups. Nil for services constructed pre-B-3; the HTTP
	// handler treats nil as "vector search disabled" so old boot
	// wires keep working.
	vector *vectorFetcher
}

// vectorFetcher wraps vector.Fetcher so we don't leak the vector
// package into search.Service's exported API.
type vectorFetcher = vector.Fetcher

// Engine exposes the underlying engine for adjacent HTTP handlers
// (save-as-collection needs a cache-bypassing execution).
func (s *Service) Engine() *Engine { return s.engine }

// Vector exposes the vector-embedding fetcher. Nil when the
// service was constructed without the B-3 vector wiring; the HTTP
// layer checks nil and returns an "unsupported" error rather than
// panicking.
func (s *Service) Vector() *vector.Fetcher { return s.vector }

// Pool exposes the underlying pgxpool for adjacent HTTP handlers
// that need one-off queries (visibility spot-checks, etc.).
func (s *Service) Pool() *pgxpool.Pool {
	if s.engine == nil {
		return nil
	}
	return s.engine.Pool
}

// WithVector attaches a vector fetcher. Post-construction so old
// boot wires that don't care about vectors keep working; new
// boot passes NewFetcher(pool).
func (s *Service) WithVector(v *vector.Fetcher) *Service {
	s.vector = v
	return s
}

// NewService wires the Service. Any component can be nil for tests —
// nil cache = never-cache (all misses); nil counter = no
// observability; nil engine = deliberate: the constructor panics
// since there's nothing to execute against.
func NewService(engine *Engine, cache *Cache, counter *Counter) *Service {
	if engine == nil {
		panic("search.NewService: engine must be non-nil")
	}
	return &Service{engine: engine, cache: cache, counter: counter}
}

// Execute runs the unified search. Public so tests can call it
// without spinning up an HTTP server.
//
// Result classification for the counter:
//   - cache_hit: found in cache
//   - cache_miss + (hit | empty): missed cache, ran query
//   - error: engine returned a non-empty-query error
//   - bad_request: engine rejected the query text (empty)
//
// Rate-limited + admin-forbidden classifications live at the HTTP
// wrapper layer — Execute doesn't see the transport concerns.
func (s *Service) Execute(ctx context.Context, q Query) (QueryResult, error) {
	start := time.Now()

	if s.cache != nil {
		if cached, ok := s.cache.Get(q); ok {
			s.record(ResultCacheHit, time.Since(start))
			return cached, nil
		}
	}

	res, err := s.engine.Run(ctx, q)
	if err != nil {
		if errors.Is(err, ErrEmptyQuery) {
			s.record(ResultBadRequest, time.Since(start))
			return QueryResult{}, err
		}
		s.record(ResultError, time.Since(start))
		return QueryResult{}, err
	}

	if s.cache != nil {
		s.cache.Put(q, res)
	}

	if len(res.Hits) == 0 && res.TotalCount == 0 {
		s.record(ResultEmpty, time.Since(start))
	} else {
		s.record(ResultCacheMiss, time.Since(start))
		s.record(ResultHit, time.Since(start))
	}
	// Phase 1.16.B-3 — surface vector-request throughput so
	// /admin/search/health shows hybrid vs pure-BM25 mix.
	if q.SimilarityHint != "" {
		if len(res.Hits) > 0 {
			s.record(ResultVectorHit, time.Since(start))
		} else {
			s.record(ResultVectorMiss, time.Since(start))
		}
	}
	return res, nil
}

// Cache exposes the underlying cache for tests + cross-package
// invalidator wiring.
func (s *Service) Cache() *Cache { return s.cache }

// Counter exposes the counter for tests + boot.
func (s *Service) Counter() *Counter { return s.counter }

// record bumps a counter class if a counter is wired.
func (s *Service) record(r Result, latency time.Duration) {
	if s.counter != nil {
		s.counter.RecordLatency(r, latency)
	}
}

// ParseTypes accepts a comma-separated CSV of hit types and returns
// the canonical HitType slice. Empty input returns nil (caller
// treats as "all"). Returns ErrBadTypes for any unknown token.
func ParseTypes(csv string) ([]HitType, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" || csv == "*" {
		return nil, nil
	}
	parts := strings.Split(csv, ",")
	out := make([]HitType, 0, len(parts))
	seen := make(map[HitType]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "" {
			continue
		}
		t, ok := ParseHitType(p)
		if !ok {
			return nil, ErrBadTypes
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out, nil
}

// ErrBadTypes is returned by ParseTypes when the CSV contains a
// token not matching a known HitType. Handler maps to 400.
var ErrBadTypes = errors.New("search: unknown type in filter")

// MarshalHitJSON produces the JSON encoding for one Hit as the
// public API surfaces it. Hides RawScore + ExtraJSON internals
// while carrying enough for the frontend cards.
//
// WITHHELD HITS (#899). When h.Restricted the map is built from
// scratch as a COMPLETE LITERAL rather than by deleting keys from the
// full one — same discipline as assets.withheldAsset and
// posts' PostMember placeholder, and for the same reason: a key added
// to the full projection later is absent by construction rather than by
// remembering to delete it. Do not refactor these two branches into one
// map with conditional deletes; that is a deny-list.
//
// The permitted key set on a withheld hit is `type`, `id`,
// `restricted`, `owner_display_name` and the three SCORES.
//
// The scores stay because they are properties of the QUERY, not of the
// item: they carry the ordering the client has to render, and the
// ordering is observable anyway from the position in the array. What
// they do leak is that this row matched the query text at all — a real
// residual oracle, inherent to ADR 0064 keeping restricted rows in the
// result set, and recorded as such in visibility.FieldsReadable's doc.
// `created_at` / `updated_at` do NOT stay: those are facts about the
// item, and the post-member placeholder does not carry them.
func MarshalHitJSON(h Hit) json.RawMessage {
	if h.Restricted {
		out := map[string]any{
			"type":         h.Type,
			"id":           h.ID.String(),
			"restricted":   true,
			"score":        h.NormalisedScore,
			"vector_score": h.VectorScore,
			"hybrid_score": h.HybridScore,
		}
		// Absent, not empty — a client that could tell "withheld" from
		// "genuinely nameless" could infer from the difference.
		if h.OwnerDisplayName != "" {
			out["owner_display_name"] = h.OwnerDisplayName
		}
		b, _ := json.Marshal(out)
		return b
	}
	extras := json.RawMessage("{}")
	if len(h.ExtraJSON) > 0 {
		extras = h.ExtraJSON
	}
	out := map[string]any{
		"type":         h.Type,
		"id":           h.ID.String(),
		"title":        h.Title,
		"summary":      h.Summary,
		"score":        h.NormalisedScore,
		"vector_score": h.VectorScore,
		"hybrid_score": h.HybridScore,
		"created_at":   h.CreatedAt,
		"updated_at":   h.UpdatedAt,
		"extra":        extras,
		// `restricted` is present and false on a readable hit for the
		// same reason the schema makes it required on Asset: a marker a
		// client has to test for presence before trusting is a marker
		// that defaults to "not restricted" when it goes missing.
		"restricted": false,
	}
	if h.OwnerUserRef != nil {
		out["owner_user_ref"] = *h.OwnerUserRef
	}
	if h.OriginServerID != nil {
		out["origin_server_id"] = h.OriginServerID.String()
	}
	b, _ := json.Marshal(out)
	return b
}
