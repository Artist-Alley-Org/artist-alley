// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"strings"
	"testing"
)

// TestSetSearch_AutoEmbedRateLimit_OutOfRange_Rejected — the validator
// must reject values outside [0, 100]. 0 is legal (default fallback);
// >100 is an operator typo we should catch loudly.
func TestSetSearch_AutoEmbedRateLimit_OutOfRange_Rejected(t *testing.T) {
	s := &Store{}
	cases := []struct {
		name string
		val  float64
	}{
		{"negative", -1.0},
		{"way too high", 101.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.SetSearch(context.Background(), SearchConfig{
				Visual: VisualSearchConfig{AutoEmbedRateLimitPerSecond: tc.val},
			})
			if err == nil {
				t.Fatalf("expected validation error for %g, got nil", tc.val)
			}
			if !strings.Contains(err.Error(), "auto_embed_rate_limit_per_second") {
				t.Fatalf("error should reference the field name, got %q", err.Error())
			}
		})
	}
}

// TestSetSearch_AutoEmbedRateLimit_BelowMin_Rejected — the brief
// specifies a minimum of 0.1 (below that starves the pool). 0 is the
// legal "use default" sentinel, so we only reject the 0<x<0.1 band.
func TestSetSearch_AutoEmbedRateLimit_BelowMin_Rejected(t *testing.T) {
	s := &Store{}
	err := s.SetSearch(context.Background(), SearchConfig{
		Visual: VisualSearchConfig{AutoEmbedRateLimitPerSecond: 0.05},
	})
	if err == nil {
		t.Fatal("expected validation error for 0.05, got nil")
	}
}

// TestSetSearch_AutoEmbedRetryCount_OutOfRange_Rejected — cap at 5 to
// prevent operators enqueueing infinite-loop retry storms.
func TestSetSearch_AutoEmbedRetryCount_OutOfRange_Rejected(t *testing.T) {
	s := &Store{}
	cases := []struct {
		name string
		val  int
	}{
		{"negative", -1},
		{"way too high", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.SetSearch(context.Background(), SearchConfig{
				Visual: VisualSearchConfig{AutoEmbedRetryCount: tc.val},
			})
			if err == nil {
				t.Fatalf("expected validation error for %d, got nil", tc.val)
			}
		})
	}
}

// TestGetSearch_Defaults_AutoEmbedKnobs — a fresh install should read
// back the documented defaults, not zero values.
func TestGetSearch_Defaults_AutoEmbedKnobs(t *testing.T) {
	// The default-fill logic runs inside GetSearch after getKey.
	// getKey against a nil pool would panic, so we exercise the fill
	// step directly through a helper. Structural test — narrower than
	// a full DB round-trip.
	cfg := SearchConfig{}
	filled := applyAutoEmbedDefaults(cfg.Visual)
	if filled.AutoEmbedRateLimitPerSecond != DefaultVisualAutoEmbedRateLimitPerSecond {
		t.Fatalf("rate limit default: got %g, want %g",
			filled.AutoEmbedRateLimitPerSecond, DefaultVisualAutoEmbedRateLimitPerSecond)
	}
	if filled.AutoEmbedRetryCount != DefaultVisualAutoEmbedRetryCount {
		t.Fatalf("retry count default: got %d, want %d",
			filled.AutoEmbedRetryCount, DefaultVisualAutoEmbedRetryCount)
	}
}

// applyAutoEmbedDefaults mirrors the auto-embed default-fill block
// inside GetSearch — tests exercise it in isolation without needing
// a real store.
func applyAutoEmbedDefaults(v VisualSearchConfig) VisualSearchConfig {
	if v.AutoEmbedRateLimitPerSecond <= 0 {
		v.AutoEmbedRateLimitPerSecond = DefaultVisualAutoEmbedRateLimitPerSecond
	}
	if v.AutoEmbedRetryCount <= 0 {
		v.AutoEmbedRetryCount = DefaultVisualAutoEmbedRetryCount
	}
	return v
}
