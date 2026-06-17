// Phase 1.18.B-3 policy guard tests.

package subtitles

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// stubQuerier implements the requiresAudioVideoQuerier interface
// so the policy unit tests can return arbitrary kinds without a
// real Postgres.
type stubQuerier struct {
	kind *string
	err  error
}

func (s *stubQuerier) GetAssetRenderableKind(_ context.Context, _ pgtype.UUID) (*string, error) {
	return s.kind, s.err
}

func ptr(s string) *string { return &s }

func TestRequiresAudioVideo_VideoAsset_NoError(t *testing.T) {
	err := RequiresAudioVideo(context.Background(), &stubQuerier{kind: ptr("Video")}, uuid.New())
	if err != nil {
		t.Errorf("err=%v, want nil for Video", err)
	}
}

func TestRequiresAudioVideo_AudioAsset_NoError(t *testing.T) {
	err := RequiresAudioVideo(context.Background(), &stubQuerier{kind: ptr("Audio")}, uuid.New())
	if err != nil {
		t.Errorf("err=%v, want nil for Audio", err)
	}
}

func TestRequiresAudioVideo_Audiobook_NoError(t *testing.T) {
	err := RequiresAudioVideo(context.Background(), &stubQuerier{kind: ptr("Audiobook")}, uuid.New())
	if err != nil {
		t.Errorf("err=%v, want nil for Audiobook", err)
	}
}

func TestRequiresAudioVideo_ImageAsset_NotApplicable(t *testing.T) {
	err := RequiresAudioVideo(context.Background(), &stubQuerier{kind: ptr("Image")}, uuid.New())
	if !errors.Is(err, ErrSubtitlesNotApplicable) {
		t.Errorf("err=%v, want ErrSubtitlesNotApplicable for Image", err)
	}
}

func TestRequiresAudioVideo_3DAsset_NotApplicable(t *testing.T) {
	err := RequiresAudioVideo(context.Background(), &stubQuerier{kind: ptr("3D Object")}, uuid.New())
	if !errors.Is(err, ErrSubtitlesNotApplicable) {
		t.Errorf("err=%v, want ErrSubtitlesNotApplicable for 3D Object", err)
	}
}

func TestRequiresAudioVideo_PdfAsset_NotApplicable(t *testing.T) {
	err := RequiresAudioVideo(context.Background(), &stubQuerier{kind: ptr("Document")}, uuid.New())
	if !errors.Is(err, ErrSubtitlesNotApplicable) {
		t.Errorf("err=%v, want ErrSubtitlesNotApplicable for Document", err)
	}
}

func TestRequiresAudioVideo_AssetNotFound(t *testing.T) {
	err := RequiresAudioVideo(context.Background(), &stubQuerier{err: pgx.ErrNoRows}, uuid.New())
	if !errors.Is(err, ErrAssetNotFound) {
		t.Errorf("err=%v, want ErrAssetNotFound", err)
	}
}

func TestRequiresAudioVideo_NullKind_NotApplicable(t *testing.T) {
	err := RequiresAudioVideo(context.Background(), &stubQuerier{kind: nil}, uuid.New())
	if !errors.Is(err, ErrSubtitlesNotApplicable) {
		t.Errorf("err=%v, want ErrSubtitlesNotApplicable for nil kind", err)
	}
}

func TestValidateLang_ValidTags(t *testing.T) {
	for _, lang := range []string{"en", "en-US", "ja", "fr-CA", "und", "zh-Hant"} {
		if err := ValidateLang(lang); err != nil {
			t.Errorf("ValidateLang(%q) = %v, want nil", lang, err)
		}
	}
}

func TestValidateLang_InvalidTags(t *testing.T) {
	// Note: "english" (7 chars) is technically RFC 5646-valid as a
	// registered language subtag, so we don't reject it. We rely
	// on the upstream chooser (sidecar parser, UI) to use 2-3
	// letter ISO codes by convention. Real garbage is what we
	// catch here: empty, all-digits, spaces, >8 chars, double
	// hyphens.
	for _, lang := range []string{"", "1234", "en US", "verylongtagthatdoesntfit", "en--US"} {
		if err := ValidateLang(lang); err == nil {
			t.Errorf("ValidateLang(%q) accepted; want error", lang)
		}
	}
}
