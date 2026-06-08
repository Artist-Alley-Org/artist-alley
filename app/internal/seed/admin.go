// Admin handlers for the demo-seed loader endpoints. NOT for
// general operator use — gated on system.admin; not surfaced
// in the admin UI. The apply-side script (see
// `seed/SEED_INSTRUCTIONS.md`) calls these to backfill
// timestamps + forge per-post reviewer comments after the
// regular API surface has done its work.

package seed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditTimestampsHook is the cross-package contract for the
// timestamps-backfilled audit event. Boot wires it to
// audit.Recorder.SeedTimestampsBackfilled. nil-safe.
type AuditTimestampsHook func(ctx context.Context, req *http.Request, actorUserRef int64, assetN, postN, commentN, skippedN int)

// AuditCommentHook is the cross-package contract for the
// comment-created audit. Boot wires to
// audit.Recorder.SeedCommentCreated. nil-safe.
type AuditCommentHook func(ctx context.Context, req *http.Request, actorUserRef int64, commentID, targetKind, targetID string, forgedAuthorRef int64)

// AdminHandler owns POST /admin/seed/timestamps + /comments.
type AdminHandler struct {
	pool       *pgxpool.Pool
	q          *Queries
	auditTimes AuditTimestampsHook
	auditComm  AuditCommentHook
}

func NewAdminHandler(pool *pgxpool.Pool, auditTimes AuditTimestampsHook, auditComm AuditCommentHook) *AdminHandler {
	return &AdminHandler{
		pool:       pool,
		q:          New(pool),
		auditTimes: auditTimes,
		auditComm:  auditComm,
	}
}

// --- timestamps backfill -----------------------------------------------

// TimestampKind enumerates the supported domain tables.
type TimestampKind string

const (
	TimestampKindAsset   TimestampKind = "asset"
	TimestampKindPost    TimestampKind = "post"
	TimestampKindComment TimestampKind = "comment"
)

// TimestampItem is one per-row backfill instruction.
type TimestampItem struct {
	Kind      TimestampKind
	ID        uuid.UUID
	CreatedAt time.Time
	UpdatedAt *time.Time // defaults to CreatedAt when nil
}

// TimestampsResult is the audit-friendly summary returned
// after a backfill call.
type TimestampsResult struct {
	AssetUpdated     int
	PostUpdated      int
	CommentUpdated   int
	SkippedUnknownID int
}

// ErrTimestampsBatchTooLarge protects the handler from
// unbounded payloads. Mirror the OpenAPI maxItems=1000 in code
// so a misbehaving client can't bypass the limit by skipping
// schema validation.
var ErrTimestampsBatchTooLarge = errors.New("seed: timestamps batch exceeds 1000 items")

// BackfillTimestamps applies the items to the three domain
// tables. Partitions by kind; one bulk UPDATE per kind; one
// transaction per call. Returns per-kind counts + skipped-
// unknown count (items whose id didn't match any row).
//
// Idempotent: re-running the same payload stamps the same
// timestamps twice → same end state.
func (h *AdminHandler) BackfillTimestamps(ctx context.Context, req *http.Request, actorUserRef int64, items []TimestampItem) (TimestampsResult, error) {
	if len(items) == 0 {
		return TimestampsResult{}, nil
	}
	if len(items) > 1000 {
		return TimestampsResult{}, ErrTimestampsBatchTooLarge
	}

	// Partition by kind + build the JSONB payloads expected by
	// the sqlc queries (jsonb_to_recordset).
	var (
		assetRows   []timestampJSON
		postRows    []timestampJSON
		commentRows []timestampJSON
	)
	for _, it := range items {
		upd := it.UpdatedAt
		if upd == nil {
			t := it.CreatedAt
			upd = &t
		}
		row := timestampJSON{
			ID:        it.ID,
			CreatedAt: it.CreatedAt,
			UpdatedAt: *upd,
		}
		switch it.Kind {
		case TimestampKindAsset:
			assetRows = append(assetRows, row)
		case TimestampKindPost:
			postRows = append(postRows, row)
		case TimestampKindComment:
			commentRows = append(commentRows, row)
		default:
			return TimestampsResult{}, fmt.Errorf("seed: unknown kind %q", it.Kind)
		}
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return TimestampsResult{}, fmt.Errorf("seed: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := h.q.WithTx(tx)

	var result TimestampsResult
	if len(assetRows) > 0 {
		payload, err := json.Marshal(assetRows)
		if err != nil {
			return TimestampsResult{}, err
		}
		n, err := qtx.BackfillAssetTimestamps(ctx, payload)
		if err != nil {
			return TimestampsResult{}, fmt.Errorf("seed: backfill assets: %w", err)
		}
		result.AssetUpdated = int(n)
		result.SkippedUnknownID += len(assetRows) - int(n)
	}
	if len(postRows) > 0 {
		payload, err := json.Marshal(postRows)
		if err != nil {
			return TimestampsResult{}, err
		}
		n, err := qtx.BackfillPostTimestamps(ctx, payload)
		if err != nil {
			return TimestampsResult{}, fmt.Errorf("seed: backfill posts: %w", err)
		}
		result.PostUpdated = int(n)
		result.SkippedUnknownID += len(postRows) - int(n)
	}
	if len(commentRows) > 0 {
		payload, err := json.Marshal(commentRows)
		if err != nil {
			return TimestampsResult{}, err
		}
		n, err := qtx.BackfillCommentTimestamps(ctx, payload)
		if err != nil {
			return TimestampsResult{}, fmt.Errorf("seed: backfill comments: %w", err)
		}
		result.CommentUpdated = int(n)
		result.SkippedUnknownID += len(commentRows) - int(n)
	}

	if err := tx.Commit(ctx); err != nil {
		return TimestampsResult{}, fmt.Errorf("seed: commit: %w", err)
	}

	if h.auditTimes != nil {
		h.auditTimes(ctx, req, actorUserRef,
			result.AssetUpdated, result.PostUpdated, result.CommentUpdated, result.SkippedUnknownID)
	}
	return result, nil
}

// timestampJSON is the on-wire shape passed to
// jsonb_to_recordset. Field names match the column names
// declared in queries.sql AS-clauses.
type timestampJSON struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// --- comment forge ----------------------------------------------------

// CommentTargetKind enumerates the comment polymorphism — same
// catalogue as the public comments table CHECK constraint.
type CommentTargetKind string

const (
	CommentTargetPost       CommentTargetKind = "post"
	CommentTargetAsset      CommentTargetKind = "asset"
	CommentTargetCollection CommentTargetKind = "collection"
)

// CommentInput is the typed argument shape for CreateComment.
type CommentInput struct {
	ID             *uuid.UUID // nil = server-generates
	TargetKind     CommentTargetKind
	TargetID       uuid.UUID
	AuthorUserRef  int64
	ParentID       *uuid.UUID
	Body           string
	BodyHTML       string
	AnnotationType *string
	AnnotationData []byte // raw JSON; nil = none
	CreatedAt      *time.Time
}

// CommentResult mirrors the existing comments-table row.
type CommentResult struct {
	ID               uuid.UUID
	TargetKind       string
	TargetID         uuid.UUID
	ParentID         *uuid.UUID
	RootID           uuid.UUID
	Depth            int32
	AuthorUserRef    *int64
	Body             string
	BodyHTML         string
	AnnotationType   *string
	AnnotationData   []byte
	LikeCount        int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
	AlreadyExisted   bool // true on idempotent re-run
}

// Common errors surfaced to the HTTP layer.
var (
	ErrTargetNotFound = errors.New("seed: comment target object not found")
	ErrAuthorNotFound = errors.New("seed: forged author not found")
)

// CreateComment forges a comment with the supplied author +
// optional created_at + optional stable id. Idempotent when the
// caller supplies an id — re-run returns the existing row with
// AlreadyExisted=true.
func (h *AdminHandler) CreateComment(ctx context.Context, req *http.Request, actorUserRef int64, in CommentInput) (CommentResult, error) {
	// Validate forged author + target object existence.
	if _, err := h.q.SeedAuthorExists(ctx, in.AuthorUserRef); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CommentResult{}, ErrAuthorNotFound
		}
		return CommentResult{}, err
	}
	if err := h.targetExists(ctx, in.TargetKind, in.TargetID); err != nil {
		return CommentResult{}, err
	}

	// Resolve thread placement (root_id + depth). Top-level
	// comments have parent_id NULL, root_id = self, depth = 0.
	// Replies inherit root_id from the parent + depth = parent.depth+1.
	commentID := uuid.New()
	if in.ID != nil {
		commentID = *in.ID
	}
	rootID := commentID
	depth := int32(0)
	parentPG := pgtype.UUID{}
	if in.ParentID != nil {
		parentPG = pgtype.UUID{Bytes: *in.ParentID, Valid: true}
		parent, err := h.q.SeedGetCommentParentInfo(ctx, parentPG)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return CommentResult{}, fmt.Errorf("seed: parent comment not found: %s", in.ParentID)
			}
			return CommentResult{}, err
		}
		rootID = uuid.UUID(parent.RootID.Bytes)
		depth = parent.Depth + 1
	}

	createdAt := time.Now()
	if in.CreatedAt != nil {
		createdAt = *in.CreatedAt
	}

	var annType *string
	if in.AnnotationType != nil && *in.AnnotationType != "" {
		annType = in.AnnotationType
	}

	row, err := h.q.SeedInsertComment(ctx, SeedInsertCommentParams{
		ID:             pgtype.UUID{Bytes: commentID, Valid: true},
		TargetKind:     string(in.TargetKind),
		TargetID:       pgtype.UUID{Bytes: in.TargetID, Valid: true},
		ParentID:       parentPG,
		RootID:         pgtype.UUID{Bytes: rootID, Valid: true},
		Depth:          depth,
		AuthorUserRef:  &in.AuthorUserRef,
		Body:           in.Body,
		BodyHtml:       in.BodyHTML,
		AnnotationType: annType,
		AnnotationData: in.AnnotationData,
		CreatedAt:      pgtype.Timestamptz{Time: createdAt, Valid: true},
	})
	if err != nil {
		// ON CONFLICT DO NOTHING returns pgx.ErrNoRows here —
		// surface as the idempotent-already-exists path below.
		if !errors.Is(err, pgx.ErrNoRows) {
			return CommentResult{}, fmt.Errorf("seed: insert comment: %w", err)
		}
	}
	already := errors.Is(err, pgx.ErrNoRows)
	if already {
		// Fetch the existing row so the response shape is
		// consistent with a fresh insert.
		existing, err := h.q.SeedGetCommentByID(ctx, pgtype.UUID{Bytes: commentID, Valid: true})
		if err != nil {
			return CommentResult{}, fmt.Errorf("seed: re-read after conflict: %w", err)
		}
		return commentRowToResult(existing, true), nil
	}

	// Audit only on actual insert — re-runs are no-ops at the
	// audit layer too (avoids audit-log spam from re-running
	// apply scripts).
	if h.auditComm != nil {
		h.auditComm(ctx, req, actorUserRef,
			commentID.String(), string(in.TargetKind), in.TargetID.String(),
			in.AuthorUserRef)
	}
	// Convert the insert RETURNING row to the result shape.
	return seedInsertRowToResult(row), nil
}

// --- helpers ---------------------------------------------------------

func (h *AdminHandler) targetExists(ctx context.Context, kind CommentTargetKind, id uuid.UUID) error {
	idPg := pgtype.UUID{Bytes: id, Valid: true}
	var (
		n   int32
		err error
	)
	switch kind {
	case CommentTargetPost:
		n, err = h.q.SeedPostExists(ctx, idPg)
	case CommentTargetAsset:
		n, err = h.q.SeedAssetExists(ctx, idPg)
	case CommentTargetCollection:
		n, err = h.q.SeedCollectionExists(ctx, idPg)
	default:
		return fmt.Errorf("seed: unknown comment target kind %q", kind)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTargetNotFound
		}
		return err
	}
	if n != 1 {
		return ErrTargetNotFound
	}
	return nil
}

func seedInsertRowToResult(r Comment) CommentResult {
	out := CommentResult{
		ID:             uuid.UUID(r.ID.Bytes),
		TargetKind:     r.TargetKind,
		TargetID:       uuid.UUID(r.TargetID.Bytes),
		RootID:         uuid.UUID(r.RootID.Bytes),
		Depth:          r.Depth,
		AuthorUserRef:  r.AuthorUserRef,
		Body:           r.Body,
		BodyHTML:       r.BodyHtml,
		AnnotationType: r.AnnotationType,
		AnnotationData: r.AnnotationData,
		LikeCount:      r.LikeCount,
		CreatedAt:      r.CreatedAt.Time,
		UpdatedAt:      r.UpdatedAt.Time,
		AlreadyExisted: false,
	}
	if r.ParentID.Valid {
		p := uuid.UUID(r.ParentID.Bytes)
		out.ParentID = &p
	}
	return out
}

func commentRowToResult(r Comment, already bool) CommentResult {
	out := CommentResult{
		ID:             uuid.UUID(r.ID.Bytes),
		TargetKind:     r.TargetKind,
		TargetID:       uuid.UUID(r.TargetID.Bytes),
		RootID:         uuid.UUID(r.RootID.Bytes),
		Depth:          r.Depth,
		AuthorUserRef:  r.AuthorUserRef,
		Body:           r.Body,
		BodyHTML:       r.BodyHtml,
		AnnotationType: r.AnnotationType,
		AnnotationData: r.AnnotationData,
		LikeCount:      r.LikeCount,
		CreatedAt:      r.CreatedAt.Time,
		UpdatedAt:      r.UpdatedAt.Time,
		AlreadyExisted: already,
	}
	if r.ParentID.Valid {
		p := uuid.UUID(r.ParentID.Bytes)
		out.ParentID = &p
	}
	return out
}
