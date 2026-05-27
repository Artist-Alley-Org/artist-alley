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

type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, logger *slog.Logger) *Handler {
	return &Handler{Pool: pool, Logger: logger}
}

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

	// Resolve parent + root + depth.
	var parentID pgtype.UUID
	var rootID pgtype.UUID
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

	q := New(h.Pool)
	row, err := q.CreateComment(ctx, CreateCommentParams{
		TargetKind:     "post",
		TargetID:       pgPostID,
		ParentID:       parentID,
		RootID:         rootID, // zero for now; will set self below for top-level
		Depth:          depth,
		AuthorUserRef:  caller.UserRef,
		Body:           body,
		BodyHtml:       "", // Phase 1.13.F-3 ships a server-side sanitiser; for now we render `body` client-side
		AnnotationType: nil,
		AnnotationData: nil,
	})
	if err != nil {
		return nil, fmt.Errorf("social: create comment: %w", err)
	}
	// For top-level comments, set root_id = id post-insert.
	if !rootID.Valid {
		if err := q.SetCommentRootSelf(ctx, row.ID); err != nil {
			return nil, fmt.Errorf("social: set root self: %w", err)
		}
		row.RootID = row.ID
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
// Helpers
// ---------------------------------------------------------------------------

func (h *Handler) postExists(ctx context.Context, id pgtype.UUID) (bool, error) {
	var exists bool
	err := h.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM posts WHERE id = $1 AND deleted_at IS NULL)`, id,
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
} = (*Handler)(nil)
