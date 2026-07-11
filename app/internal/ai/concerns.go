// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package ai provides the universal AI provider abstraction described
// in Phase 1.14.A. Five typed concern interfaces (CompletionProvider,
// EmbeddingProvider, TranscriptionProvider, TagProvider,
// CaptionProvider) split the inference surface so a provider only
// implements what it actually supports — Claude has no native embedding
// API, doesn't implement EmbeddingProvider; CLIP-local doesn't do chat,
// doesn't implement CompletionProvider. The compiler enforces the
// fit; the router never has to ask `provider.Supports(concern)` at
// runtime.
//
// The router (router.go) sits in front of the providers, picks one
// per (concern, asset_sensitivity) tuple, walks the operator-
// configured fallback chain on transient/rate-limit errors, and
// short-circuits on permanent / budget / privacy errors. Budgets +
// privacy gates live in their own files (cost.go, privacy.go) so a
// new gate kind plugs in without touching the router.
//
// Per ADR 0042 the closed catalogues — Concern + PrivacyClass —
// are typed Go constants in this package; the schema CHECK on
// ai_provider_call.concern is the cross-language gate. Add a value
// here only after adding it to the migration's CHECK clause.
package ai

import "fmt"

// Concern is the typed AI task category. Five values, mirroring the
// ai_provider_call.concern CHECK in migration 00009.
type Concern string

const (
	// ConcernComplete — chat-style text completion. Universal:
	// every cloud + LLM-serving provider supports it.
	ConcernComplete Concern = "complete"

	// ConcernEmbed — text/image → vector. 1.14.A providers that
	// support this: OpenAI (text only), Gemini (text + multimodal),
	// Ollama (text via nomic-embed). 1.14.B adds CLIP-local for the
	// deterministic-cacheable path.
	ConcernEmbed Concern = "embed"

	// ConcernTranscribe — audio → text. OpenAI (Whisper API) +
	// Gemini support; Claude + Ollama don't. 1.14.C adds Whisper-
	// local for the privacy-preserving path.
	ConcernTranscribe Concern = "transcribe"

	// ConcernTag — image/video → list of tags. Task-specialized
	// wrapper that typically composes ConcernComplete with a prompt
	// template; 1.14.B will add a CLIP-similarity-based Tag impl.
	ConcernTag Concern = "tag"

	// ConcernCaption — image/video → free-text caption. Same shape
	// as ConcernTag: task-specialized over Complete with a prompt.
	ConcernCaption Concern = "caption"
)

// AllConcerns is the closed enumeration. Useful for admin UIs that
// render the routing matrix without hardcoding the list twice.
var AllConcerns = []Concern{
	ConcernComplete,
	ConcernEmbed,
	ConcernTranscribe,
	ConcernTag,
	ConcernCaption,
}

// Valid reports whether c is one of the known constants. The schema
// CHECK is the authoritative gate; this is a frontline check for
// API + operator-config input.
func (c Concern) Valid() bool {
	switch c {
	case ConcernComplete, ConcernEmbed, ConcernTranscribe, ConcernTag, ConcernCaption:
		return true
	}
	return false
}

// ParseConcern round-trips a string through the typed constants.
// Returns a friendly error so the API handler can surface a 422
// without leaking the underlying CHECK constraint name.
func ParseConcern(s string) (Concern, error) {
	c := Concern(s)
	if !c.Valid() {
		return "", fmt.Errorf("ai: invalid concern %q (want one of %v)", s, AllConcerns)
	}
	return c, nil
}

// PrivacyClass declares whether an operation may use cloud providers
// or must stay on local providers. Computed from the asset's
// sensitivity tier + the operator's lock_sensitive_to_local policy
// at call time (see privacy.go).
type PrivacyClass string

const (
	// PrivacyClassAny — the call may go to any registered provider.
	// Default for public + team assets.
	PrivacyClassAny PrivacyClass = "any"

	// PrivacyClassLocalOnly — the call must use a provider on the
	// operator-configured local list (ai.privacy.local_providers).
	// Applied to restricted + embargo assets when the
	// lock_sensitive_to_local flag is on (the default).
	PrivacyClassLocalOnly PrivacyClass = "local_only"
)

func (p PrivacyClass) Valid() bool {
	return p == PrivacyClassAny || p == PrivacyClassLocalOnly
}
