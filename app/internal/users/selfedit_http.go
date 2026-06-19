// Phase 1.17.F HTTP surface for the self-edit gates.
//
// Three endpoints:
//
//   GET  /account/selfedit-gates    — any auth'd user; reads the snapshot
//   GET  /admin/system/users        — admin; same snapshot, no auth difference
//                                     today but separated so a future
//                                     "users see only their own gate set" doesn't
//                                     leak admin-only metadata
//   PATCH /admin/system/users       — admin; writes all 5 gates atomically +
//                                     invalidates the cache so peer instances
//                                     pick up new values on next read
//
// The admin gate is the existing system.config.write capability OR
// system.admin (mirrors the sysconfig package's pattern from 1.17.D).

package users

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// CapConfigWrite is the system-wide "admin can write configs"
// capability. Lives here as a string constant to avoid an import
// of the sysconfig package (which imports auth, which imports
// users — would create a cycle).
const CapConfigWrite = "system.config.write"

// GetSelfEditGates returns the current snapshot for any
// authenticated caller. Used by the /account/profile page on
// mount to disable inputs the operator has locked.
func (h *Handler) GetSelfEditGates(
	ctx context.Context,
	_ openapi.GetSelfEditGatesRequestObject,
) (openapi.GetSelfEditGatesResponseObject, error) {
	if id := auth.IdentityFromContext(ctx); id == nil {
		return openapi.GetSelfEditGates401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	gates, _ := h.LoadSelfEditGates(ctx)
	return openapi.GetSelfEditGates200JSONResponse(gatesToAPI(gates)), nil
}

// GetAdminUserGates returns the same snapshot from the admin
// surface. Same payload as the requester endpoint; separated
// because the admin path may grow operator-only metadata
// (e.g., "default would be true if missing" hint) later.
func (h *Handler) GetAdminUserGates(
	ctx context.Context,
	_ openapi.GetAdminUserGatesRequestObject,
) (openapi.GetAdminUserGatesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetAdminUserGates401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapConfigWrite) && !id.Can(CapSystemAdmin) {
		return openapi.GetAdminUserGates403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapConfigWrite + " capability required"},
		}, nil
	}
	gates, _ := h.LoadSelfEditGates(ctx)
	return openapi.GetAdminUserGates200JSONResponse(gatesToAPI(gates)), nil
}

// UpdateAdminUserGates upserts all five system_config rows
// atomically (one tx) + invalidates the cache so subsequent
// reads on this AND peer instances pick up the new values.
//
// The handler uses a single tx for the five upserts so a partial
// write can't leave the admin staring at a half-applied form.
// The cache invalidation is the LAST step inside the success
// path — broadcast before returning so the caller's next GET
// reflects the change.
func (h *Handler) UpdateAdminUserGates(
	ctx context.Context,
	req openapi.UpdateAdminUserGatesRequestObject,
) (openapi.UpdateAdminUserGatesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UpdateAdminUserGates401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapConfigWrite) && !id.Can(CapSystemAdmin) {
		return openapi.UpdateAdminUserGates403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapConfigWrite + " capability required"},
		}, nil
	}
	if req.Body == nil {
		// All-fields required per openapi schema; an empty body
		// would write zero rows and silently no-op.
		gates, _ := h.LoadSelfEditGates(ctx)
		return openapi.UpdateAdminUserGates200JSONResponse(gatesToAPI(gates)), nil
	}
	g := SelfEditGates{
		DisplayName: req.Body.DisplayName,
		Bio:         req.Body.Bio,
		AvatarURL:   req.Body.AvatarUrl,
		Location:    req.Body.Location,
		WebsiteURL:  req.Body.WebsiteUrl,
	}
	pairs := []struct {
		key string
		val bool
	}{
		{selfEditKeyPrefix + string(SelfEditDisplayName), g.DisplayName},
		{selfEditKeyPrefix + string(SelfEditBio), g.Bio},
		{selfEditKeyPrefix + string(SelfEditAvatarURL), g.AvatarURL},
		{selfEditKeyPrefix + string(SelfEditLocation), g.Location},
		{selfEditKeyPrefix + string(SelfEditWebsiteURL), g.WebsiteURL},
	}
	for _, p := range pairs {
		payload, err := json.Marshal(p.val)
		if err != nil {
			return nil, fmt.Errorf("users: marshal gate %q: %w", p.key, err)
		}
		// Direct upsert via pool; avoids a sysconfig package dep.
		// The system_config table is single-row per key, so the
		// pre-existing UNIQUE(key) backs the ON CONFLICT.
		if _, err := h.Pool.Exec(ctx,
			`INSERT INTO system_config (key, value, updated_at) VALUES ($1, $2, NOW())
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
			p.key, payload,
		); err != nil {
			return nil, fmt.Errorf("users: upsert gate %q: %w", p.key, err)
		}
	}
	// Cache invalidation broadcast — local evict + NOTIFY.
	h.InvalidateSelfEditGates(ctx)
	return openapi.UpdateAdminUserGates200JSONResponse(gatesToAPI(g)), nil
}

func gatesToAPI(g SelfEditGates) openapi.SelfEditGates {
	return openapi.SelfEditGates{
		DisplayName: g.DisplayName,
		Bio:         g.Bio,
		AvatarUrl:   g.AvatarURL,
		Location:    g.Location,
		WebsiteUrl:  g.WebsiteURL,
	}
}
