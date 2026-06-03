// Workflow handler — HTTP surface for the workflow state machine.
//
// Right now this is just the read side: list states for a given
// domain. The upload modal calls `GET /workflow/states?domain=post`
// to render a "set initial state" dropdown. Transition endpoints
// (POST /workflow/transitions, GET /workflow/audit/{kind}/{id}) land
// when there's a UI for them — the underlying Service.Transition()
// is already used internally by post/asset handlers.
//
// States are cached per-domain because (a) the set is small (a
// dozen rows per domain at most), (b) reads happen on every upload
// modal open and every post-detail render with a state badge, and
// (c) the underlying rows only change when an admin reseeds the
// state machine. The cache key is the domain string.

package workflow

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// CacheDomain is the stable string identifier for the workflow-states
// cache. Other packages that mutate states (e.g. a future admin
// endpoint or a migration runner) call InvalidateDomain to broadcast.
const CacheDomain = "workflow.states.domain"

// Handler implements the workflow slice of openapi.StrictServerInterface.
type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// byDomain caches the result of ListStatesForDomain keyed by the
	// domain string. nil-safe: nil registry means no caching (used by
	// tests that don't want the LISTEN goroutine).
	byDomain *cache.Cache[[]openapi.WorkflowState]
}

func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{Pool: pool, Logger: logger}
	if registry != nil {
		// 64 entries is more than enough; we have one domain for
		// posts and one per asset_type for assets (a handful in
		// practice).
		h.byDomain = cache.Register[[]openapi.WorkflowState](registry, CacheDomain, 64)
	}
	return h
}

// InvalidateDomain is the cross-package helper for cache eviction
// when a domain's states change (admin reseed, plugin install). Local
// evict + NOTIFY peers in one call. nil-safe.
func InvalidateDomain(ctx context.Context, registry *cache.Registry, domain string) {
	if registry == nil {
		return
	}
	_ = registry.Emit(ctx, CacheDomain, domain)
}

// ListWorkflowStates returns every state in the requested domain.
// Open to any authenticated user — knowing the state vocabulary is
// not sensitive; transition execution is where capability checks
// kick in.
func (h *Handler) ListWorkflowStates(
	ctx context.Context,
	req openapi.ListWorkflowStatesRequestObject,
) (openapi.ListWorkflowStatesResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListWorkflowStates401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	domain := req.Params.Domain
	if domain == "" {
		return openapi.ListWorkflowStates400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "domain query parameter is required"},
		}, nil
	}

	if h.byDomain != nil {
		if cached, ok := h.byDomain.Get(domain); ok {
			return openapi.ListWorkflowStates200JSONResponse(cached), nil
		}
	}

	q := New(h.Pool)
	rows, err := q.ListStatesForDomain(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("workflow: list states: %w", err)
	}

	out := make([]openapi.WorkflowState, 0, len(rows))
	for _, r := range rows {
		out = append(out, openapi.WorkflowState{
			Id:           openapi_types.UUID(r.ID.Bytes),
			Domain:       r.Domain,
			Code:         r.Code,
			Label:        r.Label,
			SortOrder:    r.SortOrder,
			IsInitial:    r.IsInitial,
			IsTerminal:   r.IsTerminal,
			Icon:         r.Icon,
			Color:        r.Color,
			RequiresNote: r.RequiresNote,
		})
	}
	if h.byDomain != nil {
		h.byDomain.Add(domain, out)
	}
	return openapi.ListWorkflowStates200JSONResponse(out), nil
}
