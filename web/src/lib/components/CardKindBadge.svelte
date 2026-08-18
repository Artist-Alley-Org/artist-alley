<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // THE kind indicator on a card, in every density (#1047, extracted
  // from #1111's grid overlay).
  //
  // #1111 established the notation — a lucide glyph for a single asset,
  // the count then the Shapes glyph for a set, never a word — and wrote
  // it inline in PostCard's grid overlay. The density pass needs the
  // SAME notation in three more places (thumbnail's persistent chrome,
  // AssetCard's tile in every mode, and every non-grid PostCard density
  // that until now drew CardThumb's `video` / `3D` TEXT chip). Three
  // copies of a pill is how the two notations diverged in the first
  // place, which is what PR #1124 flagged, so it is one component.
  //
  // WHAT THIS REPLACED, and why the replacement is not like-for-like:
  // CardThumb's chip covered exactly two of the thirteen ViewKinds
  // (`video`, `3D`) with hardcoded English strings, so a PDF, an
  // audiobook and a sprite sheet were all indistinguishable at rest. The
  // icon map is exhaustive over ViewKind by type, so every kind is
  // labelled and a new kind cannot ship unlabelled.
  //
  // The accessible name is the i18n kind label — the icon is the visual
  // notation, not the semantics, so a screen reader still hears
  // "Sprite sheet" and never "no text".

  import type { ViewKind } from './viewers/controller';
  import { iconForKind, MultiAssetIcon } from './kindIcon';
  import { t } from '$stores/lang.svelte';
  import { cardTooltip } from '$stores/cardTooltip.svelte';

  interface Props {
    /** The asset's kind, resolved through `kindForAsset` — never
     *  inferred from the extension alone, which cannot tell a sprite
     *  atlas from an ordinary PNG. */
    kind: ViewKind;
    /** How many assets the card stands for. > 1 states the SET instead
     *  of any one member's kind: picking one member's icon for a mixed
     *  bundle says something untrue about the others (#1111). */
    count?: number;
    /** Every asset this badge stands for is of `kind` — so the count
     *  keeps the SET's glyph company instead of replacing it (#1203).
     *
     *  #1111's rule was never "a set has no kind", it was that one
     *  member's icon cannot speak for the rest. When there is only one
     *  answer among them that objection is gone, and Shapes is then the
     *  vaguer of two true statements: the owner's "if they are all the
     *  same asset, show the icon for that asset type."
     *
     *  Meaningless below `count` 2, where the single glyph already is
     *  the kind. The CALLER decides — this component sees a count and a
     *  kind, never the membership, and cannot tell a genuinely uniform
     *  pack from a truncated payload that only looks like one. */
    uniform?: boolean;
    /** Extra positioning classes from the caller. The badge owns its own
     *  look — pill, scrim, backdrop blur — and the caller owns where it
     *  sits, because that differs per density. */
    class?: string;
    /** Which surface the badge sits on (#1136).
     *
     *  `overlay` — over artwork. The black pill and the backdrop blur
     *  are camouflage: they have to survive an unknown photograph
     *  underneath, at any brightness.
     *
     *  `inline` — in a chrome band, on the card's own surface, where
     *  that camouflage reads as a sticker. Same glyph, same notation,
     *  same accessible name; theme colours instead of the scrim. */
    variant?: 'overlay' | 'inline';
    /** Identifies the CARD this badge sits on, for the tooltip store
     *  (#1144). The store keys its timers so a stale one from a tile the
     *  pointer already left cannot commit; two badges sharing a key
     *  would make a swap between them look like a flicker. Defaults to
     *  the kind, which is right for the one-badge-per-page cases and
     *  wrong nowhere, because a wall always passes the row id. */
    tooltipKey?: string;
    /** A better sentence than this component can compose on its own.
     *
     *  It sets the accessible name AND the tooltip — one string, because
     *  a badge whose tooltip and screen-reader name disagree is two
     *  different answers to the same question depending on how you
     *  read the page.
     *
     *  The case that needs it is a post's PACK badge: the host knows
     *  what format the pack is ("4 glb assets in this post"), and this
     *  component knows only the count. Everything that does not pass it
     *  keeps the default — the count for a set, the kind for a single
     *  item, which is what an asset tile outside any post wants. */
    label?: string;
  }

  let {
    kind,
    count = 1,
    uniform = false,
    class: klass = '',
    variant = 'overlay',
    tooltipKey = '',
    label: labelOverride,
  }: Props = $props();

  const multi = $derived(count > 1);

  /** True when the glyph stands for the SET and not for a kind — a
   *  multi-asset badge whose members do not share one, which is the only
   *  case Shapes was ever for. Derived once so the glyph and the
   *  attribute that reports it cannot disagree. */
  const statesTheSet = $derived(multi && !uniform);
  const Glyph = $derived(statesTheSet ? MultiAssetIcon : iconForKind(kind));
  const label = $derived(
    labelOverride ||
      (multi
        ? t('card.multi.badge_label', { count: String(count) })
        : t(`card.fallback.kind.${kind}`)),
  );

  // #1144 — the icon gets a tooltip NAMING the type, on hover and on
  // focus, everywhere this component renders.
  //
  // # Why the store and not the `title` attribute it replaces
  //
  // `title` was doing the job badly in three ways this component cares
  // about: it is ~1s late, it is unstyled OS chrome that reads as a
  // browser artefact over a card's own chrome, and it never appears on
  // keyboard focus at all. #1126 built the mechanism for exactly this
  // and PostCard's clipped title already uses it — this is the third
  // caller, not a third mechanism. The `title` attribute is REMOVED
  // rather than left as a fallback, because two tooltips for one element
  // is what a double-render looks like.
  //
  // # Why it becomes a BUTTON, and what that costs
  //
  // A visual tooltip on FOCUS needs something focusable, and the first
  // spelling — a <span role="img" tabindex="0"> — is the wrong one:
  // svelte's a11y pass flags it (a11y_no_noninteractive_tabindex) and it
  // is right to. A non-interactive element in the tab order is a stop a
  // keyboard reader cannot act on and cannot predict. The WAI-ARIA
  // tooltip pattern puts the trigger on a real interactive element, so
  // this is a <button type="button"> with no click handler: it exists to
  // be focusable and to carry a name, which is exactly what a tooltip
  // trigger is.
  //
  // The cost is real and accepted rather than worked around — one extra
  // tab stop per card. The icon is the only thing on a thumbnail tile
  // that states what KIND of file this is, and a keyboard reader who
  // could not reach it had no way to learn it.
  //
  // It also stops being pointer-events-none, which puts it in front of
  // the marquee's `onControl` bail-out (a press starting on a <button>
  // does not start a selection band). `data-marquee-passthrough` opts
  // back out — the same escape hatch the stretched card link uses — so
  // dragging a band across a wall still works when the drag happens to
  // start on a badge. Without it, a strip of every card would have been
  // dead to the marquee (#1127).
  const tipKey = $derived(`kind:${tooltipKey || kind}`);
  const tip = $derived({ title: label, meta: [], placement: 'anchored' as const });
</script>

<button
  type="button"
  class="inline-flex cursor-default items-center rounded-full
         focus-visible:ring-2 focus-visible:ring-ring focus:outline-none
         {variant === 'inline' ? 'text-fg-muted' : 'bg-black/60 text-white backdrop-blur-sm'}
         {multi ? 'gap-1 px-2 py-1 text-xs font-semibold' : 'p-1.5'} {klass}"
  data-testid={multi ? 'card-kind-multi' : 'card-kind'}
  data-glyph={statesTheSet ? 'multi' : kind}
  data-marquee-passthrough
  aria-label={label}
  onmouseenter={(e) => cardTooltip.enter(tipKey, tip, e)}
  onmousemove={(e) => cardTooltip.move(tipKey, e)}
  onmouseleave={() => cardTooltip.leave(tipKey)}
  onfocus={(e) => cardTooltip.showFor(tipKey, tip, e.currentTarget as HTMLElement)}
  onblur={() => cardTooltip.leave(tipKey)}
>
  {#if multi}
    <!-- Count to the LEFT of the glyph (#1111's spelling): the number is
         read first and the glyph qualifies it. -->
    <span class="tabular-nums">{count}</span>
  {/if}
  <!-- One element, whichever glyph won. The set's sizing is a touch
       smaller and heavier than the lone glyph's so the pair reads as one
       pill, and that stays true of a uniform pack's kind icon: what
       changed in #1203 is WHICH glyph a set draws, never how a set is
       drawn. -->
  <Glyph
    size={multi ? 14 : 15}
    strokeWidth={multi ? 2.25 : 2}
    aria-hidden="true"
  />
</button>
