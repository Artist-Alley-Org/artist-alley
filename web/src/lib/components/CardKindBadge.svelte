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

  interface Props {
    /** The asset's kind, resolved through `kindForAsset` — never
     *  inferred from the extension alone, which cannot tell a sprite
     *  atlas from an ordinary PNG. */
    kind: ViewKind;
    /** How many assets the card stands for. > 1 states the SET instead
     *  of any one member's kind: picking one member's icon for a mixed
     *  bundle says something untrue about the others (#1111). */
    count?: number;
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
  }

  let { kind, count = 1, class: klass = '', variant = 'overlay' }: Props = $props();

  const multi = $derived(count > 1);
  const KindIcon = $derived(iconForKind(kind));
  const label = $derived(
    multi ? t('card.multi.badge_label', { count: String(count) }) : t(`card.fallback.kind.${kind}`),
  );
</script>

<span
  class="pointer-events-none inline-flex items-center rounded-full
         {variant === 'inline' ? 'text-fg-muted' : 'bg-black/60 text-white backdrop-blur-sm'}
         {multi ? 'gap-1 px-2 py-1 text-xs font-semibold' : 'p-1.5'} {klass}"
  data-testid={multi ? 'card-kind-multi' : 'card-kind'}
  aria-label={label}
  title={label}
>
  {#if multi}
    <!-- Count to the LEFT of the glyph (#1111's spelling): the number is
         read first and the glyph qualifies it. -->
    <span class="tabular-nums">{count}</span>
    <MultiAssetIcon size={14} strokeWidth={2.25} aria-hidden="true" />
  {:else}
    <KindIcon size={15} strokeWidth={2} aria-hidden="true" />
  {/if}
</span>
