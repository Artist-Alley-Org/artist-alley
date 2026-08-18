// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package facet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// [FacetType.canonicalValue].
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
)

// AllFacets returns the set of aggregators the dispatcher runs when
// the caller doesn't restrict via ?facets=...
//
// FacetCollection is deliberately absent — see its doc.
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
