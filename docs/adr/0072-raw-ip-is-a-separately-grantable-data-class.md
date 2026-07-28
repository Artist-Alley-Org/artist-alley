---
id: "0072"
title: Raw IP addresses are a separately-grantable data class
status: accepted
date: 2026-07-27
area: security
phases: []
supersedes: []
related:
  - "0024"
  - "0032"
  - "0061"
tags:
  - security
  - capabilities
  - privacy
  - authorization
excerpt: >-
  Any admin surface that returns a user's raw IP gates that field on a
  dedicated `<area>.pii.read` capability, additive to the `<area>.read`
  that admits the caller to the surface, and omits the field rather than
  blanking it.
---

## Context

Two admin surfaces return the same data class — a user's raw IP address
— and until now they protected it at two different bars.

- `/admin/audit` returns the actor IP of every event. #425 split that
  field out of `system.audit.read` into a dedicated
  `system.audit.pii.read`, after the public demo granted the read
  capability to a role whose credentials are published, handing anyone
  on the internet the IP of every visitor who had signed in.
- `/admin/users/{ref}/sessions` returns the client IP recorded at login.
  It shipped behind `users.read` alone. #567 removed IPs from the
  self-service `/account/sessions` view entirely but deliberately left
  the admin view alone, and filed #573 against the asymmetry.

One data class, two bars, is a drift generator. The two rules were
already written in two voices — one "personal data needs its own
capability", the other "already behind users.read, that is enough" — and
nothing forced them to stay in agreement. The next IP-bearing surface
(federation peer connections, share-link access logs, lockout
investigation views) would have had two precedents to copy and no rule
to follow, and would have invented a third bar.

There is a real need on both surfaces: locating the source of a
suspicious session or a burst of failed logins is exactly what an
incident responder is doing. The question is never "is this data
available" but "which capability decides".

## Decision

**A raw IP address is its own data class, granted separately from the
surface that carries it.**

### The rule

Any endpoint returning a user's raw IP gates that field on a capability
named `<area>.pii.read`, **additive** to the `<area>.read` capability
that admits the caller to the surface at all:

| Surface | Admits you to the rows | Admits you to the IP |
|---|---|---|
| `/admin/audit`, `/admin/audit/export` | `system.audit.read` | `system.audit.pii.read` |
| `/admin/users/{ref}/sessions` | `users.read` | `users.pii.read` |

`system.admin` is a wildcard in `Identity.Can` and satisfies both
without holding either row, so a full administrator is unaffected.

### The mechanism

Three properties, identical on every such surface:

1. **Field-level, not row-level.** Lacking the PII capability withholds
   the `ip` field; it never 403s the request or drops rows. A support
   role keeps a usable surface.
2. **Omitted, not blanked.** The field is absent from the JSON entirely,
   so an absent value always means "not returned to you" and never "no
   IP recorded". Both fields are already optional and nullable, so
   consumers tolerate absence. (The CSV export is the one exception —
   a stable column layout matters more there, so it emits an empty
   cell; ADR 0032.)
3. **Resolved once per response, in the handler, and passed into the
   mapper** as an `includeIP bool` whose zero value is the safe one. A
   caller who forgets the parameter omits the IP rather than leaking it,
   and any future call site has to answer the question to compile.

### Scope

This governs **raw, recoverable addresses**. It deliberately does not
cover the IP subnet *hash* on lockout events (`audit/events.go`), a
privacy-preserving grouping mechanism that carries no recoverable
address, nor the user-agent string, which labels a device, carries no
address, and is what makes a revoke UI usable at all.

Nothing here changes `/account/sessions`, which returns no IP to anyone
under any capability (#567): it is self-scoped, and on a shared-account
install "self" is every visitor, so the address is omitted
unconditionally rather than gated. Data minimisation first (ADR 0024);
capability gating is for the surfaces that have a genuine need.

## Consequences

### Positive

- One rule for one data class, with a naming convention that tells the
  next IP-bearing surface exactly what to do instead of leaving it to
  pick a precedent.
- An operator can build a read-only user-support role — see who is
  signed in, revoke a stale device — without handing over the addresses
  of everyone who has logged in.
- Failure mode is safe in both directions: a forgotten grant withholds
  data, and a mapper called wrongly omits the field.

### Negative

- A second capability to provision. An operator who wants the old
  behaviour for an existing role has to grant one more thing, and will
  discover this by finding the column empty rather than by being told.
- The capability count grows per area rather than there being one global
  `pii.read`. That is the deliberate trade in "Alternatives", but it is
  a real cost: five IP-bearing surfaces would mean five capabilities.
- The rule is a convention enforced by review, not by the type system.
  Nothing stops a new handler from passing `true`; only the
  safe-zero-value default and this ADR discourage it.

## Alternatives considered

- **Lower audit to match sessions** — drop `system.audit.pii.read` and
  let `system.audit.read` carry actor IPs again. Cheaper, and it also
  ends the asymmetry, but it weakens a control added in response to an
  actual exposure and moves in the unsafe direction.
- **One global `pii.read`** across every surface. Fewer capabilities,
  but it decouples the grant from the area: holding it would mean
  "IPs everywhere, on surfaces you may not even be admitted to", and it
  breaks the `<area>.read` carving the rest of the read capabilities
  already follow (00003 / 00005 / 00006 / 00014).
- **Gate on `system.admin`** — no new capability at all. Rejected in
  #425 for the reason that still holds: it conflates "may administer
  the system" with "may see personal data about users", which are
  different jobs in any organisation large enough to care.
- **Mask rather than omit** (`198.51.100.x`, or a subnet hash) on the
  ungated path. Attractive, and still open as #426, but it is a change
  to what the data *is* rather than to who may read it — orthogonal to
  this decision and applicable underneath it either way.

## Implementation

- Capability definitions: `app/internal/db/migrations/00011_audit_pii_read_capability.sql`
  and `00018_users_pii_read_capability.sql`. Definition only — granting
  to a role is provisioning, not schema.
- Gates: `app/internal/audit/handler.go` (`capAuditPIIRead`, list +
  export) and `app/internal/auth/sessions_handler.go`
  (`capUsersPIIRead`, admin session list).
- Mappers: `audit.toOpenAPI(row, includeIP)` and
  `auth.rowsToAPI(rows, currentID, includeIP)`.
- Tests assert on the **response JSON**, not on the mapper in isolation
  and not on the UI: the exposure being prevented is "the payload
  contains an IP", so that is what gets pinned —
  `app/internal/audit/pii_test.go`,
  `app/internal/auth/sessions_handler_test.go`.
- Frontends derive "should I show this column" from the presence of the
  field in the data, never from a second client-side capability check —
  a client-side copy of the rule is free to disagree with the response
  it is describing (`web/src/routes/admin/audit/+page.svelte`,
  `web/src/routes/admin/users/[ref]/+page.svelte`).
