// Package audit is the typed entry point for writing audit_events.
//
// Every event_type recognised by the system has a corresponding method
// on Recorder. The method signature documents which metadata fields
// the event carries; the JSON shape in the DB matches the method's
// parameter list.
//
// All writes are best-effort: a failure to record an audit event is
// logged but does not propagate to the caller. The DB is the source of
// truth, the audit log is observability.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Event-type constants. New events get a constant here and a method on
// Recorder. The string value is what lands in audit_events.event_type
// — keep it stable; downstream tooling will pivot on it.
const (
	EventLoginSucceeded   = "login.succeeded"
	EventLoginFailed      = "login.failed"
	EventLoginRateLimited = "login.rate_limited"
	EventLogout           = "logout"
	EventSessionRevoked   = "session.revoked"
	EventSessionExpired   = "session.expired"
	EventUserStatusChanged = "user.status_changed"
	EventPasswordChanged   = "user.password_changed"
	EventPasswordReset     = "user.password_reset"
)

// Recorder writes audit events. Construct one at server startup and
// share it across handlers — it's safe for concurrent use.
type Recorder struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// NewRecorder builds a Recorder. Named to avoid colliding with the
// sqlc-generated New(db DBTX) *Queries in this package.
func NewRecorder(pool *pgxpool.Pool, logger *slog.Logger) *Recorder {
	return &Recorder{Pool: pool, Logger: logger}
}

// reqContext captures the per-request observability fields every event
// shares. Extracted once per call site so handlers don't reconstruct it.
type reqContext struct {
	ip        *netip.Addr
	userAgent *string
}

func ctxFromRequest(r *http.Request) reqContext {
	if r == nil {
		return reqContext{}
	}
	raw := r.Header.Get("X-Forwarded-For")
	if raw != "" {
		raw = strings.TrimSpace(strings.SplitN(raw, ",", 2)[0])
	}
	if raw == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		raw = host
	}
	var ipPtr *netip.Addr
	if addr, err := netip.ParseAddr(raw); err == nil {
		ipPtr = &addr
	}
	var uaPtr *string
	if ua := r.Header.Get("User-Agent"); ua != "" {
		uaPtr = &ua
	}
	return reqContext{ip: ipPtr, userAgent: uaPtr}
}

// LoginSucceeded records a successful authentication.
func (r *Recorder) LoginSucceeded(ctx context.Context, req *http.Request, userRef int64, sessionID string) {
	r.write(ctx, EventLoginSucceeded, &userRef, nil, ctxFromRequest(req), map[string]any{
		"session_id": sessionID,
	})
}

// LoginFailed records a rejected authentication attempt. userRef is a
// pointer because we may not know the user (unknown username case).
func (r *Recorder) LoginFailed(ctx context.Context, req *http.Request, attemptedUsername string, userRef *int64, reason string) {
	r.write(ctx, EventLoginFailed, userRef, nil, ctxFromRequest(req), map[string]any{
		"attempted_username": attemptedUsername,
		"reason":             reason,
	})
}

// LoginRateLimited records that the rate limiter rejected an attempt.
// No userRef because the failure happens before credential verification.
func (r *Recorder) LoginRateLimited(ctx context.Context, req *http.Request, attemptedUsername, key string) {
	r.write(ctx, EventLoginRateLimited, nil, nil, ctxFromRequest(req), map[string]any{
		"attempted_username": attemptedUsername,
		"key":                key,
	})
}

// Logout records an explicit /auth/logout call.
func (r *Recorder) Logout(ctx context.Context, req *http.Request, userRef int64, sessionID string) {
	r.write(ctx, EventLogout, &userRef, nil, ctxFromRequest(req), map[string]any{
		"session_id": sessionID,
	})
}

// SessionRevoked records a session being killed by either the user
// or an admin. actorUserRef identifies the killer; equals userRef
// when self-initiated.
func (r *Recorder) SessionRevoked(ctx context.Context, req *http.Request, userRef, actorUserRef int64, sessionID, reason string) {
	r.write(ctx, EventSessionRevoked, &userRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"session_id": sessionID,
		"reason":     reason,
	})
}

// UserStatusChanged records an admin moving a user across the
// lifecycle state machine (Phase 1.17.B). `previous` + `next`
// are the underlying user.approved values (1=active, 0=pending,
// 2=disabled); `reason` is the admin-supplied free-text note —
// surfaced verbatim in the audit viewer.
func (r *Recorder) UserStatusChanged(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, next int64, reason string) {
	r.write(ctx, EventUserStatusChanged, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"previous": previous,
		"next":     next,
		"reason":   reason,
	})
}

// PasswordChanged records a self-service password change.
// `sessionsRevoked` is the count of OTHER sessions terminated as
// part of the change (the caller may opt into "sign out
// everywhere else" defensively).
func (r *Recorder) PasswordChanged(ctx context.Context, req *http.Request, userRef int64, sessionsRevoked int) {
	r.write(ctx, EventPasswordChanged, &userRef, &userRef, ctxFromRequest(req), map[string]any{
		"sessions_revoked": sessionsRevoked,
	})
}

// PasswordReset records an admin-initiated force-reset. `actor` is
// the admin who fired the action; `subject` is the user whose
// password was reset. `reason` is the admin-supplied free-text note.
func (r *Recorder) PasswordReset(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, reason string) {
	r.write(ctx, EventPasswordReset, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"reason": reason,
	})
}

// write is the single funnel for all event writes. Failures are
// logged at WARN; they never propagate.
func (r *Recorder) write(ctx context.Context, eventType string, subject, actor *int64, rc reqContext, metadata map[string]any) {
	payload, err := json.Marshal(metadata)
	if err != nil {
		// Should be impossible for our shapes, but if it happens we
		// at least keep the row with an empty payload rather than
		// dropping the event entirely.
		payload = []byte("{}")
		r.Logger.LogAttrs(ctx, slog.LevelWarn, "audit.marshal.error",
			slog.String("event_type", eventType),
			slog.String("err", err.Error()),
		)
	}
	q := New(r.Pool)
	if err := q.InsertAuditEvent(ctx, InsertAuditEventParams{
		EventType:      eventType,
		SubjectUserRef: subject,
		ActorUserRef:   actor,
		Ip:             rc.ip,
		UserAgent:      rc.userAgent,
		Metadata:       payload,
	}); err != nil {
		r.Logger.LogAttrs(ctx, slog.LevelWarn, "audit.write.error",
			slog.String("event_type", eventType),
			slog.String("err", err.Error()),
		)
	}
}
