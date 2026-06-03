---
id: "0017"
title: Monetization model + technical license enforcement
status: accepted
date: 2026-05-29
area: monetization
phases: 
  - "1.17"
  - "1.24"
  - "1.24.A"
  - "1.24.B"
  - "1.24.C"
  - "1.24.D"
supersedes: []
related: 
  - "0016"
  - "0018"
tags:
  - monetization
  - ai
  - infrastructure
  - auth
  - 3d
excerpt: >-
  ADR 0016 establishes the legal license direction (AGPL + commercial dual-license). This ADR specifies the runtime monetization model: the tier shape, the .lic file format, the enforcement architecture, and what we explicitly are not doing.
---
## Context

ADR 0016 establishes the legal license direction (AGPL + commercial
dual-license). This ADR specifies the runtime monetization model: the
tier shape, the `.lic` file format, the enforcement architecture, and
what we explicitly are not doing.

The model uses Ed25519-signed `.lic` files with tier-based feature
flags and optional host binding — a well-trodden pattern for
self-hostable commercial software — with tier limits sized for the
studio audience and a "same features at every tier" stance to
preserve the open-source community story.

## Decision

### Tiers

Three tiers. Same features at Community and Pro — the upgrade is purely
scale. Enterprise adds procurement-friendly differentiators.

| Tier | Active seats | Asset cap | Differentiators |
|---|---|---|---|
| **Community** | 15 | 50,000 | Full feature set. Free. |
| **Pro** | 50 | 500,000 | Full feature set. Paid. |
| **Enterprise** | unlimited | unlimited | + SAML / OIDC SSO, audit log export, multi-tenant, federation, HA / clustering, priority support |

**Active seats** are defined as `users with last_active_at within the
last 30 days`, not registered users. Studios cycle contractors heavily;
this definition is honest about real usage and removes the upgrade-cliff
caused by registered-seat counts inflating from short-lived contributors.

**Asset** is defined as `a single uploaded file`. Re-uploading the same
content increments the counter. Storage-layer deduplication (content
hashing) is an independent optimization — it saves bytes without
affecting the license count. The two concerns are orthogonal.

### License file format

Ed25519-signed JSON, `.lic` extension. Claims:

```jsonc
{
  "version": 1,
  "kid": "2026-01-key1",                  // public key id, for rotation
  "claims": {
    "tier": "pro",                        // community | pro | enterprise | dev
    "seats": 50,                          // null = unlimited
    "asset_cap": 500000,                  // null = unlimited
    "features": [],                       // explicit feature flags (rare; tier covers most)
    "owner_email": "ops@studio.example",
    "aud": "studio-name-slug",
    "nbf": 1735689600,                    // not-before (Unix epoch)
    "exp": 1767225600,                    // expiry (Unix epoch)
    "host_fingerprint": null,             // optional host binding (Enterprise air-gap)
    "trial": false,                       // Enterprise 60-day trial flag
    "version": 1
  },
  "signature": "base64(ed25519)"
}
```

Notes on the format:

- All fields are designed in **now** even where unused at launch. Format
  stability matters because issued licenses must verify for years.
- `features[]` exists for special bundles (e.g., a Pro customer who pre-paid
  for Enterprise SSO via a partner deal). At launch we expect this list
  to be empty for nearly all licenses.
- `host_fingerprint` is optional and only used for air-gapped Enterprise
  customers. Standard Pro and Community licenses are not host-bound.
- `trial` gates Enterprise 60-day trial licenses minted after a sales
  conversation. No Community-or-Pro trial exists (see "Not doing" below).

### Trial mechanism

- **No Community trial.** Community is free permanently.
- **No Pro trial.** Pro and Community share the full feature set, so a
  trial would only offer "slightly more seats" — not a meaningful
  evaluation experience. Upgrades happen when Community caps are hit.
- **Enterprise: 60-day trial.** Minted after a sales conversation. Same
  `.lic` file format with `trial: true` and a 60-day `exp`. Reverts to
  Pro behaviour on expiry (the binary degrades to Pro caps, not to
  unlicensed state, so the studio is not locked out of their own data).

### Enforcement architecture

The verification logic is in plain Go source in the open-source
distribution. No binary blobs, no private-repo dependencies — both
would break studio source-audit requirements at AAA procurement. The
realistic deterrent against casual license stripping is layered:

1. **Tangled value derivation across many consumers.** No single
   `if !license.valid { reject }` gate. Instead, license state is the
   input to multiple boring-looking derived values:
   - `searchQuota` — daily search ops budget
   - `uploadConcurrency` — parallel-upload worker count
   - `assetCap` — used by upload service to refuse over-quota uploads
   - `enabledPlugins` — set of registered plugin handlers
   - `federationEnabled` — bool consumed by the federation subsystem
   - `cacheSize` — cache.Registry sizing
   - `thumbnailMaxRes` — preview pipeline output bound

   Stubbing the verifier to "always return valid" produces a "valid"
   license whose derived values are all zero — breaking the app's
   surface behaviour in confusing ways. Restoring it requires chasing
   every consumer, which is friction casuals will not invest in.

2. **Multiple check points across the request lifecycle.** Verification
   is invoked on app startup, on upload acceptance, on admin route entry,
   on a background heartbeat (every 15 minutes), and on selected paid
   feature use (e.g., enabling SSO in admin). Removing one trigger
   leaves others firing.

3. **Legal / commercial license terms** (the actual deterrent). The
   commercial license explicitly forbids removing or bypassing the
   license check for commercial deployment. This is what makes
   procurement at any large studio say no — the compliance and audit
   risk, not the technical difficulty.

### Not doing

- **No binary obfuscation** (e.g., garble). Obfuscation only mangles
  the binary; the source on GitHub is the manual for stripping.
  Spending effort on it would be feel-good security theatre.
- **No closed-source license module distributed as a binary blob.**
  Breaks studio source-audit requirements at AAA procurement — the same
  buyers we most want would reject the product outright.
- **No online phone-home / revocation check** for Community and Pro.
  Studios with security audits require zero outbound traffic from
  artist-alley nodes; phone-home would kill the air-gap story. Revocation
  is handled by short license terms (1 year) — non-renewing customers
  simply have their license expire.
- **No cloud-gated paid features at launch** (e.g., centrally
  rate-limited AI gateway). Possibly later, but the cost of running it
  for the Community tier eats margin and the model works without it.
  Revisit if hostile forks materialize.

### Signing infrastructure

- **Private Ed25519 signing key lives in Cloudflare Workers Secrets**,
  never in the repo or on a customer's machine. Worker exposes a
  minimal admin-authed endpoint to mint signed licenses.
- **Cloudflare D1 (or KV)** stores the issued-license database for the
  back-office: customer email, org slug, tier, issuance date, renewal
  cadence.
- **Public verification keys are baked into the Go binary**, one
  per `kid`. Adding a new key is a code change; rotating the active
  signing key requires shipping a release that knows the new `kid`.
- **License purchase / management portal** runs on Cloudflare Pages,
  bound to the Workers backend.

### Implementation phasing

The work is gated on Phase 1.17 (Identity & teams) because seat counting
needs `last_active_at` per user.

- **Phase 1.24.A** — license file format + Ed25519 verification + public
  key bake-in + admin license-status surface.
- **Phase 1.24.B** — tangled value derivation across consumers (search
  quota, upload concurrency, cache sizing, plugin gating, federation
  gating).
- **Phase 1.24.C** — Cloudflare Worker signing service + license
  database + admin minting flow.
- **Phase 1.24.D** — Customer portal on Cloudflare Pages.

## Consequences

**Positive**

- Tier model is generous enough that Community is a usable on-ramp, not
  a crippled demo — preserves the "OSS community" pitch.
- "Same features, different scale" is the cleanest upgrade conversation
  in the OSS-with-paid-tier space ("you grew" beats "we put X behind a
  paywall").
- Active-seats definition is honest about contractor churn and removes
  the upgrade cliff.
- Format-stable license file means issuance changes never require
  shipping a new binary.
- Signing key on Cloudflare Workers is a small, well-defined attack
  surface to defend.

**Negative**

- The license counter and active-seats query is non-trivial code we now
  own — bugs here block legitimate customers. Mitigation: extensive
  tests, fail-open on derivation errors so a verifier bug doesn't lock
  a paying customer out of their own data.
- Tangled derivation makes the code slightly harder to navigate for
  maintainers. Mitigation: `license.go` documents the consumer set
  comprehensively, derivation functions are clearly named.
- We eat AI compute costs for Community when AI auto-tagging ships.
  Mitigation: bound Community AI budget per tier in the tangled
  derivation; revisit cloud-gated AI if costs exceed budget.

## Alternatives considered

- **Adne's exact tier sizes (3 seats / 100k assets Community, 10 / 1M
  Pro).** Rejected — too restrictive for an OSS community-funnel
  strategy. artist-alley wants a larger free tier to attract
  contributors and adoption.
- **Feature-gated tiers (Community without annotations, Pro with).**
  Rejected — friction over "which features did I lose?" beats the
  cleaner "you grew" upgrade conversation. Same-features-different-scale
  is a stronger pitch for OSS.
- **Cloud-gated AI from day one.** Deferred. We accept the Community AI
  compute cost short-term in exchange for the cleaner "self-host
  everything" story.
- **Garble + obfuscated license module.** Rejected per "Not doing".

## Reference

- ADR 0016 — license direction (AGPL + commercial dual-license).
- ADR 0018 (planned) — Blender as optional worker container.
- Phase 1.24 in [`docs/roadmap.md`](../roadmap.md) — implementation
  sequence.
