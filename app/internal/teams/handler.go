// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package teams implements the team-DAG slice of the artist-alley HTTP API.
//
// See ADR 0010 Layer 4. Capability gates:
//   - teams.read  — list and view (granted to Base in 00001)
//   - teams.create — create new teams (Admin only)
//   - teams.admin — edit any team / manage parents / manage members (Admin only)
//
// The DAG triggers in migration 00001 do the heavy lifting:
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
		if err := h.attachDirectoryStats(ctx, items); err != nil {
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
	if err := h.attachDirectoryStats(ctx, items); err != nil {
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
// SetTeamHero (#982)
// ---------------------------------------------------------------------------

// SetTeamHero points a team at the asset it should use as its picture,
// or clears the pointer so the team falls back to its initials tile.
//
// # The gate is team-scoped, and that is the whole reason this is not
// # part of UpdateTeam
//
// Renaming a team is a global act; choosing its picture is one a team's
// OWN admin should be able to do. So this asks for `teams.admin` IN THIS
// TEAM, which a global holder also satisfies (auth.Can treats a global
// grant as covering any scope) while a team-scoped holder satisfies only
// for their own team.
//
// It is `teams.admin`, NOT membership. Being in a team says you are one
// of its people; it does not say you speak for it, and a team's picture
// is the most public thing about it. Gating on membership would let any
// member repoint the studio's branding.
//
// # What "admissible" means, and why it is checked again later
//
// public AND owned by this team — see migration 00047. Checked here so a
// refusal reaches the caller as a 400 rather than as a silently ignored
// write, and checked AGAIN on every read (attachHeroes) because this
// answer expires the moment somebody edits the asset.
func (h *Handler) SetTeamHero(
	ctx context.Context,
	req openapi.SetTeamHeroRequestObject,
) (openapi.SetTeamHeroResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.SetTeamHero401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	teamID := uuid.UUID(req.Id)
	if !caller.Can(CapTeamsAdmin, auth.InTeam(teamID)) && !caller.Can(CapSystemAdmin) {
		return openapi.SetTeamHero403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "teams.admin capability required for this team",
			},
		}, nil
	}
	if req.Body == nil {
		return openapi.SetTeamHero400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}

	// Mutually exclusive, refused rather than resolved: a body carrying
	// both has two intentions and the server has no basis for preferring
	// either. Silently discarding one is how a "clear" that never
	// happened gets shipped. Same shape as collections' clear_cover.
	clearHero := req.Body.ClearHero != nil && *req.Body.ClearHero
	if clearHero && req.Body.AssetId != nil {
		return openapi.SetTeamHero400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "send either asset_id or clear_hero, not both",
			},
		}, nil
	}
	if !clearHero && req.Body.AssetId == nil {
		return openapi.SetTeamHero400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "send either asset_id or clear_hero",
			},
		}, nil
	}

	pgID := pgtype.UUID{Bytes: teamID, Valid: true}
	q := New(h.Pool)

	// Liveness before admissibility, so a missing team answers 404
	// rather than the 400 it would otherwise collect from the asset
	// check (no asset carries a nonexistent team's id, so every
	// candidate would look inadmissible for the wrong reason).
	live, err := q.IsTeamLive(ctx, pgID)
	if err != nil {
		return nil, fmt.Errorf("teams: hero liveness: %w", err)
	}
	if !live {
		return openapi.SetTeamHero404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
		}, nil
	}

	var heroPtr pgtype.UUID
	if req.Body.AssetId != nil {
		want := uuid.UUID(*req.Body.AssetId)
		// #1147 — the same three caller inputs the render-time re-check
		// takes, from the one helper, so "may point at" and "may see
		// painted" cannot answer differently.
		hCaller, hMature, hAdmin := heroViewerFromContext(ctx)
		ok, err := TeamHeroCandidateGated(ctx, h.Pool, want, pgID, hCaller, hMature, hAdmin)
		if err != nil {
			return nil, err
		}
		if !ok {
			// ONE response for "no such asset", "not public" and "not
			// this team's". Distinguishing them turns this endpoint
			// into an existence oracle: an admin could enumerate asset
			// ids and read the difference as "this id exists and is
			// hidden from me", which is the fact the sensitivity plane
			// withholds in the first place.
			return openapi.SetTeamHero400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{
					Error: "asset_id must be a public asset belonging to this team",
				},
			}, nil
		}
		heroPtr = pgtype.UUID{Bytes: want, Valid: true}
	}

	if _, err := q.SetTeamHero(ctx, SetTeamHeroParams{
		ID:          pgID,
		ClearHero:   clearHero,
		HeroAssetID: heroPtr,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetTeamHero404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
			}, nil
		}
		return nil, fmt.Errorf("teams: set hero: %w", err)
	}
	h.invalidateTeam(ctx, pgID)
	full, err := h.fetchTeam(ctx, pgID)
	if err != nil {
		return nil, err
	}
	return openapi.SetTeamHero200JSONResponse(*full), nil
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
			TeamId:   openapi_types.UUID(r.TeamID.Bytes),
			UserRef:  r.UserRef,
			AddedAt:  r.AddedAt.Time,
			Username: r.Username,
		}
		if r.AddedByUserRef != nil {
			m.AddedByUserRef = r.AddedByUserRef
		}
		// display_name stays absent rather than empty when the member
		// has no profile row, so a client can tell "no display name" from
		// "display name is the empty string" and fall back to username.
		if r.DisplayName != nil && *r.DisplayName != "" {
			m.DisplayName = r.DisplayName
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
		TeamID:         pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true},
		UserRef:        req.Body.UserRef,
		AddedByUserRef: &caller.UserRef,
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
		TeamID:  pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true},
		UserRef: req.UserRef,
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
//
// The hero picture (#982) is stamped on the way OUT, on both the hit and
// the miss branch, and is never what goes into the cache — see
// attachHeroes for why an entry that carried one would go stale without
// anything being able to invalidate it.
func (h *Handler) fetchTeam(ctx context.Context, id pgtype.UUID) (*openapi.Team, error) {
	key := uuidString(id)
	if h.byID != nil {
		if v, ok := h.byID.Get(key); ok {
			out := v
			one := []openapi.Team{out}
			if err := h.attachHeroes(ctx, one); err != nil {
				return nil, err
			}
			return &one[0], nil
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
	// Cached WITHOUT the hero, then enriched below: teamRowToAPI never
	// sets HeroAssetId, so what lands in the LRU is hero-free by
	// construction rather than by remembering to strip it.
	if h.byID != nil {
		h.byID.Add(key, out)
	}
	one := []openapi.Team{out}
	if err := h.attachHeroes(ctx, one); err != nil {
		return nil, err
	}
	return &one[0], nil
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

// attachDirectoryStats fills member_count / content_count on one page
// of the /teams directory (#684), in ONE round trip for the whole page
// rather than two per card.
//
// Only the LIST paths call this. getTeam deliberately does not: it
// reads through the byID LRU, and that cache is invalidated by the
// team-row and parent-edge endpoints only — AddTeamMember and
// RemoveTeamMember do not touch it, and never needed to, because
// nothing membership-shaped was cached. Putting a member count behind
// that entry would make it wrong the first time somebody joined a
// team, and the fix ("invalidate on membership change too") buys a
// number that surface does not need — the team page already fetches
// /teams/{id}/members for its member strip.
//
// Best-effort is NOT good enough here: a card silently reading "0
// members · 0 works" is worse than a failed page, because it is
// indistinguishable from an empty studio. So the error propagates.
func (h *Handler) attachDirectoryStats(ctx context.Context, items []openapi.Team) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]pgtype.UUID, 0, len(items))
	for _, it := range items {
		ids = append(ids, pgtype.UUID{Bytes: it.Id, Valid: true})
	}
	rows, err := New(h.Pool).TeamDirectoryStats(ctx, ids)
	if err != nil {
		return fmt.Errorf("teams: directory stats: %w", err)
	}
	byID := make(map[uuid.UUID]TeamDirectoryStatsRow, len(rows))
	for _, r := range rows {
		byID[uuid.UUID(r.TeamID.Bytes)] = r
	}
	for i := range items {
		r, ok := byID[uuid.UUID(items[i].Id)]
		if !ok {
			continue
		}
		m, c := r.MemberCount, r.AssetCount
		items[i].MemberCount = &m
		items[i].ContentCount = &c
	}
	return nil
}

// teamsToAPI converts a page of rows and stamps each one's hero picture
// (#982). Every LIST path funnels through here, which is the point: the
// hero is a re-derived value, and a surface that forgot to ask for it
// would silently render initials for teams that have a picture.
func (h *Handler) teamsToAPI(ctx context.Context, rows []Team) ([]openapi.Team, error) {
	out := make([]openapi.Team, 0, len(rows))
	for _, r := range rows {
		out = append(out, teamRowToAPI(r))
	}
	if err := h.attachHeroes(ctx, out); err != nil {
		return nil, err
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
	// t.HeroAssetID is DELIBERATELY not mapped here. That is the stored
	// pointer, and it is only as true as the moment it was written: the
	// asset behind it can be set to 'restricted', moved to another team
	// or soft-deleted without anything touching this row. Shipping it
	// would put a picture on the wire that the rule no longer admits.
	// attachHeroes re-derives the admissible answer instead, and it is
	// the only writer of out.HeroAssetId.
	return out
}

// attachHeroes stamps each team's hero picture after re-checking that it
// still qualifies (#982). See the TeamHeroes query and migration 00047.
//
// # Why this is an enrichment pass rather than a column on the read
//
// Two reasons, and the second is the one that bites.
//
// The stored pointer is not the answer. "Is this asset still public, and
// still this team's?" is a question about the ASSETS table that nothing
// in the teams row can answer, so it has to be asked at read time or not
// at all. A gate that runs only when the admin picks the picture is not a
// gate; it is a note about the past.
//
// And it must run AFTER the cache. fetchTeam reads through the byID LRU,
// which is invalidated by the team-row and parent-edge endpoints only —
// nothing about an asset's sensitivity touches it, and nothing sensibly
// could, since the asset does not know which teams point at it. A hero
// baked into that entry would go on rendering after the asset stopped
// qualifying, for as long as the entry lived. Same rule ADR 0013 states
// for viewer-dependent values, reached from the other direction: this one
// is the same for every viewer, but it goes stale, so it is computed
// after the cache rather than stored in it.
//
// Best-effort is NOT good enough: an error here would silently paint
// initials for every team, which is indistinguishable from "no team has a
// picture". So the error propagates.
func (h *Handler) attachHeroes(ctx context.Context, items []openapi.Team) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]pgtype.UUID, 0, len(items))
	for _, it := range items {
		ids = append(ids, pgtype.UUID{Bytes: it.Id, Valid: true})
	}
	// #1147 — the hero is a DERIVED PICTURE: the row holds a pointer and
	// the client paints that asset's rendition. So it composes the mature
	// axis like every other derived-picture surface, which makes this
	// enrichment per-viewer where it used to answer the same for
	// everybody. It already ran after the by-id cache (see the note
	// above), which is exactly where ADR 0013's amendment puts a
	// viewer-dependent value, so nothing moved to accommodate it.
	hCaller, hMature, hAdmin := heroViewerFromContext(ctx)
	rows, err := TeamHeroesGated(ctx, h.Pool, ids, hCaller, hMature, hAdmin)
	if err != nil {
		return err
	}
	byTeam := make(map[uuid.UUID]uuid.UUID, len(rows))
	for _, r := range rows {
		byTeam[uuid.UUID(r.TeamID.Bytes)] = uuid.UUID(r.HeroAssetID.Bytes)
	}
	for i := range items {
		// Cleared first rather than only set on a hit, so "the query
		// decides" is true rather than merely expected — an item that
		// arrived carrying a hero from anywhere else loses it here.
		items[i].HeroAssetId = nil
		if a, ok := byTeam[uuid.UUID(items[i].Id)]; ok {
			v := openapi_types.UUID(a)
			items[i].HeroAssetId = &v
		}
	}
	return nil
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
func isUniqueViolation(err error) bool { return pgErrCode(err) == "23505" }
func isFKViolation(err error) bool     { return pgErrCode(err) == "23503" }
func isCheckViolation(err error) bool  { return pgErrCode(err) == "23514" }

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
