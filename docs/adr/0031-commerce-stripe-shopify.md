---
id: "0031"
title: Commerce — sell assets via Stripe + Shopify
status: accepted
date: 2026-05-30
area: monetization
phases: 
  - "1.22"
  - "1.26"
  - "1.28"
  - "1.32"
  - "1.38"
  - "1.39"
supersedes: []
related: 
  - "0017"
  - "0018"
  - "0020"
  - "0024"
  - "0030"
tags:
  - monetization
  - ai
  - infrastructure
  - auth
  - commerce
  - 3d
excerpt: >-
  Operators running public-facing or semi-public Artist Alley instances want to sell their own content: digital downloads (concept-art packs, asset bundles, print-resolution files, retired game assets, soundtrack stems), print-on-demand merchandise, physical originals from archi…
---
## Context

Operators running public-facing or semi-public Artist Alley instances
want to sell their own content: digital downloads (concept-art packs,
asset bundles, print-resolution files, retired game assets, soundtrack
stems), print-on-demand merchandise, physical originals from archive
collections. The two dominant on-ramps are **Stripe** for direct
processing of digital goods and **Shopify** for full-stack commerce
including physical goods + fulfillment.

User locked this in 2026-05-30. This is operator-side commerce —
operators selling THEIR content to THEIR audience — distinct from
Artist Alley's product monetization (ADR 0017 license tiers) and from
ad revenue (ADR 0030).

## Decision

Add Phase 1.39 — Commerce — as a provider-abstracted layer that
attaches pricing + checkout + fulfillment metadata to assets,
collections, and brand-kit items.

### Provider abstraction

```go
type CommerceProvider interface {
    Name() string                                       // "stripe", "shopify", "gumroad", ...
    AuthFlow() AuthFlow                                 // OAuth or API key
    Capabilities() Capabilities                         // {Digital, Physical, Subscription, OneTime}
    CreateListing(ctx, target, price, opts) (Listing, error)
    UpdateListing(ctx, listing, opts) (Listing, error)
    CreateCheckout(ctx, items, buyer) (CheckoutURL, error)
    HandleWebhook(ctx, payload) ([]CommerceEvent, error)
    FulfillDigital(ctx, listing, buyer) (DownloadGrant, error)
}
```

Provider packages live in `app/internal/commerce/<name>/`.

### Initial providers

| Provider | Digital | Physical | Subscription | Notes |
|---|---|---|---|---|
| **Stripe** | ✓ | ✓ | ✓ | Direct checkout; fulfillment for digital via Artist Alley; physical via operator-managed shipping |
| **Shopify** | ✓ | ✓ | — | Hand off to a full Shopify storefront; physical fulfillment + tax + shipping handled there |
| **Gumroad** | ✓ | — | ✓ | Lightweight digital-only; popular with indie creators |

Adding Lemon Squeezy, Paddle, or other follow-ups is a Go package
drop-in.

### Asset / collection pricing model

A `commerce_listing` row binds:

- **Target** — asset, post, collection, or brand-kit item.
- **Provider** — which commerce provider hosts the listing.
- **Provider reference** — Stripe price ID, Shopify product ID, etc.
- **Listing state** — `draft` / `active` / `sold-out` / `archived`.
- **Pricing** — currency + amount, or a `pricing_model` reference
  (subscription, tiered, pay-what-you-want).
- **Delivery type** — `digital-download` / `physical-shipped` /
  `physical-pickup` / `service` / `license-grant`.
- **License grant** — what the buyer can do with what they bought.
  Editable terms; operator picks from a curated list (personal use,
  commercial use, royalty-free, exclusive, etc.).

### Digital-download fulfillment

When a buyer pays for a digital download:

1. Provider webhook fires (Stripe / Gumroad / etc.).
2. Artist Alley resolves the webhook to a listing.
3. Artist Alley generates a single-use share link (Phase 1.26) bound
   to the buyer's email with a 90-day expiry, scope `download`, and
   the licence-grant attached as a watermark / metadata stamp.
4. Email goes to buyer with the share link and a receipt.
5. Buyer downloads via the share link; usage is tracked.

### Physical-goods fulfillment

For Stripe direct (no Shopify), Artist Alley sends an order
notification to the operator with shipping info + Stripe payment
metadata. Operator ships and marks the order fulfilled via the admin
surface; status emails buyer.

For Shopify, AA hands off to the Shopify storefront entirely — buyer
goes to Shopify Checkout, fulfillment happens in Shopify Admin, and
AA only records the listing → order link for analytics.

### Subscription pricing

Stripe-only in v1. Subscriptions grant ongoing access to a collection
(e.g., a "behind-the-scenes" collection of WIP content). Access is
revoked via the scheduled-action engine (ADR 0020) when subscription
lapses.

### Tax + compliance

Defer to the payment processor. Stripe Tax / Shopify Tax / etc. handle
sales-tax calculation, registration, and remittance. Artist Alley does
not become a tax-compliance product; we surface the data the
processor needs.

### Revenue share

AA does **NOT** take a percentage of operator commerce. Operator pays
their processor (Stripe fees, Shopify subscription, etc.); revenue
flows to operator's bank. AA's monetization model (ADR 0017) is
per-tier license fees + Enterprise upsells.

This is the same stance taken in ADR 0030 (ads): AA monetizes the
product, operators monetize their use of it.

### Per-tier behaviour

- **Community:** up to **5 active listings** per instance. Sufficient
  for indie creators dipping a toe in.
- **Pro:** up to **500 active listings** per instance. Real
  commerce-oriented use.
- **Enterprise:** **unlimited listings** + organization-level
  Stripe / Shopify accounts (rather than the studio's personal
  account) + bulk listing import.

Enforced via the tangled-derivation model from ADR 0017 (a
`commerceListingCap` derived value).

### Buyer accounts vs guest checkout

- Default: **guest checkout** via Stripe Checkout / Shopify Checkout.
  No Artist Alley account needed to buy.
- Optional: operator enables "create an Artist Alley account on
  purchase" so the buyer's purchase history + download history is
  surfaced in their AA account.
- The buyer's email lives on the order record either way for
  fulfillment + receipt.

### Public listing surfaces

A listing renders its own card variant in:

- The post-detail modal (a "Buy" CTA next to the cover).
- Browse feed (a small price badge on listing-attached cards).
- Brand-kit pages — branded items with a price are buyable inline.
- A dedicated `/shop/{slug}` storefront surface per operator (opt-in)
  that aggregates all active listings into a browseable shop. Uses
  the same SvelteKit primitives as the main browse feed.

### Refunds + disputes

Handled by the provider; refund notifications come back via webhook
and revoke the digital-download share link (or flag the physical
order for return). Audit log entries record the lifecycle.

### Federation interaction

Listings are per-instance. Federated peers can SURFACE remote listings
(read-only, with a "buy on origin" CTA that deep-links to the issuing
instance's checkout). Federated checkout is explicitly out of scope —
each operator's commerce relationship is theirs.

### Privacy + audit

- Buyer email + purchase metadata is personal data; the DSAR tooling
  in ADR 0024 covers it.
- Purchase history retention defers to the payment processor's
  requirements (typically 7 years for financial records).

## Consequences

**Positive**

- Operators can monetize their content without a separate platform.
  The asset-source-of-truth IS the storefront.
- Provider abstraction means adding Lemon Squeezy / Paddle / new
  market entrants is bounded work.
- Sharing the share-link delivery mechanism with Phase 1.26 means
  digital fulfillment is mostly "wire two existing primitives
  together."
- License grants attached to listings carry forward into the
  buyer's audit / metadata so downstream use is traceable.

**Negative**

- Webhook reliability across providers varies. Mitigation: each
  provider's webhook handler is idempotent + retries on transient
  failures + surfaces persistent failures to operator alerts.
- Tax law is jurisdiction-dependent and changes; we explicitly defer
  to the processor. Mitigation: docs make this clear and link to
  Stripe Tax / Shopify Tax setup guides.

## Alternatives considered

- **Take a revenue share** (a la Patreon, Gumroad's free tier, etc.).
  Conflicts with the per-tier license model. Operators who already
  pay us for Pro / Enterprise should not also lose 5 % to us on each
  sale. Rejected.
- **Skip Stripe; Shopify only.** Excludes indie creators who don't
  want a full Shopify subscription. Rejected — Stripe direct is the
  on-ramp.
- **Build our own checkout flow.** Real money handling is regulated
  territory (PCI compliance, KYC, money transmitter licensing).
  Defer to the processors that already do this. Rejected.
- **No subscriptions in v1.** Considered, but subscription access to
  WIP / patron tiers is one of the most-requested commerce patterns
  for the artist-alley target audience. Worth including from v1.

## Reference

- Phase 1.39 in [`docs/roadmap.md`](../roadmap.md).
- Share links (Phase 1.26, ADR 0018) — digital fulfillment uses these.
- Scheduled-action engine (Phase 1.28, ADR 0020) — subscription
  expiry revokes access.
- Federation (Phase 1.22) — `origin_*` markers extend to listings.
- Privacy & consent (Phase 1.32, ADR 0024) — buyer DSAR coverage.
- Operator-configurable ads (Phase 1.38, ADR 0030) — sibling
  "operator monetizes their instance" pattern with the same revenue
  stance.
- AA monetization model (ADR 0017) — confirms no revenue share on
  operator commerce.
