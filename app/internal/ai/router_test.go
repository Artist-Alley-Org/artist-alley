package ai

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// stubCompletion is a minimal CompletionProvider that returns a
// preset response or error, and records the calls it received so
// tests can assert ordering + count.
type stubCompletion struct {
	name string
	resp CompletionResponse
	err  error
	calls int
}

func (s *stubCompletion) Name() string             { return s.name }
func (s *stubCompletion) SupportsVision() bool     { return true }
func (s *stubCompletion) Complete(_ context.Context, _ CompletionRequest) (CompletionResponse, error) {
	s.calls++
	if s.err != nil {
		return CompletionResponse{}, s.err
	}
	return s.resp, nil
}

// stubLoader is a configLoader that returns a preset Config.
type stubLoader struct {
	cfg Config
	err error
}

func (s *stubLoader) Load(_ context.Context) (Config, error) { return s.cfg, s.err }

// stubBudget is a BudgetGate that returns the same error for every
// provider (or blocks specific ones via the blockedFor map).
type stubBudget struct {
	defaultErr error
	blockedFor map[string]error
}

func (s *stubBudget) CheckBudgetBefore(_ context.Context, provider string, _ int64) error {
	if s.blockedFor != nil {
		if err, ok := s.blockedFor[provider]; ok {
			return err
		}
	}
	return s.defaultErr
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

func TestRouter_HappyPath_PicksDefaultProvider(t *testing.T) {
	cfg := Config{
		Routing: map[Concern]string{ConcernComplete: "openai"},
		Privacy: PrivacyPolicy{LocalProviders: []string{"ollama"}},
	}
	openai := &stubCompletion{name: "openai", resp: CompletionResponse{Text: "from openai"}}
	r := NewRouter(&stubLoader{cfg: cfg}, &stubBudget{}, nil)
	r.Register(openai)

	resp, err := r.Complete(context.Background(), CompletionRequest{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "from openai" {
		t.Errorf("text = %q, want from openai", resp.Text)
	}
	if openai.calls != 1 {
		t.Errorf("calls = %d, want 1", openai.calls)
	}
}

// ---------------------------------------------------------------------------
// Fallback walk
// ---------------------------------------------------------------------------

func TestRouter_PrimaryFailsRateLimited_FallsBack(t *testing.T) {
	cfg := Config{
		Routing: map[Concern]string{ConcernComplete: "openai"},
		FallbackChains: map[Concern][]string{
			ConcernComplete: {"openai", "ollama"}, // openai already preferred; dup deduped
		},
	}
	openai := &stubCompletion{name: "openai", err: &ProviderError{
		Class:      ErrClassRateLimit,
		Provider:   "openai",
		RetryAfter: 5 * time.Second,
	}}
	ollama := &stubCompletion{name: "ollama", resp: CompletionResponse{Text: "from ollama"}}
	r := NewRouter(&stubLoader{cfg: cfg}, &stubBudget{}, nil)
	r.Register(openai)
	r.Register(ollama)

	resp, err := r.Complete(context.Background(), CompletionRequest{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "from ollama" {
		t.Errorf("text = %q, want from ollama (fallback)", resp.Text)
	}
	if openai.calls != 1 || ollama.calls != 1 {
		t.Errorf("call counts: openai=%d ollama=%d (want 1,1)", openai.calls, ollama.calls)
	}
}

func TestRouter_PrimaryFailsTransient_FallsBack(t *testing.T) {
	cfg := Config{
		Routing:        map[Concern]string{ConcernComplete: "openai"},
		FallbackChains: map[Concern][]string{ConcernComplete: {"ollama"}},
	}
	openai := &stubCompletion{name: "openai", err: &ProviderError{
		Class: ErrClassTransient, Provider: "openai", Wrapped: errors.New("502"),
	}}
	ollama := &stubCompletion{name: "ollama", resp: CompletionResponse{Text: "ok"}}
	r := NewRouter(&stubLoader{cfg: cfg}, &stubBudget{}, nil)
	r.Register(openai)
	r.Register(ollama)

	if _, err := r.Complete(context.Background(), CompletionRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if ollama.calls != 1 {
		t.Error("fallback not invoked")
	}
}

// ---------------------------------------------------------------------------
// Terminal classes don't walk the chain
// ---------------------------------------------------------------------------

func TestRouter_PermanentError_NoFallback(t *testing.T) {
	cfg := Config{
		Routing:        map[Concern]string{ConcernComplete: "openai"},
		FallbackChains: map[Concern][]string{ConcernComplete: {"ollama"}},
	}
	openai := &stubCompletion{name: "openai", err: &ProviderError{
		Class: ErrClassPermanent, Provider: "openai", Wrapped: errors.New("400"),
	}}
	ollama := &stubCompletion{name: "ollama", resp: CompletionResponse{Text: "unreached"}}
	r := NewRouter(&stubLoader{cfg: cfg}, &stubBudget{}, nil)
	r.Register(openai)
	r.Register(ollama)

	_, err := r.Complete(context.Background(), CompletionRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := AsProviderError(err)
	if !ok || pe.Class != ErrClassPermanent {
		t.Errorf("err class = %v, want ErrClassPermanent (terminal)", pe)
	}
	if ollama.calls != 0 {
		t.Errorf("fallback invoked %d times; permanent error should be terminal", ollama.calls)
	}
}

func TestRouter_BudgetError_NoFallback(t *testing.T) {
	cfg := Config{
		Routing:        map[Concern]string{ConcernComplete: "openai"},
		FallbackChains: map[Concern][]string{ConcernComplete: {"ollama"}},
	}
	openai := &stubCompletion{name: "openai", resp: CompletionResponse{Text: "ok"}}
	ollama := &stubCompletion{name: "ollama", resp: CompletionResponse{Text: "fallback"}}
	r := NewRouter(&stubLoader{cfg: cfg}, &stubBudget{
		blockedFor: map[string]error{
			"openai": &ProviderError{Class: ErrClassBudget, Provider: "openai", Wrapped: errors.New("cloud_budget_not_configured")},
		},
	}, nil)
	r.Register(openai)
	r.Register(ollama)

	_, err := r.Complete(context.Background(), CompletionRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := AsProviderError(err)
	if !ok || pe.Class != ErrClassBudget {
		t.Errorf("err class = %v, want ErrClassBudget (terminal)", pe)
	}
	// Budget error short-circuits — neither provider's Complete should run.
	if openai.calls != 0 || ollama.calls != 0 {
		t.Errorf("budget-blocked should skip both: openai=%d ollama=%d", openai.calls, ollama.calls)
	}
}

// ---------------------------------------------------------------------------
// Privacy gate
// ---------------------------------------------------------------------------

func TestRouter_PrivacyMode_FiltersToLocalOnly(t *testing.T) {
	cfg := Config{
		Routing:        map[Concern]string{ConcernComplete: "openai"},
		FallbackChains: map[Concern][]string{ConcernComplete: {"ollama"}},
		Privacy:        PrivacyPolicy{LocalProviders: []string{"ollama"}},
	}
	openai := &stubCompletion{name: "openai", resp: CompletionResponse{Text: "cloud-leak"}}
	ollama := &stubCompletion{name: "ollama", resp: CompletionResponse{Text: "from ollama"}}
	r := NewRouter(&stubLoader{cfg: cfg}, &stubBudget{}, nil)
	r.Register(openai)
	r.Register(ollama)

	resp, err := r.Complete(context.Background(), CompletionRequest{Privacy: PrivacyClassLocalOnly})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "from ollama" {
		t.Errorf("text = %q, want from ollama (privacy gate)", resp.Text)
	}
	if openai.calls != 0 {
		t.Errorf("cloud provider invoked %d times under local-only privacy gate", openai.calls)
	}
}

func TestRouter_PrivacyMode_NoLocalAvailable_ReturnsPrivacyError(t *testing.T) {
	cfg := Config{
		Routing:        map[Concern]string{ConcernComplete: "openai"},
		FallbackChains: map[Concern][]string{ConcernComplete: {"claude"}},
		Privacy:        PrivacyPolicy{LocalProviders: []string{"ollama"}}, // none of the routes are local
	}
	r := NewRouter(&stubLoader{cfg: cfg}, &stubBudget{}, nil)
	r.Register(&stubCompletion{name: "openai"})
	r.Register(&stubCompletion{name: "claude"})

	_, err := r.Complete(context.Background(), CompletionRequest{Privacy: PrivacyClassLocalOnly})
	if err == nil {
		t.Fatal("expected privacy error")
	}
	pe, ok := AsProviderError(err)
	if !ok {
		t.Fatalf("wrong err type: %v", err)
	}
	if pe.Class != ErrClassPrivacy {
		t.Errorf("class = %v, want ErrClassPrivacy", pe.Class)
	}
}

// ---------------------------------------------------------------------------
// All-fail terminal handling
// ---------------------------------------------------------------------------

func TestRouter_AllProvidersFail_ReturnsLastError(t *testing.T) {
	cfg := Config{
		Routing:        map[Concern]string{ConcernComplete: "openai"},
		FallbackChains: map[Concern][]string{ConcernComplete: {"ollama"}},
	}
	openai := &stubCompletion{name: "openai", err: &ProviderError{
		Class: ErrClassTransient, Provider: "openai", Wrapped: errors.New("a"),
	}}
	ollama := &stubCompletion{name: "ollama", err: &ProviderError{
		Class: ErrClassTransient, Provider: "ollama", Wrapped: errors.New("b"),
	}}
	r := NewRouter(&stubLoader{cfg: cfg}, &stubBudget{}, nil)
	r.Register(openai)
	r.Register(ollama)

	_, err := r.Complete(context.Background(), CompletionRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	// The wrap is `ai: all providers failed: <last>`. Want both
	// providers to have been invoked + the error to mention "b".
	if openai.calls != 1 || ollama.calls != 1 {
		t.Errorf("call counts: openai=%d ollama=%d (want 1,1)", openai.calls, ollama.calls)
	}
	if !contains(err.Error(), "b") {
		t.Errorf("err %q should reference last failure", err.Error())
	}
}

func TestRouter_NoCandidates_ReturnsNoProviderAvailable(t *testing.T) {
	// Empty routing → no candidates → ErrNoProviderAvailable.
	cfg := Config{}
	r := NewRouter(&stubLoader{cfg: cfg}, &stubBudget{}, nil)
	r.Register(&stubCompletion{name: "openai"})

	_, err := r.Complete(context.Background(), CompletionRequest{})
	if !errors.Is(err, ErrNoProviderAvailable) {
		t.Errorf("err = %v, want ErrNoProviderAvailable", err)
	}
}

// ---------------------------------------------------------------------------
// Dedup + ordering
// ---------------------------------------------------------------------------

func TestRouter_PreferredDuplicatedInFallback_NotInvokedTwice(t *testing.T) {
	cfg := Config{
		Routing:        map[Concern]string{ConcernComplete: "openai"},
		FallbackChains: map[Concern][]string{ConcernComplete: {"openai", "ollama"}},
	}
	openai := &stubCompletion{name: "openai", err: &ProviderError{Class: ErrClassTransient, Provider: "openai"}}
	ollama := &stubCompletion{name: "ollama", resp: CompletionResponse{Text: "ok"}}
	r := NewRouter(&stubLoader{cfg: cfg}, &stubBudget{}, nil)
	r.Register(openai)
	r.Register(ollama)

	if _, err := r.Complete(context.Background(), CompletionRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if openai.calls != 1 {
		t.Errorf("openai called %d times; dedup should keep it at 1", openai.calls)
	}
}

func TestRouter_RegisteredNames_ReturnsAllRegistered(t *testing.T) {
	r := NewRouter(&stubLoader{}, &stubBudget{}, nil)
	r.Register(&stubCompletion{name: "openai"})
	r.Register(&stubCompletion{name: "claude"})
	names := r.RegisteredNames()
	if len(names) != 2 {
		t.Errorf("names = %v, want 2 entries", names)
	}
	// Membership rather than order (map iteration).
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	if !have["openai"] || !have["claude"] {
		t.Errorf("missing expected names; got %v", names)
	}
}

func TestRouter_NilProviderRegistration_NoOp(t *testing.T) {
	r := NewRouter(&stubLoader{}, &stubBudget{}, nil)
	r.Register(nil) // should NOT panic
	if len(r.providers) != 0 {
		t.Errorf("nil register added %d entries", len(r.providers))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// Avoid pulling strings just for one substring check.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Compile-time check: *Tracker satisfies BudgetGate. If a future
// refactor breaks the contract, this fails at build (not test) time.
var _ BudgetGate = (*Tracker)(nil)
var _ configLoader = (*Loader)(nil)

// Sanity: the test stubs we use satisfy the interfaces. fmt is
// imported elsewhere (Sprintf in stub formatting), so this just
// references it via `_ = fmt.Sprintf` to avoid an unused-import
// flag if the production code stops using fmt.
var _ = fmt.Sprintf
