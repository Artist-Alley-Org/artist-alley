package storage

import "testing"

// Lives in the same package (no _test suffix) so it can hit the
// unexported parseSingleRange.
func TestParseSingleRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in         string
		wantOffset int64
		wantLength int64
		wantOK     bool
	}{
		{"", 0, 0, false},
		{"items=0-99", 0, 0, false},
		{"bytes=", 0, 0, false},
		{"bytes=0-99", 0, 100, true},
		{"bytes=100-199", 100, 100, true},
		{"bytes=0-0", 0, 1, true},
		{"bytes=500-", 500, 0, true},
		{"bytes=-500", 0, 0, false},
		{"bytes=0-99,200-299", 0, 0, false},
		{"bytes=99-0", 0, 0, false},
		{"bytes=abc-99", 0, 0, false},
		{"bytes=0-abc", 0, 0, false},
		{"bytes=-1", 0, 0, false},
	}
	for _, tc := range cases {
		off, length, ok := parseSingleRange(tc.in)
		if ok != tc.wantOK || off != tc.wantOffset || length != tc.wantLength {
			t.Errorf("parseSingleRange(%q) = (%d, %d, %v); want (%d, %d, %v)",
				tc.in, off, length, ok, tc.wantOffset, tc.wantLength, tc.wantOK)
		}
	}
}
