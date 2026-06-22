// Phase 1.53.A — per-server MCP health-check goroutine.
//
// # Why per-server (not centralised poller)
//
// One slow server shouldn't degrade health-check cadence for others.
// Same isolation pattern as the userkeys sweeper from 1.22.I-h:
// boot launches one goroutine per enabled registration; goroutine
// exits when the parent ctx cancels.
//
// # Cadence
//
// Each server's `health_check_interval_s` column overrides the
// default. Default 60s, max plausible 24h (no upper-bound CHECK in
// the schema — operator can set as long as they want).
//
// # What each tick does
//
//   1. Provider.ListTools(ctx) round-trip → success/failure classifies
//      to 'healthy' / 'unreachable' / 'degraded' (per HTTP status).
//   2. Registry.UpdateHealthStatus persists the result + clears the
//      previous error on recovery.
//   3. If system_config.mcp.client.tool_list_refresh_on_health is
//      true (the seed default), the tools-cache for this server is
//      invalidated so the next admin-UI read pulls the fresh list.
//
// # No retries inside one tick
//
// A single failed tick records the failure; the next tick tries
// again. Spamming the server on a quick retry would just amplify
// load on a struggling endpoint.

package mcpdispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	mcpregistry "github.com/mscrnt/artist-alley/app/internal/ai/mcp_registry"
	mcpserver "github.com/mscrnt/artist-alley/app/internal/ai/providers/mcp_server"
)

// HealthChecker manages the per-server polling goroutines.
type HealthChecker struct {
	registry  *mcpregistry.Registry
	providers ProviderRegistry
	logger    *slog.Logger
}

// NewHealthChecker constructs the supervisor. Start launches one
// goroutine per currently-enabled server; the supervisor itself
// returns when the parent context cancels.
func NewHealthChecker(
	registry *mcpregistry.Registry,
	providers ProviderRegistry,
	logger *slog.Logger,
) *HealthChecker {
	return &HealthChecker{
		registry:  registry,
		providers: providers,
		logger:    logger,
	}
}

// Run reads the enabled-server list once + spawns one polling
// goroutine per server. Returns when ctx is done.
//
// Operator changes (add/disable/re-enable a server) require an app
// restart to take effect on the goroutine set; a future enhancement
// will reload on system_config change via the existing 1.10 NOTIFY/
// LISTEN pattern. For now, restart-to-rewire is acceptable since
// MCP server registration is an infrequent operator action.
func (h *HealthChecker) Run(ctx context.Context) error {
	servers, err := h.registry.ListEnabledServers(ctx)
	if err != nil {
		return fmt.Errorf("healthcheck: list enabled servers: %w", err)
	}
	if h.logger != nil {
		h.logger.Info("mcp.healthcheck.start", "enabled_servers", len(servers))
	}
	for _, server := range servers {
		go h.runForServer(ctx, server)
	}
	<-ctx.Done()
	return nil
}

// runForServer polls one server forever (until ctx is done). Logs
// per-tick status only when it CHANGES — steady-state healthy /
// unreachable doesn't spam the log; recovery + new failure do.
func (h *HealthChecker) runForServer(ctx context.Context, server mcpregistry.Server) {
	interval := time.Duration(server.HealthCheckIntervalS) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// First tick immediately so the admin UI sees a populated
	// status within seconds of boot instead of waiting `interval`
	// for the first poll.
	h.pollOnce(ctx, server)

	var lastStatus string
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			status := h.pollOnce(ctx, server)
			if status != lastStatus && h.logger != nil {
				h.logger.Info("mcp.healthcheck.status_changed",
					"server", server.Name,
					"status", status,
					"prev_status", lastStatus)
			}
			lastStatus = status
		}
	}
}

// pollOnce does one ListTools round-trip + persists the outcome.
// Returns the resulting status string ('healthy' / 'degraded' /
// 'unreachable') so the caller can detect transitions.
func (h *HealthChecker) pollOnce(ctx context.Context, server mcpregistry.Server) string {
	prov, ok := h.providers.Provider(server.Name)
	if !ok {
		// Provider missing from router → boot-wire bug; treat as
		// unreachable so the admin UI surfaces "something's wrong"
		// rather than a stale healthy badge.
		_ = h.registry.UpdateHealthStatus(ctx, server.ID, "unreachable",
			"provider not registered with router (boot-wire bug)")
		return "unreachable"
	}

	// Per-poll deadline at half the configured interval so a slow
	// server doesn't pile up overlapping ticks.
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(server.HealthCheckIntervalS/2)*time.Second)
	defer cancel()

	tools, err := prov.ListTools(pollCtx)
	if err != nil {
		status := "unreachable"
		// HTTP-level errors with the server reachable but mis-
		// configured → degraded (operator can fix without
		// network changes); pure transport errors → unreachable.
		if pe, ok := ai.AsProviderError(err); ok && pe.Class == ai.ErrClassPermanent {
			status = "degraded"
		}
		_ = h.registry.UpdateHealthStatus(ctx, server.ID, status, errMessage(err))
		return status
	}

	if err := h.registry.UpdateHealthStatus(ctx, server.ID, "healthy", ""); err != nil && h.logger != nil {
		h.logger.Warn("mcp.healthcheck.persist_failed",
			"server", server.Name, "err", err.Error())
	}
	h.maybeRefreshToolListCache(ctx, server.ID, tools)
	return "healthy"
}

// maybeRefreshToolListCache is a placeholder hook for the
// system_config.mcp.client.tool_list_refresh_on_health flag. The
// 1.10 cache already invalidates via NOTIFY whenever the registry
// updates the server row; the tool list itself is a per-server
// cache the admin UI surfaces. For v1 we just log the count —
// admin UI re-reads via the registry's cached path on its next
// query, which the UpdateHealthStatus call invalidated.
func (h *HealthChecker) maybeRefreshToolListCache(_ context.Context, serverID uuid.UUID, tools []mcpserver.ListedTool) {
	if h.logger != nil {
		h.logger.Debug("mcp.healthcheck.tools_refreshed",
			"server_id", serverID.String(),
			"tool_count", len(tools))
	}
}

func errMessage(err error) string {
	if err == nil {
		return ""
	}
	// Truncate so a verbose provider error doesn't bloat the
	// last_health_error column.
	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	return msg
}

// Ensure we import the errors package even when no direct use
// remains after a future refactor.
var _ = errors.New
