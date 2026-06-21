// Package mcpdispatch is the guard chain for MCP-tool invocation —
// the single entry point any caller (admin endpoint, future job
// handlers, future asset-action shortcuts) uses to reach an
// operator-registered MCP server through Phase 1.14.A's typed audit
// + cost + privacy machinery.
//
// # Guard order (fail-fast at each step)
//
//   1. caller capability — Identity.Can("mcp.client.use") plus the
//      tool's optional additional_capability
//   2. server enabled + tool whitelisted — the registry's per-server
//      tool-grant rows are the source of truth (not the live server
//      response)
//   3. privacy — restricted/embargo assets clamp to local-only
//      providers per ai.ClassifyPrivacy from 1.14.A
//   4. budget — per-server estimated cost vs the budget tracker's
//      pre-call gate from 1.14.A
//   5. invoke — provider.InvokeTool round-trip
//   6. audit — ai_provider_call row (provider=server name; model=
//      tool name; concern='complete' since MCP tools don't fit any
//      typed concern enum slot)
//
// # No retries here
//
// Transient errors bubble back to the caller; if the caller is a
// background job, the existing jobs worker handles retries via the
// standard backoff. For synchronous admin invocations the caller
// renders the error.

package mcpdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	mcpregistry "github.com/mscrnt/artist-alley/app/internal/ai/mcp_registry"
	mcpserver "github.com/mscrnt/artist-alley/app/internal/ai/providers/mcp_server"
	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// Errors surfaced by Invoke. The admin HTTP handler maps each to a
// distinct status code; job handlers classify against the existing
// ai.ProviderError + jobs.TerminalError set.
var (
	ErrServerDisabled        = errors.New("mcpdispatch: server is disabled")
	ErrToolNotWhitelisted    = errors.New("mcpdispatch: tool not in operator whitelist")
	ErrToolDisabled          = errors.New("mcpdispatch: tool grant is disabled")
	ErrMissingCapability     = errors.New("mcpdispatch: caller lacks required capability")
	ErrPrivacyBlocked        = errors.New("mcpdispatch: cloud MCP server blocked for restricted/embargo asset")
	ErrBudgetExhausted       = errors.New("mcpdispatch: server's cost cap reached")
)

// BudgetGate is the narrow surface this package needs from the
// 1.14.A cost.Tracker. *cost.Tracker satisfies it. Defined locally
// so tests can stub.
type BudgetGate interface {
	CheckBudgetBefore(ctx context.Context, provider string, estimatedCostMicros int64) error
}

// ProviderRegistry is the narrow surface for finding the runtime
// *mcpserver.Provider matching a registration. The boot wire
// populates this by registering each enabled server with the AI
// router; tests pass an in-memory map.
type ProviderRegistry interface {
	Provider(name string) (*mcpserver.Provider, bool)
}

// Dispatcher holds the cross-package deps. One per app process;
// passed to the admin handler + any future caller.
type Dispatcher struct {
	registry  *mcpregistry.Registry
	providers ProviderRegistry
	budget    BudgetGate
	auditor   *ai.CallAuditor
	policy    ai.PrivacyPolicy
	logger    *slog.Logger
}

// New wires the dispatcher. budget + auditor may be nil for tests;
// when nil the dispatcher skips the gate / audit step cleanly.
func New(
	registry *mcpregistry.Registry,
	providers ProviderRegistry,
	budget BudgetGate,
	auditor *ai.CallAuditor,
	policy ai.PrivacyPolicy,
	logger *slog.Logger,
) *Dispatcher {
	return &Dispatcher{
		registry:  registry,
		providers: providers,
		budget:    budget,
		auditor:   auditor,
		policy:    policy,
		logger:    logger,
	}
}

// InvokeOpts knobs the call.
type InvokeOpts struct {
	// ServerName — operator-chosen registration name.
	ServerName string
	// Tool — operator-whitelisted tool name.
	Tool string
	// Arguments — opaque JSON-serialisable payload the tool consumes.
	Arguments map[string]any
	// Caller — used for capability checks + audit attribution. Nil
	// callers fail the capability gate.
	Caller *auth.Identity
	// Sensitivity — asset's sensitivity tier when the call is on
	// behalf of an asset. Empty (zero value) = SensitivityPublic for
	// the privacy classifier.
	Sensitivity ai.SensitivityTier
	// AssetID — populated when the call is on behalf of an asset;
	// audit row records it for cross-reference.
	AssetID uuid.UUID
}

// Invoke runs the full guard chain + returns the raw JSON the MCP
// server emitted. Caller interprets the response based on the tool's
// known shape — the dispatcher itself is tool-agnostic.
func (d *Dispatcher) Invoke(ctx context.Context, opts InvokeOpts) (json.RawMessage, error) {
	// 1. Resolve registration.
	server, err := d.registry.GetServerByName(ctx, opts.ServerName)
	if err != nil {
		return nil, err // ErrServerNotFound bubbles
	}
	if !server.Enabled {
		d.auditBlocked(ctx, opts, server, "permanent_error", ErrServerDisabled)
		return nil, ErrServerDisabled
	}

	// 2. Caller capability gate. Two checks: the umbrella server cap,
	// plus the per-tool additional_capability if any.
	if opts.Caller == nil || !opts.Caller.Can("mcp.client.use") {
		d.auditBlocked(ctx, opts, server, "permanent_error", ErrMissingCapability)
		return nil, fmt.Errorf("%w: mcp.client.use", ErrMissingCapability)
	}

	// 3. Tool whitelist + per-tool grant lookup.
	grant, err := d.findToolGrant(ctx, server.ID, opts.Tool)
	if err != nil {
		d.auditBlocked(ctx, opts, server, "permanent_error", err)
		return nil, err
	}
	if !grant.Enabled {
		d.auditBlocked(ctx, opts, server, "permanent_error", ErrToolDisabled)
		return nil, ErrToolDisabled
	}
	if grant.AdditionalCapability != "" && !opts.Caller.Can(grant.AdditionalCapability) {
		d.auditBlocked(ctx, opts, server, "permanent_error",
			fmt.Errorf("%w: %s", ErrMissingCapability, grant.AdditionalCapability))
		return nil, fmt.Errorf("%w: %s", ErrMissingCapability, grant.AdditionalCapability)
	}

	// 4. Privacy gate. Cloud-classified server + restricted/embargo
	// asset → block. ai.ClassifyPrivacy returns LocalOnly when the
	// asset's tier triggers the lock; we compare against the
	// server's registered class.
	privacy := ai.ClassifyPrivacy(opts.Sensitivity, d.policy)
	if privacy == ai.PrivacyClassLocalOnly && server.PrivacyClass != "local" {
		d.auditBlocked(ctx, opts, server, "privacy_blocked", ErrPrivacyBlocked)
		return nil, ErrPrivacyBlocked
	}

	// 5. Budget gate. Estimated cost is the operator-declared per-
	// call number on the tool grant; budget tracker reads against
	// the per-provider rollup.
	if d.budget != nil {
		if err := d.budget.CheckBudgetBefore(ctx, server.Name, grant.CostEstimateMicros); err != nil {
			d.auditBlocked(ctx, opts, server, "budget_blocked", err)
			return nil, fmt.Errorf("%w: %v", ErrBudgetExhausted, err)
		}
	}

	// 6. Resolve provider + invoke.
	prov, ok := d.providers.Provider(server.Name)
	if !ok {
		err := fmt.Errorf("mcpdispatch: provider %q not registered with router (boot-wire bug)", server.Name)
		d.auditBlocked(ctx, opts, server, "permanent_error", err)
		return nil, err
	}

	start := time.Now()
	result, err := prov.InvokeTool(ctx, opts.Tool, opts.Arguments)
	duration := time.Since(start)
	if err != nil {
		d.auditCall(ctx, opts, server, grant, duration, classifyErrorStatus(err), err)
		return nil, err
	}
	d.auditCall(ctx, opts, server, grant, duration, ai.CallStatusSuccess, nil)
	return result, nil
}

// findToolGrant resolves the per-tool grant. Returns ErrToolNotWhitelisted
// when no row exists — operator never accidentally exposes a tool
// they didn't review, even if the live server advertises more.
func (d *Dispatcher) findToolGrant(ctx context.Context, serverID uuid.UUID, tool string) (mcpregistry.ToolGrant, error) {
	grants, err := d.registry.ListToolGrants(ctx, serverID)
	if err != nil {
		return mcpregistry.ToolGrant{}, err
	}
	for _, g := range grants {
		if g.ToolName == tool {
			return g, nil
		}
	}
	return mcpregistry.ToolGrant{}, fmt.Errorf("%w: %q", ErrToolNotWhitelisted, tool)
}

// auditCall writes the successful (or provider-error) call to
// ai_provider_call. provider=server name; model=tool name; concern=
// 'complete' since MCP tools don't map to any of the typed concern
// enum values + we don't want to extend the CHECK constraint for an
// extension surface that's still maturing.
func (d *Dispatcher) auditCall(
	ctx context.Context,
	opts InvokeOpts,
	server mcpregistry.Server,
	grant mcpregistry.ToolGrant,
	duration time.Duration,
	status ai.CallStatus,
	callErr error,
) {
	if d.auditor == nil {
		return
	}
	rec := ai.CallRecord{
		Provider:               server.Name,
		Model:                  opts.Tool,
		Concern:                ai.ConcernComplete,
		Duration:               duration,
		Status:                 status,
		EstimatedCostUSDMicros: grant.CostEstimateMicros,
		InputHash:              ai.CanonicalInputHash(opts.Tool, fmt.Sprintf("%v", opts.Arguments)),
	}
	if opts.AssetID != uuid.Nil {
		id := opts.AssetID
		rec.AssetID = &id
	}
	if opts.Caller != nil {
		actorRef := opts.Caller.UserRef
		rec.ActorUserRef = &actorRef
	}
	if callErr != nil {
		rec.ErrorMessage = callErr.Error()
	}
	d.auditor.RecordCall(ctx, rec)
}

// auditBlocked writes the gated-out call to ai_provider_call so
// operator dashboards surface "5 calls blocked by privacy policy
// last week" as actionable signal — even when no HTTP went out.
func (d *Dispatcher) auditBlocked(
	ctx context.Context,
	opts InvokeOpts,
	server mcpregistry.Server,
	status ai.CallStatus,
	reason error,
) {
	if d.auditor == nil {
		return
	}
	d.auditCall(ctx, opts, server, mcpregistry.ToolGrant{}, 0, status, reason)
}

// classifyErrorStatus maps a provider error class to the
// ai_provider_call.status enum.
func classifyErrorStatus(err error) ai.CallStatus {
	pe, ok := ai.AsProviderError(err)
	if !ok {
		return ai.CallStatusPermanentError
	}
	switch pe.Class {
	case ai.ErrClassTransient:
		return ai.CallStatusTransientError
	case ai.ErrClassRateLimit:
		return ai.CallStatusRateLimited
	case ai.ErrClassBudget:
		return ai.CallStatusBudgetBlocked
	case ai.ErrClassPrivacy:
		return ai.CallStatusPrivacyBlocked
	default:
		return ai.CallStatusPermanentError
	}
}
