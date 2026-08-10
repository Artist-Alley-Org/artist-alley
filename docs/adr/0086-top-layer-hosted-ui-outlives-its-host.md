---
id: "0086"
title: UI in the browser top layer must outlive its host, by construction
status: accepted
date: 2026-08-10
area: frontend
phases: []
supersedes: []
related:
  - "0067"
tags:
  - frontend
  - overlays
  - portal
  - viewer
excerpt: >
  Three separate invisible-UI defects came from one mechanism — the browser top layer. A node
  parented into a modal dialog is correct while that dialog lives and gone the instant it does
  not, and a dialog can end in two ways of which only one is an event. The invariant: a
  portalled node's survival is decided by the portal, never by the ordering at its call site,
  and the test for any such element asserts it is in the DOCUMENT rather than in a store.
---

## Context

A `<dialog>` opened with `showModal()` is promoted to the browser's **top layer**, which renders
above everything regardless of `z-index` or stacking context. Our viewer is such a dialog. Any
transient UI raised while it is open — a confirmation, a toast — must be parented *into* that
dialog or it is painted underneath it: present in the DOM, correct by every assertion, and
invisible on screen.

`$lib/portal` exists for that. It resolves the current top-layer host and appends the node there.

**Three defects in three sprints have come from this one mechanism**, and the pattern is what
justifies an ADR rather than a third code comment:

1. **#881/#899** — a modal declared inside a `container-type` box was trapped by that box's
   containment; `portal` was introduced to escape it.
2. **#985** — a toast declared in the root layout resolved `closest('dialog[open]')` against its
   *declaration site*, found no dialog ancestor, and landed on the body **beneath** the open
   viewer. Fixed by asking the document for `dialog:modal` instead (`scope: 'document'`).
3. **#991** — a toast correctly parented into the viewer's dialog was carried out of the page when
   that dialog was **removed** by a navigation. The node stayed parented, stayed in the store, and
   was on no screen. It shipped twice, because the tests asserted the toast was in the *store*.

Each was reasoned about correctly at its call site and still wrong. That is the signal: the call
site is the wrong place for this reasoning.

## Decision

**1. A dialog ends in two ways and only one of them is an event.** `close()` fires `close`.
Removal from the document — which is what destroying the declaring component does — fires
**nothing**. A portal that re-homes on `close` alone handles half the cases. `$lib/portal`
therefore also watches for detachment (a `MutationObserver` on `isConnected`, attached only for
rehoming nodes that actually live inside a dialog) and treats "my host left the document" as the
sibling of "my host closed".

**2. Survival is the portal's responsibility, never the call site's.** #991's call site did
everything right: it awaited `onClose()` specifically so no dialog would remain to adopt the
toast. It failed because the standalone close policy is `history.back()`, and **a history entry
cannot be awaited** — the `await` resolved immediately with the dialog still open. A caller cannot
promise its host is gone when it has no way to wait for the navigation that removes it, so it must
not be asked to. Ordering fixes at call sites are not a remedy for this class.

**3. The test asserts presence in the DOCUMENT.** A store-level assertion passes whether or not
anything reached a screen, which is precisely how #991 survived two rounds of browser
verification. Any element in this class needs a test that queries the rendered document. Where the
test environment does not implement the relevant platform behaviour — happy-dom implements
`showModal()` but not the `:modal` pseudo-class that `portal` selects on — the shim must be
explicit **and** the test must assert the node really landed in the dialog before exercising the
failure, or it passes vacuously.

**4. Modals opt out.** A modal *should* die with the surface that raised it, so `Modal` passes
`rehome: false` and takes no observer. Only rehoming nodes — toasts — pay any cost.

## Consequences

- Transient acknowledgements survive the surface that raised them, including across a navigation
  that destroys it. The delete/undo arc (#981, #985, #987, #991) depends on this.
- One `MutationObserver` exists per live rehoming node inside a dialog — in practice a handful per
  session, seconds each, with a callback that is one `isConnected` read. Accepted deliberately:
  there is no event for detachment, and the alternative is silent invisible failure.
- ⚠️ **Measuring this class needs care.** A transient element has a TTL (toasts: 9s), so a
  click-then-check across a tool round-trip cannot distinguish "never rendered" from "expired
  before I looked" — both produce a count of zero. All three rows of #991's original reproduction
  were false negatives for exactly that reason; the real path was found only after driving and
  sampling inside a single page context. Verify transient UI with an in-page sampling loop.
- This ADR states the invariant and the incident history. The mechanism stays documented in
  `web/src/lib/portal.ts`, which is the implementation of record — deliberately not restated here,
  so there is one place for it to change.

## References

- ADR 0067 — the standalone asset route (its 2026-08-09 amendment established the sibling rule:
  an affordance belongs to the shell, not the route, for the same top-layer reason).
- #881, #899 — containment escape, the original reason `portal` exists.
- #985 — declaration-site resolution vs the document's top layer.
- #991 / PR #992 — removal fires no event; the fix and its DOM-level test.
