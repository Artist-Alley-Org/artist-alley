// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package admin provides the HTTP surface for the AI inference
// subsystem's operator-facing endpoints (Phase 1.14.A):
//
//   GET  /admin/ai/config  → AIInferenceConfig + validator findings
//   PUT  /admin/ai/config  → replace + validate
//   GET  /admin/ai/usage   → per-provider cost rollup for one period
//
// All three require the ai.admin capability (seeded for the Admin
// role by migration 00009). The capability gate runs inside each
// handler — apiServer doesn't enforce per-route caps automatically
// (that's a future ADR).
//
// The package is intentionally thin: business logic lives in
// app/internal/ai (Loader, Tracker, validator). This wrapper does
// auth + shape mapping only.
package admin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// CapAIAdmin is the capability code gating every endpoint in this
// package. Seeded for the Admin role in migration 00009.
const CapAIAdmin = "ai.admin"

// Handler wires the package's endpoints to the underlying AI
// machinery. Constructed by apiServer; nil-safe for tests that
// don't exercise these endpoints.
type Handler struct {
	pool   *pgxpool.Pool
	loader *ai.Loader
	caches *ai.Caches
}

// NewHandler builds a Handler. caches may be nil (no cache layer —
// every read hits the DB); loader is required.
func NewHandler(pool *pgxpool.Pool, loader *ai.Loader, caches *ai.Caches) *Handler {
	return &Handler{pool: pool, loader: loader, caches: caches}
}

// ---------------------------------------------------------------------------
// GET /admin/ai/config
// ---------------------------------------------------------------------------

// GetAIInferenceConfig returns the parsed Config snapshot plus any
// validator findings the admin UI can render inline.
func (h *Handler) GetAIInferenceConfig(
	ctx context.Context,
	_ openapi.GetAIInferenceConfigRequestObject,
) (openapi.GetAIInferenceConfigResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetAIInferenceConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapAIAdmin) && !id.Can(auth.SuperAdminCapability) {
		return openapi.GetAIInferenceConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapAIAdmin + " capability required"},
		}, nil
	}

	cfg, validateErr := h.loader.Load(ctx)
	// validateErr is non-nil when the validator surfaces findings;
	// the Config snapshot is still returned so the admin UI shows
	// what's currently stored alongside the findings list.
	resp := configToAPI(cfg, validateErr)
	return openapi.GetAIInferenceConfig200JSONResponse(resp), nil
}

// ---------------------------------------------------------------------------
// PUT /admin/ai/config
// ---------------------------------------------------------------------------

// UpdateAIInferenceConfig writes the supplied config to system_config
// (six rows, one per key) inside a single transaction. Pre-validates
// before writing — invalid input returns 422 with findings and
// leaves the existing config untouched.
//
// Cache invalidation broadcasts via the registry so peer instances
// pick up the new values on next read.
func (h *Handler) UpdateAIInferenceConfig(
	ctx context.Context,
	req openapi.UpdateAIInferenceConfigRequestObject,
) (openapi.UpdateAIInferenceConfigResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UpdateAIInferenceConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapAIAdmin) && !id.Can(auth.SuperAdminCapability) {
		return openapi.UpdateAIInferenceConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapAIAdmin + " capability required"},
		}, nil
	}
	if req.Body == nil {
		// No body = no-op return-current; same shape as GET so the
		// client can render without branching.
		cfg, validateErr := h.loader.Load(ctx)
		return openapi.UpdateAIInferenceConfig200JSONResponse(configToAPI(cfg, validateErr)), nil
	}

	parsed := writeBodyToConfig(*req.Body)

	// Validate before writing. Blocking findings → 422.
	if err := parsed.Validate(); err != nil {
		findings := findingsFromError(err)
		return openapi.UpdateAIInferenceConfig422JSONResponse{
			Error:    err.Error(),
			Findings: findings,
		}, nil
	}

	// Persist atomically — one row per key, all-or-nothing.
	if err := h.writeConfigKeys(ctx, parsed); err != nil {
		return nil, err
	}

	// Cache broadcast so peers re-read.
	_ = h.loader.InvalidateOnConfigWrite(ctx)

	// Re-read so the response reflects what's actually stored
	// (defensive: any DB-side default fill-in surfaces here).
	cfg, validateErr := h.loader.Load(ctx)
	return openapi.UpdateAIInferenceConfig200JSONResponse(configToAPI(cfg, validateErr)), nil
}

// writeConfigKeys persists the six AI config keys in one tx.
func (h *Handler) writeConfigKeys(ctx context.Context, cfg ai.Config) error {
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const upsertQ = `
		INSERT INTO system_config (key, value, updated_at)
		VALUES ($1, $2::jsonb, NOW())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`

	enabledJSON, _ := json.Marshal(cfg.Enabled)
	if _, err := tx.Exec(ctx, upsertQ, "ai.enabled", enabledJSON); err != nil {
		return err
	}

	routingJSON, _ := json.Marshal(stringMapFromRouting(cfg.Routing))
	if _, err := tx.Exec(ctx, upsertQ, "ai.routing", routingJSON); err != nil {
		return err
	}

	chainsJSON, _ := json.Marshal(stringMapFromChains(cfg.FallbackChains))
	if _, err := tx.Exec(ctx, upsertQ, "ai.fallback_chains", chainsJSON); err != nil {
		return err
	}

	lockJSON, _ := json.Marshal(cfg.Privacy.LockSensitiveToLocal)
	if _, err := tx.Exec(ctx, upsertQ, "ai.privacy.lock_sensitive_to_local", lockJSON); err != nil {
		return err
	}

	localJSON, _ := json.Marshal(cfg.Privacy.LocalProviders)
	if _, err := tx.Exec(ctx, upsertQ, "ai.privacy.local_providers", localJSON); err != nil {
		return err
	}

	budgetJSON, _ := json.Marshal(cfg.DefaultBudget)
	if _, err := tx.Exec(ctx, upsertQ, "ai.budgets.default", budgetJSON); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// GET /admin/ai/usage
// ---------------------------------------------------------------------------

// GetAIUsage rolls up ai_provider_call rows for one billing period
// + groups by provider. status_counts breaks down per-status row
// volume so the operator dashboard can spot providers stuck on
// rate-limited / permanent-error states.
func (h *Handler) GetAIUsage(
	ctx context.Context,
	req openapi.GetAIUsageRequestObject,
) (openapi.GetAIUsageResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetAIUsage401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapAIAdmin) && !id.Can(auth.SuperAdminCapability) {
		return openapi.GetAIUsage403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapAIAdmin + " capability required"},
		}, nil
	}

	period := currentBillingPeriod()
	if req.Params.BillingPeriod != nil && *req.Params.BillingPeriod != "" {
		period = *req.Params.BillingPeriod
	}

	report, err := h.queryUsage(ctx, period)
	if err != nil {
		return nil, err
	}
	return openapi.GetAIUsage200JSONResponse(report), nil
}

// queryUsage runs the per-provider rollup query. Two passes:
// totals + per-status grouped counts; merged in Go to keep the SQL
// straightforward.
func (h *Handler) queryUsage(ctx context.Context, period string) (openapi.AIUsageReport, error) {
	report := openapi.AIUsageReport{BillingPeriod: period, Providers: []openapi.AIUsageProviderRow{}}

	const totalsQ = `
		SELECT provider,
		       COUNT(*) FILTER (WHERE status = 'success')::BIGINT AS success_calls,
		       COALESCE(SUM(estimated_cost_usd_micros) FILTER (WHERE status = 'success'), 0)::BIGINT AS cost_micros
		  FROM ai_provider_call
		 WHERE to_char(triggered_at, 'YYYY-MM') = $1
		 GROUP BY provider
		 ORDER BY provider`

	rows, err := h.pool.Query(ctx, totalsQ, period)
	if err != nil {
		return report, err
	}
	defer rows.Close()

	byProvider := map[string]*openapi.AIUsageProviderRow{}
	for rows.Next() {
		var prov string
		var calls, cost int64
		if err := rows.Scan(&prov, &calls, &cost); err != nil {
			return report, err
		}
		statusCounts := map[string]int64{}
		row := openapi.AIUsageProviderRow{
			Provider:      prov,
			CallCount:     calls,
			CostUsdMicros: cost,
			StatusCounts:  statusCounts,
		}
		byProvider[prov] = &row
	}
	if err := rows.Err(); err != nil {
		return report, err
	}

	// Per-status counts (covers all rows, not just success).
	const statusQ = `
		SELECT provider, status, COUNT(*)::BIGINT
		  FROM ai_provider_call
		 WHERE to_char(triggered_at, 'YYYY-MM') = $1
		 GROUP BY provider, status`

	srows, err := h.pool.Query(ctx, statusQ, period)
	if err != nil {
		return report, err
	}
	defer srows.Close()
	for srows.Next() {
		var prov, status string
		var n int64
		if err := srows.Scan(&prov, &status, &n); err != nil {
			return report, err
		}
		row, ok := byProvider[prov]
		if !ok {
			// Provider had only non-success rows in the period
			// (every call rate-limited, etc.) — surface it too.
			row = &openapi.AIUsageProviderRow{
				Provider:     prov,
				StatusCounts: map[string]int64{},
			}
			byProvider[prov] = row
		}
		row.StatusCounts[status] = n
	}
	if err := srows.Err(); err != nil {
		return report, err
	}

	// Flatten + total. Map iteration order is non-deterministic;
	// we ordered providers alphabetically in the totals query
	// already, but we have to re-flatten via the map. Sort by name
	// for stable client rendering.
	var total int64
	names := make([]string, 0, len(byProvider))
	for n := range byProvider {
		names = append(names, n)
	}
	stableSort(names)
	report.Providers = make([]openapi.AIUsageProviderRow, 0, len(names))
	for _, n := range names {
		report.Providers = append(report.Providers, *byProvider[n])
		total += byProvider[n].CostUsdMicros
	}
	report.TotalCostUsdMicros = total
	return report, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func configToAPI(cfg ai.Config, validateErr error) openapi.AIInferenceConfig {
	out := openapi.AIInferenceConfig{
		Enabled:        cfg.Enabled,
		Routing:        stringMapFromRouting(cfg.Routing),
		FallbackChains: stringMapFromChains(cfg.FallbackChains),
		Privacy: openapi.AIPrivacyPolicy{
			LockSensitiveToLocal: cfg.Privacy.LockSensitiveToLocal,
			LocalProviders:       cfg.Privacy.LocalProviders,
		},
		DefaultBudget: openapi.AIBudgetDefaults{
			SoftWarningUsd: cfg.DefaultBudget.SoftWarningUSD,
			HardCapUsd:     cfg.DefaultBudget.HardCapUSD,
		},
	}
	if validateErr != nil {
		findings := findingsFromError(validateErr)
		out.Findings = &findings
	}
	return out
}

func writeBodyToConfig(body openapi.AIInferenceConfigWrite) ai.Config {
	cfg := ai.Config{
		Enabled:        body.Enabled,
		Routing:        routingFromStringMap(body.Routing),
		FallbackChains: chainsFromStringMap(body.FallbackChains),
		Privacy: ai.PrivacyPolicy{
			LockSensitiveToLocal: body.Privacy.LockSensitiveToLocal,
			LocalProviders:       body.Privacy.LocalProviders,
		},
		DefaultBudget: ai.BudgetDefaults{
			SoftWarningUSD: body.DefaultBudget.SoftWarningUsd,
			HardCapUSD:     body.DefaultBudget.HardCapUsd,
		},
	}
	return cfg
}

func stringMapFromRouting(r map[ai.Concern]string) map[string]string {
	out := make(map[string]string, len(r))
	for k, v := range r {
		out[string(k)] = v
	}
	return out
}

func stringMapFromChains(r map[ai.Concern][]string) map[string][]string {
	out := make(map[string][]string, len(r))
	for k, v := range r {
		out[string(k)] = v
	}
	return out
}

func routingFromStringMap(m map[string]string) map[ai.Concern]string {
	out := make(map[ai.Concern]string, len(m))
	for k, v := range m {
		out[ai.Concern(k)] = v
	}
	return out
}

func chainsFromStringMap(m map[string][]string) map[ai.Concern][]string {
	out := make(map[ai.Concern][]string, len(m))
	for k, v := range m {
		out[ai.Concern(k)] = v
	}
	return out
}

func findingsFromError(err error) []openapi.AIConfigFinding {
	var inv *ai.ErrConfigInvalid
	if !errors.As(err, &inv) {
		return []openapi.AIConfigFinding{{
			Code:    "internal",
			Message: err.Error(),
		}}
	}
	out := make([]openapi.AIConfigFinding, 0, len(inv.Findings))
	for _, f := range inv.Findings {
		entry := openapi.AIConfigFinding{Code: f.Code, Message: f.Message}
		if f.Concern != "" {
			c := openapi.AIConfigFindingConcern(f.Concern)
			entry.Concern = &c
		}
		out = append(out, entry)
	}
	return out
}

// currentBillingPeriod mirrors the cost.Tracker helper; kept local
// to avoid importing the unexported function from the ai package.
func currentBillingPeriod() string {
	return ai.CurrentBillingPeriod()
}

// stableSort is a small selection sort to avoid importing the
// sort package just for the one call in queryUsage. The provider
// list is small (typically <10 entries) so O(n²) is fine.
func stableSort(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}
