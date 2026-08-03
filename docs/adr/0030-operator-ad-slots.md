---
id: "0030"
title: Operator-configurable ad slots — feed, sidebar, footer
status: accepted
date: 2026-05-30
area: monetization
phases: 
  - "1.23"
  - "1.32"
  - "1.38"
supersedes: []
related: 
  - "0017"
  - "0024"
  - "0079"
tags:
  - monetization
  - ai
  - infrastructure
  - auth
  - commerce
  - 3d
excerpt: >-
  Some operators will host Artist Alley as a public-facing community surface (fan sites, festival hubs, indie collectives, art-school portfolios) where ad-supported hosting is a sensible cost model. Other operators (AAA studios) will never run ads internally. Both should be serv…
---
## Context

Some operators will host Artist Alley as a public-facing community
surface (fan sites, festival hubs, indie collectives, art-school
portfolios) where ad-supported hosting is a sensible cost model. Other
operators (AAA studios) will never run ads internally. Both should be
served by the same product.

The user explicitly asked for AdSense / Facebook / similar ad
integration as an optional surface for hosting operators (2026-05-30).
The constraint set:

- Default off — no operator has ads showing without choosing to.
- Operator picks the provider; we don't take a revenue share.
- Privacy compliance — ads are third-party embeds, must integrate
  with the cookie banner (ADR 0024).
- Per-user opt-out — users can hide ads in account settings.
- No anti-AdBlock arms race — that's a user choice.
- All tiers — Community can run ads on their public-facing instance
  the same as Pro / Enterprise can.

## Decision

Add Phase 1.38 — Operator-configurable ad slots — as an opt-in feature
that exposes a small set of defined ad zones across the feed, sidebar,
post-detail modal, and footer. Operators wire their chosen ad provider
into each zone; the runtime renders the corresponding embed code with
consent + opt-out respected.

### Ad zones

```
feed.banner         — top of /browse, full-width 970×90 / responsive
feed.inline.every-N — between every Nth feed item (operator picks N, ≥6)
sidebar.top         — top of sidebar, 300×250 / responsive
sidebar.bottom      — bottom of sidebar, 300×250 / responsive
post-modal.sidebar  — between metadata sections, 300×250 / responsive
footer.banner       — bottom of page, 728×90 / responsive
```

Operator selects which zones are enabled, per zone. Disabled zones
render nothing (no markup, no JS) — pixel-perfect ad-free for studios
that don't want them.

### Provider abstraction

```go
type AdProvider interface {
    Name() string             // "adsense", "meta", "carbon", "ethicalads", "custom"
    AuthRequired() bool       // does this need an OAuth + secret key?
    Configure(slot, opts) (SlotConfig, error)
    Render(ctx, slot, user) (html, error)  // HTML + JS for the slot
    Categories() []string     // ad-category labels for operator allow-listing
}
```

Provider packages live in `app/internal/ads/<name>/`.

### Initial providers

| Provider | Auth | Notes |
|---|---|---|
| **Google AdSense** | Account ID + ad-unit slot IDs | Most common; large fill rate |
| **Meta Audience Network** | App ID + placement ID | Standard banner / native |
| **Carbon Ads** | Zone code | Curated, design-aesthetic-friendly; common on dev/design sites |
| **EthicalAds** | Publisher ID | Privacy-respecting; no cookies, no tracking |
| **Custom HTML** | None | Operator pastes ad-network HTML / JS directly |

### Operator admin surface

`/admin/system/ads`:

- Toggle per zone (default off).
- Pick provider per zone (a zone can use a different provider than
  another zone).
- Provider-specific config form (account IDs, slot IDs, custom HTML).
- Category allow-list — block ad categories the studio doesn't want
  (no gambling, no adult, no political, etc.). Honored by providers
  that surface category metadata; cosmetic for those that don't.
- Frequency rules — minimum N feed items between inline ads,
  maximum ads per page, time-of-day windows.
- Preview button — render the ad against a sandboxed test slot to
  verify config before going live.

### Consent + per-user opt-out

- The ads category in the cookie banner (ADR 0024) is auto-enabled
  for any instance with active ad slots. Until the user accepts, no
  ad slot renders — the markup is there for layout consistency but
  the JS doesn't run and a transparent placeholder fills the
  reserved space.
- Each user's account settings has an "Hide ads" toggle. When on, no
  ad markup renders for that user (a passive choice, no negotiation).
  Pro / Enterprise tiers MAY (operator's call) suppress ads
  automatically for paid users; admin-configurable.

### Revenue share

We don't take one. Operator ad revenue is theirs. Artist Alley's
monetization is per-tier license fees (ADR 0017) and Enterprise
upsells, NOT a cut of operator-side ad revenue.

### No anti-AdBlock measures

We do NOT detect or fight ad blockers. Users who block ads get a
clean view. This is a deliberate UX call — fighting blockers
generates support tickets and ill will for trivial revenue lift.

### Performance + sandbox

- Each ad iframe is `loading="lazy"` so off-screen ads don't cost
  initial-render time.
- Slot dimensions are reserved with CSS aspect-ratio so ad load /
  unload doesn't cause Cumulative Layout Shift (CLS).
- Sandboxed `iframe sandbox=…` attributes block ad code from
  navigating top-level or executing same-origin attacks.
- Ad JS does NOT have access to logged-in user data; the slot only
  knows the user is anonymous-or-not, never which user.

### Federation interaction

Ad slots are per-instance. Federated peers run their own ad config.
Cross-instance ad bidding is explicitly out of scope.

## Consequences

**Positive**

- Operators running public-facing community surfaces can monetize
  hosting without forking the product.
- Default-off means the studio-internal use case is unaffected.
- The category allow-list + per-user opt-out + AdBlock-friendly
  stance avoid the "rude ad experience" pattern that erodes trust.
- Provider abstraction means a new ad network is a 200-line Go
  package, not a re-architecture.

**Negative**

- Ad slot UI takes screen space even when the slot is consenting-
  but-empty. Mitigation: collapse the slot entirely if the provider
  signals "no ad to fill." Most providers offer this signal.
  *Amended 2026-07-29 — this holds for the banner and sidebar slots
  listed above, which sit in page margins. It does **not** hold for the
  in-grid sized slots introduced by **ADR 0079**: collapsing a 2×2 cell
  leaves a 2×2 hole in the middle of the feed. Those degrade to ordinary
  feed content instead, so the no-fill rule is now per slot kind.*
- Operator can paste malicious custom HTML into the Custom provider.
  Mitigation: Custom provider config is gated by an admin role +
  an explicit "I understand this loads arbitrary code on every
  page" acknowledgement. The HTML renders inside the sandboxed
  iframe so blast radius is bounded.

## Alternatives considered

- **Don't offer ad integration; let operators inject via plugin.**
  Plugin ecosystem (Phase 1.23) is later; making operators wait is
  poor product. Plus the UX bits (CLS, opt-out, consent integration)
  belong in core, not bolted on.
- **Take a revenue share.** Conflicts with the per-tier license
  model. We sell the product; operators monetize their instance.
- **Single provider (AdSense only).** Insufficient — operators with
  privacy commitments need EthicalAds; design-focused sites prefer
  Carbon; some need Meta. Multiple providers is table stakes.
- **Anti-AdBlock detection.** Hostile to users; not worth the
  revenue lift for the support burden.

## Reference

- Phase 1.38 in [`docs/roadmap.md`](../roadmap.md).
- Privacy & consent (Phase 1.32, ADR 0024) gates whether the slot
  fires.
- Monetization model (ADR 0017) — ad revenue is not a paid tier
  differentiator; operators on any tier can run ads.
