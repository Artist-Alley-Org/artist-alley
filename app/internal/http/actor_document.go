// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/remote"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// actorDocumentHandler serves the actor document (#1526; spec §5.2,
// §8.3) at ROOT GET /users/{username}, the actor URI every envelope
// names. Receivers fetch it once to pin the actor's Ed25519 key.
//
// Only paired peers get it. A request without a Signature header is
// handed to the web app's fallback (the SPA in production, a plain
// 404 in dev), so a browser at /users/<name> sees exactly what it saw
// before and an anonymous client learns nothing about which users or
// keys exist. A request with a Signature header must carry a valid
// instance HTTP signature of an enabled, connected peer
// (inbox.AuthenticatePeerRequest); otherwise 401. An invalid or
// reserved username, an unknown user or a user without a key is 404.
//
// Not under /api/v1 and not in the OpenAPI document, like
// /federation/inbox: it is a federation endpoint authenticated by
// HTTP signature, not a session API.
type actorDocumentHandler struct {
	pool    *pgxpool.Pool
	lookup  inbox.PeerLookup
	baseURL func(ctx context.Context) string
	clock   func() time.Time
	logger  *slog.Logger
	// fallback returns the handler for a request this endpoint does
	// not answer. Resolved per request so it is whatever the router's
	// not-found handler is once every route is mounted.
	fallback func() http.Handler
}

// actorDocumentMaxRequestBody caps the (expected empty) GET body the
// HTTP signature digest is computed over.
const actorDocumentMaxRequestBody = 4 << 10

func (h *actorDocumentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Signature") == "" {
		if h.fallback != nil {
			h.fallback().ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, actorDocumentMaxRequestBody+1))
		if err != nil || len(b) > actorDocumentMaxRequestBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		body = b
	}
	now := time.Now
	if h.clock != nil {
		now = h.clock
	}
	if _, err := inbox.AuthenticatePeerRequest(ctx, r, body, h.lookup, now()); err != nil {
		if errors.Is(err, inbox.ErrPeerUnauthenticated) {
			http.Error(w, "not signed by a paired peer", http.StatusUnauthorized)
			return
		}
		if h.logger != nil {
			h.logger.LogAttrs(ctx, slog.LevelError, "federation.actor_document.peer_lookup.error",
				slog.String("err", err.Error()))
		}
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	username := chi.URLParam(r, "username")
	if !inbox.ValidActorSegment(username) {
		http.NotFound(w, r)
		return
	}
	row, err := users.New(h.pool).GetActorSigningKeyByUsername(ctx, &username)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.SigningPublicKeyPem == nil) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		if h.logger != nil {
			h.logger.LogAttrs(ctx, slog.LevelError, "federation.actor_document.lookup.error",
				slog.String("err", err.Error()))
		}
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	base := ""
	if h.baseURL != nil {
		base = strings.TrimRight(h.baseURL(ctx), "/")
	}
	if base == "" {
		http.Error(w, "instance base URL not configured", http.StatusServiceUnavailable)
		return
	}
	// The actor URI is derived from the CURRENT base URL, the same way
	// activities/emit derives it for every envelope; the stored
	// "user".actor_uri is informational and not used.
	actorURI := base + "/users/" + username
	doc := remote.ActorDocument{
		ID:                actorURI,
		Type:              "Person",
		PreferredUsername: username,
		PublicKey: remote.ActorDocumentPubKey{
			ID:           actorURI + "#main-key",
			Owner:        actorURI,
			PublicKeyPEM: *row.SigningPublicKeyPem,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(doc)
}
