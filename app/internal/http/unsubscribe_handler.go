// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/email"
	"github.com/mscrnt/artist-alley/app/internal/userprefs"
)

// unsubscribeHandler implements RFC 8058 one-click unsubscribe (Phase
// 1.55.Y). The List-Unsubscribe header on every notification email
// points at GET/POST /api/v1/unsubscribe?token=<signed>. The signed
// token IS the authorization — no session required — so a recipient can
// unsubscribe straight from their mail client.
//
//   - POST → the RFC 8058 one-click target. Verify + apply, 200 JSON.
//   - GET  → a human clicked the link in a browser. Verify + apply,
//     serve a small self-contained HTML confirmation.
//
// Applying is idempotent (drops the email channel for the token's
// topic, or all topics for the "__all__" digest token), so a double
// hit is harmless.
type unsubscribeHandler struct {
	scrambleKey string
	prefs       *userprefs.Handler
	logger      *slog.Logger
	now         func() time.Time
}

func (h *unsubscribeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	token := r.URL.Query().Get("token")
	ref, topic, err := email.VerifyUnsubscribe(h.scrambleKey, token, now())
	if err != nil {
		h.respond(w, r, false)
		return
	}
	if err := h.prefs.UnsubscribeEmail(r.Context(), ref, topic); err != nil {
		if h.logger != nil {
			h.logger.LogAttrs(r.Context(), slog.LevelWarn, "unsubscribe.apply_failed",
				slog.Int64("user_ref", ref),
				slog.String("topic", topic),
				slog.String("err", err.Error()))
		}
		h.respond(w, r, false)
		return
	}
	h.respond(w, r, true)
}

func (h *unsubscribeHandler) respond(w http.ResponseWriter, r *http.Request, ok bool) {
	if r.Method == http.MethodPost {
		status := http.StatusOK
		body := map[string]string{"status": "unsubscribed"}
		if !ok {
			status = http.StatusBadRequest
			body = map[string]string{"error": "invalid_or_expired_token"}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	// GET — human-facing confirmation.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if ok {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(unsubscribeOKHTML))
	} else {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(unsubscribeBadHTML))
	}
}

const unsubscribeOKHTML = `<!doctype html><html><head><meta charset="utf-8">` +
	`<meta name="viewport" content="width=device-width, initial-scale=1">` +
	`<title>Unsubscribed</title></head>` +
	`<body style="font-family: system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; padding: 0 1rem; color: #1a1a1a; line-height: 1.5;">` +
	`<h1 style="font-size: 1.4rem;">You've been unsubscribed</h1>` +
	`<p>You'll no longer receive these email notifications. This didn't change your in-app notifications.</p>` +
	`<p><a href="/account/preferences" style="color: #2563eb;">Manage your notification preferences</a></p>` +
	`</body></html>`

const unsubscribeBadHTML = `<!doctype html><html><head><meta charset="utf-8">` +
	`<meta name="viewport" content="width=device-width, initial-scale=1">` +
	`<title>Link expired</title></head>` +
	`<body style="font-family: system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; padding: 0 1rem; color: #1a1a1a; line-height: 1.5;">` +
	`<h1 style="font-size: 1.4rem;">This unsubscribe link is invalid or expired</h1>` +
	`<p>You can change your email notification settings from your account.</p>` +
	`<p><a href="/account/preferences" style="color: #2563eb;">Manage your notification preferences</a></p>` +
	`</body></html>`
