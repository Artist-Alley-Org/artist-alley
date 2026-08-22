// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1238 — an (action, target) no executor can act on is refused WHEN IT
// IS SCHEDULED.
//
// # The class this closes
//
// `validTargets` accepted asset/post/collection/user for every verb.
// Three of the four executor arms then refused anything but an asset.
// So `delete` on a post enqueued cleanly, sat pending for however long
// the operator asked, and failed at a moment nobody was watching — on a
// target that was the whole reason somebody scheduled it. The row was
// well-formed and the failure was months away, which is the worst
// possible combination: the mistake is cheap to make and expensive to
// find.
//
// # Why these cases are shaped this way
//
// The rule of two: a guard that is right about one dimension of
// wrongness and wrong about the other passes a single-case test. So
// each shape is asserted separately — an unsupported ACTION on a
// supported target, a supported action on an unsupported TARGET, and
// both wrong at once — and each is asserted to name what it refused
// rather than merely to fail.
//
// And the guard is driven through Store.Schedule, not through the pair
// table directly. A table nothing consults is a table, not a guard.
//
// Skips without AA_DB_PASSWORD.

package scheduledactions

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// scheduleErr calls Schedule expecting a refusal, and asserts nothing
// was written. Rows-not-written is the load-bearing half: an error
// returned AFTER the insert would leave exactly the pending row this
// whole guard exists to prevent.
func scheduleErr(t *testing.T, s *Store, in ScheduleInput) error {
	t.Helper()
	before := pendingCount(t, s)
	row, err := s.Schedule(context.Background(), in)
	if err == nil {
		t.Cleanup(func() {
			_, _ = s.pool.Exec(context.Background(), `DELETE FROM scheduled_actions WHERE id=$1`, row.ID)
		})
		t.Fatalf("Schedule(%s, %s) was accepted; it must be refused at schedule time",
			in.Action, in.TargetKind)
	}
	if after := pendingCount(t, s); after != before {
		t.Errorf("a refused schedule still wrote a row (pending %d → %d)", before, after)
	}
	return err
}

func pendingCount(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*)::bigint FROM scheduled_actions`).Scan(&n); err != nil {
		t.Fatalf("count scheduled_actions: %v", err)
	}
	return n
}

// TestSchedule_RefusesPairsNoExecutorCanRun is the guard, at all three
// dimensions of wrongness.
func TestSchedule_RefusesPairsNoExecutorCanRun(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	someone := int64(4400001)

	for _, tc := range []struct {
		name   string
		in     ScheduleInput
		wantIn []string // substrings the message must carry
	}{
		{
			// The action is wrong for a target that IS supported by other
			// verbs — `post` is reachable by change_state and notify, and
			// `delete` is asset-only.
			name: "an unsupported ACTION on a supported target",
			in: ScheduleInput{
				Action: ActionDelete, TargetKind: TargetPost,
				TargetID: uuid.NewString(), ScheduledFor: at(time.Hour), CreatedBy: &someone,
			},
			wantIn: []string{`"delete"`, `"post"`, "asset"},
		},
		{
			// The action is fine and supports two targets; this is neither.
			name: "a supported action on an unsupported TARGET",
			in: ScheduleInput{
				Action: ActionChangeState, TargetKind: TargetCollection,
				TargetID: uuid.NewString(), ScheduledFor: at(time.Hour), CreatedBy: &someone,
			},
			wantIn: []string{`"change_state"`, `"collection"`, "asset, post"},
		},
		{
			// Both axes wrong at once: restrict is asset-only, and `user`
			// is reachable by notify alone.
			name: "both wrong at once",
			in: ScheduleInput{
				Action: ActionRestrict, TargetKind: TargetUser,
				TargetID: "4400009", ScheduledFor: at(time.Hour), CreatedBy: &someone,
			},
			wantIn: []string{`"restrict"`, `"user"`, "asset"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := scheduleErr(t, store, tc.in)
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q — "+
						"a caller cannot fix a pair the error will not identify", err, want)
				}
			}
		})
	}
}

// TestSchedule_OldBrokenShapeCanNoLongerEnqueueThenDie is acceptance 5
// stated as one case: the exact shape that used to enqueue cleanly and
// fail at fire time.
//
// change_state + post is now the PUBLICATION arm, so it is accepted —
// and the point of the assertion is that the two possible answers are
// "runs" and "refused", never "accepted and doomed". The complement is
// asserted beside it: the same verb on a target with no arm is refused.
func TestSchedule_OldBrokenShapeCanNoLongerEnqueueThenDie(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	someone := int64(4400001)

	a := schedule(t, store, ScheduleInput{
		Action: ActionChangeState, TargetKind: TargetPost,
		TargetID: uuid.NewString(), Params: map[string]any{"to_state": "published"},
		ScheduledFor: at(time.Hour), CreatedBy: &someone,
	})
	if a.State != StatePending {
		t.Errorf("state=%q, want pending", a.State)
	}
	if !executorTargets()[ActionChangeState][TargetPost] {
		t.Error("change_state accepts a post target but the pair table says no arm runs it — " +
			"that combination IS the enqueue-then-die bug")
	}

	// And the guard is not merely permissive: change_state on a target
	// with no arm at all is still refused.
	_ = scheduleErr(t, store, ScheduleInput{
		Action: ActionChangeState, TargetKind: TargetUser,
		TargetID: "4400009", ScheduledFor: at(time.Hour), CreatedBy: &someone,
	})
}

// TestSchedule_PostStateChangeNeedsAnActor: a scheduled publication is
// an act BY SOMEBODY. It runs through the endpoint's own body, where
// the gate is the actor's capability and the federation activity is
// emitted in the actor's name — so a row with no created_by has nobody
// to publish as, and that is knowable at schedule time.
func TestSchedule_PostStateChangeNeedsAnActor(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)

	err := scheduleErr(t, store, ScheduleInput{
		Action: ActionChangeState, TargetKind: TargetPost,
		TargetID: uuid.NewString(), Params: map[string]any{"to_state": "published"},
		ScheduledFor: at(time.Hour), // CreatedBy deliberately nil
	})
	if !strings.Contains(err.Error(), "created_by") {
		t.Errorf("refusal %q does not say what is missing", err)
	}

	// The system-scheduled ASSET arms are untouched by this: a retention
	// delete legitimately has nobody behind it (#931).
	a := schedule(t, store, ScheduleInput{
		Action: ActionDelete, TargetKind: TargetAsset,
		TargetID: uuid.NewString(), ScheduledFor: at(time.Hour),
	})
	if a.CreatedBy != nil {
		t.Error("a system-scheduled asset delete acquired a created_by")
	}
}

// TestExecutorTargets_MatchesTheArms pins the derivation itself.
//
// It is deliberately NOT a restatement of the table — a copy asserted
// against its original proves only that both were typed the same way.
// It asserts the two claims the table makes that are easy to get wrong
// and that the arms actually decide: that notify reaches every kind
// (notifyRecipient takes params.recipient for any target), and that
// every other verb is asset-only apart from change_state's post arm.
func TestExecutorTargets_MatchesTheArms(t *testing.T) {
	got := executorTargets()

	// notify is the ONLY reason collection and user stay in validTargets.
	for kind := range validTargets() {
		if !got[ActionNotify][kind] {
			t.Errorf("notify does not accept %q, but notifyRecipient serves any target "+
				"whose params name a recipient", kind)
		}
	}
	for kind := range validTargets() {
		reachable := false
		for a := range validActions() {
			reachable = reachable || got[a][kind]
		}
		if !reachable {
			t.Errorf("target kind %q is valid but no executor can act on it — "+
				"it is either reserved-and-unschedulable or it should leave validTargets", kind)
		}
	}

	// The mutating arms all start at assetTarget, except change_state's
	// post branch.
	for _, a := range []Action{ActionRestrict, ActionChangeSensitivity, ActionDelete} {
		if len(got[a]) != 1 || !got[a][TargetAsset] {
			t.Errorf("%s claims targets %v; its executor calls assetTarget first", a, got[a])
		}
	}
	if !got[ActionChangeState][TargetAsset] || !got[ActionChangeState][TargetPost] ||
		len(got[ActionChangeState]) != 2 {
		t.Errorf("change_state claims %v, want exactly {asset, post}", got[ActionChangeState])
	}
}
