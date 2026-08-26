// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package fixturesweep

// ArgsForTest exposes argsFor to the external test package. The binding
// rule it encodes is only observable against a real server, so the test
// that covers it lives beside the other integration tests.
func ArgsForTest(r Rule, sql string, cat Catalogue) []any { return argsFor(r, sql, cat) }
