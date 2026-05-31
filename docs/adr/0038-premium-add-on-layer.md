---
id: "0038"
title: Premium add-on layer — paid artifacts on top of an AGPL core
status: accepted
date: 2026-05-30
area: monetization
phases:
  - "1.38"
  - "1.39"
  - "1.42"
  - "1.45"
  - "1.46"
supersedes: []
related:
  - "0016"
  - "0017"
  - "0021"
  - "0030"
  - "0031"
  - "0034"
  - "0036"
  - "0037"
tags:
  - monetization
  - licensing
  - extensibility
  - commerce
  - ads
  - ai
  - dcc
excerpt: >-
  Premium add-ons are a paid, proprietary, Ed25519-licensed artifact
  layer that sits on top of the AGPL core via the existing capability
  add-on system. They cover the surfaces where operators monetize
  third parties (commerce, ads, memberships) and where we carry
  operational burden on their behalf (cloud-bridge AI, premium SSO,
  DCC plugins, backup / DR). The core stays AGPL and feature-complete
  for self-hosters; the tier system stays orthogonal; no feature is
  ever clawed back into a paywall.
---
## Context

[ADR 0016](/adr/0016-license-direction/) committed Artist Alley to an
**AGPL + commercial** dual posture. [ADR 0017](/adr/0017-monetization-and-licensing/)
specified three license tiers (Community / Pro / Enterprise) with
seat-count + asset-count + feature-set differences, enforced through
Ed25519-signed `.lic` files issued by a Cloudflare Workers signing
service. [ADR 0034](/adr/0034-capability-add-ons/) formalised the
**capability add-on** pattern — out-of-band heavy artefacts (Docker
images, model weights, runtime binaries) pulled from a curated
registry and lifecycle-managed from an admin surface.

Two operational questions follow:

1. **What's the line between free OSS and paid commercial?** Tier
   differentiation alone (seat counts, audit-export signing,
   multi-tenant federation) is a thin moat. A studio with fewer
   than 15 active artists in Community can run forever without
   paying us, and a community fork can ship "Community Plus" with
   the gates removed.
2. **What do operators monetize *with* the platform?** A growing
   list of phases puts payment-routing-shaped surfaces into the
   core: [Phase 1.38 ad slots](/adr/0030-operator-ad-slots/) and
   [Phase 1.39 commerce](/adr/0031-commerce-stripe-shopify/) are the
   obvious ones; future memberships, affiliate tracking, paid-portal
   surfaces follow. Each lets an operator extract value from third
   parties — viewers, buyers, subscribers — using software we wrote.

The honest commercial argument is simple. If an operator is using
Artist Alley to **make money from their own users**, taking a slice
through a paid add-on is fair. If we are **carrying operational
burden on their behalf** — hosting inference, signing compliance
exports, brokering paid DCC integrations — taking a slice is fair.
Neither of those slices reaches the operator who is using the
platform exactly as the AGPL Community tier promises: self-hosted,
internal, no third-party value extraction.

Three observations pin the design:

- The **capability add-on system already exists** (ADR 0034). It
  ships first-party from a curated registry, has a manifest format,
  lifecycle hooks, audit + metric integration, and is digest-verified
  at install. Premium add-ons are not a new mechanism — they are an
  attribute on existing add-on entries plus a per-install Ed25519
  license check.
- The **Ed25519 license infrastructure already exists** (ADR 0017).
  Tier licenses are signed `.lic` files validated locally without
  phone-home. Per-add-on licenses follow the exact same shape —
  same keys, same Cloudflare Worker, same customer portal.
- The **AGPL + EULA combination is a well-trodden pattern**. Home
  Assistant Core is Apache-2.0 + Nabu Casa Cloud (proprietary
  hosted bridge under EULA). The Linux kernel is GPL + proprietary
  modules over a stable interface (NVIDIA's driver shipped this
  way for two decades). The pattern requires the proprietary part
  be a *separate work* over a *stable interface*, not a derivative
  of the GPL/AGPL code. Capability add-ons are explicitly separate
  artifacts (containers / weights / binaries) with a stable HTTP
  interface — they satisfy that test by construction.

This ADR formalises the premium add-on attribute on the add-on
registry, the free-vs-paid line, the licensing posture, and the
catalogue of paid add-ons we intend to ship.

## Decision

Introduce **Premium Add-ons** as an attribute (`premium: true`) on
the capability add-on registry from ADR 0034. Premium add-ons are
paid, proprietary, Ed25519-license-gated artifacts that satisfy the
same registry / lifecycle / audit / metric interfaces as free
add-ons. They are orthogonal to the Community / Pro / Enterprise
license tier — any tier can purchase any premium add-on.

### The free-vs-paid line

An add-on is **free** if it is:

- **Substrate for OSS users** — running models locally on the
  operator's hardware. `ai-clip`, `ai-whisper`, `ai-tesseract` stay
  free regardless of how expensive the model weights are or how
  much GPU they need. *The operator's GPU is the operator's GPU.*
  (Already stated in ADR 0034; restated here as a load-bearing
  invariant.)
- **First-party DCC integration for open-source tools** — Blender,
  Krita, Inkscape, GIMP, FreeCAD, Godot. The open-source DCC
  ecosystem is foundational to the audience and we do not paywall
  artists who use free tools.
- **Migration on-ramp from competitive platforms** — the
  Phase 1.25 RS migrator stays free because it is the largest
  conversion audience. Future "import from X" tools at the
  framework level (Phase 1.43) stay free where the connector is
  generic; only specifically licensed commercial-vendor migrators
  (proprietary SDK fees, premium support obligations) become paid.

An add-on is **paid** if it falls in one of two buckets:

**(1) Operator monetizes third parties through it.**
- `aa-commerce` — Stripe / Shopify / Gumroad payment routing,
  storefront, digital-delivery signing.
- `aa-ads` — AdSense / Carbon / EthicalAds / Meta slot management,
  consent-routing, frequency rules.
- `aa-membership` — patron-tier recurring subscriptions to portfolios
  and WIP feeds.
- `aa-affiliate` — outbound link tracking and partner-revenue
  attribution.

**(2) We carry operational burden on the operator's behalf.**
- `aa-cloud-ai` / `aa-cloud-translation` / `aa-cloud-transcription`
  — managed inference for capability slots; pay per token /
  minute / image instead of provisioning GPU.
- `aa-maya` / `aa-3dsmax` / `aa-zbrush` / `aa-houdini` /
  `aa-substance` / `aa-marvelous` / `aa-marmoset` / `aa-unity` /
  `aa-unreal` — DCC plugins for proprietary tools; carry the
  vendor-version churn so the operator doesn't.
- `aa-backup-dr` — multi-region S3 replication, point-in-time
  restore, scripted DR drills.
- `aa-sso-premium` — Okta / Auth0 / WorkOS / Azure AD bridges with
  rich SCIM provisioning; generic SAML / OIDC stays in Enterprise
  tier.
- `aa-compliance` — SOC 2 / HIPAA / GDPR audit-export templates,
  signing keys, retention-policy enforcement helpers.

Premium add-ons that don't fit either bucket are not shipped. We do
not paywall storage tooling, accessibility features, federation,
audit logging, plugins, or any feature already promised at a tier.

### What stays in core

The **admin surface for every premium add-on stays in core**. An
operator with no `aa-commerce` license sees the Commerce admin
section in `/admin` with an empty state: *"Commerce is a premium
add-on. Install `aa-commerce` from the Capabilities registry to
enable."* The route, the navigation entry, the i18n strings, the
empty-state graphic — all AGPL.

The **payment / serving / signing code paths** live in the
proprietary add-on artifact. Without the add-on running, those
routes return 404 / 503 / "feature not available". Forking the
core does not bring those routes to life.

For DCC plugins, the same shape applies: the AA-side stable HTTP /
WebSocket contract is documented and AGPL; the DCC-side plugin
binary is proprietary and licensed.

### Licensing posture — three layers

Defence is layered:

1. **AGPL on the core.** Any fork that modifies AA's core must
   release source under AGPL. This is upstream-friendly: forks
   that improve the core feed back. AGPL does not directly stop
   commercial use; it requires source disclosure.
2. **Add-on EULA on the proprietary artifact.** Each premium
   add-on ships under a separate, non-redistributable EULA
   forbidding extraction / reverse-engineering / redistribution.
   The AGPL core calls the add-on across a stable, documented
   interface; the add-on is a separate work. (NVIDIA-kernel-module
   precedent.)
3. **Ed25519 license check at add-on startup.** Each premium
   add-on validates a per-install `.lic` file at boot — same
   Ed25519 keys, same Cloudflare Worker signing service, same
   customer portal as the tier license. Without a valid `.lic`,
   the add-on container refuses to start. This is not DRM in the
   AGPL core — it is gate code inside the proprietary artifact,
   under that artifact's EULA.

A forker can have the AGPL core (free, lawful) but cannot run
the premium add-ons without paying. They can re-implement an
equivalent commerce or ads add-on under AGPL — at that point we
are competing on engineering, not on permission, which is the
intended trade.

### Pricing-shape buckets

We commit to pricing-shape *buckets* in this ADR, not specific
prices (those move). Buckets:

| Add-on family | Pricing shape | Rationale |
|---|---|---|
| Revenue-extraction (`aa-commerce`, `aa-ads`, `aa-membership`, `aa-affiliate`) | Flat $/month + small transaction surcharge on cloud-bridged operations | Operators pay us a slice of what they themselves monetize |
| Cloud-bridge (`aa-cloud-*`) | Pure metered (per token / minute / image), monthly billed | Pass-through of upstream API cost + margin; no flat fee |
| DCC plugins (`aa-maya`, `aa-3dsmax`, etc.) | Flat $/seat/year | Matches DCC vendor conventions; artists understand this shape |
| Operational burden (`aa-backup-dr`, `aa-sso-premium`, `aa-compliance`) | Flat $/install/year | Carrying operational obligation, not a per-event cost |

All buckets bill via Stripe in our Cloudflare Pages customer portal
(same as ADR 0017's tier billing). Annual + monthly cadence.

### Tier orthogonality (no clawback)

Premium add-ons are an axis **orthogonal** to the Community / Pro /
Enterprise tier from ADR 0017. A Community operator can buy any
premium add-on standalone — they pay only for what they use. An
Enterprise contract can bundle multiple add-ons at a discount.

Features already promised at a tier do not move into a paid add-on
(ever). The line is: **what's in the planned Community / Pro /
Enterprise tier specs stays in those tiers**. Premium add-ons add
new capabilities; they never reach back into existing ones.

This ADR is the explicit firewall against future temptation to
move a planned-free feature behind a paid add-on. Doing so requires
an ADR supersede on this one, with a real business reason in
writing, not a quiet roadmap rewrite.

### Add-on manifest extension

ADR 0034's `addon.yaml` manifest gains two fields:

```yaml
premium: true            # boolean; default false
license:
  required_lic: aa-commerce-pro     # license key name, validated at startup
  signing_key:           # Ed25519 public key (PEM); same as tier-license key by default
    fingerprint: aa-license-2026
  cloud_bridge_endpoint: https://commerce.artist-alley.org/api/v1
                         # optional; for cloud-bridged add-ons that route through us
```

`premium: false` (the default) preserves the free add-on shape from
ADR 0034 exactly. `premium: true` enables the `.lic` check at
startup and adds the "Manage subscription" link in the
`/admin/capabilities/<addon>` detail view that deep-links to the
customer portal.

### Distribution

Premium add-ons live on the **same registry** as free add-ons —
`add-ons.artist-alley.org`. Their listing carries a price badge,
a "Subscribe" button (Stripe checkout via the customer portal), and
a license-key issuance step that fingerprints the install.
Air-gapped operators can request a `.lic` out-of-band against an
install-fingerprint export from `/admin/license`.

The registry remains a **single curated source of truth**; we
don't fragment it across "OSS registry" + "paid registry". The
distinction is a flag on the entry, not separate infrastructure.

### Explicit no-list

We do **not** ship as a premium add-on (or any add-on) any of:

- **NFTs / crypto / blockchain payment rails.** Different audience,
  polluting brand, rugpull-adjacent reputational risk.
- **Anti-AdBlock / anti-circumvention DRM in core.** The `aa-ads`
  add-on respects user-agent ad-blocking; we do not arms-race against
  it from the AGPL side.
- **Telemetry phone-home in any free add-on.** Free add-ons emit
  metrics locally (Prometheus) and never call our servers.
- **Engagement-bait / dark-pattern add-ons.** Anything optimising
  for time-on-site or notification-fatigue at the user's expense.

This list is editable in future ADRs but each addition deserves an
ADR not a quiet PR.

## Consequences

### Positive

- A coherent commercial story that doesn't gut the OSS core. Studios
  using AA internally never see a paywall; studios monetizing third
  parties pay us a small slice; studios offloading operational
  burden pay us per their consumption.
- The mechanism already exists — capability add-ons + Ed25519
  licenses — so the engineering cost is the per-add-on work, not a
  new platform.
- AGPL + per-add-on EULA + license-gate is a battle-tested pattern
  (Home Assistant, NVIDIA kernel modules, GitLab Premium add-ons).
  Defensible legally and culturally.
- The free-vs-paid criterion is principled and easy to defend:
  operator-monetizes-third-parties OR we-carry-operational-burden.
  Reduces the "is this paywallable?" judgement call to a check
  against two predicates.
- Premium add-ons add a third revenue stream beyond tier licenses
  and managed hosting — sales scale with operator success on the
  revenue-extraction add-ons, which aligns our interests with
  theirs.

### Negative

- The premium-add-on EULA and licensing infrastructure is real
  legal + engineering work that has to be done right. A botched
  `.lic` check is a customer-facing outage; a botched EULA invites
  redistribution disputes.
- Reframing Phase 1.38 + 1.39 as premium add-ons after they were
  drafted as in-core features is documentation churn — every
  cross-reference in the docs has to be re-read. The orthogonal
  tier-vs-add-on model takes effort to communicate; existing
  external messaging will need updating.
- DCC plugins for proprietary tools (`aa-maya` etc.) carry
  vendor-version-churn liability we did not previously sign up
  for. Maya 2024 / 2025 / 2026 each ship plugin SDK changes. Each
  paid plugin commits us to that treadmill for paying customers.
- The cloud-bridge add-ons require us to host inference at scale
  with billing infrastructure (Stripe metered usage, monthly
  reconciliation). This is its own ops surface, separate from the
  signing-service ops from ADR 0017.
- Cultural risk: every "premium add-on" announcement looks like a
  feature being pulled, even when it isn't. We have to be loud
  and clear that the tier line is the contract, the add-on line
  is additive.
- Forks that re-implement premium add-ons under AGPL erode the
  revenue moat over time. The defence depends on us shipping
  better engineering than community re-implementations — that is
  the explicit, intentional trade.

## Alternatives considered

- **Tier-only monetization (Community / Pro / Enterprise as the
  sole lever).** The simplest model; what ADR 0017 currently
  specifies. Rejected because the tier moat is thin — fork-and-strip
  is trivial — and because it conflates two orthogonal axes
  (scale-of-use vs revenue-extraction) into one price point.
  Premium add-ons separate them cleanly.

- **Closed-core / open-shell.** Make the core proprietary, ship a
  thin AGPL shell. Rejected: violates the AGPL + commercial
  commitment from ADR 0016, alienates the OSS audience the project
  was started for, and removes the upstream-contribution path that
  makes AGPL valuable to us.

- **SaaS-only monetization.** Build a managed hosted offering and
  charge for that, leave everything else free. Rejected because
  self-hosting is a design commitment (ADR 0006: single binary;
  Enterprise customers always self-host) and a managed offering
  is at best the fourth revenue stream behind tier licenses,
  premium add-ons, and DCC-plugin seats.

- **Revenue share on operator earnings (transaction-based only).**
  Take a percentage of every Stripe / Shopify sale routed through
  Commerce. Considered, partially adopted: the `aa-commerce`
  add-on includes a small surcharge on cloud-bridged operations,
  but the primary lever is a flat add-on subscription. Reasons:
  pure rev-share invites operators to route around us (their own
  Stripe account, our software, no checkout-through-us = no fee);
  rev-share with mandatory checkout-through-us locks operators
  into our infrastructure in a way that conflicts with the
  self-host commitment. Flat-subscription + small cloud-bridge
  surcharge captures the value-extraction signal without the
  lock-in.

- **Third-party plugin marketplace where we take a cut.** Defer
  premium-add-on revenue to the WASM plugin marketplace from
  Phase 1.23. Rejected as the *primary* model because the WASM
  plugin layer is years out and third-party authors need an
  established marketplace before they'll invest, but kept as a
  *future* model — once Phase 1.23 ships, a 70/30 third-party
  plugin store becomes a fifth revenue stream alongside this ADR's
  first-party premium add-ons.

## Implementation

This ADR informs five existing-or-new phases; each lands as its
own work.

- **ADR-only / no phase**: this ADR itself ships first to establish
  the firewall before any premium add-on lands. Add-on registry
  schema gains the `premium` + `license` fields in the manifest
  spec (ADR 0034 supplement) — implementation lands with Phase 1.42.

- **Phase 1.42 — Capability add-ons (already planned).** Extended
  to support the `premium: true` flag and per-add-on `.lic` check
  at install / startup. The free add-ons (`ai-clip`, `ai-whisper`,
  `ai-tesseract`) ship with `premium: false` and validate as free.

- **Phase 1.38 — Operator-configurable ad slots (reframed).** Now
  ships as `aa-ads` premium add-on. The admin Ad Slots surface
  remains in the AGPL core with an empty state until the add-on
  is licensed.

- **Phase 1.39 — Commerce (reframed).** Now ships as `aa-commerce`
  premium add-on. The admin Commerce surface + storefront route
  remain in the AGPL core with empty states until the add-on is
  licensed.

- **Phase 1.45 — Premium DCC integrations (new).** First-party
  paid plugins for Maya + Substance Painter + Unreal at v1. Free
  Blender / Krita / Inkscape / GIMP / FreeCAD plugins ship as
  in-tree integrations under AGPL.

- **Phase 1.46 — Cloud-bridge add-ons (new).** `aa-cloud-ai` /
  `aa-cloud-translation` / `aa-cloud-transcription`. Operator pays
  metered; we host the inference. Locally-hosted equivalents stay
  free under the existing capability slot.

Subsequent premium add-ons (`aa-membership`, `aa-affiliate`,
`aa-backup-dr`, `aa-sso-premium`, `aa-compliance`) are deferred to
future phases and follow the same template.

## References

- ADR 0016 — License direction. AGPL + commercial; add-on artifacts
  are separately licensed.
- ADR 0017 — Monetization & licensing. Ed25519 `.lic` infrastructure
  reused per add-on; three tiers stay orthogonal to add-ons.
- ADR 0021 — Platform integrations. Credentials store reused for
  cloud-bridge add-ons.
- ADR 0030 — Operator-configurable ad slots. Reframed as `aa-ads`.
- ADR 0031 — Commerce — Stripe + Shopify. Reframed as `aa-commerce`.
- ADR 0034 — Capability add-ons. The layer this builds on; gains
  the `premium` + `license` manifest fields.
- ADR 0036 — External imports. Connector kinds stay free at the
  framework level; specifically licensed commercial-vendor
  connectors may become paid in future phases.
- ADR 0037 — Caption & subtitle artifacts. The cloud-bridge
  translation add-on is `aa-cloud-translation` at Phase 1.46.
- [Home Assistant Core (Apache-2.0) + Nabu Casa Cloud (proprietary
  hosted bridge)](https://www.nabucasa.com/) — pattern reference.
- [NVIDIA kernel modules over a stable Linux interface](https://www.kernel.org/doc/html/latest/process/license-rules.html)
  — proprietary-over-stable-interface precedent for GPL/AGPL cores.
