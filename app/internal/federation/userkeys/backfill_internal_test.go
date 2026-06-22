// Internal-package tests for the [isRaceLoserError] predicate.
// Kept in the same package as backfill.go (rather than
// userkeys_test) so the predicate can be exercised directly
// without exporting it — this is implementation detail.
//
// Pairs with [TestBackfillMissingKeys_OrphanRetiredKey_NotCountedAsError]
// in backfill_test.go: that one exercises the unique-violation
// path end-to-end against the live DB; this one covers BOTH the
// unique- AND FK-violation classification at the unit level so
// the FK case doesn't require race-prone end-to-end scaffolding.

package userkeys

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsRaceLoserError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "23505 unique violation — classified",
			err:  &pgconn.PgError{Code: sqlStateUniqueViolation, Message: "duplicate key"},
			want: true,
		},
		{
			name: "23503 FK violation — classified",
			err:  &pgconn.PgError{Code: sqlStateForeignKeyViolation, Message: "fk violation"},
			want: true,
		},
		{
			name: "23505 wrapped through fmt.Errorf — still classified",
			err:  fmt.Errorf("ensure: %w", &pgconn.PgError{Code: sqlStateUniqueViolation}),
			want: true,
		},
		{
			name: "23503 wrapped through fmt.Errorf — still classified",
			err:  fmt.Errorf("ensure: %w", &pgconn.PgError{Code: sqlStateForeignKeyViolation}),
			want: true,
		},
		{
			name: "23505 wrapped twice (matches the EnsureCurrentForUser → backfillOne shape)",
			err:  fmt.Errorf("ensure: %w", fmt.Errorf("userkeys: insert: %w", &pgconn.PgError{Code: sqlStateUniqueViolation})),
			want: true,
		},
		{
			name: "different SQLSTATE — NOT classified (e.g. serialization failure)",
			err:  &pgconn.PgError{Code: "40001"},
			want: false,
		},
		{
			name: "non-pg error — NOT classified",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "nil — NOT classified",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRaceLoserError(tt.err); got != tt.want {
				t.Errorf("isRaceLoserError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
