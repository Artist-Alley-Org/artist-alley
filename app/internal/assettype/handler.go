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

	out := make(openapi.ListAssetTypes200JSONResponse, 0, len(rows))
	for _, r := range rows {
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
