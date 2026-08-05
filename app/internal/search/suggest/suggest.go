// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package suggest implements the trigram-based autocomplete
// endpoint. Phase 1.16.B-2.
//
// Uses pg_trgm's similarity() function against a corpus of:
//
//   - post_tags.tag (any live tag application)
//   - collections.name (visibility-gated)
//   - posts.title (visibility-gated)
//   - assets.title (visibility-gated)
//
// Similarity threshold default 0.3 (sysconfig
// search.suggest_similarity_threshold); ordered by similarity DESC;
// limit 10. Visibility gate: shared visibility.Filter per entity —
// restricted entities never contribute to the suggestion corpus.
package suggest

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// DefaultSimilarityThreshold is the pg_trgm cutoff below which
// suggestions are dropped. 0.3 balances recall (partial prefixes
// like "sub" match "subtitle") against noise.
const DefaultSimilarityThreshold = 0.3

// MaxResults caps the response. 10 is the shared-industry norm for
// typeahead widgets.
const MaxResults = 10

// Kind identifies what the suggestion came from so the frontend
// can badge it in the dropdown.
type Kind string

const (
	KindTag        Kind = "tag"
	KindCollection Kind = "collection"
	KindPostTitle  Kind = "post"
	KindAssetTitle Kind = "asset"
)

// Suggestion is one autocomplete entry.
type Suggestion struct {
	Value      string  `json:"value"`
	Kind       Kind    `json:"kind"`
	Similarity float64 `json:"similarity"`
}

// Request drives one Suggest call.
type Request struct {
	Prefix string
	Caller visibility.Caller
	// Caps is the caller's content-plane capabilities (#899). A
	// completion is an asset TITLE, so this surface answers the
	// same question the asset payload does and needs the same
	// short-circuits. Zero value = none, correct for anonymous.
	Caps visibility.ContentCaps
	// PostCaps is the caller's post-plane capabilities (#873). A post
	// title completes iff the caller may read the post, which is the
	// full read rule and not the `public OR author` this surface used
	// to compose. Zero value = none.
	PostCaps  visibility.PostCaps
	Threshold float64
	Limit     int
}

// Response is the ordered result set.
type Response struct {
	Suggestions []Suggestion `json:"suggestions"`
}

// Service encapsulates the pool + counter. One per process.
type Service struct {
	Pool *pgxpool.Pool
	// Counter is nil-safe.
	Counter Counter
}

// Counter is the observability hook.
type Counter interface {
	RecordSuggestion()
	RecordSuggestionHit()
	RecordSuggestionMiss()
}

// NewService constructs a Service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{Pool: pool}
}

// Suggest runs the four sub-queries in parallel, merges, dedupes,
// orders by similarity, caps.
func (s *Service) Suggest(ctx context.Context, req Request) (Response, error) {
	prefix := strings.TrimSpace(req.Prefix)
	if prefix == "" {
		return Response{}, ErrEmptyPrefix
	}
	threshold := req.Threshold
	if threshold <= 0 {
		threshold = DefaultSimilarityThreshold
	}
	limit := req.Limit
	if limit <= 0 || limit > MaxResults {
		limit = MaxResults
	}

	// Assemble all four sub-queries + assemble in-memory.
	all := make([]Suggestion, 0, limit*4)

	tags, err := s.tags(ctx, prefix, threshold)
	if err != nil {
		return Response{}, err
	}
	all = append(all, tags...)

	cols, err := s.collections(ctx, prefix, threshold, req.Caller)
	if err != nil {
		return Response{}, err
	}
	all = append(all, cols...)

	postTitles, err := s.postTitles(ctx, prefix, threshold, req.Caller, req.PostCaps)
	if err != nil {
		return Response{}, err
	}
	all = append(all, postTitles...)

	assetTitles, err := s.assetTitles(ctx, prefix, threshold, req.Caller, req.Caps)
	if err != nil {
		return Response{}, err
	}
	all = append(all, assetTitles...)

	// Dedupe on (value, kind) — same tag can appear on many posts
	// but should surface once. Higher similarity wins on collision.
	seen := make(map[string]int, len(all))
	deduped := make([]Suggestion, 0, len(all))
	for _, s := range all {
		key := string(s.Kind) + ":" + s.Value
		if prev, ok := seen[key]; ok {
			if s.Similarity > deduped[prev].Similarity {
				deduped[prev] = s
			}
			continue
		}
		seen[key] = len(deduped)
		deduped = append(deduped, s)
	}
	sort.Slice(deduped, func(i, j int) bool {
		return deduped[i].Similarity > deduped[j].Similarity
	})
	if len(deduped) > limit {
		deduped = deduped[:limit]
	}
	if s.Counter != nil {
		s.Counter.RecordSuggestion()
	}
	return Response{Suggestions: deduped}, nil
}

// tags queries post_tags via similarity against pg_trgm-indexed tag
// column. Not visibility-gated on the tag itself — tags are
// publicly meaningful even when the posts they appear on aren't;
// the JOIN restricts to posts the caller can see.
func (s *Service) tags(ctx context.Context, prefix string, threshold float64) ([]Suggestion, error) {
	sql := `
		SELECT pt.tag AS value, similarity(pt.tag, $1) AS sim
		  FROM post_tags pt
		  JOIN posts p ON p.id = pt.post_id
		 WHERE similarity(pt.tag, $1) > $2
		 GROUP BY pt.tag
		 ORDER BY sim DESC
		 LIMIT $3
	`
	return s.scanSuggestions(ctx, KindTag, sql, prefix, threshold, MaxResults)
}

func (s *Service) collections(ctx context.Context, prefix string, threshold float64, caller visibility.Caller) ([]Suggestion, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityCollection, caller)
	if err != nil {
		return nil, err
	}
	frag, args := pred.ToSQL("c", 2)
	sql := `
		SELECT c.name AS value, similarity(c.name, $1) AS sim
		  FROM collections c
		 WHERE similarity(c.name, $1) > $2` + frag + `
		 ORDER BY sim DESC
		 LIMIT ` + itoa(MaxResults)
	queryArgs := append([]any{prefix, threshold}, args...)
	return s.scanSuggestionsWith(ctx, KindCollection, sql, queryArgs)
}

// postTitles completes on post titles.
//
// #873 — it composes the full post read rule, the same one the feed
// runs. It used to compose `public OR author`, so a post you could open
// from your feed would not complete: type its first six letters and the
// dropdown stayed empty, with nothing to distinguish that from "no such
// post". This is the widening direction, and it is the opposite of the
// asset rule below — a post you may READ may be completed; what a
// findable post does NOT do is make its restricted members' fields
// readable, which FieldsReadable still governs (#899).
func (s *Service) postTitles(ctx context.Context, prefix string, threshold float64, caller visibility.Caller, caps visibility.PostCaps) ([]Suggestion, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityPost, caller,
		visibility.WithPostCaps(caps))
	if err != nil {
		return nil, err
	}
	frag, args := pred.ToSQL("", 2)
	sql := `
		SELECT title AS value, similarity(title, $1) AS sim
		  FROM posts
		 WHERE similarity(title, $1) > $2` + frag + `
		 ORDER BY sim DESC
		 LIMIT ` + itoa(MaxResults)
	queryArgs := append([]any{prefix, threshold}, args...)
	return s.scanSuggestionsWith(ctx, KindPostTitle, sql, queryArgs)
}

// assetTitles completes on asset titles.
//
// #899 — a completion IS the title, so there is no placeholder shape
// available here: an unreadable asset must not contribute a row at all,
// and this is the one asset surface where dropping rather than
// withholding is the correct answer. The content plane is therefore a
// SQL conjunct rather than a per-row Go decision, because a suggestion
// has no id to hang a marker on.
//
// This surface was the sharpest of the #899 leaks in practice: it takes
// a PREFIX, so it let any signed-in caller reconstruct a restricted
// asset's title letter by letter, without ever touching /assets/{id}.
func (s *Service) assetTitles(ctx context.Context, prefix string, threshold float64, caller visibility.Caller, caps visibility.ContentCaps) ([]Suggestion, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return nil, err
	}
	frag, args := pred.ToSQL("", 2)
	// Caller ref inlined as a literal, same reasoning as the facet
	// aggregators: it is an int64 this package produced, never
	// caller-supplied text.
	frag += visibility.ContentReadableSQL("", strconv.FormatInt(caller.UserRef, 10), caps)
	sql := `
		SELECT title AS value, similarity(title, $1) AS sim
		  FROM assets
		 WHERE similarity(title, $1) > $2
		   AND title <> ''` + frag + `
		 ORDER BY sim DESC
		 LIMIT ` + itoa(MaxResults)
	queryArgs := append([]any{prefix, threshold}, args...)
	return s.scanSuggestionsWith(ctx, KindAssetTitle, sql, queryArgs)
}

// scanSuggestions is the 2-column (value, sim) scanner.
func (s *Service) scanSuggestions(ctx context.Context, kind Kind, sql string, args ...any) ([]Suggestion, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Suggestion, 0, MaxResults)
	for rows.Next() {
		var v string
		var sim float64
		if err := rows.Scan(&v, &sim); err != nil {
			return nil, err
		}
		out = append(out, Suggestion{Value: v, Kind: kind, Similarity: sim})
	}
	return out, rows.Err()
}

// scanSuggestionsWith is scanSuggestions but with args pre-built (needed
// for the visibility-fragment case where we can't use variadic).
func (s *Service) scanSuggestionsWith(ctx context.Context, kind Kind, sql string, args []any) ([]Suggestion, error) {
	return s.scanSuggestions(ctx, kind, sql, args...)
}

// itoa is a tiny alias so the SQL string builders stay compact.
func itoa(n int) string { return strconv.Itoa(n) }

// ErrEmptyPrefix is returned by Suggest for an empty prefix. HTTP
// layer maps to 400.
var ErrEmptyPrefix = errors.New("suggest: prefix is required")
