// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package feedback

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// stubConfig returns a fixed Config.
type stubConfig struct{ cfg Config }

func (s stubConfig) Get(context.Context) (Config, error) { return s.cfg, nil }

// stubVisibility answers CanSee with fixed results.
type stubVisibility struct {
	canSee bool
	err    error
}

func (s stubVisibility) CanSee(context.Context, int64, uuid.UUID) (bool, error) {
	return s.canSee, s.err
}

// countingCounter records invocations for assertions.
type countingCounter struct {
	feedback      int
	undo          int
	rateLimited   int
	disabled      int
	lastDirection string
}

func (c *countingCounter) RecordFeedback(direction string) {
	c.feedback++
	c.lastDirection = direction
}
func (c *countingCounter) RecordFeedbackUndo()        { c.undo++ }
func (c *countingCounter) RecordFeedbackRateLimited() { c.rateLimited++ }
func (c *countingCounter) RecordFeedbackDisabled()    { c.disabled++ }

// stubStore is a Service test seam. Since Service consumes concrete
// *Store, we can't fully substitute — but we can exercise the
// error-path branches by injecting a Store whose Pool is nil (which
// would panic on the DB call). For unit-level branch coverage we lean
// on the disabled + visibility gates that short-circuit BEFORE the DB
// call, and defer DB-touching tests to ./scripts/test.sh integration
// runs.

func TestService_Submit_Disabled_ReturnsErrDisabled(t *testing.T) {
	cnt := &countingCounter{}
	svc := &Service{
		Cfg:     stubConfig{cfg: Config{Enabled: false, MaxPerUserPerDay: 60, AggregationWindowDays: 7}},
		Counter: cnt,
	}
	_, err := svc.Submit(context.Background(), SubmitParams{
		UserRef:     1,
		DSL:         "cat",
		HitAssetID:  uuid.New(),
		HitPosition: 1,
		Direction:   DirectionUp,
	})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
	if cnt.disabled != 1 {
		t.Fatalf("disabled counter: got %d, want 1", cnt.disabled)
	}
}

func TestService_Submit_HitNotVisible_ReturnsErrHitNotVisible(t *testing.T) {
	svc := &Service{
		Cfg:        stubConfig{cfg: Config{Enabled: true, MaxPerUserPerDay: 60, AggregationWindowDays: 7}},
		Visibility: stubVisibility{canSee: false},
		Counter:    &countingCounter{},
	}
	_, err := svc.Submit(context.Background(), SubmitParams{
		UserRef:     1,
		DSL:         "cat",
		HitAssetID:  uuid.New(),
		HitPosition: 1,
		Direction:   DirectionUp,
	})
	if !errors.Is(err, ErrHitNotVisible) {
		t.Fatalf("expected ErrHitNotVisible, got %v", err)
	}
}

func TestService_Submit_VisibilityError_Propagates(t *testing.T) {
	sentinel := errors.New("db down")
	svc := &Service{
		Cfg:        stubConfig{cfg: Config{Enabled: true, MaxPerUserPerDay: 60, AggregationWindowDays: 7}},
		Visibility: stubVisibility{err: sentinel},
	}
	_, err := svc.Submit(context.Background(), SubmitParams{
		UserRef:     1,
		DSL:         "cat",
		HitAssetID:  uuid.New(),
		HitPosition: 1,
		Direction:   DirectionUp,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestService_DeleteOwn_Disabled_ReturnsErrDisabled(t *testing.T) {
	cnt := &countingCounter{}
	svc := &Service{
		Cfg:     stubConfig{cfg: Config{Enabled: false, MaxPerUserPerDay: 60, AggregationWindowDays: 7}},
		Counter: cnt,
	}
	err := svc.DeleteOwn(context.Background(), uuid.New(), 1)
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
	if cnt.disabled != 1 {
		t.Fatalf("disabled counter: got %d, want 1", cnt.disabled)
	}
}

func TestValidDirection(t *testing.T) {
	if !ValidDirection("up") || !ValidDirection("down") {
		t.Fatal("valid values rejected")
	}
	if ValidDirection("") || ValidDirection("sideways") || ValidDirection("UP") {
		t.Fatal("invalid values accepted")
	}
}

func TestConfig_AggregationWindow(t *testing.T) {
	c := Config{AggregationWindowDays: 7}
	got := c.AggregationWindow().Hours()
	if got != 24*7 {
		t.Fatalf("aggregation window: got %g hours, want %g", got, float64(24*7))
	}
}
