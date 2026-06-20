package ai

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// Five cache domains for the AI subsystem (per Phase 1.14.A brief).
// Each value is the stable NOTIFY channel name; the cache.Registry
// uses it as both the local LRU identity and the cross-instance
// broadcast topic. Adding a sixth domain means appending here, wiring
// it in NewCaches, and adding the corresponding invalidate hook on
// the write path.
const (
	// cacheDomainAIProviderConfig — global (single entry). The whole
	// parsed Config object is the value. Invalidated on any
	// system_config write to an `ai.*` key.
	cacheDomainAIProviderConfig = "ai.provider_config"

	// cacheDomainAIBudgetUsage — per (provider, billing_period).
	// Hot read path for budget gating; the cost.Tracker increments
	// the cached entry on each successful call rather than
	// re-summing the ai_provider_call table.
	cacheDomainAIBudgetUsage = "ai.budget_usage"

	// cacheDomainAITagsForAsset — per asset. Cached so the asset
	// detail page doesn't re-query on every render.
	cacheDomainAITagsForAsset = "ai.tags_for_asset"

	// cacheDomainAICaptionForAsset — per asset.
	cacheDomainAICaptionForAsset = "ai.caption_for_asset"

	// cacheDomainAIPromptTemplate — per (concern, version). Tiny;
	// the registry rarely changes but the read is on every inference
	// call's hot path.
	cacheDomainAIPromptTemplate = "ai.prompt_template"
)

// Default sizes per the brief. Operator-tunable later; for now these
// are the values NewCaches uses unconditionally.
const (
	cacheSizeProviderConfig  = 8      // single entry plus headroom for cache-domain accounting
	cacheSizeBudgetUsage     = 100    // ~10 providers × 10 months retained
	cacheSizeTagsForAsset    = 50_000 // brief default
	cacheSizeCaptionForAsset = 50_000 // brief default
	cacheSizePromptTemplate  = 100    // ~5 concerns × ~20 versions; generous
)

// Caches bundles the 5 AI domain caches. Constructed once at boot
// and passed to every consumer (Router, cost.Tracker, prompt
// registry refresh, tag/caption read paths). All fields are
// nil-safe at call sites that handle the nil case explicitly; the
// nil-Caches helper below covers tests that don't wire a registry.
type Caches struct {
	ProviderConfig  *cache.Cache[ProviderConfigEntry]
	BudgetUsage     *cache.Cache[BudgetUsage]
	TagsForAsset    *cache.Cache[[]Tag]
	CaptionForAsset *cache.Cache[string]
	PromptTemplate  *cache.Cache[Template]
}

// ProviderConfigEntry is the small wrapper around the parsed Config
// that lets cache.Cache hold a single global value via the
// well-known key cacheKeyProviderConfig.
type ProviderConfigEntry struct {
	Config Config
}

// BudgetUsage is one cached row from the cost-tracker rollup. Keyed
// by (provider, billing_period) at the cache layer via budgetKey().
type BudgetUsage struct {
	Provider       string
	BillingPeriod  string // "YYYY-MM"
	SpentMicros    int64
	SoftWarnMicros int64
	HardCapMicros  int64
}

// cacheKeyProviderConfig is the sole key for the
// ai.provider_config domain. Single-entry global; the key is a
// stable sentinel so every reader hits the same slot.
const cacheKeyProviderConfig = "global"

// budgetKey is the composite cache key for the budget-usage domain.
// "<provider>|<YYYY-MM>" — provider names can't contain `|` per the
// admin UI validator (codePattern in metadata; same shape).
func budgetKey(provider, billingPeriod string) string {
	return provider + "|" + billingPeriod
}

// promptKeyString is the composite cache key for the prompt-template
// domain. Mirrors the in-package promptKey struct as a flat string
// so cache.Cache[Template] (which is keyed by string per the
// Registry API) can hold it.
func promptKeyString(concern Concern, version string) string {
	return string(concern) + "|" + version
}

// assetKey is the cache key for tag + caption per-asset caches.
// Plain string form of the UUID for cross-instance NOTIFY
// compatibility.
func assetKey(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		id.Bytes[0:4], id.Bytes[4:6], id.Bytes[6:8], id.Bytes[8:10], id.Bytes[10:16])
}

// NewCaches registers all 5 domains against the shared registry.
// Pass a nil registry to get an all-nil Caches (test-friendly:
// callers that handle the nil case skip the cache layer entirely
// and go straight to the DB).
func NewCaches(registry *cache.Registry) *Caches {
	if registry == nil {
		return &Caches{}
	}
	return &Caches{
		ProviderConfig:  cache.Register[ProviderConfigEntry](registry, cacheDomainAIProviderConfig, cacheSizeProviderConfig),
		BudgetUsage:     cache.Register[BudgetUsage](registry, cacheDomainAIBudgetUsage, cacheSizeBudgetUsage),
		TagsForAsset:    cache.Register[[]Tag](registry, cacheDomainAITagsForAsset, cacheSizeTagsForAsset),
		CaptionForAsset: cache.Register[string](registry, cacheDomainAICaptionForAsset, cacheSizeCaptionForAsset),
		PromptTemplate:  cache.Register[Template](registry, cacheDomainAIPromptTemplate, cacheSizePromptTemplate),
	}
}

// InvalidateProviderConfig drops the global config entry and
// broadcasts. Called from the admin handlers whenever an `ai.*`
// system_config key is written.
func (c *Caches) InvalidateProviderConfig(ctx context.Context) error {
	if c == nil || c.ProviderConfig == nil {
		return nil
	}
	return c.ProviderConfig.Invalidate(ctx, cacheKeyProviderConfig)
}

// InvalidateBudgetUsage drops the (provider, period) entry and
// broadcasts. Called by cost.Tracker after a write OR when the
// admin manually adjusts the budget.
func (c *Caches) InvalidateBudgetUsage(ctx context.Context, provider, billingPeriod string) error {
	if c == nil || c.BudgetUsage == nil {
		return nil
	}
	return c.BudgetUsage.Invalidate(ctx, budgetKey(provider, billingPeriod))
}

// InvalidateTagsForAsset drops the tag list for one asset.
func (c *Caches) InvalidateTagsForAsset(ctx context.Context, id pgtype.UUID) error {
	if c == nil || c.TagsForAsset == nil {
		return nil
	}
	return c.TagsForAsset.Invalidate(ctx, assetKey(id))
}

// InvalidateCaptionForAsset drops the caption for one asset.
func (c *Caches) InvalidateCaptionForAsset(ctx context.Context, id pgtype.UUID) error {
	if c == nil || c.CaptionForAsset == nil {
		return nil
	}
	return c.CaptionForAsset.Invalidate(ctx, assetKey(id))
}

// InvalidatePromptTemplate drops one (concern, version) template.
func (c *Caches) InvalidatePromptTemplate(ctx context.Context, concern Concern, version string) error {
	if c == nil || c.PromptTemplate == nil {
		return nil
	}
	return c.PromptTemplate.Invalidate(ctx, promptKeyString(concern, version))
}
