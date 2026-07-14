<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<!--
  ┌─────────────────────────────────────────────────────────────────┐
  │  TEMPORARY — LOGO A/B EXPERIMENT (remove when the mark is chosen) │
  └─────────────────────────────────────────────────────────────────┘
  Two brand-mark finalists are in the running:
    • mark-candidate-chevron.svg
    • mark-candidate-rounded.svg
  This component picks ONE at random per full page load and renders it
  everywhere the logo mark appears (navbar + login card). The pick is
  made once at module scope, so it stays stable across client-side
  navigations within a session and re-rolls only on a full refresh. No
  cookie / localStorage — refresh is the reroll.

  REMOVAL CONDITION: once a final mark is chosen, delete the losing SVG
  and this randomizer, and point the survivors (navbar + login) straight
  at the winning asset (or /logo.svg). Tracked in the follow-up issue
  "brand: resolve logo A/B experiment".
-->
<script lang="ts" module>
  import chevronMark from '$lib/assets/brand/mark-candidate-chevron.svg';
  import roundedMark from '$lib/assets/brand/mark-candidate-rounded.svg';

  // Candidate → its asset URL. Keyed so telemetry / debugging can name
  // which one a given load rolled without diffing the SVG bytes.
  const CANDIDATES = [
    { id: 'chevron', src: chevronMark },
    { id: 'rounded', src: roundedMark },
  ] as const;

  // Module-scope roll: evaluated once on first import, cached for the
  // lifetime of the page (survives client-side navigation), re-rolled
  // on a full reload.
  const picked = CANDIDATES[Math.floor(Math.random() * CANDIDATES.length)];
</script>

<script lang="ts">
  // `class` sizes the mark per call site (navbar h-7 w-7, login larger).
  // Decorative: the adjacent wordmark carries the accessible name.
  let { class: className = 'h-7 w-7' }: { class?: string } = $props();
</script>

<img src={picked.src} alt="" class={className} aria-hidden="true" data-brand-mark={picked.id} />
