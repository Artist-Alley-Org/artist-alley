---
id: "0007"
title: Federation — thinking ahead
status: superseded
date: 2026-05-24
area: infrastructure
phases: []
supersedes: []
superseded_by: "0043"
related:
  - "0043"
tags:
  - infrastructure
  - history
  - federation
excerpt: >-
  Historical. Early "we will federate eventually, leave room for it in the schema" placeholder. Superseded by ADR 0043, which commits to the concrete walled-garden ActivityPub-shaped protocol.
---
> **Superseded — historical record.** This ADR was a placeholder
> committing to forward-compatibility for federation without
> specifying the protocol. The concrete federation protocol — an
> artist-alley walled-garden ActivityPub-shaped design with opt-in
> per-object sharing — is now defined in
> [ADR 0043](/adr/0043-federation-walled-garden-protocol/).
> Retained as historical record.


## Context

artist-alley targets game studios. Studios above a certain size have
multiple game teams (Blizzard alone has WoW, Overwatch, Diablo,
Hearthstone, Hearthstone, etc.) and an archives function that sits
above the per-game work. Two real-world topologies came up while
discussing this future need:

1. **Peer federation.** Each game team runs its own artist-alley.
   The servers are sovereign — they own their data and permissions —
   but a user with the right capabilities can browse another team's
   server. Mastodon/ActivityPub-style.
2. **Hub-and-spoke with replication.** The archives team operates a
   central artist-alley. Each game team runs a child install that
   replicates assets up. The archive can search down into the children;
   children stay otherwise independent.

Both are valid and serve different organisational shapes. We don't
have to choose now — but a handful of design decisions made now make
*either* topology viable later, while doing nothing locks us into a
single-server world.

## Decision

We are **not** implementing federation in any Phase 1.x or Phase 2.x
work. The cost is multi-year and the value only manifests when a
stakeholder is ready to run multiple installs.

We **are** adopting a handful of cheap design choices now that keep
both target topologies open. Retrofitting them later is painful;
applying them now is approximately free.

### Cheap design choices we adopt now

| Choice | Cost | Federation payoff |
|---|---|---|
| UUIDs for all artist-alley-owned PKs | $0 (already in place) | Global uniqueness across servers; no INT collisions |
| `origin_server_id UUID NULL` column on federated tables | one column per table | Knowing where a row originated; default NULL means "this server" |
| Stable URI scheme for cross-references: `aa://<server-id>/resource/<uuid>` instead of bare IDs in comments, annotations, etc. | modest discipline | Cross-server references survive any future replication |
| Sign API tokens with a server-identifying key (HMAC or asymmetric) so peers can verify origin | modest | Federated identity verification path stays open |
| Namespace capability codes so they can carry server scope later (e.g. `resource.read@blizz-wow`) without renaming existing codes | $0 (just naming discipline) | Per-site permissions in a federated world |

These get enforced starting **Phase 1.3** (the capability/role
authorization redesign), where most of them naturally land. They will
be documented in the relevant per-feature ADR or code comments.

### Things we explicitly defer

- The federation protocol itself (sync semantics, search-across-sites,
  request/response between peers, conflict resolution).
- Cross-site authentication and identity (OIDC federation? Signed
  delegation tokens? Mutual TLS between peers? All have tradeoffs).
- Replication topology choice (peer / hub-spoke / hybrid).
- Storage layer implications (do peers also share blob storage or just
  metadata?).
- The cross-server browsing UI in the SvelteKit frontend.

These will be designed properly when there is a real consumer ready
to use them.

### Trigger to revisit

We start designing the real implementation when one of these becomes
true:

- A stakeholder (Blizzard or otherwise) commits to running ≥2
  artist-alley installs that need to be connected.
- A feature request explicitly demands cross-site browsing or
  replication.
- The artist-alley user base reaches a scale where a single install
  is becoming a bottleneck.

Until then, we keep the design hooks above honest and focus on
single-server quality of life.

## Consequences

**Positive**
- We have option value on federation without paying the implementation
  cost.
- New tables get UUIDs + `origin_server_id` from day one; capability
  codes are namespaced from day one. Adding federation later won't
  require renaming or schema migrations of millions of rows.
- The ADR is in version control, so we don't lose the thread.

**Negative**
- We pay a small ongoing tax on table design (one extra column, slightly
  longer capability names than strictly necessary).
- Future contributors will see UUIDs and `origin_server_id` columns
  with no immediate purpose. We pay for that confusion in comments.

**Mitigations**
- Document the federation prep in code comments and feature-package
  READMEs, pointing back to this ADR.
- Don't over-prepare. If a piece of prep isn't on the table above,
  don't add it speculatively — wait for the real implementation to
  shape the decision.

## Two topologies — what we'd build for each (sketch only, no commitment)

### Peer federation
- Servers identify themselves with a public key; signed handshakes
  between peers.
- Each server's `/.well-known/artist-alley` exposes capabilities and
  cap-discovery.
- Users present a federated identity (most likely OIDC) to the peer
  they want to access. The peer enforces its own caps but trusts the
  identity.
- Search is per-server; aggregating happens client-side or via a
  thin "federated search" endpoint that queries known peers.
- Asset access is a redirect or signed-URL flow to the asset's home
  server — copies are explicit, not implicit.

### Hub-and-spoke with replication
- Archive ("hub") and games ("spokes") have a shared bootstrap key.
- Spokes push asset metadata + content to the hub on a schedule or
  on commit. The hub stores everything.
- Search down: hub-side searches its full index; can optionally
  consult spokes for fresher results.
- Search up: spoke users browse their own assets normally; the hub is
  invisible to them unless they need to fetch an archived asset that
  the spoke has pruned.
- Permissions are per-server: spokes own their access controls, the
  hub has its own. Identity is shared (single sign-on across).

Both share the cheap-prep design choices above. We can implement
either or both without breaking compatibility with single-server
installs.

## Notes

This ADR is design-level. It does not introduce schema changes,
endpoints, or code. The cheap-prep items will appear in subsequent
phase ADRs as they get implemented — they will reference this ADR
for context.
