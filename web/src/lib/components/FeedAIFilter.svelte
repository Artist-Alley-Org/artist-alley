<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The browse footer's "Hide AI-made work" toggle (#1251 slice 3,
  // ADR 0094 fourth amendment) — the second control in the bar's right
  // cluster, beside #1166's type filter and the sort direction.
  //
  // # What it actually does, because the obvious reading is wrong
  //
  // ON hides PURELY-AI posts and nothing else. A post mixing AI and
  // human contributors STAYS on the wall. That is the owner's ruling,
  // not a rounding of it: "AI could be used as part of an ideation phase
  // and the final project might be pure human made", so excluding a post
  // because ONE of its members carries an honest declaration would
  // punish exactly the declaration ADR 0094 is built to make cheap.
  //
  // The distinction is decided in SQL, on `posts.ai_pure` (migration
  // 00061), and this component computes nothing about it. It sends
  // `ai=not_pure` or it sends nothing.
  //
  // # A SWITCH, not a panel
  //
  // #1166's neighbour is a checkbox list behind an Apply because it
  // commits a SET and live-committing four ticks would reshuffle the
  // wall under the pointer between clicks. This one carries a single
  // boolean, so a panel would put two clicks and a dismissal rule in
  // front of one bit. It commits on click, like the sort toggle two
  // controls over.
  //
  // `aria-pressed` rather than a checkbox `role="switch"`: the control
  // IS a button in a toolbar, and pressed/unpressed is what a screen
  // reader should hear from a toolbar button that stays down.
  //
  // # THE BUTTON CARRIES THE STATE
  //
  // #1166's third rule, and it applies harder here: a wall with the
  // pure-AI work removed looks exactly like a wall that never had any,
  // because on most instances it IS that wall. So the ON state is the
  // accent fill the type filter uses for a real subset, and the label
  // stays visible from `sm` up — the sort control's own convention —
  // rather than leaving one unlabelled glyph to carry a policy-shaped
  // choice.
  //
  // # Anonymous callers get it too
  //
  // It is a FILTER, never a gate (ADR 0094 §4). Nothing is withheld from
  // anybody on this axis and no account is consulted, so there is no
  // signed-in check here — a signed-out visitor browsing a public-mode
  // install can decline to look at AI work exactly as a member can.
  //
  // # ⛔ Not a label
  //
  // This does not put an AI badge on any card. Labelling ("does this
  // post contain AI?") and filtering ("do I want to see purely-AI
  // work?") are different facts on different columns — ADR 0094's fifth
  // amendment — and the per-asset label is #1243.
  import Sparkles from '@lucide/svelte/icons/sparkles';
  import { t } from '$stores/lang.svelte';

  let {
    hidden,
    onchange,
  }: {
    /** Is "hide AI-made work" currently on? Owned by the caller (the
     *  browseView store), so this component holds no state of its own
     *  and cannot drift from what the feed request carries. */
    hidden: boolean;
    /** Commit the new value. */
    onchange: (next: boolean) => void;
  } = $props();
</script>

<button
  type="button"
  data-testid="ai-filter-toggle"
  onclick={() => onchange(!hidden)}
  title={t('browse.filter.ai.hint')}
  aria-label={t('browse.filter.ai.label')}
  aria-pressed={hidden}
  data-active={hidden ? 'true' : undefined}
  class="pointer-events-auto inline-flex h-11 min-w-11 items-center justify-center gap-1.5
         rounded-full border px-3 text-sm shadow-lg transition-colors
         focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring
         {hidden
    ? 'border-accent bg-accent text-accent-fg hover:opacity-90'
    : 'border-border bg-surface-elevated text-fg hover:bg-surface-hover'}"
>
  <Sparkles size={16} strokeWidth={2} aria-hidden="true" />
  <!-- Below `sm` the bar has the type filter, the sort control and this
       one competing for a 390px row, so the label collapses to the
       glyph. The accessible name is `aria-label` in BOTH states — set on
       the button rather than left to this span, so the control is named
       the same at 390px as at 1920px instead of going anonymous
       exactly where it has the least room to explain itself. Same
       breakpoint as the sort control's own label. -->
  <span class="hidden sm:inline">{t('browse.filter.ai.label')}</span>
</button>
