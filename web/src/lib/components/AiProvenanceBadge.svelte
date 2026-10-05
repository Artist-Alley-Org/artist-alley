<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // THE AI DECLARATION, DRAWN (#1243, ADR 0094 amendment 2026-08-25).
  //
  // One component for both halves of that amendment, because they are
  // one rule at two sizes and two components would be two places for the
  // `none`/`null` prohibition to be got wrong:
  //
  //   `label`  — the VIEWER's real label. Icon + the word, in the
  //              menubar, on every route the viewer serves.
  //   `overlay`/`inline` — the CARD's small icon. Icon only, no word,
  //              which is the cap the amendment puts on a tile.
  //
  // ⛔ NOTHING RENDERS FOR `none` OR FOR `null`, and that includes the
  // tooltip and the accessible name. The gate is `isMarkedAi`, imported
  // rather than re-spelled here — see $lib/aiProvenance for why a client
  // that renders absence as "no AI" is lying for the artist.
  //
  // ⭐ `assisted` and `generated` are DIFFERENT GLYPHS, not one glyph in
  // two colours. Colour alone is not a distinction (1.4.1) and it is not
  // one for a sighted reader either at 15px. Sparkles reads as "a tool
  // helped"; Bot reads as "a machine made it", which is the actual claim
  // the two states separate. The accessible name carries the same
  // distinction in words, from the SAME i18n keys the upload control
  // writes with — so the word an artist chose when declaring is the word
  // a reader hears.
  //
  // Icons are named lucide imports via their own deep paths (ADR 0096 +
  // kindIcon's note on the barrel). No pasted `<svg>`; #1288 is
  // sweeping the 61 that already exist and this must not become 62.

  import Bot from '@lucide/svelte/icons/bot';
  import Sparkles from '@lucide/svelte/icons/sparkles';
  import { t } from '$stores/lang.svelte';
  import { cardTooltip } from '$stores/cardTooltip.svelte';
  import { aiLabelKey, isMarkedAi } from '$lib/aiProvenance';

  interface Props {
    /** The declaration. `none`, `null` and `undefined` all draw nothing.
     *  Typed loosely on purpose: it arrives from the hand-written API
     *  mirrors scattered through the client, and a narrow type here
     *  would put a cast at every call site. */
    value: string | null | undefined;
    /** Which surface this sits on.
     *
     *  `overlay` — over artwork, so the pill has to survive an unknown
     *  photograph underneath: solid dark plate, purple glyph.
     *  `inline`  — in a card's chrome band, on the card's own surface,
     *  where that plate would read as a sticker.
     *  `label`   — the viewer's labelled pill, the only variant that
     *  prints the word. */
    variant?: 'overlay' | 'inline' | 'label';
    /** Identifies the CARD, for the shared tooltip store — same
     *  contract as CardKindBadge's, so two badges on one tile cannot
     *  fight over one timer. Unused by `label`, which shows its word. */
    tooltipKey?: string;
    /** Extra positioning classes. The badge owns its look; the caller
     *  owns where it sits, which differs per density. */
    class?: string;
  }

  let { value, variant = 'overlay', tooltipKey = '', class: klass = '' }: Props = $props();

  const marked = $derived(isMarkedAi(value));
  const Glyph = $derived(value === 'generated' ? Bot : Sparkles);
  // Non-null whenever `marked` — the key function and the gate read the
  // same predicate, so there is no state where one answers and the other
  // does not.
  const label = $derived(marked ? t(aiLabelKey(value)!) : '');

  const tipKey = $derived(`ai:${tooltipKey || value}`);
  const tip = $derived({ title: label, meta: [], placement: 'anchored' as const });
</script>

{#if marked}
  {#if variant === 'label'}
    <!-- THE VIEWER'S LABEL. A <span>, not a tooltip trigger: the word is
         on the page, so there is nothing a hover could add and nothing a
         keyboard reader would have to go looking for.

         The word is `sr-only` on a phone and visible from `sm` up. It is
         never REMOVED — a marker whose only distinguishing feature
         disappears below 640px would leave the two states apart for a
         sighted desktop reader and together for everyone else. The glyph
         still differs at every width; the word is what a screen reader
         gets at all of them. -->
    <span
      class="inline-flex shrink-0 items-center gap-1.5 rounded-full bg-ai-container px-2 py-0.5
             text-[11px] font-medium text-on-ai-container {klass}"
      data-testid="viewer-ai-provenance"
      data-ai={value}
    >
      <Glyph size={14} strokeWidth={2} aria-hidden="true" />
      <span class="sr-only sm:not-sr-only whitespace-nowrap">{label}</span>
    </span>
  {:else}
    <!-- THE CARD'S ICON. A <button type="button"> with no click handler,
         for the reason CardKindBadge is one: a visual tooltip on FOCUS
         needs a focusable element, and a `<span role="img" tabindex=0>`
         is a tab stop a keyboard reader cannot act on (and svelte's a11y
         pass rejects it). The cost is one extra tab stop, on the small
         subset of cards that carry a declaration at all — and this icon
         is the only thing on the tile that states it, so a reader who
         could not reach it had no way to learn it.

         `data-marquee-passthrough` opts back out of the marquee's
         "a press on a control is not a drag" bail-out, exactly as the
         kind badge does (#1127) — without it a strip of every marked
         card would be dead to the selection band. -->
    <button
      type="button"
      class="inline-flex shrink-0 cursor-default items-center rounded-full p-1
             focus-visible:ring-2 focus-visible:ring-ring focus:outline-none
             {variant === 'inline'
        ? 'text-ai'
        : 'bg-black/60 text-ai backdrop-blur-sm'} {klass}"
      data-testid="card-ai-provenance"
      data-ai={value}
      data-marquee-passthrough
      aria-label={label}
      onmouseenter={(e) => cardTooltip.enter(tipKey, tip, e)}
      onmousemove={(e) => cardTooltip.move(tipKey, e)}
      onmouseleave={() => cardTooltip.leave(tipKey)}
      onfocus={(e) => cardTooltip.showFor(tipKey, tip, e.currentTarget as HTMLElement)}
      onblur={() => cardTooltip.leave(tipKey)}
    >
      <Glyph size={15} strokeWidth={2} aria-hidden="true" />
    </button>
  {/if}
{/if}
