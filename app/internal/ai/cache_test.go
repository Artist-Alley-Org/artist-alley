// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"testing"
)

// NewCaches with nil registry returns an all-nil Caches struct so
// tests + nil-aware callers behave the same way.
func TestNewCaches_NilRegistry_ReturnsNilFields(t *testing.T) {
	c := NewCaches(nil)
	if c == nil {
		t.Fatal("NewCaches(nil) returned nil pointer; want zero-value struct")
	}
	if c.ProviderConfig != nil || c.BudgetUsage != nil || c.TagsForAsset != nil ||
		c.CaptionForAsset != nil || c.PromptTemplate != nil {
		t.Errorf("nil registry should give nil fields, got %+v", c)
	}
}

// All five Invalidate* helpers must no-op (no panic, return nil) on a
// nil-Caches receiver. The cost.Tracker + admin handlers rely on
// this contract so test setups that skip the cache layer don't crash.
func TestCaches_NilSafe_AllInvalidatesNoOp(t *testing.T) {
	var c *Caches
	if err := c.InvalidateProviderConfig(t.Context()); err != nil {
		t.Errorf("InvalidateProviderConfig: %v", err)
	}
	if err := c.InvalidateBudgetUsage(t.Context(), "openai", "2026-06"); err != nil {
		t.Errorf("InvalidateBudgetUsage: %v", err)
	}
	if err := c.InvalidateTagsForAsset(t.Context(), pgtype.UUID{}); err != nil {
		t.Errorf("InvalidateTagsForAsset: %v", err)
	}
	if err := c.InvalidateCaptionForAsset(t.Context(), pgtype.UUID{}); err != nil {
		t.Errorf("InvalidateCaptionForAsset: %v", err)
	}
	if err := c.InvalidatePromptTemplate(t.Context(), ConcernTag, "v1.0"); err != nil {
		t.Errorf("InvalidatePromptTemplate: %v", err)
	}

	// Zero-valued Caches (not pointer-nil but field-nil) — same
	// no-op contract.
	c2 := &Caches{}
	if err := c2.InvalidateProviderConfig(t.Context()); err != nil {
		t.Errorf("zero-Caches InvalidateProviderConfig: %v", err)
	}
}

func TestBudgetKey_ComposesProviderAndPeriod(t *testing.T) {
	got := budgetKey("openai", "2026-06")
	want := "openai|2026-06"
	if got != want {
		t.Errorf("budgetKey = %q, want %q", got, want)
	}
}

func TestPromptKeyString_ComposesConcernAndVersion(t *testing.T) {
	got := promptKeyString(ConcernCaption, "v1.0")
	want := "caption|v1.0"
	if got != want {
		t.Errorf("promptKeyString = %q, want %q", got, want)
	}
}

func TestAssetKey_FormatsValidUUID(t *testing.T) {
	id, _ := uuid.Parse("12345678-90ab-cdef-1234-567890abcdef")
	pg := pgtype.UUID{Bytes: id, Valid: true}
	got := assetKey(pg)
	want := "12345678-90ab-cdef-1234-567890abcdef"
	if got != want {
		t.Errorf("assetKey = %q, want %q", got, want)
	}
}

func TestAssetKey_InvalidUUID_ReturnsEmpty(t *testing.T) {
	if got := assetKey(pgtype.UUID{}); got != "" {
		t.Errorf("assetKey(invalid) = %q, want empty", got)
	}
}

// cacheKeyProviderConfig is the well-known sentinel for the global
// config domain. Stability matters: every reader hits this exact
// string, so accidentally changing it would leak the cache between
// versions of the binary.
func TestCacheKeyProviderConfig_StableSentinel(t *testing.T) {
	if cacheKeyProviderConfig != "global" {
		t.Errorf("cacheKeyProviderConfig = %q, want \"global\" (stable sentinel)", cacheKeyProviderConfig)
	}
}

// The 5 domain names are NOTIFY channel topics — peer instances
// listen on these literally. Drift between binaries would break
// cross-instance invalidation. Lock them down.
func TestCacheDomainNames_Stable(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{cacheDomainAIProviderConfig, "ai.provider_config"},
		{cacheDomainAIBudgetUsage, "ai.budget_usage"},
		{cacheDomainAITagsForAsset, "ai.tags_for_asset"},
		{cacheDomainAICaptionForAsset, "ai.caption_for_asset"},
		{cacheDomainAIPromptTemplate, "ai.prompt_template"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("domain drift: got %q, want %q", c.got, c.want)
		}
	}
}
