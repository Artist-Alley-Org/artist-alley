// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-h HTTP surface for the rotation + admin observability
// endpoints.
//
// Three operations, one handler:
//
//   - POST /account/security/rotate-federation-keys     (self-rotation)
//   - POST /admin/federation/users/{ref}/rotate-keys    (admin recovery)
//   - GET  /admin/federation/key-health                 (dashboard tile data)
//
// All three live in the userkeys package because they share the
// rotation primitive + the queries.sql layer. The package-level
// audit hooks fire as part of the underlying primitives — the
// handler is purely the HTTP adaptation layer.
//
// # Why no separate account/security_handler.go
//
// The brief originally sketched one. Two reasons it folds into
// userkeys.AdminHandler instead: (1) the self-rotation endpoint
// is a thin wrapper over the same primitive the admin recovery
// path calls, sharing the sysconfig retention lookup + the
// audit firing; (2) the auth package doesn't currently import
// federation/userkeys, and adding the edge for one delegation
// just to call back into userkeys is more wiring than the
// problem warrants.
//
// Self-rotation is still gated on auth (the Identity check at
// the top of the method) — the placement-in-userkeys is a code
// organisation choice, not a security tier change.

package userkeys

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const (
	capAdmin = "system.admin"
	// capRead gates the key-health READ (#356). Admin-initiated key
	// rotation stays on capAdmin; self-rotation is the user's own.
	capRead = "federation.read"
)

// RetentionDaysSysconfigKey is the system_config row that holds
// the default retained_until grace window (in days). NO migration
// seeds it — a fresh install has no such row, and the fallback is
// [DefaultRetentionDays] in-binary until an admin writes one via the
// sysconfig admin UI. (This doc used to name a migration as the
// source of a default 30; nothing in db/migrations inserts this key.)
// The rotation primitive reads this before each call so an updated
// setting takes effect on the next rotation without a restart.
const RetentionDaysSysconfigKey = "federation.user_keys.retained_until_days"

// AdminHandler owns the three I-h HTTP endpoints. Construct once
// at boot via [NewAdminHandler]; safe for concurrent use.
type AdminHandler struct {
	pool   *pgxpool.Pool
	audit  *audit.Recorder
	logger *slog.Logger
}

// NewAdminHandler builds the handler. auditRec may be nil — the
// rotation + dashboard endpoints will skip their audit emits
// (test fixtures). pool MUST be non-nil; nil triggers a fail-
// fast at the first request via the rotation primitive's own
// guard.
func NewAdminHandler(
	pool *pgxpool.Pool,
	auditRec *audit.Recorder,
	logger *slog.Logger,
) *AdminHandler {
	return &AdminHandler{
		pool:   pool,
		audit:  auditRec,
		logger: logger,
	}
}

// retentionDays reads the sysconfig override + falls back to
// DefaultRetentionDays. Returns the resolved value + any
// transient error from the read; callers fall back to the
// default on error so a sysconfig outage doesn't block
// rotation.
func (h *AdminHandler) retentionDays(ctx context.Context) int {
	var raw []byte
	err := h.pool.QueryRow(ctx,
		`SELECT value FROM system_config WHERE key = $1`,
		RetentionDaysSysconfigKey,
	).Scan(&raw)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && h.logger != nil {
			h.logger.LogAttrs(ctx, slog.LevelWarn,
				"userkeys.admin.retention_days.lookup_error",
				slog.String("err", err.Error()),
			)
		}
		return DefaultRetentionDays
	}
	var days int
	if err := json.Unmarshal(raw, &days); err != nil || days <= 0 {
		return DefaultRetentionDays
	}
	return days
}

// rotateAuditHook returns a closure binding the recorder so
// RotateForUser fires federation.user.key_rotated. Nil-safe
// (returns a nil RotateAuditFireFn when the recorder is unset).
func (h *AdminHandler) rotateAuditHook() RotateAuditFireFn {
	if h.audit == nil {
		return nil
	}
	return h.audit.FederationUserKeyRotated
}

// resultToAPI shapes a RotationResult into the OpenAPI response.
// Centralised so both rotate endpoints render identically + the
// base64 + days fields stay in one place.
func (h *AdminHandler) resultToAPI(result *RotationResult, retentionDays int) openapi.UserKeyRotationResult {
	prev := result.PreviousVersion
	days := int32(retentionDays)
	out := openapi.UserKeyRotationResult{
		UserRef:           result.UserRef,
		NewVersion:        result.NewVersion,
		PreviousVersion:   &prev,
		NewPublicKeyB64:   base64.StdEncoding.EncodeToString(result.NewPublicKey),
		Algorithm:         result.Algorithm,
		RetainedUntilDays: &days,
	}
	return out
}

// RotateOwnFederationKeys — POST /account/security/rotate-federation-keys.
// Self-rotation: subject + rotatedByUserRef both = caller.
func (h *AdminHandler) RotateOwnFederationKeys(
	ctx context.Context,
	_ openapi.RotateOwnFederationKeysRequestObject,
) (openapi.RotateOwnFederationKeysResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.RotateOwnFederationKeys401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	days := h.retentionDays(ctx)
	result, err := RotateForUser(ctx, h.pool, id.UserRef, id.UserRef,
		days, h.rotateAuditHook())
	if err != nil {
		return nil, fmt.Errorf("rotate own keys: %w", err)
	}
	return openapi.RotateOwnFederationKeys200JSONResponse(h.resultToAPI(result, days)), nil
}

// RotateUserFederationKeysAsAdmin — POST
// /admin/federation/users/{ref}/rotate-keys. Compromised-key
// recovery: subject = target user; rotatedByUserRef = admin.
func (h *AdminHandler) RotateUserFederationKeysAsAdmin(
	ctx context.Context,
	req openapi.RotateUserFederationKeysAsAdminRequestObject,
) (openapi.RotateUserFederationKeysAsAdminResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.RotateUserFederationKeysAsAdmin401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.RotateUserFederationKeysAsAdmin403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}

	// Confirm the subject user actually exists before generating
	// keypair material we'd then have to roll back. Tx isolation
	// would let us bundle the check, but the upfront cost is
	// negligible + the cleaner failure mode is worth it.
	var exists bool
	if err := h.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM "user" WHERE ref = $1)`,
		req.Ref,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("user existence check: %w", err)
	}
	if !exists {
		return openapi.RotateUserFederationKeysAsAdmin404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
		}, nil
	}

	days := h.retentionDays(ctx)
	result, err := RotateForUser(ctx, h.pool, req.Ref, id.UserRef,
		days, h.rotateAuditHook())
	if err != nil {
		return nil, fmt.Errorf("rotate user keys (admin): %w", err)
	}
	return openapi.RotateUserFederationKeysAsAdmin200JSONResponse(h.resultToAPI(result, days)), nil
}

// GetFederationKeyHealth — GET /admin/federation/key-health.
// One aggregate query for the dashboard top-of-page + two
// drill-down lists (users missing keypair, recent rotations) so
// the page renders without separate fetches.
func (h *AdminHandler) GetFederationKeyHealth(
	ctx context.Context,
	_ openapi.GetFederationKeyHealthRequestObject,
) (openapi.GetFederationKeyHealthResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetFederationKeyHealth401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capRead) && !id.Can(capAdmin) {
		return openapi.GetFederationKeyHealth403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: capRead + " capability required"},
		}, nil
	}

	q := New(h.pool)

	summary, err := q.GetKeyHealthSummary(ctx)
	if err != nil {
		return nil, fmt.Errorf("key health summary: %w", err)
	}
	missing, err := q.ListUsersMissingKeypair(ctx)
	if err != nil {
		return nil, fmt.Errorf("users missing keypair: %w", err)
	}
	rotations, err := q.ListRecentRotations(ctx)
	if err != nil {
		return nil, fmt.Errorf("recent rotations: %w", err)
	}

	missingAPI := make([]openapi.UserMissingKeypair, len(missing))
	for i, m := range missing {
		row := openapi.UserMissingKeypair{
			Ref: m.Ref,
		}
		if m.Username != nil {
			row.Username = m.Username
		}
		row.Created = pgTimeToAPI(m.Created)
		missingAPI[i] = row
	}

	rotationsAPI := make([]openapi.FederationKeyRotationEvent, 0, len(rotations))
	for _, r := range rotations {
		// rotated_at is the WHERE-clause filter (non-NULL only)
		// in ListRecentRotations; this nil check is defense in
		// depth.
		if !r.RotatedAt.Valid {
			continue
		}
		rotationsAPI = append(rotationsAPI, openapi.FederationKeyRotationEvent{
			UserRef:          r.UserRef,
			Version:          r.Version,
			RotatedAt:        r.RotatedAt.Time,
			RotatedByUserRef: r.RotatedByUserRef,
		})
	}

	resp := openapi.FederationKeyHealth{
		UsersTotal:                summary.UsersTotal,
		UsersMissingKeypair:       summary.UsersMissingKeypair,
		RemoteActorsMissingEncKey: summary.RemoteActorsMissingEncKey,
		PeersMissingCapabilities:  summary.PeersMissingCapabilities,
		RetainedKeysNearExpiry:    summary.RetainedKeysNearExpiry,
		RecentRotations:           &rotationsAPI,
		UsersMissingKeypairSample: &missingAPI,
	}
	return openapi.GetFederationKeyHealth200JSONResponse(resp), nil
}

// pgTimeToAPI converts pgtype.Timestamptz to *time.Time —
// the shape oapi-codegen renders for nullable date-time fields.
// Returns nil when the column is SQL NULL.
func pgTimeToAPI(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
