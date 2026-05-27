// Package teams implements the team-DAG slice of the artist-alley HTTP API.
//
// See ADR 0010 Layer 4. Capability gates:
//   - teams.read  — list and view (granted to Base in 00015)
//   - teams.create — create new teams (Admin only)
//   - teams.admin — edit any team / manage parents / manage members (Admin only)
//
// The DAG triggers in migration 00015 do the heavy lifting:
//   - team_parents BEFORE INSERT rejects cycles (we surface as 409)
//   - team_parents AFTER INSERT propagates closure rows
//   - team_parents AFTER DELETE rebuilds the closure
package teams

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const (
	CapTeamsRead   = "teams.read"
	CapTeamsCreate = "teams.create"
	CapTeamsAdmin  = "teams.admin"
	CapSystemAdmin = "system.admin"
)

const maxListLimit = 500

// cacheDomainTeamByID is the NOTIFY channel for per-team-id cache
// entries. Local writes invalidate via Cache.Invalidate; future
// cross-package writers would use a registry.Emit helper (not
// needed yet — no other package mutates teams today).
const cacheDomainTeamByID = "team.id"

type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// byID caches the fetchTeam result (team row + direct parents)
	// by UUID string. Team chips in the post modal + the upload
	// modal team picker both repeatedly hit GetTeam; warm cache
	// elides the join.
	byID *cache.Cache[openapi.Team]
}

func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{Pool: pool, Logger: logger}
	if registry != nil {
		// 1_000 teams is generous — most orgs have dozens, even
		// huge studios have hundreds across all sub-teams. Per-row
		// memory is ~1KB so the LRU stays under 1MB.
		h.byID = cache.Register[openapi.Team](registry, cacheDomainTeamByID, 1_000)
	}
	return h
}

// ---------------------------------------------------------------------------
// ListTeams
// ---------------------------------------------------------------------------

func (h *Handler) ListTeams(
	ctx context.Context,
	req openapi.ListTeamsRequestObject,
) (openapi.ListTeamsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListTeams401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapTeamsRead) {
		return openapi.ListTeams403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.read capability required"},
		}, nil
	}

	limit := int32(100)
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

	q := New(h.Pool)

	// Ancestor filter short-circuits the regular paginated list — it
	// goes through the closure table for a single-query descendant
	// fetch.
	if req.Params.Ancestor != nil {
		rows, err := q.ListTeamsUnderAncestor(ctx, ListTeamsUnderAncestorParams{
			AncestorID: pgtype.UUID{Bytes: uuid.UUID(*req.Params.Ancestor), Valid: true},
			Limit:      limit,
		})
		if err != nil {
			return nil, fmt.Errorf("teams: list under ancestor: %w", err)
		}
		items, err := h.teamsToAPI(ctx, rows)
		if err != nil {
			return nil, err
		}
		return openapi.ListTeams200JSONResponse(openapi.TeamList{Items: items}), nil
	}

	var cursorName *string
	var cursorID pgtype.UUID
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		name, id, err := decodeCursor(*req.Params.Cursor)
		if err != nil {
			return openapi.ListTeams500JSONResponse{
				InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorName = &name
		cursorID = pgtype.UUID{Bytes: id, Valid: true}
	}

	fetch := limit + 1
	rows, err := q.ListTeams(ctx, ListTeamsParams{
		CursorName: cursorName,
		CursorID:   cursorID,
		RowLimit:   fetch,
	})
	if err != nil {
		return nil, fmt.Errorf("teams: list: %w", err)
	}
	more := len(rows) > int(limit)
	if more {
		rows = rows[:limit]
	}
	items, err := h.teamsToAPI(ctx, rows)
	if err != nil {
		return nil, err
	}
	resp := openapi.TeamList{Items: items}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		c := encodeCursor(last.Name, uuid.UUID(last.ID.Bytes))
		resp.NextCursor = &c
	}
	return openapi.ListTeams200JSONResponse(resp), nil
}

// ---------------------------------------------------------------------------
// CreateTeam
// ---------------------------------------------------------------------------

func (h *Handler) CreateTeam(
	ctx context.Context,
	req openapi.CreateTeamRequestObject,
) (openapi.CreateTeamResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.CreateTeam401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapTeamsCreate) && !id.Can(CapTeamsAdmin) && !id.Can(CapSystemAdmin) {
		return openapi.CreateTeam403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.create capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreateTeam400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	in := req.Body
	slug := strings.TrimSpace(in.Slug)
	name := strings.TrimSpace(in.Name)
	if slug == "" || name == "" {
		return openapi.CreateTeam400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "slug and name are required"},
		}, nil
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("teams: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	row, err := q.CreateTeam(ctx, CreateTeamParams{
		Slug:        slug,
		Name:        name,
		Description: strOr(in.Description, ""),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return openapi.CreateTeam409JSONResponse{
				ConflictJSONResponse: openapi.ConflictJSONResponse{Error: "team slug already in use"},
			}, nil
		}
		return nil, fmt.Errorf("teams: create: %w", err)
	}

	// Attach optional parents. Cycle-rejection trigger surfaces as 409
	// (CHECK violation in plpgsql). Missing parent surfaces as 404.
	if in.ParentIds != nil {
		for _, p := range *in.ParentIds {
			if err := q.AddTeamParent(ctx, AddTeamParentParams{
				ChildID:  row.ID,
				ParentID: pgtype.UUID{Bytes: uuid.UUID(p), Valid: true},
			}); err != nil {
				if isCheckViolation(err) {
					return openapi.CreateTeam409JSONResponse{
						ConflictJSONResponse: openapi.ConflictJSONResponse{Error: "parent edge would create a cycle"},
					}, nil
				}
				if isFKViolation(err) {
					return openapi.CreateTeam404JSONResponse{
						NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "parent team not found"},
					}, nil
				}
				return nil, fmt.Errorf("teams: add parent: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("teams: commit: %w", err)
	}

	full, err := h.fetchTeam(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	return openapi.CreateTeam201JSONResponse(*full), nil
}

// ---------------------------------------------------------------------------
// GetTeam
// ---------------------------------------------------------------------------

func (h *Handler) GetTeam(
	ctx context.Context,
	req openapi.GetTeamRequestObject,
) (openapi.GetTeamResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetTeam401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapTeamsRead) {
		return openapi.GetTeam403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.read capability required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	full, err := h.fetchTeam(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetTeam404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.GetTeam200JSONResponse(*full), nil
}

// ---------------------------------------------------------------------------
// UpdateTeam
// ---------------------------------------------------------------------------

func (h *Handler) UpdateTeam(
	ctx context.Context,
	req openapi.UpdateTeamRequestObject,
) (openapi.UpdateTeamResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UpdateTeam401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapTeamsAdmin) && !id.Can(CapSystemAdmin) {
		return openapi.UpdateTeam403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdateTeam400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	if _, err := q.UpdateTeam(ctx, UpdateTeamParams{
		ID:          pgID,
		Name:        req.Body.Name,
		Description: req.Body.Description,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateTeam404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
			}, nil
		}
		return nil, fmt.Errorf("teams: update: %w", err)
	}
	h.invalidateTeam(ctx, pgID)
	full, err := h.fetchTeam(ctx, pgID)
	if err != nil {
		return nil, err
	}
	return openapi.UpdateTeam200JSONResponse(*full), nil
}

// ---------------------------------------------------------------------------
// DeleteTeam
// ---------------------------------------------------------------------------

func (h *Handler) DeleteTeam(
	ctx context.Context,
	req openapi.DeleteTeamRequestObject,
) (openapi.DeleteTeamResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.DeleteTeam401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsAdmin) && !caller.Can(CapSystemAdmin) {
		return openapi.DeleteTeam403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.admin capability required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	rows, err := New(h.Pool).SoftDeleteTeam(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("teams: delete: %w", err)
	}
	if rows == 0 {
		return openapi.DeleteTeam404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
		}, nil
	}
	h.invalidateTeam(ctx, pgID)
	return openapi.DeleteTeam204Response{}, nil
}

// ---------------------------------------------------------------------------
// Parents
// ---------------------------------------------------------------------------

func (h *Handler) ListTeamParents(
	ctx context.Context,
	req openapi.ListTeamParentsRequestObject,
) (openapi.ListTeamParentsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListTeamParents401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsRead) {
		return openapi.ListTeamParents403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.read capability required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	rows, err := New(h.Pool).ListTeamParents(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("teams: list parents: %w", err)
	}
	items, err := h.teamsToAPI(ctx, rows)
	if err != nil {
		return nil, err
	}
	return openapi.ListTeamParents200JSONResponse(items), nil
}

func (h *Handler) AddTeamParent(
	ctx context.Context,
	req openapi.AddTeamParentRequestObject,
) (openapi.AddTeamParentResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddTeamParent401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsAdmin) && !caller.Can(CapSystemAdmin) {
		return openapi.AddTeamParent403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddTeamParent400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	pgChild := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	pgParent := pgtype.UUID{Bytes: uuid.UUID(req.Body.ParentId), Valid: true}
	if err := New(h.Pool).AddTeamParent(ctx, AddTeamParentParams{
		ChildID:  pgChild,
		ParentID: pgParent,
	}); err != nil {
		if isCheckViolation(err) {
			return openapi.AddTeamParent409JSONResponse{
				ConflictJSONResponse: openapi.ConflictJSONResponse{Error: "parent edge would create a cycle"},
			}, nil
		}
		if isFKViolation(err) {
			return openapi.AddTeamParent404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team or parent not found"},
			}, nil
		}
		return nil, fmt.Errorf("teams: add parent: %w", err)
	}
	// The child's `parents` list just changed, so drop its cached
	// merged shape.
	h.invalidateTeam(ctx, pgChild)
	return openapi.AddTeamParent204Response{}, nil
}

func (h *Handler) RemoveTeamParent(
	ctx context.Context,
	req openapi.RemoveTeamParentRequestObject,
) (openapi.RemoveTeamParentResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemoveTeamParent401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsAdmin) && !caller.Can(CapSystemAdmin) {
		return openapi.RemoveTeamParent403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.admin capability required"},
		}, nil
	}
	pgChild := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	rows, err := New(h.Pool).RemoveTeamParent(ctx, RemoveTeamParentParams{
		ChildID:  pgChild,
		ParentID: pgtype.UUID{Bytes: uuid.UUID(req.ParentId), Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("teams: remove parent: %w", err)
	}
	if rows == 0 {
		return openapi.RemoveTeamParent404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "edge not found"},
		}, nil
	}
	h.invalidateTeam(ctx, pgChild)
	return openapi.RemoveTeamParent204Response{}, nil
}

// ---------------------------------------------------------------------------
// Members
// ---------------------------------------------------------------------------

func (h *Handler) ListTeamMembers(
	ctx context.Context,
	req openapi.ListTeamMembersRequestObject,
) (openapi.ListTeamMembersResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListTeamMembers401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsRead) {
		return openapi.ListTeamMembers403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.read capability required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	rows, err := New(h.Pool).ListTeamMembers(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("teams: list members: %w", err)
	}
	out := make([]openapi.TeamMember, 0, len(rows))
	for _, r := range rows {
		m := openapi.TeamMember{
			TeamId:    openapi_types.UUID(r.TeamID.Bytes),
			RsUserId:  r.RsUserID,
			AddedAt:   r.AddedAt.Time,
		}
		if r.AddedByRsUserID != nil {
			m.AddedByRsUserId = r.AddedByRsUserID
		}
		out = append(out, m)
	}
	return openapi.ListTeamMembers200JSONResponse(out), nil
}

func (h *Handler) AddTeamMember(
	ctx context.Context,
	req openapi.AddTeamMemberRequestObject,
) (openapi.AddTeamMemberResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddTeamMember401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsAdmin) && !caller.Can(CapSystemAdmin) {
		return openapi.AddTeamMember403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddTeamMember400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	if err := New(h.Pool).AddTeamMember(ctx, AddTeamMemberParams{
		TeamID:              pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true},
		RsUserID:            req.Body.RsUserId,
		AddedByRsUserID:     &caller.UserRef,
	}); err != nil {
		if isFKViolation(err) {
			return openapi.AddTeamMember404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
			}, nil
		}
		return nil, fmt.Errorf("teams: add member: %w", err)
	}
	return openapi.AddTeamMember204Response{}, nil
}

func (h *Handler) RemoveTeamMember(
	ctx context.Context,
	req openapi.RemoveTeamMemberRequestObject,
) (openapi.RemoveTeamMemberResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemoveTeamMember401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapTeamsAdmin) && !caller.Can(CapSystemAdmin) {
		return openapi.RemoveTeamMember403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "teams.admin capability required"},
		}, nil
	}
	rows, err := New(h.Pool).RemoveTeamMember(ctx, RemoveTeamMemberParams{
		TeamID:   pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true},
		RsUserID: req.RsUserId,
	})
	if err != nil {
		return nil, fmt.Errorf("teams: remove member: %w", err)
	}
	if rows == 0 {
		return openapi.RemoveTeamMember404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "membership not found"},
		}, nil
	}
	return openapi.RemoveTeamMember204Response{}, nil
}

// ---------------------------------------------------------------------------
// /auth/me/teams — caller's direct memberships.
// ---------------------------------------------------------------------------

func (h *Handler) GetMyTeams(
	ctx context.Context,
	req openapi.GetMyTeamsRequestObject,
) (openapi.GetMyTeamsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.GetMyTeams401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	rows, err := New(h.Pool).ListUserTeams(ctx, caller.UserRef)
	if err != nil {
		return nil, fmt.Errorf("teams: list user teams: %w", err)
	}
	items, err := h.teamsToAPI(ctx, rows)
	if err != nil {
		return nil, err
	}
	return openapi.GetMyTeams200JSONResponse(items), nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fetchTeam reads the team row + its direct parent links, returning
// the API shape. Reads through byID cache when present; on miss
// queries the two SQLs and populates the cache.
func (h *Handler) fetchTeam(ctx context.Context, id pgtype.UUID) (*openapi.Team, error) {
	key := uuidString(id)
	if h.byID != nil {
		if v, ok := h.byID.Get(key); ok {
			out := v
			return &out, nil
		}
	}
	q := New(h.Pool)
	row, err := q.GetTeam(ctx, id)
	if err != nil {
		return nil, err
	}
	parents, err := q.ListTeamParents(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("teams: list parents: %w", err)
	}
	out := teamRowToAPI(row)
	parentLinks := make([]openapi.TeamParentLink, 0, len(parents))
	for _, p := range parents {
		parentLinks = append(parentLinks, openapi.TeamParentLink{
			ParentId:   openapi_types.UUID(p.ID.Bytes),
			ParentSlug: ptr(p.Slug),
			ParentName: ptr(p.Name),
		})
	}
	out.Parents = &parentLinks
	if h.byID != nil {
		h.byID.Add(key, out)
	}
	return &out, nil
}

// invalidateTeam drops the local entry and broadcasts. Called by
// every mutating endpoint after commit. Best-effort.
func (h *Handler) invalidateTeam(ctx context.Context, id pgtype.UUID) {
	if h.byID == nil {
		return
	}
	_ = h.byID.Invalidate(ctx, uuidString(id))
}

func uuidString(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }

func (h *Handler) teamsToAPI(_ context.Context, rows []Team) ([]openapi.Team, error) {
	out := make([]openapi.Team, 0, len(rows))
	for _, r := range rows {
		out = append(out, teamRowToAPI(r))
	}
	return out, nil
}

func teamRowToAPI(t Team) openapi.Team {
	out := openapi.Team{
		Id:          openapi_types.UUID(t.ID.Bytes),
		Slug:        t.Slug,
		Name:        t.Name,
		Description: t.Description,
		CreatedAt:   t.CreatedAt.Time,
		UpdatedAt:   t.UpdatedAt.Time,
	}
	if t.OriginServerID.Valid {
		v := openapi_types.UUID(t.OriginServerID.Bytes)
		out.OriginServerId = &v
	}
	return out
}

// Cursor format: "name|uuid" base64-url encoded. Same shape as posts.
func encodeCursor(name string, id uuid.UUID) string {
	raw := name + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (string, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", uuid.Nil, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return "", uuid.Nil, errors.New("bad cursor shape")
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return "", uuid.Nil, err
	}
	return parts[0], id, nil
}

// isUniqueViolation / isFKViolation / isCheckViolation match by pg
// SQLSTATE class so we don't depend on error-string formatting.
func isUniqueViolation(err error) bool   { return pgErrCode(err) == "23505" }
func isFKViolation(err error) bool       { return pgErrCode(err) == "23503" }
func isCheckViolation(err error) bool    { return pgErrCode(err) == "23514" }

func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func ptr[T any](v T) *T { return &v }

// Compile-time assertion against the strict-server interface drift.
var _ interface {
	ListTeams(context.Context, openapi.ListTeamsRequestObject) (openapi.ListTeamsResponseObject, error)
	CreateTeam(context.Context, openapi.CreateTeamRequestObject) (openapi.CreateTeamResponseObject, error)
	GetTeam(context.Context, openapi.GetTeamRequestObject) (openapi.GetTeamResponseObject, error)
	UpdateTeam(context.Context, openapi.UpdateTeamRequestObject) (openapi.UpdateTeamResponseObject, error)
	DeleteTeam(context.Context, openapi.DeleteTeamRequestObject) (openapi.DeleteTeamResponseObject, error)
	ListTeamParents(context.Context, openapi.ListTeamParentsRequestObject) (openapi.ListTeamParentsResponseObject, error)
	AddTeamParent(context.Context, openapi.AddTeamParentRequestObject) (openapi.AddTeamParentResponseObject, error)
	RemoveTeamParent(context.Context, openapi.RemoveTeamParentRequestObject) (openapi.RemoveTeamParentResponseObject, error)
	ListTeamMembers(context.Context, openapi.ListTeamMembersRequestObject) (openapi.ListTeamMembersResponseObject, error)
	AddTeamMember(context.Context, openapi.AddTeamMemberRequestObject) (openapi.AddTeamMemberResponseObject, error)
	RemoveTeamMember(context.Context, openapi.RemoveTeamMemberRequestObject) (openapi.RemoveTeamMemberResponseObject, error)
	GetMyTeams(context.Context, openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error)
} = (*Handler)(nil)
