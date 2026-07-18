<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin dropdown — gear icon → list of admin sections.
  //
  // Visibility-gated on `auth.can('system.admin')` so non-admin users
  // never see it. Backend enforces every action regardless; this is
  // a render-time hide for ergonomics, not a security gate.
  //
  // Layout: a scrollable list of all top-level sections plus an
  // "About" pinned at the bottom. Each section opens its landing
  // page, which is the gateway into the section's pages.

  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import Menu from '$components/Menu.svelte';
  import Pill from '$components/Pill.svelte';
  import AdminIcon from '$components/AdminIcon.svelte';
  import { ADMIN_SECTIONS } from '$lib/admin/sections';

  // #385 — show the gear to anyone who can open at least one admin
  // surface (read-cap holders included), and list only the sections
  // that have a live tile they can actually open. `system.admin` sees
  // everything, unchanged.
  const canSeeAdmin = $derived(auth.canSeeAdmin);
  const visibleSections = $derived(
    ADMIN_SECTIONS.filter((s) => s.tiles.some((t) => auth.canSeeTile(t))),
  );
  const pendingCount = 0; // future: pending workflow approvals etc.
</script>

{#if canSeeAdmin}
  <Menu align="right" triggerTestId="nav-admin-menu-trigger" panelTestId="admin-menu-panel">
    {#snippet trigger({ open })}
      <span
        class="relative inline-flex h-9 w-9 items-center justify-center rounded-full hover:bg-state-hover"
        title={t('admin_menu.title')}
        aria-label={t('admin_menu.title')}
      >
        <!-- 3-dot vertical "more" icon — the standard admin/overflow
             affordance. The Pill in the corner surfaces pending admin
             alerts when there are any. -->
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="12" cy="5" r="1.2" />
          <circle cx="12" cy="12" r="1.2" />
          <circle cx="12" cy="19" r="1.2" />
        </svg>
        <Pill count={pendingCount} label="admin alerts" />
        <span class="sr-only">{open ? 'close admin menu' : 'open admin menu'}</span>
      </span>
    {/snippet}

    <div class="max-h-[80vh] w-64 overflow-y-auto py-1">
      <a href="/admin" role="menuitem" class="block px-3 py-1.5 text-sm font-medium text-fg hover:bg-surface-elevated">
        {t('admin_menu.overview')}
      </a>
      <div class="my-1 border-t border-border"></div>

      {#each visibleSections as section (section.slug)}
        <a
          href={`/admin/${section.slug}`}
          role="menuitem"
          class="flex items-center gap-2 px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated"
        >
          <span class="text-fg-muted"><AdminIcon name={section.iconKey} size={16} /></span>
          <span>{t(`admin.sections.${section.slug}.title`)}</span>
        </a>
      {/each}

      <div class="my-1 border-t border-border"></div>
      <a href="/admin/about" role="menuitem" class="flex items-center gap-2 px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated">
        <span class="text-fg-muted"><AdminIcon name="about" size={16} /></span>
        <span>{t('admin_menu.about')}</span>
      </a>
    </div>
  </Menu>
{/if}
