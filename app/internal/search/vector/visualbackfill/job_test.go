// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visualbackfill

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualprovider"
)

// stubProvider satisfies visualprovider.Provider with configurable
// EmbedImage behaviour.
type stubProvider struct {
	embed func(ctx context.Context, b []byte) (visualprovider.Embedding, error)
}

func (s stubProvider) EmbedImage(ctx context.Context, b []byte) (visualprovider.Embedding, error) {
	if s.embed != nil {
		return s.embed(ctx, b)
	}
	return visualprovider.Embedding{
		Vector: make([]float32, 768), Dim: 768,
		Model: "ViT-L-14", Checkpoint: "openai",
	}, nil
}

func (s stubProvider) Health(ctx context.Context) (visualprovider.Health, error) {
	return visualprovider.Health{Status: "ok", Dim: 768}, nil
}
func (s stubProvider) Info() visualprovider.Info {
	return visualprovider.Info{Dim: 768, Model: "ViT-L-14"}
}

// stubStorage satisfies StorageAccessor.
type stubStorage struct {
	data []byte
	err  error
}

func (s stubStorage) Download(ctx context.Context, hash, variant string) (io.ReadCloser, StorageObjectInfo, error) {
	if s.err != nil {
		return nil, StorageObjectInfo{}, s.err
	}
	return io.NopCloser(bytesReader(s.data)), StorageObjectInfo{
		ContentType: "image/jpeg",
		SizeBytes:   int64(len(s.data)),
	}, nil
}

// bytesReader is a tiny io.Reader for arbitrary bytes without pulling
// bytes.NewReader (which the test file already avoids to keep the
// dependency graph minimal).
type bytesReader []byte

func (b bytesReader) Read(p []byte) (int, error) {
	if len(b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b)
	return n, nil
}

func TestIsTransientProviderError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"sidecar unreachable → transient", visualprovider.ErrSidecarUnreachable, true},
		{"wrapped sidecar unreachable → transient", errors.Join(errors.New("outer"), visualprovider.ErrSidecarUnreachable), true},
		{"dim mismatch → permanent", visualprovider.ErrDimMismatch, false},
		{"generic error → permanent", errors.New("bad decode"), false},
		{"nil → permanent (unreachable in prod)", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientProviderError(tc.err); got != tc.want {
				t.Fatalf("isTransientProviderError(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestOutcomeKind_Constants(t *testing.T) {
	// Guard against enum-value drift; the failure-handling switch in
	// Handle() relies on outcomeFailedTransient being distinct so a
	// re-order that collapses the classifications shows up here.
	if outcomeSucceeded == outcomeFailedPermanent || outcomeSucceeded == outcomeFailedTransient || outcomeFailedPermanent == outcomeFailedTransient {
		t.Fatal("outcomeKind constants collided — Handle() classification broken")
	}
}
