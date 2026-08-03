// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package users implements the public user-profile surface.
//
// The legacy "user" table carries auth-bearing data we never expose;
// user_profiles (migration 00021) carries display-layer fields. Reads
// merge both; defaults substitute when no profile row exists. Federation:
// the profile row is what gets mirrored to peer sites.
package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const (
	CapEditAnyProfile = "users.profile.edit.any"
	CapSystemAdmin    = "system.admin"
	// CapUpdateSelfProfile is the per-user gate (Phase 1.17.F).
	// Seeded for the Base role by migration 00007 so every
	// existing user keeps the ability by default. An operator
	// can revoke it per-user (disciplinary lock-out) without
	// touching the handler's auth model.
	CapUpdateSelfProfile = "profile.update_self"
)

// updateBodyProbe adapts the openapi update body to the
// selfEditBodyProbe interface in selfedit.go. Lives here so the
// openapi import stays in handler.go (selfedit.go is openapi-free).
type updateBodyProbe struct{ body *openapi.UserProfileUpdate }

func (p updateBodyProbe) HasDisplayName() bool { return p.body != nil && p.body.DisplayName != nil }
func (p updateBodyProbe) HasBio() bool         { return p.body != nil && p.body.Bio != nil }
func (p updateBodyProbe) HasAvatarURL() bool   { return p.body != nil && p.body.AvatarUrl != nil }
func (p updateBodyProbe) HasLocation() bool    { return p.body != nil && p.body.Location != nil }
func (p updateBodyProbe) HasWebsiteURL() bool  { return p.body != nil && p.body.WebsiteUrl != nil }

// CacheDomain is the NOTIFY channel for per-user public-profile cache
// entries. Exported because cross-package writers (the posts handler
// on post create/delete, future federation imports) call
// [InvalidateProfile] which references it.
const CacheDomain = "user.profile"

// CacheDomainActorKeys keys the federation-keypair LRU. Hot
// surface on the federation hot path: every outbound activity
// signs with the actor's private key, and every inbound activity
// verifies against the actor's published public key. Each of
// those would otherwise be a DB roundtrip per envelope; cached,
// they become memory hits after warm-up.
//
// What's cached is the AT-REST FORM of the key material (the
// ActorKeyMaterial struct, with private keys still encrypted).
// Decryption happens on demand inside DecryptSigningPrivateKey /
// DecryptEncryptionPrivateKey so plaintext private keys live in
// memory only for the duration of the signing operation, not for
// the LRU's residency window.
const CacheDomainActorKeys = "user.actor_keys"

type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// byRef caches the closure-resolved openapi.UserPublic by
	// user_ref. nil-safe — nil means "no cache", every request
	// hits the DB. The by-username path doesn't cache (rare URL,
	// not worth the second-key bookkeeping); it always queries.
	byRef *cache.Cache[openapi.UserPublic]

	// actorKeys caches ActorKeyMaterial by userRef. Federation hot
	// path. Invalidated by EnsureActorKeyMaterial when it writes
	// new keys (rare; once per user lifetime at v1, since rotation
	// is deferred to 1.22.K).
	actorKeys *cache.Cache[ActorKeyMaterial]

	// Activities ledger writer + baseURL resolver (Phase 1.22.A-bis-4
	// per ADR 0044). When wired, UpdateUserProfile emits an Update
	// activity targeting the user's own actor so federated peers
	// can sync display_name / avatar / bio changes. nil-safe pre-
	// ADR-0044 fallback for tests.
	activities *activities.Writer
	baseURLFn  func(ctx context.Context) string

	// Audit is the typed audit recorder for lifecycle mutations
	// (Phase 1.17.B + onward — status changes, role assignments,
	// password resets). Nil-safe — tests that construct a bare
	// Handler can leave it unset and the audit calls degrade to
	// no-ops.
	Audit auditRecorder

	// state is the per-user state cache (Phase 1.17.A). 50k entries
	// fits the active-user set on any plausible install; LRU eviction
	// handles overflow. nil-safe — tests without a registry get a
	// Handler whose state path falls through to PG every time.
	state *UserStateCache

	// sessionRevoker is wired by api.go (Phase 1.17.A). Returns the
	// number of sessions cascade-revoked when a user transitions out
	// of UserStateActive. nil-safe — when unset (test fixtures that
	// don't need session cascading), the transition skips the cascade
	// silently.
	sessionRevoker SessionRevokerFn

	// selfEditGates is the per-field gate cache (Phase 1.17.F).
	// Read by UpdateUserProfile to enforce the operator-set
	// flags; invalidated by the admin write handler when an
	// operator toggles a gate. nil-safe.
	selfEditGates *selfEditGatesCache
}

// SessionRevokerFn is the dependency-inverted signature for cascading
// session revocation. Wired at boot to auth.SessionManager.RevokeAllForUser;
// kept as a closure on Handler so the users package doesn't import auth's
// session subsystem (the package already depends on auth for Identity;
// pulling in the session DB queries would invert the layering).
type SessionRevokerFn func(ctx context.Context, userRef int64) (int64, error)

// auditRecorder is the subset of *audit.Recorder this package needs.
// Interface form so tests can substitute a fake without dragging
// the full audit package's DB dependency in.
//
// Phase 1.17.A adds the four typed per-transition methods alongside
// the generic UserStatusChanged backstop. The new methods take the
// state values as strings (typed-state nouns: "pending", "active",
// etc.) rather than int — easier to read in the audit viewer and
// shields the audit layer from the int-magic legacy.
type auditRecorder interface {
	UserStatusChanged(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, next int64, reason string)
	AdminUserApproved(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, next, reason string)
	AdminUserDisabled(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, next, reason string)
	AdminUserArchived(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, next, reason string)
	AdminUserRestored(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, next, reason string)
	AdminUserRefusedLastAdmin(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, attempted, reason string)
	// Phase 1.17.D — field-level change recording via the
	// reflective helper. The interface form lets tests substitute
	// a recording fake without dragging in the audit package's
	// DB dependency.
	RecordChange(ctx context.Context, req *http.Request, eventType string, subject, actor *int64, before, after any, extra map[string]any)
}

func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{Pool: pool, Logger: logger}
	if registry != nil {
		// 5_000 entries comfortably fits ~1MB resident for typical
		// profile sizes and covers the hot end of any plausible
		// active-author set. Anything cold falls back to DB.
		h.byRef = cache.Register[openapi.UserPublic](registry, CacheDomain, 5_000)
		// Actor-key cache: ~200B per entry (PEMs + 32B raw keys +
		// AES-GCM ciphertext); 10k entries comfortably fits 2MB
		// and covers the active-federated-actor population on any
		// realistic install. Federation peers may push this higher
		// in 1.22.B-onward; LRU eviction handles overflow.
		h.actorKeys = cache.Register[ActorKeyMaterial](registry, CacheDomainActorKeys, 10_000)
	}
	// Per-user state cache (Phase 1.17.A). Internally nil-safe;
	// when registry is nil, h.state is nil + state reads fall
	// through to PG.
	h.state = newUserStateCache(registry)
	// Phase 1.17.F — per-field self-edit gates cache. Same nil-safe
	// pattern as state above.
	h.selfEditGates = newSelfEditGatesCache(registry)
	return h
}

// SetAuditRecorder wires the audit pipeline post-construction so the
// api.go composition can keep its existing NewHandler call without
// growing a positional argument every time we add an audit-emitting
// surface. Safe to call once at startup.
func (h *Handler) SetAuditRecorder(rec auditRecorder) {
	h.Audit = rec
}

// SetSessionRevoker wires the session-cascade closure (Phase 1.17.A).
// Same post-construction-setter pattern as SetAuditRecorder. Safe to
// call once at startup.
func (h *Handler) SetSessionRevoker(fn SessionRevokerFn) {
	h.sessionRevoker = fn
}

// SetActivitiesWriter installs the federation activity-ledger
// writer + baseURL resolver per ADR 0044. Mirrors the setters on
// posts.Handler / social.Handler / messages.Handler /
// collections.Handler.
func (h *Handler) SetActivitiesWriter(w *activities.Writer, baseURLFn func(ctx context.Context) string) {
	h.activities = w
	h.baseURLFn = baseURLFn
}

func derefOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ResolveUsername returns the username for the given user_ref,
// preferring the existing UserPublic cache (h.byRef) to avoid a
// DB roundtrip. Used by cross-package consumers — the federation
// activity emitters in social/messages/collections call this to
// build actor URIs without slamming the user table on every
// Like/Follow/DM/Block emission.
//
// Per docs/spec/federation/v1.md §8.4 the username is immutable
// from the federation perspective once an actor exists, so the
// cache hit rate is effectively 100% after warm-up. Returns empty
// string on miss + DB error (best-effort — caller treats this as
// "skip federated addressing for this user" and continues with
// the local-only emission).
func (h *Handler) ResolveUsername(ctx context.Context, userRef int64) string {
	// Cache-first.
	if h.byRef != nil {
		if hit, ok := h.byRef.Get(strconv.FormatInt(userRef, 10)); ok {
			return hit.Username
		}
	}
	// Cold path — single indexed read on the user table. We don't
	// hydrate the full UserPublic just for the username because
	// rowToAPI does extra joins (post_count, follower_count, etc.)
	// and we don't need those here.
	var username *string
	if err := h.Pool.QueryRow(ctx,
		`SELECT username FROM "user" WHERE ref = $1`, userRef,
	).Scan(&username); err != nil || username == nil {
		return ""
	}
	return *username
}

// InvalidateProfile broadcasts a cache invalidation for the given
// user's public-profile entry. Call after any DB write that could
// change what /users/{ref} returns:
//   - profile edits (UpsertUserProfile)
//   - posts.CreatePost / DeletePost (post_count is part of the cached
//     value, so author-side post mutations need to evict)
//
// Broadcast-only. Same-process callers without direct Cache access
// rely on the Registry's LISTEN goroutine to dispatch the eviction.
// Within this package, UpsertUserProfile uses byRef.Invalidate
// directly for immediate local eviction.
//
// Safe to call with a nil registry (no-op).
func InvalidateProfile(ctx context.Context, registry *cache.Registry, userRef int64) {
	if registry == nil {
		return
	}
	_ = registry.Emit(ctx, CacheDomain, strconv.FormatInt(userRef, 10))
}

// publicRow is the structural common shape of both GetUserPublicBy*
// sqlc result types. sqlc generates a distinct type per query so we
// adapt both into this shared shape before rendering.
type publicRow struct {
	UserRef               int64
	Username              *string
	Fullname              *string
	CreatedAt             pgtype.Timestamptz
	DisplayName           string
	Bio                   string
	AvatarURL             *string
	Location              string
	WebsiteURL            *string
	SocialLinks           []byte // raw JSONB
	Language              string
	Theme                 string
	HideFromAnonymous     bool
	ProfileOriginServerID pgtype.UUID
}

func fromByRef(r GetUserPublicByRefRow) publicRow {
	return publicRow{
		UserRef: r.UserRef, Username: r.Username, Fullname: r.Fullname,
		CreatedAt: r.CreatedAt, DisplayName: r.DisplayName, Bio: r.Bio,
		AvatarURL: r.AvatarUrl, Location: r.Location, WebsiteURL: r.WebsiteUrl,
		SocialLinks: r.SocialLinks, Language: r.Language, Theme: r.Theme,
		HideFromAnonymous:     r.HideFromAnonymous,
		ProfileOriginServerID: r.ProfileOriginServerID,
	}
}

func fromByUsername(r GetUserPublicByUsernameRow) publicRow {
	return publicRow{
		UserRef: r.UserRef, Username: r.Username, Fullname: r.Fullname,
		CreatedAt: r.CreatedAt, DisplayName: r.DisplayName, Bio: r.Bio,
		AvatarURL: r.AvatarUrl, Location: r.Location, WebsiteURL: r.WebsiteUrl,
		SocialLinks: r.SocialLinks, Language: r.Language, Theme: r.Theme,
		HideFromAnonymous:     r.HideFromAnonymous,
		ProfileOriginServerID: r.ProfileOriginServerID,
	}
}

// ---------------------------------------------------------------------------
// GetUserPublicByRef
// ---------------------------------------------------------------------------

func (h *Handler) GetUserPublicByRef(
	ctx context.Context,
	req openapi.GetUserPublicByRefRequestObject,
) (openapi.GetUserPublicByRefResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.GetUserPublicByRef401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	key := strconv.FormatInt(req.Ref, 10)
	if h.byRef != nil {
		if v, ok := h.byRef.Get(key); ok {
			return openapi.GetUserPublicByRef200JSONResponse(v), nil
		}
	}
	q := New(h.Pool)
	row, err := q.GetUserPublicByRef(ctx, req.Ref)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetUserPublicByRef404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("users: get by ref: %w", err)
	}
	out, err := h.rowToAPI(ctx, q, fromByRef(row), false)
	if err != nil {
		return nil, err
	}
	if h.byRef != nil {
		h.byRef.Add(key, *out)
	}
	return openapi.GetUserPublicByRef200JSONResponse(*out), nil
}

// ---------------------------------------------------------------------------
// GetUserPublicByUsername / GetUserPublicByRefPath — the public profile
// pages (#478, ADR 0070). Anonymous admission is decided upstream by the
// public-mode gate (auth.PublicSurfaceRoutes): with public mode off an
// anonymous request never reaches here (middleware 401s); with it on the
// caller arrives as anonymous. So there's no 401 here — instead the
// anonymous path (a) 404s when the owner opted out of anonymous exposure
// (ADR 0024, don't confirm existence) and (b) strips personal data beyond
// the display layer (no real name — ADR 0070 §3). The content lists are a
// separate owner-scoped browse (/assets, /collections, /posts).
// ---------------------------------------------------------------------------

func (h *Handler) GetUserPublicByUsername(
	ctx context.Context,
	req openapi.GetUserPublicByUsernameRequestObject,
) (openapi.GetUserPublicByUsernameResponseObject, error) {
	anonymous := auth.IdentityFromContext(ctx) == nil
	q := New(h.Pool)
	username := req.Username
	row, err := q.GetUserPublicByUsername(ctx, &username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetUserPublicByUsername404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("users: get by username: %w", err)
	}
	pr := fromByUsername(row)
	if anonymous && pr.HideFromAnonymous {
		return openapi.GetUserPublicByUsername404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
		}, nil
	}
	out, err := h.rowToAPI(ctx, q, pr, anonymous)
	if err != nil {
		return nil, err
	}
	return openapi.GetUserPublicByUsername200JSONResponse(*out), nil
}

func (h *Handler) GetUserPublicByRefPath(
	ctx context.Context,
	req openapi.GetUserPublicByRefPathRequestObject,
) (openapi.GetUserPublicByRefPathResponseObject, error) {
	anonymous := auth.IdentityFromContext(ctx) == nil
	q := New(h.Pool)
	row, err := q.GetUserPublicByRef(ctx, req.Ref)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetUserPublicByRefPath404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("users: get public by ref: %w", err)
	}
	pr := fromByRef(row)
	if anonymous && pr.HideFromAnonymous {
		return openapi.GetUserPublicByRefPath404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
		}, nil
	}
	out, err := h.rowToAPI(ctx, q, pr, anonymous)
	if err != nil {
		return nil, err
	}
	return openapi.GetUserPublicByRefPath200JSONResponse(*out), nil
}

// ---------------------------------------------------------------------------
// UpdateUserProfile
// ---------------------------------------------------------------------------

func (h *Handler) UpdateUserProfile(
	ctx context.Context,
	req openapi.UpdateUserProfileRequestObject,
) (openapi.UpdateUserProfileResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.UpdateUserProfile401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdateUserProfile400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	if caller.UserRef != req.Ref && !caller.Can(CapEditAnyProfile) && !caller.Can(CapSystemAdmin) {
		return openapi.UpdateUserProfile403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "cannot edit another user's profile"},
		}, nil
	}

	// Phase 1.17.F — self-edit operator gates.
	//
	// Apply ONLY on self-edit (caller == subject). Admin edits via
	// CapEditAnyProfile / CapSystemAdmin bypass the gates because
	// the gates exist to lock operator-controlled fields against
	// the user themselves; the operator can still write them.
	//
	// All-or-nothing: if any PATCHed field is gated off, reject
	// the entire request. Partial application would surprise
	// users + leave the form in an inconsistent state vs the
	// payload they submitted. The 422 response carries the
	// FIRST rejected field name; if more than one is gated, the
	// frontend re-renders + the user sees subsequent gates next
	// time they try.
	isSelfEdit := caller.UserRef == req.Ref
	if isSelfEdit {
		// profile.update_self capability gate. Bootstrap admin + Base
		// role have it by default (migration 00007); an operator who
		// wants to lock a user out of self-editing entirely can revoke
		// this capability.
		if !caller.Can(CapUpdateSelfProfile) && !caller.Can(CapSystemAdmin) {
			return openapi.UpdateUserProfile403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "profile.update_self capability required"},
			}, nil
		}
		if fge := h.checkSelfEditGates(ctx, updateBodyProbe{req.Body}); fge != nil {
			return openapi.UpdateUserProfile422JSONResponse{
				Error:  fge.Error(),
				Reason: openapi.FieldDisabledByOperator,
				Field:  string(fge.Field),
			}, nil
		}
	}

	q := New(h.Pool)
	existing, err := q.GetUserPublicByRef(ctx, req.Ref)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateUserProfile404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("users: existence check: %w", err)
	}

	// PATCH resolution. Present field overrides current; absent keeps.
	displayName := existing.DisplayName
	if req.Body.DisplayName != nil {
		displayName = *req.Body.DisplayName
	}
	bio := existing.Bio
	if req.Body.Bio != nil {
		bio = *req.Body.Bio
	}
	avatarURL := existing.AvatarUrl
	if req.Body.AvatarUrl != nil {
		v := *req.Body.AvatarUrl
		avatarURL = &v
	}
	location := existing.Location
	if req.Body.Location != nil {
		location = *req.Body.Location
	}
	websiteURL := existing.WebsiteUrl
	if req.Body.WebsiteUrl != nil {
		v := *req.Body.WebsiteUrl
		websiteURL = &v
	}
	socialLinks := existing.SocialLinks
	if req.Body.SocialLinks != nil {
		b, err := json.Marshal(*req.Body.SocialLinks)
		if err != nil {
			return openapi.UpdateUserProfile400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid social_links"},
			}, nil
		}
		socialLinks = b
	}
	language := existing.Language
	if req.Body.Language != nil {
		language = *req.Body.Language
	}
	// `system` is a real stored value, not a synonym for '' (#677).
	// '' means the account has no preference and each device falls back
	// to the app default; 'system' means every device follows its own
	// OS. Collapsing them would make an explicit "follow my OS" unable
	// to reach a second device — see migration 00033.
	theme := existing.Theme
	if req.Body.Theme != nil {
		switch string(*req.Body.Theme) {
		case "", "light", "dark", "system":
			theme = string(*req.Body.Theme)
		default:
			return openapi.UpdateUserProfile400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "theme must be '', 'light', 'dark', or 'system'"},
			}, nil
		}
	}
	// Opt-out from anonymous exposure (#478). PATCH: absent keeps the
	// existing value — otherwise an unrelated edit would silently reset
	// a user's opt-out to false.
	hideFromAnon := existing.HideFromAnonymous
	if req.Body.HideFromAnonymous != nil {
		hideFromAnon = *req.Body.HideFromAnonymous
	}

	// Gold-standard path: UpsertUserProfile + Update(Actor)
	// activity in one tx per AP §6.3 / §7.3. Only fires when the
	// caller is editing their OWN profile — admin-as-someone-else
	// edits get logged via audit, not activity (that's not a
	// federated social action by the caller; it's an
	// administrative override of someone else's data).
	if h.activities != nil && caller.UserRef == req.Ref && h.baseURLFn != nil {
		actorCtx := emit.ActorContext{
			UserRef:  caller.UserRef,
			Username: caller.Username,
			BaseURL:  h.baseURLFn(ctx),
		}
		snap := emit.ProfileSnapshot{
			DisplayName: derefOrEmpty(&displayName),
			Bio:         bio,
			AvatarURL:   derefOrEmpty(avatarURL),
			Location:    location,
			WebsiteURL:  derefOrEmpty(websiteURL),
		}
		em := emit.UpdateProfile(actorCtx, snap)
		err := h.activities.WithEmission(ctx, activities.EmissionInput{
			Activity: em.Activity,
		}, func(tx pgx.Tx) error {
			_, err := New(tx).UpsertUserProfile(ctx, UpsertUserProfileParams{
				UserRef:           req.Ref,
				DisplayName:       &displayName,
				Bio:               bio,
				AvatarUrl:         avatarURL,
				Location:          location,
				WebsiteUrl:        websiteURL,
				SocialLinks:       socialLinks,
				Language:          language,
				Theme:             theme,
				HideFromAnonymous: hideFromAnon,
			})
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("users: upsert profile: %w", err)
		}
	} else {
		// Legacy fallback: admin-edits-other (no activity) + the
		// test path (no activities writer wired).
		if _, err := q.UpsertUserProfile(ctx, UpsertUserProfileParams{
			UserRef:           req.Ref,
			DisplayName:       &displayName,
			Bio:               bio,
			AvatarUrl:         avatarURL,
			Location:          location,
			WebsiteUrl:        websiteURL,
			SocialLinks:       socialLinks,
			Language:          language,
			Theme:             theme,
			HideFromAnonymous: hideFromAnon,
		}); err != nil {
			return nil, fmt.Errorf("users: upsert profile: %w", err)
		}
	}

	// Phase 1.17.D — emit field-level changeset audit BEFORE the
	// cache invalidate so the audit row lands close-in-time with
	// the write. before is the snapshot we loaded at line 368;
	// after is the merged-PATCH local-state we just wrote.
	if h.Audit != nil {
		before := profileSnapshot{
			DisplayName: existing.DisplayName,
			Bio:         existing.Bio,
			AvatarURL:   derefOrEmpty(existing.AvatarUrl),
			Location:    existing.Location,
			WebsiteURL:  derefOrEmpty(existing.WebsiteUrl),
			Language:    existing.Language,
			Theme:       existing.Theme,
		}
		after := profileSnapshot{
			DisplayName: displayName,
			Bio:         bio,
			AvatarURL:   derefOrEmpty(avatarURL),
			Location:    location,
			WebsiteURL:  derefOrEmpty(websiteURL),
			Language:    language,
			Theme:       theme,
		}
		actor := caller.UserRef
		subject := req.Ref
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventUserProfileUpdated,
			&subject, &actor,
			before, after, nil)
	}

	// The cached entry (if any) just went stale. Local-evict +
	// broadcast in one Invalidate call — peer instances and the
	// LISTEN goroutine pick up via NOTIFY.
	if h.byRef != nil {
		_ = h.byRef.Invalidate(ctx, strconv.FormatInt(req.Ref, 10))
	}

	row, err := q.GetUserPublicByRef(ctx, req.Ref)
	if err != nil {
		return nil, fmt.Errorf("users: refetch: %w", err)
	}
	out, err := h.rowToAPI(ctx, q, fromByRef(row), false)
	if err != nil {
		return nil, err
	}
	if h.byRef != nil {
		h.byRef.Add(strconv.FormatInt(req.Ref, 10), *out)
	}
	return openapi.UpdateUserProfile200JSONResponse(*out), nil
}

// profileSnapshot is the diffable shape for the Phase 1.17.D
// user.profile_updated event. Carries only the operator-visible
// profile fields — no password, no actor keys, no system-managed
// metadata. The benign-only-field invariant means the
// changeset.go sensitive-pattern backstop has nothing to strip
// here (all fields pass through cleanly).
type profileSnapshot struct {
	DisplayName string
	Bio         string
	AvatarURL   string
	Location    string
	WebsiteURL  string
	Language    string
	Theme       string
}

// ---------------------------------------------------------------------------
// Row → API
// ---------------------------------------------------------------------------

// rowToAPI maps the merged user+profile row into the public API shape,
// resolving display_name precedence and computing post_count.
//
// Precedence for display_name (the always-non-empty resolved string):
//  1. profile.display_name (if non-empty)
//  2. user.fullname (if non-empty)
//  3. user.username
//
// The frontend never has to do this resolution itself.
func (h *Handler) rowToAPI(ctx context.Context, q *Queries, r publicRow, anonymous bool) (*openapi.UserPublic, error) {
	display := r.DisplayName
	// An anonymous viewer must never see the real name (ADR 0070 §3) —
	// not directly, and not smuggled through the display_name fallback.
	// So skip the fullname rung for anonymous: display_name → username.
	if display == "" && !anonymous && r.Fullname != nil && *r.Fullname != "" {
		display = *r.Fullname
	}
	if display == "" && r.Username != nil {
		display = *r.Username
	}
	if display == "" {
		display = fmt.Sprintf("user %d", r.UserRef)
	}

	postCount, err := q.CountPostsByAuthor(ctx, r.UserRef)
	if err != nil {
		return nil, fmt.Errorf("users: count posts: %w", err)
	}

	// social_links is stored as raw JSONB bytes; decode into a map for
	// the API response. Empty / NULL just renders as an empty map.
	var socialMap map[string]string
	if len(r.SocialLinks) > 0 {
		if err := json.Unmarshal(r.SocialLinks, &socialMap); err != nil {
			// Tolerate malformed rows — return empty rather than 500.
			socialMap = map[string]string{}
		}
	}

	out := openapi.UserPublic{
		Ref:         r.UserRef,
		DisplayName: display,
		Bio:         &r.Bio,
		Location:    &r.Location,
		AvatarUrl:   r.AvatarURL,
		WebsiteUrl:  r.WebsiteURL,
		MemberSince: r.CreatedAt.Time,
		PostCount:   postCount,
	}
	if r.Username != nil {
		out.Username = *r.Username
	}
	// Real name is authenticated-only (ADR 0070 §3).
	if !anonymous && r.Fullname != nil && *r.Fullname != "" {
		out.Fullname = r.Fullname
	}
	if len(socialMap) > 0 {
		m := socialMap
		out.SocialLinks = &m
	}
	if r.Language != "" {
		l := r.Language
		out.Language = &l
	}
	if r.Theme != "" {
		t := openapi.UserPublicTheme(r.Theme)
		out.Theme = &t
	}
	if r.ProfileOriginServerID.Valid {
		v := openapi_types.UUID(r.ProfileOriginServerID.Bytes)
		out.OriginServerId = &v
	}
	return &out, nil
}

// Compile-time strict-server interface assertion.
var _ interface {
	GetUserPublicByRef(context.Context, openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error)
	GetUserPublicByRefPath(context.Context, openapi.GetUserPublicByRefPathRequestObject) (openapi.GetUserPublicByRefPathResponseObject, error)
	GetUserPublicByUsername(context.Context, openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error)
	UpdateUserProfile(context.Context, openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error)
} = (*Handler)(nil)
