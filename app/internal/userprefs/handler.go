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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
		prefs, err = UnmarshalPreferencesRow(row.NotificationChannels, row.DefaultViews, row.EmailCadence)
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

	channelsJSON, err := MarshalNotificationChannels(prefs.NotificationChannels)
	if err != nil {
		return nil, err
	}
	viewsJSON, err := MarshalDefaultViews(prefs.DefaultViews)
	if err != nil {
		return nil, err
	}
	cadenceJSON, err := MarshalEmailCadence(prefs.EmailCadence)
	if err != nil {
		return nil, err
	}

	q := New(h.pool)
	if err := q.UpsertUserPreferences(ctx, UpsertUserPreferencesParams{
		UserRef:              id.UserRef,
		NotificationChannels: channelsJSON,
		DefaultViews:         viewsJSON,
		EmailCadence:         cadenceJSON,
	}); err != nil {
		return nil, err
	}

	// Invalidate the cached row across this process AND every
	// federated peer via the cache.Registry NOTIFY broadcast. The
	// post-write notification writer might race to read prefs with
	// stale defaults; we want that next read to hit the DB once + the
	// freshly-saved values everywhere.
	if h.byUser != nil {
		if err := h.byUser.Invalidate(ctx, userKey(id.UserRef)); err != nil && h.logger != nil {
			h.logger.LogAttrs(ctx, slog.LevelWarn, "userprefs.cache.invalidate.error",
				slog.Int64("user_ref", id.UserRef),
				slog.String("err", err.Error()),
			)
		}
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
		v := p.DefaultViews.HomeTab
		views.HomeTab = &v
	}
	if p.DefaultViews.BrowseLayout != "" {
		v := p.DefaultViews.BrowseLayout
		views.BrowseLayout = &v
	}
	if p.DefaultViews.BrowseSort != "" {
		v := p.DefaultViews.BrowseSort
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

	return openapi.UserPreferencesResponse{
		NotificationChannels:   channels,
		EmailCadence:           &cadence,
		DefaultViews:           views,
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
			views.HomeTab = *body.DefaultViews.HomeTab
		}
		if body.DefaultViews.BrowseLayout != nil {
			views.BrowseLayout = *body.DefaultViews.BrowseLayout
		}
		if body.DefaultViews.BrowseSort != nil {
			views.BrowseSort = *body.DefaultViews.BrowseSort
		}
	}
	cadence := EmailCadences{}
	if body.EmailCadence != nil {
		for k, v := range *body.EmailCadence {
			cadence[k] = v
		}
	}
	return Preferences{
		NotificationChannels: channels,
		DefaultViews:         views,
		EmailCadence:         cadence,
	}
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
	if err := New(h.pool).UpsertUserPreferences(ctx, UpsertUserPreferencesParams{
		UserRef:              ref,
		NotificationChannels: channelsJSON,
		DefaultViews:         viewsJSON,
		EmailCadence:         cadenceJSON,
	}); err != nil {
		return err
	}
	if h.byUser != nil {
		_ = h.byUser.Invalidate(ctx, userKey(ref))
	}
	return nil
}

// touch keeps the time import live for the (currently unused)
// updated_at logging path; pre-empting an unused-import error if we
// later add a "saved X seconds ago" admin surface against the
// timestamps. Cheap, removable once that surface exists.
var _ = time.Now
