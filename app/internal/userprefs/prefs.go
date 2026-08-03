// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package userprefs holds per-user application-behavior preferences
// (Phase 1.17.G, feat/user-surfaces).
//
// Sibling to the existing user_profiles surface — user_profiles owns
// the public-facing identity (display name, bio, language, theme),
// userprefs owns the private knobs the user toggles to control how
// the app behaves for them (notification channels, default views,
// and — in later sub-phases — follow filters and read-cursor state).
//
// Schema is JSONB-on-disk for forward compatibility: G2 (follows),
// I2 (notifications), I (DMs), L (resource_requests) will each add
// new event types or default-view selectors without a schema
// migration. The typed Go structs below are the only place we
// enumerate the valid keys, so client + server stay in agreement
// via the openapi schema rather than via DB constraints.

package userprefs

import (
	"encoding/json"
	"fmt"
)

// Preferences is the typed shape of a user_preferences row's JSON
// content. Mirrors the openapi UserPreferences schema field-for-
// field so the wire shape and the persisted shape are the same.
type Preferences struct {
	NotificationChannels NotificationChannels `json:"notification_channels"`
	DefaultViews         DefaultViews         `json:"default_views"`
	// EmailCadence maps each EventType to the email delivery cadence
	// (Phase 1.55.Y). Absent key = CadenceImmediate (send-now — the
	// pre-1.55.Y behaviour, so existing users are never surprised).
	// "off" is NOT a cadence value: muting email means dropping "email"
	// from NotificationChannels for that topic; cadence only refines
	// *when* email fires when it IS enabled.
	EmailCadence EmailCadences `json:"email_cadence"`
}

// EmailCadences maps an EventType → cadence ("immediate" | "hourly" |
// "daily" | "weekly"). Absent = immediate.
type EmailCadences map[string]string

// Email-cadence values. immediate = enqueue the email now (default);
// hourly/daily/weekly = queue into digest_queue for the coordinator to
// batch. There is deliberately no "off" — off is the absence of the
// "email" channel in NotificationChannels for that topic.
const (
	CadenceImmediate = "immediate"
	CadenceHourly    = "hourly"
	CadenceDaily     = "daily"
	CadenceWeekly    = "weekly"
)

// KnownCadences is the validation set. Used by ValidatePreferences to
// reject unknown values.
var KnownCadences = []string{CadenceImmediate, CadenceHourly, CadenceDaily, CadenceWeekly}

// CadenceFor returns the email cadence for a verb, defaulting to
// immediate when unset — preserving send-now for every topic a user
// hasn't explicitly moved onto a digest.
func (p Preferences) CadenceFor(verb string) string {
	if c, ok := p.EmailCadence[verb]; ok && c != "" {
		return c
	}
	return CadenceImmediate
}

// NotificationChannels maps each EventType to the list of channels
// the user wants notified through. An absent key means "use the
// system default for this event type" — NOT "no notification" — so
// new event types added in later sub-phases don't silently disable
// for existing users.
//
// Use the wide string-keyed type rather than `map[EventType][]Channel`
// because callers serialize through openapi codegen which represents
// JSON object keys as plain strings; converting back and forth at
// every boundary costs more than the type safety would save.
type NotificationChannels map[string][]string

// DefaultViews captures the per-user view selections that the
// frontend would otherwise have to guess from cookies or first-visit
// heuristics. Empty string = fall back to per-route default. The set
// of valid values for each field is enforced HERE and pinned by the
// openapi schema, NOT by a DB CHECK constraint — the list will grow
// (e.g. new browse layouts in 1.18) and the DB shouldn't have to
// migrate every time the UI ships a new option.
//
// Every member of every set below names a state the app can actually
// reach. That is the whole lesson of #736: the vocabulary shipped with
// `trending` and `for_you` on HomeTab and `popular`/`trending` on
// BrowseSort, none of which had a query behind them, so choosing one
// stored a durable preference for a screen that does not exist and
// the user silently got the default. A value belongs here only once
// something serves it.
type DefaultViews struct {
	HomeTab      string `json:"home_tab,omitempty"`      // "" | "latest" | "following"
	BrowseLayout string `json:"browse_layout,omitempty"` // "" | "grid" | "masonry" | "thumbnail" | "list" | "feed"
	BrowseSort   string `json:"browse_sort,omitempty"`   // "" | "newest" | "oldest"
}

// The closed value sets for the three view knobs. Each mirrors a
// vocabulary that already exists somewhere concrete:
//
//	KnownHomeTabs       ← the `feed` enum on GET /posts
//	                      (app/api/openapi.yaml, /posts:)
//	KnownBrowseLayouts  ← ViewMode in web/src/lib/stores/browseView.svelte.ts
//	KnownBrowseSorts    ← the two orderings the client can produce
//
// `feed` is in KnownBrowseLayouts and was never in the pref
// vocabulary, which is the mirror-image defect to the phantom tabs: a
// mode a phone lands on by default but no user could ask for.
//
// KnownBrowseSorts stops at two on purpose. GET /posts takes no
// ordering parameter of any kind, so `newest` / `oldest` are the
// client reversing what it holds; `popular` and `trending` would need
// a ranking model chosen first, and a guessed one is worse than none.
var (
	KnownHomeTabs      = []string{"latest", "following"}
	KnownBrowseLayouts = []string{"grid", "masonry", "thumbnail", "list", "feed"}
	KnownBrowseSorts   = []string{"newest", "oldest"}
)

func inSet(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// Sanitized drops any selection this build no longer serves, leaving
// the field empty so the caller falls back to its built-in default.
//
// This is the READ-side counterpart to the write-side rejection in
// ValidatePreferences, and it is what makes shrinking a vocabulary
// safe. A row saved before #706/#736 may hold `trending`, `for_you`
// or `popular`; GET /account/preferences must return something the
// preferences page can render and the browse store can act on, and
// "no selection" is the only honest answer for a value nothing can
// serve. It must never be an error — a stale string in a JSONB blob
// is not a reason to 500 a preferences read, and a user locked out of
// the page that would let them fix the value has no way out.
//
// Same shape as readFilter() in browseView.svelte.ts, for the same
// reason and against the same removed values.
func (v DefaultViews) Sanitized() DefaultViews {
	if !inSet(v.HomeTab, KnownHomeTabs) {
		v.HomeTab = ""
	}
	if !inSet(v.BrowseLayout, KnownBrowseLayouts) {
		v.BrowseLayout = ""
	}
	if !inSet(v.BrowseSort, KnownBrowseSorts) {
		v.BrowseSort = ""
	}
	return v
}

// EventType enumerates the notification trigger events the UI lets
// users toggle channels for. The constants below are the canonical
// set — adding a new event is a code change (here + the i18n catalog
// + the writer that emits it), never a schema migration.
//
// Each constant's comment names the sub-phase that ships the writer.
// Until that sub-phase lands, the toggle in the UI is dormant: it
// persists fine, no event fires against it.
const (
	// Phase 1.13 (shipped) — wires up once we add the writer in I2.
	EventCommentOnMyPost  = "comment_on_my_post"
	EventLikeOnMyPost     = "like_on_my_post"
	EventReplyToMyComment = "reply_to_my_comment"

	// Phase 1.17.G2 — follows ship in the next sub-phase.
	EventNewFollower   = "new_follower"
	EventFollowedPosts = "post_from_followed_user"

	// Phase 1.17.I2 — notifications subsystem itself.
	EventMentionOfMe = "mention_of_me"

	// Phase 1.17.I — DMs.
	EventDirectMessageReceived = "direct_message_received"
	EventBroadcastReceived     = "broadcast_received"

	// Phase 1.17.L — resource access requests.
	EventResourceRequestApproved = "resource_request_approved"
	EventResourceRequestDenied   = "resource_request_denied"
	EventResourceRequestReceived = "resource_request_received_to_approve"

	// Phase 1.17.O (shipped) — license-bridge already in place; the
	// writer lands when the notifications subsystem (I2) does, since
	// the channel today is "admin UI red banner" and we want a real
	// notification row instead.
	EventLicenseExpiringSoon = "license_expiring_soon"
	EventLicenseExpired      = "license_expired"
)

// Channel enumerates the delivery channels a user can pick per event.
// in_app + email are the V1 set; SMS / push / webhook are future
// additions that drop in as new constants without breaking existing
// channel lists.
const (
	ChannelInApp = "in_app"
	ChannelEmail = "email"
)

// KnownEventTypes is the canonical ordered list of events. Returned
// from GET /account/preferences so the UI can render channel toggles
// for every event the build knows about — even ones with no rows in
// the user's prefs JSONB yet. Order is the rendering order in the
// admin UI, grouped roughly by social → personal → system.
var KnownEventTypes = []string{
	EventCommentOnMyPost,
	EventLikeOnMyPost,
	EventReplyToMyComment,
	EventMentionOfMe,
	EventNewFollower,
	EventFollowedPosts,
	EventDirectMessageReceived,
	EventBroadcastReceived,
	EventResourceRequestReceived,
	EventResourceRequestApproved,
	EventResourceRequestDenied,
	EventLicenseExpiringSoon,
	EventLicenseExpired,
}

// KnownChannels is the canonical set for the channel-list values.
// Used by ValidatePreferences to reject unknown channel names rather
// than persisting them silently.
var KnownChannels = []string{
	ChannelInApp,
	ChannelEmail,
}

// SystemDefaultChannels is the per-event-type fallback applied when
// a user's NotificationChannels map omits the key. Conservative
// defaults: everything is in_app, only "you got a DM" and "license
// expired" also email by default. Users can opt in to email for the
// rest via the prefs UI.
func SystemDefaultChannels(event string) []string {
	switch event {
	case EventDirectMessageReceived, EventLicenseExpired:
		return []string{ChannelInApp, ChannelEmail}
	default:
		return []string{ChannelInApp}
	}
}

// ChannelsFor returns the resolved channel list for an event,
// falling back to system defaults when the user hasn't set anything.
// This is the helper notification-writers (in I2 and beyond) call
// before deciding whether to insert an in_app row + queue an email.
func (p *Preferences) ChannelsFor(event string) []string {
	if p == nil {
		return SystemDefaultChannels(event)
	}
	chs, ok := p.NotificationChannels[event]
	if !ok {
		return SystemDefaultChannels(event)
	}
	// An explicit empty slice IS meaningful — "I want no
	// notifications for this event" — so we return it as-is rather
	// than reaching for defaults again. Use a nil check (not
	// len(chs) == 0) to distinguish "unset" from "explicit none".
	return chs
}

// ValidatePreferences rejects values the client shouldn't be able
// to persist: unknown event types in the channels map, unknown
// channel names, known events with channel lists containing the same
// channel twice, and default-view selections outside the closed sets
// above. Returns the first violation found — callers surface it to
// the user verbatim.
//
// The view knobs are validated on WRITE and sanitized on READ, not
// one or the other. Rejecting on write is what stops a new phantom
// value entering the store; sanitizing on read is what keeps the rows
// already holding one readable. Only doing the first would 500 every
// preferences GET for a user who set `trending` before #736.
func ValidatePreferences(p Preferences) error {
	if v := p.DefaultViews.HomeTab; v != "" && !inSet(v, KnownHomeTabs) {
		return fmt.Errorf("unknown home_tab %q", v)
	}
	if v := p.DefaultViews.BrowseLayout; v != "" && !inSet(v, KnownBrowseLayouts) {
		return fmt.Errorf("unknown browse_layout %q", v)
	}
	if v := p.DefaultViews.BrowseSort; v != "" && !inSet(v, KnownBrowseSorts) {
		return fmt.Errorf("unknown browse_sort %q", v)
	}
	known := make(map[string]bool, len(KnownEventTypes))
	for _, e := range KnownEventTypes {
		known[e] = true
	}
	knownCh := make(map[string]bool, len(KnownChannels))
	for _, c := range KnownChannels {
		knownCh[c] = true
	}
	for event, chs := range p.NotificationChannels {
		if !known[event] {
			return fmt.Errorf("unknown event type %q", event)
		}
		seen := make(map[string]bool, len(chs))
		for _, ch := range chs {
			if !knownCh[ch] {
				return fmt.Errorf("unknown channel %q for event %q", ch, event)
			}
			if seen[ch] {
				return fmt.Errorf("duplicate channel %q for event %q", ch, event)
			}
			seen[ch] = true
		}
	}
	knownCad := make(map[string]bool, len(KnownCadences))
	for _, c := range KnownCadences {
		knownCad[c] = true
	}
	for event, cad := range p.EmailCadence {
		if !known[event] {
			return fmt.Errorf("unknown event type %q in email_cadence", event)
		}
		if !knownCad[cad] {
			return fmt.Errorf("unknown email cadence %q for event %q", cad, event)
		}
	}
	return nil
}

// MarshalNotificationChannels and MarshalDefaultViews are tiny
// wrappers used by the handler to produce the []byte JSONB payload
// for the sqlc-generated upsert. Keeping them here means the package
// owns the on-disk shape end-to-end and the handler stays a thin
// HTTP adapter.
func MarshalNotificationChannels(c NotificationChannels) ([]byte, error) {
	if c == nil {
		c = NotificationChannels{}
	}
	return json.Marshal(c)
}

func MarshalDefaultViews(v DefaultViews) ([]byte, error) {
	return json.Marshal(v)
}

// MarshalEmailCadence produces the email_cadence JSONB payload.
func MarshalEmailCadence(c EmailCadences) ([]byte, error) {
	if c == nil {
		c = EmailCadences{}
	}
	return json.Marshal(c)
}

// UnmarshalPreferencesRow parses a DB row's three JSONB columns back
// into the typed struct. A malformed column (only possible via direct
// DB tampering) surfaces as a loud error rather than a silently-zeroed
// value.
func UnmarshalPreferencesRow(channelsJSON, viewsJSON, cadenceJSON []byte) (Preferences, error) {
	var p Preferences
	if len(channelsJSON) > 0 {
		if err := json.Unmarshal(channelsJSON, &p.NotificationChannels); err != nil {
			return Preferences{}, fmt.Errorf("notification_channels: %w", err)
		}
	}
	if p.NotificationChannels == nil {
		p.NotificationChannels = NotificationChannels{}
	}
	if len(viewsJSON) > 0 {
		if err := json.Unmarshal(viewsJSON, &p.DefaultViews); err != nil {
			return Preferences{}, fmt.Errorf("default_views: %w", err)
		}
		// Every read goes through here, so this is the one place a
		// value removed from a vocabulary can be neutralised. See
		// DefaultViews.Sanitized.
		p.DefaultViews = p.DefaultViews.Sanitized()
	}
	if len(cadenceJSON) > 0 {
		if err := json.Unmarshal(cadenceJSON, &p.EmailCadence); err != nil {
			return Preferences{}, fmt.Errorf("email_cadence: %w", err)
		}
	}
	if p.EmailCadence == nil {
		p.EmailCadence = EmailCadences{}
	}
	return p, nil
}
