---
id: "0054"
title: Account lifecycle + email substrate + admin impersonation + self-service 2FA
status: accepted
date: 2026-06-26
area: security
phases:
  - "1.19"
supersedes: []
related:
  - "0010"
  - "0017"
  - "0041"
tags:
  - auth
  - email
  - 2fa
  - account-lifecycle
  - operator-grade
excerpt: >-
  Operators can run a public AA instance: SMTP email substrate + admin impersonation + self-service TOTP 2FA + self-registration with email verification — all built on the existing capability + audit + sessions infrastructure with no architectural regressions.
---

## Status (2026-06-26)

Phase 1.19 shipped across 4 PRs:

- **1.19.A-1 (PR #161)** — Email substrate: SMTP-at-rest, async send job, notification job integration, admin test-send surface
- **1.19.A-2 (PR #162)** — Admin impersonation: session shape, HTTP endpoints, always-visible banner, audit emit recording both actor IDs
- **1.19.B (PR #163)** — Self-service TOTP 2FA: enrollment, login gate, recovery codes
- **1.19.C (PR #164)** — Self-registration + email verification: signup flow, verification token, admin approval queue

What this means operationally: AA can now accept a public user without admin handholding. Operator stands up the instance, configures SMTP, opens registration → users sign up, verify their email, optionally enrol 2FA, and use the instance. The RS-gap audit's S-tier blocker arc closes here.

## Context

The 1.17 identity arc shipped INTERNAL identity (roles, capabilities, teams, audit log) and the bootstrap-admin workflow. INTERNAL means: admin seeds users; no public-facing entry exists. Per the RS-gap audit (2026-06-22), this was the single largest blocker to AA being usable by anyone other than the operator themselves.

Closing the gap required four discrete substrates that share dependencies and capability infrastructure but are otherwise independent feature surfaces. This ADR documents the integrated decision because the substrates compose intentionally — email is required for verification, impersonation needs audit, 2FA gates login, self-registration uses verification + email — and treating them as separate ADRs would obscure the composition.

### Why all four in one phase

Each one alone is a half-measure:

- Email substrate without self-registration → operators get test-send but no real users to email
- Self-registration without email verification → spam farm risk on day one
- 2FA without self-registration → admin-only feature; nobody to enroll
- Admin impersonation without the rest → admin can act as users that don't exist yet

Shipping them together is the smallest deployable unit that delivers the actual operational outcome: "anyone can use this instance, securely."

### Departure from original 1.19.B brief

The original 1.19.B brief called for password reset + per-username account lockout. The federation agent's pre-audit found that:

- Password reset flow folded cleanly into 1.19.C alongside email verification (same token infrastructure, same email pattern); shipping it as part of self-registration was the natural shape
- Per-username account lockout could be deferred without blocking the arc (existing per-IP rate limiting prevented the worst attacks)
- TOTP 2FA was a higher operator-grade win for the same effort budget and was already on the gap audit's wider list

The substitution was approved by the operator pre-flight. **Per-username account lockout remains open** as a follow-up issue for a future security-hardening pass.

## Decision

### Email substrate (1.19.A-1)

SMTP client lives in `app/internal/email/` with at-rest secret resolution via the existing secrets-vault (`secret_ref` shape). Async send job `email.send` registered with the existing job framework; uses the email-low priority lane to avoid starving other work. Test-mode capture (`smtp_host = "test://capture"`) writes to `email_capture` table for E2E tests to assert against.

Templates registered at boot via `templates.Register(key, defaultSubject, defaultBody, varsSchema)` — defaults live in code, edits live in DB, DB wins. Operators edit at `/admin/email-templates`; "reset to default" deletes the DB row and the next boot re-seeds.

Failure routing: SMTP transient → retry per job-framework backoff; permanent → `email_failure` admin queue using the PR-B admin-queue pattern.

Observability: `/admin/email/health` per the generic `/admin/{subsystem}/health` shim; counter for `email.sends_total{template, result}`.

### Admin impersonation (1.19.A-2)

New capability `auth.impersonate` — admin-only, never seeded for any other role. New `impersonation_session` table; admin's existing session pauses, new session has `impersonator_user_id` set. On stop: impersonation session deleted, admin session unpaused.

**Capability intersection**: an impersonator's effective capabilities are the INTERSECTION of (admin's caps, target user's caps) — NOT just the target's caps. Prevents privilege escalation through impersonation.

**Audit always fires**: every changeset written during impersonation records BOTH `actor_user_id` (target) AND `impersonator_user_id` (admin). Operator can disable the user-notification email (`auth.impersonation_notify_user`, default `true`); cannot disable the audit row.

**Always-visible banner**: `ImpersonationBanner.svelte` mounted in root layout; never dismissible; "Click here to stop" action.

### Self-service TOTP 2FA (1.19.B)

Standard TOTP (RFC 6238) using `github.com/pquerna/otp` (pure Go). Enrollment flow: user generates secret → renders QR for authenticator app → user enters code to confirm → secret committed + recovery codes generated. Login gate: after password succeeds, if 2FA enabled, second factor required. Recovery codes: 10 single-use codes generated at enrollment; re-generatable from account settings (invalidates old set + emits audit).

**No SMS / no email-OTP fallback** — TOTP only in v1. Per the no-CGo guardrail and the operator-grade-security posture; SMS adds Twilio/etc dependency and email-OTP weakens the security model.

### Self-registration + email verification + approval queue (1.19.C)

Three operator-configurable modes via `system_config.auth.signup_mode`:

- `auto_approve` — anyone can sign up + immediately active after email verify
- `auto_approve_by_domain` — auto if email matches `auth.signup_allowed_domains` CSV; else queue
- `manual_queue` — every signup waits for admin approval

Two-stage activation: `signup → verification_pending → email_verified → approval_pending (if not auto-mode) → active`.

**Email enumeration safe**: signup with already-registered email returns 200 "check your inbox" but sends NOTHING (existing user's email is silent; new user gets verification email). Same anti-enumeration shape on password-reset endpoint.

**GDPR shape**: abandoned signups (no verification within 30d, sysconfig-configurable) are hard-deleted by a scheduled job. Verified-but-not-approved users count toward the operator's quota only after approval (per ADR 0017 active-seats model).

Approval queue admin surface at `/admin/account-approvals` follows the PR-B admin-queue pattern.

### Federation

All four substrates are per-instance:

- Email sends are local (no peer sends email on your behalf)
- Impersonation is per-instance (cannot impersonate federated users)
- 2FA secrets are per-instance (federated identities authenticate on their home instance)
- Signups are local (federated identities never go through signup)

### Observability surfaces

All four subsystems expose `/admin/{subsystem}/health` per the generic shim from 1.18.A-2 PR-B:

- `/admin/email/health` — send counters by template + result
- `/admin/auth/health` — login attempts, 2FA challenges, signups, verifications, impersonation events
- Existing admin queue surfaces use the PR-B paginated + filterable + bulk-action pattern

### What this is NOT

- **Not an SSO replacement.** SSO / SAML / OAuth config UI exists today as a stub; real enforcement is a separate arc per ADR 0041 (Identity provider registry + enterprise gates).
- **Not bulk-admin-broadcast email.** Listed on the gap-audit A-tier; separate follow-up.
- **Not email digest preferences** (immediate / hourly / daily / off). Listed on the gap-audit A-tier; the substrate makes it cheap when it lands.
- **Not per-username account lockout.** Deferred from the original 1.19.B brief; tracked as a follow-up.
- **Not SMS or email-OTP 2FA fallback.** TOTP only in v1.

## Consequences

**Positive:**

- Operators can run a public AA instance with no architectural caveats.
- The S-tier blocker arc from the gap audit closes; 90% of "fundamental DAM expectation" gaps that block real users are now closed.
- Email substrate composes with every future subsystem that needs to send mail (notifications, saved-search digests, share-link emails, scheduled reports).
- Impersonation gives support staff a first-class tool with a load-bearing audit trail.
- 2FA gives operator-grade security without adding any external dependency.

**Negative:**

- Four new substrates to maintain; surface-area is meaningful.
- 2FA recovery codes need a documented operator procedure for "user lost both phone and recovery codes" (admin can disable 2FA on user via impersonation + account settings).
- Per-username account lockout remains a known gap until the follow-up lands.

## Alternatives considered

- **Build email substrate later, after self-registration.** Rejected — self-registration NEEDS email; the dependency order is fixed.
- **Skip impersonation; support staff use database access.** Rejected — operator burden + zero audit trail.
- **Ship 2FA later as a separate phase.** Considered; bundled because the operator-security posture demands it and the substrate cost is low (one pure-Go library + standard TOTP flow).
- **Use a third-party email service (SendGrid / Postmark / SES) SDK.** Rejected — SMTP is universal; operator-owned mail server is the safer default; third-party-specific clients add brittleness without functional gain.

## Reference

- ADR 0010 — Permissions / teams / workflow (the capability + audit infrastructure these substrates extend)
- ADR 0017 — License model + active-seats counting (signup quota interaction)
- ADR 0041 — Identity provider registry + enterprise gates (SSO arc; complementary, not overlapping)
- RFC 6238 (TOTP) — second-factor algorithm
- Phase 1.19 — Account lifecycle (this ADR)
- RS-gap audit 2026-06-22 — original S-tier blocker arc finding
- 1.18.A-2 PR-B — admin-queue pattern + `/admin/{subsystem}/health` shim that 1.19 reuses
