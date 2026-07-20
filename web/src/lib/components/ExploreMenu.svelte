<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Left-nav "Explore" dropdown — browsable surfaces grouped under
  // one entry point so the navbar doesn't grow horizontally as new
  // explorable surfaces (Gallery, Blogs, Tags, Featured collections,
  // etc.) land. For Phase 1.16 only Gallery + Blogs are wired; the
  // rest get added inline as their phases ship.

  import { t } from '$stores/lang.svelte';
  import { auth } from '$stores/auth.svelte';
  import Menu from '$components/Menu.svelte';
</script>

<Menu align="left">
  {#snippet trigger({ open })}
    <span
      class="inline-flex items-center gap-1 rounded-md px-3 py-1.5 text-sm font-medium text-fg hover:bg-state-hover"
      title={t('nav.explore')}
      aria-label={t('nav.explore')}
    >
      <span>{t('nav.explore')}</span>
      <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" class="text-fg-muted">
        <path d={open ? 'm18 15-6-6-6 6' : 'm6 9 6 6 6-6'} />
      </svg>
    </span>
  {/snippet}

  <a href="/" role="menuitem" class="block px-3 py-1.5 text-sm text-fg hover:bg-surface">
    {t('nav.gallery')}
  </a>
  <!-- Blogs is members-only (#416); a guest picking it from this menu
       would land on the sign-in page. Gallery above stays: `/` is a
       public route and renders a guest empty state. -->
  {#if auth.user}
    <a href="/blogs" role="menuitem" class="block px-3 py-1.5 text-sm text-fg hover:bg-surface">
      {t('nav.blogs')}
    </a>
  {/if}
</Menu>
