// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"fmt"
)

// KeyAI — system_config key for the AI provider settings. Read/written
// via the Store's GetAI/SetAI methods.
const KeyAI = "ai"

// AIProviderKind enumerates the AI backend integrations the app
// supports. This struct just persists the admin's choices; the
// dedicated AI phase owns how they are applied.
type AIProviderKind string

const (
	AIKindOpenAI    AIProviderKind = "openai"
	AIKindAnthropic AIProviderKind = "anthropic"
	AIKindGoogle    AIProviderKind = "google"
	AIKindLocal     AIProviderKind = "local" // self-hosted via Ollama / vLLM / etc.
)

func validAIKind(k AIProviderKind) bool {
	switch k {
	case AIKindOpenAI, AIKindAnthropic, AIKindGoogle, AIKindLocal:
		return true
	default:
		return false
	}
}

// AIProvider is a single configured AI backend. Multiple providers of
// the same kind are allowed (e.g. two OpenAI accounts for different
// teams' billing).
type AIProvider struct {
	// Stable id (uuid) chosen by the admin UI when the provider is
	// added. Lets the user-pref layer point at a specific provider
	// for their override.
	ID string `json:"id"`

	Kind        AIProviderKind `json:"kind"`
	Enabled     bool           `json:"enabled"`
	DisplayName string         `json:"display_name"`
	Model       string         `json:"model,omitempty"`    // "gpt-4o", "claude-sonnet-4-6", etc.
	BaseURL     string         `json:"base_url,omitempty"` // override for self-hosted / proxied endpoints

	// API key as a raw string for now. TODO(secrets): move to a
	// reference into a future secrets backend (Vault, the OS
	// keychain, an env-var template, etc.) once that lands. Storing
	// raw keys in system_config is a known interim — the table is
	// admin-only and we'll never federate this row.
	APIKey string `json:"api_key,omitempty"`

	// Per-provider inference defaults.
	Config AIProviderConfig `json:"config"`
}

// AIProviderConfig is the typed per-provider tuning block.
//
// Closed, like SSOProviderConfig, and for the same reason (#718): the
// read path returns this whole block to any `system.config.read`
// holder, so a free-form map on a provider record is a credential leak
// waiting for the first admin who parks a token in it. #711 made the
// provider's real credential — APIKey — write-only but left this map
// beside it copied out verbatim. Every field here is a tuning knob;
// there is nothing secret to redact because nothing secret can be
// stored.
type AIProviderConfig struct {
	// Temperature / TopP are pointers because 0 is a meaningful
	// setting and "unset" has to mean "the model's own default".
	Temperature           *float32 `json:"temperature,omitempty"`
	TopP                  *float32 `json:"top_p,omitempty"`
	MaxOutputTokens       int      `json:"max_output_tokens,omitempty"`
	SystemPrompt          string   `json:"system_prompt,omitempty"`
	RequestTimeoutSeconds int      `json:"request_timeout_seconds,omitempty"`
	RateLimitRPM          int      `json:"rate_limit_rpm,omitempty"`
}

// AIConfig is the full AI settings payload stored under KeyAI.
type AIConfig struct {
	// DefaultProviderID is the AIProvider.ID picked when nothing
	// else specifies one. Empty = no default (every caller must
	// choose explicitly, or the AI feature is disabled).
	DefaultProviderID string `json:"default_provider_id"`
	// Providers carries per-provider AIProvider.APIKey strings that
	// the Phase 1.17.D changeset helper cannot strip per-element
	// (the slice gets DeepEqual'd; a single field change dumps the
	// whole before/after slices including embedded API keys).
	// Stripping the entire slice from the changeset is the
	// conservative choice — operators see "AI config changed,
	// DefaultProviderID went from X to Y" and read the new
	// provider list via the API. Lost diff signal is a known
	// MVP limitation; addressing it would require slice-element-
	// aware recursion in the diff helper.
	Providers []AIProvider `json:"providers" audit:"-"`
}

// GetAI returns the AI config or, if unset, an empty AIConfig.
func (s *Store) GetAI(ctx context.Context) (AIConfig, error) {
	var out AIConfig
	if err := s.getKey(ctx, KeyAI, &out); err != nil {
		return AIConfig{}, err
	}
	return out, nil
}

// SetAI validates and writes the AI config.
func (s *Store) SetAI(ctx context.Context, v AIConfig) error {
	seen := make(map[string]int, len(v.Providers))
	for i, p := range v.Providers {
		if !validAIKind(p.Kind) {
			return fmt.Errorf("sysconfig: providers[%d]: unknown kind %q", i, p.Kind)
		}
		if p.DisplayName == "" {
			return fmt.Errorf("sysconfig: providers[%d]: display_name is required", i)
		}
		if p.ID != "" {
			if prev, dup := seen[p.ID]; dup {
				return fmt.Errorf("sysconfig: providers[%d] and [%d] share id %q", prev, i, p.ID)
			}
			seen[p.ID] = i
		}
	}
	if v.DefaultProviderID != "" {
		if _, ok := seen[v.DefaultProviderID]; !ok {
			return fmt.Errorf("sysconfig: default_provider_id %q does not match any provider", v.DefaultProviderID)
		}
	}
	return s.setKey(ctx, KeyAI, v)
}
