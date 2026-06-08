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
	EventCapabilityGranted = "user.capability_granted"
	EventCapabilityRevoked = "user.capability_revoked"
	EventCapabilityGrantRemoved = "user.capability_grant_removed"
	EventCapabilityRevokeRemoved = "user.capability_revoke_removed"

	// 1.22.C federation share events. Emitted via WriteInTx so
	// the audit row commits atomically with the share write per
	// the design proposal §7.2 write-ahead invariant.
	EventFederationShareGranted    = "federation.share.granted"
	EventFederationShareRevoked    = "federation.share.revoked"
	EventFederationActivityRejected = "federation.activity.rejected"

	// 1.22.D-b outbox-dispatcher emission events. emission.skipped
	// fires when the sender-side resolver refuses to enqueue an
	// activity (sensitivity-without-encryption per ADR 0020, or
	// recipient set is empty, or peer is mid-defederation). See
	// spec §12.3 for the reason catalogue.
	EventFederationEmissionSkipped = "federation.emission.skipped"

	// 1.22.D-c admin operator events. Both pool-bound (NOT tx-
	// bound) — the admin handler's tx is the state-change unit;
	// the audit records the operator decision after commit.
	EventFederationOutboxRequeued         = "federation.outbox.requeued"
	EventFederationPeerCascadeCancelled   = "federation.peer.cascade_cancelled"

	// Demo-seed loader events (post-1.22.D dogfood unblock).
	// Both gated on system.admin; emitted by the apply-side
	// script the seed agent owns. Visible in the admin audit
	// log so operators can see what was rewritten vs created.
	EventAdminSeedTimestampsBackfilled = "admin.seed.timestamps_backfilled"
	EventAdminSeedCommentCreated       = "admin.seed.comment_created"
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

// CapabilityGranted records an explicit grant being added to a
// user. `teamID` is empty for global grants.
func (r *Recorder) CapabilityGranted(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, capability, teamID, note string) {
	r.write(ctx, EventCapabilityGranted, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"capability": capability,
		"team_id":    teamID,
		"note":       note,
	})
}

// CapabilityRevoked records an explicit revoke being added to a
// user. Symmetric to CapabilityGranted.
func (r *Recorder) CapabilityRevoked(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, capability, teamID, note string) {
	r.write(ctx, EventCapabilityRevoked, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"capability": capability,
		"team_id":    teamID,
		"note":       note,
	})
}

// CapabilityGrantRemoved records a previously-issued grant being
// withdrawn. Distinct event type from CapabilityRevoked so the
// audit viewer can distinguish "removed an additive grant" from
// "added a subtractive revoke" — they look identical from the
// user-effect perspective (they both reduce caps) but the
// intent + reversal path are different.
func (r *Recorder) CapabilityGrantRemoved(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, capability, teamID string) {
	r.write(ctx, EventCapabilityGrantRemoved, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"capability": capability,
		"team_id":    teamID,
	})
}

// CapabilityRevokeRemoved records a previously-issued revoke being
// withdrawn (the cap re-enables, modulo the rest of the resolution).
func (r *Recorder) CapabilityRevokeRemoved(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, capability, teamID string) {
	r.write(ctx, EventCapabilityRevokeRemoved, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"capability": capability,
		"team_id":    teamID,
	})
}

// write is the single funnel for all event writes. Failures are
// logged at WARN; they never propagate.
func (r *Recorder) write(ctx context.Context, eventType string, subject, actor *int64, rc reqContext, metadata map[string]any) {
	r.writeWith(ctx, New(r.Pool), eventType, subject, actor, rc, metadata)
}

// writeWith is the tx-aware funnel. WriteInTx wraps it for callers
// that need write-ahead audit semantics — the audit row commits
// in the SAME transaction as the domain write, so a tx rollback
// rolls back the audit row too. Used by federation_shares per
// the 1.22.C design proposal §7.2.
func (r *Recorder) writeWith(ctx context.Context, q *Queries, eventType string, subject, actor *int64, rc reqContext, metadata map[string]any) {
	payload, err := json.Marshal(metadata)
	if err != nil {
		payload = []byte("{}")
		r.Logger.LogAttrs(ctx, slog.LevelWarn, "audit.marshal.error",
			slog.String("event_type", eventType),
			slog.String("err", err.Error()),
		)
	}
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

// OutboxRequeued records a federation.outbox.requeued event
// per the 1.22.D-c admin re-queue button. The audit fires
// AFTER the row's status flips queued so the audit trail
// reflects the state change.
//
// actorUserRef is the admin who clicked the button. Last_error
// is the prior failure reason — captured for the audit so an
// operator can later see what the prior failure was without
// joining the outbox row (which may have moved through multiple
// states since).
func (r *Recorder) OutboxRequeued(
	ctx context.Context,
	req *http.Request,
	actorUserRef int64,
	outboxID, peerID, activityID, priorLastError string,
) {
	meta := map[string]any{
		"outbox_id":         outboxID,
		"peer_id":           peerID,
		"activity_id":       activityID,
		"prior_last_error":  priorLastError,
	}
	r.write(ctx, EventFederationOutboxRequeued, nil, &actorUserRef, ctxFromRequest(req), meta)
}

// PeerCascadeCancelled records a federation.peer.cascade_cancelled
// event per the 1.22.D-c defederation-cascade hook. ONE audit
// row per cascade — NOT N — per the single-audit-per-operator-
// decision invariant (the operator made ONE decision: cancel
// everything queued for this peer; the audit reflects that).
func (r *Recorder) PeerCascadeCancelled(
	ctx context.Context,
	req *http.Request,
	actorUserRef int64,
	peerID string,
	cancelledCount int,
) {
	meta := map[string]any{
		"peer_id":         peerID,
		"cancelled_count": cancelledCount,
	}
	r.write(ctx, EventFederationPeerCascadeCancelled, nil, &actorUserRef, ctxFromRequest(req), meta)
}

// SeedTimestampsBackfilled records an
// admin.seed.timestamps_backfilled event. One per call;
// captures per-kind counts so operators can later answer
// "what rows did the seed loader rewrite?" without scanning
// every row.
func (r *Recorder) SeedTimestampsBackfilled(
	ctx context.Context,
	req *http.Request,
	actorUserRef int64,
	assetN, postN, commentN, skippedN int,
) {
	meta := map[string]any{
		"asset_updated":      assetN,
		"post_updated":       postN,
		"comment_updated":    commentN,
		"skipped_unknown_id": skippedN,
	}
	r.write(ctx, EventAdminSeedTimestampsBackfilled, nil, &actorUserRef, ctxFromRequest(req), meta)
}

// SeedCommentCreated records an admin.seed.comment_created
// event. One per forged comment. Helps operators distinguish
// seeded vs organic comments in the audit log when
// investigating later.
func (r *Recorder) SeedCommentCreated(
	ctx context.Context,
	req *http.Request,
	actorUserRef int64,
	commentID, targetKind, targetID string,
	forgedAuthorRef int64,
) {
	meta := map[string]any{
		"comment_id":        commentID,
		"target_kind":       targetKind,
		"target_id":         targetID,
		"forged_author_ref": forgedAuthorRef,
	}
	r.write(ctx, EventAdminSeedCommentCreated, nil, &actorUserRef, ctxFromRequest(req), meta)
}

// EmissionSkipped records a federation.emission.skipped event
// per the 1.22.D-b design proposal §3.9 — the outbox dispatcher
// calls this whenever the recipient resolver refuses to enqueue
// an activity. activityID is the local activities row UUID;
// reason is from spec §12.3 (encryption_required_but_not_
// supported / recipient_set_empty / defederation_in_progress).
//
// Pool-bound (NOT tx-bound) because the dispatcher's cursor
// advance has already committed by the time we audit; the
// emission decision is the audit's own write.
func (r *Recorder) EmissionSkipped(
	ctx context.Context,
	activityID, activityType, objectKind, objectID, visibility, sensitivity, reason string,
) {
	meta := map[string]any{
		"activity_id":   activityID,
		"activity_type": activityType,
		"object_kind":   objectKind,
		"object_id":     objectID,
		"visibility":    visibility,
		"sensitivity":   sensitivity,
		"reason":        reason,
	}
	r.write(ctx, EventFederationEmissionSkipped, nil, nil, reqContext{}, meta)
}

// ActivityRejected records a federation.activity.rejected event
// per the 1.22.C design proposal §7.1 — the inbox dispatcher
// (1.22.D) calls this whenever the shares gate denies an
// inbound activity. Pool-bound (NOT tx-bound) because the
// inbox doesn't have a domain write to atomically pair with;
// rejection drops the activity outright. correlationID is the
// inbound activity's UUID so the audit chain links back to the
// /admin/activities ledger row for the dropped delivery.
func (r *Recorder) ActivityRejected(
	ctx context.Context,
	peerID, sourceUserURL, activityType, objectKind, objectID, reason, correlationID string,
) {
	meta := map[string]any{
		"peer_id":        peerID,
		"activity_type":  activityType,
		"object_kind":    objectKind,
		"object_id":      objectID,
		"reason":         reason,
		"correlation_id": correlationID,
	}
	if sourceUserURL != "" {
		meta["source_user_url"] = sourceUserURL
	}
	r.write(ctx, EventFederationActivityRejected, nil, nil, reqContext{}, meta)
}

// WriteInTx is the tx-aware public funnel. Callers needing the
// write-ahead-audit invariant (federation_shares grant/revoke,
// per the 1.22.C design proposal §7.2) call this from inside a
// WithEmissionFn closure so the audit row lives or dies with the
// domain write. The Queries argument must be bound to the same
// pgx.Tx the closure uses for its other writes.
//
// Failures are logged but NOT returned — same contract as the
// non-tx write. The argument for that contract: a tx rollback
// triggered by a failing audit insert would block the user-facing
// share operation on an audit-layer issue. The audit row's
// absence shows up in the audit feed as a gap, which is the
// correct surface for an operator to notice + investigate.
func (r *Recorder) WriteInTx(ctx context.Context, q *Queries, eventType string, subject, actor *int64, metadata map[string]any) {
	// Tx-bound calls don't carry req-context (no http.Request on
	// the path); pass an empty reqContext so the ip + UA columns
	// stay null. Federation share writes are server-internal —
	// the originating user_ref is the actor, which is enough.
	r.writeWith(ctx, q, eventType, subject, actor, reqContext{}, metadata)
}
