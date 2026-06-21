package transcribe_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	aitranscribe "github.com/mscrnt/artist-alley/app/internal/ai/transcribe"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/subtitles"
)

// stubStorage captures PutBytes args + records the calls. Used by
// the Writer happy-path test without spinning up real storage.
type stubStorage struct {
	puts []struct {
		bytes      []byte
		ctype      string
		subjType   string
		subjID     string
		returnHash string
	}
}

func (s *stubStorage) PutBytes(_ context.Context, b []byte, ctype, st, sid string) (string, error) {
	hash := "deadbeefcafebabe1234567890abcdef1234567890abcdef1234567890abcdef"
	s.puts = append(s.puts, struct {
		bytes      []byte
		ctype      string
		subjType   string
		subjID     string
		returnHash string
	}{b, ctype, st, sid, hash})
	return hash, nil
}
func (s *stubStorage) Download(_ context.Context, _, _ string) (io.ReadCloser, *storage.ObjectInfo, error) {
	return nil, nil, errors.New("stubStorage.Download not implemented for writer tests")
}
func (s *stubStorage) PoolHandle() *pgxpool.Pool { return nil }

// stubSubs records upserts. The Writer wraps storage + subs; we
// verify both got called with the right args.
type stubSubs struct {
	got    subtitles.Track
	called bool
	err    error
}

func (s *stubSubs) Upsert(_ context.Context, t subtitles.Track) (subtitles.Track, error) {
	s.called = true
	s.got = t
	return t, s.err
}

func TestWriter_HappyPath_StoresVTT_AndUpsertsTrack(t *testing.T) {
	st := &stubStorage{}
	subs := &stubSubs{}
	w := aitranscribe.NewWriter(st, subs, slog.New(slog.NewTextHandler(io.Discard, nil)))

	assetID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	vtt := []byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nhello\n\n")

	err := w.SetAITranscriptForAsset(context.Background(), ai.TranscriptInput{
		AssetID:    assetID,
		Language:   "en",
		VTTContent: vtt,
		Confidence: 0.92,
		Model:      "large-v3",
		Provider:   "whisper_local",
	})
	if err != nil {
		t.Fatalf("SetAITranscriptForAsset: %v", err)
	}
	if len(st.puts) != 1 {
		t.Fatalf("storage.PutBytes called %d times, want 1", len(st.puts))
	}
	put := st.puts[0]
	if string(put.bytes) != string(vtt) {
		t.Errorf("VTT bytes mismatch")
	}
	if !strings.HasPrefix(put.ctype, "text/vtt") {
		t.Errorf("content-type = %q, want text/vtt prefix", put.ctype)
	}
	if put.subjType != "subtitle_track" {
		t.Errorf("pin subject type = %q", put.subjType)
	}
	if !strings.Contains(put.subjID, "en") || !strings.Contains(put.subjID, assetID.String()) {
		t.Errorf("pin subject id = %q, want <asset>-en", put.subjID)
	}

	if !subs.called {
		t.Fatal("subtitles.Upsert was not called")
	}
	if subs.got.AssetID != assetID {
		t.Errorf("upsert asset = %s, want %s", subs.got.AssetID, assetID)
	}
	if subs.got.Lang != "en" {
		t.Errorf("upsert lang = %q, want en", subs.got.Lang)
	}
	if subs.got.SourceFormat != "whisper" {
		t.Errorf("upsert source_format = %q, want whisper", subs.got.SourceFormat)
	}
	if subs.got.Confidence != 0.92 {
		t.Errorf("upsert confidence = %v, want 0.92", subs.got.Confidence)
	}
	if subs.got.FileHash == "" {
		t.Error("upsert file_hash empty")
	}
}

func TestWriter_EmptyLanguage_DefaultsToUnd(t *testing.T) {
	st := &stubStorage{}
	subs := &stubSubs{}
	w := aitranscribe.NewWriter(st, subs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := w.SetAITranscriptForAsset(context.Background(), ai.TranscriptInput{
		AssetID:    uuid.New(),
		VTTContent: []byte("WEBVTT\n\n"),
		Confidence: 1.0,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if subs.got.Lang != "und" {
		t.Errorf("lang = %q, want und when empty", subs.got.Lang)
	}
}

func TestWriter_MissingArgs_Errors(t *testing.T) {
	st := &stubStorage{}
	subs := &stubSubs{}
	w := aitranscribe.NewWriter(st, subs, nil)

	if err := w.SetAITranscriptForAsset(context.Background(), ai.TranscriptInput{}); err == nil {
		t.Error("expected error on nil asset_id")
	}
	if err := w.SetAITranscriptForAsset(context.Background(), ai.TranscriptInput{
		AssetID: uuid.New(),
	}); err == nil {
		t.Error("expected error on empty VTTContent")
	}
}

func TestWriter_ConfidenceClampsToOne(t *testing.T) {
	st := &stubStorage{}
	subs := &stubSubs{}
	w := aitranscribe.NewWriter(st, subs, nil)

	// Confidence outside [0, 1] → clamps to 1.0 (schema CHECK is
	// [0, 1]; we'd rather upsert with a safe value than crash on
	// a provider that fills the field weirdly).
	err := w.SetAITranscriptForAsset(context.Background(), ai.TranscriptInput{
		AssetID:    uuid.New(),
		VTTContent: []byte("WEBVTT\n\n"),
		Confidence: 5.0,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if subs.got.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 (clamped)", subs.got.Confidence)
	}
}

// Compile-time: Writer satisfies ai.TranscriptWriter.
var _ ai.TranscriptWriter = (*aitranscribe.Writer)(nil)
