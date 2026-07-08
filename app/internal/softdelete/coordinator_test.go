package softdelete

import (
	"testing"
	"time"
)

// TestNextWakeHour covers the tick-scheduling math the gc
// coordinator uses to hand off from tick N to tick N+1. Load-
// bearing because a wrong "next wake" pushes the whole retention
// window: a stuck-at-yesterday next-tick would run the same tick
// forever (never advancing); a stuck-at-tomorrow-plus-24h next-tick
// would silently double the effective retention window.
func TestNextWakeHour(t *testing.T) {
	tests := []struct {
		name    string
		now     time.Time
		hourUTC int
		want    time.Time
	}{
		{
			name:    "before_today_hour_returns_today",
			now:     time.Date(2026, 7, 7, 3, 15, 0, 0, time.UTC),
			hourUTC: 5,
			want:    time.Date(2026, 7, 7, 5, 0, 0, 0, time.UTC),
		},
		{
			name:    "exactly_at_today_hour_returns_tomorrow",
			now:     time.Date(2026, 7, 7, 5, 0, 0, 0, time.UTC),
			hourUTC: 5,
			want:    time.Date(2026, 7, 8, 5, 0, 0, 0, time.UTC),
		},
		{
			name:    "after_today_hour_returns_tomorrow",
			now:     time.Date(2026, 7, 7, 6, 30, 0, 0, time.UTC),
			hourUTC: 5,
			want:    time.Date(2026, 7, 8, 5, 0, 0, 0, time.UTC),
		},
		{
			name:    "midnight_wake_before_midnight_returns_today_at_zero",
			now:     time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC).Add(-time.Nanosecond),
			hourUTC: 0,
			want:    time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC),
		},
		{
			name:    "midnight_wake_after_returns_tomorrow",
			now:     time.Date(2026, 7, 7, 1, 0, 0, 0, time.UTC),
			hourUTC: 0,
			want:    time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC),
		},
		{
			name:    "end_of_month_rolls_correctly",
			now:     time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC),
			hourUTC: 3,
			want:    time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nextWakeHour(tc.now, tc.hourUTC)
			if !got.Equal(tc.want) {
				t.Errorf("nextWakeHour(%v, %d) = %v; want %v", tc.now, tc.hourUTC, got, tc.want)
			}
		})
	}
}
