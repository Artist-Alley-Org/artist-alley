// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package suggest implements the trigram-based autocomplete
// endpoint. Phase 1.16.B-2.
//
// Uses pg_trgm's similarity() function against a corpus of:
//
//   - post_tags.tag (gated by the POST read rule — #1075)
//   - collections.name (gated by the collection rule)
//   - posts.title (gated by the POST read rule)
//   - assets.title (gated by the asset FIELD plane — #1064)
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
	//
	// #1075 — it governs the TAG source too, which used to run
	// ungated. See [Service.tags].
	PostCaps visibility.PostCaps
	// MutationCaps is the caller's asset-mutation scope (#1064, ADR
	// 0064). A completion is an asset TITLE and a title is a FIELD, so
	// this surface answers on the field plane: a team-scoped
	// `assets.admin` holder may already read that title on the asset
	// page, and /search matches it for them since #902, so refusing to
	// complete it was the one place the three surfaces disagreed.
	// Zero value = none, correct for anonymous.
	MutationCaps visibility.AssetMutationCaps
	// CollectionCaps is the caller's capability lookup for the
	// COLLECTION read rule (#1078). Unlike its three neighbours this is
	// the raw checker rather than a resolved value type, because
	// [visibility.CanReadCollection] — the rule this surface must agree
	// with — takes a checker, and resolving it into a new struct here
	// would be a fourth shape of the same question. Nil = no
	// capabilities, correct for anonymous.
	CollectionCaps visibility.CapabilityChecker
	Threshold      float64
	Limit          int
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

	tags, err := s.tags(ctx, prefix, threshold, req.Caller, req.PostCaps)
	if err != nil {
		return Response{}, err
	}
	all = append(all, tags...)

	cols, err := s.collections(ctx, prefix, threshold, req.Caller, req.CollectionCaps)
	if err != nil {
		return Response{}, err
	}
	all = append(all, cols...)

	postTitles, err := s.postTitles(ctx, prefix, threshold, req.Caller, req.PostCaps)
	if err != nil {
		return Response{}, err
	}
	all = append(all, postTitles...)

	assetTitles, err := s.assetTitles(ctx, prefix, threshold, req.Caller, req.Caps, req.MutationCaps)
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

// tags completes on the tags applied to posts, gated by the POST read
// rule — the same predicate [Service.postTitles] composes.
//
// # What this used to be (#1075)
//
// It ran with no caller at all. Its doc claimed the gate was structural
// — *"tags are publicly meaningful even when the posts they appear on
// aren't; the JOIN restricts to posts the caller can see"* — and the
// second half of that sentence was simply false: `JOIN posts p ON p.id =
// pt.post_id` requires the post to EXIST, not to be readable. There was
// no predicate on `p` whatsoever, and the function had no `caller`
// parameter to build one from, while all three of its siblings did.
//
// So the tag source drew from every post on the instance — private,
// draft, and tiers the caller cannot read — and the endpoint takes a
// PREFIX. That is the #899 asset-title recovery in the shape #902 closed
// for `@@`: walk the alphabet and read back the tag vocabulary of
// content you cannot see. Anonymous callers reach /search/suggest on a
// public install, so it did not even need an account.
//
// # Why the post read rule and not something narrower
//
// A tag is a FIELD of the post carrying it, which is the same reason the
// tag facet counts through the post rule (facet.tagAgg) and the same
// reason [Service.postTitles] widened in #873. Composing anything else
// here would put a third answer on a question two surfaces already
// agree on.
//
// The alternative — a curated public tag vocabulary, where the corpus is
// an owned list rather than a projection of live applications — is a
// defensible product, and it is NOT what the old comment described
// either: a vocabulary is authored, not leaked. Out of scope for #1075.
//
// Note the ASYMMETRY with facet.tagAgg that remains: that aggregator
// counts `asset_tag` as well as `post_tags`, and this source still does
// not complete asset tags at all. That is a pre-existing gap in the
// CORPUS, not in the gate, and widening it is a product change.
func (s *Service) tags(
	ctx context.Context, prefix string, threshold float64,
	caller visibility.Caller, caps visibility.PostCaps,
) ([]Suggestion, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityPost, caller,
		visibility.WithPostCaps(caps))
	if err != nil {
		return nil, err
	}
	// $1=prefix, $2=threshold, the predicate's args continue from $3.
	// The LIMIT is INLINED rather than bound: it used to be `$3`, which
	// is the first placeholder the spliced fragment now claims, and
	// leaving it there would have bound the row limit to the caller ref
	// (ADR 0063 — a fragment numbers from $N up and the caller does not
	// renumber it). The siblings all inline it for the same reason.
	frag, args := pred.ToSQL("p", 2)
	sql := `
		SELECT pt.tag AS value, similarity(pt.tag, $1) AS sim
		  FROM post_tags pt
		  JOIN posts p ON p.id = pt.post_id
		 WHERE similarity(pt.tag, $1) > $2` + frag + `
		 GROUP BY pt.tag
		 ORDER BY sim DESC
		 LIMIT ` + itoa(MaxResults)
	queryArgs := append([]any{prefix, threshold}, args...)
	return s.scanSuggestionsWith(ctx, KindTag, sql, queryArgs)
}

// collections completes on collection names.
//
// #1078 — it composes [visibility.CollectionReadableSQL], the whole
// collection read rule, rather than `Filter(EntityCollection)` alone.
// The predicate has no admin disjunct by design (the row plane
// describes who a collection is SHARED WITH, and an instance admin is
// on nobody's share list), so this source used to complete nothing for
// a private collection a system.admin can open from its own page — the
// collections counterpart of #1064, failing CLOSED in the same way.
//
// The rule is #1059's composite, called rather than restated: two hand
// copies is how an admin ends up able to open a collection and unable
// to find it, and a third would have been this one.
//
// ⚠️ The soft-delete conjunct below is load-bearing and is NOT the
// #449 defect. CollectionReadableSQL returns an EMPTY fragment for a
// system.admin — the admin arm deliberately says nothing about
// tombstones, because GetCollection needs it not to (its Restore branch
// depends on an admin passing the read check on a deleted row). Without
// this line an admin's autocomplete would start completing the names of
// collections in the trash. On the row-plane arm the predicate states
// the same rule and the two agree; on the admin arm this is the only
// expression of it, which is why it is here rather than removed as a
// duplicate. It is a CORPUS constraint — "what may be completed" —
// not a second copy of the read rule.
func (s *Service) collections(
	ctx context.Context, prefix string, threshold float64,
	caller visibility.Caller, caps visibility.CapabilityChecker,
) ([]Suggestion, error) {
	frag, args, err := visibility.CollectionReadableSQL(ctx, "c", caller, caps, 2)
	if err != nil {
		return nil, err
	}
	sql := `
		SELECT c.name AS value, similarity(c.name, $1) AS sim
		  FROM collections c
		 WHERE similarity(c.name, $1) > $2
		   AND c.deleted_at IS NULL` + frag + `
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
//
// # The plane: FIELD, not content (#1064)
//
// This composed [visibility.ContentReadableSQL] until #1064 — the plane
// that governs the BYTES. A title is not bytes; it is a FIELD, and ADR
// 0064 confers the field plane on a mutation-capability holder. So a
// team-scoped `assets.admin` holder could open the asset page and read
// the very title this endpoint refused to complete for them.
//
// The decision was not really open. #902 made /search match asset text
// through [visibility.AssetSearchMatchSQL], which is `search_text @@ q
// AND FieldsReadableSQL` — the field plane. A completion that used a
// narrower plane than the search it feeds means typing six letters
// offers nothing and pressing Enter returns the row, which is the
// disagreement #1064 was filed about. One plane, both surfaces.
//
// The direction is a WIDENING and it stays inside 0064: the holder gets
// the field they may already read, and nothing here touches the binary
// plane — [visibility.PreviewReadable] still decides the picture, and
// this endpoint returns no picture at all.
func (s *Service) assetTitles(
	ctx context.Context, prefix string, threshold float64,
	caller visibility.Caller, caps visibility.ContentCaps,
	mut visibility.AssetMutationCaps,
) ([]Suggestion, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return nil, err
	}
	frag, args := pred.ToSQL("", 2)
	// Caller ref inlined as a literal, same reasoning as the facet
	// aggregators: it is an int64 this package produced, never
	// caller-supplied text. FieldsReadableSQL likewise renders the team
	// scope as UUID literals, so neither adds a placeholder and the
	// predicate's numbering above is untouched (ADR 0063).
	frag += visibility.FieldsReadableSQL("", strconv.FormatInt(caller.UserRef, 10), caller, caps, mut)
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
