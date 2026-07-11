// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package reindex

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// SysconfigBackfillOnBoot is the sysconfig key that gates the
// boot-task backfill. Default true; operators disable via
// sysconfig for e.g. read-replica-only deployments.
const SysconfigBackfillOnBoot = "search.federation_backfill_on_boot"

// FederationBackfillOnBoot enqueues one reindex run scoped to
// federated-arrived assets that lack embeddings. Called once
// from Server.Run at startup.
//
// Guardrails:
//
//   - Skips silently if the reindex Store isn't wired (nil pool).
//   - Skips silently when there are zero missing rows — never
//     spins up an empty coordinator.
//   - Skips silently when a reindex run is already active
//     (StartParams returns ErrActiveRunExists → we're done).
//   - Logs a single "backfill_enqueued" event with the estimated
//     row count so operators upgrading from pre-B-5 see the
//     activity in the boot logs.
//
// Per pre-audit Q2: the federation-inbox commit-site DOESN'T exist
// today — federation only exchanges links/activities, not asset
// row inserts. This backfill covers whatever pre-B-5 assets were
// federated by an earlier ingest path that has since been removed;
// on a fresh install the missing-count is zero and the backfill
// is a no-op.
func FederationBackfillOnBoot(
	ctx context.Context,
	pool *pgxpool.Pool,
	store *Store,
	jobSvc *jobs.Service,
	logger *slog.Logger,
) error {
	if pool == nil || store == nil || jobSvc == nil {
		return nil
	}
	var missing int64
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM assets
		 WHERE deleted_at IS NULL
		   AND origin_server_id IS NOT NULL
		   AND id NOT IN (SELECT asset_id FROM asset_embedding_d768)
	`).Scan(&missing); err != nil {
		return fmt.Errorf("reindex.boot_backfill: probe: %w", err)
	}
	if missing == 0 {
		return nil
	}

	row, err := store.Start(ctx, StartParams{
		Scope:  Scope{Kind: ScopeFederationMissing},
		Target: TargetEmbedding,
	})
	if err != nil {
		if err == ErrActiveRunExists {
			// Someone else already kicked it off — nothing to do.
			return nil
		}
		return fmt.Errorf("reindex.boot_backfill: start: %w", err)
	}
	if _, err := jobSvc.Enqueue(ctx, JobTypeReindex, Payload{RunID: row.ID}, jobs.EnqueueOpts{}); err != nil {
		// Cancel-mark so the reindex doesn't stay wedged as
		// "active" — a re-run at next boot picks it up.
		_ = store.Complete(ctx, row.ID, "boot backfill enqueue: "+err.Error())
		return fmt.Errorf("reindex.boot_backfill: enqueue: %w", err)
	}
	if logger != nil {
		logger.LogAttrs(ctx, slog.LevelInfo, "reindex.boot_backfill.enqueued",
			slog.Int64("missing_count", missing),
			slog.String("run_id", row.ID.String()),
		)
	}
	return nil
}
