package search

// AsFeedbackCounter returns an adapter that maps feedback events into
// the shared search Counter. Nil-safe on the receiver so tests can
// construct a nil Counter and still pass through the boot wiring
// (matches the AsSavedSearchCounter pattern).
func (c *Counter) AsFeedbackCounter() FeedbackCounter {
	return feedbackCounterAdapter{c: c}
}

// FeedbackCounter is the narrow interface the feedback subsystem
// consumes for observability. Mirrors feedback.Counter without
// importing the feedback package (search must not depend on
// search/feedback).
type FeedbackCounter interface {
	RecordFeedback(direction string) // "up" | "down"
	RecordFeedbackUndo()
	RecordFeedbackRateLimited()
	RecordFeedbackDisabled()
}

type feedbackCounterAdapter struct{ c *Counter }

func (a feedbackCounterAdapter) RecordFeedback(direction string) {
	if a.c == nil {
		return
	}
	switch direction {
	case "up":
		a.c.Record(ResultSearchFeedbackUp, 0)
	case "down":
		a.c.Record(ResultSearchFeedbackDown, 0)
	}
}

func (a feedbackCounterAdapter) RecordFeedbackUndo() {
	if a.c == nil {
		return
	}
	a.c.Record(ResultSearchFeedbackUndo, 0)
}

func (a feedbackCounterAdapter) RecordFeedbackRateLimited() {
	if a.c == nil {
		return
	}
	a.c.Record(ResultSearchFeedbackRateLimit, 0)
}

func (a feedbackCounterAdapter) RecordFeedbackDisabled() {
	if a.c == nil {
		return
	}
	a.c.Record(ResultSearchFeedbackDisabled, 0)
}
