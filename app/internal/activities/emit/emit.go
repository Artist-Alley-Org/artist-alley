// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package emit holds the typed per-activity-type helpers that
// build [activities.Input] for the writer's `WithEmission` /
// `WithEmissionFn` dispatch helpers.
//
// One file per activity type. Each file's exported builder
// function takes typed inputs (an ActorContext + a typed
// reference to the affected object) and returns an [Emission]
// containing the Activity to record + any per-recipient
// notifications to fire after commit.
//
// The convention is intentional: handlers should never construct
// raw [activities.Input] by hand. The emit helpers are the
// single chokepoint where every activity's:
//
//   - URI shape (per [docs/spec/federation/v1.md] §8) is enforced
//   - addressing fields (to/cc per AP §6.1, with our walled-garden
//     interpretation per ADR 0043 §"Trust model") are correct
//   - notification fan-out rules (which side-effects fire on this
//     activity type) live in one place
//
// Adding a new activity type means: a new file here + a new
// const in [federation.ActivityType] + a CHECK constraint update
// in the migration. Per ADR 0042 §4, all three change in one PR.
package emit

import (
	"strconv"

	"github.com/mscrnt/artist-alley/app/internal/activities"
)

// ActorContext bundles the per-call identity of the local actor.
// Handlers build this once from the caller's auth.Identity +
// the configured instance baseURL.
type ActorContext struct {
	UserRef  int64
	Username string
	BaseURL  string // e.g. "https://studio-a.example"
}

// URI returns the actor's canonical handle per
// docs/spec/federation/v1.md §8.3 — `{baseURL}/users/{username}`.
func (a ActorContext) URI() string {
	return a.BaseURL + "/users/" + a.Username
}

// MintActivityURI is a convenience pass-through to the writer's
// URI minter; centralises the call site so emit helpers don't
// import the writer package's top-level symbols directly.
func (a ActorContext) MintActivityURI() string {
	return activities.MintActivityURI(a.BaseURL)
}

// ObjectURI builds the canonical URI for a local object given
// its kind + local ID. Per docs/spec/federation/v1.md §8.2:
// `{baseURL}/{kind}/{localID}`.
//
// Note: §8.2 limits {kind} to a closed set (`posts`, `assets`,
// `collections`, `workspaces`, `brand_kits`); other object kinds
// here (comment, message, user) use different prefixes. This
// helper routes accordingly so emit helpers don't have to
// remember the per-kind URL shape.
func (a ActorContext) ObjectURI(kind activities.ActivityObjectKind, localID string) string {
	switch kind {
	case activities.ObjectKindPost:
		return a.BaseURL + "/posts/" + localID
	case activities.ObjectKindAsset:
		return a.BaseURL + "/assets/" + localID
	case activities.ObjectKindCollection:
		return a.BaseURL + "/collections/" + localID
	case activities.ObjectKindWorkspace:
		return a.BaseURL + "/workspaces/" + localID
	case activities.ObjectKindBrandKit:
		return a.BaseURL + "/brand_kits/" + localID
	case activities.ObjectKindComment:
		// Comments aren't in §8.2's first-class set but they need
		// canonical URIs for federation. Use /comments/{uuid}.
		return a.BaseURL + "/comments/" + localID
	case activities.ObjectKindMessage:
		// DMs use /messages/{uuid}. The activity wraps a Note
		// addressed to the recipient; this is the Note's own URI.
		return a.BaseURL + "/messages/" + localID
	case activities.ObjectKindUser:
		// Users are actors — return the actor URI. Caller usually
		// has the username already; we accept a numeric ref here
		// as a fallback.
		if _, err := strconv.ParseInt(localID, 10, 64); err == nil {
			return a.BaseURL + "/users/by-ref/" + localID
		}
		return a.BaseURL + "/users/" + localID
	case activities.ObjectKindActivity:
		// An Undo's target activity — already-canonical URI.
		// Callers pass the full activity_uri as localID here.
		return localID
	}
	// Unknown kind: best-effort.
	return a.BaseURL + "/" + string(kind) + "/" + localID
}

// NotificationFanout is one per-recipient notification that fires
// AFTER the activity row commits. The writer's WithEmission helper
// dispatches these via the notifier interface; the recipient's
// channel preferences gate actual delivery (in_app vs email).
//
// Best-effort: notification dispatch errors are logged but never
// propagate to the handler — a notifications-table problem must
// not block the social action it accompanies.
type NotificationFanout struct {
	// Recipient is the local user_ref to notify.
	Recipient int64

	// Verb is one of userprefs.KnownEventTypes. The notification
	// writer maps verb → channel preferences per recipient.
	Verb string

	// Target identifies the entity the notification is "about"
	// (used by the frontend renderer to route the click). Same
	// shape as activities.ObjectRef but lives here so the emit
	// helpers don't need to construct a separate type.
	TargetKind string
	TargetID   string

	// Payload carries per-verb extra data — e.g. post_title,
	// comment excerpt. The frontend inbox card renders from this
	// without N+1 fetches.
	Payload map[string]any
}

// Emission is the return type of every emit helper: the Activity
// to record (in the same tx as the domain write) and zero or more
// notifications to fire after commit.
//
// Notifications may be empty (e.g. Block doesn't notify the
// blocked actor per AP §6.9; UpdatePost doesn't notify anyone by
// default).
type Emission struct {
	Activity      activities.Input
	Notifications []NotificationFanout
}
