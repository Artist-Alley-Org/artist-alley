// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package presentation

import (
	"math"
	"testing"
)

// TestCanvasDims_Boundaries pins the pair rule at its edges (#196). The
// route-level cases in dimensions_test.go show the fallback reaching the
// manifest; this shows exactly where valid stops.
func TestCanvasDims_Boundaries(t *testing.T) {
	p := func(v float64) *float64 { return &v }
	cases := []struct {
		name   string
		w, h   *float64
		ok     bool
		ww, hh int
	}{
		{"both present", p(1080), p(1920), true, 1080, 1920},
		{"smallest", p(1), p(1), true, 1, 1},
		{"largest", p(maxCanvasDimension), p(maxCanvasDimension), true, maxCanvasDimension, maxCanvasDimension},
		{"one over the cap", p(maxCanvasDimension + 1), p(10), false, 0, 0},
		{"zero", p(0), p(10), false, 0, 0},
		{"negative", p(10), p(-1), false, 0, 0},
		{"fraction", p(10.25), p(10), false, 0, 0},
		{"NaN", p(math.NaN()), p(10), false, 0, 0},
		{"+Inf", p(10), p(math.Inf(1)), false, 0, 0},
		{"-Inf", p(math.Inf(-1)), p(10), false, 0, 0},
		{"width missing", nil, p(10), false, 0, 0},
		{"height missing", p(10), nil, false, 0, 0},
		{"both missing", nil, nil, false, 0, 0},
	}
	for _, tc := range cases {
		w, h, ok := canvasDims(tc.w, tc.h)
		if ok != tc.ok || w != tc.ww || h != tc.hh {
			t.Errorf("%s: got (%d, %d, %v), want (%d, %d, %v)", tc.name, w, h, ok, tc.ww, tc.hh, tc.ok)
		}
	}
}
