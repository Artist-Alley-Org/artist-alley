// Package users implements the public user-profile surface.
//
// The RS "user" table carries auth-bearing data we never expose;
// user_profiles (migration 00021) carries display-layer fields. Reads
// merge both; defaults substitute when no profile row exists. Federation:
// the profile row is what gets mirrored to peer sites.
package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const (
	CapEditAnyProfile = "users.profile.edit.any"
	CapSystemAdmin    = "system.admin"
)

// CacheDomain is the NOTIFY channel for per-user public-profile cache
// entries. Exported because cross-package writers (the posts handler
// on post create/delete, future federation imports) call
// [InvalidateProfile] which references it.
const CacheDomain = "user.profile"

type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// byRef caches the closure-resolved openapi.UserPublic by
	// rs_user_id. nil-safe — nil means "no cache", every request
	// hits the DB. The by-username path doesn't cache (rare URL,
	// not worth the second-key bookkeeping); it always queries.
	byRef *cache.Cache[openapi.UserPublic]
}

func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{Pool: pool, Logger: logger}
	if registry != nil {
		// 5_000 entries comfortably fits ~1MB resident for typical
		// profile sizes and covers the hot end of any plausible
		// active-author set. Anything cold falls back to DB.
		h.byRef = cache.Register[openapi.UserPublic](registry, CacheDomain, 5_000)
	}
	return h
}

// InvalidateProfile broadcasts a cache invalidation for the given
// user's public-profile entry. Call after any DB write that could
// change what /users/{ref} returns:
//   - profile edits (UpsertUserProfile)
//   - posts.CreatePost / DeletePost (post_count is part of the cached
//     value, so author-side post mutations need to evict)
//
// Broadcast-only. Same-process callers without direct Cache access
// rely on the Registry's LISTEN goroutine to dispatch the eviction.
// Within this package, UpsertUserProfile uses byRef.Invalidate
// directly for immediate local eviction.
//
// Safe to call with a nil registry (no-op).
func InvalidateProfile(ctx context.Context, registry *cache.Registry, userRef int64) {
	if registry == nil {
		return
	}
	_ = registry.Emit(ctx, CacheDomain, strconv.FormatInt(userRef, 10))
}

// publicRow is the structural common shape of both GetUserPublicBy*
// sqlc result types. sqlc generates a distinct type per query so we
// adapt both into this shared shape before rendering.
type publicRow struct {
	RsUserID              int64
	Username              *string
	Fullname              *string
	CreatedAt             pgtype.Timestamptz
	DisplayName           string
	Bio                   string
	AvatarURL             *string
	Location              string
	WebsiteURL            *string
	SocialLinks           []byte // raw JSONB
	Language              string
	Theme                 string
	ProfileOriginServerID pgtype.UUID
}

func fromByRef(r GetUserPublicByRefRow) publicRow {
	return publicRow{
		RsUserID: r.RsUserID, Username: r.Username, Fullname: r.Fullname,
		CreatedAt: r.CreatedAt, DisplayName: r.DisplayName, Bio: r.Bio,
		AvatarURL: r.AvatarUrl, Location: r.Location, WebsiteURL: r.WebsiteUrl,
		SocialLinks: r.SocialLinks, Language: r.Language, Theme: r.Theme,
		ProfileOriginServerID: r.ProfileOriginServerID,
	}
}

func fromByUsername(r GetUserPublicByUsernameRow) publicRow {
	return publicRow{
		RsUserID: r.RsUserID, Username: r.Username, Fullname: r.Fullname,
		CreatedAt: r.CreatedAt, DisplayName: r.DisplayName, Bio: r.Bio,
		AvatarURL: r.AvatarUrl, Location: r.Location, WebsiteURL: r.WebsiteUrl,
		SocialLinks: r.SocialLinks, Language: r.Language, Theme: r.Theme,
		ProfileOriginServerID: r.ProfileOriginServerID,
	}
}

// ---------------------------------------------------------------------------
// GetUserPublicByRef
// ---------------------------------------------------------------------------

func (h *Handler) GetUserPublicByRef(
	ctx context.Context,
	req openapi.GetUserPublicByRefRequestObject,
) (openapi.GetUserPublicByRefResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.GetUserPublicByRef401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	key := strconv.FormatInt(req.Ref, 10)
	if h.byRef != nil {
		if v, ok := h.byRef.Get(key); ok {
			return openapi.GetUserPublicByRef200JSONResponse(v), nil
		}
	}
	q := New(h.Pool)
	row, err := q.GetUserPublicByRef(ctx, req.Ref)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetUserPublicByRef404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("users: get by ref: %w", err)
	}
	out, err := h.rowToAPI(ctx, q, fromByRef(row))
	if err != nil {
		return nil, err
	}
	if h.byRef != nil {
		h.byRef.Add(key, *out)
	}
	return openapi.GetUserPublicByRef200JSONResponse(*out), nil
}

// ---------------------------------------------------------------------------
// GetUserPublicByUsername
// ---------------------------------------------------------------------------

func (h *Handler) GetUserPublicByUsername(
	ctx context.Context,
	req openapi.GetUserPublicByUsernameRequestObject,
) (openapi.GetUserPublicByUsernameResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.GetUserPublicByUsername401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	username := req.Username
	row, err := q.GetUserPublicByUsername(ctx, &username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetUserPublicByUsername404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("users: get by username: %w", err)
	}
	out, err := h.rowToAPI(ctx, q, fromByUsername(row))
	if err != nil {
		return nil, err
	}
	return openapi.GetUserPublicByUsername200JSONResponse(*out), nil
}

// ---------------------------------------------------------------------------
// UpdateUserProfile
// ---------------------------------------------------------------------------

func (h *Handler) UpdateUserProfile(
	ctx context.Context,
	req openapi.UpdateUserProfileRequestObject,
) (openapi.UpdateUserProfileResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.UpdateUserProfile401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdateUserProfile400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	if caller.UserRef != req.Ref && !caller.Can(CapEditAnyProfile) && !caller.Can(CapSystemAdmin) {
		return openapi.UpdateUserProfile403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "cannot edit another user's profile"},
		}, nil
	}

	q := New(h.Pool)
	existing, err := q.GetUserPublicByRef(ctx, req.Ref)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateUserProfile404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("users: existence check: %w", err)
	}

	// PATCH resolution. Present field overrides current; absent keeps.
	displayName := existing.DisplayName
	if req.Body.DisplayName != nil {
		displayName = *req.Body.DisplayName
	}
	bio := existing.Bio
	if req.Body.Bio != nil {
		bio = *req.Body.Bio
	}
	avatarURL := existing.AvatarUrl
	if req.Body.AvatarUrl != nil {
		v := *req.Body.AvatarUrl
		avatarURL = &v
	}
	location := existing.Location
	if req.Body.Location != nil {
		location = *req.Body.Location
	}
	websiteURL := existing.WebsiteUrl
	if req.Body.WebsiteUrl != nil {
		v := *req.Body.WebsiteUrl
		websiteURL = &v
	}
	socialLinks := existing.SocialLinks
	if req.Body.SocialLinks != nil {
		b, err := json.Marshal(*req.Body.SocialLinks)
		if err != nil {
			return openapi.UpdateUserProfile400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid social_links"},
			}, nil
		}
		socialLinks = b
	}
	language := existing.Language
	if req.Body.Language != nil {
		language = *req.Body.Language
	}
	theme := existing.Theme
	if req.Body.Theme != nil {
		switch string(*req.Body.Theme) {
		case "", "light", "dark":
			theme = string(*req.Body.Theme)
		default:
			return openapi.UpdateUserProfile400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "theme must be '', 'light', or 'dark'"},
			}, nil
		}
	}

	if _, err := q.UpsertUserProfile(ctx, UpsertUserProfileParams{
		RsUserID:    req.Ref,
		DisplayName: &displayName,
		Bio:         bio,
		AvatarUrl:   avatarURL,
		Location:    location,
		WebsiteUrl:  websiteURL,
		SocialLinks: socialLinks,
		Language:    language,
		Theme:       theme,
	}); err != nil {
		return nil, fmt.Errorf("users: upsert profile: %w", err)
	}

	// The cached entry (if any) just went stale. Local-evict +
	// broadcast in one Invalidate call — peer instances and the
	// LISTEN goroutine pick up via NOTIFY.
	if h.byRef != nil {
		_ = h.byRef.Invalidate(ctx, strconv.FormatInt(req.Ref, 10))
	}

	row, err := q.GetUserPublicByRef(ctx, req.Ref)
	if err != nil {
		return nil, fmt.Errorf("users: refetch: %w", err)
	}
	out, err := h.rowToAPI(ctx, q, fromByRef(row))
	if err != nil {
		return nil, err
	}
	if h.byRef != nil {
		h.byRef.Add(strconv.FormatInt(req.Ref, 10), *out)
	}
	return openapi.UpdateUserProfile200JSONResponse(*out), nil
}

// ---------------------------------------------------------------------------
// Row → API
// ---------------------------------------------------------------------------

// rowToAPI maps the merged user+profile row into the public API shape,
// resolving display_name precedence and computing post_count.
//
// Precedence for display_name (the always-non-empty resolved string):
//   1. profile.display_name (if non-empty)
//   2. user.fullname (if non-empty)
//   3. user.username
// The frontend never has to do this resolution itself.
func (h *Handler) rowToAPI(ctx context.Context, q *Queries, r publicRow) (*openapi.UserPublic, error) {
	display := r.DisplayName
	if display == "" && r.Fullname != nil && *r.Fullname != "" {
		display = *r.Fullname
	}
	if display == "" && r.Username != nil {
		display = *r.Username
	}
	if display == "" {
		display = fmt.Sprintf("user %d", r.RsUserID)
	}

	postCount, err := q.CountPostsByAuthor(ctx, r.RsUserID)
	if err != nil {
		return nil, fmt.Errorf("users: count posts: %w", err)
	}

	// social_links is stored as raw JSONB bytes; decode into a map for
	// the API response. Empty / NULL just renders as an empty map.
	var socialMap map[string]string
	if len(r.SocialLinks) > 0 {
		if err := json.Unmarshal(r.SocialLinks, &socialMap); err != nil {
			// Tolerate malformed rows — return empty rather than 500.
			socialMap = map[string]string{}
		}
	}

	out := openapi.UserPublic{
		Ref:         r.RsUserID,
		DisplayName: display,
		Bio:         &r.Bio,
		Location:    &r.Location,
		AvatarUrl:   r.AvatarURL,
		WebsiteUrl:  r.WebsiteURL,
		MemberSince: r.CreatedAt.Time,
		PostCount:   postCount,
	}
	if r.Username != nil {
		out.Username = *r.Username
	}
	if r.Fullname != nil && *r.Fullname != "" {
		out.Fullname = r.Fullname
	}
	if len(socialMap) > 0 {
		m := socialMap
		out.SocialLinks = &m
	}
	if r.Language != "" {
		l := r.Language
		out.Language = &l
	}
	if r.Theme != "" {
		t := openapi.UserPublicTheme(r.Theme)
		out.Theme = &t
	}
	if r.ProfileOriginServerID.Valid {
		v := openapi_types.UUID(r.ProfileOriginServerID.Bytes)
		out.OriginServerId = &v
	}
	return &out, nil
}

// Compile-time strict-server interface assertion.
var _ interface {
	GetUserPublicByRef(context.Context, openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error)
	GetUserPublicByUsername(context.Context, openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error)
	UpdateUserProfile(context.Context, openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error)
} = (*Handler)(nil)
