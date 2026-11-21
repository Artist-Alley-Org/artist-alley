// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetsend

// WithAfterRead is s with f run right after Tombstone reads the
// announcements (#1575): a test changes an announcement there, as a
// concurrent delivery would.
func WithAfterRead(s Source, f func()) Source {
	s.afterRead = f
	return s
}
