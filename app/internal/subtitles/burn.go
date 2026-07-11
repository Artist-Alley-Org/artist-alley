// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.18.B-3 burned-subtitle export job — STUB.
//
// Burning a subtitle track into a video file means re-encoding the
// video stream with the chosen track baked in as visible pixels.
// The non-burned alternative (sidecar VTT) is what every modern
// player handles natively, so burning is reserved for the export
// path: a user downloads a final cut they want to share on a
// platform that doesn't honour <track kind="subtitles"> (Twitter
// uploads, embedded video on a non-HTML5 surface, etc.).
//
// Implementation outline (deferred to Phase 1.18.B-3-b):
//
//   1. Resolve AssetID → file_hash + container codec via assets pkg.
//   2. Resolve (AssetID, Lang) → VTT file_hash via subtitles.Get.
//   3. Stream both bytes to a temp dir on disk (ffmpeg subtitles
//      filter is filesystem-only — no in-memory pipe support for
//      the VTT input).
//   4. Run:
//        ffmpeg -i source.mp4 -vf subtitles=track.vtt:force_style=...
//               -c:a copy -c:v libx264 -crf 18 out.mp4
//      The font-rendering style comes from sysconfig (operator-
//      overridable; default = white/black-outlined Roboto 24pt).
//   5. Insert the encoded bytes as a new asset (asset_type=Video)
//      linked back to the source via metadata.video.burned_from =
//      {source_asset_id, lang}.
//   6. Return the new asset ID in the job result so the UI can
//      surface a "download" link.
//
// Why a stub now: the API surface + job registration need to land
// in the same commit as the OpenAPI changes so the dispatcher
// knows the type name; the actual ffmpeg integration is a separate
// PR (matches the audiobook.merge / audiobook.decrypt precedent).

package subtitles

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

// BurnPayload is the JSON shape stored in jobs.payload when the
// burn job is enqueued. AssetID is the source video; Lang picks
// which track from asset_subtitle_tracks to bake in.
type BurnPayload struct {
	AssetID uuid.UUID `json:"asset_id"`
	Lang    string    `json:"lang"`
}

// BurnHandler is the subtitle.burn worker. Phase 1.18.B-3 stub —
// matches the audiobook.{merge,decrypt} pattern of registering
// the type + returning TerminalError until the real implementation
// lands.
type BurnHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger
}

func NewBurnHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *BurnHandler {
	return &BurnHandler{Pool: pool, Storage: st, SysConfig: sc, Logger: logger}
}

func (h *BurnHandler) Type() jobs.JobType { return jobs.TypeSubtitleBurn }

func (h *BurnHandler) Handle(_ context.Context, job *jobs.Claim) (json.RawMessage, error) {
	var p BurnPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: err}
	}
	if h.Logger != nil {
		h.Logger.LogAttrs(context.Background(), slog.LevelWarn, "subtitles.burn.stub",
			slog.String("asset_id", p.AssetID.String()),
			slog.String("lang", p.Lang),
		)
	}
	return nil, &jobs.TerminalError{Err: errors.New(
		"subtitles.burn: handler is a Phase 1.18.B-3-b stub — ffmpeg integration in app/internal/subtitles/burn.go",
	)}
}
