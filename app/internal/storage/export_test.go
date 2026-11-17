// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

// SetImportAfterLockHook runs f once ImportVerified holds the live-import
// lock, before it records its intent. Tests only.
func (s *Service) SetImportAfterLockHook(f func()) { s.importHooks.afterLock = f }
