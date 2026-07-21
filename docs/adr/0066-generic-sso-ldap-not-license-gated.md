---
id: "0066"
title: Generic SSO / LDAP is not license-gated
status: accepted
date: 2026-07-20
area: architecture
phases: []
supersedes: []
related:
  - "0016"
  - "0017"
  - "0038"
  - "0041"
tags:
  - auth
  - sso
  - licensing
  - monetization
excerpt: >-
  Self-hosted SAML / OIDC / LDAP move to the free (AGPL) tier — auth is
  security hygiene, not a paywall. The managed hosted-IdP bridges
  (Okta / Auth0 / WorkOS / Azure AD) plus SCIM stay the paid
  aa-sso-premium add-on: that is operational burden we carry, not a
  security tax. Amends the tier tables in 0017 / 0038 / 0041.
---

## Context

[ADR 0041](0041-identity-provider-registry-and-enterprise-gates.md) made
LDAP / SAML / OIDC Enterprise-tier features, gated structurally: each
`IdentityProvider` declares a `RequiredLicenseFeature()` (`sso_ldap`,
`sso_saml`, …) and the registry refuses to construct a provider the
install isn't licensed for. Password auth declares `""` and is always free.

Two facts make gating generic SSO the wrong call:

- **It is the "SSO tax."** Charging for basic single-sign-on forces the
  most security-conscious operators — exactly the studios we want — to buy
  up a tier to get a security-hygiene baseline. The objection (sso.tax) is
  specifically to gating *basic* SAML/OIDC behind a large price step; it is
  **not** an objection to charging for genuinely-more-work managed offerings.
- **The enforcement value is near-zero, and we already conceded that exact
  point once.** ADR 0041 itself de-gated **federation** with the reasoning
  "the enforcement value would be near-zero … the OSS positioning is worth
  more than a near-zero deterrent." Self-hosted SSO is the same shape: an
  operator who wants LDAP will stand it up regardless, and the source is
  public under AGPL anyway. This ADR applies 0041's own logic to auth.

## Decision

**Generic, self-hosted SSO/LDAP is free (AGPL / Community tier).**
The LDAP, SAML, and OIDC providers declare `RequiredLicenseFeature() == ""`,
join the registry unconditionally like the password provider, and their
admin capability codes (`system.sso.ldap.write`, `system.sso.saml.write`, …)
drop their `required_license_feature`.

**The managed hosted-IdP bridges + SCIM stay paid** — the `aa-sso-premium`
add-on ([ADR 0038](0038-premium-add-on-layer.md)): Okta / Auth0 / WorkOS /
Azure AD bridges with rich SCIM provisioning. The line is **who carries the
burden**: an operator configuring their own IdP against generic SAML/OIDC/LDAP
is free; a bridge where *we* absorb per-vendor API churn, SCIM lifecycle, and
directory-sync support is a service, priced as one. This is the sso.tax-clean
boundary — basic auth is never a paywall; managed operational convenience is.

**Enterprise keeps** multi-tenant (single binary, isolated tenants under one
operator — a vendor-scaling tool), signed audit-log export, HA / clustering,
and priority support. Multi-tenant remains gated per 0041; it is a
commercial-scaling capability, not a security baseline, so the SSO reasoning
does not extend to it.

### Architecture impact is near-zero

Nothing in [ADR 0041](0041-identity-provider-registry-and-enterprise-gates.md)
is undone. The `IdentityProvider` registry, construction-site gating,
hot-swap on license change, and the capability-bridge all stay — they are the
right seam regardless of which providers are free. The registry *already*
carries free providers (that is how password works); moving generic SSO to
free is the one-field change of returning `""` from `RequiredLicenseFeature()`
on those providers, plus removing `required_license_feature` from the generic
SSO capability codes. The gating machinery now guards multi-tenant and the
managed-bridge add-on instead of generic SSO.

## Consequences

- **Amends [ADR 0017](0017-monetization-and-licensing.md)** — the Enterprise
  tier row no longer lists "SAML / OIDC SSO" (and, per 0041, no longer lists
  federation either); the "enabling SSO in admin" license-trigger example is
  replaced with a still-gated feature.
- **Amends [ADR 0038](0038-premium-add-on-layer.md)** — `aa-sso-premium`'s
  "generic SAML/OIDC stays in Enterprise" clause becomes "generic SAML/OIDC is
  free"; the add-on itself (managed bridges + SCIM) is unchanged and still paid.
- **Amends [ADR 0041](0041-identity-provider-registry-and-enterprise-gates.md)**
  — the `sso_ldap` / `sso_saml` / `sso_oidc` license features are retired for
  the generic providers; the registry's gating scope narrows to multi-tenant
  and the managed-bridge add-on. The registry design and its security
  properties are otherwise intact.
- **Implementation follow-up** (tracked as a GitHub issue): flip
  `RequiredLicenseFeature()` to `""` on the `ldapauth` / `samlauth` (and future
  `oidcauth`) generic providers, drop `required_license_feature` on the generic
  SSO capability codes, and confirm the managed-bridge add-on path (when built)
  keeps its own gate. Gated alongside the licensing epic (#27).

## References

- ADR 0016 — AGPL + commercial dual-license direction.
- ADR 0017 — monetization model + runtime enforcement architecture.
- ADR 0038 — premium add-on layer (`aa-sso-premium` = managed bridges + SCIM).
- ADR 0041 — the identity-provider registry + the federation de-gating precedent this ADR extends to auth.
