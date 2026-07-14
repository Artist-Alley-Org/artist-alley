<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<!--
  The Artist Alley brand mark (chevron). Rendered wherever the logo
  appears (navbar + login card) and served as the canonical /logo.svg,
  so the auth pages, favicon, and this component all share one asset.

  Decorative: the adjacent wordmark carries the accessible name, so the
  image is aria-hidden with an empty alt. Sized per call site via `class`.
-->
<script lang="ts">
  let { class: className = 'h-7 w-7' }: { class?: string } = $props();
</script>

<img src="/logo.svg" alt="" class={className} aria-hidden="true" />
