---
id: "0087"
title: A consumer of the shared chrome signal composes with it locally; mutating it is a global act
status: accepted
date: 2026-08-11
area: ux
phases: []
supersedes: []
related:
  - "0014"
tags:
  - frontend
  - ux
  - browse
excerpt: >-
  The auto-hiding navbar and the browse control bar read one shared `hidden`
  signal. A consumer that wants to stay visible for its own reason must AND a
  local term into its own derivation; calling the store's `reveal()` shows
  *everything*, which is right for a global interaction and wrong for a local
  one. Nothing recorded the distinction, and a brief written by the planning
  agent got it backwards.
---

# A consumer of the shared chrome signal composes with it locally; mutating it is a global act

## Context

`chromeScroll` (`web/src/lib/stores/chromeScroll.svelte.ts`) owns a single scroll listener on
the app shell's `<main>` and publishes one boolean, `hidden`, meaning *"the chrome should be
off-screen."* It is deliberately one listener and one signal: the top navbar and the browse
control bar hide and return **together**, so the page does not appear to come apart while
scrolling. The store is ref-counted precisely because more than one component attaches to it.

Three components consume it today:

| consumer | how |
|---|---|
| `+layout.svelte:149` (navbar) | reads `hidden` raw |
| `ViewControls.svelte:56` (browse bar) | `hidden && !expanded` |
| `AssetPlaylist.svelte:322` | calls `reveal()` |

The store also exposes `reveal()`, which sets `hidden = false` and lets the next scroll-down
re-hide normally. It was added for #554: picking a view collapsed the switcher, and if the user
had scrolled down while it was open the bar vanished the instant they chose.

**Nothing recorded which of these two mechanisms a new requirement should use**, and the names
actively mislead. `reveal()` is discoverable, well-commented and reads exactly like "show this
thing" — so it is the first thing anyone reaches for. #1020 (reveal the browse bar when the
pointer approaches the bottom of the window) is the case that forced the question, and the
planning agent's brief for it **specified `reveal()`**. That would have slid the *navbar* down
too, every time the user moved the mouse toward the bottom of the screen.

## Decision

**A consumer that wants to stay visible for its own reason ANDs a local term into its own
derivation. It does not touch the store.**

```js
// ViewControls.svelte — the browse bar's own visibility
const hidden = $derived(
  chromeScroll.hidden && !expanded && !focusInside && !(pointerNear && !hoverDismissed),
);
```

`expanded`, `focusInside` and `pointerNear` are all local state. The navbar's `chromeHidden`
is unaffected by any of them, which is the point.

**`reveal()` stays, and stays legitimate — for global intent.** The test is *whose* visibility
the triggering interaction is about:

- **Global** — the interaction concerns the chrome as a whole, and showing all of it is correct.
  Closing a maximized playlist (`AssetPlaylist.svelte:322`) returns the user to the page; the
  page's chrome should be there. Picking a view (`ViewControls.svelte:87`) ends an interaction
  the user started from the chrome. → `reveal()`.
- **Local** — only this component has a reason to be visible, and the reason is invisible to
  every other consumer. Hovering the bottom edge, holding focus inside the bar, keeping the
  switcher open mid-interaction. → a local term.

## Amendment 2026-08-13 (#1061) — the auto-hiding layer is a hazard for AUTOMATION, and the naive signals lie

This ADR settles *who decides* the chrome is visible. It says nothing about **interacting** with
it, and a UI test that clicks something inside the layer hit two traps that cost a full CI failure
and two rejected candidate fixes.

1. ⭐ **Scroll events COALESCE.** A test (or any code) that issues two `scrollTo` calls in quick
   succession may produce **one** scroll event carrying only the final offset. The store sees a
   single downward move past its threshold, hides the chrome, and **never sees the upward move
   that would bring it back.** The chrome then stays hidden indefinitely — the observed CI failure
   was `element is outside of the viewport`, retried 55 times to timeout, not a transient.
   Each scroll must wait for **its own** scroll event before the next is issued.

2. ⛔ **The target element's own bounding rectangle is the WRONG readiness signal**, and it fails
   in two different directions: it lies *before* the scroll event has dispatched (the store has not
   reacted yet) and again *during* the 200ms `transition-transform` (the layer is mid-slide). Two
   candidate fixes built on it failed 8-in-10 and 9-in-10. **Read the layer's own rendered state
   instead.**

3. **`reveal()` is not the tool for this**, and the test in the Decision above is what says so:
   nothing in an automated scroll is *a user acting on the chrome*. Reaching for `reveal()` to make
   a test pass would assert global user intent that did not occur, which is exactly the misuse this
   ADR was written to prevent.

**The generalisable point:** an auto-hiding layer driven by a *derived, event-timed* signal is not
addressable the way a static element is. Anything that must interact with it — a test, a
programmatic focus, a scroll-into-view — has to wait on the **signal**, not on the element.

⚠️ Note that Playwright's own click implementation scrolls an element into view first, so
**clicking an element inside a scroll-driven layer can perturb the very state that decides its
visibility.** That interaction is the reason this deserves a place in the ADR rather than a comment
in one spec.

Stated as a rule: **`reveal()` answers "the user just did something that should bring the chrome
back." A local term answers "this component in particular should not be hidden right now."**
If the answer names one component, it is not a store call.

## Consequences

- The browse bar can be revealed, focused, hovered and expanded without the navbar moving.
- `chromeScroll` keeps a small surface — one boolean and one verb — rather than growing a
  per-consumer pin registry. Consumers that need more nuance express it in their own scope,
  where the state already lives.
- **The invariant needs an explicit test, because it looks redundant.** `ViewControls.test.ts`
  asserts `chromeScroll.hidden` is *still true* after a local reveal. Reading that line, the
  natural reaction is "of course it is — we didn't touch it," which is exactly why it must be
  written down: it is the only thing standing between this design and the `reveal()` shortcut.
  It was mutation-tested when written (rewriting the component to call `reveal()` fails three
  tests), so it is known to bite rather than assumed to.
- A fourth consumer inherits the question. The table above should grow with it.

⚠️ **Related trap, recorded because it cost a browser run.** Keyboard parity on a locally-revealed
element must use **`:focus-visible`**, not `:focus-within`. A mouse click focuses the control it
lands on, so `focus-within` keeps the element pinned open after any click until focus moves
elsewhere — a regression on the mouse path introduced by a line intended for keyboard users. The
#1020 brief specified `focus-within`; the coding agent's own browser run caught it. Unit tests do
not: the defect lives in what a real click does to the focus tree.
