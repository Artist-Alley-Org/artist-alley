// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"context"
	"errors"
	"fmt"
)

// Router is the orchestrator every inference call funnels through.
// Given a concern + a request, it:
//
//   1. Loads the operator's Config snapshot (cache-fronted).
//   2. Builds the candidate-provider set per concern: preferred
//      first, then the fallback chain.
//   3. Filters by PrivacyClass: PrivacyClassLocalOnly clamps to
//      Config.Privacy.LocalProviders. Empty result → terminal
//      ErrClassPrivacy.
//   4. Filters by budget: providers whose hard cap is exhausted
//      drop from candidates pre-call.
//   5. Walks survivors in order. Each provider call goes through
//      Tracker.CheckBudgetBefore + the actual invocation +
//      Tracker.RecordCallAfter. Per-class behaviour:
//        - Success → return immediately.
//        - ErrClassTransient / ErrClassRateLimit → try next.
//        - ErrClassPermanent / ErrClassBudget / ErrClassPrivacy →
//          terminal; return without walking further (fallback
//          would fail identically).
//
// The router never panics on a missing provider. A routing entry
// that names an unregistered provider is silently skipped at the
// candidate-build step — the ValidateAgainstProviders call at boot
// surfaces it as an admin warning; runtime degrades gracefully.

// BudgetGate is the narrow surface the router needs from the cost
// tracker. Extracted as an interface so router_test.go can stub it
// without spinning up a real Tracker (which would need a pool +
// loader). *Tracker satisfies it.
type BudgetGate interface {
	CheckBudgetBefore(ctx context.Context, provider string, estimatedCostMicros int64) error
}

// configLoader is the narrow surface for reading the active config.
// *Loader satisfies it; tests can stub.
type configLoader interface {
	Load(ctx context.Context) (Config, error)
}

// Router orchestrates provider selection per inference call.
type Router struct {
	loader    configLoader
	budget    BudgetGate
	auditor   *CallAuditor
	providers map[string]Provider
}

// NewRouter binds a router to its dependencies. Providers register
// later via Register so the boot wire can assemble them in any
// order.
func NewRouter(loader configLoader, budget BudgetGate, auditor *CallAuditor) *Router {
	return &Router{
		loader:    loader,
		budget:    budget,
		auditor:   auditor,
		providers: map[string]Provider{},
	}
}

// Register adds a provider under its Name(). Last-write-wins on a
// duplicate name; the admin UI prevents that at registration time
// but the router shouldn't error on it (a hot-reload could re-
// register cleanly).
func (r *Router) Register(p Provider) {
	if p == nil {
		return
	}
	r.providers[p.Name()] = p
}

// RegisteredNames returns the sorted-stable list of provider names
// currently registered. Used by Config.ValidateAgainstProviders at
// boot.
func (r *Router) RegisteredNames() []string {
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	// Insertion-order isn't guaranteed by map iteration; the caller
	// (validator) doesn't care about order, only set membership.
	return out
}

// ---------------------------------------------------------------------------
// Per-concern entry points
// ---------------------------------------------------------------------------
//
// Each concern's public method picks the typed candidate set
// (CompletionProvider / EmbeddingProvider / ...) and walks the
// fallback chain. The shared walk helper handles error
// classification + retry-vs-terminal routing.

// Complete dispatches a CompletionRequest. Returns the first
// successful CompletionResponse or a terminal error.
func (r *Router) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	candidates, terminal := r.candidatesForCompletion(ctx, req.Privacy)
	if terminal != nil {
		return CompletionResponse{}, terminal
	}

	var lastErr error
	for _, p := range candidates {
		// Pre-call budget gate. Estimate is a per-model proxy;
		// for v1 we pass zero (no estimation), so only the
		// already-exhausted case rejects pre-call. Providers'
		// post-call RecordCallAfter rolls up actual cost.
		if err := r.budget.CheckBudgetBefore(ctx, p.Name(), 0); err != nil {
			lastErr = err
			if isTerminal(err) {
				return CompletionResponse{}, err
			}
			continue
		}

		// Stamp the model field if the request left it empty —
		// per-provider defaults are configured by the operator and
		// the provider impl falls back to them if Model is "".
		resp, err := p.Complete(ctx, req)
		if err == nil {
			// Successful call. Cost recording is the provider's
			// responsibility (it knows the model + token counts);
			// the router doesn't double-record.
			return resp, nil
		}

		lastErr = err
		if isTerminal(err) {
			return CompletionResponse{}, err
		}
		// Transient / rate-limit: walk to next candidate.
	}

	if lastErr == nil {
		// candidates was empty AND no terminal error — that means
		// the router had nothing to try (no registered provider
		// matched). Surface as the structured no-provider error so
		// the caller can render the actionable signal.
		return CompletionResponse{}, ErrNoProviderAvailable
	}
	return CompletionResponse{}, fmt.Errorf("ai: all providers failed: %w", lastErr)
}

// Embed dispatches an embedding request. Same walk shape as Complete.
func (r *Router) Embed(ctx context.Context, in EmbedInput, privacy PrivacyClass) ([]float32, error) {
	candidates, terminal := r.candidatesForEmbedding(ctx, privacy)
	if terminal != nil {
		return nil, terminal
	}

	var lastErr error
	for _, p := range candidates {
		if err := r.budget.CheckBudgetBefore(ctx, p.Name(), 0); err != nil {
			lastErr = err
			if isTerminal(err) {
				return nil, err
			}
			continue
		}
		vec, err := p.Embed(ctx, in)
		if err == nil {
			return vec, nil
		}
		lastErr = err
		if isTerminal(err) {
			return nil, err
		}
	}
	if lastErr == nil {
		return nil, ErrNoProviderAvailable
	}
	return nil, fmt.Errorf("ai: all embedding providers failed: %w", lastErr)
}

// Transcribe dispatches a transcription request. Same walk shape as
// Complete + Embed. Privacy gate clamps restricted/embargo assets
// to local-only providers; budget gate runs before each provider
// attempt; transient errors walk the fallback chain, terminal
// errors short-circuit.
func (r *Router) Transcribe(ctx context.Context, audio AudioInput, opts TranscribeOpts, privacy PrivacyClass) (Transcript, error) {
	candidates, terminal := r.candidatesForTranscription(ctx, privacy)
	if terminal != nil {
		return Transcript{}, terminal
	}

	var lastErr error
	for _, p := range candidates {
		if err := r.budget.CheckBudgetBefore(ctx, p.Name(), 0); err != nil {
			lastErr = err
			if isTerminal(err) {
				return Transcript{}, err
			}
			continue
		}
		tx, err := p.Transcribe(ctx, audio, opts)
		if err == nil {
			return tx, nil
		}
		lastErr = err
		if isTerminal(err) {
			return Transcript{}, err
		}
	}
	if lastErr == nil {
		return Transcript{}, ErrNoProviderAvailable
	}
	return Transcript{}, fmt.Errorf("ai: all transcription providers failed: %w", lastErr)
}

// Tag dispatches a tagging request.
func (r *Router) Tag(ctx context.Context, asset AssetRef, opts TagOpts, privacy PrivacyClass) ([]Tag, error) {
	candidates, terminal := r.candidatesForTag(ctx, privacy)
	if terminal != nil {
		return nil, terminal
	}

	var lastErr error
	for _, p := range candidates {
		if err := r.budget.CheckBudgetBefore(ctx, p.Name(), 0); err != nil {
			lastErr = err
			if isTerminal(err) {
				return nil, err
			}
			continue
		}
		tags, err := p.Tag(ctx, asset, opts)
		if err == nil {
			return tags, nil
		}
		lastErr = err
		if isTerminal(err) {
			return nil, err
		}
	}
	if lastErr == nil {
		return nil, ErrNoProviderAvailable
	}
	return nil, fmt.Errorf("ai: all tag providers failed: %w", lastErr)
}

// Caption dispatches a captioning request.
func (r *Router) Caption(ctx context.Context, asset AssetRef, opts CaptionOpts, privacy PrivacyClass) (string, error) {
	candidates, terminal := r.candidatesForCaption(ctx, privacy)
	if terminal != nil {
		return "", terminal
	}

	var lastErr error
	for _, p := range candidates {
		if err := r.budget.CheckBudgetBefore(ctx, p.Name(), 0); err != nil {
			lastErr = err
			if isTerminal(err) {
				return "", err
			}
			continue
		}
		text, err := p.Caption(ctx, asset, opts)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if isTerminal(err) {
			return "", err
		}
	}
	if lastErr == nil {
		return "", ErrNoProviderAvailable
	}
	return "", fmt.Errorf("ai: all caption providers failed: %w", lastErr)
}

// ---------------------------------------------------------------------------
// Candidate selection
// ---------------------------------------------------------------------------
//
// Per concern: build the ordered name list (preferred + fallback
// chain), type-assert each to the concern's typed interface, then
// apply the privacy gate. Returns nil, terminal-error when the
// gate empties the set or no registered provider matches.

func (r *Router) candidatesForCompletion(ctx context.Context, privacy PrivacyClass) ([]CompletionProvider, error) {
	names, terminal := r.orderedNames(ctx, ConcernComplete, privacy)
	if terminal != nil {
		return nil, terminal
	}
	out := make([]CompletionProvider, 0, len(names))
	for _, n := range names {
		if p, ok := r.providers[n].(CompletionProvider); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (r *Router) candidatesForEmbedding(ctx context.Context, privacy PrivacyClass) ([]EmbeddingProvider, error) {
	names, terminal := r.orderedNames(ctx, ConcernEmbed, privacy)
	if terminal != nil {
		return nil, terminal
	}
	out := make([]EmbeddingProvider, 0, len(names))
	for _, n := range names {
		if p, ok := r.providers[n].(EmbeddingProvider); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (r *Router) candidatesForTranscription(ctx context.Context, privacy PrivacyClass) ([]TranscriptionProvider, error) {
	names, terminal := r.orderedNames(ctx, ConcernTranscribe, privacy)
	if terminal != nil {
		return nil, terminal
	}
	out := make([]TranscriptionProvider, 0, len(names))
	for _, n := range names {
		if p, ok := r.providers[n].(TranscriptionProvider); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (r *Router) candidatesForTag(ctx context.Context, privacy PrivacyClass) ([]TagProvider, error) {
	names, terminal := r.orderedNames(ctx, ConcernTag, privacy)
	if terminal != nil {
		return nil, terminal
	}
	out := make([]TagProvider, 0, len(names))
	for _, n := range names {
		if p, ok := r.providers[n].(TagProvider); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (r *Router) candidatesForCaption(ctx context.Context, privacy PrivacyClass) ([]CaptionProvider, error) {
	names, terminal := r.orderedNames(ctx, ConcernCaption, privacy)
	if terminal != nil {
		return nil, terminal
	}
	out := make([]CaptionProvider, 0, len(names))
	for _, n := range names {
		if p, ok := r.providers[n].(CaptionProvider); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// orderedNames builds the candidate name list for a concern: the
// preferred provider first, then anything in the fallback chain
// that wasn't already named. Then applies the privacy gate.
//
// Returns a terminal *ProviderError{Class: ErrClassPrivacy} when
// the gate empties the set — the operator dashboard's "blocked by
// privacy policy" counter relies on this surfacing as a distinct
// error class rather than the generic ErrNoProviderAvailable.
func (r *Router) orderedNames(ctx context.Context, concern Concern, privacy PrivacyClass) ([]string, error) {
	cfg, _ := r.loader.Load(ctx) // tolerate validator findings; partial config still routes

	names := []string{}
	seen := map[string]struct{}{}

	if preferred, ok := cfg.Routing[concern]; ok && preferred != "" {
		names = append(names, preferred)
		seen[preferred] = struct{}{}
	}

	for _, n := range cfg.FallbackChains[concern] {
		if _, dup := seen[n]; dup {
			continue
		}
		names = append(names, n)
		seen[n] = struct{}{}
	}

	// Privacy gate: when the request is local-only, clamp to the
	// operator's local set. Empty result is a terminal signal.
	if privacy == PrivacyClassLocalOnly {
		filtered := FilterLocalOnly(names, cfg.Privacy)
		if len(filtered) == 0 {
			return nil, &ProviderError{
				Class:   ErrClassPrivacy,
				Wrapped: errors.New("no local provider configured for privacy-restricted request"),
			}
		}
		names = filtered
	}

	return names, nil
}

// isTerminal classifies an error as "stop walking the fallback
// chain; no other provider could succeed either". Wraps
// errors.As(*ProviderError) so the caller doesn't have to import
// the error types for the check.
func isTerminal(err error) bool {
	pe, ok := AsProviderError(err)
	if !ok {
		// Plain Go error (network / context cancel / serialization
		// crash) — treat as transient. The worker may retry later.
		return false
	}
	switch pe.Class {
	case ErrClassPermanent, ErrClassBudget, ErrClassPrivacy:
		return true
	}
	return false
}
