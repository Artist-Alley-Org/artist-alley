// Package assettype implements the artist-alley asset-type
// catalog endpoints (formerly RS's hand-rolled
// `pages/team/team_asset_types.php` and the relevant slices of
// `include/resource_functions.php`).
//
// The HTTP contract is defined in `app/api/openapi.yaml`. The
// `app/internal/openapi` package contains code generated from that
// spec by `oapi-codegen`; the Handler below implements
// `openapi.StrictServerInterface` (currently the single
// ListAssetTypes operation).
//
// Layout:
//
//	queries.sql            -- sqlc input (hand-written SQL)
//	queries.sql.go, db.go, -- sqlc generated; regenerate with
//	  models.go               `cd app && docker run ... sqlc generate`
//	handler.go             -- HTTP handler, this file
//	handler_test.go        -- integration tests against live Postgres
//
// This is the template for every feature package that follows.
package assettype

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Handler holds dependencies for the asset-type endpoints.
type Handler struct {
	queries *Queries
	logger  *slog.Logger
}

// NewHandler binds the package's handlers to a Postgres pool.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger) *Handler {
	return &Handler{queries: New(pool), logger: logger}
}

// ListAssetTypes implements openapi.StrictServerInterface.
// GET /api/v1/asset_types
//
// Per-type ACLs (Phase 1.17.F-bis) restrict the result for non-admin
// callers: any asset_type with at least one ACL row is "restricted",
// and only callers with a non-expired matching ACL row (user, role,
// or team membership) see it. Anonymous callers see every
// unrestricted type; system.admin holders see everything.
func (h *Handler) ListAssetTypes(
	ctx context.Context,
	_ openapi.ListAssetTypesRequestObject,
) (openapi.ListAssetTypesResponseObject, error) {
	rows, err := h.queries.List(ctx)
	if err != nil {
		h.logger.LogAttrs(ctx, slog.LevelError, "asset_types.list.error",
			slog.String("err", err.Error()),
		)
		msg := "could not list asset types"
		return openapi.ListAssetTypes500JSONResponse{
			InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: msg},
		}, nil
	}

	// Compute the set of asset_type_refs the caller MUST NOT see.
	// Admins (system.admin) bypass the filter entirely.
	hidden := map[int64]struct{}{}
	id := auth.IdentityFromContext(ctx)
	if id == nil || !id.Can(auth.SuperAdminCapability) {
		// Anonymous callers and authenticated non-admins both go
		// through the filter; the query handles user_ref=0 as
		// "no roles, no teams" (sees only unrestricted types).
		var userRef int64
		if id != nil {
			userRef = id.UserRef
		}
		unauthorised, err := h.queries.ListUnauthorisedTypeRefsForUser(ctx, userRef)
		if err != nil {
			h.logger.LogAttrs(ctx, slog.LevelError, "asset_types.acl_filter.error",
				slog.String("err", err.Error()),
			)
			// Fall through with no filter rather than 500 — the
			// caller still gets a useful (if over-broad) list. The
			// follow-up commit that enforces on upload paths gates
			// independently.
		}
		for _, ref := range unauthorised {
			hidden[ref] = struct{}{}
		}
	}

	out := make(openapi.ListAssetTypes200JSONResponse, 0, len(rows))
	for _, r := range rows {
		if _, drop := hidden[r.Ref]; drop {
			continue
		}
		out = append(out, openapi.AssetType{
			Ref:               r.Ref,
			Name:              r.Name,
			AllowedExtensions: r.AllowedExtensions,
			OrderBy:           r.OrderBy,
			Icon:              r.Icon,
			Colour:            r.Colour,
			Tab:               r.Tab,
		})
	}
	return out, nil
}

// HasTypeAccess is the exported helper that the asset / upload
// packages call to check per-type permission. Admins always pass.
// Permission is one of "read" | "write" | "admin" — higher levels
// imply lower ones (an admin grant satisfies a read check).
//
// Returns true when the caller may exercise the permission, false
// otherwise. A DB error returns false + the error so callers can
// surface or log it; the safe default for the access decision is
// "deny" rather than "allow on error".
func (h *Handler) HasTypeAccess(ctx context.Context, id *auth.Identity, typeRef int64, permission string) (bool, error) {
	if id != nil && id.Can(auth.SuperAdminCapability) {
		return true, nil
	}
	// Anonymous + non-admin: if the type has zero ACL rows it's
	// open. Otherwise consult the ACL.
	var userRef int64
	if id != nil {
		userRef = id.UserRef
	}
	// Cheap "is this type restricted at all?" check: ask the cache /
	// underlying query for the unauthorised set and short-circuit
	// when our typeRef isn't even in the restricted list. The query
	// returns "restricted AND not authorised" rows; a type with no
	// ACL rows can never appear, so absence == open access.
	unauthorised, err := h.queries.ListUnauthorisedTypeRefsForUser(ctx, userRef)
	if err != nil {
		return false, err
	}
	for _, r := range unauthorised {
		if r == typeRef {
			// The type is restricted AND the caller has no matching
			// grant for read. For 'read' we can return false directly;
			// for 'write'/'admin' we still need to query, because the
			// "unauthorised" set is computed at the 'read' threshold.
			if permission == "read" {
				return false, nil
			}
			break
		}
	}
	// For write/admin permission checks, fall through to the precise
	// per-permission query.
	if permission == "read" {
		// We made it past the unauthorised loop without finding the
		// type, so either it's unrestricted (open) or the caller has
		// at least 'read'.
		return true, nil
	}
	return h.queries.HasAssetTypeAccess(ctx, HasAssetTypeAccessParams{
		UserRef:     userRef,
		AssetTypeRef: typeRef,
		Permission:   permission,
	})
}
