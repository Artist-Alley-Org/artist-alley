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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Handler is the openapi-strict adapter. Wraps a pgxpool.Pool +
// logger; api.go's apiServer delegates to it.
type Handler struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewHandler wires the handler to its dependencies.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger) *Handler {
	return &Handler{pool: pool, logger: logger}
}

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

	q := New(h.pool)
	row, err := q.GetUserPreferences(ctx, id.UserRef)
	var prefs Preferences
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// First-visit case — return zero-value prefs. Not a 404.
		prefs = Preferences{NotificationChannels: NotificationChannels{}}
	case err != nil:
		return nil, err
	default:
		prefs, err = UnmarshalPreferencesRow(row.NotificationChannels, row.DefaultViews)
		if err != nil {
			return nil, err
		}
	}

	return openapi.GetAccountPreferences200JSONResponse(buildResponse(prefs)), nil
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

	q := New(h.pool)
	if err := q.UpsertUserPreferences(ctx, UpsertUserPreferencesParams{
		RsUserID:             id.UserRef,
		NotificationChannels: channelsJSON,
		DefaultViews:         viewsJSON,
	}); err != nil {
		return nil, err
	}

	if h.logger != nil {
		h.logger.Info("user preferences saved",
			slog.Int64("rs_user_id", id.UserRef),
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

	return openapi.UserPreferencesResponse{
		NotificationChannels:   channels,
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
	return Preferences{
		NotificationChannels: channels,
		DefaultViews:         views,
	}
}

// touch keeps the time import live for the (currently unused)
// updated_at logging path; pre-empting an unused-import error if we
// later add a "saved X seconds ago" admin surface against the
// timestamps. Cheap, removable once that surface exists.
var _ = time.Now
