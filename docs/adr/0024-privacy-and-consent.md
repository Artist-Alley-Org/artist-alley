---
id: "0024"
title: Privacy & consent management — cookie banner, DSAR, retention
status: accepted
date: 2026-05-30
area: security
phases: 
  - "1.17"
  - "1.21"
  - "1.26"
  - "1.28"
  - "1.32"
supersedes: []
related: 
  - "0017"
  - "0020"
tags:
  - security
  - ai
  - auth
  - 3d
excerpt: >-
  A studio running Artist Alley as a self-hosted internal tool has minimal end-user-facing privacy obligations — the operating-system account map covers most. But the moment any of these is true, real GDPR / CCPA / LGPD considerations land:
---
## Context

A studio running Artist Alley as a self-hosted internal tool has
minimal end-user-facing privacy obligations — the operating-system
account map covers most. But the moment any of these is true, real
GDPR / CCPA / LGPD considerations land:

- Artist Alley is exposed to contractors / external reviewers via
  share links (Phase 1.26).
- A public-facing portal surfaces published collections.
- The Pro hosted offering on artist-alley.org takes data from
  customer studios who themselves have privacy obligations.
- The product is sold into the EU / California / Brazil.

The initial audit dismissed cookie banners as marketing-team work.
User override 2026-05-30: ship it. We need the consent layer not
because we have ads — we don't — but because we have analytics, we
have third-party integrations (Slack, Vimeo OAuth, etc.), and we want
to ship into the EU without legal hand-wringing.

## Decision

Add Phase 1.32 — Privacy & consent management — as a small,
opinionated layer that ships consent UI + data-subject-access tooling
+ retention controls. Not a generic consent platform; a sharp tool
for the actual obligations.

### Cookie / tracker banner

- Renders only when **at least one** non-essential category is in
  use on the current instance. A bare studio install with no analytics
  and no third-party embeds shows no banner at all.
- Categories:
  - **Essential** — session, CSRF, locale (always on, no opt-out).
  - **Functional** — theme preference, viewer-state remembering
    (default on, easy opt-out).
  - **Analytics** — first-party usage analytics if the operator
    enables them in admin (default off; opt-in only).
  - **Third-party embeds** — Vimeo player, YouTube embed, etc., only
    if a published page surfaces them (opt-in to load the embed at
    all; until consent, a placeholder shows).
- Banner is unstyled until paired with the theme tokens; fits the
  M3-shaped palette. Reduced-motion respected.
- Operator-configurable text per category in admin (privacy policy
  link, additional disclosure).

### Data Subject Access Requests (DSAR)

- A user can request **export** of all their personal data in a
  machine-readable JSON archive from account settings. Includes
  profile, uploads, comments, annotations, login history, audit
  log entries referencing them.
- A user can request **deletion** of their personal data. Deletion is
  a soft-state for 30 days (recoverable by admin), then hard-deleted
  via the scheduled-action engine (Phase 1.28). Posts they authored
  are reassigned to a `deleted-user` placeholder so collaborative
  content survives. Operator can configure a stricter "delete
  authored posts too" mode for studios with that policy.
- Admin DSAR surface lists pending requests; operator approves /
  rejects with reason.

### Retention policies

- Per-asset-type retention policies set in admin: "auto-archive
  posts after 24 months of no activity," "delete trash items after
  90 days," etc.
- Retention is executed by the scheduled-action engine (Phase 1.28).
- Audit log entries themselves are retained for a configurable
  window; default 7 years (the typical legal retention floor) but
  operator can shorten or extend.

### Privacy policy + terms surfaces

- Two markdown surfaces editable in admin: `/legal/privacy` and
  `/legal/terms`. Default content is a usable starting template
  (NOT legal advice; operators are responsible for fitting to
  their jurisdiction).
- Privacy policy link surfaces in the cookie banner, the account
  settings page, the share-link page, and the footer.

### Anonymous browse policy

- Admin toggle: "allow anonymous browse of public collections."
  Default off. When on, anonymous users see only the categories
  configured for them; no personal data is collected unless they
  click consent.
- This dovetails with Phase 1.21 community & moderation's
  anonymous-browse policy — same toggle.

### Audit log + privacy

- Audit-log entries reference users by ID. Deletion reassigns the
  user ID to a tombstone. The audit log itself does NOT delete its
  entries when a user deletes their account — the entries are
  re-keyed to the tombstone. This preserves change history while
  satisfying right-to-be-forgotten on the personal data axis.

### Per-tier behaviour

- **Community + Pro:** all features above ship for everyone. Privacy
  is foundational, not a paid differentiator.
- **Enterprise:** adds compliance-grade export (audit log export
  signed with the instance's signing key for non-repudiation) and
  multi-jurisdiction policy switching.

## Consequences

**Positive**

- Studios shipping into the EU / California / Brazil have an
  out-of-the-box answer to "does it respect privacy law?"
- DSAR tooling is one of the most expensive features to retrofit;
  building it in early is materially cheaper than later.
- The retention policy engine reuses the scheduled-action engine
  from ADR 0020; no new infrastructure.
- Cookie banner shows only when non-essential trackers are actually
  in use, avoiding the modern web's worst pattern of "annoying
  banner for no reason."

**Negative**

- The defaults are a starting template, not legal advice. Mitigation:
  in-product disclaimer + a recommended-counsel guide in docs.
- Audit-log retention longer than the typical "delete me" expectation
  can confuse users. Mitigation: explicit disclosure in DSAR export
  that the audit log retains tombstoned references.

## Alternatives considered

- **Ignore privacy until a customer asks.** Works until it doesn't;
  EU expansion or California sales come up faster than expected.
  Rejected per user override.
- **Use a third-party consent-management platform (CMP).** Adds a
  vendor dependency and a per-pageview cost. Rejected for a self-
  hosted product.
- **Cookie banner on every page regardless.** Defeats the whole
  purpose; consent banners exist to disclose actual tracking.
  Rejected.

## Reference

- Phase 1.32 in [`docs/roadmap.md`](../roadmap.md).
- Scheduled-action engine (ADR 0020) executes retention policies.
- Audit log (Phase 1.17 + 1.20.A) is the data being protected.
- ADR 0017 monetization model — privacy is NOT a paid differentiator.
