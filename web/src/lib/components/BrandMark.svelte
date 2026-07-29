<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<!--
  The instance brand mark. Rendered wherever the logo appears (navbar,
  login card, auth pages).

  Shows the operator-uploaded logo when one is set (#517) and the
  shipped /logo.svg otherwise. Empty means default — the same contract
  the font slots use, and the reason the default is the safe pick: it
  is in the frontend bundle, so it renders even when storage is
  unreachable.

  If a configured logo fails to load — the object was lost behind its
  pin, the network blipped — we fall back to the shipped mark rather
  than leaving a broken-image icon in the navbar of every page. Chrome
  must not be able to break the page it frames.

  Decorative: the adjacent wordmark carries the accessible name, so the
  image is aria-hidden with an empty alt. Sized per call site via `class`.

  `object-contain` is not cosmetic. Every call site fixes BOTH height and
  width, which was fine while the only mark was the square shipped SVG;
  an operator logo of any other aspect ratio would have been stretched to
  fill that box. Letterboxing inside the box instead keeps the operator's
  proportions AND keeps the rendered box exactly the size the call site
  asked for — which matters in the navbar, because the root layout
  measures the chrome layer and publishes its height as
  `--aa-chrome-bottom` (#707). A mark that could grow the header would
  move that edge for everything positioned against it.
-->
<script lang="ts">
  import { appearance } from '$stores/appearance.svelte';

  let { class: className = 'h-7 w-7' }: { class?: string } = $props();

  const DEFAULT_MARK = '/logo.svg';

  // Set when a custom logo URL fails to load, so we stop retrying it
  // and show the default instead. Keyed by URL so a later upload gets
  // a fresh attempt rather than inheriting the previous failure.
  let failedUrl = $state('');

  const src = $derived(
    appearance.logoUrl && appearance.logoUrl !== failedUrl ? appearance.logoUrl : DEFAULT_MARK,
  );
</script>

<img
  {src}
  alt=""
  class="{className} object-contain"
  aria-hidden="true"
  data-testid="brand-mark"
  onerror={() => {
    if (appearance.logoUrl) failedUrl = appearance.logoUrl;
  }}
/>
