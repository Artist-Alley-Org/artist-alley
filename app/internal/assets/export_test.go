// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import "github.com/google/uuid"

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
