---
id: "0103"
title: Derived access copies move between instances only under their own grant
status: proposed
date: 2026-11-26
area: architecture
phases:
  - "1.22"
supersedes: []
related:
  - "0043"
  - "0102"
tags:
  - federation
  - access-copies
  - grants
excerpt: >-
  A sealed version's access copy reaches a peer only under the opt-in
  profile derived-transfer-1, through a version-bound signed request,
  and only under an explicit access-copy grant that is independent of
  the original-file grant and of every scope. A hash or a listing is
  never permission, and original transfer is unchanged.
---

## Status (2026-11-26)

Proposed (#1597). The decision below is unchanged; this section
records how far the reference implementation has followed it.
- **Normative text:** federation spec v1, Addition A4: §7.5.7 *Access
  copies*, §8.5.1, §12.4 and §16.7, with Implementation status S4.
- **Implemented, not advertised:** every part of the decision, in the
  order it sets out.
  - The origin's grant: `allow_access_copies` on a share, off unless
    set through the admin share API (migration 00102), and its decision
    and signed version-bound route (§8.5.1).
  - The share member `aa:accessTransfer` and the receiver's record of
    it on its inbound grant (migration 00103).
  - The receiver's additive phase: a bounded background worker, separate
    from original import and never delaying it, that fetches each
    access copy of a complete version under a transfer lease of its
    own, checks the exact request at the origin again and records a
    per-file outcome (migrations 00104 and 00105).
  - Two-instance gates on production servers, including a peer without
    `derived-transfer-1`.
- **Not advertised:** `derived-transfer-1` stays out of
  `peer.KnownCapabilities`, so no production peer has it in effect and
  no access copy moves between production instances. The two-instance
  gates advertise it in-process for their own handshake only.
  Advertisement remains a separate final gate.
- **Already landed:** the bytes this decision relies on.
  - #1618 records a byte-derived, generation-bound SHA-256 for every
    stored variant and checks it in the sweep.
  - #1622 and #1623 promote verified access copies into sealed versions
    (ADR 0102).
  - #1628 lists them as role `access` in an announcement.

## Context

An `archival-1` announcement can now list a version's access copies
(§17.2.2). A receiver still keeps them as claims that were not
transferred, because the contract defines no way to move them. The
origin's §8.5 endpoint serves only the original and answers `access`
and `preview` with `class_not_available`.

Three existing rules shape any answer:
- **§7.5.7** keeps byte classes separate from share scopes, and gives
  the original its own grant.
- **§17.2.2** makes a verified SHA-256 the condition for listing a file,
  never a permission to send it.
- **§16** freezes every published profile, so `original-transfer-1` and
  `aa:originalTransfer` cannot change meaning.

The bytes are ready on the sending side. A sealed access copy is an
immutable object of its own, verified at its SHA-256 and size, and bound
to the generation of its version's original (ADR 0102, migration
00085). No mutable rendition key needs to be exposed. What is missing is
a permission, a request, and the receiving rules.

## Decision

1. **A separate profile.** `derived-transfer-1` is a new token, in
   effect only together with `archival-1`, `original-transfer-1` and
   `share-lifecycle-1`. The profile itself never filters or rewrites an
   announcement's file set, roles or version identity; every other
   per-peer rule still applies. A peer without it keeps access files as
   claims that were not transferred.
2. **A separate grant.** An origin share carries an explicit
   access-copy grant, default off, only on asset and collection shares,
   independent of scope and of the original-file grant. Neither grant
   implies the other. It reaches the receiver as `aa:accessTransfer:
   true` on `aa:Share`, under every `share-lifecycle-1` rule.
3. **A version-bound request.**
   `GET|HEAD /federation/assets/{asset_id}/versions/{n}/files/access/{sha256}?size=`,
   signed like §8.5, so the signature binds asset, version, class, hash
   and size. The original request is unchanged and still refuses
   `access` and `preview`.
4. **The origin decides from its own records**, on every request:
   - the peer and profile;
   - the actor on that peer;
   - a live local asset;
   - a sealed version listing exactly that access file, whose immutable
     stored object has a recorded verified SHA-256 and size equal to the
     file's, with its recorded fixity status still verified when the
     request is authorised (the receiver still verifies the bytes);
   - its own record that it announced that file of that version to that
     peer, which it writes before sending, never a delivery
     acknowledgement;
   - a public or team tier;
   - a live share carrying the access-copy grant from a grantor with
     current authority.

   Every refusal that concerns the asset is the same `403
   not_available`.
5. **The receiver's phase is additive.** Originals complete on their
   own. Each access copy is then fetched and verified from its own bytes
   under both authorities, with its own outcome, retry and failure.
   Reusing bytes already held, even as some other original, is
   re-authorised for the access class. A revoke before recording records
   nothing, a late grant may start a transfer, and a replaced version
   keeps copies already verified.
6. **Only wire role `access`.** A sender folds sealed copies of local
   preview and access renditions into `access`. There is no preview
   transfer, and `preservation` stays a listed claim: no local producer
   of a preservation copy exists, and none is introduced here.

## Consequences

### Positive

- **Least surprise for existing grants:** no existing share, scope or
  original-file grant gains a new meaning.
- **One immutable file per request:** a request names exactly one
  sealed, verified file of one version, and the origin serves only what
  it said it would send to that peer.
- **Older peers and original ingest are untouched.**

### Negative

- **More for a grantor to set:** a second grant per share. A grantor who
  wants both originals and access copies sets both.
- **Four profiles to negotiate** before any access copy moves. In the
  reference implementation `share-lifecycle-1` is advertised, but the
  prerequisites `archival-1` and `original-transfer-1` are not yet, and
  `derived-transfer-1` itself stays reserved and unadvertised.
- **More receiving state:** each access file needs its own outcome, and
  the import pipeline gains a second phase.

## Alternatives considered

- **Treat `view` scope as permission to receive access copies.** It is
  smaller, but it would widen every existing view share retroactively,
  including shares made before any bytes could move. §7.5.7 keeps
  classes apart from scopes. Rejected.
- **Let the original-file grant cover access copies.** It would change
  the meaning of a frozen member (`aa:originalTransfer`) and tie a
  display copy to a grant made for the original. Rejected.
- **Fetch by hash through the original request's class segment.** A hash
  identifies bytes, not permission, and the same bytes can belong to
  several assets and versions. Rejected; the request names the version.
- **A new version of `original-transfer-1` or `archival-1`.** It would
  force peers that want originals only to take access copies, or the
  other way round. Rejected in favour of an independent token.

## References

- Federation spec v1: §7.5.7, §8.5, §8.5.1, §12.4, §16.6, §16.7,
  §17.2.2 (`docs/spec/federation/v1.md`).
- ADR 0043 (the federation protocol) and ADR 0102 (asset versions and
  access-copy promotion).
- #1597, building on #1573, #1571, #1574, #1575, #1596, #1618, #1622,
  #1623 and #1628.
