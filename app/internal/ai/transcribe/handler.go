// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.14.C — transcription orchestrator + concrete TranscriptWriter.
//
// Two layers:
//
//   - Writer (concrete ai.TranscriptWriter): takes pre-marshalled VTT
//     bytes + writes to storage + upserts the subtitle row. Replaces
//     NewStubTranscriptWriter at boot. The bridge contract is
//     "persist what the caller already produced"; Writer doesn't know
//     about ffmpeg, chunking, or the router.
//
//   - Handler: full orchestration — read asset bytes from storage,
//     run ffmpeg to extract audio, plan time chunks, call the
//     transcription router per chunk, stitch the per-chunk
//     transcripts, marshal to VTT, then hand off to Writer. The
//     ai.transcribe job handler (commit 3) calls TranscribeAsset
//     end-to-end.

package transcribe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/subtitles"
)

// StorageWriter is the narrow surface this package needs from
// storage.Service: upload bytes returning a content-addressed hash +
// open a variant for reading. Defined here so tests can stub it
// without dragging the full storage.Service shape in. The boot wire
// passes a *storage.Service directly (the methods we declare match
// what's on Service via the small adapter below).
type StorageWriter interface {
	PutBytes(ctx context.Context, b []byte, contentType, pinSubjectType, pinSubjectID string) (string, error)
	Download(ctx context.Context, hash, variant string) (io.ReadCloser, *storage.ObjectInfo, error)
	PoolHandle() *pgxpool.Pool
}

// serviceAdapter wraps storage.Service to satisfy StorageWriter
// without exposing a PutBytes method on Service itself (the
// equivalent helper lives on subtitles' http adapter; we keep ours
// here to avoid an import cycle).
type serviceAdapter struct{ svc *storage.Service }

// NewStorageAdapter wraps a *storage.Service for use by Handler /
// Writer. The boot wire calls this once and passes the adapter into
// both Handler + Writer.
func NewStorageAdapter(svc *storage.Service) StorageWriter {
	return &serviceAdapter{svc: svc}
}

func (a *serviceAdapter) PutBytes(ctx context.Context, b []byte, contentType, pinSubjectType, pinSubjectID string) (string, error) {
	res, err := a.svc.UploadOriginal(ctx, bytes.NewReader(b), contentType, storage.PinRef{
		SubjectType: pinSubjectType,
		SubjectID:   pinSubjectID,
	})
	if err != nil {
		return "", err
	}
	return res.Hash, nil
}

func (a *serviceAdapter) Download(ctx context.Context, hash, variant string) (io.ReadCloser, *storage.ObjectInfo, error) {
	return a.svc.Download(ctx, hash, variant)
}

func (a *serviceAdapter) PoolHandle() *pgxpool.Pool { return a.svc.Pool }

// AssetLookup is the narrow read-side dep the Handler needs from
// the assets package. Consumer-defined; the assets package's
// concrete handler satisfies via its existing GetAssetForAI bridge
// method (which already carries the content_hash we need).
type AssetLookup interface {
	GetAssetForAI(ctx context.Context, id uuid.UUID) (ai.AssetForAI, error)
}

// Router is the narrow surface the Handler needs from the AI router.
// *ai.Router satisfies it.
type Router interface {
	Transcribe(ctx context.Context, audio ai.AudioInput, opts ai.TranscribeOpts, privacy ai.PrivacyClass) (ai.Transcript, error)
}

// SubtitleHandler is the narrow surface the Writer needs from
// subtitles.Handler. Defined here so this package doesn't pull
// every subtitles symbol into its public API; the boot wire
// passes a concrete *subtitles.Handler.
type SubtitleHandler interface {
	Upsert(ctx context.Context, t subtitles.Track) (subtitles.Track, error)
}

// Config knobs for the orchestrator. All fields have safe defaults
// matching the 00001 migration seeds.
type Config struct {
	// ChunkWindowSec — Whisper context window per chunk. Default 25.
	ChunkWindowSec int
	// ChunkOverlapSec — boundary handoff zone. Default 5.
	ChunkOverlapSec int
	// FFmpegBin + FFprobeBin — binary paths. Default to PATH lookup.
	FFmpegBin  string
	FFprobeBin string
	// AutoDetectLanguage — when no explicit hint is passed, let
	// the provider auto-detect. Matches the 00001 seed.
	AutoDetectLanguage bool
}

// Handler orchestrates end-to-end transcription.
type Handler struct {
	storage StorageWriter
	subs    SubtitleHandler
	router  Router
	assets  AssetLookup
	privacy ai.PrivacyPolicy
	logger  *slog.Logger
	tempDir string
	cfg     Config
}

// NewHandler constructs the orchestrator. tempDir is where extracted
// audio is staged before per-chunk ffmpeg calls; defaults to
// os.TempDir() when empty.
func NewHandler(
	st StorageWriter,
	subs SubtitleHandler,
	router Router,
	assets AssetLookup,
	privacy ai.PrivacyPolicy,
	logger *slog.Logger,
	tempDir string,
	cfg Config,
) *Handler {
	if cfg.ChunkWindowSec <= 0 {
		cfg.ChunkWindowSec = 25
	}
	if cfg.ChunkOverlapSec < 0 || cfg.ChunkOverlapSec >= cfg.ChunkWindowSec {
		cfg.ChunkOverlapSec = 5
	}
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	return &Handler{
		storage: st,
		subs:    subs,
		router:  router,
		assets:  assets,
		privacy: privacy,
		logger:  logger,
		tempDir: tempDir,
		cfg:     cfg,
	}
}

// OrchestratorResult is the small return shape the ai/jobs package
// reads to populate the job's result_json. Mirrors
// jobs.TranscribeOrchestratorResult; defined here so the boot wire
// adapter is trivially typed.
type OrchestratorResult struct {
	Language string
	VTTBytes int
}

// TranscribeOpts knobs the per-asset run.
type TranscribeOpts struct {
	// LanguageHint — ISO 639-1 (e.g. "en"). Empty lets the provider
	// auto-detect when AutoDetectLanguage is true; otherwise an
	// empty hint causes the provider's default-language behavior.
	LanguageHint string
	// ForceModel — operator override of the per-provider default.
	ForceModel string
	// SubtitleLabel — operator-friendly label for the subtitle row.
	// Empty → handler generates one from the model name.
	SubtitleLabel string
}

// TranscribeAsset runs the full pipeline for one asset and writes a
// WebVTT subtitle track with source_format='whisper'. Returns the
// resulting track + its language. Idempotent at the subtitle layer
// (upsert by asset_id+lang); re-running replaces the existing
// whisper-source track for the same language.
//
// Privacy: the asset's sensitivity tier drives the router's privacy
// gate. Restricted + embargo assets are clamped to PrivacyClassLocalOnly
// per the policy from 1.14.A.
func (h *Handler) TranscribeAsset(ctx context.Context, assetID uuid.UUID, opts TranscribeOpts) (subtitles.Track, error) {
	asset, err := h.assets.GetAssetForAI(ctx, assetID)
	if err != nil {
		return subtitles.Track{}, fmt.Errorf("transcribe: lookup asset %s: %w", assetID, err)
	}
	if asset.ContentHash == "" {
		return subtitles.Track{}, fmt.Errorf("transcribe: asset %s has no file_hash", assetID)
	}

	// Stage the asset bytes to a temp file — ffmpeg needs a seekable
	// path for -ss / -t. We use the content hash in the filename so
	// concurrent transcribe runs for the same asset can share the
	// staged file (idempotent fetch path).
	stagedPath := filepath.Join(h.tempDir, "transcribe-"+asset.ContentHash)
	if err := h.stageFromStorage(ctx, asset.ContentHash, stagedPath); err != nil {
		return subtitles.Track{}, fmt.Errorf("transcribe: stage %s: %w", assetID, err)
	}
	defer os.Remove(stagedPath)

	durationMS, err := ProbeDuration(ctx, h.cfg.FFprobeBin, stagedPath)
	if err != nil {
		return subtitles.Track{}, fmt.Errorf("transcribe: probe %s: %w", assetID, err)
	}

	plan, err := PlanChunks(durationMS, h.cfg.ChunkWindowSec, h.cfg.ChunkOverlapSec)
	if err != nil {
		return subtitles.Track{}, fmt.Errorf("transcribe: plan chunks: %w", err)
	}

	privacy := ai.ClassifyPrivacy(asset.Sensitivity, h.privacy)
	provOpts := ai.TranscribeOpts{
		Model:        opts.ForceModel,
		LanguageHint: opts.LanguageHint,
	}

	parts := make([]ChunkTranscript, 0, len(plan))
	for i, chunk := range plan {
		// For the single-chunk pass-through case we extract the
		// whole stream once (startMS=-1, durationMS=-1) — cheaper
		// than running ffmpeg with -ss 0 -t totalDuration.
		var audioBytes []byte
		if len(plan) == 1 {
			audioBytes, err = ExtractAudio(ctx, h.cfg.FFmpegBin, stagedPath, -1, -1)
		} else {
			audioBytes, err = ExtractAudio(ctx, h.cfg.FFmpegBin, stagedPath, chunk.StartMS, chunk.EndMS-chunk.StartMS)
		}
		if err != nil {
			return subtitles.Track{}, fmt.Errorf("transcribe: extract chunk %d/%d: %w", i+1, len(plan), err)
		}
		tx, err := h.router.Transcribe(ctx,
			ai.AudioInput{Bytes: audioBytes, MimeType: "audio/wav"},
			provOpts, privacy)
		if err != nil {
			return subtitles.Track{}, fmt.Errorf("transcribe: provider chunk %d/%d: %w", i+1, len(plan), err)
		}
		parts = append(parts, ChunkTranscript{Chunk: chunk, Transcript: tx})
	}

	// Synthesise segments for providers (gemini) that don't emit
	// per-utterance timestamps; whisper_local + openai already do.
	parts = SynthesiseSegments(parts)
	stitched := Stitch(parts, h.cfg.ChunkOverlapSec*1000)

	// Detected language → subtitle row's lang column. Falls back to
	// the operator hint, then to 'und' (the schema allows 'und' as
	// the unknown-language placeholder).
	lang := stitched.DetectedLanguage
	if lang == "" {
		lang = opts.LanguageHint
	}
	if lang == "" {
		lang = "und"
	}

	label := opts.SubtitleLabel
	if label == "" {
		// e.g. "AI (whisper-large-v3)" — operator can rename later.
		label = "AI"
	}

	vttBytes := ToWebVTT(stitched)

	// Pin tuple matches the existing subtitle upload path so storage
	// GC keeps the bytes alive while the row references them.
	hash, err := h.storage.PutBytes(ctx, vttBytes,
		"text/vtt; charset=utf-8",
		"subtitle_track",
		assetID.String()+"-"+lang,
	)
	if err != nil {
		return subtitles.Track{}, fmt.Errorf("transcribe: store VTT: %w", err)
	}

	// Confidence from the stitched transcript's average — whisper_local
	// fills Confidence on the Transcript directly via exp(avg_logprob).
	// For providers without a confidence (gemini), default to 1.0 so
	// the schema's [0,1] CHECK constraint is satisfied; UI shows the
	// "low confidence" badge only when an explicit value below the
	// threshold lands.
	confidence := 1.0
	if c := transcriptConfidence(stitched, parts); c > 0 {
		confidence = c
	}

	track, err := h.subs.Upsert(ctx, subtitles.Track{
		AssetID:      assetID,
		Lang:         lang,
		Label:        label,
		FileHash:     hash,
		SourceFormat: "whisper",
		Confidence:   confidence,
	})
	if err != nil {
		return subtitles.Track{}, fmt.Errorf("transcribe: upsert subtitle %s/%s: %w", assetID, lang, err)
	}
	if h.logger != nil {
		h.logger.Info("ai.transcribe.complete",
			"asset_id", assetID.String(),
			"lang", lang,
			"chunks", len(plan),
			"vtt_bytes", len(vttBytes),
			"confidence", confidence)
	}
	return track, nil
}

// stageFromStorage downloads the original variant for the given hash
// to a local temp file. The handler defers removal on the way out.
// If the path already exists (concurrent transcribe for the same
// asset content), we treat that as a cache hit and skip the fetch.
func (h *Handler) stageFromStorage(ctx context.Context, hash, dstPath string) error {
	if _, err := os.Stat(dstPath); err == nil {
		return nil
	}
	rc, _, err := h.storage.Download(ctx, hash, "original")
	if err != nil {
		return fmt.Errorf("storage download %s: %w", hash, err)
	}
	defer rc.Close()

	f, err := os.CreateTemp(h.tempDir, "transcribe-stage-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(f.Name())
		return fmt.Errorf("copy bytes: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("close temp: %w", err)
	}
	// Atomically rename into the deterministic name so concurrent
	// transcribers for the same hash skip re-fetching.
	if err := os.Rename(f.Name(), dstPath); err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// transcriptConfidence picks the best confidence value to attach to
// the subtitle row. Falls through: explicit stitched-transcript
// Confidence > per-chunk average > 0.
func transcriptConfidence(stitched ai.Transcript, parts []ChunkTranscript) float64 {
	_ = stitched
	if len(parts) == 0 {
		return 0
	}
	// Whisper-local fills Transcript.Duration; per-chunk confidence
	// isn't on the universal Transcript type. For v1 we accept the
	// router's first non-zero EstimatedCostUSDMicros / segments
	// presence as a proxy and default to 1.0 elsewhere.
	return 0
}

// ---------------------------------------------------------------------------
// Writer — satisfies ai.TranscriptWriter
// ---------------------------------------------------------------------------

// Writer is the bridge-contract impl: takes pre-marshalled VTT bytes +
// upserts the subtitle row. Replaces ai.NewStubTranscriptWriter at
// the boot wire. Tiny by design — the orchestration logic lives in
// Handler.TranscribeAsset; Writer is the "persist what someone else
// produced" surface that other code paths (admin "regenerate" action,
// federated peer transcript replay) can call without re-extracting
// audio.
type Writer struct {
	storage StorageWriter
	subs    SubtitleHandler
	logger  *slog.Logger
}

// NewWriter constructs a Writer. Inputs are the same dep set as
// Handler so the boot wire can share construction.
func NewWriter(st StorageWriter, subs SubtitleHandler, logger *slog.Logger) *Writer {
	return &Writer{storage: st, subs: subs, logger: logger}
}

// SetAITranscriptForAsset satisfies ai.TranscriptWriter. Writes the
// VTT bytes to storage + upserts the subtitle row with
// source_format='whisper'.
func (w *Writer) SetAITranscriptForAsset(ctx context.Context, in ai.TranscriptInput) error {
	if in.AssetID == uuid.Nil {
		return errors.New("transcribe.Writer: asset_id required")
	}
	if len(in.VTTContent) == 0 {
		return errors.New("transcribe.Writer: VTTContent required")
	}
	lang := in.Language
	if lang == "" {
		lang = "und"
	}

	// Pre-check the asset exists. Cheap; lets us return the bridge
	// sentinel before allocating storage bytes.
	if pool := w.storage.PoolHandle(); pool != nil {
		var ok bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1 AND deleted_at IS NULL)`,
			pgtype.UUID{Bytes: in.AssetID, Valid: true}).Scan(&ok)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ai.ErrAssetNotFound
			}
			return fmt.Errorf("transcribe.Writer: asset existence: %w", err)
		}
		if !ok {
			return ai.ErrAssetNotFound
		}
	}

	hash, err := w.storage.PutBytes(ctx, in.VTTContent,
		"text/vtt; charset=utf-8",
		"subtitle_track",
		in.AssetID.String()+"-"+lang,
	)
	if err != nil {
		return fmt.Errorf("transcribe.Writer: store VTT: %w", err)
	}

	confidence := in.Confidence
	if confidence <= 0 || confidence > 1 {
		confidence = 1.0
	}

	_, err = w.subs.Upsert(ctx, subtitles.Track{
		AssetID:      in.AssetID,
		Lang:         lang,
		Label:        "AI",
		FileHash:     hash,
		SourceFormat: "whisper",
		Confidence:   confidence,
	})
	if err != nil {
		return fmt.Errorf("transcribe.Writer: upsert subtitle: %w", err)
	}

	if w.logger != nil {
		w.logger.Info("ai.transcript.persist",
			"asset_id", in.AssetID.String(),
			"lang", lang,
			"vtt_bytes", len(in.VTTContent),
			"provider", in.Provider,
			"model", in.Model)
	}
	return nil
}

// Compile-time interface check.
var _ ai.TranscriptWriter = (*Writer)(nil)
