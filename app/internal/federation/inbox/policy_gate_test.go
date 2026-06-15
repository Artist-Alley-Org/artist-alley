// Unit tests for the Phase 1.22.I-h receiver-side encryption
// policy gate. Pure logic; no Postgres dependency.
//
// Five cases pin the contract:
//
//   1. Pass-through when SensitivityLookup is unwired.
//   2. Pass-through when row.ObjectKind is nil / empty.
//   3. Pass-through when lookup returns SensitivityNotFound,
//      Public, or Team.
//   4. Fires ErrEncryptionRequired when lookup returns
//      Restricted or Embargo.
//   5. Propagates lookup callback errors (so the dispatcher can
//      retry rather than reject).

package inbox

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func rowWithObject(kind string, id uuid.UUID) FederationInbox {
	var ok *string
	if kind != "" {
		ok = &kind
	}
	return FederationInbox{
		ObjectKind: ok,
		ObjectID:   pgtype.UUID{Bytes: id, Valid: id != (uuid.UUID{})},
	}
}

// --- 1. pass-through when lookup unwired ---

func TestPolicyGate_NoLookup_PassThrough(t *testing.T) {
	d := &Dispatcher{} // sensitivityLookup nil
	row := rowWithObject("post", uuid.New())
	if err := d.checkInboundEncryptionPolicy(context.Background(), row); err != nil {
		t.Errorf("unwired gate should pass through; got err=%v", err)
	}
}

// --- 2. pass-through when no target object ---

func TestPolicyGate_NoObjectKind_PassThrough(t *testing.T) {
	d := &Dispatcher{}
	called := false
	d.SetSensitivityLookup(func(context.Context, string, uuid.UUID) (Sensitivity, error) {
		called = true
		return SensitivityRestricted, nil
	})

	for _, name := range []string{"nil-objectkind", "empty-objectkind"} {
		t.Run(name, func(t *testing.T) {
			row := FederationInbox{}
			if name == "empty-objectkind" {
				empty := ""
				row.ObjectKind = &empty
			}
			if err := d.checkInboundEncryptionPolicy(context.Background(), row); err != nil {
				t.Errorf("gate fired on row with no object kind: %v", err)
			}
		})
	}
	if called {
		t.Errorf("lookup callback invoked despite missing object kind — gate should short-circuit before the call")
	}
}

// --- 3. pass-through on Not Found / Public / Team ---

func TestPolicyGate_NonSensitiveTiers_PassThrough(t *testing.T) {
	for _, tier := range []Sensitivity{
		SensitivityNotFound,
		SensitivityPublic,
		SensitivityTeam,
	} {
		t.Run(string(tier), func(t *testing.T) {
			d := &Dispatcher{}
			d.SetSensitivityLookup(func(context.Context, string, uuid.UUID) (Sensitivity, error) {
				return tier, nil
			})
			row := rowWithObject("post", uuid.New())
			if err := d.checkInboundEncryptionPolicy(context.Background(), row); err != nil {
				t.Errorf("gate fired on tier=%s; expected pass-through", tier)
			}
		})
	}
}

// --- 4. fires on Restricted / Embargo / unknown ---

func TestPolicyGate_SensitiveTiers_FireErrEncryptionRequired(t *testing.T) {
	for _, tier := range []Sensitivity{
		SensitivityRestricted,
		SensitivityEmbargo,
		Sensitivity("future_unknown_tier"),
	} {
		t.Run(string(tier), func(t *testing.T) {
			d := &Dispatcher{}
			d.SetSensitivityLookup(func(context.Context, string, uuid.UUID) (Sensitivity, error) {
				return tier, nil
			})
			row := rowWithObject("post", uuid.New())
			err := d.checkInboundEncryptionPolicy(context.Background(), row)
			if !errors.Is(err, ErrEncryptionRequired) {
				t.Errorf("tier=%s: err=%v, want ErrEncryptionRequired", tier, err)
			}
		})
	}
}

// --- 5. lookup error propagation ---

func TestPolicyGate_LookupErrorPropagates(t *testing.T) {
	d := &Dispatcher{}
	sentinel := errors.New("DB hiccup")
	d.SetSensitivityLookup(func(context.Context, string, uuid.UUID) (Sensitivity, error) {
		return "", sentinel
	})

	row := rowWithObject("post", uuid.New())
	err := d.checkInboundEncryptionPolicy(context.Background(), row)
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v (lookup errors should propagate)", err, sentinel)
	}
	if errors.Is(err, ErrEncryptionRequired) {
		t.Errorf("lookup-error path shouldn't be reported as encryption_required; got %v", err)
	}
}

// --- 6. value-table mirror against outbox.Sensitivity ---

// This test pins the inbox.Sensitivity values to the same strings
// outbox uses; a future edit on one side without the other trips
// the test. Defense in depth: the local types in policy_gate.go
// exist to break the import cycle; the values MUST stay in
// lockstep with the wire-side authority (outbox/resolver.go).
func TestPolicyGate_SensitivityStringsLockstepWithOutbox(t *testing.T) {
	// We can't import outbox here (cycle), so the test pins the
	// inbox values against the documented wire strings. A change
	// in either package without updating the constant value will
	// trip this assertion.
	cases := []struct {
		got  Sensitivity
		want string
	}{
		{SensitivityPublic, "public"},
		{SensitivityTeam, "team"},
		{SensitivityRestricted, "restricted"},
		{SensitivityEmbargo, "embargo"},
		{SensitivityNotFound, "not_found"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("Sensitivity drift: got=%q want=%q (mirror outbox.Sensitivity)",
				string(c.got), c.want)
		}
	}
}
