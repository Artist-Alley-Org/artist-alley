// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package mention

import (
	"context"
	"log/slog"

	"github.com/mscrnt/artist-alley/app/internal/notifications"
)

// Notifier is the slice of notifications.Writer this package needs,
// kept as a local interface so the mention package doesn't hard-depend
// on the concrete Writer (and so tests can assert the fired inputs).
// The boot wiring injects an adapter over *notifications.Writer.
type Notifier interface {
	Notify(ctx context.Context, recipient int64, actor *int64, verb, targetKind, targetID string, payload map[string]any) error
}

// Service is the single hook the write handlers call after a post or
// comment insert commits. It parses the body, resolves local mentions,
// and fires a mention_of_me notification per resolved ref. Best-effort:
// a notify failure is logged, never returned — the caller's content is
// already persisted and a missed notification must not fail the write.
type Service struct {
	resolver *Resolver
	notifier Notifier
	logger   *slog.Logger
}

// NewService wires the parse+resolve+notify pipeline. notifier may be
// nil (tests / a boot phase before the Writer exists) — a nil notifier
// makes Process a parse-and-resolve no-op on the fire step.
func NewService(resolver *Resolver, notifier Notifier, logger *slog.Logger) *Service {
	return &Service{resolver: resolver, notifier: notifier, logger: logger}
}

// Process parses text for @mentions, resolves them to local refs, and
// fires a mention_of_me notification for each. actorRef is the author
// (the notifications.Writer drops the row when a resolved ref equals
// the actor, so self-mentions never notify). targetKind/targetID point
// at the deep-link surface — for both post-body and comment-body
// mentions this is the containing post, so the bell routes to
// /posts/{id}. payload carries per-verb context (e.g. an excerpt).
//
// Call this AFTER the insert transaction commits. Never returns an
// error — failures are logged and swallowed.
func (s *Service) Process(ctx context.Context, actorRef int64, text, targetKind, targetID string, payload map[string]any) {
	if s == nil || s.resolver == nil {
		return
	}
	mentions := ParseMentions(text)
	if len(mentions) == 0 {
		return
	}
	refs := s.resolver.ResolveLocal(ctx, mentions)
	if len(refs) == 0 {
		return
	}
	actor := actorRef
	for _, ref := range refs {
		if s.notifier == nil {
			continue
		}
		if err := s.notifier.Notify(ctx, ref, &actor, notifications.VerbMentionOfMe, targetKind, targetID, payload); err != nil {
			if s.logger != nil {
				s.logger.LogAttrs(ctx, slog.LevelWarn, "mention.notify_failed",
					slog.Int64("recipient", ref),
					slog.Int64("actor", actorRef),
					slog.String("target_id", targetID),
					slog.String("err", err.Error()))
			}
		}
	}
}

// ProcessForPost is the convenience the posts + comments handlers call.
// Both surfaces deep-link to the containing post, so the target is
// always (TargetKindPost, postID) — a comment-body mention routes the
// bell to /posts/{id} just like a post-body one. Keeps the handlers
// from importing the notifications package for the target-kind const.
func (s *Service) ProcessForPost(ctx context.Context, actorRef int64, text, postID string, payload map[string]any) {
	s.Process(ctx, actorRef, text, notifications.TargetKindPost, postID, payload)
}
