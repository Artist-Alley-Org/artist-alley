// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// HTTP handler for the licensing surface.
//
// Endpoints (Phase 1.17.O):
//
//	GET  /api/v1/admin/license/status     current license Status snapshot
//	POST /api/v1/admin/license/validate   dry-run verify without installing
//	POST /api/v1/admin/license/upload     install a new .lic file
//
// Future (Phase 1.17.O-3+):
//
//	GET  /api/v1/license/status           limited public view for users

package licensing

import (
	"context"
	"errors"
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

// ValidateAdminLicense — POST /api/v1/admin/license/validate
//
// Dry-run verify: parses the supplied envelope, runs the full verifier
// chain (envelope → signature → temporal window → issuer match), and
// returns the resulting Status WITHOUT writing anything to disk or
// touching the cached State. Lets the admin UI preview a candidate
// license before clicking "Install".
func (h *Handler) ValidateAdminLicense(
	ctx context.Context,
	req openapi.ValidateAdminLicenseRequestObject,
) (openapi.ValidateAdminLicenseResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ValidateAdminLicense401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	if !id.Can(auth.SuperAdminCapability) {
		return openapi.ValidateAdminLicense403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "system.admin capability required",
			},
		}, nil
	}
	if req.Body == nil {
		return openapi.ValidateAdminLicense400JSONResponse(buildValidateError(ErrBadEnvelope)), nil
	}
	claims, err := Verify(req.Body.LicenseText)
	if err != nil {
		return openapi.ValidateAdminLicense400JSONResponse(buildValidateError(err)), nil
	}
	// Cross-binding: dry-run check against the install's current
	// org.key so the operator sees the same outcome the upload would
	// give. We deliberately return 400 here (not 200 with OrgBound:false)
	// because UI behaviour matches: a license the install can't activate
	// is rejected, not previewed-then-broken.
	if claims.OrgPubkey != nil && *claims.OrgPubkey != "" {
		if err := VerifyOrgCrossBinding(*claims.OrgPubkey, h.state.OrgKeyPath()); err != nil {
			return openapi.ValidateAdminLicense400JSONResponse(buildValidateError(err)), nil
		}
	}
	// Reuse the same Status mapper the GET endpoint uses so the preview
	// renders identically to the post-install snapshot. Path comes from
	// the configured LicensePath even though we haven't written there
	// yet — that's the path the upload would target, so showing it is
	// useful diagnostic context.
	st := statusFromClaims(claims, h.state.Path(), h.state.OrgKeyPath())
	return openapi.ValidateAdminLicense200JSONResponse(statusToWire(st)), nil
}

// UploadAdminLicense — POST /api/v1/admin/license/upload
//
// Verifies the supplied envelope, persists it to LicensePath on
// success, and swaps the cached State. Verification runs BEFORE the
// write so a bad envelope can never overwrite a working file.
//
// A 409 is returned when the server was started with no LicensePath
// configured — the operator must set `AA_LICENSE_PATH` and restart
// before uploading.
func (h *Handler) UploadAdminLicense(
	ctx context.Context,
	req openapi.UploadAdminLicenseRequestObject,
) (openapi.UploadAdminLicenseResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UploadAdminLicense401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	if !id.Can(auth.SuperAdminCapability) {
		return openapi.UploadAdminLicense403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "system.admin capability required",
			},
		}, nil
	}
	if req.Body == nil {
		return openapi.UploadAdminLicense400JSONResponse(buildValidateError(ErrBadEnvelope)), nil
	}
	st, err := h.state.SaveAndReload(req.Body.LicenseText)
	switch {
	case errors.Is(err, ErrStateNil):
		return openapi.UploadAdminLicense409JSONResponse{
			Error: "no license path configured: set AA_LICENSE_PATH and restart",
		}, nil
	case isVerifierError(err):
		return openapi.UploadAdminLicense400JSONResponse(buildValidateError(err)), nil
	case err != nil:
		// Verified but the disk write failed — 500 so the admin sees
		// it as an infrastructure problem, not a bad license.
		if h.logger != nil {
			h.logger.Error("license upload write failed",
				slog.String("path", h.state.Path()),
				slog.String("err", err.Error()),
			)
		}
		return openapi.UploadAdminLicense500JSONResponse{
			Error: "license verified but write to disk failed: " + err.Error(),
		}, nil
	}
	if h.logger != nil {
		h.logger.Info("license uploaded",
			slog.String("path", h.state.Path()),
			slog.String("tier", st.Tier),
			slog.String("lid", st.LID),
		)
	}
	return openapi.UploadAdminLicense200JSONResponse(statusToWire(st)), nil
}

// isVerifierError reports whether err is one of the typed verifier
// sentinels — i.e. the envelope itself is bad. Anything else (disk
// errors, programming bugs) should NOT surface as a 400 to the UI.
func isVerifierError(err error) bool {
	switch {
	case errors.Is(err, ErrBadEnvelope),
		errors.Is(err, ErrBadSignature),
		errors.Is(err, ErrExpired),
		errors.Is(err, ErrNotYetValid),
		errors.Is(err, ErrNotInWindow),
		errors.Is(err, ErrUnknownKID),
		errors.Is(err, ErrWrongIssuer),
		errors.Is(err, ErrChainExpired),
		errors.Is(err, ErrChainBadSig),
		errors.Is(err, ErrChainScope),
		errors.Is(err, ErrChainKIDMismatch),
		errors.Is(err, ErrOrgKeyMissing),
		errors.Is(err, ErrOrgKeyBadFormat),
		errors.Is(err, ErrOrgKeyMismatch):
		return true
	}
	return false
}

// buildValidateError maps a verifier error into the wire-level
// LicenseValidateError. The `code` field is what the frontend
// switches on for i18n; `message` carries the raw error string so
// admins have something to grep logs for.
func buildValidateError(err error) openapi.LicenseValidateError {
	return openapi.LicenseValidateError{
		Error:   "license validation failed",
		Code:    validateErrorCode(err),
		Message: err.Error(),
	}
}

func validateErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrBadEnvelope):
		return "bad_envelope"
	case errors.Is(err, ErrBadSignature):
		return "bad_signature"
	case errors.Is(err, ErrExpired):
		return "expired"
	case errors.Is(err, ErrNotYetValid):
		return "not_yet_valid"
	case errors.Is(err, ErrNotInWindow):
		return "not_yet_valid"
	case errors.Is(err, ErrUnknownKID):
		return "unknown_publisher_key"
	case errors.Is(err, ErrWrongIssuer):
		return "wrong_issuer"
	case errors.Is(err, ErrChainExpired):
		return "chain_expired"
	case errors.Is(err, ErrChainBadSig):
		return "chain_bad_signature"
	case errors.Is(err, ErrChainScope):
		return "chain_scope"
	case errors.Is(err, ErrChainKIDMismatch):
		return "chain_kid_mismatch"
	case errors.Is(err, ErrOrgKeyMissing):
		return "org_key_missing"
	case errors.Is(err, ErrOrgKeyBadFormat):
		return "org_key_bad_format"
	case errors.Is(err, ErrOrgKeyMismatch):
		return "org_key_mismatch"
	}
	return "unknown"
}

// statusToWire converts our internal Status to the openapi-generated
// wire shape. The internal Status carries RFC3339 strings for the
// admin UI; the openapi schema declares the same fields as
// `format: date-time`, which the Go codegen renders as `*time.Time`.
// We re-parse here so the wire JSON serialises in the standard
// openapi-friendly form.
func statusToWire(s Status) openapi.LicenseStatus {
	return openapi.LicenseStatus{
		Loaded:             s.Loaded,
		Tier:               s.Tier,
		Features:           s.Features,
		Owner:              ptrIfNotEmpty(s.Owner),
		Org:                ptrIfNotEmpty(s.Org),
		Lid:                ptrIfNotEmpty(s.LID),
		Seats:              s.Seats,
		SeatWindowDays:     ptrIfNotZero(s.SeatWindowDays),
		AssetCap:           s.AssetCap,
		Nbf:                parseISOPtr(s.NotBefore),
		Exp:                parseISOPtr(s.Expires),
		Iat:                parseISOPtr(s.IssuedAt),
		DaysUntilExpiry:    s.DaysUntilExpiry,
		LastError:          ptrIfNotEmpty(s.LastError),
		Iss:                ptrIfNotEmpty(s.Issuer),
		Path:               ptrIfNotEmpty(s.Path),
		OrgBindingRequired: s.OrgBindingRequired,
		OrgBound:           s.OrgBound,
		OrgBindingError:    ptrIfNotEmpty(s.OrgBindingError),
		OrgKeyPath:         ptrIfNotEmpty(s.OrgKeyPath),
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
