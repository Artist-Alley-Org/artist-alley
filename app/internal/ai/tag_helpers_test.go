// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// stubCompletionForTag is a CompletionProvider stub that records the
// CompletionRequest it received and returns a preset response.
type stubCompletionForTag struct {
	name        string
	respText    string
	err         error
	lastRequest CompletionRequest
}

func (s *stubCompletionForTag) Name() string             { return s.name }
func (s *stubCompletionForTag) SupportsVision() bool     { return true }
func (s *stubCompletionForTag) Complete(_ context.Context, req CompletionRequest) (CompletionResponse, error) {
	s.lastRequest = req
	if s.err != nil {
		return CompletionResponse{}, s.err
	}
	return CompletionResponse{Text: s.respText}, nil
}

func TestTagViaCompletion_ParsesOnePerLine(t *testing.T) {
	cp := &stubCompletionForTag{respText: "cat\ndog\nbird"}
	reg := NewPromptRegistry()
	tags, err := TagViaCompletion(context.Background(), cp, reg,
		AssetRef{ID: uuid.New(), PreviewURL: "http://x/p"}, TagOpts{MaxTags: 5})
	if err != nil {
		t.Fatalf("TagViaCompletion: %v", err)
	}
	if len(tags) != 3 {
		t.Fatalf("len=%d, want 3", len(tags))
	}
	if tags[0].Term != "cat" || tags[2].Term != "bird" {
		t.Errorf("tags = %+v", tags)
	}
}

func TestTagViaCompletion_StripsListMarkersAndPunctuation(t *testing.T) {
	cp := &stubCompletionForTag{respText: "- Cat,\n* Dog.\n1. BIRD;"}
	reg := NewPromptRegistry()
	tags, err := TagViaCompletion(context.Background(), cp, reg,
		AssetRef{ID: uuid.New()}, TagOpts{MaxTags: 5})
	if err != nil {
		t.Fatalf("TagViaCompletion: %v", err)
	}
	want := []string{"cat", "dog", "bird"}
	for i, w := range want {
		if i >= len(tags) || tags[i].Term != w {
			t.Errorf("tags[%d].Term = %q, want %q (full: %+v)", i, tags[i].Term, w, tags)
		}
	}
}

func TestTagViaCompletion_Dedupes(t *testing.T) {
	cp := &stubCompletionForTag{respText: "cat\ncat\nCat\ndog"}
	reg := NewPromptRegistry()
	tags, _ := TagViaCompletion(context.Background(), cp, reg,
		AssetRef{ID: uuid.New()}, TagOpts{MaxTags: 5})
	if len(tags) != 2 {
		t.Errorf("expected dedup to 2 tags, got %+v", tags)
	}
}

func TestTagViaCompletion_RespectsMaxTags(t *testing.T) {
	cp := &stubCompletionForTag{respText: "a\nb\nc\nd\ne"}
	reg := NewPromptRegistry()
	tags, _ := TagViaCompletion(context.Background(), cp, reg,
		AssetRef{ID: uuid.New()}, TagOpts{MaxTags: 3})
	if len(tags) != 3 {
		t.Errorf("len=%d, want 3 (capped)", len(tags))
	}
}

func TestTagViaCompletion_DefaultsMaxTagsToTen(t *testing.T) {
	// MaxTags=0 → default to 10. Stub renders 12 lines; result caps at 10.
	cp := &stubCompletionForTag{respText: strings.Repeat("x\n", 12)}
	reg := NewPromptRegistry()
	tags, _ := TagViaCompletion(context.Background(), cp, reg,
		AssetRef{ID: uuid.New()}, TagOpts{}) // MaxTags zero
	// "x\n" repeated gives 12 identical "x" lines — dedupe collapses to 1.
	if len(tags) != 1 {
		t.Errorf("expected dedup to 1 tag, got %+v", tags)
	}
}

func TestTagViaCompletion_PassesImageContentToProvider(t *testing.T) {
	cp := &stubCompletionForTag{respText: "cat"}
	reg := NewPromptRegistry()
	asset := AssetRef{ID: uuid.New(), PreviewURL: "http://example.test/preview.jpg"}
	_, err := TagViaCompletion(context.Background(), cp, reg, asset, TagOpts{})
	if err != nil {
		t.Fatalf("TagViaCompletion: %v", err)
	}

	// The Complete request should carry a text part + image_url part.
	msg := cp.lastRequest.Messages[0]
	if len(msg.Content) != 2 {
		t.Fatalf("content parts = %d, want 2", len(msg.Content))
	}
	if msg.Content[0].Type != ContentTypeText {
		t.Errorf("part[0].Type = %q, want text", msg.Content[0].Type)
	}
	if msg.Content[1].Type != ContentTypeImageURL {
		t.Errorf("part[1].Type = %q, want image_url", msg.Content[1].Type)
	}
	if msg.Content[1].ImageURL != "http://example.test/preview.jpg" {
		t.Errorf("image url = %q", msg.Content[1].ImageURL)
	}
}

func TestTagViaCompletion_OriginalURLPreferredOverPreview(t *testing.T) {
	cp := &stubCompletionForTag{respText: "cat"}
	reg := NewPromptRegistry()
	asset := AssetRef{
		ID:          uuid.New(),
		OriginalURL: "http://example.test/original.jpg",
		PreviewURL:  "http://example.test/preview.jpg",
	}
	_, _ = TagViaCompletion(context.Background(), cp, reg, asset, TagOpts{})
	if cp.lastRequest.Messages[0].Content[1].ImageURL != "http://example.test/original.jpg" {
		t.Errorf("expected original url to be preferred over preview, got %q",
			cp.lastRequest.Messages[0].Content[1].ImageURL)
	}
}

func TestTagViaCompletion_PropagatesCompleteError(t *testing.T) {
	cp := &stubCompletionForTag{err: errors.New("upstream boom")}
	reg := NewPromptRegistry()
	_, err := TagViaCompletion(context.Background(), cp, reg, AssetRef{ID: uuid.New()}, TagOpts{})
	if err == nil || err.Error() != "upstream boom" {
		t.Errorf("err = %v, want upstream boom", err)
	}
}

func TestTagViaCompletion_NilRegistry_Errors(t *testing.T) {
	cp := &stubCompletionForTag{}
	_, err := TagViaCompletion(context.Background(), cp, nil, AssetRef{}, TagOpts{})
	if err == nil {
		t.Error("expected error when registry is nil")
	}
}

func TestTagViaCompletion_PromptVersionRecordedOnRequest(t *testing.T) {
	cp := &stubCompletionForTag{respText: "cat"}
	reg := NewPromptRegistry()
	_, _ = TagViaCompletion(context.Background(), cp, reg, AssetRef{ID: uuid.New()}, TagOpts{})
	if cp.lastRequest.PromptVersion != "v1.0" {
		t.Errorf("PromptVersion = %q, want v1.0 (default)", cp.lastRequest.PromptVersion)
	}
}

func TestCaptionViaCompletion_HappyPath(t *testing.T) {
	cp := &stubCompletionForTag{respText: "A cat watches a sunset over the rooftops."}
	reg := NewPromptRegistry()
	got, err := CaptionViaCompletion(context.Background(), cp, reg, AssetRef{ID: uuid.New()}, CaptionOpts{})
	if err != nil {
		t.Fatalf("CaptionViaCompletion: %v", err)
	}
	if got != "A cat watches a sunset over the rooftops." {
		t.Errorf("got %q", got)
	}
}

func TestCaptionViaCompletion_StripsWhitespace(t *testing.T) {
	cp := &stubCompletionForTag{respText: "   trimmed   \n"}
	reg := NewPromptRegistry()
	got, _ := CaptionViaCompletion(context.Background(), cp, reg, AssetRef{ID: uuid.New()}, CaptionOpts{})
	if got != "trimmed" {
		t.Errorf("got %q, want \"trimmed\"", got)
	}
}

func TestCaptionViaCompletion_PropagatesError(t *testing.T) {
	cp := &stubCompletionForTag{err: errors.New("boom")}
	reg := NewPromptRegistry()
	_, err := CaptionViaCompletion(context.Background(), cp, reg, AssetRef{}, CaptionOpts{})
	if err == nil || err.Error() != "boom" {
		t.Errorf("err = %v", err)
	}
}
