// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package audiobook holds the async-job handlers for audiobook-
// specific post-upload work that's too slow to run inline with the
// HTTP request:
//
//   audiobook.merge   — ffmpeg-concat N audio members of a post into
//                       a single .m4b with chapter atoms baked at
//                       the file boundaries. ~1-3 minutes per hour
//                       of audio depending on whether we can use
//                       stream-copy or need to re-encode.
//
//   audiobook.decrypt — Audible .aax → .m4b via ffmpeg's
//                       -activation_bytes flag. The activation key
//                       comes from the .key sidecar Audible bundles
//                       next to the .aax (32-hex-char Key= line);
//                       AAXtoMP3 reference: https://github.com/KrumpetPirate/AAXtoMP3
//
// Both handlers are STUBS today — they register so the dispatcher
// + admin queue page see the type names, mark the asset as
// "merge_pending" / "decrypt_pending", and return a terminal
// error explaining the implementation is queued. Drop a real
// implementation into Handle() to ship; nothing else changes.

package audiobook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// MergePayload — JSON body for an audiobook.merge job.
//
// PostID identifies the post whose audio members get concatenated.
// MemberAssetIDs is the ordered list of source assets to merge —
// the dispatcher captures them at enqueue time so a later post
// edit doesn't change the merge output. ChapterTitles parallels
// MemberAssetIDs and supplies the per-member chapter title to bake
// into the m4b's chpl atom (typically each member's display title).
type MergePayload struct {
	PostID          uuid.UUID   `json:"post_id"`
	MemberAssetIDs  []uuid.UUID `json:"member_asset_ids"`
	ChapterTitles   []string    `json:"chapter_titles"`
	// OutputTitle is the title for the merged .m4b's metadata tags.
	OutputTitle string `json:"output_title"`
	// AuthorOverride / NarratorOverride — when set, override the
	// per-source ID3 tags on the output. Empty strings inherit from
	// the first source member.
	AuthorOverride   string `json:"author_override,omitempty"`
	NarratorOverride string `json:"narrator_override,omitempty"`
}

// MergeResult — empty when the stub returns. Real implementation
// will carry { merged_asset_id, duration_s, source_size, output_size,
// reencoded bool }.
type MergeResult struct {
	MergedAssetID uuid.UUID `json:"merged_asset_id,omitempty"`
	Note          string    `json:"note,omitempty"`
}

// DecryptPayload — JSON body for an audiobook.decrypt job.
//
// AssetID identifies the .aax asset. KeyAssetID, when set, points
// at a companion .key file uploaded alongside the .aax; without it
// the worker bails (no chance of guessing activation bytes).
type DecryptPayload struct {
	AssetID    uuid.UUID `json:"asset_id"`
	KeyAssetID uuid.UUID `json:"key_asset_id,omitempty"`
	// ActivationBytes — when set, skips the .key lookup and uses
	// these bytes directly. 8-hex-char string (the standard AAX
	// activation_bytes format ffmpeg accepts).
	ActivationBytes string `json:"activation_bytes,omitempty"`
}

// DecryptResult — empty when the stub returns. Real implementation
// will carry { decrypted_asset_id, duration_s, output_size }.
type DecryptResult struct {
	DecryptedAssetID uuid.UUID `json:"decrypted_asset_id,omitempty"`
	Note             string    `json:"note,omitempty"`
}

// MergeHandler is the audiobook.merge worker. Stubbed: returns
// a terminal error so retries don't keep firing while the
// implementation isn't ready.
type MergeHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger
}

func NewMergeHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *MergeHandler {
	return &MergeHandler{Pool: pool, Storage: st, SysConfig: sc, Logger: logger}
}

func (h *MergeHandler) Type() jobs.JobType { return jobs.TypeAudiobookMerge }

func (h *MergeHandler) Handle(_ context.Context, job *jobs.Claim) (json.RawMessage, error) {
	var p MergePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: err}
	}
	h.Logger.LogAttrs(context.Background(), slog.LevelWarn, "audiobook.merge.stub",
		slog.String("post_id", p.PostID.String()),
		slog.Int("members", len(p.MemberAssetIDs)))
	// TODO(phase B-2): ffmpeg concat. Plan:
	//   1. Resolve every MemberAssetID → file_hash + duration.
	//   2. Stage each source bytes to a temp dir.
	//   3. Try stream-copy first (lossless when all sources share
	//      sample rate / codec / channel layout — typical for a
	//      single audiobook). Fall back to AAC re-encode at the
	//      source bitrate when copies fail.
	//   4. Build a Quicktime chpl atom from MemberAssetIDs +
	//      ChapterTitles (start time = cumulative duration).
	//   5. Insert the merged file as a new asset with
	//      asset_type=11; attach the source post via metadata.audio.
	//      .merged_from = [{asset_id, position}].
	//   6. Optionally tombstone the source post (config flag).
	return nil, &jobs.TerminalError{Err: errors.New(
		"audiobook.merge: handler is a Phase B-2 stub — implement in app/internal/audiobook/jobs.go",
	)}
}

// DecryptHandler is the audiobook.decrypt worker. Same stub story
// as MergeHandler.
type DecryptHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger
}

func NewDecryptHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *DecryptHandler {
	return &DecryptHandler{Pool: pool, Storage: st, SysConfig: sc, Logger: logger}
}

func (h *DecryptHandler) Type() jobs.JobType { return jobs.TypeAudiobookDecrypt }

func (h *DecryptHandler) Handle(_ context.Context, job *jobs.Claim) (json.RawMessage, error) {
	var p DecryptPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: err}
	}
	h.Logger.LogAttrs(context.Background(), slog.LevelWarn, "audiobook.decrypt.stub",
		slog.String("asset_id", p.AssetID.String()))
	// TODO(phase B-2): AAX decryption. Plan:
	//   1. If ActivationBytes is empty, read the companion .key file
	//      ("Key=<32-hex>") and extract activation_bytes from the
	//      hex per AAXtoMP3's recipe.
	//   2. Shell out to:
	//        ffmpeg -activation_bytes <bytes> -i in.aax \
	//               -c copy -map_metadata 0 out.m4b
	//      (stream-copy — AAX is already AAC inside MP4 with
	//      encryption; we just strip the DRM, never re-encode.)
	//   3. Insert the decrypted .m4b as a new asset and link it
	//      back to the source .aax via metadata.audio.decrypted_from.
	return nil, &jobs.TerminalError{Err: errors.New(
		"audiobook.decrypt: handler is a Phase B-2 stub — implement in app/internal/audiobook/jobs.go",
	)}
}
