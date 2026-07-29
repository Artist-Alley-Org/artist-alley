---
id: "0077"
title: A stored credential is never returned, and config that can hold one must be typed
status: accepted
date: 2026-07-29
area: security
phases: []
supersedes: []
related:
  - "0041"
  - "0072"
tags:
  - security
  - sysconfig
  - credentials
  - admin
excerpt: >-
  Three admin-config surfaces stored credentials and two of them handed
  those credentials back on read. The rule: a stored secret is write-only —
  the read reports whether one exists, never what it is — and any config
  structure capable of holding a secret must be a closed typed schema,
  because a free-form map makes the rule unenforceable by construction.
---

# A stored credential is never returned, and config that can hold one must be typed

## Context

Three admin-configurable subsystems store credentials on behalf of the
instance: SMTP (a password), AI/MCP providers (API keys), and SSO
providers (OAuth client secrets, LDAP bind passwords, SAML
service-provider private keys).

SMTP got this right from the start — `password_set` on the read, the
stored value merged back in on a write that omits it. The other two did
not, and each was found separately:

- **#711** — AI and MCP provider API keys were returned verbatim to any
  holder of `system.config.read`.
- **#718** — SSO provider config was returned verbatim by the same
  mechanism. Found while grounding a sprint plan, three weeks after #711.

The second finding is the reason this is an ADR rather than two bug
fixes. #711 fixed the *typed* `api_key` field on the AI path and left
the free-form `config` map on the very next line still being copied into
the response. A credential placed in `config` instead of `api_key` would
have leaked by exactly the route that had just been closed. The fix was
correct for the field it named and blind to the shape beside it.

Two structural facts made this recur:

**The capability asymmetry.** Setting a credential needs a write
capability; reading the config needs `system.config.read`, which is
strictly weaker and much more widely granted. Returning the secret on
read means the narrower write capability protects nothing.

**A free-form map cannot be audited.** When config is
`map[string]any`, the server cannot know which key holds a credential.
Any protection becomes a name-matching exercise against keys that have
not been invented yet.

## Decision

**A stored credential is write-only. The read reports whether one is
set, never what it is. Any configuration structure capable of holding a
credential is a closed typed schema.**

### The rule

1. **Never returned, to anyone.** Not to `system.config.read`, not to
   `system.admin`. There is no read-back workflow for a stored
   credential; if an operator has lost it, they rotate it. "Admins can
   see it" is the assumption that makes every one of these bugs look
   reasonable while it is being written.

2. **The read reports existence.** Each secret field has a `*_set`
   boolean companion, `readOnly`, computed server-side. This is what
   lets an admin surface render "configured" with a rotate affordance
   without ever holding the value.

3. **The write merges rather than replaces.** A PATCH that omits the
   secret — or sends it empty — keeps the stored one, matched on the
   owning entity's ID. Without this, the fix *causes* a data-loss bug:
   an admin UI round-trips what it read, so the first save after the
   read stops returning secrets wipes every credential on the install.
   This is #708's shape (a font PATCH that cleared the logo), and it is
   why the merge is part of the rule and not a refinement of it.

4. **The merge read is load-bearing, so its failure is fatal.** The
   handler must abort if it cannot read the current config. Tolerating
   that error writes a config with every credential blanked — the same
   destruction the merge exists to prevent, reached by a different path.

5. **Config that can hold a secret is a closed typed schema.** Every
   field named, `additionalProperties: false`, no free-form remainder.
   This is what makes rules 1–4 enforceable rather than aspirational.

### What this rejects

- **Denylisting known secret key names.** It **fails open**: the next
  field somebody adds leaks by default, and the failure is silent. The
  cost of being wrong is unbounded and the discovery moment is a
  disclosure.
- **Not returning config at all.** Provably safe and genuinely
  considered. Rejected because it makes every edit a retype-everything:
  the admin surface cannot show non-secret settings, so operators lose
  the ability to see what an integration is pointed at.
- **"It's only admins."** See rule 1. This reasoning is what produced
  all three instances.
- **Free-form config with careful handling.** Care does not survive the
  next contributor. The type is the enforcement.

## Consequences

- Adding a credential-bearing integration means extending a typed schema,
  not adding a map key. That is deliberate friction at exactly the point
  where the mistake gets made.
- A config write now depends on a config read succeeding, so config
  mutation can fail for reasons unrelated to the payload. Callers handle
  that rather than assuming config writes are local and cheap.
- Admin UIs cannot round-trip config blindly. A secret input sends
  nothing when untouched, which the frontend must model explicitly.
- Typed config cannot express an integration's settings before we have
  modelled them. For SSO the field names were taken from the upstream
  protocol vocabulary (RFC 6749, RFC 4511, SAML 2.0) ahead of the
  integration code, so the eventual implementation inherits them rather
  than renaming.
- The rule is retroactive: any existing surface that stores a credential
  is in scope, not just new ones. SMTP already complied; AI/MCP and SSO
  were brought into compliance by #711 and #739.

## References

- ADR 0041 — identity provider registry (the SSO surface this governs)
- ADR 0072 — raw IP as a separately-grantable data class (same shape of
  argument: a data class gated apart from the surface carrying it)
- #711 — AI/MCP API keys; the typed half
- #718 / #739 — SSO provider config, and the free-form half of #711
