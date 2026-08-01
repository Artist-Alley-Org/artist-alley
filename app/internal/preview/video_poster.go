// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// VideoPosterResult — what the cheap poster job writes back to
// jobs.result for the admin queue view.
//
// AtS and MeanLuma are the two numbers that say whether #810's frame
// selection did its job on this asset, per asset, without anyone having
// to look at the picture. A row with MeanLuma near zero is a clip that
// is genuinely dark all the way through, not a bug.
// omitempty on the frame-selection trio is load-bearing, not tidiness:
// the skip path measures nothing, and a zero-valued `mean_luma: 0` in
// the admin queue reads as "this poster is pitch black" — the exact
// condition #810 is about — when it means "no frame was chosen". An
// absent key cannot be misread that way, and it makes `WHERE result ?
// 'mean_luma'` select the runs that actually measured something.
type VideoPosterResult struct {
	Rendered  bool    `json:"rendered"`
	AtS       float64 `json:"at_s,omitempty"`
	MeanLuma  float64 `json:"mean_luma,omitempty"`
	Tries     int     `json:"tries,omitempty"`
	DurationS float64 `json:"duration_s,omitempty"`
	WorkS     float64 `json:"work_s"`
}

// VideoPosterHandler renders JUST the poster frame and the raster ladder
// for a video, then stops (#818).
//
// WHY IT IS A SEPARATE JOB AND NOT A REORDER INSIDE preview.video. The
// ordering inside that job is already right — the poster commits before
// the HLS ladder, which commits before the sprite sheet. The gap is
// entirely BEFORE the job is claimed: preview.video runs at concurrency
// 2 and takes minutes per asset, so on a bulk upload the hundredth video
// waits for the ninety-ninth encode before anything at all appears on
// its card. Nothing that happens inside the job can fix a queue.
//
// WHY IT EMBEDS VideoHandler rather than reimplementing. Staging,
// probing, frame selection and the ladder fan are all shared, and a
// second copy of any of them is a second thing to keep in step with
// #810. This type overrides exactly two methods: which queue it reads,
// and where it stops.
//
// WHAT IT DELIBERATELY DOES NOT DO: touch assets.processing_status. The
// asset is not ready — it has a picture but no stream — and marking it
// so would trade a blank card for a video that cannot play. The status
// transitions stay with preview.video, which is the job that finishes
// the asset.
type VideoPosterHandler struct {
	*VideoHandler
}

// NewVideoPosterHandler — recommended constructor.
//
// MaxJobDuration is minutes, not the full handler's two hours: this job
// does one seek and one JPEG encode. If it has not finished in ten
// minutes the source is pathological, and the right outcome is to let
// the full ladder deal with it rather than to hold a worker slot.
func NewVideoPosterHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *VideoPosterHandler {
	inner := NewVideoHandler(pool, st, sc, logger)
	inner.MaxJobDuration = 10 * time.Minute
	return &VideoPosterHandler{VideoHandler: inner}
}

// Type implements jobs.Handler.
func (h *VideoPosterHandler) Type() jobs.JobType { return jobs.TypePreviewVideoPoster }

// Handle implements jobs.Handler.
func (h *VideoPosterHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()
	var p VideoPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.video.poster: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.video.poster: file_hash is required")}
	}
	if !isVideoExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.video.poster: extension %q is not a video format", p.FileExtension)}
	}

	jobCtx, cancel := context.WithTimeout(ctx, h.MaxJobDuration)
	defer cancel()

	result := VideoPosterResult{}

	// Everything this job produces is already on the backend, so there
	// is nothing to download the source for. The reconcile inside
	// variantDone/ladderDone has still run by the time this returns
	// (#827), and the thumbhash heal covers the case where the rows were
	// the only thing missing.
	if variantDone(jobCtx, h.Storage, p.FileHash, "poster", p.Force) &&
		ladderDone(jobCtx, h.Storage, p.FileHash, p.Force) {
		healThumbhashOnSkip(jobCtx, ladderInput{
			Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
			AssetID: p.AssetID, Hash: p.FileHash, Kind: "video.poster",
		})
		result.WorkS = time.Since(started).Seconds()
		return json.Marshal(result)
	}

	work, cleanup, err := h.stage(jobCtx, p.FileHash)
	if err != nil {
		// No markFailed. This job failing says nothing about whether
		// preview.video will succeed — it stages the same source through
		// the same code — and stamping the asset failed here would put a
		// permanent error on a row that is about to be rendered fine.
		return nil, &jobs.TerminalError{Err: err}
	}
	defer cleanup()

	probe, err := h.probe(jobCtx, work.sourcePath)
	if err != nil {
		return nil, &jobs.TerminalError{Err: err}
	}
	result.DurationS = probe.DurationS

	pick, err := h.selectPoster(jobCtx, work, probe)
	if err != nil {
		return nil, fmt.Errorf("preview.video.poster: %w", err)
	}
	result.AtS, result.MeanLuma, result.Tries = pick.atS, pick.luma, pick.tries

	if !variantDone(jobCtx, h.Storage, p.FileHash, "poster", p.Force) {
		if err := h.uploadFile(jobCtx, p.FileHash, "poster", pick.path, "image/jpeg"); err != nil {
			return nil, fmt.Errorf("preview.video.poster: upload: %w", err)
		}
		result.Rendered = true
	}
	// The ladder fan is what actually makes the card appear: `col` is
	// the rung preview_available is computed from, and fanToLadder
	// stamps the thumbhash from the same frame, so the blur-up and the
	// picture it resolves into are the same image.
	if err := h.writePosterVariants(jobCtx, p.AssetID, p.FileHash, pick.img, p.Force); err != nil {
		return nil, fmt.Errorf("preview.video.poster: ladder: %w", err)
	}

	result.WorkS = time.Since(started).Seconds()
	logAttrs(h.Logger, jobCtx, slog.LevelDebug, "preview.video.poster.done",
		slog.String("asset_id", p.AssetID.String()),
		slog.Float64("at_s", result.AtS),
		slog.Float64("mean_luma", result.MeanLuma),
		slog.Float64("work_s", result.WorkS))
	return json.Marshal(result)
}

// Compile-time assertion.
var _ jobs.Handler = (*VideoPosterHandler)(nil)
