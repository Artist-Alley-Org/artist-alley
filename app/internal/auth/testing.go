// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import "github.com/google/uuid"

// SetIdentityScopedCapForTest installs a team-scoped capability grant
// on the given Identity. Exclusively for use from external test packages
// (workflow_test, etc.) that need to construct a synthetic Identity
// without paying the cost of a real DB grant + resolver pass.
//
// This helper lives in non-_test.go because Go's package boundary
// only exposes _test.go symbols to other tests in the SAME package;
// external _test packages (e.g. workflow_test) can't see them. A
// regular .go file is the only way to let auth_test- and
// workflow_test-residents share the helper while keeping the
// Identity.scopedCaps field unexported.
func SetIdentityScopedCapForTest(id *Identity, code string, team uuid.UUID) {
	if id == nil || code == "" {
		return
	}
	if id.scopedCaps == nil {
		id.scopedCaps = make(map[string]map[uuid.UUID]struct{})
	}
	set, ok := id.scopedCaps[code]
	if !ok {
		set = make(map[uuid.UUID]struct{})
		id.scopedCaps[code] = set
	}
	set[team] = struct{}{}
}
