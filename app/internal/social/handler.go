// Package social implements the likes + comments HTTP surface on top
// of the polymorphic data plane from Phase 1.13.D-4. The schema and
// triggers are in migration 00020; this file is the handler layer.
//
// Endpoints (rooted under /api/v1):
//
//   GET    /posts/{id}/like              — whether the caller has liked
//   POST   /posts/{id}/like              — idempotent like
//   DELETE /posts/{id}/like              — unlike (404 if no like)
//   GET    /posts/{id}/comments          — list thread, cursor-paginated
//   POST   /posts/{id}/comments          — create (optionally a reply)
//   DELETE /comments/{id}                — soft-delete (own or moderator)
//
// Capability gates (seeded in 00020):
//   - posts.like           — Base default
//   - posts.comment        — Base default
//   - comments.delete.own  — Base default (gates the own-delete branch)
//   - comments.delete.any  — Admin only (moderator override)
//
// All counter maintenance happens in DB triggers; this handler never
// touches posts.like_count or posts.comment_count directly.
package social

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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

const (
	CapPostsLike          = "posts.like"
	CapPostsComment       = "posts.comment"
	CapCommentsDeleteOwn  = "comments.delete.own"
	CapCommentsDeleteAny  = "comments.delete.any"
	CapSystemAdmin        = "system.admin"
)

const maxListLimit = 200

// Cache domains for the social graph (Phase 1.17.G2). Each domain
// invalidates independently so a follow doesn't blow away the block
// cache, and vice versa. cache.Registry NOTIFY broadcasts every
// invalidation across federated peers — same federation-prep
// pattern the rest of the codebase uses.
const (
	// cacheDomainFollowEdge holds boolean follow membership, keyed
	// by "<follower>:<followee>". Single hottest social-graph read:
	// posts.handler.go consults it for every visibility='followers'
	// post served; the browse-feed Following filter consults it per
	// page render. ~1 byte per entry → 50k entries comfortable.
	cacheDomainFollowEdge = "social.follow_edge"

	// cacheDomainBlockEdge holds bidirectional block presence, keyed
	// by the canonical "<min(a,b)>:<max(a,b)>" pair so the cache hit
	// is identical regardless of which side asks. Consumers ALWAYS
	// check the bidirectional gate; the per-direction split in
	// GetUserRelationship intentionally bypasses this cache.
	cacheDomainBlockEdge = "social.block_edge"

	// cacheDomainFollowerCount + cacheDomainFollowingCount feed
	// profile-page badges and (in I2+) digest selection. Keyed by
	// the subject user's ref.
	cacheDomainFollowerCount  = "social.follower_count"
	cacheDomainFollowingCount = "social.following_count"
)

type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	registry        *cache.Registry
	followEdge      *cache.Cache[bool]
	blockEdge       *cache.Cache[bool]
	followerCount   *cache.Cache[int64]
	followingCount  *cache.Cache[int64]
}

func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{Pool: pool, Logger: logger, registry: registry}
	if registry != nil {
		// Edge caches dominate the working set (one entry per
		// active (a,b) pair the request stream touches); counts
		// are bounded by the active-user population.
		h.followEdge = cache.Register[bool](registry, cacheDomainFollowEdge, 50_000)
		h.blockEdge = cache.Register[bool](registry, cacheDomainBlockEdge, 20_000)
		h.followerCount = cache.Register[int64](registry, cacheDomainFollowerCount, 10_000)
		h.followingCount = cache.Register[int64](registry, cacheDomainFollowingCount, 10_000)
	}
	return h
}

// --- cache key helpers ----------------------------------------------------

func followKey(follower, followee int64) string {
	return strconv.FormatInt(follower, 10) + ":" + strconv.FormatInt(followee, 10)
}

// canonicalBlockKey produces the same string regardless of argument
// order so HasBlockBetween's symmetric semantics get a single cache
// row per pair (rather than two stale rows after one invalidation).
func canonicalBlockKey(a, b int64) string {
	lo, hi := a, b
	if lo > hi {
		lo, hi = b, a
	}
	return strconv.FormatInt(lo, 10) + ":" + strconv.FormatInt(hi, 10)
}

func countKey(ref int64) string { return strconv.FormatInt(ref, 10) }

// ---------------------------------------------------------------------------
// Likes
// ---------------------------------------------------------------------------

func (h *Handler) GetPostLike(
	ctx context.Context,
	req openapi.GetPostLikeRequestObject,
) (openapi.GetPostLikeResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.GetPostLike401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	// Confirm the post exists so we can return a proper 404 rather
	// than silently saying "not liked" for a missing target.
	if exists, err := h.postExists(ctx, pgID); err != nil {
		return nil, fmt.Errorf("social: post check: %w", err)
	} else if !exists {
		return openapi.GetPostLike404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
		}, nil
	}
	liked, err := New(h.Pool).HasUserLikedTarget(ctx, HasUserLikedTargetParams{
		TargetKind: "post",
		TargetID:   pgID,
		RsUserID:   caller.UserRef,
	})
	if err != nil {
		return nil, fmt.Errorf("social: has liked: %w", err)
	}
	return openapi.GetPostLike200JSONResponse(openapi.PostLikeState{Liked: liked}), nil
}

func (h *Handler) LikePost(
	ctx context.Context,
	req openapi.LikePostRequestObject,
) (openapi.LikePostResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.LikePost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapPostsLike) && !caller.Can(CapSystemAdmin) {
		return openapi.LikePost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "posts.like capability required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if exists, err := h.postExists(ctx, pgID); err != nil {
		return nil, fmt.Errorf("social: post check: %w", err)
	} else if !exists {
		return openapi.LikePost404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
		}, nil
	}
	if err := New(h.Pool).LikeTarget(ctx, LikeTargetParams{
		TargetKind: "post",
		TargetID:   pgID,
		RsUserID:   caller.UserRef,
	}); err != nil {
		return nil, fmt.Errorf("social: like: %w", err)
	}
	return openapi.LikePost204Response{}, nil
}

func (h *Handler) UnlikePost(
	ctx context.Context,
	req openapi.UnlikePostRequestObject,
) (openapi.UnlikePostResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.UnlikePost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	rows, err := New(h.Pool).UnlikeTarget(ctx, UnlikeTargetParams{
		TargetKind: "post",
		TargetID:   pgID,
		RsUserID:   caller.UserRef,
	})
	if err != nil {
		return nil, fmt.Errorf("social: unlike: %w", err)
	}
	if rows == 0 {
		return openapi.UnlikePost404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "no like to remove"},
		}, nil
	}
	return openapi.UnlikePost204Response{}, nil
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

func (h *Handler) ListPostComments(
	ctx context.Context,
	req openapi.ListPostCommentsRequestObject,
) (openapi.ListPostCommentsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListPostComments401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if exists, err := h.postExists(ctx, pgID); err != nil {
		return nil, fmt.Errorf("social: post check: %w", err)
	} else if !exists {
		return openapi.ListPostComments404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
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

	var cursorCreatedAt pgtype.Timestamptz
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		t, err := decodeCommentCursor(*req.Params.Cursor)
		if err != nil {
			// Malformed cursor is a client error — return 401 path
			// (we have no 400 in this spec; fall back via the
			// general 401 since auth was OK but data was bad).
			// Actually the simplest defensible answer: empty page.
			cursorCreatedAt = pgtype.Timestamptz{Valid: false}
			_ = err
		} else {
			cursorCreatedAt = pgtype.Timestamptz{Time: t, Valid: true}
		}
	}

	// thread_limit is the number of root threads to return; we ask for
	// one extra to know whether there's a next page.
	rows, err := New(h.Pool).ListThreadForTarget(ctx, ListThreadForTargetParams{
		TargetKind:           "post",
		TargetID:             pgID,
		CursorRootCreatedAt:  cursorCreatedAt,
		ThreadLimit:          limit + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("social: list thread: %w", err)
	}

	// The query orders root_created_at DESC then depth/created_at
	// within each root, so we can detect "more pages" by collecting
	// distinct root_ids and stopping at the (limit+1)th.
	out := make([]openapi.Comment, 0, len(rows))
	rootsSeen := map[uuid.UUID]struct{}{}
	var lastRootCreatedAt time.Time
	rootCount := 0
	for _, r := range rows {
		rootID := uuid.UUID(r.RootID.Bytes)
		if _, ok := rootsSeen[rootID]; !ok {
			if rootCount == int(limit) {
				// We've now seen limit+1th root — stop, but record the
				// previous root's timestamp as the next cursor anchor.
				break
			}
			rootsSeen[rootID] = struct{}{}
			rootCount++
		}
		out = append(out, commentRowToAPI(r))
		// The created_at on the LAST included row will be on the
		// last included root (since rows within a root come together).
		// For cursor purposes we want the root's created_at; the root
		// itself is always present in the result so we capture its
		// timestamp when we see it.
		if uuid.UUID(r.ID.Bytes) == rootID {
			lastRootCreatedAt = r.CreatedAt.Time
		}
	}

	resp := openapi.CommentList{Items: out}
	if rootCount > int(limit)-1 && !lastRootCreatedAt.IsZero() && len(rows) > len(out) {
		c := encodeCommentCursor(lastRootCreatedAt)
		resp.NextCursor = &c
	}
	return openapi.ListPostComments200JSONResponse(resp), nil
}

func (h *Handler) CreatePostComment(
	ctx context.Context,
	req openapi.CreatePostCommentRequestObject,
) (openapi.CreatePostCommentResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.CreatePostComment401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapPostsComment) && !caller.Can(CapSystemAdmin) {
		return openapi.CreatePostComment403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "posts.comment capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreatePostComment400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	body := strings.TrimSpace(req.Body.Body)
	if body == "" {
		return openapi.CreatePostComment400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "body is required"},
		}, nil
	}
	pgPostID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if exists, err := h.postExists(ctx, pgPostID); err != nil {
		return nil, fmt.Errorf("social: post check: %w", err)
	} else if !exists {
		return openapi.CreatePostComment404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
		}, nil
	}

	// Generate the new comment's id up front so a top-level comment
	// can satisfy the NOT NULL root_id constraint with root_id = id at
	// INSERT time. Replies copy the parent's root_id.
	newID := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	var parentID pgtype.UUID
	rootID := newID
	depth := int32(0)
	if req.Body.ParentId != nil {
		parentID = pgtype.UUID{Bytes: uuid.UUID(*req.Body.ParentId), Valid: true}
		parentRow, err := New(h.Pool).GetComment(ctx, parentID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return openapi.CreatePostComment404JSONResponse{
					NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "parent comment not found"},
				}, nil
			}
			return nil, fmt.Errorf("social: get parent: %w", err)
		}
		// Parent must be on the same target.
		if parentRow.TargetKind != "post" || parentRow.TargetID != pgPostID {
			return openapi.CreatePostComment400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "parent not on this post"},
			}, nil
		}
		rootID = parentRow.RootID
		depth = parentRow.Depth + 1
	}

	row, err := New(h.Pool).CreateComment(ctx, CreateCommentParams{
		ID:             newID,
		TargetKind:     "post",
		TargetID:       pgPostID,
		ParentID:       parentID,
		RootID:         rootID,
		Depth:          depth,
		AuthorUserRef:  caller.UserRef,
		Body:           body,
		BodyHtml:       "", // server-side markdown sanitiser is a later phase
		AnnotationType: nil,
		AnnotationData: nil,
	})
	if err != nil {
		return nil, fmt.Errorf("social: create comment: %w", err)
	}
	return openapi.CreatePostComment201JSONResponse(commentRowToAPI(row)), nil
}

func (h *Handler) DeleteComment(
	ctx context.Context,
	req openapi.DeleteCommentRequestObject,
) (openapi.DeleteCommentResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.DeleteComment401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	row, err := New(h.Pool).GetComment(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.DeleteComment404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "comment not found"},
			}, nil
		}
		return nil, fmt.Errorf("social: get comment: %w", err)
	}
	// Already soft-deleted — treat as 404 to match REST semantics.
	if row.DeletedAt.Valid {
		return openapi.DeleteComment404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "comment not found"},
		}, nil
	}

	// Authorization: own (with the .own cap) OR any (moderator) OR
	// system.admin.
	isOwn := row.AuthorUserRef == caller.UserRef
	canDeleteOwn := isOwn && caller.Can(CapCommentsDeleteOwn)
	canDeleteAny := caller.Can(CapCommentsDeleteAny) || caller.Can(CapSystemAdmin)
	if !canDeleteOwn && !canDeleteAny {
		return openapi.DeleteComment403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "cannot delete this comment"},
		}, nil
	}

	rows, err := New(h.Pool).SoftDeleteComment(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("social: soft delete comment: %w", err)
	}
	if rows == 0 {
		return openapi.DeleteComment404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "comment not found"},
		}, nil
	}
	return openapi.DeleteComment204Response{}, nil
}

// ---------------------------------------------------------------------------
// Whiteboards — top-level comments with annotation_type='whiteboard'
// ---------------------------------------------------------------------------
//
// Whiteboards are stored as comments so they inherit threading (reply
// to a sketch), likes, soft-delete, federation, and audit for free.
// Migration 00029 extended the comments.annotation_type CHECK to
// include 'whiteboard'; this is the HTTP surface that drives it.
//
// Capability: posts.comment (same gate as a regular comment).
// Whiteboards ARE comments at the storage layer — if we ever want to
// gate them separately a future migration can split the capability
// without restructuring the data.

func (h *Handler) ListPostWhiteboards(
	ctx context.Context,
	req openapi.ListPostWhiteboardsRequestObject,
) (openapi.ListPostWhiteboardsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListPostWhiteboards401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgPostID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if exists, err := h.postExists(ctx, pgPostID); err != nil {
		return nil, fmt.Errorf("social: post check: %w", err)
	} else if !exists {
		return openapi.ListPostWhiteboards404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
		}, nil
	}
	rows, err := New(h.Pool).ListWhiteboardsForPost(ctx, pgPostID)
	if err != nil {
		return nil, fmt.Errorf("social: list whiteboards: %w", err)
	}
	out := make(openapi.ListPostWhiteboards200JSONResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, commentRowToAPI(r))
	}
	return out, nil
}

func (h *Handler) CreatePostWhiteboard(
	ctx context.Context,
	req openapi.CreatePostWhiteboardRequestObject,
) (openapi.CreatePostWhiteboardResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.CreatePostWhiteboard401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapPostsComment) && !caller.Can(CapSystemAdmin) {
		return openapi.CreatePostWhiteboard403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "posts.comment capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreatePostWhiteboard400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	// Marshal the content payload back to JSON for storage. The
	// generated type is the validated WhiteboardContent struct; re-
	// marshalling preserves the schema-validated shape without us
	// re-walking each field.
	contentBytes, err := json.Marshal(req.Body.Content)
	if err != nil {
		return openapi.CreatePostWhiteboard400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid content payload"},
		}, nil
	}
	// Light client-side-validation backstop — we trust openapi-codegen's
	// validation for shape, but defend against an empty layers array
	// slipping through (the schema requires minItems=1 but a malicious
	// client could still send raw bytes via a different path later).
	if len(contentBytes) < 2 {
		return openapi.CreatePostWhiteboard400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "content is required"},
		}, nil
	}

	pgPostID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if exists, err := h.postExists(ctx, pgPostID); err != nil {
		return nil, fmt.Errorf("social: post check: %w", err)
	} else if !exists {
		return openapi.CreatePostWhiteboard404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "post not found"},
		}, nil
	}

	title := ""
	if req.Body.Title != nil {
		title = strings.TrimSpace(*req.Body.Title)
	}

	// Whiteboards are top-level by definition — we don't reply to a
	// whiteboard with another whiteboard. Replies are normal comments
	// threaded under it via parent_id (handled by the regular
	// CreatePostComment handler).
	newID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	annotationType := "whiteboard"
	row, err := New(h.Pool).CreateComment(ctx, CreateCommentParams{
		ID:             newID,
		TargetKind:     "post",
		TargetID:       pgPostID,
		ParentID:       pgtype.UUID{},
		RootID:         newID,
		Depth:          0,
		AuthorUserRef:  caller.UserRef,
		Body:           title, // title lives in body for whiteboards
		BodyHtml:       "",
		AnnotationType: &annotationType,
		AnnotationData: contentBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("social: create whiteboard: %w", err)
	}
	return openapi.CreatePostWhiteboard201JSONResponse(commentRowToAPI(row)), nil
}

// ---------------------------------------------------------------------------
// Text-range annotations — doc-viewer review tools.
// ---------------------------------------------------------------------------
//
// Same storage trick as whiteboards: a text annotation is a top-level
// comments row with annotation_type='text-range'. annotation_data
// carries { style, color, start_line, start_col, end_line, end_col,
// resolved }. Replies on annotations thread under it via the regular
// comment thread query — only the top-level anchors surface here.

func (h *Handler) ListAssetTextAnnotations(
	ctx context.Context,
	req openapi.ListAssetTextAnnotationsRequestObject,
) (openapi.ListAssetTextAnnotationsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListAssetTextAnnotations401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgAssetID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if exists, err := h.assetExists(ctx, pgAssetID); err != nil {
		return nil, fmt.Errorf("social: asset check: %w", err)
	} else if !exists {
		return openapi.ListAssetTextAnnotations404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}, nil
	}
	rows, err := New(h.Pool).ListTextAnnotationsForAsset(ctx, pgAssetID)
	if err != nil {
		return nil, fmt.Errorf("social: list text annotations: %w", err)
	}
	out := make(openapi.ListAssetTextAnnotations200JSONResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, commentRowToAPI(r))
	}
	return out, nil
}

func (h *Handler) CreateAssetTextAnnotation(
	ctx context.Context,
	req openapi.CreateAssetTextAnnotationRequestObject,
) (openapi.CreateAssetTextAnnotationResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.CreateAssetTextAnnotation401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapPostsComment) && !caller.Can(CapSystemAdmin) {
		return openapi.CreateAssetTextAnnotation403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "posts.comment capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreateAssetTextAnnotation400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgAssetID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	if exists, err := h.assetExists(ctx, pgAssetID); err != nil {
		return nil, fmt.Errorf("social: asset check: %w", err)
	} else if !exists {
		return openapi.CreateAssetTextAnnotation404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}, nil
	}
	anchorBytes, err := json.Marshal(req.Body.Anchor)
	if err != nil {
		return openapi.CreateAssetTextAnnotation400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid anchor payload"},
		}, nil
	}
	body := ""
	if req.Body.Body != nil {
		body = *req.Body.Body
	}
	newID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	annotationType := "text-range"
	row, err := New(h.Pool).CreateComment(ctx, CreateCommentParams{
		ID:             newID,
		TargetKind:     "asset",
		TargetID:       pgAssetID,
		ParentID:       pgtype.UUID{},
		RootID:         newID,
		Depth:          0,
		AuthorUserRef:  caller.UserRef,
		Body:           body,
		BodyHtml:       "",
		AnnotationType: &annotationType,
		AnnotationData: anchorBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("social: create text annotation: %w", err)
	}
	return openapi.CreateAssetTextAnnotation201JSONResponse(commentRowToAPI(row)), nil
}

func (h *Handler) UpdateTextAnnotation(
	ctx context.Context,
	req openapi.UpdateTextAnnotationRequestObject,
) (openapi.UpdateTextAnnotationResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.UpdateTextAnnotation401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdateTextAnnotation400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	existing, err := New(h.Pool).GetComment(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateTextAnnotation404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "annotation not found"},
			}, nil
		}
		return nil, fmt.Errorf("social: load annotation: %w", err)
	}
	if existing.DeletedAt.Valid {
		return openapi.UpdateTextAnnotation404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "annotation not found"},
		}, nil
	}
	if existing.AnnotationType == nil || *existing.AnnotationType != "text-range" {
		return openapi.UpdateTextAnnotation400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "not a text-range annotation"},
		}, nil
	}
	// Author can always update; moderators (comments.delete.any holders)
	// can also update — we treat the moderator cap as "manage any
	// comment" for now since we don't have a separate update gate.
	isAuthor := existing.AuthorUserRef == caller.UserRef
	isMod := caller.Can(CapCommentsDeleteAny) || caller.Can(CapSystemAdmin)
	if !isAuthor && !isMod {
		return openapi.UpdateTextAnnotation403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "not the author"},
		}, nil
	}

	// Resolve next anchor — caller can pass a full anchor or omit it
	// to keep the existing blob.
	anchorBytes := existing.AnnotationData
	if req.Body.Anchor != nil {
		nb, err := json.Marshal(req.Body.Anchor)
		if err != nil {
			return openapi.UpdateTextAnnotation400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid anchor payload"},
			}, nil
		}
		anchorBytes = nb
	}

	params := UpdateAnnotationDataParams{
		ID:             pgID,
		AnnotationData: anchorBytes,
	}
	if req.Body.Body != nil {
		params.Body = req.Body.Body
	}
	row, err := New(h.Pool).UpdateAnnotationData(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateTextAnnotation404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "annotation not found"},
			}, nil
		}
		return nil, fmt.Errorf("social: update annotation: %w", err)
	}
	return openapi.UpdateTextAnnotation200JSONResponse(commentRowToAPI(row)), nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *Handler) postExists(ctx context.Context, id pgtype.UUID) (bool, error) {
	var exists bool
	err := h.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM posts WHERE id = $1 AND deleted_at IS NULL)`, id,
	).Scan(&exists)
	return exists, err
}

// assetExists mirrors postExists for the text-annotation handlers —
// asserts the target asset row is present and not soft-deleted before
// we accept a write. Returns false (not an error) on a clean miss so
// the caller can surface a 404.
func (h *Handler) assetExists(ctx context.Context, id pgtype.UUID) (bool, error) {
	var exists bool
	err := h.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1)`, id,
	).Scan(&exists)
	return exists, err
}

// commentRowToAPI converts the sqlc-generated Comment model (used by
// both ListThreadForTarget and CreateComment — sqlc returns the same
// shape for both queries) into the openapi response shape.
func commentRowToAPI(r Comment) openapi.Comment {
	out := openapi.Comment{
		Id:            openapi_types.UUID(r.ID.Bytes),
		TargetKind:    openapi.CommentTargetKind(r.TargetKind),
		TargetId:      openapi_types.UUID(r.TargetID.Bytes),
		RootId:        openapi_types.UUID(r.RootID.Bytes),
		Depth:         int(r.Depth),
		AuthorUserRef: r.AuthorUserRef,
		Body:          r.Body,
		BodyHtml:      r.BodyHtml,
		LikeCount:     r.LikeCount,
		CreatedAt:     r.CreatedAt.Time,
		UpdatedAt:     r.UpdatedAt.Time,
	}
	if r.ParentID.Valid {
		v := openapi_types.UUID(r.ParentID.Bytes)
		out.ParentId = &v
	}
	if r.AnnotationType != nil {
		v := openapi.CommentAnnotationType(*r.AnnotationType)
		out.AnnotationType = &v
	}
	if len(r.AnnotationData) > 0 {
		// JSONB → generic map. The schema is per-annotation_type and
		// validated client-side; we don't want to bake every variant
		// into the Go type system.
		var data map[string]interface{}
		if err := json.Unmarshal(r.AnnotationData, &data); err == nil && data != nil {
			out.AnnotationData = &data
		}
	}
	if r.EditedAt.Valid {
		t := r.EditedAt.Time
		out.EditedAt = &t
	}
	return out
}

// Cursor: just the root_created_at — the thread query advances by
// strictly-older roots. base64-url-encoded RFC3339Nano so it survives
// in URL params cleanly.
func encodeCommentCursor(t time.Time) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano)))
}

func decodeCommentCursor(s string) (time.Time, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, string(raw))
}

// Compile-time strict-server interface assertion.
var _ interface {
	GetPostLike(context.Context, openapi.GetPostLikeRequestObject) (openapi.GetPostLikeResponseObject, error)
	LikePost(context.Context, openapi.LikePostRequestObject) (openapi.LikePostResponseObject, error)
	UnlikePost(context.Context, openapi.UnlikePostRequestObject) (openapi.UnlikePostResponseObject, error)
	ListPostComments(context.Context, openapi.ListPostCommentsRequestObject) (openapi.ListPostCommentsResponseObject, error)
	CreatePostComment(context.Context, openapi.CreatePostCommentRequestObject) (openapi.CreatePostCommentResponseObject, error)
	DeleteComment(context.Context, openapi.DeleteCommentRequestObject) (openapi.DeleteCommentResponseObject, error)
	ListPostWhiteboards(context.Context, openapi.ListPostWhiteboardsRequestObject) (openapi.ListPostWhiteboardsResponseObject, error)
	CreatePostWhiteboard(context.Context, openapi.CreatePostWhiteboardRequestObject) (openapi.CreatePostWhiteboardResponseObject, error)
} = (*Handler)(nil)
