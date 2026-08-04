// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Canonical outbound help links.
//
// Centralised because they had already drifted: three pages under
// /admin/help still pointed at `github.com/mscrnt/artist-alley`, which
// only works because GitHub redirects after the move to the
// Artist-Alley-Org org. One constant per destination so the next move
// is one edit.

export const DOCS_URL = 'https://artist-alley.org';
export const REPO_URL = 'https://github.com/Artist-Alley-Org/artist-alley';
export const ISSUES_URL = `${REPO_URL}/issues`;
export const RELEASES_URL = `${REPO_URL}/releases`;
export const CHANGELOG_URL = `${REPO_URL}/blob/main/CHANGELOG.md`;
