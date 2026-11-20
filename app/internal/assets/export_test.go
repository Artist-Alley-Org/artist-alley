// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// SetReconcileAfterPromoteHook runs f in Reconcile after every promotion
// and before any asset's transaction. Tests only (#1596).
func (r *VersionReconciler) SetReconcileAfterPromoteHook(f func(hash string)) {
	r.hooks.afterPromote = f
}

// SetReconcileBeforeAssetTxHook runs f before each asset's transaction; a
// non-nil error fails the reconcile there. Tests only (#1596).
func (r *VersionReconciler) SetReconcileBeforeAssetTxHook(f func(assetID uuid.UUID) error) {
	r.hooks.beforeAssetTx = f
}

// SetReplaceBeforeCommitHook runs f in CreateAssetVersion after every
// write and before the commit; a non-nil error rolls it back. Tests only
// (#1596).
func (h *Handler) SetReplaceBeforeCommitHook(f func(ctx context.Context, tx pgx.Tx) error) {
	h.replaceHooks.beforeCommit = f
}

// SetCacheRegistryForTest gives the handler the cache registry the
// production wire passes to NewHandler, so a test can watch the caches a
// replacement invalidates (#1596). Tests only.
func (h *Handler) SetCacheRegistryForTest(r *cache.Registry) { h.registry = r }
