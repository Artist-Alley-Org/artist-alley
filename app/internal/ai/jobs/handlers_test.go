package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type stubTagRouter struct {
	tags []ai.Tag
	err  error

	gotAsset   ai.AssetRef
	gotOpts    ai.TagOpts
	gotPrivacy ai.PrivacyClass
}

func (s *stubTagRouter) Tag(_ context.Context, asset ai.AssetRef, opts ai.TagOpts, privacy ai.PrivacyClass) ([]ai.Tag, error) {
	s.gotAsset = asset
	s.gotOpts = opts
	s.gotPrivacy = privacy
	return s.tags, s.err
}

type stubCaptionRouter struct {
	caption string
	err     error

	gotPrivacy ai.PrivacyClass
}

func (s *stubCaptionRouter) Caption(_ context.Context, asset ai.AssetRef, opts ai.CaptionOpts, privacy ai.PrivacyClass) (string, error) {
	s.gotPrivacy = privacy
	_ = asset
	_ = opts
	return s.caption, s.err
}

type stubAssets struct {
	asset       ai.AssetRef
	sensitivity ai.SensitivityTier
	found       bool
	err         error
}

func (s *stubAssets) LookupForAI(_ context.Context, _ uuid.UUID) (ai.AssetRef, ai.SensitivityTier, bool, error) {
	return s.asset, s.sensitivity, s.found, s.err
}

type stubTagWriter struct {
	got      []ai.Tag
	gotProv  Provenance
	gotAsset uuid.UUID
	err      error
}

func (s *stubTagWriter) SetTagsFromAI(_ context.Context, id uuid.UUID, tags []ai.Tag, prov Provenance) error {
	s.gotAsset = id
	s.got = tags
	s.gotProv = prov
	return s.err
}

type stubCaptionWriter struct {
	got      string
	gotProv  Provenance
	gotAsset uuid.UUID
	err      error
}

func (s *stubCaptionWriter) SetCaptionFromAI(_ context.Context, id uuid.UUID, c string, prov Provenance) error {
	s.gotAsset = id
	s.got = c
	s.gotProv = prov
	return s.err
}

// ---------------------------------------------------------------------------
// TagHandler
// ---------------------------------------------------------------------------

func TestTagHandler_Type(t *testing.T) {
	h := &TagHandler{}
	if h.Type() != JobTypeTag {
		t.Errorf("Type = %q", h.Type())
	}
}

func TestTagHandler_HappyPath_PersistsTagsAndReturnsResult(t *testing.T) {
	assetID := uuid.New()
	router := &stubTagRouter{tags: []ai.Tag{{Term: "cat"}, {Term: "dog"}}}
	assets := &stubAssets{
		asset:       ai.AssetRef{ID: assetID, PreviewURL: "http://x/p.jpg"},
		sensitivity: ai.SensitivityPublic,
		found:       true,
	}
	writer := &stubTagWriter{}

	h := NewTagHandler(router, assets, writer, ai.PrivacyPolicy{
		LockSensitiveToLocal: true,
	})

	payload, _ := json.Marshal(TagPayload{
		AssetID:       assetID,
		PromptVersion: "v1.0",
		MaxTags:       5,
	})
	result, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(writer.got) != 2 {
		t.Errorf("writer got %d tags, want 2", len(writer.got))
	}
	if writer.gotAsset != assetID {
		t.Error("writer received wrong asset id")
	}
	if writer.gotProv.PromptVersion != "v1.0" {
		t.Errorf("provenance.PromptVersion = %q", writer.gotProv.PromptVersion)
	}

	var resultMap map[string]int
	_ = json.Unmarshal(result, &resultMap)
	if resultMap["tag_count"] != 2 {
		t.Errorf("result = %v", resultMap)
	}
}

func TestTagHandler_PublicAsset_PassesPrivacyAny(t *testing.T) {
	router := &stubTagRouter{tags: []ai.Tag{}}
	assets := &stubAssets{
		asset:       ai.AssetRef{ID: uuid.New()},
		sensitivity: ai.SensitivityPublic,
		found:       true,
	}
	h := NewTagHandler(router, assets, &stubTagWriter{}, ai.PrivacyPolicy{
		LockSensitiveToLocal: true,
	})
	payload, _ := json.Marshal(TagPayload{AssetID: assets.asset.ID})
	_, _ = h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if router.gotPrivacy != ai.PrivacyClassAny {
		t.Errorf("privacy class = %q, want any (public asset)", router.gotPrivacy)
	}
}

func TestTagHandler_RestrictedAsset_LocksToLocal(t *testing.T) {
	router := &stubTagRouter{tags: []ai.Tag{}}
	assets := &stubAssets{
		asset:       ai.AssetRef{ID: uuid.New()},
		sensitivity: ai.SensitivityRestricted,
		found:       true,
	}
	h := NewTagHandler(router, assets, &stubTagWriter{}, ai.PrivacyPolicy{
		LockSensitiveToLocal: true,
		LocalProviders:       []string{"ollama"},
	})
	payload, _ := json.Marshal(TagPayload{AssetID: assets.asset.ID})
	_, _ = h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if router.gotPrivacy != ai.PrivacyClassLocalOnly {
		t.Errorf("privacy class = %q, want local_only (restricted asset)", router.gotPrivacy)
	}
}

func TestTagHandler_BadPayload_TerminalError(t *testing.T) {
	h := &TagHandler{Assets: &stubAssets{}}
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: []byte("not json")})
	if !jobs.IsTerminal(err) {
		t.Errorf("err should be terminal, got %v", err)
	}
}

func TestTagHandler_AssetNotFound_TerminalError(t *testing.T) {
	assets := &stubAssets{found: false}
	h := NewTagHandler(&stubTagRouter{}, assets, &stubTagWriter{}, ai.PrivacyPolicy{})
	payload, _ := json.Marshal(TagPayload{AssetID: uuid.New()})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if !jobs.IsTerminal(err) {
		t.Errorf("err should be terminal on missing asset, got %v", err)
	}
}

func TestTagHandler_RouterPermanentError_TerminalError(t *testing.T) {
	router := &stubTagRouter{err: &ai.ProviderError{Class: ai.ErrClassPermanent, Provider: "openai"}}
	assets := &stubAssets{
		asset: ai.AssetRef{ID: uuid.New()}, sensitivity: ai.SensitivityPublic, found: true,
	}
	h := NewTagHandler(router, assets, &stubTagWriter{}, ai.PrivacyPolicy{})
	payload, _ := json.Marshal(TagPayload{AssetID: assets.asset.ID})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if !jobs.IsTerminal(err) {
		t.Errorf("permanent router err should map to terminal, got %v", err)
	}
}

func TestTagHandler_RouterBudgetError_TerminalError(t *testing.T) {
	router := &stubTagRouter{err: &ai.ProviderError{
		Class: ai.ErrClassBudget, Provider: "openai",
		Wrapped: errors.New("cloud_budget_not_configured"),
	}}
	assets := &stubAssets{
		asset: ai.AssetRef{ID: uuid.New()}, sensitivity: ai.SensitivityPublic, found: true,
	}
	h := NewTagHandler(router, assets, &stubTagWriter{}, ai.PrivacyPolicy{})
	payload, _ := json.Marshal(TagPayload{AssetID: assets.asset.ID})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if !jobs.IsTerminal(err) {
		t.Errorf("budget err should map to terminal (operator must act); got %v", err)
	}
}

func TestTagHandler_RouterTransientError_RetriableError(t *testing.T) {
	router := &stubTagRouter{err: &ai.ProviderError{Class: ai.ErrClassTransient, Provider: "openai"}}
	assets := &stubAssets{
		asset: ai.AssetRef{ID: uuid.New()}, sensitivity: ai.SensitivityPublic, found: true,
	}
	h := NewTagHandler(router, assets, &stubTagWriter{}, ai.PrivacyPolicy{})
	payload, _ := json.Marshal(TagPayload{AssetID: assets.asset.ID})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if jobs.IsTerminal(err) {
		t.Errorf("transient err should NOT be terminal (worker retries); got %v", err)
	}
	if err == nil {
		t.Error("expected error")
	}
}

func TestTagHandler_WriterError_NotTerminal(t *testing.T) {
	// Persistence failure could be transient (DB hiccup) — let the
	// worker retry rather than marking permanent.
	writer := &stubTagWriter{err: errors.New("db down")}
	router := &stubTagRouter{tags: []ai.Tag{{Term: "x"}}}
	assets := &stubAssets{
		asset: ai.AssetRef{ID: uuid.New()}, sensitivity: ai.SensitivityPublic, found: true,
	}
	h := NewTagHandler(router, assets, writer, ai.PrivacyPolicy{})
	payload, _ := json.Marshal(TagPayload{AssetID: assets.asset.ID})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if jobs.IsTerminal(err) {
		t.Errorf("writer err should NOT be terminal; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CaptionHandler
// ---------------------------------------------------------------------------

func TestCaptionHandler_Type(t *testing.T) {
	h := &CaptionHandler{}
	if h.Type() != JobTypeCaption {
		t.Errorf("Type = %q", h.Type())
	}
}

func TestCaptionHandler_HappyPath(t *testing.T) {
	assetID := uuid.New()
	router := &stubCaptionRouter{caption: "A serene mountain landscape."}
	assets := &stubAssets{
		asset: ai.AssetRef{ID: assetID}, sensitivity: ai.SensitivityPublic, found: true,
	}
	writer := &stubCaptionWriter{}
	h := NewCaptionHandler(router, assets, writer, ai.PrivacyPolicy{})
	payload, _ := json.Marshal(CaptionPayload{AssetID: assetID, PromptVersion: "v1.0"})
	result, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if writer.got != "A serene mountain landscape." {
		t.Errorf("writer got = %q", writer.got)
	}
	if writer.gotProv.PromptVersion != "v1.0" {
		t.Errorf("prov.PromptVersion = %q", writer.gotProv.PromptVersion)
	}
	var rmap map[string]int
	_ = json.Unmarshal(result, &rmap)
	if rmap["caption_length"] != len(writer.got) {
		t.Errorf("result = %v", rmap)
	}
}

func TestCaptionHandler_PrivacyMode_LocksToLocal(t *testing.T) {
	router := &stubCaptionRouter{caption: ""}
	assets := &stubAssets{
		asset: ai.AssetRef{ID: uuid.New()}, sensitivity: ai.SensitivityEmbargo, found: true,
	}
	h := NewCaptionHandler(router, assets, &stubCaptionWriter{}, ai.PrivacyPolicy{
		LockSensitiveToLocal: true,
		LocalProviders:       []string{"ollama"},
	})
	payload, _ := json.Marshal(CaptionPayload{AssetID: assets.asset.ID})
	_, _ = h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if router.gotPrivacy != ai.PrivacyClassLocalOnly {
		t.Errorf("privacy = %q, want local_only", router.gotPrivacy)
	}
}

// ---------------------------------------------------------------------------
// Idempotency keys
// ---------------------------------------------------------------------------

func TestTagIdempotencyKey_StableAcrossRuns(t *testing.T) {
	id := uuid.New()
	k1 := TagIdempotencyKey(id, "v1.0")
	k2 := TagIdempotencyKey(id, "v1.0")
	if k1 != k2 {
		t.Errorf("same inputs give different keys: %s vs %s", k1, k2)
	}
}

func TestTagIdempotencyKey_DifferentVersionsGiveDifferentKeys(t *testing.T) {
	id := uuid.New()
	if TagIdempotencyKey(id, "v1.0") == TagIdempotencyKey(id, "v2.0") {
		t.Error("version bump should give a new key")
	}
}

func TestTagIdempotencyKey_DifferentAssetsGiveDifferentKeys(t *testing.T) {
	if TagIdempotencyKey(uuid.New(), "v1.0") == TagIdempotencyKey(uuid.New(), "v1.0") {
		t.Error("different assets should give different keys")
	}
}

func TestCaptionIdempotencyKey_DifferentFromTagKey(t *testing.T) {
	// Same asset + same prompt version — but caption and tag are
	// different job types and should derive different idempotency
	// keys (or the UNIQUE INDEX would conflate two unrelated jobs).
	id := uuid.New()
	if TagIdempotencyKey(id, "v1.0") == CaptionIdempotencyKey(id, "v1.0") {
		t.Error("tag + caption keys should differ even for same asset/version")
	}
}

// ---------------------------------------------------------------------------
// Compile-time interface conformance
// ---------------------------------------------------------------------------

// Sanity that the stubs satisfy the interfaces — catches drift if a
// future refactor changes a method signature.
var _ TagRouter = (*stubTagRouter)(nil)
var _ CaptionRouter = (*stubCaptionRouter)(nil)
var _ AssetLookup = (*stubAssets)(nil)
var _ TagWriter = (*stubTagWriter)(nil)
var _ CaptionWriter = (*stubCaptionWriter)(nil)
