---
id: "0070"
title: Public user-profile pages, gated by the existing visibility predicate
status: accepted
date: 2026-07-21
area: architecture
phases:
  - "1.17"
supersedes: []
related:
  - "0024"
  - "0041"
  - "0060"
  - "0063"
  - "0064"
  - "0067"
tags:
  - architecture
  - profiles
  - visibility
  - privacy
  - ux
excerpt: >-
  Artist Alley gets public user-profile pages resolvable by username
  and by ref, showing a display name and avatar plus exactly the
  assets, posts, and collections the viewer is already allowed to
  see — reusing the visibility predicate, so no new enforcement
  plane. Anonymous visibility follows public mode; owners can opt out
  per the privacy model. A post-by-asset route resolves to the posts
  that feature an asset.
---
## Context

The route/link-integrity net from ADR 0068 (added in #477) surfaced
three pre-existing dead internal links (#478) — components that link
to routes that never existed:

- `/users/by-username/[username]` — rendered by `UserMenu`,
  `MobileNavDrawer`, and post-author links.
- `/users/by-ref/[ref]` — rendered by notifications.
- `/posts/by-asset/[id]` — rendered by `SimilarAssetsPanel`.

They are parked in a `KNOWN_GAPS` allowlist so the net still catches
*new* dead links; each entry must be deleted the moment its route
lands. Clicking a user's name or a similar-asset link dead-ends
today.

Two features are implied: (1) a **user-profile page** resolvable by
both username and ref, and (2) a **post-by-asset** lookup route. The
open product question was whether these pages are **public**
(anonymous-visible) or authenticated-only — the "what does the
visitor see" call that ADR 0063's public-mode work deferred for
per-surface decisions.

Nothing in the ADRs decided it. ADR 0024 (privacy and consent)
supplies the frame — anonymous exposure is default-off with per-user
opt-out — but does not itself decide that profile pages exist or what
they show. This ADR makes that call.

## Decision

**Build public user-profile pages, gated by the existing visibility
predicate — not a new access model.**

1. **The pages exist**, resolvable two ways:
   - `/users/by-username/[username]` — the human-facing permalink.
   - `/users/by-ref/[ref]` — the stable-identifier path used by
     notifications and cross-references.
   Both render the same profile. This mirrors ADR 0067's
   standalone-permalink treatment for assets.

2. **A profile shows a display name and avatar, plus exactly the
   assets, posts, and collections the viewer is already allowed to
   see** — the profile's content lists run through the same
   `visibility` predicate (ADR 0063) and content plane (ADR 0064)
   that govern every other listing. There is **no new enforcement
   plane**: a profile is a filtered view keyed on `owner`, nothing
   more. An anonymous viewer sees the owner's public work; an
   authenticated teammate additionally sees team-tier work they'd
   see anywhere else. The plane never confirms restricted content
   (ADR 0064) — a profile is not a side channel around it.

3. **Anonymous visibility follows public mode** (ADR 0063). With
   public mode off, profiles require authentication like the rest of
   the surface. With it on, a profile is anonymous-visible showing
   only public content — and the **owner can opt out** of anonymous
   exposure per the ADR 0024 default-off/opt-out model. Personal
   data beyond display name + avatar (email, real name) is never on
   the public profile.

4. **`post-by-asset` resolves to the posts that feature an asset.**
   An asset can be a member of more than one post, so the route is a
   "posts featuring this asset" resolution, not a 1:1 redirect (it
   may redirect when there is exactly one, list when there are
   several) — and the list is visibility-filtered like any other.

5. **Each landed route deletes its `KNOWN_GAPS` entry in the same
   PR**, returning the link-integrity net (ADR 0068) to
   zero-tolerance for that shape.

The two features are independent slices: the profile page (rows 1–2
of #478) and the post-by-asset route (row 3).

## Consequences

### Positive

- The artist-portfolio surface an "artist alley" platform is named
  for — a public page per creator showing their work — exists, and
  costs almost nothing to enforce because it rides the visibility
  predicate rather than inventing a profile-specific access rule.
- Attribution and discovery links (author names, similar-asset →
  post) stop dead-ending; the three `KNOWN_GAPS` entries clear.
- Privacy posture is inherited, not reinvented: public mode gates
  anonymous access, ADR 0024 gives owners the opt-out, and no
  personal data beyond display-name/avatar is exposed.
- No new authorization code to get wrong — the risky part (who sees
  what) is the predicate that already has a contract test.

### Negative

- A profile aggregates a user's public work in one place, which is a
  discovery surface some users may not expect. The ADR 0024 opt-out
  is the mitigation; it must be wired before profiles go anonymous,
  not after.
- `post-by-asset` returning a list (not a single post) is a small UX
  branch to design (which post to land on when several feature the
  asset).
- Display-name/avatar become a lightly public surface; abuse/
  moderation of those fields is now in scope (defer to the existing
  moderation track, not this ADR).

## Alternatives considered

- **Authenticated-only profiles.** Same page, never anonymous.
  Simpler privacy story, but discards the public-portfolio value
  that fits the platform's thesis, and still needs the same
  visibility gating for the authenticated case. Rejected as the
  default; public mode + opt-out already bounds the exposure.
- **Defer — hide the dead links.** Remove the author-name and
  similar-asset links so nothing 404s, revisit post-v1. Lowest
  effort, but removes attribution and discovery, and leaves
  `KNOWN_GAPS` carrying entries indefinitely. Rejected.
- **A profile-specific access model.** A dedicated "profile
  visibility" setting separate from the predicate. Rejected: it
  would be a second expression of the visibility rule (the exact
  drift ADR 0063 exists to prevent).

## References

- ADR 0024 — Privacy and consent. Default-off anonymous exposure +
  per-user opt-out; the frame for a public profile's personal data.
- ADR 0041 — Identity-provider registry. Profiles are for platform
  users regardless of how they authenticate.
- ADR 0060 — Public read-only demo. Profiles are part of the
  anonymous browsing surface the demo exercises.
- ADR 0063 — Content-visibility predicate. The single enforcement
  point a profile's content lists reuse.
- ADR 0064 — Content-visibility plane. A profile never confirms
  restricted content.
- ADR 0067 — Asset permalink route. The standalone-permalink pattern
  the profile route mirrors.
