// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Per-asset-type ACL admin endpoints (Phase 1.17.F-bis).
//
// Three operations on the asset-types admin surface:
//
//   GET    /asset_types/{ref}/acls                                          — list ACL rows
//   POST   /asset_types/{ref}/acls                                          — grant a permission
//   DELETE /asset_types/{ref}/acls/{principal_type}/{principal_id}/{permission}
//                                                                         — remove a row
//
// Cap gate: system.asset_types.admin (seeded by migration 00001).
// system.admin holders bypass via the existing wildcard logic in
// Identity.Can.
//
// Access model: ACL rows are restrictive. When zero rows exist for a
// type, the type is "open" and every caller sees it. When at least
// one row exists, the type is restricted to its grantees (plus
// admins). Filtering happens in ListAssetTypes via the
// ListUnauthorisedTypeRefsForUser query — admins skip the filter
// entirely.

package assettype

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/acls"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ListAssetTypeAcls — GET /asset_types/{ref}/acls.
func (h *Handler) ListAssetTypeAcls(
	ctx context.Context,
	req openapi.ListAssetTypeAclsRequestObject,
) (openapi.ListAssetTypeAclsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListAssetTypeAcls401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("system.asset_types.admin") {
		return openapi.ListAssetTypeAcls403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.asset_types.admin capability required"},
		}, nil
	}

	// 404 if the type itself doesn't exist — the empty-ACL case is a
	// legitimate 200 with an empty array.
	if _, err := h.queries.Get(ctx, req.Ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListAssetTypeAcls404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset_type not found"},
			}, nil
		}
		return nil, fmt.Errorf("assettype: get: %w", err)
	}

	rows, err := h.queries.ListAcls(ctx, req.Ref)
	if err != nil {
		return nil, fmt.Errorf("assettype: list acls: %w", err)
	}

	out := make(openapi.ListAssetTypeAcls200JSONResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, openapi.AclEntry{
			PrincipalType:    openapi.AclEntryPrincipalType(r.PrincipalType),
			PrincipalId:      r.PrincipalID,
			Permission:       openapi.AclEntryPermission(r.Permission),
			GrantedAt:        r.GrantedAt.Time,
			GrantedByUserRef: r.GrantedByUserRef,
			ExpiresAt:        tsToTimePtr(r.ExpiresAt),
		})
	}
	return out, nil
}

// AddAssetTypeAcl — POST /asset_types/{ref}/acls.
func (h *Handler) AddAssetTypeAcl(
	ctx context.Context,
	req openapi.AddAssetTypeAclRequestObject,
) (openapi.AddAssetTypeAclResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.AddAssetTypeAcl401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("system.asset_types.admin") {
		return openapi.AddAssetTypeAcl403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.asset_types.admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddAssetTypeAcl400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	body := req.Body
	pt := strings.ToLower(string(body.PrincipalType))
	perm := strings.ToLower(string(body.Permission))
	// Unlike post_acls / collection_acls, this surface honours all
	// three principal types — ListRestrictedAssetTypes and
	// HasAssetTypeAccess resolve role and team membership properly —
	// so the full set is admitted and only the REFERENCE SHAPE is
	// checked: a numeric user ref, or a UUID for a role or team.
	//
	// The shape check is the #916 fix. This handler already rejected an
	// empty principal_id and an unknown principal_type, which made it
	// the strictest of the three ACL writers, but "non-empty" still let
	// a username through into a column nothing would ever match it in.
	if err := acls.ValidatePrincipalRef(pt, body.PrincipalId); err != nil {
		return openapi.AddAssetTypeAcl400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	if perm != "read" && perm != "write" && perm != "admin" {
		return openapi.AddAssetTypeAcl400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "permission must be read|write|admin"},
		}, nil
	}

	// 404 if the type doesn't exist.
	if _, err := h.queries.Get(ctx, req.Ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddAssetTypeAcl404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset_type not found"},
			}, nil
		}
		return nil, fmt.Errorf("assettype: get: %w", err)
	}

	expires := pgtype.Timestamptz{}
	if body.ExpiresAt != nil {
		expires.Time = *body.ExpiresAt
		expires.Valid = true
	}

	if err := h.queries.InsertAcl(ctx, InsertAclParams{
		AssetTypeRef:     req.Ref,
		PrincipalType:    pt,
		PrincipalID:      body.PrincipalId,
		Permission:       perm,
		GrantedByUserRef: &id.UserRef,
		ExpiresAt:        expires,
	}); err != nil {
		return nil, fmt.Errorf("assettype: insert acl: %w", err)
	}

	return openapi.AddAssetTypeAcl204Response{}, nil
}

// RemoveAssetTypeAcl — DELETE /asset_types/{ref}/acls/{principal_type}/{principal_id}/{permission}.
func (h *Handler) RemoveAssetTypeAcl(
	ctx context.Context,
	req openapi.RemoveAssetTypeAclRequestObject,
) (openapi.RemoveAssetTypeAclResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.RemoveAssetTypeAcl401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("system.asset_types.admin") {
		return openapi.RemoveAssetTypeAcl403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.asset_types.admin capability required"},
		}, nil
	}

	n, err := h.queries.DeleteAcl(ctx, DeleteAclParams{
		AssetTypeRef:  req.Ref,
		PrincipalType: strings.ToLower(req.PrincipalType),
		PrincipalID:   req.PrincipalId,
		Permission:    strings.ToLower(req.Permission),
	})
	if err != nil {
		return nil, fmt.Errorf("assettype: delete acl: %w", err)
	}
	if n == 0 {
		return openapi.RemoveAssetTypeAcl404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "acl entry not found"},
		}, nil
	}
	return openapi.RemoveAssetTypeAcl204Response{}, nil
}

// tsToTimePtr converts a nullable pgtype.Timestamptz to *time.Time
// for the openapi-generated optional ExpiresAt field.
func tsToTimePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
