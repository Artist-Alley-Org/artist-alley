---
id: "0089"
title: An ordinary read capability is not administrative standing
status: accepted
date: 2026-08-15
area: security
phases: []
supersedes: []
related:
  - "0010"
  - "0063"
excerpt: >-
  The admin shell opens on a narrower set than "every capability a live tile names". A capability
  ordinary signed-in users hold — because it gates a public surface — opens its tile but not the
  door. The gate narrows; the role is never touched. Deciding this at the role would have broken
  public browsing to fix a UI guard.
---

# 0089 — An ordinary read capability is not administrative standing

- **Deciders:** planning agent, under the operator's standing "only the most robust fix" instruction
- **Context:** #962, PR #1089. Found during #958 / PR #960; pre-existing.
- **Related:** ADR 0010 (permissions, teams, workflow), ADR 0063 (one expression of a rule)

## Context

`/admin` refused nobody. The admin shell's "You don't have permission to view this page." branch
was gated on `!canSeeAdmin`, and `canSeeAdmin` asked whether the caller held **any capability named
by any live admin tile**. Two of those fourteen codes were `roles.read` and `teams.read`, and the
seeded `Base` role holds both — so the branch was unreachable for every authenticated account, and
an ordinary user opening `/admin` got a two-tile grid instead of a refusal.

**Nothing was exposed.** Every handler behind those tiles enforces its own capability server-side;
the account saw exactly the two surfaces its capabilities genuinely permit. The defect was shape,
not access: a guard that reads as live and cannot execute. This codebase has now been bitten by
that class four times, and here it did concrete damage — it made an acceptance criterion of a prior
sprint (*"a plain `Base` user still sees the permission panel"*) unsatisfiable, and a less careful
sprint would have edited the test to match the bug.

## Decision

**Opening the admin shell requires administrative standing, which is narrower than "names a live
tile's capability".** A capability that ordinary signed-in users hold opens its tile but does not
open the door.

Concretely: `AdminTile` carries `grantsAdminEntry`, defaulting to true. The entry set is **derived
from the tile table**, never hand-listed. `roles.read` and `teams.read` are marked false.

### 1. The test for which side a capability falls on

Not "does an admin tile use it" — that is circular, and it is exactly the reasoning that produced
the bug. The question is:

> **Does an ordinary signed-in user hold this capability as a matter of course, for a reason that
> has nothing to do with administration?**

`teams.read` answers yes, unambiguously, and the evidence is in the tree rather than in an opinion:
it gates `routes/teams`, `ExploreMenu`, `TeamsRail`, `TeamFollowButton`, `MobileNavDrawer` and
`stores/teamFollows` — six public surfaces, every one of which documents the capability as **the
signed-in-versus-guest line**. A capability that separates a member from a visitor cannot also
mean "this person administers the instance". `roles.read`, whose own description is *"List
available roles and their capabilities"*, is the same kind of thing.

### 2. ⭐ The fix belongs in the gate. Deciding it at the role would have been a real regression

The tempting fix is to remove `roles.read`/`teams.read` from `Base`, which makes the gate correct
without touching the gate. **It would have broken public browsing** — all six surfaces above go
dark for every ordinary account — to repair a UI guard.

The general rule, worth carrying to the next instance:

> **When a gate misclassifies, correct the gate. Changing the input to make a wrong rule produce a
> right answer moves the defect somewhere it is harder to see.**

A capability grant is a statement about what someone may do. It is not a knob for tuning an
unrelated conditional, and the blast radius of editing it is every surface that reads it — here,
six of them, none named in the issue.

### 3. One list, not two

The obvious implementation adds a second constant beside `ADMIN_TILE_CAPS`. It was rejected on
inspection of the call sites: per-tile visibility is `canSeeTile(tile)`, which reads `tile.cap`
**directly** and never consults a flattened list. `ADMIN_TILE_CAPS` had exactly **one** production
consumer, the entry gate. Keeping both would have left an exported symbol with zero callers — the
precise defect deleted in the same PR (#947), and a standing invitation to wire it somewhere it
does not belong.

So the constant is **renamed and narrowed**, not duplicated, and both it and the tile predicate
derive from the same table. Per ADR 0063 this is one expression of one rule; a hand-copied second
array would have been a third place to drift.

### 4. The refusal is now reachable, and that is the acceptance criterion

An unreachable guard is not a safe guard, it is a lie about the system. The criterion is not "the
gate is correct" but **"a real account reaches the refusing branch"**, asserted by a test that gets
there. Verified on a live instance with an account holding exactly `roles.read` + `teams.read` —
the precise shape that used to pass — at 1080px and 390px.

## Consequences

- A `Base` account is refused at `/admin`, **and at `/admin/roles` and `/admin/teams`**, because
  the layout gate wraps its children. Those two tiles are therefore unreachable for an account
  whose only qualifying capabilities are ordinary read codes. Accepted: nothing in the non-admin UI
  links to either, and `/teams` is the surface `teams.read` exists to serve.
- ⚠️ **A future tile whose capability `Base` holds must set `grantsAdminEntry: false`, or it
  silently re-opens the shell to everyone.** The flag defaults to *true* deliberately — almost
  every admin capability is administrative, and a default of false would fail closed in a way
  nobody notices until an admin cannot get in. The cost is that this specific mistake is
  re-committable; the entry/tile test in `sections.test.ts` is what catches it.
- The verification pattern generalises: **for a gate that classifies principals, the test fixture
  is a principal holding exactly the boundary set**, not a convenient role. A plain `Base` account
  would have proved less — it holds other capabilities, and the reason it is refused would have
  been ambiguous.
- This does not touch ADR 0010's capability model. Nothing about what a capability *permits*
  changed; only which capabilities imply *standing to enter an operator surface*.
