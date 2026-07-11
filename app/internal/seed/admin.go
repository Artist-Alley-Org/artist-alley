// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
)

// AuditTimestampsHook is the cross-package contract for the
// timestamps-backfilled audit event. Boot wires it to
// audit.Recorder.SeedTimestampsBackfilled. nil-safe.
type AuditTimestampsHook func(ctx context.Context, req *http.Request, actorUserRef int64, assetN, postN, commentN, skippedN int)

// AuditCommentHook is the cross-package contract for the
// comment-created audit. Boot wires to
// audit.Recorder.SeedCommentCreated. nil-safe.
type AuditCommentHook func(ctx context.Context, req *http.Request, actorUserRef int64, commentID, targetKind, targetID string, forgedAuthorRef int64)

// AuditUserHook is the cross-package contract for the
// user-created audit. Boot wires to
// audit.Recorder.SeedUserCreated. nil-safe.
type AuditUserHook func(ctx context.Context, req *http.Request, actorUserRef int64, userRef int64, username string)

// PasswordHasher is the cross-package contract the seed
// handler uses to hash plaintext passwords supplied via
// SeedUserRequest.Password. Boot wires it to a closure over
// auth.HashPassword + the legacy scramble key (the same path
// the setup flow uses for the initial admin). nil-safe — when
// not wired AND a password is supplied, the handler returns a
// clear error rather than silently writing the plaintext.
type PasswordHasher func(plaintext string) (string, error)

// AdminHandler owns POST /admin/seed/timestamps + /comments + /users.
type AdminHandler struct {
	pool       *pgxpool.Pool
	q          *Queries
	auditTimes AuditTimestampsHook
	auditComm  AuditCommentHook
	auditUser  AuditUserHook
	hashPwd    PasswordHasher

	// recorder is the tx-bound audit sink for federation.user.
	// key_generated events (1.22.I-b). Distinct from the hooks
	// above — those are pool-bound after-the-fact best-effort
	// logging; this one needs to commit atomically with the
	// keypair insert. nil-safe: when unwired the keypair still
	// lands; only the audit row is skipped.
	recorder *audit.Recorder
}

func NewAdminHandler(
	pool *pgxpool.Pool,
	auditTimes AuditTimestampsHook,
	auditComm AuditCommentHook,
	auditUser AuditUserHook,
	hashPwd PasswordHasher,
	recorder *audit.Recorder,
) *AdminHandler {
	return &AdminHandler{
		pool:       pool,
		q:          New(pool),
		auditTimes: auditTimes,
		auditComm:  auditComm,
		auditUser:  auditUser,
		hashPwd:    hashPwd,
		recorder:   recorder,
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

// --- user forge ------------------------------------------------------

// UserInput is the typed argument shape for CreateUser.
// Username is the idempotency key — re-runs with the same
// username return the existing row with AlreadyExisted=true.
type UserInput struct {
	Username  string
	Fullname  *string
	Email     *string
	Password  *string // nil → user has no password (can't log in)
	Usergroup *int64  // nil → defaults to 2 (regular user) inside the query
	Approved  bool    // defaults to true in handler when not explicitly false
	CreatedAt *time.Time
}

// UserResult mirrors the existing user row.
type UserResult struct {
	Ref            int64
	Username       string
	Fullname       *string
	Email          *string
	Usergroup      *int64
	Approved       bool
	CreatedAt      time.Time
	AlreadyExisted bool
}

// ErrPasswordHasherNotWired is returned when the caller supplies
// a plaintext password but the handler wasn't constructed with a
// hasher. Fails loud rather than silently writing plaintext.
var ErrPasswordHasherNotWired = errors.New("seed: password hasher not wired; cannot persist password")

// CreateUser forges a user with the supplied username + optional
// password + optional created_at. Idempotent on username — re-
// runs return the existing row with AlreadyExisted=true.
//
// When Password is non-nil, the configured PasswordHasher
// hashes it; the on-disk column carries the hash (matches the
// regular CreateUser path). When nil, the password column is
// NULL → the user can't log in but can be referenced as an
// actor on posts / comments / activities.
func (h *AdminHandler) CreateUser(ctx context.Context, req *http.Request, actorUserRef int64, in UserInput) (UserResult, error) {
	if strings.TrimSpace(in.Username) == "" {
		return UserResult{}, errors.New("seed: username is required")
	}

	var passwordHash *string
	if in.Password != nil && *in.Password != "" {
		if h.hashPwd == nil {
			return UserResult{}, ErrPasswordHasherNotWired
		}
		hash, err := h.hashPwd(*in.Password)
		if err != nil {
			return UserResult{}, fmt.Errorf("seed: hash password: %w", err)
		}
		passwordHash = &hash
	}

	usergroup := in.Usergroup
	if usergroup == nil {
		ug := int64(2) // regular user — admin-tier (3) is for the bootstrap admin only
		usergroup = &ug
	}

	approved := int64(1)
	if !in.Approved {
		approved = 0
	}

	created := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	if in.CreatedAt != nil {
		created = pgtype.Timestamptz{Time: *in.CreatedAt, Valid: true}
	}

	username := in.Username

	// Wrap the whole create flow in a transaction. Before 1.22.I-b
	// this was a pool-direct INSERT, but the federation keypair
	// has to land atomically with the user row — a user committed
	// without a key would violate the I-c/I-e/I-f precondition
	// that every user has exactly one current key. ON CONFLICT
	// DO NOTHING + the EnsureCurrentForUser idempotency check
	// together also backfill keys for users that pre-existed the
	// seed call.
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return UserResult{}, fmt.Errorf("seed: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := h.q.WithTx(tx)

	row, err := qtx.SeedInsertUser(ctx, SeedInsertUserParams{
		Username:  &username,
		Password:  passwordHash,
		Fullname:  in.Fullname,
		Email:     in.Email,
		Usergroup: usergroup,
		Approved:  approved,
		Created:   created,
	})
	var userRef int64
	already := false
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return UserResult{}, fmt.Errorf("seed: insert user: %w", err)
		}
		// ON CONFLICT DO NOTHING returned 0 rows — re-fetch the
		// existing row inside the same tx so the result is
		// consistent and EnsureCurrentForUser can backfill a key
		// if this pre-existing user is missing one.
		already = true
		existing, err := qtx.SeedGetUserByUsername(ctx, &username)
		if err != nil {
			return UserResult{}, fmt.Errorf("seed: re-read after username conflict: %w", err)
		}
		userRef = existing.Ref
		row = SeedInsertUserRow{
			Ref:       existing.Ref,
			Username:  existing.Username,
			Fullname:  existing.Fullname,
			Email:     existing.Email,
			Usergroup: existing.Usergroup,
			Approved:  existing.Approved,
			Created:   existing.Created,
		}
	} else {
		userRef = row.Ref
	}

	// Federation keypair (Phase 1.22.I-b). Idempotent — fires
	// the audit only when a fresh keypair is actually minted.
	// For users that pre-existed the seed call AND already had
	// a key, alreadyHadKey=true → silent no-op.
	ukq := userkeys.New(tx)
	alreadyHadKey, err := userkeys.EnsureCurrentForUser(ctx, ukq, userRef)
	if err != nil {
		return UserResult{}, fmt.Errorf("seed: ensure federation user key: %w", err)
	}
	if !alreadyHadKey && h.recorder != nil {
		// actor = the admin who called the seed endpoint, same
		// as SeedUserCreated. Same tx so the audit row commits
		// atomically with the key insert.
		actor := actorUserRef
		h.recorder.FederationUserKeyGenerated(ctx, audit.New(tx), userRef, &actor, 1, userkeys.Algorithm)
	}

	if err := tx.Commit(ctx); err != nil {
		return UserResult{}, fmt.Errorf("seed: commit: %w", err)
	}

	if already {
		return seedUserRowToResult(SeedGetUserByUsernameRow{
			Ref:       row.Ref,
			Username:  row.Username,
			Fullname:  row.Fullname,
			Email:     row.Email,
			Usergroup: row.Usergroup,
			Approved:  row.Approved,
			Created:   row.Created,
		}, true), nil
	}

	// Fresh-insert audit (existing pool-bound, fire-and-forget
	// hook — distinct from the tx-bound recorder above).
	if h.auditUser != nil {
		h.auditUser(ctx, req, actorUserRef, row.Ref, username)
	}
	return seedInsertUserRowToResult(row), nil
}

func seedInsertUserRowToResult(r SeedInsertUserRow) UserResult {
	out := UserResult{
		Ref:            r.Ref,
		Fullname:       r.Fullname,
		Email:          r.Email,
		Usergroup:      r.Usergroup,
		Approved:       r.Approved == 1,
		AlreadyExisted: false,
	}
	if r.Username != nil {
		out.Username = *r.Username
	}
	if r.Created.Valid {
		out.CreatedAt = r.Created.Time
	}
	return out
}

func seedUserRowToResult(r SeedGetUserByUsernameRow, already bool) UserResult {
	out := UserResult{
		Ref:            r.Ref,
		Fullname:       r.Fullname,
		Email:          r.Email,
		Usergroup:      r.Usergroup,
		Approved:       r.Approved == 1,
		AlreadyExisted: already,
	}
	if r.Username != nil {
		out.Username = *r.Username
	}
	if r.Created.Valid {
		out.CreatedAt = r.Created.Time
	}
	return out
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
