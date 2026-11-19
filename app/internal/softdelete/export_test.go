// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package softdelete

import "github.com/google/uuid"

// SetPurgeAfterSelectHook runs f in HardDeletePastAssets after the
// candidate SELECT and before the purge transaction begins. Tests only
// (#1596).
func (s *Service) SetPurgeAfterSelectHook(f func(candidates []uuid.UUID)) {
	s.purgeHooks.afterSelect = f
}

// SetPurgeBeforeCommitHook runs f in HardDeletePastAssets after the
// DELETE and before its commit. Tests only (#1596).
func (s *Service) SetPurgeBeforeCommitHook(f func()) { s.purgeHooks.beforeCommit = f }
