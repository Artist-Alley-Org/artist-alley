package sysconfig

import (
	"context"
	"fmt"
)

// KeyAI — system_config key for the AI provider settings. Read/written
// via the Store's GetAI/SetAI methods.
const KeyAI = "ai"

// AIProviderKind enumerates the AI backend integrations the app
// supports. Like SSOProviderKind, the per-kind config schema is owned
// by the integration code in the dedicated AI phase — this struct
// just persists the admin's choices.
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

	// Per-kind opaque config (temperature defaults, system prompts,
	// rate limits, ...). The integration code owns the schema.
	Config map[string]any `json:"config,omitempty"`
}

// AIConfig is the full AI settings payload stored under KeyAI.
type AIConfig struct {
	// DefaultProviderID is the AIProvider.ID picked when nothing
	// else specifies one. Empty = no default (every caller must
	// choose explicitly, or the AI feature is disabled).
	DefaultProviderID string       `json:"default_provider_id"`
	Providers         []AIProvider `json:"providers"`
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
