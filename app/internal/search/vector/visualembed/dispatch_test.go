// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visualembed

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualprovider"
)

// stubProvider satisfies visualprovider.Provider — no methods matter
// for dispatch tests (Dispatch never calls the provider), we just need
// a non-nil value.
type stubProvider struct{}

func (stubProvider) EmbedImage(context.Context, []byte) (visualprovider.Embedding, error) {
	return visualprovider.Embedding{}, nil
}
func (stubProvider) Health(context.Context) (visualprovider.Health, error) {
	return visualprovider.Health{Status: "ok", Dim: 768}, nil
}
func (stubProvider) Info() visualprovider.Info { return visualprovider.Info{Dim: 768} }

func TestIsImageExtension(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"jpg", true},
		{"JPG", true},
		{".jpg", true},
		{" .jpeg ", true},
		{"png", true},
		{"webp", true},
		{"gif", true},
		{"bmp", true},
		{"tif", true},
		{"tiff", true},
		{"pdf", false},
		{"mp4", false},
		{"", false},
		{"heic", false}, // future TODO — Pillow gains HEIC via pillow-heif
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := IsImageExtension(tc.in); got != tc.want {
				t.Fatalf("IsImageExtension(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestDispatch_ProviderNil_SilentSkip — sidecar not registered ⇒
// skip counter bumps; no enqueue happens.
func TestDispatch_ProviderNil_SilentSkip(t *testing.T) {
	c := NewCounter()
	d := &Dispatcher{
		Jobs:    nil,
		Logger:  slog.Default(),
		Counter: c,
		// Provider intentionally left nil for this test.
	}
	d.Dispatch(context.Background(), DispatchInput{AssetID: uuid.New(), FileExtension: "jpg"})
	if got := c.Snapshot()["visual_embed_auto_skipped"]; got != 1 {
		t.Fatalf("skipped: got %d, want 1", got)
	}
}

// TestDispatch_AutoEmbedDisabled_SilentSkip — sysconfig knob false ⇒
// skip counter bumps; no enqueue happens.
func TestDispatch_AutoEmbedDisabled_SilentSkip(t *testing.T) {
	c := NewCounter()
	// Use a non-nil Jobs pointer via a fake surface — the guard
	// short-circuits before we touch it. Passing a typed nil that
	// the getter check catches is the cleanest test shape.
	d := &Dispatcher{
		Jobs:     nil,
		Logger:   slog.Default(),
		Counter:  c,
		Provider: stubProvider{},
		EnabledGetter: func(context.Context) (bool, error) {
			return false, nil
		},
	}
	d.Dispatch(context.Background(), DispatchInput{AssetID: uuid.New(), FileExtension: "jpg"})
	if got := c.Snapshot()["visual_embed_auto_skipped"]; got != 1 {
		t.Fatalf("skipped: got %d, want 1", got)
	}
}

// TestDispatch_NonImageAsset_SilentSkip — mp4 upload skips silently.
func TestDispatch_NonImageAsset_SilentSkip(t *testing.T) {
	c := NewCounter()
	d := &Dispatcher{
		Jobs:     nil,
		Logger:   slog.Default(),
		Counter:  c,
		Provider: stubProvider{},
		EnabledGetter: func(context.Context) (bool, error) {
			return true, nil
		},
	}
	d.Dispatch(context.Background(), DispatchInput{AssetID: uuid.New(), FileExtension: "mp4"})
	if got := c.Snapshot()["visual_embed_auto_skipped"]; got != 1 {
		t.Fatalf("skipped: got %d, want 1", got)
	}
}

// TestDispatch_EnabledGetterError_SilentSkip — fail-safe on
// sysconfig read failure: don't dispatch when we can't confirm.
func TestDispatch_EnabledGetterError_SilentSkip(t *testing.T) {
	c := NewCounter()
	d := &Dispatcher{
		Jobs:     nil,
		Logger:   slog.Default(),
		Counter:  c,
		Provider: stubProvider{},
		EnabledGetter: func(context.Context) (bool, error) {
			return true, errBoom
		},
	}
	d.Dispatch(context.Background(), DispatchInput{AssetID: uuid.New(), FileExtension: "jpg"})
	if got := c.Snapshot()["visual_embed_auto_skipped"]; got != 1 {
		t.Fatalf("skipped: got %d, want 1", got)
	}
}

// TestDispatch_NilReceiver_NoPanic — Dispatch(nil dispatcher, ...)
// must be a silent no-op (mirrors the ai.embed enqueue guard shape).
func TestDispatch_NilReceiver_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil Dispatcher.Dispatch panicked: %v", r)
		}
	}()
	var d *Dispatcher
	d.Dispatch(context.Background(), DispatchInput{AssetID: uuid.New(), FileExtension: "jpg"})
}

// errBoom is a shared error value used by the sysconfig-getter test.
var errBoom = errorConstant("boom")

type errorConstant string

func (e errorConstant) Error() string { return string(e) }
