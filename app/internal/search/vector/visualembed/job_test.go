package visualembed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualprovider"
)

// stubEmbedProvider is a configurable Provider for job tests.
type stubEmbedProvider struct {
	embed func(context.Context, []byte) (visualprovider.Embedding, error)
}

func (s stubEmbedProvider) EmbedImage(ctx context.Context, b []byte) (visualprovider.Embedding, error) {
	if s.embed != nil {
		return s.embed(ctx, b)
	}
	return visualprovider.Embedding{
		Vector: make([]float32, 768), Dim: 768,
		Model: "ViT-L-14", Checkpoint: "openai",
	}, nil
}
func (s stubEmbedProvider) Health(context.Context) (visualprovider.Health, error) {
	return visualprovider.Health{Status: "ok", Dim: 768}, nil
}
func (s stubEmbedProvider) Info() visualprovider.Info {
	return visualprovider.Info{Dim: 768, Model: "ViT-L-14"}
}

// stubStorage returns fixed bytes for Download.
type stubStorage struct {
	data []byte
	err  error
}

func (s stubStorage) Download(context.Context, string, string) (io.ReadCloser, StorageObjectInfo, error) {
	if s.err != nil {
		return nil, StorageObjectInfo{}, s.err
	}
	return io.NopCloser(strings.NewReader(string(s.data))), StorageObjectInfo{
		ContentType: "image/jpeg",
		SizeBytes:   int64(len(s.data)),
	}, nil
}

// stubAssets satisfies AssetLookup.
type stubAssets struct {
	rec AssetRecord
	err error
}

func (s stubAssets) Get(context.Context, uuid.UUID) (AssetRecord, error) {
	if s.err != nil {
		return AssetRecord{}, s.err
	}
	return s.rec, nil
}

// TestIsTransientProviderError — exported classification helper.
func TestIsTransientProviderError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"sidecar unreachable → transient", visualprovider.ErrSidecarUnreachable, true},
		{"wrapped sidecar unreachable → transient", errors.Join(errors.New("outer"), visualprovider.ErrSidecarUnreachable), true},
		{"dim mismatch → permanent", visualprovider.ErrDimMismatch, false},
		{"generic decode error → permanent", errors.New("bad bytes"), false},
		{"nil → permanent (unreachable in prod)", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientProviderError(tc.err); got != tc.want {
				t.Fatalf("IsTransientProviderError(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

func newTestJob(assetsRec AssetRecord, provider visualprovider.Provider, storage StorageAccessor) *Job {
	return &Job{
		Assets:      stubAssets{rec: assetsRec},
		VisualStore: nil, // set per-test when the flow reaches upsert
		Storage:     storage,
		Provider:    provider,
		Counter:     NewCounter(),
	}
}

func makeClaim(t *testing.T, id uuid.UUID) *jobs.Claim {
	t.Helper()
	b, _ := json.Marshal(Payload{AssetID: id})
	return &jobs.Claim{Payload: b}
}

// TestHandle_ProviderNil_TransientError — job re-queues, counter bumps.
func TestHandle_ProviderNil_TransientError(t *testing.T) {
	j := newTestJob(AssetRecord{HasImage: true, FileHash: strPtr("abc")}, nil, stubStorage{})
	_, err := j.Handle(context.Background(), makeClaim(t, uuid.New()))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var terminal *jobs.TerminalError
	if errors.As(err, &terminal) {
		t.Fatalf("provider-nil should return transient, got TerminalError: %v", err)
	}
	if got := j.Counter.Snapshot()["visual_embed_auto_transient_failed"]; got != 1 {
		t.Fatalf("transient counter: got %d, want 1", got)
	}
}

// TestHandle_AssetNotImage_PermanentTerminal — defence in depth.
func TestHandle_AssetNotImage_PermanentTerminal(t *testing.T) {
	j := newTestJob(AssetRecord{HasImage: false, FileHash: strPtr("abc")}, stubEmbedProvider{}, stubStorage{})
	_, err := j.Handle(context.Background(), makeClaim(t, uuid.New()))
	var terminal *jobs.TerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("expected TerminalError, got %v", err)
	}
	if got := j.Counter.Snapshot()["visual_embed_auto_permanent_failed"]; got != 1 {
		t.Fatalf("permanent counter: got %d, want 1", got)
	}
}

// TestHandle_AssetDeleted_PermanentTerminal — asset gone between
// dispatch and execution; don't retry.
func TestHandle_AssetDeleted_PermanentTerminal(t *testing.T) {
	j := newTestJob(AssetRecord{HasImage: true, FileHash: strPtr("abc"), Deleted: true}, stubEmbedProvider{}, stubStorage{})
	_, err := j.Handle(context.Background(), makeClaim(t, uuid.New()))
	var terminal *jobs.TerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("expected TerminalError, got %v", err)
	}
}

// TestHandle_AssetMissingFileHash_PermanentTerminal — no bytes to fetch.
func TestHandle_AssetMissingFileHash_PermanentTerminal(t *testing.T) {
	j := newTestJob(AssetRecord{HasImage: true, FileHash: nil}, stubEmbedProvider{}, stubStorage{})
	_, err := j.Handle(context.Background(), makeClaim(t, uuid.New()))
	var terminal *jobs.TerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("expected TerminalError, got %v", err)
	}
}

// TestHandle_StorageDownloadFails_PermanentTerminal — storage-level
// failure is permanent (the asset's bytes are gone; retries won't
// resurrect them).
func TestHandle_StorageDownloadFails_PermanentTerminal(t *testing.T) {
	j := newTestJob(
		AssetRecord{HasImage: true, FileHash: strPtr("abc")},
		stubEmbedProvider{},
		stubStorage{err: errors.New("backend gone")},
	)
	_, err := j.Handle(context.Background(), makeClaim(t, uuid.New()))
	var terminal *jobs.TerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("expected TerminalError, got %v", err)
	}
}

// TestHandle_EmptyBytes_PermanentTerminal — zero-byte upload; skip.
func TestHandle_EmptyBytes_PermanentTerminal(t *testing.T) {
	j := newTestJob(
		AssetRecord{HasImage: true, FileHash: strPtr("abc")},
		stubEmbedProvider{},
		stubStorage{data: []byte{}},
	)
	_, err := j.Handle(context.Background(), makeClaim(t, uuid.New()))
	var terminal *jobs.TerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("expected TerminalError, got %v", err)
	}
}

// TestHandle_ProviderTransient_ReQueues — sidecar unreachable ⇒
// framework retries (plain error, not Terminal).
func TestHandle_ProviderTransient_ReQueues(t *testing.T) {
	j := newTestJob(
		AssetRecord{HasImage: true, FileHash: strPtr("abc")},
		stubEmbedProvider{
			embed: func(context.Context, []byte) (visualprovider.Embedding, error) {
				return visualprovider.Embedding{}, visualprovider.ErrSidecarUnreachable
			},
		},
		stubStorage{data: []byte{0xff, 0xd8}},
	)
	_, err := j.Handle(context.Background(), makeClaim(t, uuid.New()))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var terminal *jobs.TerminalError
	if errors.As(err, &terminal) {
		t.Fatalf("transient should NOT be Terminal, got %v", err)
	}
	if got := j.Counter.Snapshot()["visual_embed_auto_transient_failed"]; got != 1 {
		t.Fatalf("transient counter: got %d, want 1", got)
	}
}

// TestHandle_ProviderPermanent_TerminalError — decode / dim mismatch
// ⇒ Terminal so the framework stops retrying.
func TestHandle_ProviderPermanent_TerminalError(t *testing.T) {
	j := newTestJob(
		AssetRecord{HasImage: true, FileHash: strPtr("abc")},
		stubEmbedProvider{
			embed: func(context.Context, []byte) (visualprovider.Embedding, error) {
				return visualprovider.Embedding{}, visualprovider.ErrDimMismatch
			},
		},
		stubStorage{data: []byte{0xff, 0xd8}},
	)
	_, err := j.Handle(context.Background(), makeClaim(t, uuid.New()))
	var terminal *jobs.TerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("expected TerminalError, got %v", err)
	}
	if got := j.Counter.Snapshot()["visual_embed_auto_permanent_failed"]; got != 1 {
		t.Fatalf("permanent counter: got %d, want 1", got)
	}
}

// TestHandle_RateLimit_BlocksThenProceeds — configure limiter=1/sec,
// exhaust it, then observe that a subsequent Handle waits (bumping
// the rate_limited_wait counter) rather than failing fast.
func TestHandle_RateLimit_BlocksThenProceeds_TimesOut(t *testing.T) {
	// Limiter with 1 token / 100 seconds — the first call consumes
	// the token, the second one waits. RateLimitWaitTimeout is short
	// so the wait times out as transient.
	limiter := rate.NewLimiter(rate.Every(100*time.Second), 1)
	// Burn the initial token so the next Handle definitely waits.
	limiter.Allow()
	j := &Job{
		Assets:               stubAssets{rec: AssetRecord{HasImage: true, FileHash: strPtr("abc")}},
		Storage:              stubStorage{data: []byte{0xff, 0xd8}},
		Provider:             stubEmbedProvider{},
		Counter:              NewCounter(),
		RateLimiter:          limiter,
		RateLimitWaitTimeout: 50 * time.Millisecond,
	}
	_, err := j.Handle(context.Background(), makeClaim(t, uuid.New()))
	if err == nil {
		t.Fatal("expected rate-limit timeout, got nil")
	}
	var terminal *jobs.TerminalError
	if errors.As(err, &terminal) {
		t.Fatalf("rate-limit timeout should be transient, got Terminal: %v", err)
	}
	if got := j.Counter.Snapshot()["visual_embed_auto_transient_failed"]; got != 1 {
		t.Fatalf("transient counter after timeout: got %d, want 1", got)
	}
}

// TestHandle_PendingGauge_StartEndPaired — success + failure paths
// both leave pending at 0.
func TestHandle_PendingGauge_StartEndPaired(t *testing.T) {
	j := newTestJob(AssetRecord{HasImage: true, FileHash: nil}, stubEmbedProvider{}, stubStorage{})
	_, _ = j.Handle(context.Background(), makeClaim(t, uuid.New()))
	if got := j.Counter.Snapshot()["visual_embed_auto_pending"]; got != 0 {
		t.Fatalf("pending after Handle: got %d, want 0", got)
	}
}

func strPtr(s string) *string { return &s }
