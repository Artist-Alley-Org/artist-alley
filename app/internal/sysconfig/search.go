package sysconfig

import (
	"context"
	"fmt"
)

// KeySearch — system_config key for the search subsystem's operator-
// tunable knobs. Currently carries the Phase 1.16.B-3-followup
// visual-search config (CLIP visual encoder sidecar activation).
// New search-arc knobs land in this struct rather than growing the
// sysconfig key namespace.
const KeySearch = "search"

// VisualSearchConfig captures the sysconfig-tunable visual-search
// knobs. Zero values disable the feature entirely — POST /search/by-image
// continues to return the 501 sidecar_not_installed stub response.
//
// Phase 1.16.B-3-followup design decisions:
//
//   - Enabled is the master switch. False (default) → provider not
//     registered → 501 stub. True + sidecar reachable at boot →
//     provider registered → 200 path active.
//   - SidecarURL points at the aa-clip-visual-local sidecar over
//     the Docker network (aa-clip-visual-local:8402 by default).
//   - TimeoutMs is the per-request budget. CLIP inference is
//     100–500 ms CPU + 20–50 ms GPU + network overhead; 5 s
//     default absorbs both cold-load latency + slow-hardware
//     inference without cascading failure.
//   - MaxUploadBytes bounds the reverse-image upload size. Sidecar
//     enforces the same limit; frontend UX rejects earlier with a
//     helpful message.
//   - RateLimitPerUserPerMinute is heavier than text search's rate
//     limit — CLIP inference is more expensive. 20 rpm/user
//     default; operators tune based on hardware.
//   - AutoEmbedOnUpload controls whether new image asset uploads
//     kick off a visual-embed job. Reserved for the follow-up that
//     wires the async pipeline; the MVP path is admin-triggered
//     backfill.
type VisualSearchConfig struct {
	Enabled                    bool   `json:"enabled"`
	SidecarURL                 string `json:"sidecar_url"`
	TimeoutMs                  int    `json:"timeout_ms"`
	MaxUploadBytes             int    `json:"max_upload_bytes"`
	RateLimitPerUserPerMinute  int    `json:"rate_limit_per_user_per_minute"`
	AutoEmbedOnUpload          bool   `json:"auto_embed_on_upload"`
}

// SearchConfig is the payload stored under KeySearch. Extensible;
// new search-arc knobs (feedback loop, cross-modal, etc.) grow this
// struct in future PRs.
type SearchConfig struct {
	Visual VisualSearchConfig `json:"visual"`
}

// Sensible defaults for VisualSearchConfig. Chosen conservative for
// public deploys; operators tune per their threat model + hardware.
const (
	DefaultVisualSidecarURL         = "http://aa-clip-visual-local:8402"
	DefaultVisualTimeoutMs          = 5000
	DefaultVisualMaxUploadBytes     = 10 * 1024 * 1024
	DefaultVisualRateLimitPerMinute = 20
)

// GetSearch returns the search config or, if unset, a zero-value
// SearchConfig (Enabled=false, so the feature stays dormant).
func (s *Store) GetSearch(ctx context.Context) (SearchConfig, error) {
	var out SearchConfig
	if err := s.getKey(ctx, KeySearch, &out); err != nil {
		return SearchConfig{}, err
	}
	// Fill defaults for any zero-value knobs so callers don't need
	// to nil-check every field. Enabled stays false by default.
	if out.Visual.SidecarURL == "" {
		out.Visual.SidecarURL = DefaultVisualSidecarURL
	}
	if out.Visual.TimeoutMs <= 0 {
		out.Visual.TimeoutMs = DefaultVisualTimeoutMs
	}
	if out.Visual.MaxUploadBytes <= 0 {
		out.Visual.MaxUploadBytes = DefaultVisualMaxUploadBytes
	}
	if out.Visual.RateLimitPerUserPerMinute <= 0 {
		out.Visual.RateLimitPerUserPerMinute = DefaultVisualRateLimitPerMinute
	}
	return out, nil
}

// SetSearch validates + writes the search config. Rejects
// nonsensical values (negative timeouts, huge upload caps).
func (s *Store) SetSearch(ctx context.Context, v SearchConfig) error {
	if v.Visual.TimeoutMs < 0 || v.Visual.TimeoutMs > 60_000 {
		return fmt.Errorf("sysconfig: visual.timeout_ms must be 0..60000, got %d", v.Visual.TimeoutMs)
	}
	if v.Visual.MaxUploadBytes < 0 || v.Visual.MaxUploadBytes > 100*1024*1024 {
		return fmt.Errorf("sysconfig: visual.max_upload_bytes must be 0..100MB, got %d", v.Visual.MaxUploadBytes)
	}
	if v.Visual.RateLimitPerUserPerMinute < 0 || v.Visual.RateLimitPerUserPerMinute > 10_000 {
		return fmt.Errorf("sysconfig: visual.rate_limit_per_user_per_minute must be 0..10000, got %d", v.Visual.RateLimitPerUserPerMinute)
	}
	return s.setKey(ctx, KeySearch, v)
}
