// HTTP handler for the licensing surface.
//
// v1 exposes a single admin-gated endpoint:
//
//	GET /api/v1/admin/license/status   → current license Status snapshot
//
// Future endpoints (Phase 1.17.O-2):
//
//	POST /api/v1/admin/license/upload     install a new .lic file
//	POST /api/v1/admin/license/validate   parse + verify without installing
//	GET  /api/v1/license/status           limited public view for users

package licensing

import (
	"context"
	"log/slog"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Handler is the openapi-strict adapter. Wraps *State and exposes
// the openapi method set; api.go's apiServer delegates to it.
type Handler struct {
	state  *State
	logger *slog.Logger
}

// NewHandler wires the handler to a State + logger. Both must be
// non-nil — community mode is signaled by a non-nil State with no
// license loaded, not by passing nil.
func NewHandler(state *State, logger *slog.Logger) *Handler {
	return &Handler{state: state, logger: logger}
}

// GetAdminLicenseStatus — GET /api/v1/admin/license/status
func (h *Handler) GetAdminLicenseStatus(
	ctx context.Context,
	_ openapi.GetAdminLicenseStatusRequestObject,
) (openapi.GetAdminLicenseStatusResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetAdminLicenseStatus401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	if !id.Can(auth.SuperAdminCapability) {
		return openapi.GetAdminLicenseStatus403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "system.admin capability required",
			},
		}, nil
	}

	st := h.state.Status()
	return openapi.GetAdminLicenseStatus200JSONResponse(statusToWire(st)), nil
}

// statusToWire converts our internal Status to the openapi-generated
// wire shape. The internal Status carries RFC3339 strings for the
// admin UI; the openapi schema declares the same fields as
// `format: date-time`, which the Go codegen renders as `*time.Time`.
// We re-parse here so the wire JSON serialises in the standard
// openapi-friendly form.
func statusToWire(s Status) openapi.LicenseStatus {
	return openapi.LicenseStatus{
		Loaded:          s.Loaded,
		Tier:            s.Tier,
		Features:        s.Features,
		Owner:           ptrIfNotEmpty(s.Owner),
		Org:             ptrIfNotEmpty(s.Org),
		Lid:             ptrIfNotEmpty(s.LID),
		Seats:           s.Seats,
		SeatWindowDays:  ptrIfNotZero(s.SeatWindowDays),
		AssetCap:        s.AssetCap,
		Nbf:             parseISOPtr(s.NotBefore),
		Exp:             parseISOPtr(s.Expires),
		Iat:             parseISOPtr(s.IssuedAt),
		DaysUntilExpiry: s.DaysUntilExpiry,
		LastError:       ptrIfNotEmpty(s.LastError),
		Iss:             ptrIfNotEmpty(s.Issuer),
		Path:            ptrIfNotEmpty(s.Path),
	}
}

func ptrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrIfNotZero(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func parseISOPtr(iso string) *time.Time {
	if iso == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return nil
	}
	return &t
}
