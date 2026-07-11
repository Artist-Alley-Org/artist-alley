// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
		a.c.RecordEvent(ResultSearchFeedbackUp)
	case "down":
		a.c.RecordEvent(ResultSearchFeedbackDown)
	}
}

func (a feedbackCounterAdapter) RecordFeedbackUndo() {
	if a.c == nil {
		return
	}
	a.c.RecordEvent(ResultSearchFeedbackUndo)
}

func (a feedbackCounterAdapter) RecordFeedbackRateLimited() {
	if a.c == nil {
		return
	}
	a.c.RecordEvent(ResultSearchFeedbackRateLimit)
}

func (a feedbackCounterAdapter) RecordFeedbackDisabled() {
	if a.c == nil {
		return
	}
	a.c.RecordEvent(ResultSearchFeedbackDisabled)
}
