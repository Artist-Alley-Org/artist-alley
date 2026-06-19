// Phase 1.17.F — per-field self-edit gates for /account/profile.
//
// Operators occasionally need to lock specific profile fields
// (e.g., "display_name is mapped from HR, users can't edit it
// themselves"). Migration 00007 seeded five system_config rows
// + the profile.update_self capability + the Base-role binding.
//
// This file owns the typed surface:
//
//   * SelfEditField — typed identifier (closed set per ADR 0042)
//   * SelfEditGates — the loaded snapshot of all five flags
//   * Handler.CanEditField — cache-fronted per-field check
//   * Handler.InvalidateSelfEditGates — called from the admin
//     write path so the next read picks up the new value
//   * FieldGateError — typed error wrapping the rejected field;
//     the API layer maps to HTTP 422 with {reason, field}
//
// # Fail-open semantics
//
// If the system_config key is missing OR the read errors OR the
// JSON unmarshals to something non-bool, the gate returns true
// (editable). Rationale: preserves current behavior — a partial
// migration / corrupted row shouldn't lock everyone out. The
// alternative (fail-closed) would surprise operators when they
// downgrade or when a seed was skipped.

package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// SelfEditField is the typed identifier for each gateable profile
// field. Closed set — adding a new field means adding the const,
// the system_config seed in a new migration, and the handler
// branch in UpdateUserProfile.
type SelfEditField string

const (
	SelfEditDisplayName SelfEditField = "display_name"
	SelfEditBio         SelfEditField = "bio"
	SelfEditAvatarURL   SelfEditField = "avatar_url"
	SelfEditLocation    SelfEditField = "location"
	SelfEditWebsiteURL  SelfEditField = "website_url"
)

// AllSelfEditFields is the canonical enumeration. The admin
// surface iterates this to render toggles; the GET endpoint
// returns every key so the frontend gets a complete picture even
// when a seed row is missing (treated as default true).
var AllSelfEditFields = []SelfEditField{
	SelfEditDisplayName,
	SelfEditBio,
	SelfEditAvatarURL,
	SelfEditLocation,
	SelfEditWebsiteURL,
}

const cacheDomainSelfEditGates = "user.self_edit_gates"
const selfEditKeyPrefix = "users.allow_self_edit."

// SelfEditGates is the snapshot — one bool per field. Missing
// entries are treated as true (fail-open).
type SelfEditGates struct {
	DisplayName bool `json:"display_name"`
	Bio         bool `json:"bio"`
	AvatarURL   bool `json:"avatar_url"`
	Location    bool `json:"location"`
	WebsiteURL  bool `json:"website_url"`
}

// Allows returns whether the given field is editable per the
// current gate set. Mirrors a flat map access; kept as a typed
// method so the call sites stay grep-able.
func (g SelfEditGates) Allows(field SelfEditField) bool {
	switch field {
	case SelfEditDisplayName:
		return g.DisplayName
	case SelfEditBio:
		return g.Bio
	case SelfEditAvatarURL:
		return g.AvatarURL
	case SelfEditLocation:
		return g.Location
	case SelfEditWebsiteURL:
		return g.WebsiteURL
	}
	return true // unknown field → fail-open
}

// selfEditGatesCache wraps cache.Cache for the all-fields-in-one-
// row payload. Key is the literal "_" sentinel because there's
// only one set globally — using a per-field key would force five
// LRU lookups per profile save.
type selfEditGatesCache struct {
	mu    sync.Mutex
	c     *cache.Cache[SelfEditGates]
	regOK bool
}

const selfEditGatesCacheKey = "_"

func newSelfEditGatesCache(registry *cache.Registry) *selfEditGatesCache {
	if registry == nil {
		return &selfEditGatesCache{}
	}
	return &selfEditGatesCache{
		c:     cache.Register[SelfEditGates](registry, cacheDomainSelfEditGates, 1),
		regOK: true,
	}
}

// CanEditField returns whether the operator currently allows
// self-edit of the given field. Cache-fronted; on miss, loads
// every gate row in a single query and caches the merged
// SelfEditGates struct.
//
// Errors fail-open: a DB hiccup or missing rows reads true
// (editable) so a transient outage doesn't lock users out of
// their own profile.
func (h *Handler) CanEditField(ctx context.Context, field SelfEditField) bool {
	g, _ := h.LoadSelfEditGates(ctx)
	return g.Allows(field)
}

// LoadSelfEditGates returns the snapshot of all five gates.
// Cache-fronted; populates on miss via GetSelfEditGates.
// Returned error is best-effort — the gates struct is always
// usable (fail-open on missing rows).
func (h *Handler) LoadSelfEditGates(ctx context.Context) (SelfEditGates, error) {
	if h.selfEditGates != nil && h.selfEditGates.c != nil {
		if cached, ok := h.selfEditGates.c.Get(selfEditGatesCacheKey); ok {
			return cached, nil
		}
	}
	gates, err := h.loadSelfEditGatesFromDB(ctx)
	// Even on err we cache the (zero) struct? No — on err we
	// return fail-open without caching so a transient DB hiccup
	// doesn't pin a wrong value.
	if err == nil && h.selfEditGates != nil && h.selfEditGates.c != nil {
		h.selfEditGates.c.Add(selfEditGatesCacheKey, gates)
	}
	return gates, err
}

// loadSelfEditGatesFromDB reads all 5 gate rows in one query.
// Missing rows default to true (fail-open).
func (h *Handler) loadSelfEditGatesFromDB(ctx context.Context) (SelfEditGates, error) {
	// Start fail-open — every field defaults to true. Missing
	// rows leave the default in place.
	gates := SelfEditGates{
		DisplayName: true, Bio: true, AvatarURL: true,
		Location: true, WebsiteURL: true,
	}
	rows, err := h.Pool.Query(ctx,
		`SELECT key, value FROM system_config WHERE key LIKE $1`,
		selfEditKeyPrefix+"%",
	)
	if err != nil {
		return gates, fmt.Errorf("users: load self-edit gates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			if h.Logger != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn,
					"users.selfedit.scan.failed",
					slog.String("err", err.Error()),
				)
			}
			continue
		}
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			// Garbage row — leave the default (true) in place.
			continue
		}
		fieldName := strings.TrimPrefix(key, selfEditKeyPrefix)
		switch SelfEditField(fieldName) {
		case SelfEditDisplayName:
			gates.DisplayName = v
		case SelfEditBio:
			gates.Bio = v
		case SelfEditAvatarURL:
			gates.AvatarURL = v
		case SelfEditLocation:
			gates.Location = v
		case SelfEditWebsiteURL:
			gates.WebsiteURL = v
		}
	}
	return gates, nil
}

// InvalidateSelfEditGates clears the cached snapshot + broadcasts
// the eviction so peer instances pick up the new gate values on
// their next read. Called by the admin write handler when an
// operator toggles any gate.
//
// nil-safe: when no registry was wired (test fixtures), this is
// a no-op.
func (h *Handler) InvalidateSelfEditGates(ctx context.Context) {
	if h.selfEditGates == nil || h.selfEditGates.c == nil {
		return
	}
	if err := h.selfEditGates.c.Invalidate(ctx, selfEditGatesCacheKey); err != nil && h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn,
			"users.selfedit.cache.invalidate.failed",
			slog.String("err", err.Error()),
		)
	}
}

// FieldGateError is the typed error UpdateUserProfile returns
// when a PATCHed field is locked by the operator gates. The
// API layer maps to HTTP 422 with {reason: "field_disabled_by_
// operator", field: <name>}.
//
// Wraps the sentinel ErrFieldDisabledByOperator so callers can
// errors.Is() against the sentinel without unwrapping for the
// field name.
type FieldGateError struct {
	Field SelfEditField
}

func (e *FieldGateError) Error() string {
	return fmt.Sprintf("users: field %q disabled by operator", e.Field)
}

// Unwrap exposes the sentinel for errors.Is().
func (e *FieldGateError) Unwrap() error { return ErrFieldDisabledByOperator }

// ErrFieldDisabledByOperator is the sentinel callers check via
// errors.Is(). The wrapping FieldGateError carries the specific
// field name.
var ErrFieldDisabledByOperator = errors.New("users: field disabled by operator")

// checkSelfEditGates loads the gate snapshot + walks the PATCH
// body to verify every present field is editable. Returns the
// first gated-off field as a *FieldGateError (the all-or-nothing
// semantics + the first-fail return — see UpdateUserProfile's
// call site for rationale).
//
// Returns nil when every PATCHed field passes (including the
// case where no gateable field is present, e.g. a theme-only
// edit).
//
// req is the openapi update body. We accept it as an interface
// to avoid an import cycle (selfedit.go can't import openapi
// without breaking the open-codegen pattern). The accessor
// methods below match the openapi.UpdateUserProfileJSONBody
// shape.
func (h *Handler) checkSelfEditGates(ctx context.Context, body selfEditBodyProbe) *FieldGateError {
	gates, _ := h.LoadSelfEditGates(ctx)
	if body.HasDisplayName() && !gates.Allows(SelfEditDisplayName) {
		return &FieldGateError{Field: SelfEditDisplayName}
	}
	if body.HasBio() && !gates.Allows(SelfEditBio) {
		return &FieldGateError{Field: SelfEditBio}
	}
	if body.HasAvatarURL() && !gates.Allows(SelfEditAvatarURL) {
		return &FieldGateError{Field: SelfEditAvatarURL}
	}
	if body.HasLocation() && !gates.Allows(SelfEditLocation) {
		return &FieldGateError{Field: SelfEditLocation}
	}
	if body.HasWebsiteURL() && !gates.Allows(SelfEditWebsiteURL) {
		return &FieldGateError{Field: SelfEditWebsiteURL}
	}
	return nil
}

// selfEditBodyProbe is the minimal interface UpdateUserProfile's
// body needs to satisfy for the gate check. Implemented inline
// at the call site so the openapi-generated body type doesn't
// have to grow methods.
type selfEditBodyProbe interface {
	HasDisplayName() bool
	HasBio() bool
	HasAvatarURL() bool
	HasLocation() bool
	HasWebsiteURL() bool
}
