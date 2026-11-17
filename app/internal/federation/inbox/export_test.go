// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package inbox

import (
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// RegisterAppliedForTest makes verb applied for the rest of tb, exactly
// as landing its real handler in appliedHandlers would (#1569). It lets
// a test prove that a formerly unsupported type flips to accepted and
// dispatched through the production receive and dispatch paths, without
// a real domain handler existing yet. Not safe for parallel tests;
// none of this package's tests run in parallel.
func RegisterAppliedForTest(tb testing.TB, verb federation.ActivityType, ctor func(*Dispatcher) HandlerFn) {
	tb.Helper()
	if _, exists := appliedHandlers[verb]; exists {
		tb.Fatalf("RegisterAppliedForTest: %q already has a real handler", verb)
	}
	appliedHandlers[verb] = ctor
	tb.Cleanup(func() { delete(appliedHandlers, verb) })
}
