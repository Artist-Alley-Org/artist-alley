// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visualembed

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualprovider"
)

// Dispatcher is the assets/handler.go-side surface. Constructed at
// boot alongside the Job handler; the CreateAsset fanout calls
// Dispatch after the ai.embed dispatch so both embed subsystems fan
// out from the same insertion point.
type Dispatcher struct {
	Jobs    *jobs.Service
	Logger  *slog.Logger
	Counter *Counter

	// Provider is the same provider handle the boot-time bootstrap
	// registered. When nil the sidecar isn't wired; Dispatch skips
	// silently (mirrors the by-image handler's stub-when-unregistered
	// semantics).
	Provider visualprovider.Provider

	// EnabledGetter reads the sysconfig auto_embed_on_upload knob
	// per-dispatch so a runtime toggle takes effect on the next
	// upload without a restart. Returns false + non-nil error → skip
	// (fail-safe: don't dispatch when we can't confirm the operator
	// wants it).
	EnabledGetter func(ctx context.Context) (bool, error)

	// MaxAttempts is the total attempts the jobs framework runs
	// before marking the job as failed. Seeded from sysconfig
	// search.visual.auto_embed_retry_count via
	// (1 + retryCount). Zero falls back to the framework default (3).
	MaxAttempts int
}

// DispatchInput carries the minimal asset facts Dispatch needs. The
// caller (assets/handler.go) already has these in scope from the
// upload commit path — passing them in avoids a re-query for the same
// row we just wrote.
type DispatchInput struct {
	AssetID       uuid.UUID
	FileExtension string
}

// Dispatch enqueues a visualembed job when all guards pass. Silent
// skip on any guard failure — non-image uploads are the common case
// for mixed corpora and shouldn't produce warn logs. Enqueue failure
// (rare — Postgres write hiccup) warn-logs but does not surface an
// error: like ai.embed dispatch, upload succeeds regardless.
//
// Guard order (cheapest first per pre-audit + design decision #2):
//  1. Provider registered
//  2. Sysconfig auto_embed enabled
//  3. Asset is an image (extension sniff)
func (d *Dispatcher) Dispatch(ctx context.Context, in DispatchInput) {
	// Nil receiver → silent no-op (defence in depth; callers shouldn't
	// need to nil-check the seam).
	if d == nil {
		return
	}
	if d.Provider == nil {
		d.Counter.RecordSkipped()
		return
	}
	if d.EnabledGetter != nil {
		enabled, err := d.EnabledGetter(ctx)
		if err != nil || !enabled {
			d.Counter.RecordSkipped()
			return
		}
	}
	if !IsImageExtension(in.FileExtension) {
		d.Counter.RecordSkipped()
		return
	}
	// Nil Jobs is a boot-wire error (dispatcher constructed without
	// its jobs.Service). Skip counter fires so operators see it on
	// /admin/search/health; warn-log so it's investigatable.
	if d.Jobs == nil {
		if d.Logger != nil {
			d.Logger.LogAttrs(ctx, slog.LevelWarn, "visualembed.dispatch.jobs_nil",
				slog.String("asset_id", in.AssetID.String()))
		}
		d.Counter.RecordSkipped()
		return
	}

	payload := Payload{AssetID: in.AssetID}
	opts := jobs.EnqueueOpts{
		IdempotencyKey: idempotencyKey(in.AssetID),
	}
	if d.MaxAttempts > 0 {
		opts.MaxAttempts = &d.MaxAttempts
	}
	if _, err := d.Jobs.Enqueue(ctx, JobTypeVisualEmbed, payload, opts); err != nil {
		if d.Logger != nil {
			d.Logger.LogAttrs(ctx, slog.LevelWarn, "visualembed.dispatch.enqueue_failed",
				slog.String("asset_id", in.AssetID.String()),
				slog.String("err", err.Error()))
		}
		// Skip counter: the guards passed but the framework couldn't
		// accept the job. Distinct from provider/sysconfig skip, but
		// reusing the counter keeps the /admin/search/health surface
		// simple + the operator can spot enqueue-failure spikes via
		// jobs-framework observability separately.
		d.Counter.RecordSkipped()
	}
}

// idempotencyKey is deterministic per-asset so a retry of the upload
// path (e.g. federation replay) doesn't double-enqueue.
func idempotencyKey(assetID uuid.UUID) string {
	return "search.visual_embed|" + assetID.String()
}

// IsImageExtension reports whether the file extension is one the
// CLIP visual sidecar can process. Extensions match the has_image
// classification on the assets table. Exported so tests can pin the
// set + so the dispatch guard is verifiable by grep.
//
// The set intentionally overlaps 1.18.A-2's isExifExtractableImageExt
// but stays independent — a future extension may add HEIC / AVIF
// support to Pillow before EXIF extraction picks them up.
func IsImageExtension(ext string) bool {
	trimmed := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	switch trimmed {
	case "jpg", "jpeg", "png", "webp", "gif", "bmp", "tif", "tiff":
		return true
	}
	return false
}
