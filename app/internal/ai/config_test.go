// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"errors"
	"reflect"
	"sort"
	"testing"
)

// ParseConfig with empty raw map returns the defaults (matches what
// migration 00009 seeded into system_config on a fresh install).
func TestParseConfig_EmptyRaw_ReturnsDefaults(t *testing.T) {
	cfg, err := ParseConfig(map[string][]byte{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Enabled {
		t.Error("default Enabled = true, want false (fresh install master switch OFF)")
	}
	if got := cfg.Routing[ConcernTag]; got != "ollama" {
		t.Errorf("default routing[tag] = %q, want ollama", got)
	}
	if !cfg.Privacy.LockSensitiveToLocal {
		t.Error("default Privacy.LockSensitiveToLocal = false, want true")
	}
	if got := cfg.DefaultBudget.HardCapUSD; got != 0 {
		t.Errorf("default HardCapUSD = %d, want 0 (fail-closed)", got)
	}
}

func TestParseConfig_HappyPath_PopulatesAllFields(t *testing.T) {
	raw := map[string][]byte{
		"ai.enabled":                         []byte(`true`),
		"ai.routing":                         []byte(`{"tag":"openai","caption":"claude","embed":"clip_local","transcribe":"whisper_local","complete":"claude"}`),
		"ai.fallback_chains":                 []byte(`{"complete":["openai","claude"],"tag":["ollama"]}`),
		"ai.privacy.lock_sensitive_to_local": []byte(`false`),
		"ai.privacy.local_providers":         []byte(`["ollama","vllm"]`),
		"ai.budgets.default":                 []byte(`{"soft_warning_usd":50,"hard_cap_usd":200}`),
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.Enabled {
		t.Error("Enabled didn't survive round-trip")
	}
	if cfg.Routing[ConcernTag] != "openai" {
		t.Errorf("routing[tag] = %q, want openai", cfg.Routing[ConcernTag])
	}
	if cfg.Privacy.LockSensitiveToLocal {
		t.Error("LockSensitiveToLocal = true, want false (from raw)")
	}
	if got := cfg.Privacy.LocalProviders; !reflect.DeepEqual(got, []string{"ollama", "vllm"}) {
		t.Errorf("LocalProviders = %v", got)
	}
	if cfg.DefaultBudget.SoftWarningUSD != 50 || cfg.DefaultBudget.HardCapUSD != 200 {
		t.Errorf("budget = %+v", cfg.DefaultBudget)
	}
}

func TestParseConfig_UnknownConcernKey_SilentlyDropped(t *testing.T) {
	// Operator's stored config contains a concern from a future
	// version of the binary (or a typo). Don't crash; the validator
	// will surface the missing-concern finding separately.
	raw := map[string][]byte{
		"ai.routing": []byte(`{"tag":"ollama","totallymadeup":"x"}`),
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := cfg.Routing["totallymadeup"]; ok {
		t.Error("unknown concern leaked into typed routing map")
	}
	if cfg.Routing[ConcernTag] != "ollama" {
		t.Error("known concern dropped alongside unknown")
	}
}

func TestParseConfig_BadJSON_ReturnsError(t *testing.T) {
	raw := map[string][]byte{
		"ai.enabled": []byte(`not-a-bool`),
	}
	if _, err := ParseConfig(raw); err == nil {
		t.Error("expected parse error on bad bool")
	}
}

// The validator catches an admin-config UI footgun: lock is on but
// no local providers are listed → no provider can serve restricted
// + embargo assets. Surface inline so the operator sees it before
// the first call fires.
func TestConfig_Validate_RejectsLockOnWithEmptyLocalList(t *testing.T) {
	cfg := defaultConfig()
	cfg.Privacy.LocalProviders = nil

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	var inv *ErrConfigInvalid
	if !errors.As(err, &inv) {
		t.Fatalf("error type = %T, want *ErrConfigInvalid", err)
	}
	want := "privacy_lock_with_empty_local_list"
	found := false
	for _, f := range inv.Findings {
		if f.Code == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("findings = %+v, want code %q", inv.Findings, want)
	}
}

func TestConfig_Validate_FlagsMissingConcerns(t *testing.T) {
	cfg := Config{
		Routing: map[Concern]string{ConcernTag: "ollama"}, // others missing
		Privacy: PrivacyPolicy{LocalProviders: []string{"ollama"}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	var inv *ErrConfigInvalid
	if !errors.As(err, &inv) {
		t.Fatalf("wrong err type %T", err)
	}
	// Each of the 4 concerns that wasn't in routing should have a finding.
	missingConcerns := map[Concern]bool{
		ConcernComplete: true, ConcernEmbed: true, ConcernTranscribe: true, ConcernCaption: true,
	}
	for _, f := range inv.Findings {
		if f.Code == "routing_missing_concern" {
			delete(missingConcerns, f.Concern)
		}
	}
	if len(missingConcerns) > 0 {
		t.Errorf("missing-concern findings not emitted for: %v", missingConcerns)
	}
}

func TestConfig_Validate_HappyPath_ReturnsNil(t *testing.T) {
	if err := defaultConfig().Validate(); err != nil {
		t.Errorf("default config should validate clean: %v", err)
	}
}

// ValidateAgainstProviders catches "routing references 'gemini' but
// no gemini provider is registered" at boot time — the structural
// validator alone can't see this because it doesn't know what's
// actually been registered.
func TestConfig_ValidateAgainstProviders_FlagsUndefinedRouting(t *testing.T) {
	cfg := defaultConfig()
	// Default routing references ollama, claude, clip_local, etc.;
	// only register a subset to provoke the undefined finding.
	registered := []string{"ollama"}

	err := cfg.ValidateAgainstProviders(registered)
	if err == nil {
		t.Fatal("expected validation error")
	}
	var inv *ErrConfigInvalid
	if !errors.As(err, &inv) {
		t.Fatalf("wrong err type %T", err)
	}

	// At least one of the missing routing references should flag
	// `routing_undefined_provider`.
	found := false
	for _, f := range inv.Findings {
		if f.Code == "routing_undefined_provider" {
			found = true
			break
		}
	}
	if !found {
		var codes []string
		for _, f := range inv.Findings {
			codes = append(codes, f.Code)
		}
		sort.Strings(codes)
		t.Errorf("no routing_undefined_provider finding; got codes %v", codes)
	}
}

func TestConfig_ValidateAgainstProviders_AllRegistered_ReturnsNil(t *testing.T) {
	cfg := defaultConfig()
	registered := []string{"ollama", "claude", "clip_local", "whisper_local", "openai", "gemini", "vllm"}
	if err := cfg.ValidateAgainstProviders(registered); err != nil {
		t.Errorf("all-registered should validate clean: %v", err)
	}
}

// ErrConfigInvalid.Error joins findings — exercised so a future
// refactor that breaks the format surfaces immediately.
func TestErrConfigInvalid_Error_JoinsFindingMessages(t *testing.T) {
	e := &ErrConfigInvalid{
		Findings: []ConfigFinding{
			{Code: "a", Message: "alpha"},
			{Code: "b", Message: "beta"},
		},
	}
	got := e.Error()
	if got != "ai: config invalid: alpha; beta" {
		t.Errorf("Error() = %q", got)
	}
}
