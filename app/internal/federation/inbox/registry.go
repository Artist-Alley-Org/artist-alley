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
// A profile-gated type (profileGated) is applied only for a peer whose
// stored intersection holds the profile; for any other peer it is
// unsupported exactly like a type without a handler. aa:Share and
// aa:Unshare (and its alias aa:RevokeShare) are applied only under
// share-lifecycle-1 (#1571): without it there is no ordering the
// receiver can trust, so no grant is created, ended or reordered.
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
	"slices"
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
//
// Undo is applied only as an Undo of a Like (#1572): the verb is keyed
// here, and federation.ValidateMembers answers any other Undo (of a
// Follow, a Block, a Create, ...) unsupported_activity at receipt and
// again after decryption, before a handler runs. An Undo without
// object_type is invalid_member. Nothing about an Undo of another type
// is ever processed.
//
// Delete and Update are applied only as a Delete or an edit of a comment
// (#1572), the same way: object_type "Note" is required, and a Delete or
// an Update of a post, an asset, a profile or anything else is
// unsupported_activity.
var appliedHandlers = map[federation.ActivityType]func(*Dispatcher) HandlerFn{
	federation.ActivityLike:      LikeHandler,
	federation.ActivityUndo:      UndoHandler,
	federation.ActivityDelete:    DeleteHandler,
	federation.ActivityUpdate:    UpdateHandler,
	federation.ActivityCreate:    CommentHandler,
	federation.ActivityAAShare:   ShareHandler,
	federation.ActivityAAUnshare: UnshareHandler,
}

// profileGated maps an applied type to the profile its sender must have
// negotiated with this receiver. The verb is the normalized one.
var profileGated = map[federation.ActivityType]string{
	federation.ActivityAAShare:   federation.ProfileShareLifecycle1,
	federation.ActivityAAUnshare: federation.ProfileShareLifecycle1,
}

// profileAllows reports whether a peer with these negotiated
// capabilities meets t's profile requirement (true for an ungated type).
func profileAllows(t federation.ActivityType, capabilities []string) bool {
	p, gated := profileGated[normalizeVerb(t)]
	return !gated || slices.Contains(capabilities, p)
}

// normalizeVerb applies the §12.5 #3 alias before any lookup:
// aa:RevokeShare is handled as aa:Unshare.
func normalizeVerb(t federation.ActivityType) federation.ActivityType {
	if t == federation.ActivityAARevokeShare {
		return federation.ActivityAAUnshare
	}
	return t
}

// Applies reports whether this release applies the activity type for a
// peer with these negotiated capabilities: it has a real domain handler
// and, for a profile-gated type, the peer negotiated the profile. A
// false answer means the envelope is rejected with
// InboxStatusUnsupportedActivity and never recorded as processed.
func Applies(t federation.ActivityType, capabilities []string) bool {
	_, ok := appliedHandlers[normalizeVerb(t)]
	return ok && profileAllows(t, capabilities)
}

// AppliedActivityTypes returns every type this release can apply,
// sorted, profile-gated ones included (see ProfileGatedActivityTypes).
// For tests and for keeping docs/spec/federation/v1.md (Implementation
// status D3) honest.
func AppliedActivityTypes() []federation.ActivityType {
	out := make([]federation.ActivityType, 0, len(appliedHandlers))
	for t := range appliedHandlers {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ProfileGatedActivityTypes maps each profile-gated applied type to the
// profile it needs.
func ProfileGatedActivityTypes() map[federation.ActivityType]string {
	out := make(map[federation.ActivityType]string, len(profileGated))
	for t, p := range profileGated {
		out[t] = p
	}
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
