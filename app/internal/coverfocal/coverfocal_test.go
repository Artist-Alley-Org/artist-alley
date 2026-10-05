// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package coverfocal

import "testing"

func f(v float64) *float64 { return &v }

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		x, y    *float64
		clear   bool
		refused bool
	}{
		{"absent", nil, nil, false, false},
		{"absent with clear", nil, nil, true, false},
		{"a full pair", f(0.25), f(0.75), false, false},
		{"the corners", f(0), f(1), false, false},
		{"x without y", f(0.5), nil, false, true},
		{"y without x", nil, f(0.5), false, true},
		{"a pair alongside clear", f(0.5), f(0.5), true, true},
		{"x above 1", f(1.5), f(0.5), false, true},
		{"y below 0", f(0.5), f(-0.1), false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := Validate("cover_focal_x", "cover_focal_y", "clear_cover_focal", c.x, c.y, c.clear)
			if (msg != "") != c.refused {
				t.Errorf("refused=%v (%q), want refused=%v", msg != "", msg, c.refused)
			}
		})
	}
}
