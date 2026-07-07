package search

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
	a.c.RecordEvent(ResultSavedSearchCoordinatorTick)
}

func (a savedSearchCounterAdapter) RecordRunResult(result string) {
	if a.c == nil {
		return
	}
	switch result {
	case "hit":
		a.c.RecordEvent(ResultSavedSearchRunHit)
	case "empty":
		a.c.RecordEvent(ResultSavedSearchRunEmpty)
	case "disabled":
		a.c.RecordEvent(ResultSavedSearchRunDisabled)
	default:
		a.c.RecordEvent(ResultSavedSearchRunError)
	}
}

func (a savedSearchCounterAdapter) RecordNotificationSent() {
	if a.c == nil {
		return
	}
	a.c.RecordEvent(ResultSavedSearchNotificationSent)
}

func (a savedSearchCounterAdapter) RecordDeltaHit() {
	if a.c == nil {
		return
	}
	a.c.RecordEvent(ResultSavedSearchDeltaHit)
}

