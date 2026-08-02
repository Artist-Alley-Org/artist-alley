// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package sitetext is the operator override layer over the shipped UI
// string catalogue (#794, ADR 0081 §1).
//
// Every string the frontend renders comes from a dotted i18n key in
// web/src/lib/i18n/en.json, bundled into the build. An operator who
// wants "Collections" to read "Libraries" — or who simply wants to fix
// confusing wording — had to fork and rebuild. This package stores
// per-(key, language) replacements as data, serves them on a public
// read, and lets the client resolve them over the bundled dictionary at
// render time.
//
// Three deliberate shapes, all from ADR 0081 §1:
//
//   - The identity is the i18n key, not a page+name composite. Our
//     strings already have a global dotted namespace; the composite the
//     prior art uses exists to compensate for not having one.
//   - `language` is part of the key. Translation is live (#289) and a
//     locale-blind override would apply English to every locale.
//   - A write naming a key the catalogue does not define is REFUSED,
//     not stored. An override that silently does nothing is the exact
//     failure mode #774 established we do not ship — and it is worse
//     here than there, because the operator has no way to tell whether
//     they mistyped a key or the feature is broken.
//
// Site text does not federate: it is instance identity, how THIS
// installation speaks, and a peer receiving content must not receive
// the operator's wording.
package sitetext

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Capabilities. Reads are ungated — see [Handler.All]. Writes take the
// existing config-surface capability rather than inventing a new one:
// an operator who may rewrite the site's wording is doing the same kind
// of thing as one who may rename the site (#718).
const (
	CapConfigWrite = "system.config.write"
	CapSystemAdmin = "system.admin"
)

// ErrUnknownKey is returned by Set when the key is absent from the
// embedded shipped catalogue. Mapped to HTTP 422 with the key named in
// the message.
var ErrUnknownKey = errors.New("sitetext: key is not in the shipped catalogue")

// ErrNotFound is returned by Delete when no override matched. Mapped to
// HTTP 404 — reverting something that was never overridden is a
// mistake worth reporting, not a silent no-op.
var ErrNotFound = errors.New("sitetext: override not found")

// Handler is the package's domain surface.
type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	cache *Cache
}

// NewHandler builds the Handler. The cache may be nil, in which case
// every read hits the database — useful in tests that do not stand up a
// registry, and harmless in production because api.go always wires one.
func NewHandler(pool *pgxpool.Pool, c *Cache, logger *slog.Logger) *Handler {
	return &Handler{Pool: pool, Logger: logger, cache: c}
}

// All returns the whole override map, language → key → value.
//
// Cached under a single key and rebuilt wholesale on a miss. There is
// no per-caller variation to key on: this is the same map for every
// visitor, signed in or not, which is also why the HTTP layer serves it
// anonymously — a logged-out visitor reads the same navbar as everyone
// else, and gating it would mean the operator's wording appeared only
// after sign-in.
//
// The returned map is READ-ONLY: on a hit it IS the cached value, so
// mutating it would corrupt what every other reader sees. It is never
// nil — a fresh install returns an empty map, so the JSON is `{}` and
// no client has to special-case "nothing overridden yet".
func (h *Handler) All(ctx context.Context) (Overrides, error) {
	if h.cache != nil {
		if hit, ok := h.cache.Map.Get(CacheKeyAll); ok {
			return hit, nil
		}
	}
	rows, err := New(h.Pool).ListSiteText(ctx)
	if err != nil {
		return nil, fmt.Errorf("sitetext: list: %w", err)
	}
	out := make(Overrides, 4)
	for _, r := range rows {
		byKey, ok := out[r.Language]
		if !ok {
			byKey = make(map[string]string)
			out[r.Language] = byKey
		}
		byKey[r.Key] = r.Value
	}
	if h.cache != nil {
		h.cache.Map.Add(CacheKeyAll, out)
	}
	return out, nil
}

// Set writes one override and invalidates the cached map.
//
// The key check is here rather than only in the admin UI because the
// UI is not a boundary: anyone holding `system.config.write` can call
// the endpoint directly, and ADR 0081's fail-loud rule has to hold for
// them too.
func (h *Handler) Set(ctx context.Context, key, language, value string, userRef *int64) (SiteText, error) {
	if !KnownKey(key) {
		return SiteText{}, fmt.Errorf("%w: %q", ErrUnknownKey, key)
	}
	row, err := New(h.Pool).UpsertSiteText(ctx, UpsertSiteTextParams{
		Key:              key,
		Language:         language,
		Value:            value,
		UpdatedByUserRef: userRef,
	})
	if err != nil {
		return SiteText{}, fmt.Errorf("sitetext: upsert %q/%q: %w", key, language, err)
	}
	h.invalidate(ctx)
	return row, nil
}

// Delete removes one override, reverting the string to what ships.
func (h *Handler) Delete(ctx context.Context, key, language string) error {
	n, err := New(h.Pool).DeleteSiteText(ctx, DeleteSiteTextParams{Key: key, Language: language})
	if err != nil {
		return fmt.Errorf("sitetext: delete %q/%q: %w", key, language, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	h.invalidate(ctx)
	return nil
}

// invalidate is the one place a write announces itself. Kept private so
// no caller can write without invalidating — the stale-cache class
// #845 exists to retrofit elsewhere is avoided here by construction,
// not by remembering.
func (h *Handler) invalidate(ctx context.Context) {
	if h.cache == nil {
		return
	}
	h.cache.Invalidate(ctx)
}
