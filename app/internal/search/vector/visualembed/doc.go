// Package visualembed implements the async upload-hook CLIP visual-
// embedding job (Phase 1.16.B-3-followup-2, closes #201).
//
// Companion to the operator-triggered backfill in
// app/internal/search/vector/visualbackfill (PR #205):
//
//   - Backfill catches existing image assets that predate the sidecar.
//   - Auto-embed (this package) catches NEW image uploads on the way
//     in so operators don't have to re-trigger backfill after every
//     ingest batch.
//
// Shape mirrors the 1.14.B ai.embed job (see app/internal/ai/jobs):
//
//   - Dispatch is a fire-and-forget enqueue on the assets/handler.go
//     CreateAsset fanout, POST-asset-commit. Enqueue failure warn-logs
//     but never fails the upload.
//   - Retry is delegated to the jobs framework via EnqueueOpts.MaxAttempts
//     (there is NO in-handler retry loop — mirrors ai.embed exactly).
//     Permanent errors are wrapped in *jobs.TerminalError so the worker
//     stops attempting; transient errors return plain error and the
//     framework re-queues per max_attempts.
//   - Guards at dispatch: provider registered → sysconfig auto_embed
//     enabled → asset extension is an image. Cheapest checks first;
//     non-image uploads (the common case for mixed corpora) skip without
//     touching the jobs table.
//
// Divergence from 1.14.B ai.embed:
//
//   - Process-shared rate limiter (x/time/rate) so a bulk upload can't
//     saturate the CPU sidecar. Rate is a distinct sysconfig knob from
//     backfill's rate limit — auto-embed is user-facing and prioritized
//     higher than the background sweep.
//   - Dedicated Counter with visual_embed_auto_* result classes surfaced
//     on /admin/search/health so operators can distinguish upload-hook
//     traffic from backfill traffic.
//
// Zero federation state — visual embeddings remain per-instance
// (same posture as backfill + the underlying asset_visual_embedding table).
package visualembed
