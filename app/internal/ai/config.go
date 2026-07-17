// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is the operator-tunable AI subsystem snapshot, parsed from
// the six `ai.*` keys seeded in migration 00009. Read-hot path:
// every inference call asks the router for a provider, which asks
// the config for the routing preference + privacy policy. The whole
// object lives in cacheDomainAIProviderConfig as a single global
// entry; admin writes invalidate the cache, next read repopulates.
//
// The validator (Validate) catches the operator footguns:
//   - routing references a provider that isn't configured
//   - fallback chain references undefined providers
//   - lock_sensitive_to_local is on but local_providers is empty
//     (no provider could ever serve a restricted asset)
//
// These are surfaced at parse time so the admin UI can show the
// error inline rather than at first inference-call time.
type Config struct {
	Enabled        bool                 // ai.enabled — master switch
	Routing        map[Concern]string   // per-task default provider
	FallbackChains map[Concern][]string // walk if primary fails
	Privacy        PrivacyPolicy        // lock + local provider list
	DefaultBudget  BudgetDefaults       // applied to newly-configured providers
}

// BudgetDefaults mirrors the `ai.budgets.default` JSONB shape. $0
// hard cap is the fail-closed default; first cloud call to a
// freshly-configured provider hits ErrClassBudget with
// cloud_budget_not_configured until operator raises explicitly.
type BudgetDefaults struct {
	SoftWarningUSD int64 `json:"soft_warning_usd"`
	HardCapUSD     int64 `json:"hard_cap_usd"`
}

// ---------------------------------------------------------------------------
// Loader — reads system_config keys + parses to Config
// ---------------------------------------------------------------------------

// Loader hydrates Config from system_config. Cache-fronted via the
// shared Caches.ProviderConfig domain; first read after invalidation
// pays the DB cost, subsequent calls hit the LRU.
type Loader struct {
	pool   *pgxpool.Pool
	caches *Caches
	// mu serialises concurrent miss-then-populate so two simultaneous
	// cache misses don't both run the full system_config read in
	// parallel.
	mu sync.Mutex
}

// NewLoader binds a loader to the pool + caches. Caches may be nil
// for tests (every read hits the DB).
func NewLoader(pool *pgxpool.Pool, caches *Caches) *Loader {
	return &Loader{pool: pool, caches: caches}
}

// Load returns the current Config, cache-warm or freshly parsed.
// Returns the validator's error (already wrapped) as a soft signal
// — the partial Config is still returned so the caller can decide
// whether to operate on it or refuse.
func (l *Loader) Load(ctx context.Context) (Config, error) {
	if l.caches != nil && l.caches.ProviderConfig != nil {
		if entry, ok := l.caches.ProviderConfig.Get(cacheKeyProviderConfig); ok {
			// Return the cached config plus a re-run of the validator
			// so configuration errors stay surfaced even on warm hits.
			return entry.Config, entry.Config.Validate()
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// Double-check after the lock; a concurrent loader may have
	// populated the cache while we waited.
	if l.caches != nil && l.caches.ProviderConfig != nil {
		if entry, ok := l.caches.ProviderConfig.Get(cacheKeyProviderConfig); ok {
			return entry.Config, entry.Config.Validate()
		}
	}

	cfg, err := l.readFromDB(ctx)
	if err != nil {
		return Config{}, fmt.Errorf("ai: load config: %w", err)
	}

	if l.caches != nil && l.caches.ProviderConfig != nil {
		l.caches.ProviderConfig.Add(cacheKeyProviderConfig, ProviderConfigEntry{Config: cfg})
	}
	return cfg, cfg.Validate()
}

// readFromDB pulls the six ai.* keys in one query, then parses each
// JSONB value into the typed Config field. Missing keys fall back
// to the in-binary defaults — operator deleting a key (or running
// an older migration) shouldn't crash the loader.
func (l *Loader) readFromDB(ctx context.Context) (Config, error) {
	const q = `
		SELECT key, value
		  FROM system_config
		 WHERE key = ANY($1::text[])`

	keys := []string{
		"ai.enabled",
		"ai.routing",
		"ai.fallback_chains",
		"ai.privacy.lock_sensitive_to_local",
		"ai.privacy.local_providers",
		"ai.budgets.default",
	}

	rows, err := l.pool.Query(ctx, q, keys)
	if err != nil {
		return Config{}, err
	}
	defer rows.Close()

	raw := map[string][]byte{}
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return Config{}, err
		}
		raw[k] = v
	}
	if err := rows.Err(); err != nil {
		return Config{}, err
	}

	return ParseConfig(raw)
}

// ParseConfig converts the raw `{key: jsonb_bytes}` map into a
// typed Config. Exported so tests can hand-build configs without
// going through the DB.
//
// Missing keys fall back to the migration's seeded defaults so an
// installation that hasn't been admin-tuned still gets a sane
// snapshot (matches what migration 00009 seeded).
func ParseConfig(raw map[string][]byte) (Config, error) {
	cfg := defaultConfig()

	if v, ok := raw["ai.enabled"]; ok && len(v) > 0 {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return cfg, fmt.Errorf("ai.enabled: %w", err)
		}
		cfg.Enabled = b
	}

	if v, ok := raw["ai.routing"]; ok && len(v) > 0 {
		// JSONB shape: {"concern_name": "provider_name", ...}
		var m map[string]string
		if err := json.Unmarshal(v, &m); err != nil {
			return cfg, fmt.Errorf("ai.routing: %w", err)
		}
		cfg.Routing = typedRouting(m)
	}

	if v, ok := raw["ai.fallback_chains"]; ok && len(v) > 0 {
		var m map[string][]string
		if err := json.Unmarshal(v, &m); err != nil {
			return cfg, fmt.Errorf("ai.fallback_chains: %w", err)
		}
		cfg.FallbackChains = typedFallbackChains(m)
	}

	if v, ok := raw["ai.privacy.lock_sensitive_to_local"]; ok && len(v) > 0 {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return cfg, fmt.Errorf("ai.privacy.lock_sensitive_to_local: %w", err)
		}
		cfg.Privacy.LockSensitiveToLocal = b
	}

	if v, ok := raw["ai.privacy.local_providers"]; ok && len(v) > 0 {
		var list []string
		if err := json.Unmarshal(v, &list); err != nil {
			return cfg, fmt.Errorf("ai.privacy.local_providers: %w", err)
		}
		cfg.Privacy.LocalProviders = list
	}

	if v, ok := raw["ai.budgets.default"]; ok && len(v) > 0 {
		var b BudgetDefaults
		if err := json.Unmarshal(v, &b); err != nil {
			return cfg, fmt.Errorf("ai.budgets.default: %w", err)
		}
		cfg.DefaultBudget = b
	}

	return cfg, nil
}

// defaultConfig returns the in-binary fallback snapshot mirroring
// the migration 00009 seeds. Used when system_config has been
// wiped or a key is missing.
func defaultConfig() Config {
	return Config{
		Enabled: false, // fresh-install master switch is OFF
		Routing: map[Concern]string{
			ConcernTag:        "ollama",
			ConcernCaption:    "claude",
			ConcernEmbed:      "clip_local",
			ConcernTranscribe: "whisper_local",
			ConcernComplete:   "claude",
		},
		FallbackChains: map[Concern][]string{
			ConcernComplete:   {"claude", "openai", "ollama"},
			ConcernEmbed:      {"clip_local", "ollama", "openai"},
			ConcernTranscribe: {"whisper_local", "openai"},
			ConcernTag:        {"ollama", "gemini", "openai"},
			ConcernCaption:    {"claude", "openai", "ollama"},
		},
		Privacy: PrivacyPolicy{
			LockSensitiveToLocal: true,
			LocalProviders:       []string{"ollama", "vllm", "whisper_local", "clip_local"},
		},
		DefaultBudget: BudgetDefaults{
			SoftWarningUSD: 0,
			HardCapUSD:     0,
		},
	}
}

// typedRouting converts the JSONB string-keyed map to the typed
// Concern-keyed shape. Unknown concern names are silently dropped
// (operator typo' or a future-only concern from a newer migration);
// the validator surfaces missing concerns as a separate error.
func typedRouting(m map[string]string) map[Concern]string {
	out := make(map[Concern]string, len(m))
	for k, v := range m {
		c := Concern(k)
		if c.Valid() {
			out[c] = v
		}
	}
	return out
}

func typedFallbackChains(m map[string][]string) map[Concern][]string {
	out := make(map[Concern][]string, len(m))
	for k, v := range m {
		c := Concern(k)
		if c.Valid() {
			out[c] = v
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Validator
// ---------------------------------------------------------------------------

// ErrConfigInvalid wraps one or more validation findings so the
// admin UI can render them as a structured list.
type ErrConfigInvalid struct {
	Findings []ConfigFinding
}

// ConfigFinding is one validation error. Code lets the UI render
// localized messages; Message is the fallback English.
type ConfigFinding struct {
	Code    string  // stable identifier ("routing_undefined_provider", etc.)
	Concern Concern // empty when not concern-specific
	Message string
}

func (e *ErrConfigInvalid) Error() string {
	if e == nil || len(e.Findings) == 0 {
		return "ai: config invalid (no findings)"
	}
	parts := make([]string, len(e.Findings))
	for i, f := range e.Findings {
		parts[i] = f.Message
	}
	return "ai: config invalid: " + joinStrings(parts, "; ")
}

// Validate runs the structural checks. The errors are operator-
// addressable (provider name typo, missing local provider entry); a
// fresh-install Config with defaults validates clean.
//
// Returns nil OR *ErrConfigInvalid (so callers can errors.As to get
// the structured Findings list).
func (c Config) Validate() error {
	var findings []ConfigFinding

	// Build the set of provider names referenced anywhere so the
	// next checks can flag references that aren't backed by an
	// actual provider entry. NOTE: the provider entries themselves
	// live in their own system_config keys (one per registered
	// provider); for this slice we accept any non-empty string as
	// "named" — the cross-check against actual registered providers
	// fires at Router build time (router.go can call back into
	// Validate with the registered set).
	for _, concern := range AllConcerns {
		preferred, ok := c.Routing[concern]
		if !ok || preferred == "" {
			findings = append(findings, ConfigFinding{
				Code:    "routing_missing_concern",
				Concern: concern,
				Message: fmt.Sprintf("routing missing for concern %q", concern),
			})
		}
	}

	if c.Privacy.LockSensitiveToLocal && len(c.Privacy.LocalProviders) == 0 {
		findings = append(findings, ConfigFinding{
			Code:    "privacy_lock_with_empty_local_list",
			Message: "lock_sensitive_to_local is on but local_providers is empty; restricted+embargo assets would have no valid provider",
		})
	}

	if len(findings) == 0 {
		return nil
	}
	// Stable order so test assertions don't flake on map-iteration
	// ordering.
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Code != findings[j].Code {
			return findings[i].Code < findings[j].Code
		}
		return findings[i].Concern < findings[j].Concern
	})
	return &ErrConfigInvalid{Findings: findings}
}

// ValidateAgainstProviders runs the structural validator and ALSO
// cross-checks that every provider name referenced in Routing /
// FallbackChains / Privacy.LocalProviders has a corresponding entry
// in `registered`. Used by the Router at boot to surface "operator
// referenced 'ollama' in routing but no ollama provider is
// registered" as an inline finding the admin UI can highlight.
func (c Config) ValidateAgainstProviders(registered []string) error {
	base := c.Validate()
	regSet := map[string]struct{}{}
	for _, r := range registered {
		regSet[r] = struct{}{}
	}

	var findings []ConfigFinding
	if base != nil {
		var inv *ErrConfigInvalid
		if errors.As(base, &inv) {
			findings = append(findings, inv.Findings...)
		}
	}

	for concern, name := range c.Routing {
		if name == "" {
			continue
		}
		if _, ok := regSet[name]; !ok {
			findings = append(findings, ConfigFinding{
				Code:    "routing_undefined_provider",
				Concern: concern,
				Message: fmt.Sprintf("routing for %q references undefined provider %q", concern, name),
			})
		}
	}

	for concern, chain := range c.FallbackChains {
		for _, name := range chain {
			if _, ok := regSet[name]; !ok {
				findings = append(findings, ConfigFinding{
					Code:    "fallback_undefined_provider",
					Concern: concern,
					Message: fmt.Sprintf("fallback chain for %q references undefined provider %q", concern, name),
				})
			}
		}
	}

	for _, name := range c.Privacy.LocalProviders {
		if _, ok := regSet[name]; !ok {
			findings = append(findings, ConfigFinding{
				Code:    "local_undefined_provider",
				Message: fmt.Sprintf("local_providers references undefined provider %q", name),
			})
		}
	}

	if len(findings) == 0 {
		return nil
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Code != findings[j].Code {
			return findings[i].Code < findings[j].Code
		}
		if findings[i].Concern != findings[j].Concern {
			return findings[i].Concern < findings[j].Concern
		}
		return findings[i].Message < findings[j].Message
	})
	return &ErrConfigInvalid{Findings: findings}
}

// joinStrings is a small local helper to avoid the strings package
// import becoming load-bearing on a single Join.
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for i := 1; i < len(parts); i++ {
		out += sep + parts[i]
	}
	return out
}

// invalidateOnWrite is the cross-package hook operators call from
// the admin handlers after a system_config write. Currently exposed
// as a thin wrapper around Caches.InvalidateProviderConfig — kept
// here next to the loader so the API surface for config state lives
// in one place.
func (l *Loader) invalidateOnWrite(ctx context.Context) error {
	if l.caches == nil {
		return nil
	}
	return l.caches.InvalidateProviderConfig(ctx)
}

// InvalidateOnConfigWrite is the public hook admin handlers call
// after writing an `ai.*` system_config key. Wrapper around the
// caches' broadcast.
func (l *Loader) InvalidateOnConfigWrite(ctx context.Context) error {
	return l.invalidateOnWrite(ctx)
}

// txReader is the minimal pool interface the loader needs;
// extracted so tests can swap in a pgx.Tx without dragging the
// whole pool.
type txReader interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Sentinel used by tests to ensure the pool interface compiles
// against pgxpool.Pool without a runtime cast.
var _ txReader = (*pgxpool.Pool)(nil)
