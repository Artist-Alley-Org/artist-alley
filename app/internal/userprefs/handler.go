// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// HTTP handler for the per-user preferences surface
// (Phase 1.17.G, feat/user-surfaces).
//
// Endpoints (both require an authenticated session — no admin gate;
// any signed-in user reads + writes their OWN row):
//
//   GET   /account/preferences   → current Preferences + KnownEventTypes hint
//   PATCH /account/preferences   → upserts the supplied Preferences
//
// The PATCH semantics are deliberately full-object replacement,
// NOT field-level merge. The frontend always sends the entire
// resolved object (channels + views), so a "remove channel" toggle
// is a write that omits that channel from the array — there's no
// "delete a single key" verb. Keeps the wire contract small and
// the merge logic absent.

package userprefs

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// cacheDomainByUser is the cache-registry domain string for the
// per-user Preferences blob. Hot read: every notification writer in
// Phase 1.17.I2+ calls ChannelsFor per delivery decision, and most
// of those land on the same active user repeatedly within a single
// request burst (e.g. someone gets 10 likes in a minute). Without
// the cache that's 10 DB hits for a row that almost never changes.
// Federation note: writes Invalidate via the cache.Registry NOTIFY
// channel, so peer instances drop their stale copy too.
const cacheDomainByUser = "userprefs.by_user"

// Handler is the openapi-strict adapter. Wraps a pgxpool.Pool +
// logger; api.go's apiServer delegates to it.
type Handler struct {
	pool     *pgxpool.Pool
	logger   *slog.Logger
	registry *cache.Registry
	byUser   *cache.Cache[Preferences]
}

// NewHandler wires the handler to its dependencies. A nil registry
// is legal — handler falls back to direct DB reads (useful for the
// test path that doesn't spin up the cache LISTEN goroutine).
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{pool: pool, logger: logger, registry: registry}
	if registry != nil {
		// 5k entries fits ~1MB of resident memory at typical prefs
		// payload sizes (small JSONB) and easily covers the active-
		// users-in-last-30-days population the writers iterate over.
		h.byUser = cache.Register[Preferences](registry, cacheDomainByUser, 5_000)
	}
	return h
}

// userKey is the cache key for a per-user Preferences entry. Same
// shape every consumer uses (the int64 ref stringified) so the
// invalidator and the reader can't drift.
func userKey(ref int64) string { return strconv.FormatInt(ref, 10) }

// GetAccountPreferences — GET /account/preferences
//
// Returns the caller's resolved Preferences. When no row exists yet
// (the user has never touched the UI), returns the zero-value
// Preferences so the frontend's render path doesn't have to
// special-case "first visit".
//
// Also returns KnownEventTypes + KnownChannels + per-event
// SystemDefaultChannels so the UI can render toggles for every
// event the build knows about — even ones with no user-set rows —
// without hard-coding the catalog client-side.
func (h *Handler) GetAccountPreferences(
	ctx context.Context,
	_ openapi.GetAccountPreferencesRequestObject,
) (openapi.GetAccountPreferencesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetAccountPreferences401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}

	prefs, err := h.loadPreferences(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	return openapi.GetAccountPreferences200JSONResponse(buildResponse(prefs)), nil
}

// ChannelsFor is the cross-package entry point notification writers
// call to resolve the channel list ("in_app", "email", ...) the user
// wants the given event verb delivered through. Goes through the
// LRU first so the hot per-delivery decision path doesn't slam the
// DB. Returns SystemDefaultChannels(verb) when the user has no
// override; explicit empty array MEANS "mute" and propagates as-is.
func (h *Handler) ChannelsFor(ctx context.Context, ref int64, verb string) ([]string, error) {
	prefs, err := h.loadPreferences(ctx, ref)
	if err != nil {
		return nil, err
	}
	return prefs.ChannelsFor(verb), nil
}

// loadPreferences fetches the user's Preferences via the LRU when
// available, falling back to the DB on cache miss. Exposed as a
// method so cross-package consumers (notification writers in I2+)
// can reuse the same cache-aware path.
func (h *Handler) loadPreferences(ctx context.Context, ref int64) (Preferences, error) {
	if h.byUser != nil {
		if hit, ok := h.byUser.Get(userKey(ref)); ok {
			return hit, nil
		}
	}
	q := New(h.pool)
	row, err := q.GetUserPreferences(ctx, ref)
	var prefs Preferences
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// First-visit case — synthesize the zero-value prefs.
		prefs = Preferences{NotificationChannels: NotificationChannels{}}
	case err != nil:
		return Preferences{}, err
	default:
		prefs, err = UnmarshalPreferencesRow(row.NotificationChannels, row.DefaultViews, row.EmailCadence, row.FeedFilters, row.TeamRail)
		if err != nil {
			return Preferences{}, err
		}
	}
	if h.byUser != nil {
		// Best-effort populate. The LRU's Add never fails — but using
		// a separate variable here keeps the code shape consistent
		// with the social handler's cache wiring.
		h.byUser.Add(userKey(ref), prefs)
	}
	return prefs, nil
}

// PatchAccountPreferences — PATCH /account/preferences
//
// Replaces the caller's preferences with the supplied object.
// Validates against the known event-type + channel sets before
// touching the DB so a typo'd client doesn't end up persisted.
func (h *Handler) PatchAccountPreferences(
	ctx context.Context,
	req openapi.PatchAccountPreferencesRequestObject,
) (openapi.PatchAccountPreferencesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.PatchAccountPreferences401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	if req.Body == nil {
		return openapi.PatchAccountPreferences400JSONResponse{
			Error: "request body required",
		}, nil
	}

	prefs := preferencesFromRequest(*req.Body)
	if err := ValidatePreferences(prefs); err != nil {
		return openapi.PatchAccountPreferences400JSONResponse{
			Error: err.Error(),
		}, nil
	}

	// ONE write path, shared with UnsubscribeEmail. It used to be two
	// copies of the same marshal-and-upsert, which is how a column
	// added to the row gets persisted by one caller and silently reset
	// to its default by the other — exactly what would have happened to
	// feed_filters here (#891).
	if err := h.savePreferences(ctx, id.UserRef, prefs); err != nil {
		return nil, err
	}

	if h.logger != nil {
		h.logger.Info("user preferences saved",
			slog.Int64("user_ref", id.UserRef),
			slog.Int("channel_event_count", len(prefs.NotificationChannels)),
		)
	}

	return openapi.PatchAccountPreferences200JSONResponse(buildResponse(prefs)), nil
}

// buildResponse projects a Preferences value into the openapi wire
// shape, including the build's known catalogs so the UI doesn't
// have to hard-code them.
func buildResponse(p Preferences) openapi.UserPreferencesResponse {
	defaults := make(map[string][]string, len(KnownEventTypes))
	for _, e := range KnownEventTypes {
		defaults[e] = SystemDefaultChannels(e)
	}
	views := openapi.UserPreferencesViews{}
	if p.DefaultViews.HomeTab != "" {
		v := openapi.UserPreferencesViewsHomeTab(p.DefaultViews.HomeTab)
		views.HomeTab = &v
	}
	if p.DefaultViews.BrowseLayout != "" {
		v := openapi.UserPreferencesViewsBrowseLayout(p.DefaultViews.BrowseLayout)
		views.BrowseLayout = &v
	}
	if p.DefaultViews.BrowseSort != "" {
		v := openapi.UserPreferencesViewsBrowseSort(p.DefaultViews.BrowseSort)
		views.BrowseSort = &v
	}
	// Channels come back as a map keyed by event type. We always
	// surface the user's saved values verbatim; the UI fills missing
	// keys against the defaults map to render the toggles.
	channels := make(map[string][]string, len(p.NotificationChannels))
	for k, v := range p.NotificationChannels {
		channels[k] = append([]string(nil), v...)
	}

	cadence := make(map[string]string, len(p.EmailCadence))
	for k, v := range p.EmailCadence {
		cadence[k] = v
	}

	// Always present, even when every filter is off. Unlike the view
	// selections above — where "unset" and "the default" are genuinely
	// different states the UI renders differently — a boolean filter has
	// no third value, so an omitted object would just make the client
	// guess `false` rather than read it.
	filters := openapi.UserPreferencesFeedFilters{
		ShowRestricted: &p.FeedFilters.ShowRestricted,
	}

	// Always present, with both lists materialised even when empty
	// (#1113). Same argument as feed_filters one line up, for the list
	// case: "no curation" and "an empty curation" are the same rail, so
	// an omitted object would only make the client re-derive `[]`.
	rail := p.TeamRail.Sanitized()
	teamRail := openapi.UserPreferencesTeamRail{
		HiddenTeamIds: toWireIDs(rail.HiddenTeamIDs),
		TeamOrder:     toWireIDs(rail.TeamOrder),
	}

	return openapi.UserPreferencesResponse{
		NotificationChannels:   channels,
		EmailCadence:           &cadence,
		DefaultViews:           views,
		FeedFilters:            filters,
		TeamRail:               teamRail,
		KnownEventTypes:        append([]string(nil), KnownEventTypes...),
		KnownChannels:          append([]string(nil), KnownChannels...),
		DefaultChannelsByEvent: defaults,
	}
}

// preferencesFromRequest converts the openapi wire shape back into
// the typed Preferences struct for validation + persistence.
func preferencesFromRequest(body openapi.UserPreferencesRequest) Preferences {
	channels := NotificationChannels{}
	if body.NotificationChannels != nil {
		for k, v := range *body.NotificationChannels {
			channels[k] = append([]string(nil), v...)
		}
	}
	views := DefaultViews{}
	if body.DefaultViews != nil {
		if body.DefaultViews.HomeTab != nil {
			views.HomeTab = string(*body.DefaultViews.HomeTab)
		}
		if body.DefaultViews.BrowseLayout != nil {
			views.BrowseLayout = string(*body.DefaultViews.BrowseLayout)
		}
		if body.DefaultViews.BrowseSort != nil {
			views.BrowseSort = string(*body.DefaultViews.BrowseSort)
		}
	}
	cadence := EmailCadences{}
	if body.EmailCadence != nil {
		for k, v := range *body.EmailCadence {
			cadence[k] = v
		}
	}
	// Absent object, or an absent key inside it, decodes to the zero
	// value — which since #921 is the build's DEFAULT feed, not "no
	// filtering". There is no "unset" state for a boolean to fall back
	// to, which is exactly why the keys are named so that `false` is the
	// default experience (see FeedFilters).
	filters := FeedFilters{}
	if body.FeedFilters != nil && body.FeedFilters.ShowRestricted != nil {
		filters.ShowRestricted = *body.FeedFilters.ShowRestricted
	}
	// Full-object replacement, like everything else on this endpoint: an
	// absent `team_rail` clears the curation back to the default rail,
	// and an absent list inside it clears that list. The manage panel
	// always sends both, which is what makes "unhide the last hidden
	// team" expressible at all.
	rail := TeamRail{}
	if body.TeamRail != nil {
		rail.HiddenTeamIDs = fromWireIDs(body.TeamRail.HiddenTeamIds)
		rail.TeamOrder = fromWireIDs(body.TeamRail.TeamOrder)
	}
	return Preferences{
		NotificationChannels: channels,
		DefaultViews:         views,
		EmailCadence:         cadence,
		FeedFilters:          filters,
		TeamRail:             rail.Sanitized(),
	}
}

// toWireIDs projects the stored id list onto the generated wire type.
//
// It hands back a pointer to a NON-NIL slice, so an empty list
// marshals as `[]` rather than `null`. The client treats the two the
// same, but a response that alternates between them depending on
// whether the reader has ever hidden a team is a needless shape change
// in a session payload.
//
// A parse failure is impossible here rather than swallowed:
// TeamRail.Sanitized has already dropped anything that is not a UUID,
// and every caller sanitizes before projecting. Belt and braces anyway,
// because a silently truncated rail would be a bad way to learn that a
// future caller skipped that step.
func toWireIDs(in []string) *[]openapi_types.UUID {
	out := make([]openapi_types.UUID, 0, len(in))
	for _, id := range in {
		u, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		out = append(out, u)
	}
	return &out
}

// fromWireIDs is the inverse. The JSONB column stores plain strings —
// the typed UUID exists to reject a malformed id at the edge, not to
// change what is on disk.
func fromWireIDs(in *[]openapi_types.UUID) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(*in))
	for _, u := range *in {
		out = append(out, u.String())
	}
	return out
}

// CadenceFor returns the caller's email cadence for a verb via the
// cache-aware prefs load. Defaults to immediate. The notifications
// Writer calls this on every email-eligible notification, so it rides
// the same 5-min userprefs.by_user LRU as ChannelsFor.
func (h *Handler) CadenceFor(ctx context.Context, ref int64, verb string) (string, error) {
	prefs, err := h.loadPreferences(ctx, ref)
	if err != nil {
		return CadenceImmediate, err
	}
	return prefs.CadenceFor(verb), nil
}

// UnsubscribeEmail turns off the email channel for a topic (Phase
// 1.55.Y one-click unsubscribe). topic == "__all__" turns email off
// for every known event type. Sets an explicit override that keeps
// in_app on but drops email, so it survives the "absent key = system
// default" fallback. Persists + invalidates the prefs cache. Called by
// the unauthenticated /unsubscribe endpoint after token verification —
// the signed token IS the authorization.
func (h *Handler) UnsubscribeEmail(ctx context.Context, ref int64, topic string) error {
	prefs, err := h.loadPreferences(ctx, ref)
	if err != nil {
		return err
	}
	if prefs.NotificationChannels == nil {
		prefs.NotificationChannels = NotificationChannels{}
	}
	dropEmail := func(t string) {
		cur := prefs.NotificationChannels[t]
		if cur == nil {
			// No override yet — start from the system default so we
			// preserve whatever non-email channels it carried.
			cur = SystemDefaultChannels(t)
		}
		next := make([]string, 0, len(cur))
		for _, ch := range cur {
			if ch != ChannelEmail {
				next = append(next, ch)
			}
		}
		prefs.NotificationChannels[t] = next
	}
	if topic == "__all__" {
		for _, t := range KnownEventTypes {
			dropEmail(t)
		}
	} else {
		dropEmail(topic)
	}
	return h.savePreferences(ctx, ref, prefs)
}

// savePreferences persists a full Preferences object + invalidates the
// cache. Extracted so UnsubscribeEmail reuses the same write path as
// PatchAccountPreferences.
func (h *Handler) savePreferences(ctx context.Context, ref int64, prefs Preferences) error {
	channelsJSON, err := MarshalNotificationChannels(prefs.NotificationChannels)
	if err != nil {
		return err
	}
	viewsJSON, err := MarshalDefaultViews(prefs.DefaultViews)
	if err != nil {
		return err
	}
	cadenceJSON, err := MarshalEmailCadence(prefs.EmailCadence)
	if err != nil {
		return err
	}
	filtersJSON, err := MarshalFeedFilters(prefs.FeedFilters)
	if err != nil {
		return err
	}
	teamRailJSON, err := MarshalTeamRail(prefs.TeamRail)
	if err != nil {
		return err
	}
	if err := New(h.pool).UpsertUserPreferences(ctx, UpsertUserPreferencesParams{
		UserRef:              ref,
		NotificationChannels: channelsJSON,
		DefaultViews:         viewsJSON,
		EmailCadence:         cadenceJSON,
		FeedFilters:          filtersJSON,
		TeamRail:             teamRailJSON,
	}); err != nil {
		return err
	}
	// Invalidate the cached row across this process AND every federated
	// peer via the cache.Registry NOTIFY broadcast. A reader racing this
	// write should hit the DB once and see the freshly-saved values
	// everywhere rather than serve a stale feed filter.
	if h.byUser != nil {
		if err := h.byUser.Invalidate(ctx, userKey(ref)); err != nil && h.logger != nil {
			h.logger.LogAttrs(ctx, slog.LevelWarn, "userprefs.cache.invalidate.error",
				slog.Int64("user_ref", ref),
				slog.String("err", err.Error()),
			)
		}
	}
	return nil
}

// ShowRestrictedFeedMembers reports whether the caller has asked to keep
// the #883 placeholders in their browse feed (#891, inverted by #921).
// The cross-package entry point the posts handler calls once per feed
// page — through the same 5-minute userprefs.by_user LRU as ChannelsFor,
// so the hottest list in the app pays a PK lookup on cache miss and
// nothing on a hit.
//
// A user with no preferences row returns false, which is the whole
// default guarantee: since #921 the placeholders are hidden unless the
// reader asks for them back. `false` on the error path means the same
// thing it means everywhere else here — the DEFAULT feed — so a failed
// lookup degrades to the experience every other account is having rather
// than to a surprising one. See posts.showRestricted for the seam.
func (h *Handler) ShowRestrictedFeedMembers(ctx context.Context, ref int64) (bool, error) {
	prefs, err := h.loadPreferences(ctx, ref)
	if err != nil {
		return false, err
	}
	return prefs.FeedFilters.ShowRestricted, nil
}

// touch keeps the time import live for the (currently unused)
// updated_at logging path; pre-empting an unused-import error if we
// later add a "saved X seconds ago" admin surface against the
// timestamps. Cheap, removable once that surface exists.
var _ = time.Now
