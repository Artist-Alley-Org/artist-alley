---
id: "0041"
title: Identity provider registry + license-gated enterprise gates
status: accepted
date: 2026-06-04
area: architecture
phases:
  - "1.17"
  - "1.18"
supersedes: []
related:
  - "0010"
  - "0016"
  - "0017"
  - "0040"
tags:
  - auth
  - sso
  - licensing
  - architecture
  - extensibility
excerpt: >-
  Authentication backends, multi-tenancy, and other enterprise-only
  surfaces register through a license-gated process-wide registry that
  evaluates feature flags at construction time. Patching the runtime
  license check in isolation produces nothing, because the registration
  block never ran for unlicensed features.
---

> **Status note (2026-07-13):** the canonical GitHub org is now
> **Artist-Alley-Org** (v0.1.0 org move, 2026-07-11) — repo links in
> this ADR have been host-swapped.
>
> **Amended 2026-07-20 ([ADR 0066](0066-generic-sso-ldap-not-license-gated.md)):** generic
> self-hosted SSO/LDAP is **no longer license-gated** — the `sso_ldap` / `sso_saml` / `sso_oidc`
> features are retired for the generic providers, which now declare `RequiredLicenseFeature() == ""`
> and register unconditionally like password. This applies the "Federation is explicitly NOT gated"
> reasoning below (near-zero enforcement value; OSS positioning worth more) to auth. **The registry
> architecture in this ADR is unchanged** — it still gates multi-tenant and the managed hosted-IdP
> bridge add-on (`aa-sso-premium`, ADR 0038); only the *scope* of what it gates narrows. Read the
> Enterprise-gating framing below as applying to multi-tenant + managed bridges, not generic SSO.

## Context

ADR 0017 establishes the runtime monetization model (tiered `.lic`
files, Ed25519 verification, tangled-value derivation as the
deterrent against casual stripping). It commits to "enforcement is in
plain Go source" and to "multiple check points across the request
lifecycle" but leaves the architectural shape of those check points
unspecified.

When Phase 1.17.P needed to add SSO providers (LDAP, SAML) as
Enterprise-tier-only features, the existing patterns surfaced three
recurring questions:

1. **Where does an enterprise feature plug in?** A scattered
   `if license.HasFeature("sso_ldap") { ... }` at each call site is
   easy to patch in isolation (one boolean per site) and easy to miss
   when a new feature ships.
2. **What happens on community installs?** If the SSO routes mount
   unconditionally and return 403 at request time, a runtime patch
   that flips `HasFeature` to true is a one-byte change. If they
   refuse to mount, an admin who later upgrades to Enterprise has to
   restart the process for the new license to take effect — operator
   pain we have explicitly decided to avoid.
3. **How does the auth subsystem stay future-proof?** Today's set
   is password + LDAP + SAML; tomorrow's is OIDC + Kerberos + mTLS
   + WebAuthn + Apple Sign-in. The auth handler must not know how
   each is implemented — only that *some* set of identity providers
   exists and can answer "do these credentials map to a local user?"

This ADR specifies the architectural seam that answers all three
questions in one shape.

## Decision

Every authentication backend implements a single
`auth.IdentityProvider` interface. A process-wide `auth.Registry`
holds the active set and is rebuilt on every license-state change.
License gating happens at registration (construction-site), not at
request time.

### The interface

```go
type IdentityProvider interface {
    Name() string                      // stable registry id, e.g. "ldap-eng"
    DisplayName() string               // login-screen label
    Kind() ProviderKind                // password | ldap | saml | …
    RequiredLicenseFeature() string    // "" or e.g. "sso_ldap"
    SupportsPassword() bool            // true for POST /auth/login
    Authenticate(ctx, username, password) (AuthResult, error)
}
```

The password / LDAP forms route via `POST /auth/login` with an
optional `provider` field. Redirect-flow providers (SAML, OIDC) get
kind-specific routes (`/auth/saml/{login,acs,metadata}`) and
implement an additional `RedirectFlowHandler` interface alongside
`IdentityProvider`. The split is intentional: the login handler
stays uniform for the credential-verification step regardless of
backend, and only the kinds that need extra HTTP verbs grow extra
surface.

### Construction-site gating

`Registry.Register(p, src)` and `Registry.Replace(ps, src)` both
consult `src.HasFeature(p.RequiredLicenseFeature())` before
admitting the provider. A missing feature is a hard rejection — the
registry never holds an entry that the install isn't licensed to
use. The boot wiring calls Register exactly once per provider with
the install's `*licensing.State` as the source.

The consequence: a runtime patch that flips `HasFeature` to true
gets nothing back. The registration block already ran. The
provider was never constructed. No code path in the auth handler
references the provider directly — it dispatches through the
registry's `Get(name)`.

This is the load-bearing "tangled-derivation" pattern from ADR 0017
applied to the auth subsystem specifically: the license is an
**input** to the construction of the provider set, not a runtime
**check** on a pre-constructed set. Patching the check in isolation
leaves the construction step intact: still nothing in the registry.

### Hot-swap on license change

`licensing.State.OnReload(fn)` registers a callback that fires after
every successful Status swap (boot load, admin upload, 24h
re-verify ticker). The boot wiring registers a callback that
rebuilds the provider list via `buildProviders(...)` and atomically
swaps the registry contents via `Replace`. An admin who uploads a
new `.lic` via `/admin/license/upload` sees the new enterprise
providers register without restarting the process.

`Replace` validates every provider in the new list against the
license source before touching the registry. A single bad entry
rejects the entire swap and leaves the previous state intact — no
partial update can corrupt the registry.

### Redirect-flow routing under hot-swap

SAML and any future redirect-based protocol need three additional
HTTP routes (`/auth/saml/{login,acs,metadata}`). To preserve
hot-swap, the routes are mounted unconditionally at boot, and each
handler performs a per-request lookup against the registry:

```go
func (s *samlRouter) BeginLogin(w, r) {
    h, ok := s.providers.Get("saml")
    if !ok { http.NotFound(w, r); return }
    h.(samlauth.RedirectFlowHandler).BeginLogin(w, r)
}
```

When the SAML provider isn't registered (Community install), the
route 404s — from the client's perspective indistinguishable from a
route that was never mounted. When it IS registered (license uploaded
without restart), the route starts serving immediately.

We accepted the loss of the strict "no route exists at all on
Community" property in exchange for hot-swap. The remaining defense
layers (capability-bridge from ADR 0017, chain-of-trust verifier,
24h re-verify ticker) remain the load-bearing security guarantees.

### Capability gating downstream

ADR 0017 introduced `capabilities.required_license_feature`. The
boot wiring loads that mapping once and the auth `Identity.Can()`
consults it **before** the SuperAdmin shortcut. The capability layer
is therefore the second wall after the registry: even if a patcher
manages to inject a provider, the capability codes that gate its
admin UI (`system.sso.ldap.write`, `system.tenancy.write`, etc.)
deny on a Community install because they declare a
`required_license_feature` the install doesn't hold. Two unrelated
layers, two files, two idioms — a patcher has to bypass both.

### Federation is explicitly NOT gated

The same registry pattern could in principle gate federation
(multi-instance metadata gossip). Federation is **deliberately
excluded** from the enterprise tier: small communities can stand up
two artist-alley instances and federate them freely. The
enforcement value would be near-zero (anyone can run two binaries
on different boxes; we cannot prevent them from gossiping anyway)
and the OSS positioning is worth more than a near-zero deterrent.

Multi-tenant (a single binary serving multiple isolated tenants
under one operator, with per-tenant admins and quotas) IS gated —
that's a vendor-scaling tool with clear commercial value.

## Consequences

### Positive

- One seam to add an SSO provider — implement `IdentityProvider`,
  call `Register` in the boot block. The auth handler, the login UI,
  and the admin status page all pick it up via the registry.
- License gating is structural, not check-based. A patcher who
  defeats one runtime check inherits no compromise of the registry.
- Hot-swap means Community → Enterprise upgrade is paste-license,
  click Install, done. No restart needed; in-flight sessions stay
  live; the operator never has to schedule downtime.
- The redirect-flow split keeps the password handler from growing
  protocol-specific branches. SAML logic stays in `samlauth`; the
  central login flow stays small.
- New protocols (OIDC, mTLS) cost one new package each; the
  registry contract doesn't change.

### Negative

- The "no route exists at all on Community" property is gone for
  redirect-flow routes — they always mount and 404 dynamically.
  Accepted tradeoff per the hot-swap requirement; the
  capability-bridge + chain-of-trust + re-verify ticker remain the
  load-bearing defenses.
- A nil `LicenseSource` passed at construction is treated as
  "no features" — code-load-order bugs at boot can mask as license
  rejection. Mitigation: `Registry.Register` errors loudly with the
  required-feature name in the message; boot wiring logs the
  rejection so operators see "ldap not registered: requires
  sso_ldap" rather than a silent absence.
- Tests that construct a `Handler` directly without a registry get
  a legacy inline-password fallback. Future test infra should always
  wire a real `Registry` to exercise the dispatch path; the
  fallback is preserved only for the existing pre-registry test
  suite. Marked for cleanup when that suite is refactored.

## Alternatives considered

- **Scattered `if license.HasFeature("sso_ldap") { ... }` at each
  consumer.** Rejected — every new consumer is a new patch site,
  and the patcher's job is linear in the consumer count.
- **Build-time stripping of enterprise packages (`//go:build
  enterprise`).** Rejected — incompatible with the AGPL + commercial
  dual-license. Source is public regardless of build tags; the
  enterprise binary would either still need the stripped code (and
  thus be rebuildable) or split the repo (which we have decided
  against per ADR 0017).
- **Closed-source enterprise binary distributed separately.**
  Rejected per ADR 0017 — breaks studio source-audit requirements
  at AAA procurement. Same buyers we most want would reject the
  product.
- **Always mount routes + check at request time.** Rejected for the
  general pattern; accepted only for redirect-flow routes that need
  hot-swap. The pattern is "construct enterprise state only when
  licensed" because that's structurally more robust than "always
  construct, check at use".
- **Restart-required upgrades.** Rejected per user direction —
  operator pain is the wrong tradeoff against a single patcher-byte.

## Implementation

Phase 1.17.P-foundation shipped:

- [`app/internal/auth/identity_provider.go`](https://github.com/Artist-Alley-Org/artist-alley/blob/main/app/internal/auth/identity_provider.go)
  — interface, `Registry`, `Register` / `Replace` / `Get` / `Has` /
  `List`, sentinel errors (`ErrInvalidCredentials`,
  `ErrProviderUnimplemented`, `ErrProviderUnsupportedMethod`).
- [`app/internal/auth/provider_password.go`](https://github.com/Artist-Alley-Org/artist-alley/blob/main/app/internal/auth/provider_password.go)
  — built-in unconditionally-registered provider wrapping the
  existing HMAC-then-bcrypt verify.
- [`app/internal/ldapauth/`](https://github.com/Artist-Alley-Org/artist-alley/tree/main/app/internal/ldapauth)
  + [`samlauth/`](https://github.com/Artist-Alley-Org/artist-alley/tree/main/app/internal/samlauth)
  — license-gated stub packages. Real impls land in 1.18 against
  these blueprints + ADR 0040's clean-room methodology.
- [`app/internal/tenancy/`](https://github.com/Artist-Alley-Org/artist-alley/tree/main/app/internal/tenancy)
  — `Manager` is `nil` without `multi_tenant`; consumers check via
  `m.Enabled()`. Tenant CRUD + middleware land in 1.18.
- [`app/internal/licensing/state.go`](https://github.com/Artist-Alley-Org/artist-alley/blob/main/app/internal/licensing/state.go)
  — `OnReload(fn)` callback hook; `swap()` snapshots + releases the
  lock before firing so callbacks can call back into State methods.
- [`app/internal/http/server.go`](https://github.com/Artist-Alley-Org/artist-alley/blob/main/app/internal/http/server.go)
  `buildProviders` + `samlRouter` — boot constructs once and the
  `OnReload` callback rebuilds on every Status swap.

## References

- [ADR 0010](/adr/0010-permissions-teams-workflow/) — the
  capability + team model the SuperAdmin shortcut + cap-gate
  consumes.
- [ADR 0016](/adr/0016-license-direction/) — AGPL + commercial
  dual-license direction (foundational for why open-source
  enforcement looks the way it does).
- [ADR 0017](/adr/0017-monetization-and-licensing/) — runtime
  enforcement architecture, tier model, tangled-derivation pattern.
- [ADR 0040](/adr/0040-clean-room-reverse-engineering-methodology/)
  — methodology the Phase 1.18 real LDAP / SAML implementations
  follow when adapting `plugins/simpleldap` + `plugins/simplesaml`
  blueprints.
