// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package facet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// FacetType identifies a facet aggregator by kebab-case string.
type FacetType string

const (
	FacetAssetType   FacetType = "asset_type"
	FacetTag         FacetType = "tag"
	FacetSensitivity FacetType = "sensitivity"
	FacetOwner       FacetType = "owner"
	FacetExtension   FacetType = "extension"

	// FacetCollection scopes a search to one collection's members
	// (#910). It is a FILTER-ONLY dimension: there is deliberately no
	// [Aggregator] for it and it is NOT in [AllFacets].
	//
	// A bucket list for this dimension would enumerate collection names
	// beside every search and cost a COUNT per collection, and it would
	// answer a question nobody asked — scoping arrives from a collection
	// page or a found collection tile, carrying an id, not from a rail
	// the caller browses. The dimension is also unlike the other five in
	// kind: `extension` describes the row, while `collection` names
	// another entity with its own read rule, which is why
	// [Selection.Authorize] exists and the other dimensions need no
	// equivalent.
	//
	// Being absent from AllFacets means `?facets=collection` resolves
	// (ParseFacetType accepts it, so `filter=collection:…` parses) and
	// then produces no bucket, which is [Dispatcher.Run]'s existing
	// behaviour for any unregistered type.
	FacetCollection FacetType = "collection"

	// FacetField constrains a search by ONE METADATA FIELD's value
	// (#1157). Its wire value carries two things — the field
	// definition's federation-stable `code` and the value — joined by
	// `=`: `filter=field:material=steel`.
	//
	// # Why one dimension and not one per field
	//
	// Because field definitions are DATA. An operator adds one at
	// runtime, so "one FacetType per field" is not expressible — the
	// enum would have to be built from a query, and [ParseFacetType]
	// would stop being a parse-time whitelist. One dimension whose
	// value names the field keeps the whitelist closed and puts the
	// open set where it belongs, in the value grammar. See
	// [FacetType.CanonicalValue].
	//
	// This is the #1157 advanced page's whole mechanism, and it is
	// deliberately the SAME mechanism the rail already uses — the
	// issue's "do NOT invent a second query language" is satisfied by
	// there being nothing new on the wire beyond one more dimension.
	//
	// # Filter-only, like FacetCollection
	//
	// No [Aggregator] and absent from [AllFacets]: a bucket list would
	// mean a COUNT per field per value on every search, and the
	// advanced page renders its pickers from the field DEFINITIONS
	// (`GET /fields`, which carries each field's vocabulary) rather
	// than from counts. #907's invariant is about a bucket's number
	// equalling what ticking it returns, and a dimension that shows no
	// bucket makes no such promise to break.
	//
	// # It is the first dimension that needs AUTHORIZING BY DIMENSION
	//
	// `collection:` needed [Selection.Authorize] because its VALUE
	// names another entity. This one needs it because the FIELD does:
	// `field_definition.read_capability` decides who may read a field
	// at all, and #907 settled that a filter must not answer a question
	// about a column the caller may not read — "with a narrow enough
	// selection, the filter IS the item". So Authorize refuses a term
	// naming a field this caller cannot read, and the search returns
	// empty rather than an error, for the same no-oracle reason.
	FacetField FacetType = "field"

	// FacetAI excludes — or isolates — PURELY AI-generated work
	// (#1242, ADR 0094 fourth amendment). Two values, and they
	// partition the corpus: [AIPure] and [AINotPure].
	//
	// # It keys on PURITY, and keying it on `ai_provenance` would be
	// the bug
	//
	// `posts.ai_provenance` is the LABELLING fact — "does this post
	// contain AI?" — and its positive arm propagates on ANY member, so
	// `{generated, generated}`, `{generated, none}`,
	// `{generated, undeclared}` and `{generated, assisted}` all read
	// `generated`. A "hide AI work" filter keyed on that column would
	// exclude the three MIXED posts along with the pure one, which is
	// exactly what the owner's ruling forbids: an artist who used a
	// generative tool to explore compositions and then painted the final
	// piece by hand has made human work, and excluding their post for
	// one member's declaration punishes the honest declaration the whole
	// design depends on. So the dimension reads `posts.ai_pure`, the
	// second derived fact (migration 00061).
	//
	// # It fails toward SHOWING
	//
	// An UNDECLARED contributor makes a post not-pure, so it SURVIVES
	// [AINotPure]. Wrongly hiding human work is a worse error than
	// showing one more AI post to someone who asked not to see them —
	// ADR 0094 §3 and both amendments take the same direction, and the
	// SQL below carries it in two places: `IS DISTINCT FROM` on the
	// asset arm (`<> 'generated'` is NULL for an undeclared asset, and
	// a NULL conjunct hides the row) and NOT NULL on `posts.ai_pure`.
	//
	// # ⛔ A FILTER, NEVER A GATE (ADR 0094 §4)
	//
	// Nothing is withheld on this axis. The work stays public, findable
	// and countable; a caller who does not ask for this dimension sees
	// pure-AI work in their hits, their counts, their facet buckets and
	// their suggestions exactly as before. That is what keeps the column
	// free of the derived-copies obligation the #1066 list would
	// otherwise impose, and it is why an operator policy ("no AI on this
	// instance") is NOT this dimension — that is moderation, and it
	// belongs in the sensitivity/state machinery.
	//
	// # Filter-only, like FacetCollection and FacetField
	//
	// No [Aggregator] and absent from [AllFacets]. A two-bucket rail
	// reading "not_pure 1,946 / pure 1" is not a discovery surface, and
	// the control this dimension exists for is a toggle rather than a
	// bucket list. #907's invariant — a bucket's number equals what
	// ticking it returns — makes no promise a dimension without buckets
	// can break.
	FacetAI FacetType = "ai"

	// FacetKind narrows to the BADGE KIND a card draws — image, video,
	// ebook, 3d — the browse footer's type filter (#1166), converged
	// onto the shared grammar by #1251 per ADR 0093 decision 1.
	//
	// # It is DERIVED, which is why it is a dimension and not a column
	//
	// There is no `kind` column and there deliberately is not one. The
	// glyph in a tile's corner is resolved in the browser by
	// `kindForAsset` from two inputs — `asset_type` and
	// `file_extension` — and package viewkind is the server-side mirror
	// of that derivation, held to its source by a parity test. The
	// predicate below is [viewkind.KindSQL], the same derivation
	// transcribed to SQL, so "the filter selected this row" and "the
	// card draws this badge" are one decision rather than two that agree
	// today. Filtering on `asset_type` instead was the obvious shortcut
	// and it is provably wrong on the seeded corpus: ref 2 is "Document"
	// and the badge splits it into `ebook` and `doc`.
	//
	// # ⚠️ THE FIRST DIMENSION WHOSE PREDICATE NEEDS THE CALLER
	//
	// [Selection]'s own doc used to state that dimensionSQL is
	// caller-blind BY DESIGN, and that a dimension needing the caller
	// must handle it at the execution chokepoints instead. That holds
	// for a question with a whole-query answer — "may you read this
	// collection", "may you read this field" — and it cannot hold for
	// this one, because the readability rule here applies PER MEMBER of
	// a post, inside a correlated EXISTS that only the renderer builds.
	// So [RenderContext] exists and dimensionSQL takes it. See the post
	// arm in [dimensionSQL] for what is at stake if it is dropped: a
	// restricted member's kind becomes recoverable by asking for each
	// kind in turn.
	//
	// # Its values combine with OR
	//
	// The control is a multi-select — "show me images and videos" — and
	// `?kind=image,video` has meant the union since #1166. For an ASSET,
	// AND would be unsatisfiable (a row resolves to exactly one kind).
	// For a POST it would be satisfiable and WRONG: it would read "a post
	// holding both an image and a video", which is not what ticking two
	// boxes on a type filter asks for. Non-conjunctive on both counts —
	// see [FacetType.conjunctive].
	//
	// # Filter-only, like FacetCollection, FacetField and FacetAI
	//
	// No [Aggregator] and absent from [AllFacets]. The browse footer
	// renders its boxes from the VOCABULARY (viewkind.All), not from
	// counts, so there is no bucket whose number could disagree with
	// what ticking it returns.
	FacetKind FacetType = "kind"

	// FacetVisibility narrows to a SHARING TIER — private, org-only,
	// followers, explicit-share, public — the browse feed's
	// `?visibility=` parameter, converged onto the shared grammar by
	// #1251 slice 2 per ADR 0093 decision 1.
	//
	// # ⛔ IT NARROWS, AND NOTHING ABOUT MOVING IT HERE MAY CHANGE THAT
	//
	// This is the only dimension whose column is also an input to a READ
	// RULE, so it is the only one where "a filter" and "an authorization
	// decision" name the same word, and the distinction is the whole
	// safety argument. A tier is SELECTED here and GRANTED nowhere: every
	// site that renders this fragment ANDs the entity's read rule on
	// after it (posts.ListPostsPageGated splices `readRuleSQL`,
	// search.runPosts splices `visibility.Filter`), so naming five tiers
	// picks among the ones the caller could already read rather than
	// adding any. A `visibility` filter that could widen would be an
	// authorization bypass wearing a filter's clothes.
	//
	// The composition is a property of the SITES, not of this const, and
	// that is why it is asserted rather than asserted-about: see
	// posts.TestVisibilityFilter_NarrowsNeverWidens, which drives a tier
	// the caller cannot read from both sides and requires an EMPTY page
	// for the stranger and the real rows for the owner.
	//
	// # Posts AND collections, because they share one vocabulary
	//
	// `posts.visibility` and `collections.visibility` carry the SAME
	// five-value CHECK constraint and the same meaning (ADR 0009/0010's
	// tiers). Assets do not have the column at all — their axis is
	// `sensitivity`, a DIFFERENT four-value vocabulary already served by
	// [FacetSensitivity] — so the asset arm falls through to ok=false and
	// assets drop out of a tier-filtered page entirely.
	//
	// ⚠️ That makes this THE FIRST DIMENSION NO ASSET CAN SATISFY, which
	// retires a claim [buildAssetPopulationSQL] made in its doc ("no
	// dimension is post-only today so it cannot fire"). The branch it
	// guarded was already correct; what was untrue was that it was
	// unreachable.
	//
	// The direction check [FacetAI]'s collection arm established applies
	// and this lands on the other side of it: a tier filter is a POSITIVE
	// narrowing — the caller is asking FOR something, not excluding it —
	// so an entity that cannot answer leaving the page is the answer,
	// not a loss.
	//
	// # Its values combine with OR
	//
	// A post is in exactly ONE tier, so AND is unsatisfiable — the same
	// reason [FacetExtension] and [FacetSensitivity] are non-conjunctive.
	// OR is also what the feed's own default needs: #1193 made the
	// signed-in default the UNION of four shared tiers, and it is
	// expressed here as four terms of this dimension.
	//
	// # Filter-only, like FacetCollection, FacetField, FacetAI and
	// FacetKind
	//
	// No [Aggregator] and absent from [AllFacets]. A tier rail would be a
	// bucket list of the sharing states of other people's work, which is
	// a moderation view rather than a discovery surface, and the control
	// this dimension exists for is the feed's own display filter.
	FacetVisibility FacetType = "visibility"
)

// The [FacetVisibility] value vocabulary — the five sharing tiers, in
// the order the `posts_visibility_check` / `collections_visibility_check`
// constraints list them, widest last.
//
// CLOSED and validated in [FacetType.CanonicalValue] for the reason
// [FacetAI]'s pair is: there is no `::UUID` cast here to raise a 22P02,
// so a tolerated `visibility:orgonly` would render a predicate matching
// nothing and hand back an EMPTY page to a caller who asked to narrow.
// A 400 at the parser is a mistake the client can see.
//
// ⛔ It is a display vocabulary, NOT a permission vocabulary. Adding a
// value here does not admit a row; the read rule ANDed on after this
// decides that, and it consults its own tables. See [FacetVisibility].
const (
	VisibilityPrivate       = "private"
	VisibilityOrgOnly       = "org-only"
	VisibilityFollowers     = "followers"
	VisibilityExplicitShare = "explicit-share"
	VisibilityPublic        = "public"
)

// VisibilityTiers returns the [FacetVisibility] vocabulary.
//
// Exported because the feed's default tier set is expressed as a subset
// of it (posts.defaultFeedTiers) and because the value validator and the
// tests both need one list rather than two that agree today.
func VisibilityTiers() []string {
	return []string{
		VisibilityPrivate, VisibilityOrgOnly, VisibilityFollowers,
		VisibilityExplicitShare, VisibilityPublic,
	}
}

// The [FacetAI] value vocabulary. CLOSED, validated in
// [FacetType.CanonicalValue], and a 400 out of [ParseSelection] for
// anything else — a filter that looked applied and was not is the whole
// defect the `filter=` parameter was introduced to fix.
//
// They are a PARTITION, not a pair of independent flags: every row is
// exactly one of them, which is what makes the OR of both terms mean
// "no constraint" rather than "nothing" — see [FacetType.conjunctive].
const (
	// AIPure selects work that is ENTIRELY AI-generated: every live
	// contributor declares `generated`, over a non-empty set.
	AIPure = "pure"
	// AINotPure selects everything else — mixed work, wholly human
	// work, and work nobody was asked about. This is the value a
	// "hide AI work" control sends.
	AINotPure = "not_pure"
)

// AllFacets returns the set of aggregators the dispatcher runs when
// the caller doesn't restrict via ?facets=...
//
// FacetCollection, FacetField, FacetAI, FacetKind and FacetVisibility
// are deliberately absent — see their docs.
func AllFacets() []FacetType {
	return []FacetType{FacetAssetType, FacetTag, FacetSensitivity, FacetOwner, FacetExtension}
}

// ParseFacetType returns the canonical FacetType for a case-
// insensitive input, or (empty, false).
func ParseFacetType(s string) (FacetType, bool) {
	switch s {
	case "asset_type", "type":
		return FacetAssetType, true
	case "tag", "tags":
		return FacetTag, true
	case "sensitivity":
		return FacetSensitivity, true
	case "owner", "author":
		return FacetOwner, true
	case "extension", "ext":
		return FacetExtension, true
	case "collection":
		return FacetCollection, true
	case "field":
		return FacetField, true
	case "ai":
		return FacetAI, true
	case "kind":
		return FacetKind, true
	case "visibility":
		return FacetVisibility, true
	}
	return "", false
}

// Bucket is one { value, count } pair the aggregator emits.
type Bucket struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
	// Label is a human-readable rendering for buckets whose value
	// is opaque (asset_type_ref → asset_type.name; owner_user_ref
	// → username). Empty when Value is already display-ready.
	Label string `json:"label,omitempty"`
}

// Result is one aggregator's output: the facet type + its buckets +
// any warnings (timeout, etc.).
type Result struct {
	Type    FacetType `json:"type"`
	Buckets []Bucket  `json:"buckets"`
	// TimedOut is set when the aggregator hit the per-request
	// timeout; buckets is empty in that case.
	TimedOut bool `json:"timed_out,omitempty"`
}

// Request is the input to the dispatcher. The Engine builds one
// per /search/facets call.
type Request struct {
	// QueryText is the same q= string passed to /search. Filters
	// the population before aggregation.
	QueryText string

	// Facets is the subset to compute. Empty = all seeded types.
	Facets []FacetType

	// Selection is the caller's ACTIVE facet filter (#907). Every
	// aggregator narrows its population by [Selection.ForFacet] of its
	// own type before grouping, so the counts describe the result set
	// the caller is actually looking at.
	//
	// A count that ignores the active filter is the same defect as a
	// facet that cannot filter, one level up: the rail would keep
	// reporting the unfiltered corpus while the grid showed a subset,
	// and every number on it would be a lie about the page it sits
	// beside.
	Selection Selection

	// Caller drives visibility.Filter. Anonymous callers see facet
	// counts from the public subset only (via the same shared
	// helper the Engine uses).
	Caller visibility.Caller

	// Caps is the caller's content-plane capabilities (#899).
	// Consulted by the asset aggregators, which count only rows the
	// caller could actually open — see
	// buildAssetVisibilityAppendedSQL for why. Zero value = none.
	Caps visibility.ContentCaps

	// PostCaps is the caller's post-plane capabilities (#873). The tag
	// aggregator counts through posts, so it composes the post read
	// rule in full and needs the capability that opens its `private`
	// tier. Zero value = none.
	PostCaps visibility.PostCaps

	// MutationCaps is the caller's asset-mutation scope (#1056, ADR
	// 0064). The asset aggregators count on the FIELD plane, and a
	// team-scoped `assets.admin` holder is owed the fields of the
	// assets they administer — so they must be COUNTED for them too.
	//
	// This field is the reason #1056 existed: without it the
	// aggregators could only compose the content plane, and the
	// Engine's filter conjunct was pinned to the content plane to
	// match them (see runAssets). Both widened together, because
	// widening either alone makes the rail's number disagree with the
	// result set ticking it returns — #907's defect in a subtler form.
	// Zero value = none, correct for anonymous.
	MutationCaps visibility.AssetMutationCaps

	// Mature is the caller's resolved mature-content axis (#1117,
	// ADR 0090). Zero value = the DISQUALIFIED viewer, so a Request
	// built without it counts the narrower population — the direction
	// that under-reports rather than the one that leaks.
	//
	// It must move in lockstep with search.Query.Mature: the rail's
	// number has to equal the size of the result set that ticking the
	// bucket returns, and these are the two places that population is
	// expressed. Widening one alone shows `png 7` beside a filter that
	// returns 8 (#907's defect, on a second axis).
	Mature visibility.MatureViewer

	// Timeout caps EACH aggregator's runtime independently.
	// Zero = DefaultAggregatorTimeout.
	Timeout time.Duration
}

// renderContext is the caller half of [Selection.SQL] for an aggregator
// (#1251).
//
// The caller ref is INLINED as a literal rather than bound, matching the
// two [visibility.FieldsReadableSQL] / [visibility.MatureFilterSQL] call
// sites in aggregators_impl.go and for the reason recorded there: it is
// an int64 this process produced, never caller-supplied text, and
// threading another placeholder through four aggregators' arg lists is
// where an off-by-one lives. The hot browse feed makes the opposite
// trade — see [RenderContext.CallerArg].
//
// ⚠️ IT IS NOT OPTIONAL HERE, even though no aggregator counts a
// kind bucket. A caller who has ticked `kind:` and asks for the TAG
// facet reaches the post branch with that term in its selection, and a
// zero context would make the post half unsatisfiable — silently
// dropping every post-derived tag count from a rail whose whole
// invariant is that its number equals what ticking it returns.
func (r Request) renderContext() RenderContext {
	return RenderContext{
		Caller:       r.Caller,
		Caps:         r.Caps,
		MutationCaps: r.MutationCaps,
		CallerArg:    strconv.FormatInt(r.Caller.UserRef, 10),
	}
}

// DefaultAggregatorTimeout is the fallback if Request.Timeout is
// zero. Chosen to keep p95 facet response under 700ms even with
// four aggregators running in parallel on a warm cache.
const DefaultAggregatorTimeout = 500 * time.Millisecond

// Response is the assembled per-facet-type payload the endpoint
// returns.
type Response struct {
	Facets map[FacetType]Result `json:"facets"`
}

// Aggregator is one facet's compute path.
type Aggregator interface {
	// Type identifies this aggregator in the dispatcher map.
	Type() FacetType

	// Aggregate queries Postgres + returns buckets. Called inside
	// its own goroutine per /search/facets request; must be
	// concurrency-safe (all implementations are stateless).
	Aggregate(ctx context.Context, pool *pgxpool.Pool, req Request) ([]Bucket, error)
}

// Dispatcher runs all requested aggregators in parallel + assembles
// the response. Stateless; one per process.
type Dispatcher struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// aggregators keyed by type; boot-time registration.
	aggregators map[FacetType]Aggregator

	// Counter is the observability hook. Nil-safe.
	Counter Counter
}

// NewDispatcher wires the pool + seeded aggregators.
func NewDispatcher(pool *pgxpool.Pool, logger *slog.Logger) *Dispatcher {
	d := &Dispatcher{
		Pool:        pool,
		Logger:      logger,
		aggregators: map[FacetType]Aggregator{},
	}
	d.Register(assetTypeAgg{})
	d.Register(tagAgg{})
	d.Register(sensitivityAgg{})
	d.Register(ownerAgg{})
	d.Register(extensionAgg{})
	return d
}

// Register attaches an Aggregator under its Type. Late calls
// overwrite earlier registrations of the same type.
func (d *Dispatcher) Register(a Aggregator) {
	d.aggregators[a.Type()] = a
}

// Run dispatches. Runs each requested aggregator in its own goroutine
// with the per-request timeout; collects results; returns.
func (d *Dispatcher) Run(ctx context.Context, req Request) Response {
	types := req.Facets
	if len(types) == 0 {
		types = AllFacets()
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultAggregatorTimeout
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make(map[FacetType]Result, len(types))
	)

	// #910 — the same parent gate the Engine runs, for the same reason
	// one level up. A rail computed inside a collection the caller may
	// not open would answer "how many pngs are in it" without ever
	// listing a row, which is the count-as-oracle failure #883 pinned
	// wearing a different hat. Empty buckets, not an error: the response
	// must not distinguish "not yours" from "nothing in it".
	if ok, err := req.Selection.Authorize(
		ctx, d.Pool, req.Caller, req.Caps.Checker(),
	); err != nil || !ok {
		if err != nil && d.Logger != nil {
			d.Logger.LogAttrs(ctx, slog.LevelWarn,
				"search.facet.authorize_error",
				slog.String("err", err.Error()),
			)
		}
		for _, ft := range types {
			if _, registered := d.aggregators[ft]; registered {
				results[ft] = Result{Type: ft}
			}
		}
		return Response{Facets: results}
	}

	for _, ft := range types {
		agg, ok := d.aggregators[ft]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(a Aggregator) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			buckets, err := a.Aggregate(cctx, d.Pool, req)
			r := Result{Type: a.Type(), Buckets: buckets}
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					r.TimedOut = true
					if d.Counter != nil {
						d.Counter.RecordFacetTimeout(a.Type())
					}
					if d.Logger != nil {
						d.Logger.LogAttrs(ctx, slog.LevelWarn,
							"search.facet.timeout",
							slog.String("facet", string(a.Type())),
						)
					}
				} else if d.Logger != nil {
					d.Logger.LogAttrs(ctx, slog.LevelWarn,
						"search.facet.error",
						slog.String("facet", string(a.Type())),
						slog.String("err", err.Error()),
					)
				}
			}
			if d.Counter != nil {
				d.Counter.RecordFacet(a.Type())
			}
			mu.Lock()
			results[a.Type()] = r
			mu.Unlock()
		}(agg)
	}
	wg.Wait()
	return Response{Facets: results}
}

// Counter is the observability hook. Wired to the search
// subsystem's counter so /admin/search/health surfaces per-facet
// throughput + timeouts.
type Counter interface {
	RecordFacet(t FacetType)
	RecordFacetTimeout(t FacetType)
}

// ErrTypeNotRegistered is returned when the caller requests a
// facet type no aggregator is registered for. Currently unused —
// unknown types silently skipped; kept for future strictness.
var ErrTypeNotRegistered = fmt.Errorf("facet: type not registered")
