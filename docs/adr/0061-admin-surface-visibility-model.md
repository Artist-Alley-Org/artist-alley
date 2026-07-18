---
id: "0061"
title: Admin surface visibility model — public, capability-gated, superuser-only
status: accepted
date: 2026-07-18
area: security
phases: []
supersedes: []
related:
  - "0060"
tags:
  - admin
  - capabilities
  - authorization
  - frontend
excerpt: >-
  Admin navigation resolves against three explicit tiers — universally
  public, capability-gated, and superuser-only — with "no capability
  declared" meaning superuser-only rather than public, because most tiles
  have not yet been migrated to a read capability.
---

## Context

Read capabilities (`system.*.read`, `<domain>.read`) let an operator hold a
read-only admin role without the `system.admin` wildcard. The admin menu
then gates **per tile** on the capability each surface actually enforces,
so a read-only auditor sees the sections their caps permit.

That left one question unanswered: **what does a tile with no declared
capability mean?** The original gate resolved it as

```ts
canSeeTile(tile) { return this.can(tile.cap ?? SYSTEM_ADMIN); }
```

— i.e. no capability implied `system.admin`. That is safe, but it also hid
genuinely public content: the entire **help** section (docs, keyboard
shortcuts, about, release notes, support) declares no capability, so a
read-capability holder could not see About at all. On desktop the link
survived only because the layout hard-coded a second path to it; the mobile
drawer had no such hard-code, so the surface silently differed by viewport.

The obvious fix — treat a missing capability as "visible to everyone" — is
**wrong, and dangerously so**. Only **28 of 100** declared tiles carry a
capability. Flipping the default would have exposed the other 72, including
Federation, System, and Storage, to any read-capability holder. That was
caught only because the change was applied and then driven in a browser.

## Decision

Admin tile visibility resolves against **three explicit tiers**:

1. **`public: true`** — visible to anyone who can reach the admin surface.
   Reserved for content that is not sensitive in any deployment: help,
   documentation, keyboard shortcuts, about, release notes, support.
2. **`cap: '<code>'`** — visible if and only if the caller holds that
   capability. `system.admin` wildcards every capability check, so a
   superuser always sees these.
3. **Neither declared** — **superuser-only**. This is the deliberate,
   conservative default for every tile that has not yet been migrated to a
   read capability.

```ts
canSeeTile(tile) {
  if (tile.public) return true;
  return this.can(tile.cap ?? SYSTEM_ADMIN);
}
```

Two further rules follow from this:

- **Navigation is not authorization.** The tile flag decides what is
  *listed*; the backend independently enforces the capability on every
  request. A visible tile whose endpoint refuses the caller is a bug, but a
  hidden tile is never the security boundary.
- **Opening a surface to a read capability is an explicit, per-tile
  migration** — declare the capability on the tile *and* enforce it in the
  handler. There is no bulk default that opens surfaces implicitly.

## Consequences

- The safe default holds for the ~72 tiles that predate read capabilities:
  they stay superuser-only until someone deliberately migrates them.
- Public help content is reachable by read-only operators on every viewport,
  and the desktop hard-coded link could be deleted, so desktop and mobile
  now derive navigation from one source.
- The three-tier rule is pinned by a contract test over the matrix
  (`{public}` → visible; `{}` → superuser-only; `{cap}` → capability-gated;
  superuser → all).
- Writing this down is itself the point: two separate implementation briefs
  proposed the "cap-less means public" inversion, and only an in-browser
  check caught it. The model needs to be stated somewhere durable rather
  than re-derived per sprint.

## References

- Read capabilities backend + the read-only operator role.
- Per-tile capability gating of the admin menu.
- The help-visibility defect and its correction, which established the
  `public` tier.
