<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Renders a single admin section as a tile grid. Used by every
  // /admin/[section] page and by the dynamic catch-all route.
  //
  // Tile statuses:
  //   live   — clickable, full color
  //   stub   — clickable, "Stub" badge
  //   future — non-clickable, "Phase X" badge, muted

  import type { AdminSection } from '$lib/admin/sections';
  import { t } from '$stores/lang.svelte';
  import { auth } from '$stores/auth.svelte';
  import AdminIcon from './AdminIcon.svelte';

  interface Props {
    section: AdminSection;
  }

  let { section }: Props = $props();

  // #385 — a read-cap holder lands here on a section they partly hold;
  // show only tiles they can open. canSeeTile is true for every tile
  // when system.admin (unchanged full view), and for a read-only role
  // it drops both superuser-only live tiles (no 403 on click) and
  // future tiles (whose absent cap resolves to system.admin-only).
  const visibleTiles = $derived(section.tiles.filter((tile) => auth.canSeeTile(tile)));

  function tileKey(slug: string, key: string): string {
    return `admin.sections.${slug}.tiles.${key}`;
  }
</script>

<header class="mb-6 flex items-start gap-3">
  <span class="mt-0.5 text-fg-muted">
    <AdminIcon name={section.iconKey} size={28} />
  </span>
  <div>
    <h2 class="text-xl font-semibold">{t(`admin.sections.${section.slug}.title`)}</h2>
    <p class="mt-1 text-sm text-fg-muted">{t(`admin.sections.${section.slug}.intro`)}</p>
  </div>
</header>

<div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
  {#each visibleTiles as tile (tile.key)}
    {@const titleK = `${tileKey(section.slug, tile.key)}.title`}
    {@const blurbK = `${tileKey(section.slug, tile.key)}.blurb`}
    {#if tile.status === 'future'}
      <div
        class="cursor-not-allowed rounded-lg border border-dashed border-border bg-surface-elevated/50 p-4 opacity-70"
        aria-disabled="true"
      >
        <div class="flex items-start justify-between gap-2">
          <h3 class="text-sm font-medium text-fg-muted">{t(titleK)}</h3>
          <span class="shrink-0 rounded-full border border-border bg-surface px-2 py-0.5 text-[10px] font-medium text-fg-muted">
            {t('admin.status.phase', { phase: tile.phase ?? '?' })}
          </span>
        </div>
        <p class="mt-1 text-xs text-fg-muted">{t(blurbK)}</p>
      </div>
    {:else}
      <a
        href={tile.href}
        class="rounded-lg border border-border bg-surface-elevated p-4 transition-colors hover:border-accent hover:bg-state-hover"
      >
        <div class="flex items-start justify-between gap-2">
          <h3 class="text-sm font-medium text-fg">{t(titleK)}</h3>
          {#if tile.status === 'stub'}
            <span class="shrink-0 rounded-full bg-warning-container px-2 py-0.5 text-[10px] font-medium text-warning">
              {t('admin.status.stub')}
            </span>
          {/if}
        </div>
        <p class="mt-1 text-xs text-fg-muted">{t(blurbK)}</p>
      </a>
    {/if}
  {/each}
</div>
