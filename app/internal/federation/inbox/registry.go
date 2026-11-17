// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The set of activity types this release applies, and the per-verb
// handler registry built from it (#1569).
//
// # Fail honestly
//
// An inbound activity is either applied by a real domain handler or
// rejected with InboxStatusUnsupportedActivity. There is no third
// outcome: no verb is recorded as processed without a domain write.
// Before #1569, every §7 type other than Like and Create was wired to
// a no-op stub that returned OutcomeProcessed, and the dispatcher
// treated a missing handler the same way, so a peer's Undo, Delete,
// aa:Share or aa:AssetVersion was acknowledged and silently dropped.
//
// appliedHandlers is the single source of truth. Applies (the
// receive-time gate in PostInbox and PostInboxBatch) and BuildRegistry
// (the dispatcher's verb map) both read it, so the two cannot drift.
// Landing a real handler means adding ONE entry here; nothing else
// flips a type from unsupported to applied.
//
// When aa:Add lands (#882), it MUST apply the same asset gate the HTTP
// path applies (collections.Handler.mayCollectAsset): the ROW plane
// (visibility.CanSee, EntityAsset) AND the CONTENT plane
// (visibility.CanReadContent, ADR 0064), with "unreadable" and
// "nonexistent" producing the SAME outcome. A remote actor must not be
// able to collect what a local one cannot, and the inbox is the easier
// surface to forget.

package inbox

import (
	"log/slog"
	"sort"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// appliedHandlers maps each activity type this release applies to the
// constructor of its real domain handler. Every other type, including
// a type added to federation.KnownActivityTypes later, is unsupported
// by default.
//
// Create is applied only as a comment: CommentHandler rejects any other
// Create with invalid_type at dispatch time.
var appliedHandlers = map[federation.ActivityType]func(*Dispatcher) HandlerFn{
	federation.ActivityLike:   LikeHandler,
	federation.ActivityCreate: CommentHandler,
}

// normalizeVerb applies the §12.5 #3 alias before any lookup:
// aa:RevokeShare is handled as aa:Unshare.
func normalizeVerb(t federation.ActivityType) federation.ActivityType {
	if t == federation.ActivityAARevokeShare {
		return federation.ActivityAAUnshare
	}
	return t
}

// Applies reports whether this release has a real domain handler for
// the activity type. A false answer means the envelope is rejected with
// InboxStatusUnsupportedActivity and never recorded as processed.
func Applies(t federation.ActivityType) bool {
	_, ok := appliedHandlers[normalizeVerb(t)]
	return ok
}

// AppliedActivityTypes returns the applied set, sorted. For tests and
// for keeping docs/spec/federation/v1.md (Implementation status D3)
// honest.
func AppliedActivityTypes() []federation.ActivityType {
	out := make([]federation.ActivityType, 0, len(appliedHandlers))
	for t := range appliedHandlers {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// BuildRegistry returns the per-verb handler map: exactly one real
// handler per applied type and nothing else. Boot passes the result to
// Dispatcher.SetRegistry; tests can call it and override entries.
//
// The dispatcher applies the aa:RevokeShare → aa:Unshare normalization
// before registry lookup, so no alias entries are needed.
func BuildRegistry(d *Dispatcher, _ *slog.Logger) map[federation.ActivityType]HandlerFn {
	reg := make(map[federation.ActivityType]HandlerFn, len(appliedHandlers))
	for t, ctor := range appliedHandlers {
		reg[t] = ctor(d)
	}
	return reg
}
