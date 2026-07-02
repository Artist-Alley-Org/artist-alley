package search

import "time"

// AsSavedSearchCounter returns an adapter that maps saved-search
// events into the shared search Counter. Nil-safe on the receiver
// so tests can construct a nil Counter and still pass through the
// coordinator wiring.
func (c *Counter) AsSavedSearchCounter() SavedSearchCounter {
	return savedSearchCounterAdapter{c: c}
}

// SavedSearchCounter is the narrow interface the saved-search
// coordinator + run handlers accept for observability. Mirrors
// saved.CoordinatorCounter without importing the saved package
// (search must not depend on saved).
type SavedSearchCounter interface {
	RecordCoordinatorTick(dueCount int)
	RecordRunResult(result string)
	RecordNotificationSent()
	RecordDeltaHit()
}

type savedSearchCounterAdapter struct{ c *Counter }

func (a savedSearchCounterAdapter) RecordCoordinatorTick(dueCount int) {
	if a.c == nil {
		return
	}
	a.c.Record(ResultSavedSearchCoordinatorTick, 0)
}

func (a savedSearchCounterAdapter) RecordRunResult(result string) {
	if a.c == nil {
		return
	}
	switch result {
	case "hit":
		a.c.Record(ResultSavedSearchRunHit, 0)
	case "empty":
		a.c.Record(ResultSavedSearchRunEmpty, 0)
	case "disabled":
		a.c.Record(ResultSavedSearchRunDisabled, 0)
	default:
		a.c.Record(ResultSavedSearchRunError, 0)
	}
}

func (a savedSearchCounterAdapter) RecordNotificationSent() {
	if a.c == nil {
		return
	}
	a.c.Record(ResultSavedSearchNotificationSent, 0)
}

func (a savedSearchCounterAdapter) RecordDeltaHit() {
	if a.c == nil {
		return
	}
	a.c.Record(ResultSavedSearchDeltaHit, 0)
}

// keep time imported for future latency histograms tracking
// saved-search execution.
var _ = time.Second
