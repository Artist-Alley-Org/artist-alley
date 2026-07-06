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
	"time"

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

	// Phase 1.17.A — typed per-transition admin events. The
	// generic user.status_changed event remains for backstop /
	// ad-hoc transitions; the four below are the canonical
	// admin-driven lifecycle moves and ship per-transition so
	// downstream consumers (search filters, dashboards, audit-log
	// changeset retrofits in 1.17.D) can pivot without parsing
	// the metadata pair.
	EventAdminUserApproved = "admin.users.approved"
	EventAdminUserDisabled = "admin.users.disabled"
	EventAdminUserArchived = "admin.users.archived"
	EventAdminUserRestored = "admin.users.restored"

	// Phase 1.17.A — refused-by-invariant event. Distinct from
	// user.status_changed (which only fires on a successful write)
	// so the "operator tried to leave us with zero admins" signal
	// is unambiguous and easy to alert on.
	EventAdminUserRefusedLastAdmin = "admin.users.refused_last_admin"

	EventPasswordChanged   = "user.password_changed"
	EventPasswordReset     = "user.password_reset"

	// Phase 1.19.A-2 — admin impersonation lifecycle. Two
	// events, both with subject=target_user_ref + actor=admin_
	// user_ref so the audit viewer pivots on either side. The
	// END event is fired by the explicit "end impersonation"
	// path; expirations / forced revokes show up on the
	// generic EventSessionRevoked event instead.
	EventAdminImpersonationStarted = "admin.impersonation.started"
	EventAdminImpersonationEnded   = "admin.impersonation.ended"

	// Phase 1.19.C — self-service registration lifecycle.
	EventUserRegistered     = "user.registered"
	EventUserEmailVerified  = "user.email_verified"

	// Phase 1.19.D — per-username account lockout. Emitted exactly
	// once per lockout window (the failed attempt that CROSSES the
	// threshold), not once per subsequent locked attempt. Payload
	// carries an IP subnet hash (not the raw IP) so operators can
	// group by threat class without a per-request IP audit log.
	EventAuthLockoutTriggered = "auth.lockout.triggered"
	// Phase 1.19.D — lockout cleared. Fires on both admin manual
	// unlock (source=admin, actor=admin.user_ref) and on
	// successful login after auto-clear (source=auto,
	// actor=self via userRef; treated separately below).
	EventAuthLockoutCleared = "auth.lockout.cleared"
	EventCapabilityGranted = "user.capability_granted"
	EventCapabilityRevoked = "user.capability_revoked"
	EventCapabilityGrantRemoved = "user.capability_grant_removed"
	EventCapabilityRevokeRemoved = "user.capability_revoke_removed"

	// Phase 1.17.C — fired by capability_sweeper.go once per
	// reaped grant or revoke that's past its expires_at. The
	// pre-reap row contents (subject, capability, team, expires_at)
	// go in metadata so the audit row alone reconstructs the
	// grant/revoke's life cycle without joining against the
	// (now-deleted) source table.
	EventCapabilityGrantExpiredSwept  = "user.capability_grant_expired_swept"
	EventCapabilityRevokeExpiredSwept = "user.capability_revoke_expired_swept"

	// Phase 1.17.D — admin-driven system-config changes. One
	// event per surface so operators can filter by what got
	// touched (alerting "auth config changed" is materially
	// different from "appearance changed"). Each carries a
	// metadata.changeset built via Recorder.RecordChange.
	EventAdminSiteConfigUpdated       = "admin.system.site_config_updated"
	EventAdminSMTPConfigUpdated       = "admin.system.smtp_config_updated"
	EventAdminAuthConfigUpdated       = "admin.system.auth_config_updated"
	EventAdminAIConfigUpdated         = "admin.system.ai_config_updated"
	EventAdminAppearanceConfigUpdated = "admin.system.appearance_config_updated"

	// Phase 1.16.B-5-followup — search feedback abuse-review event.
	// Fires whenever an admin opens the per-user feedback log page
	// so we have an audit trail for who's browsing which user's
	// vote history. Aggregation views (top down-voted queries +
	// under-ranked hits) don't fire this — they're anonymized.
	EventAdminSearchFeedbackAuditViewed = "admin.search.feedback.audit_viewed"

	// Phase 1.17.D — user-profile change event. Distinct from
	// the state-transition events (approve/disable/etc.) because
	// this is a field-level edit — name / bio / avatar etc. —
	// surfaced as a metadata.changeset.
	EventUserProfileUpdated = "user.profile_updated"

	// Phase 1.17.E — resource request lifecycle events. Each
	// fires from the corresponding requests.Handler method
	// (Submit / Grant / Deny / sweeper-cascade). Metadata
	// carries the request_id + asset + capability + reason so
	// the audit row alone reconstructs the request lifecycle
	// without joining against the (potentially stale) source row.
	EventRequestCreated = "request.created"
	EventRequestGranted = "request.granted"
	EventRequestDenied  = "request.denied"
	EventRequestExpired = "request.expired"

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

	// 1.22.I-b per-user federation keypair events. Fired from the
	// three user-create paths (bootstrap, /setup, /admin/seed/users)
	// when EnsureCurrentForUser actually mints a new keypair — the
	// "user already had a current key" no-op path stays silent so
	// idempotent re-runs don't generate audit noise. Tx-bound:
	// the audit row commits atomically with the keypair insert.
	EventFederationUserKeyGenerated = "federation.user.key_generated"

	// 1.22.I-c remote-actor encryption-key distribution event.
	// Fired by federation/remote.Handler.SetEncryptionKey when the
	// inbound key differs from the previously cached value
	// (first-time advertisement OR rotation). Pool-bound (NOT
	// tx-bound) — the remote-actor upsert path is best-effort
	// and not transactional; pairing the audit to it via a tx
	// would change the inbox dispatcher's correctness story.
	EventFederationRemoteActorKeyUpdated = "federation.remote_actor.key_updated"

	// 1.22.I-d per-recipient capability-gate skip. Fired by the
	// outbox resolver when an activity requires encryption AND
	// the recipient peer hasn't negotiated the required capability
	// at handshake time. Same event_type string as the existing
	// instance-level federation.emission.skipped — the new reason
	// codes (SkippedCapabilityMissing*) live in
	// federation/outbox/resolver.go's typed catalogue + appear in
	// the audit metadata's `reason` field. Pool-bound.
	//
	// Dormant in production traffic at 1.22.I-d (no caller sets
	// Input.RequiresEncryption true yet); 1.22.I-e flips the
	// flag. Scenario 08 exercises the audit path via synthetic
	// injection.

	// 1.22.I-e per-recipient encrypted emission. Fired by the
	// outbox delivery worker (Worker.buildEnvelope) once per
	// outbox row that took the encryption branch. Pool-bound;
	// not coupled to any tx because the encryption + the
	// audit row are both pool-direct in the dispatcher loop.
	// Operator observability: "how many encrypted dispatches
	// per hour did this peer accept?" answerable via grouping
	// on metadata->>'peer_id'.
	EventFederationEmissionEncrypted = "federation.emission.encrypted"

	// 1.22.I-f inbox decryption events. Both fired by the inbox
	// dispatcher's stage-4 decrypt branch (federation/inbox/
	// dispatcher.go) once per encrypted envelope. Pool-bound;
	// the decrypt runs outside any tx because the dispatcher's
	// MarkInboxProcessed / MarkInboxRejected is the unit of work
	// being audited.
	//
	//   federation.inbox.decrypted        — happy path. Metadata
	//                                       carries which receiver
	//                                       key version unlocked
	//                                       the payload + the
	//                                       attempt count (1 = no
	//                                       fallback fired; 2+ =
	//                                       rotation grace window
	//                                       saved us).
	//
	//   federation.inbox.decrypt_failed   — terminal path. Walked
	//                                       every retained key,
	//                                       all attempts failed.
	//                                       Inbox row transitions
	//                                       to status=rejected with
	//                                       reject_reason=decrypt_failed.
	//
	// Operator dashboards group on metadata->>'peer_id' +
	// 'sender_key_version' to surface "is one peer's rotated key
	// not making it through?" without joining federation_inbox.
	EventFederationInboxDecrypted     = "federation.inbox.decrypted"
	EventFederationInboxDecryptFailed = "federation.inbox.decrypt_failed"

	// 1.22.I-g sender-refusal policy event. Distinct from the
	// existing federation.emission.skipped: semantically
	// skipped = "informational, not relevant" (recipient_set_
	// empty / defederation_in_progress / peer-disabled), refused
	// = "policy DECISION, blocked an emission that would
	// otherwise have happened" (the share's sensitivity tier
	// mandated encryption + the recipient peer or key wasn't
	// available). Operators grep these differently — two events
	// = two intent shapes.
	//
	// Fired by the outbox delivery Worker once per outbox row
	// whose [outbox.ChoosePathFor] returned [outbox.EmissionRefused].
	// Pool-bound; the audit row + the [outbox.MarkOutboxRefused]
	// commit independently (the audit feed accepts the "DB write
	// committed, audit row missed" gap as the lesser failure
	// mode, same contract as I-e/I-f's emission/inbox events).
	//
	// Operator analytics: SELECT metadata->>'peer_id',
	// metadata->>'reason', COUNT(*) FROM audit_events
	// WHERE event_type = 'federation.emission.refused'
	// GROUP BY 1, 2 — answers "which peers + reasons are
	// causing refusals?" — the load-bearing query for the
	// dogfood validation that follows I-g.
	EventFederationEmissionRefused = "federation.emission.refused"

	// Phase 1.22.I-h key rotation lifecycle events.
	//
	// EventFederationUserKeyRotated fires once per successful
	// rotation primitive call (userkeys.RotateForUser). The audit
	// row's subject is the user whose key was rotated; the actor
	// is the principal who triggered the flip (= subject for
	// self-rotation, the admin's ref for compromised-key recovery
	// via the /admin/federation/users/{ref}/rotate-keys path).
	EventFederationUserKeyRotated = "federation.user.key_rotated"

	// EventFederationUserKeyRetainedExpired fires once per
	// non-zero sweeper reap (userkeys.Sweeper). One audit row
	// per sweep, NOT one per reaped key — the count is the
	// metadata field. Operators tracking churn on "how many
	// retained keys aged out in the last week" sum the metadata
	// values across audit rows of this type.
	EventFederationUserKeyRetainedExpired = "federation.user.key_retained_expired"

	// EventFederationInboxEncryptionRequiredRejected fires when
	// the receiver-side encryption policy gate (1.22.I-h) catches
	// a plaintext envelope whose target object's sensitivity
	// tier mandates encryption. Distinct from
	// EventFederationInboxDecryptFailed (which covers actual
	// decryption attempts) — this one fires BEFORE the decrypt
	// stage on the plaintext-but-shouldn't-be path.
	EventFederationInboxEncryptionRequiredRejected = "federation.inbox.encryption_required_rejected"

	// Demo-seed loader events (post-1.22.D dogfood unblock).
	// Both gated on system.admin; emitted by the apply-side
	// script the seed agent owns. Visible in the admin audit
	// log so operators can see what was rewritten vs created.
	EventAdminSeedTimestampsBackfilled = "admin.seed.timestamps_backfilled"
	EventAdminSeedCommentCreated       = "admin.seed.comment_created"
	EventAdminSeedUserCreated          = "admin.seed.user_created"
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

// AuthLockoutTriggered records the failed attempt that crossed the
// lockout threshold. Emitted exactly once per lockout window (not
// once per subsequent locked attempt). ipSubnetHash is a SHA-256
// digest of the request's IP subnet (ipnet(24) IPv4 / ipnet(56)
// IPv6) salted per-instance — records threat class without a
// per-request IP audit log. Phase 1.19.D.
func (r *Recorder) AuthLockoutTriggered(ctx context.Context, req *http.Request, userRef int64, failedCount, threshold, durationMinutes int32, ipSubnetHash string) {
	r.write(ctx, EventAuthLockoutTriggered, &userRef, nil, ctxFromRequest(req), map[string]any{
		"failed_count":     failedCount,
		"threshold":        threshold,
		"duration_minutes": durationMinutes,
		"ip_subnet_hash":   ipSubnetHash,
	})
}

// AuthLockoutCleared records a lockout being cleared. `source` is
// "admin" (manual admin.user_ref → target.user_ref) or "auto"
// (self-cleared on next login after lockout_until expired). For
// admin source: actorUserRef is the admin's ref. For auto source:
// actorUserRef is nil. Phase 1.19.D.
func (r *Recorder) AuthLockoutCleared(ctx context.Context, req *http.Request, userRef int64, actorUserRef *int64, priorFailedCount int32, source string) {
	r.write(ctx, EventAuthLockoutCleared, &userRef, actorUserRef, ctxFromRequest(req), map[string]any{
		"prior_failed_count": priorFailedCount,
		"source":             source,
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

// AdminUserApproved / AdminUserDisabled / AdminUserArchived /
// AdminUserRestored — Phase 1.17.A typed per-transition events.
// Fire IN ADDITION TO UserStatusChanged so backstop consumers
// (everything that already filters on user.status_changed) keep
// working, AND new consumers (admin dashboards, the 1.17.D
// changeset retrofit) can pivot on the specific transition
// without parsing metadata. previous/next carry the typed state
// strings ("pending" / "active" / etc.) — easier to read in the
// audit viewer than the raw int values.
// AdminSearchFeedbackAuditViewed — Phase 1.16.B-5-followup. Admin
// opened /admin/search/feedback/audit/{user_ref}, i.e. exposed
// themselves to a specific user's vote log. Not gated on any
// state-changing action (read-only page) but audit-logged anyway so
// operators can see who's browsing whom. Anonymized aggregation
// views do NOT fire this.
func (r *Recorder) AdminSearchFeedbackAuditViewed(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64) {
	r.write(ctx, EventAdminSearchFeedbackAuditViewed, &subjectUserRef, &actorUserRef, ctxFromRequest(req), nil)
}

func (r *Recorder) AdminUserApproved(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, next, reason string) {
	r.write(ctx, EventAdminUserApproved, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"previous": previous,
		"next":     next,
		"reason":   reason,
	})
}

func (r *Recorder) AdminUserDisabled(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, next, reason string) {
	r.write(ctx, EventAdminUserDisabled, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"previous": previous,
		"next":     next,
		"reason":   reason,
	})
}

func (r *Recorder) AdminUserArchived(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, next, reason string) {
	r.write(ctx, EventAdminUserArchived, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"previous": previous,
		"next":     next,
		"reason":   reason,
	})
}

func (r *Recorder) AdminUserRestored(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, next, reason string) {
	r.write(ctx, EventAdminUserRestored, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"previous": previous,
		"next":     next,
		"reason":   reason,
	})
}

// AdminUserRefusedLastAdmin — Phase 1.17.A. Fires when the
// last-admin invariant blocks a transition. Distinct event so
// alerting on "operator tried to leave us with zero admins" is
// unambiguous. previous/next are the typed state strings; the
// transition did NOT commit.
func (r *Recorder) AdminUserRefusedLastAdmin(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, previous, attempted, reason string) {
	r.write(ctx, EventAdminUserRefusedLastAdmin, &subjectUserRef, &actorUserRef, ctxFromRequest(req), map[string]any{
		"previous":  previous,
		"attempted": attempted,
		"reason":    reason,
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

// UserRegistered records a fresh self-registration. subject =
// the new user; actor = nil (anonymous endpoint). Metadata
// carries the email so the audit row alone explains who came
// in via /auth/register.
func (r *Recorder) UserRegistered(ctx context.Context, req *http.Request, userRef int64, emailAddr string) {
	r.write(ctx, EventUserRegistered, &userRef, nil, ctxFromRequest(req), map[string]any{
		"email": emailAddr,
	})
}

// UserEmailVerified fires when a verification token is consumed.
// subject = user; actor = nil (the link click is anonymous from
// the auth-resolver perspective).
func (r *Recorder) UserEmailVerified(ctx context.Context, req *http.Request, userRef int64) {
	r.write(ctx, EventUserEmailVerified, &userRef, nil, ctxFromRequest(req), nil)
}

// ImpersonationStarted records an admin issuing an impersonation
// session for a target user. subject = target, actor = admin. The
// session_id lets the audit viewer correlate with the
// EventSessionRevoked row that fires whenever the impersonation
// session ends (whether via explicit end, expiry, or forced revoke).
func (r *Recorder) ImpersonationStarted(ctx context.Context, req *http.Request, targetUserRef, adminUserRef int64, sessionID, reason string) {
	r.write(ctx, EventAdminImpersonationStarted, &targetUserRef, &adminUserRef, ctxFromRequest(req), map[string]any{
		"session_id": sessionID,
		"reason":     reason,
	})
}

// ImpersonationEnded records the explicit end-of-impersonation
// path (admin clicks "End impersonation" in the banner). The
// session is also revoked separately; this event is the
// intent-level signal distinct from the generic session.revoked.
func (r *Recorder) ImpersonationEnded(ctx context.Context, req *http.Request, targetUserRef, adminUserRef int64, sessionID string) {
	r.write(ctx, EventAdminImpersonationEnded, &targetUserRef, &adminUserRef, ctxFromRequest(req), map[string]any{
		"session_id": sessionID,
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

// CapabilityGrantExpiredSwept / CapabilityRevokeExpiredSwept —
// Phase 1.17.C. Fired by capability_sweeper.go once per reaped
// row. Pool-bound (the sweeper has no http.Request); subject is
// the grant/revoke's user_ref; actor is nil (the system swept it,
// not a person). team_id is "" for global rows. expired_at carries
// the timestamp that triggered the sweep so the audit row
// reconstructs the lifecycle without the (now-deleted) source row.
func (r *Recorder) CapabilityGrantExpiredSwept(ctx context.Context, subjectUserRef int64, capability, teamID string, expiredAt time.Time) {
	r.write(ctx, EventCapabilityGrantExpiredSwept, &subjectUserRef, nil, reqContext{}, map[string]any{
		"capability": capability,
		"team_id":    teamID,
		"expired_at": expiredAt.UTC().Format(time.RFC3339Nano),
	})
}

func (r *Recorder) CapabilityRevokeExpiredSwept(ctx context.Context, subjectUserRef int64, capability, teamID string, expiredAt time.Time) {
	r.write(ctx, EventCapabilityRevokeExpiredSwept, &subjectUserRef, nil, reqContext{}, map[string]any{
		"capability": capability,
		"team_id":    teamID,
		"expired_at": expiredAt.UTC().Format(time.RFC3339Nano),
	})
}

// RequestCreated — Phase 1.17.E. Fired by requests.Handler.Submit.
// requestID is the new resource_request row id; assetID is the
// target; capability is the code the requester asked for. reason
// is the requester's free-text justification (may be empty).
func (r *Recorder) RequestCreated(ctx context.Context, req *http.Request, requesterRef int64, requestID, assetID, capability, reason string) {
	r.write(ctx, EventRequestCreated, &requesterRef, &requesterRef, ctxFromRequest(req), map[string]any{
		"request_id": requestID,
		"asset_id":   assetID,
		"capability": capability,
		"reason":     reason,
	})
}

// RequestGranted — Phase 1.17.E. Fired by requests.Handler.Grant
// after the resource_request row + the user_capability_grants row
// commit atomically. expiresAt is the optional auto-expiry that
// the CapabilitySweeper will later reap (zero time → permanent).
func (r *Recorder) RequestGranted(ctx context.Context, req *http.Request, approverRef, requesterRef int64, requestID, assetID, capability, decisionReason string, expiresAt time.Time) {
	meta := map[string]any{
		"request_id":      requestID,
		"asset_id":        assetID,
		"capability":      capability,
		"requester":       requesterRef,
		"decision_reason": decisionReason,
	}
	if !expiresAt.IsZero() {
		meta["expires_at"] = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	r.write(ctx, EventRequestGranted, &requesterRef, &approverRef, ctxFromRequest(req), meta)
}

// RequestDenied — Phase 1.17.E. Fired by requests.Handler.Deny.
// Mirrors RequestGranted shape; no expires_at because deny is
// terminal.
func (r *Recorder) RequestDenied(ctx context.Context, req *http.Request, approverRef, requesterRef int64, requestID, assetID, capability, decisionReason string) {
	r.write(ctx, EventRequestDenied, &requesterRef, &approverRef, ctxFromRequest(req), map[string]any{
		"request_id":      requestID,
		"asset_id":        assetID,
		"capability":      capability,
		"requester":       requesterRef,
		"decision_reason": decisionReason,
	})
}

// RequestExpired — Phase 1.17.E. Fired by the CapabilitySweeper
// request-cascade callback when a grant with request_ref reaps.
// Pool-bound (the sweeper has no http.Request); actor is nil
// (the system swept it, not a person).
func (r *Recorder) RequestExpired(ctx context.Context, requesterRef int64, requestID, capability string, expiredAt time.Time) {
	r.write(ctx, EventRequestExpired, &requesterRef, nil, reqContext{}, map[string]any{
		"request_id": requestID,
		"capability": capability,
		"expired_at": expiredAt.UTC().Format(time.RFC3339Nano),
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

// SeedUserCreated records an admin.seed.user_created event.
// One per forged user. Helps operators distinguish seeded vs
// organic users in the audit log when investigating later.
func (r *Recorder) SeedUserCreated(
	ctx context.Context,
	req *http.Request,
	actorUserRef int64,
	userRef int64,
	username string,
) {
	meta := map[string]any{
		"user_ref": userRef,
		"username": username,
	}
	r.write(ctx, EventAdminSeedUserCreated, nil, &actorUserRef, ctxFromRequest(req), meta)
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

// FederationEmissionEncrypted records a
// federation.emission.encrypted event per Phase 1.22.I-e — fired
// once per outbox row that the delivery worker actually
// dispatched encrypted (capability gate passed, recipient key
// resolved, NaCl-box seal completed).
//
// Pool-bound. Metadata fields are the per-emission detail an
// operator wants when grepping the audit log:
//
//   peer_id                — recipient peer's UUID
//   activity_type          — the verb (Like / Comment / etc.)
//   sender_key_version     — sender's key version sealed against
//   recipient_key_version  — recipient's key version sealed against
//
// One row per encrypted recipient. A broadcast activity to N
// peers that all support e2e produces N audit rows.
func (r *Recorder) FederationEmissionEncrypted(
	ctx context.Context,
	peerID, activityType string,
	senderKeyVersion, recipientKeyVersion int32,
) {
	meta := map[string]any{
		"peer_id":               peerID,
		"activity_type":         activityType,
		"sender_key_version":    senderKeyVersion,
		"recipient_key_version": recipientKeyVersion,
	}
	r.write(ctx, EventFederationEmissionEncrypted, nil, nil, reqContext{}, meta)
}

// FederationEmissionSkippedForPeer records a
// federation.emission.skipped event scoped to a single recipient
// peer — fired by the outbox resolver's per-recipient capability
// gate (Phase 1.22.I-d). Distinct call shape from the
// instance-level [EmissionSkipped] so the metadata reflects the
// per-peer decision: peer_id + reason + verb, no object metadata
// because the gate is upstream of resolved-object context.
//
// Multiple events may fire per Resolve call when a broadcast
// activity fans out to several capability-missing peers; each
// gets its own audit row so the operator can see exactly which
// peers were dropped.
func (r *Recorder) FederationEmissionSkippedForPeer(
	ctx context.Context,
	peerID, reason, verb string,
) {
	meta := map[string]any{
		"peer_id": peerID,
		"reason":  reason,
		"verb":    verb,
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

// FederationRemoteActorKeyUpdated records a
// federation.remote_actor.key_updated event per Phase 1.22.I-c.
// Fires from federation/remote.Handler.SetEncryptionKey when the
// inbound key actually changes — first-time advertisement OR a
// version bump (the remote peer rotated). The no-op refresh path
// (same key, same version) intentionally does NOT fire to keep
// the audit log signal-to-noise high.
//
// Pool-bound — the inbox dispatcher's actor-cache upsert is
// best-effort and not transactional, so binding the audit row
// to a tx would change the dispatcher's failure semantics. The
// audit row therefore commits independently; the worst case is
// an audit row without a matching DB write, which the operator
// can detect by cross-referencing actor_uri against
// federation_remote_actors.encryption_public_key_updated_at.
//
// `actorURI` is the federated actor whose key changed.
// `peerID` is the parent peer's UUID (audit metadata for the
// admin federation surface).
// `previousVersion` is the prior version number; 0 means there
// was no key before (first-time advertisement).
// `newVersion` is the version the peer is now advertising.
func (r *Recorder) FederationRemoteActorKeyUpdated(
	ctx context.Context,
	actorURI, peerID string,
	previousVersion, newVersion int32,
) {
	meta := map[string]any{
		"actor_uri":        actorURI,
		"peer_id":          peerID,
		"new_version":      newVersion,
		"previous_version": previousVersion,
		"first_time":       previousVersion == 0,
	}
	r.write(ctx, EventFederationRemoteActorKeyUpdated, nil, nil, reqContext{}, meta)
}

// FederationUserKeyGenerated records a federation.user.key_generated
// event per Phase 1.22.I-b. Fires from the three user-create paths
// (bootstrap, /setup, /admin/seed/users) when EnsureCurrentForUser
// actually mints a new keypair; the idempotent "already had one"
// path stays silent so re-runs don't generate audit noise.
//
// Tx-bound — the audit row commits atomically with the keypair
// insert. The pattern shares' grant/revoke established.
//
// subjectUserRef is the user whose keypair was generated.
// actorUserRef is the principal who triggered the create:
//
//   - nil for bootstrap (no logged-in principal exists at first-
//     boot; the system itself is the actor).
//   - subjectUserRef for the /setup wizard (the new admin is
//     creating themselves).
//   - the seed-endpoint caller's user_ref for /admin/seed/users.
func (r *Recorder) FederationUserKeyGenerated(
	ctx context.Context,
	q *Queries,
	subjectUserRef int64,
	actorUserRef *int64,
	version int32,
	algorithm string,
) {
	meta := map[string]any{
		"version":   version,
		"algorithm": algorithm,
	}
	r.WriteInTx(ctx, q, EventFederationUserKeyGenerated, &subjectUserRef, actorUserRef, meta)
}

// FederationUserKeyGeneratedSystem is the pool-bound
// system-actor variant of [FederationUserKeyGenerated]. Fired by
// the Phase 1.22.I-b boot-time backfill sweep
// ([userkeys.BackfillMissingKeys]) once per user the sweep mints
// a keypair for. Distinct from the tx-bound variant because the
// sweep's commit-then-audit ordering is a deliberate relaxation
// of the write-ahead-audit invariant — see [userkeys.AuditFireFn]
// for the rationale.
//
// Always system-actor (no actorUserRef); no human triggered the
// boot sweep. subjectUserRef is the user whose keypair was just
// minted. Metadata mirrors [FederationUserKeyGenerated] so the
// admin audit feed surfaces both flavours as the same event_type
// + columnar shape — only the actor_user_ref discriminates.
func (r *Recorder) FederationUserKeyGeneratedSystem(
	ctx context.Context,
	subjectUserRef int64,
	version int32,
	algorithm string,
) {
	meta := map[string]any{
		"version":   version,
		"algorithm": algorithm,
	}
	r.write(ctx, EventFederationUserKeyGenerated, &subjectUserRef, nil, reqContext{}, meta)
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

// FederationInboxDecrypted records a federation.inbox.decrypted
// event per Phase 1.22.I-f. Fires from the inbox dispatcher's
// stage-4 decrypt branch once per envelope that successfully
// opened against one of the recipient's retained keys.
//
// Pool-bound (NOT tx-bound) — the dispatcher's
// MarkInboxProcessedWithEncryption commits independently. Pairing
// the audit row to that tx would change the dispatcher's failure
// semantics; the audit feed accepts the "domain write committed,
// audit row missed" gap as the lesser failure mode.
//
//   peerID                  — sender peer's UUID (audit metadata
//                             for the admin federation surface).
//   activityType            — the verb (Like / Comment / etc.).
//   activityID              — envelope.id (so the audit feed
//                             cross-references the inbox row).
//   senderKeyVersion        — sender's key version sealed against
//                             (from envelope.encryption block).
//   decryptedWithKeyVersion — which receiver key actually opened
//                             the payload. 1 = current key (steady
//                             state); 2+ = retained key fallback
//                             fired (rotation grace window).
//   attemptCount            — how many keys the dispatcher tried
//                             before one worked. AttemptCount=1 is
//                             the common case; >1 means rotation
//                             drift saved us.
func (r *Recorder) FederationInboxDecrypted(
	ctx context.Context,
	subjectUserRef *int64,
	peerID, activityType, activityID string,
	senderKeyVersion, decryptedWithKeyVersion int32,
	attemptCount int,
) {
	meta := map[string]any{
		"peer_id":                    peerID,
		"activity_type":              activityType,
		"activity_id":                activityID,
		"sender_key_version":         senderKeyVersion,
		"decrypted_with_key_version": decryptedWithKeyVersion,
		"attempt_count":              attemptCount,
	}
	r.write(ctx, EventFederationInboxDecrypted, subjectUserRef, nil, reqContext{}, meta)
}

// FederationInboxDecryptFailed records a
// federation.inbox.decrypt_failed event per Phase 1.22.I-f. Fires
// from the inbox dispatcher's stage-4 decrypt branch when every
// retained receiver key fails to open the ciphertext.
//
// Pool-bound — same reasoning as [FederationInboxDecrypted]. The
// audit + the MarkInboxRejected(reason=decrypt_failed) commit
// independently.
//
// `reason` is the operator-facing breakdown:
//   - "no_keys_walked"          — recipient had no current + no
//                                  retained keys (post-I-b
//                                  invariant violation; defensive).
//   - "sender_key_missing"      — sender pubkey lookup returned
//                                  empty; pre-I-c peer that hasn't
//                                  advertised an encryption key.
//   - "recipient_unresolvable"  — envelope.to didn't resolve to a
//                                  local user (likely misrouted
//                                  delivery).
//   - "no_key_worked"           — walked every retained key, none
//                                  opened. Tamper, corruption, or
//                                  sender used a recipient key
//                                  version we've fully aged out.
//
// `recipientKeyVersionAttempted` is the version of the FIRST key
// the dispatcher tried (current key, by convention). 0 when no
// receiver keys were walked at all.
func (r *Recorder) FederationInboxDecryptFailed(
	ctx context.Context,
	subjectUserRef *int64,
	peerID, activityType, activityID, reason string,
	senderKeyVersion, recipientKeyVersionAttempted int32,
) {
	meta := map[string]any{
		"peer_id":                        peerID,
		"activity_type":                  activityType,
		"activity_id":                    activityID,
		"reason":                         reason,
		"sender_key_version":             senderKeyVersion,
		"recipient_key_version_attempted": recipientKeyVersionAttempted,
	}
	r.write(ctx, EventFederationInboxDecryptFailed, subjectUserRef, nil, reqContext{}, meta)
}

// FederationEmissionRefused records a federation.emission.refused
// event per Phase 1.22.I-g. Fires from the outbox delivery
// Worker when policy.ChoosePathFor returns EmissionRefused — the
// share sensitivity tier mandated encryption + the recipient
// peer's capability or pubkey wasn't available, so the row was
// not POSTed at all.
//
// Pool-bound — the dispatcher's MarkOutboxRefused commits
// independently. Pairing the audit to a tx would change the
// dispatcher's failure semantics; the audit feed accepts the
// "DB write committed, audit row missed" gap as the lesser
// failure mode (same contract as I-e/I-f).
//
//   peerID        — recipient peer's UUID. Operator dashboards
//                   group on this to surface "which peers cause
//                   the most refusals?"
//   activityType  — verb (Like / Create / aa:Share / etc.).
//   sensitivity   — share tier that triggered the refusal
//                   (restricted / embargo / future tier).
//   reason        — catalogue value from
//                   [outbox.RefuseReason]. Today only
//                   encryption_required_but_unavailable;
//                   future reasons land here as new constants.
func (r *Recorder) FederationEmissionRefused(
	ctx context.Context,
	peerID, activityType, sensitivity, reason string,
) {
	meta := map[string]any{
		"peer_id":       peerID,
		"activity_type": activityType,
		"sensitivity":   sensitivity,
		"reason":        reason,
	}
	r.write(ctx, EventFederationEmissionRefused, nil, nil, reqContext{}, meta)
}

// FederationUserKeyRotated records a federation.user.key_rotated
// event per Phase 1.22.I-h. Fires once per successful
// [userkeys.RotateForUser] call AFTER the commit succeeds.
//
// Pool-bound (NOT tx-bound to the keypair insert): same contract
// as [FederationUserKeyGeneratedSystem] — the keypair commit is
// load-bearing, audit row is observability, and an audit-write
// failure must not roll back the rotation. The pool-bound path
// matches that contract.
//
// subjectUserRef is the user whose key was rotated;
// rotatedByUserRef is whoever triggered the rotation:
//
//   - subjectUserRef == rotatedByUserRef → self-rotation
//     (the user's own /account/security action).
//   - subjectUserRef != rotatedByUserRef → admin-initiated
//     rotation (compromised-key recovery via
//     /admin/federation/users/{ref}/rotate-keys). The audit
//     feed shows "rotated by admin X on behalf of user Y."
//
// `previousVersion == 0` is the defensive first-time-rotation
// path (no prior current key existed); the audit row metadata
// surfaces it so an operator notices the post-I-b invariant
// violation.
func (r *Recorder) FederationUserKeyRotated(
	ctx context.Context,
	subjectUserRef int64,
	rotatedByUserRef int64,
	newVersion int32,
	previousVersion int32,
	algorithm string,
) {
	actor := rotatedByUserRef
	meta := map[string]any{
		"new_version":         newVersion,
		"previous_version":    previousVersion,
		"algorithm":           algorithm,
		"self_rotation":       subjectUserRef == rotatedByUserRef,
		"first_time_rotation": previousVersion == 0,
	}
	r.write(ctx, EventFederationUserKeyRotated, &subjectUserRef, &actor, reqContext{}, meta)
}

// FederationUserKeyRetainedExpired records a
// federation.user.key_retained_expired event per Phase 1.22.I-h.
// Fires once per non-zero sweeper tick (userkeys.Sweeper);
// `count` is the number of retained rows the sweep reaped in
// that pass.
//
// Pool-bound; system-actor (sweeper is the system). No subject —
// the sweep is cross-user. Operators reconcile the per-user
// retention timeline by joining audit rows of
// EventFederationUserKeyRotated (which carry subject) against
// retained_until timestamps; the sweep audit is the "garbage
// collection actually ran" signal, not a per-user one.
//
// Zero-count sweeps are NOT audited — the sweeper's quiet steady
// state would otherwise spam the audit feed.
func (r *Recorder) FederationUserKeyRetainedExpired(
	ctx context.Context,
	count int64,
) {
	meta := map[string]any{
		"count": count,
	}
	r.write(ctx, EventFederationUserKeyRetainedExpired, nil, nil, reqContext{}, meta)
}

// FederationInboxEncryptionRequiredRejected records a
// federation.inbox.encryption_required_rejected event per
// Phase 1.22.I-h. Fires from the inbox dispatcher's receiver-side
// policy gate when a plaintext envelope arrives for a target
// object whose sensitivity tier mandates encryption.
//
// Pool-bound — same contract as [FederationInboxDecryptFailed].
// Distinct event type from decrypt_failed: the decrypt branch
// NEVER ran (the envelope wasn't encrypted), so attributing the
// failure to "decryption" would mislead an operator triaging
// the audit feed. The dedicated event type makes the gate's
// firing greppable.
//
//   peerID        — sender peer's UUID.
//   activityType  — verb on the envelope (Like / Create / etc.).
//   activityID    — envelope.id.
//   objectKind    — target object's kind ("post" / "asset" / etc.);
//                   empty when row.ObjectKind was nil (the gate
//                   shouldn't have fired in that case but the
//                   audit records the actual value seen).
func (r *Recorder) FederationInboxEncryptionRequiredRejected(
	ctx context.Context,
	subjectUserRef *int64,
	peerID, activityType, activityID, objectKind string,
) {
	meta := map[string]any{
		"peer_id":       peerID,
		"activity_type": activityType,
		"activity_id":   activityID,
		"object_kind":   objectKind,
	}
	r.write(ctx, EventFederationInboxEncryptionRequiredRejected, subjectUserRef, nil, reqContext{}, meta)
}
