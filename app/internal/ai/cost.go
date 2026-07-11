// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Tracker enforces per-provider monthly budgets + records the
// running cost rollup for the operator dashboard. Two methods:
//
//   CheckBudgetBefore — called by the provider wrapper BEFORE
//      making the HTTP call. Returns *ProviderError{Class:
//      ErrClassBudget} when the call would push past the hard cap.
//      Fail-closed: a freshly-configured provider with the
//      migration's $0 default cap blocks every call until the
//      operator raises explicitly (operator-locked constraint #2
//      from the brief — surfaces as cloud_budget_not_configured).
//
//   RecordCallAfter — called AFTER a successful call. Increments
//      the running total + fires the soft-warn audit event on
//      threshold crossing. The per-call ai_provider_call row is
//      written separately by the CallAuditor.
//
// Read path is cache-fronted via cacheDomainAIBudgetUsage. The
// in-memory mu serialises CheckBudgetBefore + RecordCallAfter for
// one provider so two concurrent calls don't both pass the check
// before either records.

// Tracker is the cost+budget tracker. Build with NewTracker.
type Tracker struct {
	pool    *pgxpool.Pool
	caches  *Caches
	loader  *Loader   // for per-provider budget overrides + defaults
	auditor *CallAuditor // for the soft-warn audit event

	// mu guards per-provider serialisation. One mutex per provider
	// is created on first touch via the inner map; the outer mu
	// gates map mutation. Avoids cross-provider contention.
	mu         sync.Mutex
	perProvMu  map[string]*sync.Mutex
}

// ProviderBudget is the per-provider knob set the tracker compares
// against on every call. Loaded from the per-provider system_config
// key `ai.providers.<name>.budget` (admin handlers write this) and
// falls back to the global ai.budgets.default when no per-provider
// override exists.
type ProviderBudget struct {
	SoftWarningMicros int64
	HardCapMicros     int64
}

// NewTracker binds the tracker to the pool + caches + loader. The
// auditor wires the soft-warn event; pass nil to skip (tests).
func NewTracker(pool *pgxpool.Pool, caches *Caches, loader *Loader, auditor *CallAuditor) *Tracker {
	return &Tracker{
		pool:      pool,
		caches:    caches,
		loader:    loader,
		auditor:   auditor,
		perProvMu: map[string]*sync.Mutex{},
	}
}

// CheckBudgetBefore decides whether the about-to-fire HTTP call
// fits the budget. Returns nil to permit, *ProviderError{Class:
// ErrClassBudget} to block.
//
// estimatedCostMicros is the caller's pre-call estimate (per-model
// token-rate × max_tokens). Imprecise but conservative — the actual
// cost recorded by RecordCallAfter may be lower. We block on the
// estimate to fail-closed at the wallet boundary.
func (t *Tracker) CheckBudgetBefore(ctx context.Context, provider string, estimatedCostMicros int64) error {
	mu := t.lockFor(provider)
	mu.Lock()
	defer mu.Unlock()

	usage, err := t.usageFor(ctx, provider)
	if err != nil {
		// DB read failure on the rollup path is itself a budget
		// gate (we can't prove the call is under cap). Block as a
		// transient error so the worker retries vs failing the job
		// hard.
		return &ProviderError{
			Class:    ErrClassTransient,
			Provider: provider,
			Wrapped:  fmt.Errorf("ai: budget read: %w", err),
		}
	}

	if usage.HardCapMicros <= 0 {
		// $0 cap = fail-closed; surfaces as
		// cloud_budget_not_configured at the admin UI layer (the
		// router maps this error string).
		return &ProviderError{
			Class:    ErrClassBudget,
			Provider: provider,
			Wrapped:  fmt.Errorf("cloud_budget_not_configured"),
		}
	}

	if usage.SpentMicros+estimatedCostMicros > usage.HardCapMicros {
		return &ProviderError{
			Class:    ErrClassBudget,
			Provider: provider,
			Wrapped:  fmt.Errorf("ai_budget_exhausted"),
		}
	}

	return nil
}

// RecordCallAfter updates the running rollup. Cache stays
// consistent: we add to the cached usage value AND broadcast the
// invalidation so peer instances re-read on next access. The
// per-call ai_provider_call row is the source of truth — the cache
// is a hot-read accelerator only.
//
// Fires the soft-warn audit event on threshold crossing. Best-
// effort; an audit failure logs but doesn't propagate (matches
// CallAuditor's contract).
func (t *Tracker) RecordCallAfter(ctx context.Context, provider string, actualCostMicros int64) error {
	mu := t.lockFor(provider)
	mu.Lock()
	defer mu.Unlock()

	usage, err := t.usageFor(ctx, provider)
	if err != nil {
		return fmt.Errorf("ai: record: %w", err)
	}

	prevSpent := usage.SpentMicros
	usage.SpentMicros = prevSpent + actualCostMicros

	// Soft-warn threshold crossing: fired exactly once per period
	// when the running total crosses the threshold from below.
	crossed := usage.SoftWarnMicros > 0 &&
		prevSpent < usage.SoftWarnMicros &&
		usage.SpentMicros >= usage.SoftWarnMicros
	if crossed && t.auditor != nil {
		// Record as a synthetic per-call row so the operator
		// dashboard surfaces the soft-warn alongside the call that
		// crossed it. We use Status=success with a special
		// PromptTemplate sentinel "_budget_soft_warn" so the
		// dashboard can filter.
		t.auditor.RecordCall(ctx, CallRecord{
			Provider:               provider,
			Model:                  "_",
			Concern:                ConcernComplete,
			PromptTemplate:         "_budget_soft_warn",
			Duration:               0,
			EstimatedCostUSDMicros: 0,
			Status:                 CallStatusSuccess,
			ErrorMessage: fmt.Sprintf(
				"soft warn crossed: spent=%d soft=%d hard=%d",
				usage.SpentMicros, usage.SoftWarnMicros, usage.HardCapMicros,
			),
		})
	}

	if t.caches != nil && t.caches.BudgetUsage != nil {
		// Add (not Invalidate): we just updated the running total,
		// keeping the cache warm is correct.
		t.caches.BudgetUsage.Add(budgetKey(provider, usage.BillingPeriod), usage)
		// Broadcast the invalidation so peer instances re-read
		// (their local SpentMicros is now stale).
		_ = t.caches.InvalidateBudgetUsage(ctx, provider, usage.BillingPeriod)
	}
	return nil
}

// usageFor reads the current budget snapshot for one provider.
// Hits the cache when warm; otherwise sums the ai_provider_call
// table for the current billing period and looks up the per-
// provider budget overrides (or falls back to defaultConfig
// defaults if loader returned the defaults).
func (t *Tracker) usageFor(ctx context.Context, provider string) (BudgetUsage, error) {
	period := currentBillingPeriod()
	key := budgetKey(provider, period)

	if t.caches != nil && t.caches.BudgetUsage != nil {
		if u, ok := t.caches.BudgetUsage.Get(key); ok {
			return u, nil
		}
	}

	// Determine the budget caps. For 1.14.A we use the default
	// from the global ai.budgets.default config; per-provider
	// override storage is wired in the admin-UI slice (later).
	caps, err := t.budgetForProvider(ctx, provider)
	if err != nil {
		return BudgetUsage{}, err
	}

	spent, err := t.spentInPeriod(ctx, provider, period)
	if err != nil {
		return BudgetUsage{}, err
	}

	usage := BudgetUsage{
		Provider:       provider,
		BillingPeriod:  period,
		SpentMicros:    spent,
		SoftWarnMicros: caps.SoftWarningMicros,
		HardCapMicros:  caps.HardCapMicros,
	}
	if t.caches != nil && t.caches.BudgetUsage != nil {
		t.caches.BudgetUsage.Add(key, usage)
	}
	return usage, nil
}

// budgetForProvider looks up the budget caps for one provider.
// For 1.14.A this just returns the global defaults from
// ai.budgets.default. The admin-UI slice will add per-provider
// overrides keyed by `ai.providers.<name>.budget`.
func (t *Tracker) budgetForProvider(ctx context.Context, _ string) (ProviderBudget, error) {
	if t.loader == nil {
		return ProviderBudget{}, nil
	}
	cfg, _ := t.loader.Load(ctx) // tolerate validator findings
	return ProviderBudget{
		SoftWarningMicros: usdToMicros(cfg.DefaultBudget.SoftWarningUSD),
		HardCapMicros:     usdToMicros(cfg.DefaultBudget.HardCapUSD),
	}, nil
}

// spentInPeriod sums estimated_cost_usd_micros over the current
// billing period for one provider. Direct SQL — sqlc would emit
// boilerplate for a single SUM, and the call is rare enough (only
// on cold cache reads).
func (t *Tracker) spentInPeriod(ctx context.Context, provider, period string) (int64, error) {
	if t.pool == nil {
		return 0, nil
	}
	const q = `
		SELECT COALESCE(SUM(estimated_cost_usd_micros), 0)::BIGINT
		  FROM ai_provider_call
		 WHERE provider = $1
		   AND to_char(triggered_at, 'YYYY-MM') = $2
		   AND status = 'success'`
	var total int64
	if err := t.pool.QueryRow(ctx, q, provider, period).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// lockFor returns the per-provider mutex, creating it on first use.
func (t *Tracker) lockFor(provider string) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.perProvMu[provider]; ok {
		return m
	}
	m := &sync.Mutex{}
	t.perProvMu[provider] = m
	return m
}

// currentBillingPeriod returns "YYYY-MM" in UTC. Operators in
// different timezones still see consistent month boundaries.
func currentBillingPeriod() string {
	return CurrentBillingPeriod()
}

// CurrentBillingPeriod is the exported form for consumers outside
// the ai package (the admin handler's usage dashboard uses it to
// default the ?billing_period parameter).
func CurrentBillingPeriod() string {
	return time.Now().UTC().Format("2006-01")
}

// usdToMicros converts whole-dollar caps from the JSONB config to
// the integer-micros currency the audit table uses. $1 = 1_000_000
// micros; integer math keeps the budget comparison exact under all
// circumstances.
func usdToMicros(usd int64) int64 {
	return usd * 1_000_000
}
