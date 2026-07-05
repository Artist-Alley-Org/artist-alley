package visualbackfill

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRow_IsActive(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		row  Row
		want bool
	}{
		{
			name: "no terminal timestamps → active",
			row:  Row{},
			want: true,
		},
		{
			name: "completed → inactive",
			row:  Row{CompletedAt: &now},
			want: false,
		},
		{
			name: "cancelled → inactive",
			row:  Row{CancelledAt: &now},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.IsActive(); got != tc.want {
				t.Fatalf("IsActive() = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestScope_JSONRoundtrip(t *testing.T) {
	in := Scope{Kind: ScopeAll}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Scope
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Kind != in.Kind {
		t.Fatalf("kind: got %q want %q", out.Kind, in.Kind)
	}
}

func TestRowToJSON_Shape(t *testing.T) {
	// Import the http.go rowToJSON via a fabricated Row + check the
	// JSON shape the admin UI depends on.
	id := uuid.New()
	completed := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	userRef := int64(42)
	total := int64(100)
	errMsg := "boom"
	row := Row{
		ID:               id,
		StartedAt:        time.Date(2026, 7, 5, 11, 30, 0, 0, time.UTC),
		CompletedAt:      &completed,
		Scope:            Scope{Kind: ScopeAll},
		TotalEstimated:   &total,
		Processed:        50,
		Succeeded:        49,
		Failed:           1,
		StartedByUserRef: &userRef,
		LastError:        &errMsg,
	}
	out := rowToJSON(row)
	if out["id"] != id.String() {
		t.Fatalf("id: got %v", out["id"])
	}
	if out["processed"] != int64(50) {
		t.Fatalf("processed: got %v", out["processed"])
	}
	if out["is_active"] != false {
		t.Fatalf("is_active: got %v", out["is_active"])
	}
	if out["total_estimated"] != total {
		t.Fatalf("total_estimated: got %v", out["total_estimated"])
	}
	if out["last_error"] != errMsg {
		t.Fatalf("last_error: got %v", out["last_error"])
	}
	if _, ok := out["cancelled_at"]; ok {
		t.Fatalf("cancelled_at should be omitted when nil, got %v", out["cancelled_at"])
	}
}
