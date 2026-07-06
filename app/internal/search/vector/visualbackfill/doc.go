// Package visualbackfill implements the operator-triggered visual-
// embedding backfill for CLIP visual search (Phase 1.16.B-3-followup-4,
// closes #200).
//
// Shape mirrors search/reindex (Phase 1.16.B-5):
//
//   - Store — hand-rolled SQL over search_visual_backfill_run
//     (partial UNIQUE INDEX enforces single-active-run invariant).
//   - Job  — coordinator that iterates image assets missing a
//     visual embedding, calls the sidecar-backed Provider per row,
//     writes the embedding via visualstore.UpsertAssetVisualEmbedding,
//     honours cancel probes at every batch boundary.
//   - Handler — 4 admin routes (trigger / list / get / cancel) gated
//     on the "system.admin" capability.
//
// Design differences vs reindex:
//
//   - No target/scope multiplexing. Visual backfill has one mode
//     (embed images) and the MVP scope is "all image assets missing
//     an embedding". A "scope=asset_type" filter can grow later; the
//     scope JSONB column matches reindex so the admin history table
//     renders consistently. Consolidation into a shared
//     BackfillRun table is tracked by #186.
//   - Coordinator loop (not per-asset job dispatch). Visual embed is
//     a single sidecar HTTP call; enqueueing per-asset jobs adds
//     machinery for no benefit. The async ai.visualembed job for
//     upload-time hooks is a separate follow-up (#159 area).
//   - Depends on visualprovider.Provider registration. Trigger 503s
//     when the sidecar isn't wired (search.visual.enabled=false OR
//     sidecar unreachable at boot) so operators diagnose the config
//     gap before enqueueing work that can't run.
package visualbackfill
