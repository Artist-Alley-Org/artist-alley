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
)

// AllFacets returns the set of aggregators the dispatcher runs when
// the caller doesn't restrict via ?facets=...
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

	// Caller drives visibility.Filter. Anonymous callers see facet
	// counts from the public subset only (via the same shared
	// helper the Engine uses).
	Caller visibility.Caller

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
