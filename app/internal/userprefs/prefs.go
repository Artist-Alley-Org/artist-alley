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

	"github.com/google/uuid"
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
	// FeedFilters are the content filters the browse feed subtracts with
	// (#891). Distinct from DefaultViews, which only rearranges the same
	// set.
	FeedFilters FeedFilters `json:"feed_filters"`
	// BrowseRail is the browse rail's per-user curation — team chips
	// (#1113) and followed-tag chips (#1123). Distinct from FeedFilters,
	// which subtracts CONTENT server-side; this curates NAVIGATION
	// FURNITURE and is applied by the client.
	BrowseRail BrowseRail `json:"browse_rail"`
}

// BrowseRail is the browse page's rail curation (#1113, widened by
// #1123).
//
// The rail lists every team the caller can see, plus the tags they
// follow. This is the reader's edit of that list: which chips they took
// out of it, and which ones they pulled to the front. Every member is a
// LIST, and the same naming contract the booleans in FeedFilters carry
// applies: the ZERO VALUE — a user with no preferences row, an empty
// `{}` blob, or a key this build has never heard of — is THE BUILD'S
// DEFAULT RAIL: every visible team, followed-first then name order,
// followed by every followed tag, most recent first.
//
// # One bag, because the rail is one piece of furniture
//
// The tag half joined this struct rather than getting a sibling because
// a `#fantasy` chip and a team chip are two rows of one strip, edited in
// one panel and saved in one write — not two concerns. Migration 00051
// carries the full argument, including why the column was renamed off
// `team_rail` rather than growing tag keys under a name that denies
// they exist.
//
// # It never reaches the feed
//
// Nothing here is ever consulted to decide which POSTS a caller sees.
// That is the difference from FeedFilters, which the posts handler
// reads, and it is a requirement rather than an accident: #1113 says
// hiding a team from your rail must not hide its posts from your feed,
// and #1123 says the same of a tag. So this struct has no server-side
// consumer at all — it is persisted, returned on /auth/me, and applied
// in the rail component. If a future change ever wants a server-side
// reader, that is a product decision to argue in an issue, not a
// convenience to reach for here.
//
// # Why the entries are not validated against `teams` / `tag_follows`
//
// A team can be deleted, or stop being visible to this caller, and a tag
// can be unfollowed, between the save and the next read. A 400 on "your
// own list mentions something that no longer exists" would strand the
// reader in a rail they cannot edit. Unknown entries are inert instead:
// the client intersects these lists with what the server returned, so a
// dead one is dropped at render. See ValidatePreferences for what IS
// enforced (shape and size).
type BrowseRail struct {
	// HiddenTeamIDs are teams the reader removed from their rail.
	// Empty = nothing hidden, which is the default rail.
	//
	// Named for what it holds rather than its inverse: a
	// `shown_team_ids` allow-list would make the empty zero value mean
	// "show everything", which is the opposite of what an empty
	// allow-list says, and the first partial write would blank the rail.
	HiddenTeamIDs []string `json:"hidden_team_ids,omitempty"`

	// TeamOrder is the reader's explicit ordering, applied to the
	// FOLLOWED group (the drag-reorder in the manage panel). Empty = the
	// server's order.
	//
	// PARTIAL LISTS ARE LEGAL and are the normal case: the ids named
	// here lead, in this order, and everything else keeps its previous
	// relative position behind them. That is what lets "drag one team to
	// the top" persist one id rather than a full snapshot the next
	// follow would immediately make stale.
	TeamOrder []string `json:"team_order,omitempty"`

	// HiddenTags are followed tags whose chip the reader removed from
	// their rail (#1123). Empty = nothing hidden.
	//
	// HIDING IS NOT UNFOLLOWING. Unfollowing deletes the `tag_follows`
	// row and changes what the Following feed contains; this only takes
	// the chip off the strip. The manage panel puts both verbs on every
	// row for exactly that reason.
	HiddenTags []string `json:"hidden_tags,omitempty"`

	// TagOrder is the reader's explicit ordering of their followed-tag
	// chips. Empty = the server's order (most recently followed first).
	// Partial lists are legal, exactly as TeamOrder.
	TagOrder []string `json:"tag_order,omitempty"`
}

// MaxBrowseRailIDs caps each list in BrowseRail.
//
// A cap exists because this blob is joined onto /auth/me — the call
// that gates the entire app — so an unbounded list is a session
// response an authenticated user can inflate at will. 1000 is far past
// any real instance's team count (the reference install has 11) and far
// below a payload anyone would notice.
const MaxBrowseRailIDs = 1000

// MaxRailTagLen bounds one entry in the tag lists, mirroring the CHECK
// constraint migration 00050 puts on `tag_follows.tag`.
//
// The UUID lists need no such bound — `uuid.Parse` is itself a length
// check — but a tag is free text, and 1000 entries of unbounded length
// is the same /auth/me inflation the count cap exists to prevent, just
// spent on width instead of depth. A tag longer than this cannot be
// followed, so a rail entry naming one could never have matched a chip.
const MaxRailTagLen = 200

// Sanitized drops entries a rail cannot use: blanks, duplicates,
// non-UUIDs in the id lists, over-long strings in the tag lists, and
// anything past the cap.
//
// Read-side counterpart to ValidatePreferences, and the same division
// of labour DefaultViews.Sanitized uses — reject a bad write, but never
// fail a read over a row that is already on disk. Unknown-but-wellformed
// entries are deliberately kept: see the type's note on why they are
// inert rather than invalid. UNPARSEABLE ones are not, and that is a
// different judgement rather than an inconsistency: the wire type
// declares the id lists `format: uuid`, so a non-UUID cannot arrive
// through the API at all and one on disk is tampering or a bug. Keeping
// it would break the projection back onto the wire; dropping it costs a
// reader nothing, because no team was ever identified by it.
//
// The tag lists are NOT UUID-parsed, which is why they get their own
// pass rather than sharing dedupeIDs. A tag is the string itself, so
// "wellformed" means only "non-empty and not absurdly long" — running
// them through the id sanitiser would have silently deleted every tag
// chip a reader ever curated.
func (r BrowseRail) Sanitized() BrowseRail {
	r.HiddenTeamIDs = dedupeIDs(r.HiddenTeamIDs)
	r.TeamOrder = dedupeIDs(r.TeamOrder)
	r.HiddenTags = dedupeRailTags(r.HiddenTags)
	r.TagOrder = dedupeRailTags(r.TagOrder)
	return r
}

// dedupeRailTags is dedupeIDs for free-text tag entries: same blank,
// duplicate and cap handling, with a length bound in place of the UUID
// parse. Entries are compared and stored verbatim — the corpus matches
// tags exactly (see tags.normalizeTag), so folding case here would make
// a reader's hidden chip stop matching the chip it hides.
func dedupeRailTags(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, tag := range in {
		if tag == "" || len(tag) > MaxRailTagLen || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
		if len(out) == MaxBrowseRailIDs {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func dedupeIDs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, id := range in {
		if id == "" || seen[id] {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == MaxBrowseRailIDs {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FeedFilters is the browse feed's per-user presentation set (#891,
// default inverted by #921).
//
// Every member is a BOOLEAN THAT DEFAULTS TO FALSE, and that is a
// contract rather than an accident: the zero value of this struct — what
// a user with no preferences row, an empty `{}` blob, or a key this
// build has never heard of all decode to — is THE BUILD'S DEFAULT FEED.
// Each key is therefore NAMED so that `false` is the default experience,
// not so that `false` is "no filtering". #921 is what made those two
// things different: hiding restricted work became the default, so the
// key that used to be `hide_restricted` is now `show_restricted` and the
// storage guarantee survives untouched — absent still means "whatever
// this build does by default".
//
// Renaming rather than flipping a default is the whole point. Leaving
// the key called `hide_restricted` and defaulting it to TRUE would have
// made an absent key mean the opposite of what the name asserts, which
// is precisely how the next reader gets it backwards.
//
// It can only SUBTRACT. Nothing here is ever consulted to decide whether
// a caller MAY read something — that is visibility.FieldsReadable's job
// and it has already run by the time these are applied (see
// posts.applyHideRestricted). A filter that could add a row would be a
// second expression of the read rule, which is the defect class epic
// #665 exists for. That is still true with the default inverted: the
// feed's UNFILTERED state is the set of rows the read rule returned, and
// `show_restricted` selects between "all of it" and "all of it minus the
// placeholders". It never reaches past the rule's output.
type FeedFilters struct {
	// ShowRestricted keeps the #883 placeholders in the browse feed.
	//
	// OFF by default, and off means the feed SUBTRACTS them: members the
	// caller cannot read are omitted rather than rendered as "you can't
	// see this", and a post whose members are ALL restricted drops out of
	// the page entirely — unless the caller wrote it, because your own
	// work does not disappear from your own feed over a display
	// preference.
	//
	// #921 inverted this. #891 shipped the machinery as an opt-in on the
	// theory that the placeholder is the more informative answer; a
	// measurement on the stock seed dataset said otherwise — a third of
	// one account's 82-post feed was entirely placeholders. The principle
	// the default now encodes: a placeholder belongs where the user asked
	// a question (a post opened by name) or opened a container (a
	// collection), not where they were handed a feed.
	//
	// Turning it ON restores the pre-#921 feed exactly, placeholders and
	// #913's "Request access" button included.
	ShowRestricted bool `json:"show_restricted,omitempty"`
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

	// #875 — someone granted you access to one of their posts.
	EventPostSharedWithMe = "post_shared_with_me"

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
	EventPostSharedWithMe,
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
	// The browse rail's four lists are checked for SIZE, not for
	// membership. An entry naming a team or tag that no longer exists is
	// inert at render (see BrowseRail); a list long enough to bloat every
	// /auth/me response is not, and this is the only place a client can
	// write one.
	//
	// The tag lists are checked for WIDTH as well as depth: they hold
	// free text rather than uuids, so 1000 legal-count entries can still
	// be an arbitrarily large payload. Refused rather than truncated,
	// because this is the write path and a silently shortened tag is a
	// chip that will never match the tag it names.
	for _, l := range []struct {
		name string
		ids  []string
	}{
		{"hidden_team_ids", p.BrowseRail.HiddenTeamIDs},
		{"team_order", p.BrowseRail.TeamOrder},
		{"hidden_tags", p.BrowseRail.HiddenTags},
		{"tag_order", p.BrowseRail.TagOrder},
	} {
		if n := len(l.ids); n > MaxBrowseRailIDs {
			return fmt.Errorf("%s holds %d entries, max %d", l.name, n, MaxBrowseRailIDs)
		}
	}
	for _, l := range []struct {
		name string
		tags []string
	}{
		{"hidden_tags", p.BrowseRail.HiddenTags},
		{"tag_order", p.BrowseRail.TagOrder},
	} {
		for _, tag := range l.tags {
			if len(tag) > MaxRailTagLen {
				return fmt.Errorf("%s holds a %d-character tag, max %d", l.name, len(tag), MaxRailTagLen)
			}
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

// MarshalFeedFilters produces the feed_filters JSONB payload. Every
// field is `omitempty`, so "every key at its zero value" persists as
// `{}` — the same bytes the column defaults to, which keeps a
// saved-but-untouched preference indistinguishable from a never-saved
// one. Since #921 that zero value is "the build's default feed" rather
// than "no filtering"; see FeedFilters for why the keys are named so
// those stay the same thing.
func MarshalFeedFilters(f FeedFilters) ([]byte, error) {
	return json.Marshal(f)
}

// MarshalBrowseRail produces the browse_rail JSONB payload (#1113,
// #1123). Every member is `omitempty`, so an untouched rail persists as
// `{}` — the same bytes the column defaults to, which keeps a
// saved-but-default preference indistinguishable from a never-saved
// one, exactly as MarshalFeedFilters does for the boolean bag.
func MarshalBrowseRail(r BrowseRail) ([]byte, error) {
	return json.Marshal(r.Sanitized())
}

// UnmarshalPreferencesRow parses a DB row's five JSONB columns back
// into the typed struct. A malformed column (only possible via direct
// DB tampering) surfaces as a loud error rather than a silently-zeroed
// value.
func UnmarshalPreferencesRow(channelsJSON, viewsJSON, cadenceJSON, filtersJSON, browseRailJSON []byte) (Preferences, error) {
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
	if len(filtersJSON) > 0 {
		if err := json.Unmarshal(filtersJSON, &p.FeedFilters); err != nil {
			return Preferences{}, fmt.Errorf("feed_filters: %w", err)
		}
	}
	if len(browseRailJSON) > 0 {
		if err := json.Unmarshal(browseRailJSON, &p.BrowseRail); err != nil {
			return Preferences{}, fmt.Errorf("browse_rail: %w", err)
		}
		// Same read-side neutralisation DefaultViews gets above: a row
		// on disk is never a reason to fail a preferences read.
		p.BrowseRail = p.BrowseRail.Sanitized()
	}
	return p, nil
}
