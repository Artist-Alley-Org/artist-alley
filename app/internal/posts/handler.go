// Package posts implements the post slice of the artist-alley HTTP
// API — the feed entity that wraps 1+ assets per the post-model
// decision (see Phase 1.13.D-2).
//
// Routing surface:
//
//	GET    /posts                          — paginated feed
//	POST   /posts                          — create
//	GET    /posts/{id}                     — single post with members
//	PATCH  /posts/{id}                     — partial update
//	DELETE /posts/{id}                     — soft delete
//	POST   /posts/{id}/assets              — attach an asset
//	DELETE /posts/{id}/assets/{asset_id}   — detach an asset
//
// Authorization (1.13.D-2 baseline):
//   - All read endpoints require an authenticated session. Visibility
//     filters apply on top: callers see all `public` posts plus their
//     own private/followers posts. 1.13.G adds anonymous-browse for
//     public posts.
//   - Write endpoints require the caller to be the post's author.
//     `posts.admin` / `system.admin` capabilities override.
//
// Like/comment counts (`like_count`, `comment_count`) come straight
// from the table's denormalised columns; Phase 1.13.D-4 will start
// maintaining them via triggers when the likes / comments tables ship.
package posts

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Cache domain name. Stable string used as NOTIFY target — peer
// instances key off this when dispatching invalidations.
const cacheDomainPostByID = "post.id"

// Capability gates. `posts.admin` lets a moderator edit/delete any
// post; `system.admin` is the global override.
const (
	CapPostsAdmin  = "posts.admin"
	CapSystemAdmin = "system.admin"
)

const maxListLimit = 200

// Handler implements the posts slice of openapi.StrictServerInterface.
//
// `byID` caches the fully-hydrated post (with members + tags joined
// in) by UUID string. The browse feed renders 30–50 posts per page;
// each cold render fans out into 3 queries per item via
// fetchFullPost (GetPost + ListPostAssets + ListPostTags). With the
// LRU warm, repeat reads collapse to one ListPostsPage + cache
// hits. Writes (Create / Update / Delete / Add-Remove member) call
// invalidate which both drops the local entry and broadcasts via
// the Phase 1.10 cache.Registry NOTIFY channel so peer instances
// drop their copy too.
//
// Like / comment count updates land in Phase 1.13.D-4 — those tables'
// triggers will pg_notify the cache_invalidate channel directly,
// no app-side hook needed.
//
// Nil registry is legal — handler falls back to direct DB reads
// (useful in tests that don't want the LISTEN goroutine).
type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	byID *cache.Cache[openapi.Post]
}

func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{Pool: pool, Logger: logger}
	if registry != nil {
		// 10k entries comfortably fits ~10MB of resident memory for a
		// typical post body and gives us a deep working set for the
		// hot end of the feed. LRU evicts cold entries; next read
		// repopulates.
		h.byID = cache.Register[openapi.Post](registry, cacheDomainPostByID, 10_000)
	}
	return h
}

// ---------------------------------------------------------------------------
// CreatePost
// ---------------------------------------------------------------------------

func (h *Handler) CreatePost(
	ctx context.Context,
	req openapi.CreatePostRequestObject,
) (openapi.CreatePostResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.CreatePost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreatePost400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	in := req.Body
	if len(in.Members) == 0 {
		return openapi.CreatePost400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "members: at least one asset required"},
		}, nil
	}

	visibility := "public"
	if in.Visibility != nil {
		visibility = string(*in.Visibility)
	}
	if !validVisibility(visibility) {
		return openapi.CreatePost400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "visibility must be public|followers|private"},
		}, nil
	}

	// Cover defaults to the first member when the caller doesn't pin
	// one explicitly. This matches the modal UX (first uploaded =
	// cover unless dragged).
	var coverID pgtype.UUID
	if in.CoverAssetId != nil {
		coverID = pgtype.UUID{Bytes: uuid.UUID(*in.CoverAssetId), Valid: true}
	} else {
		coverID = pgtype.UUID{Bytes: uuid.UUID(in.Members[0].AssetId), Valid: true}
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("posts: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	row, err := q.CreatePost(ctx, CreatePostParams{
		AuthorUserRef: id.UserRef,
		Title:         strOr(in.Title, ""),
		Description:   strOr(in.Description, ""),
		Visibility:    visibility,
		CoverAssetID:  coverID,
	})
	if err != nil {
		return nil, fmt.Errorf("posts: create: %w", err)
	}

	// Members. Idempotent on (post_id, asset_id) so de-dupes on input.
	for _, m := range in.Members {
		if err := q.AddPostAsset(ctx, AddPostAssetParams{
			PostID:    row.ID,
			AssetID:   pgtype.UUID{Bytes: uuid.UUID(m.AssetId), Valid: true},
			SortOrder: int32Or(m.SortOrder, 0),
		}); err != nil {
			if isFKError(err, "post_assets_asset_id_fkey") {
				return openapi.CreatePost404JSONResponse{
					NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found: " + m.AssetId.String()},
				}, nil
			}
			return nil, fmt.Errorf("posts: add member: %w", err)
		}
	}

	// Tags (replace-all shape; we just inserted the post so it has
	// none yet).
	if in.Tags != nil && len(*in.Tags) > 0 {
		clean := dedupeTags(*in.Tags)
		if err := q.ReplacePostTags(ctx, ReplacePostTagsParams{
			PostID:  row.ID,
			Column2: clean,
		}); err != nil {
			return nil, fmt.Errorf("posts: tags: %w", err)
		}
	}

	// Optional: add the new post to a collection at create time.
	// Used by the context-aware upload modal when the user is on a
	// collection page and drops files in.
	if in.CollectionId != nil {
		if err := q.AddCollectionPost(ctx, AddCollectionPostParams{
			CollectionID: pgtype.UUID{Bytes: uuid.UUID(*in.CollectionId), Valid: true},
			PostID:       row.ID,
			SortOrder:    0,
			Pinned:       true,
		}); err != nil {
			if isFKError(err, "collection_posts_collection_id_fkey") {
				return openapi.CreatePost404JSONResponse{
					NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "collection not found"},
				}, nil
			}
			return nil, fmt.Errorf("posts: add to collection: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("posts: commit: %w", err)
	}

	// Brand-new ID — nothing to evict locally, but the Invalidate
	// also broadcasts NOTIFY so any peer that race-read this row
	// (unlikely but possible) drops its copy.
	h.cacheInvalidate(ctx, row.ID)

	// Re-read with full member + tag join. The trigger has fired by
	// now so search_text reflects the new state.
	full, err := h.fetchFullPost(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	return openapi.CreatePost201JSONResponse(*full), nil
}

// ---------------------------------------------------------------------------
// GetPost
// ---------------------------------------------------------------------------

func (h *Handler) GetPost(
	ctx context.Context,
	req openapi.GetPostRequestObject,
) (openapi.GetPostResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetPost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	full, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetPost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canReadPost(id, full) {
		return openapi.GetPost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not visible to this user"},
		}, nil
	}
	return openapi.GetPost200JSONResponse(*full), nil
}

// ---------------------------------------------------------------------------
// UpdatePost
// ---------------------------------------------------------------------------

func (h *Handler) UpdatePost(
	ctx context.Context,
	req openapi.UpdatePostRequestObject,
) (openapi.UpdatePostResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.UpdatePost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdatePost400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("posts: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	current, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, current.AuthorUserRef) {
		return openapi.UpdatePost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}

	in := req.Body
	var visPtr *string
	if in.Visibility != nil {
		s := string(*in.Visibility)
		if !validVisibility(s) {
			return openapi.UpdatePost400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "visibility must be public|followers|private"},
			}, nil
		}
		visPtr = &s
	}
	var coverPtr pgtype.UUID
	if in.CoverAssetId != nil {
		coverPtr = pgtype.UUID{Bytes: uuid.UUID(*in.CoverAssetId), Valid: true}
	}

	if _, err := q.UpdatePost(ctx, UpdatePostParams{
		ID:           pgID,
		Title:        in.Title,
		Description:  in.Description,
		Visibility:   visPtr,
		CoverAssetID: coverPtr,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdatePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, fmt.Errorf("posts: update: %w", err)
	}

	if in.Tags != nil {
		clean := dedupeTags(*in.Tags)
		if err := q.ReplacePostTags(ctx, ReplacePostTagsParams{
			PostID:  pgID,
			Column2: clean,
		}); err != nil {
			return nil, fmt.Errorf("posts: replace tags: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("posts: commit: %w", err)
	}

	h.cacheInvalidate(ctx, pgID)

	full, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		return nil, err
	}
	return openapi.UpdatePost200JSONResponse(*full), nil
}

// ---------------------------------------------------------------------------
// DeletePost
// ---------------------------------------------------------------------------

func (h *Handler) DeletePost(
	ctx context.Context,
	req openapi.DeletePostRequestObject,
) (openapi.DeletePostResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.DeletePost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)

	cur, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.DeletePost404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef) {
		return openapi.DeletePost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}
	if err := q.SoftDeletePost(ctx, pgID); err != nil {
		return nil, fmt.Errorf("posts: delete: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.DeletePost204Response{}, nil
}

// ---------------------------------------------------------------------------
// ListPosts (the feed)
// ---------------------------------------------------------------------------

func (h *Handler) ListPosts(
	ctx context.Context,
	req openapi.ListPostsRequestObject,
) (openapi.ListPostsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListPosts401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	limit := int32(50)
	if req.Params.Limit != nil {
		l := *req.Params.Limit
		if l < 1 {
			l = 1
		}
		if l > maxListLimit {
			l = maxListLimit
		}
		limit = int32(l)
	}

	var cursorTs pgtype.Timestamptz
	var cursorID pgtype.UUID
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		ts, id, err := decodeCursor(*req.Params.Cursor)
		if err != nil {
			return openapi.ListPosts500JSONResponse{
				InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorTs = pgtype.Timestamptz{Time: ts, Valid: true}
		cursorID = pgtype.UUID{Bytes: id, Valid: true}
	}

	// Default visibility filter: public-only. Callers can pass
	// `?visibility=private` to get their own private posts (we still
	// AND with the caller's own author_ref so other people's privates
	// aren't leaked).
	visibility := "public"
	if req.Params.Visibility != nil {
		visibility = string(*req.Params.Visibility)
	}
	// Author filter: if caller asks for their own posts, drop the
	// visibility filter — they see all of their own visibilities.
	var visPtr *string
	if req.Params.AuthorRef != nil && *req.Params.AuthorRef == caller.UserRef {
		visPtr = nil
	} else {
		visPtr = &visibility
	}
	var authorPtr *int64
	if req.Params.AuthorRef != nil {
		authorPtr = req.Params.AuthorRef
	}

	var qText *string
	if req.Params.Q != nil {
		s := strings.TrimSpace(*req.Params.Q)
		if s != "" {
			qText = &s
		}
	}
	var tagPtr *string
	if req.Params.Tag != nil && *req.Params.Tag != "" {
		tagPtr = req.Params.Tag
	}

	fetch := limit + 1
	rows, err := New(h.Pool).ListPostsPage(ctx, ListPostsPageParams{
		AuthorUserRef:   authorPtr,
		Visibility:      visPtr,
		Q:               qText,
		Tag:             tagPtr,
		CursorPostedAt:  cursorTs,
		CursorID:        cursorID,
		RowLimit:        fetch,
	})
	if err != nil {
		return nil, fmt.Errorf("posts: list: %w", err)
	}

	items := make([]openapi.Post, 0, limit)
	var lastPostedAt time.Time
	var lastID uuid.UUID
	for i, r := range rows {
		if i >= int(limit) {
			break
		}
		full, err := h.fetchFullPost(ctx, r.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, err
		}
		items = append(items, *full)
		lastPostedAt = r.PostedAt.Time
		lastID = uuid.UUID(r.ID.Bytes)
	}

	resp := openapi.PostList{Items: items}
	if len(rows) > int(limit) {
		next := encodeCursor(lastPostedAt, lastID)
		resp.NextCursor = &next
	}
	return openapi.ListPosts200JSONResponse(resp), nil
}

// ---------------------------------------------------------------------------
// AddPostAsset / RemovePostAsset
// ---------------------------------------------------------------------------

func (h *Handler) AddPostAsset(
	ctx context.Context,
	req openapi.AddPostAssetRequestObject,
) (openapi.AddPostAssetResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddPostAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddPostAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddPostAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef) {
		return openapi.AddPostAsset403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}
	pgAsset := pgtype.UUID{Bytes: uuid.UUID(req.Body.AssetId), Valid: true}
	if err := q.AddPostAsset(ctx, AddPostAssetParams{
		PostID:    pgID,
		AssetID:   pgAsset,
		SortOrder: int32Or(req.Body.SortOrder, 0),
	}); err != nil {
		if isFKError(err, "post_assets_asset_id_fkey") {
			return openapi.AddPostAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("posts: add asset: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.AddPostAsset204Response{}, nil
}

func (h *Handler) RemovePostAsset(
	ctx context.Context,
	req openapi.RemovePostAssetRequestObject,
) (openapi.RemovePostAssetResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemovePostAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RemovePostAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef) {
		return openapi.RemovePostAsset403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}
	if err := q.RemovePostAsset(ctx, RemovePostAssetParams{
		PostID:  pgID,
		AssetID: pgtype.UUID{Bytes: uuid.UUID(req.AssetId), Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("posts: remove asset: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.RemovePostAsset204Response{}, nil
}

// ---------------------------------------------------------------------------
// ACLs — additive grants on top of role/team/visibility (ADR 0010 L6)
// ---------------------------------------------------------------------------
//
// Authorization: reading the ACL list requires read access to the post
// (canReadPost). Adding/removing requires write access (canMutatePost)
// so a viewer can't expand their own access by editing the ACL list.

func (h *Handler) ListPostAcls(
	ctx context.Context,
	req openapi.ListPostAclsRequestObject,
) (openapi.ListPostAclsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListPostAcls401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	full, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListPostAcls404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canReadPost(caller, full) {
		return openapi.ListPostAcls403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not visible to this user"},
		}, nil
	}
	rows, err := New(h.Pool).ListPostAcls(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("posts: list acls: %w", err)
	}
	out := make([]openapi.AclEntry, 0, len(rows))
	for _, r := range rows {
		e := openapi.AclEntry{
			PrincipalType: openapi.AclEntryPrincipalType(r.PrincipalType),
			PrincipalId:   r.PrincipalID,
			Permission:    openapi.AclEntryPermission(r.Permission),
			GrantedAt:     r.GrantedAt.Time,
		}
		if r.GrantedByRsUserID != nil {
			e.GrantedByRsUserId = r.GrantedByRsUserID
		}
		if r.ExpiresAt.Valid {
			t := r.ExpiresAt.Time
			e.ExpiresAt = &t
		}
		out = append(out, e)
	}
	return openapi.ListPostAcls200JSONResponse(out), nil
}

func (h *Handler) AddPostAcl(
	ctx context.Context,
	req openapi.AddPostAclRequestObject,
) (openapi.AddPostAclResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddPostAcl401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddPostAcl400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddPostAcl404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef) {
		return openapi.AddPostAcl403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}

	var expires pgtype.Timestamptz
	if req.Body.ExpiresAt != nil {
		expires = pgtype.Timestamptz{Time: *req.Body.ExpiresAt, Valid: true}
	}
	if err := q.AddPostAcl(ctx, AddPostAclParams{
		PostID:              pgID,
		PrincipalType:       string(req.Body.PrincipalType),
		PrincipalID:         req.Body.PrincipalId,
		Permission:          string(req.Body.Permission),
		GrantedByRsUserID:   &caller.UserRef,
		ExpiresAt:           expires,
	}); err != nil {
		return nil, fmt.Errorf("posts: add acl: %w", err)
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.AddPostAcl204Response{}, nil
}

func (h *Handler) RemovePostAcl(
	ctx context.Context,
	req openapi.RemovePostAclRequestObject,
) (openapi.RemovePostAclResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemovePostAcl401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	cur, err := q.GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RemovePostAcl404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
			}, nil
		}
		return nil, err
	}
	if !canMutatePost(caller, cur.AuthorUserRef) {
		return openapi.RemovePostAcl403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the post author"},
		}, nil
	}
	rows, err := q.RemovePostAcl(ctx, RemovePostAclParams{
		PostID:        pgID,
		PrincipalType: string(req.PrincipalType),
		PrincipalID:   req.PrincipalId,
		Permission:    string(req.Permission),
	})
	if err != nil {
		return nil, fmt.Errorf("posts: remove acl: %w", err)
	}
	if rows == 0 {
		return openapi.RemovePostAcl404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "ACL entry not found"},
		}, nil
	}
	h.cacheInvalidate(ctx, pgID)
	return openapi.RemovePostAcl204Response{}, nil
}

// ---------------------------------------------------------------------------
// Internal: fetch + cache plumbing
// ---------------------------------------------------------------------------

// fetchFullPost reads through the post-by-id cache. On miss it loads
// the post + members + tags (3 queries) and populates the cache.
func (h *Handler) fetchFullPost(ctx context.Context, id pgtype.UUID) (*openapi.Post, error) {
	key := uuidString(id)
	if h.byID != nil {
		if v, ok := h.byID.Get(key); ok {
			out := v
			return &out, nil
		}
	}
	q := New(h.Pool)
	row, err := q.GetPost(ctx, id)
	if err != nil {
		return nil, err
	}
	members, err := q.ListPostAssets(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("posts: list members: %w", err)
	}
	tags, err := q.ListPostTags(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("posts: list tags: %w", err)
	}
	out := postRowToAPI(row, members, tags)
	if h.byID != nil {
		h.byID.Add(key, out)
	}
	return &out, nil
}

// cacheInvalidate drops the local LRU entry and broadcasts to peers
// via the NOTIFY channel. Best-effort — a NOTIFY failure logs but
// doesn't propagate, so writers don't fail because of cache
// plumbing.
func (h *Handler) cacheInvalidate(ctx context.Context, id pgtype.UUID) {
	if h.byID == nil {
		return
	}
	key := uuidString(id)
	if err := h.byID.Invalidate(ctx, key); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "posts.cache.invalidate.error",
			slog.String("domain", cacheDomainPostByID),
			slog.String("key", key),
			slog.String("err", err.Error()),
		)
	}
}

func uuidString(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }

// ---------------------------------------------------------------------------
// Authorization helpers
// ---------------------------------------------------------------------------

// canMutatePost returns true if the caller can edit/delete this post.
// Author, system.admin, or posts.admin.
func canMutatePost(id *auth.Identity, authorRef int64) bool {
	if id == nil {
		return false
	}
	if id.UserRef == authorRef {
		return true
	}
	return id.Can(CapPostsAdmin) || id.Can(CapSystemAdmin)
}

// canReadPost gates GetPost. Author always wins; otherwise the
// visibility decides.
func canReadPost(id *auth.Identity, p *openapi.Post) bool {
	if id == nil {
		return false
	}
	if id.UserRef == p.AuthorUserRef {
		return true
	}
	switch p.Visibility {
	case openapi.PostVisibilityPublic:
		return true
	case openapi.PostVisibilityFollowers:
		// Phase 1.13.I lands the follows table; treat as public for now.
		// TODO(1.13.I): check follows(follower=id.UserRef, followee=p.AuthorUserRef)
		return true
	case openapi.PostVisibilityPrivate:
		return id.Can(CapPostsAdmin) || id.Can(CapSystemAdmin)
	}
	return false
}

func validVisibility(s string) bool {
	switch s {
	case "private", "followers", "public":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Row → API conversions
// ---------------------------------------------------------------------------

func postRowToAPI(p GetPostRow, members []ListPostAssetsRow, tags []string) openapi.Post {
	out := openapi.Post{
		Id:             openapi_types.UUID(p.ID.Bytes),
		AuthorUserRef:  p.AuthorUserRef,
		Title:          p.Title,
		Description:    p.Description,
		Visibility:     openapi.PostVisibility(p.Visibility),
		PostedAt:       p.PostedAt.Time,
		LikeCount:      p.LikeCount,
		CommentCount:   p.CommentCount,
		Tags:           append([]string{}, tags...),
		CreatedAt:      p.CreatedAt.Time,
		UpdatedAt:      p.UpdatedAt.Time,
		Members:        make([]openapi.PostMember, 0, len(members)),
	}
	if p.CoverAssetID.Valid {
		v := openapi_types.UUID(p.CoverAssetID.Bytes)
		out.CoverAssetId = &v
	}
	if p.OriginServerID.Valid {
		v := openapi_types.UUID(p.OriginServerID.Bytes)
		out.OriginServerId = &v
	}
	for _, m := range members {
		out.Members = append(out.Members, openapi.PostMember{
			AssetId:   openapi_types.UUID(m.AssetID.Bytes),
			SortOrder: int(m.SortOrder),
			Asset:     memberToAsset(m),
		})
	}
	return out
}

func memberToAsset(m ListPostAssetsRow) openapi.Asset {
	a := openapi.Asset{
		Id:            openapi_types.UUID(m.AssetID.Bytes),
		Title:         m.Title,
		Description:   &m.Description,
		ResourceType:  m.ResourceType,
		Status:        openapi.AssetStatus(m.Status),
		FileHash:      m.FileHash,
		FileExtension: m.FileExtension,
		FileSizeBytes: m.FileSizeBytes,
		Tags:          []string{},
		CreatedAt:     m.AssetCreatedAt.Time,
		UpdatedAt:     m.AssetUpdatedAt.Time,
	}
	if m.OwnerUserRef != nil {
		a.OwnerUserRef = m.OwnerUserRef
	}
	return a
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

func encodeCursor(t time.Time, id uuid.UUID) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, errors.New("bad cursor shape")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return t, id, nil
}

func dedupeTags(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func int32Or(p *int, def int32) int32 {
	if p == nil {
		return def
	}
	return int32(*p)
}

func isFKError(err error, constraint string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), constraint)
}

// ---------------------------------------------------------------------------
// Compile-time assertion: catches openapi-codegen signature drift.
// ---------------------------------------------------------------------------

var _ interface {
	ListPosts(context.Context, openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error)
	CreatePost(context.Context, openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error)
	GetPost(context.Context, openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error)
	UpdatePost(context.Context, openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error)
	DeletePost(context.Context, openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error)
	AddPostAsset(context.Context, openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error)
	RemovePostAsset(context.Context, openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error)
	ListPostAcls(context.Context, openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error)
	AddPostAcl(context.Context, openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error)
	RemovePostAcl(context.Context, openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error)
} = (*Handler)(nil)
