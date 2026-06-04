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
// of valid values for each field is enforced by the frontend and
// pinned by the openapi schema, NOT by a DB CHECK constraint — the
// list will grow (e.g. new browse layouts in 1.18) and the DB
// shouldn't have to migrate every time the UI ships a new option.
type DefaultViews struct {
	HomeTab       string `json:"home_tab,omitempty"`       // "" | "following" | "latest" | "trending" | "for_you"
	BrowseLayout  string `json:"browse_layout,omitempty"`  // "" | "grid" | "masonry" | "thumbnail" | "list"
	BrowseSort    string `json:"browse_sort,omitempty"`    // "" | "newest" | "oldest" | "popular" | "trending"
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
// channel names, and known events with channel lists containing the
// same channel twice. Returns the first violation found — callers
// surface it to the user verbatim.
func ValidatePreferences(p Preferences) error {
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

// UnmarshalPreferencesRow parses a DB row's two JSONB columns back
// into the typed struct. Either column being malformed (which would
// only happen if the DB was tampered with directly) surfaces as a
// loud error rather than a silently-zeroed value.
func UnmarshalPreferencesRow(channelsJSON, viewsJSON []byte) (Preferences, error) {
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
	}
	return p, nil
}
