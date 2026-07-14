<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Phase 1.14.A-bridge — /admin/system/ai landing.
  //
  // The earlier flat surface conflated three audiences:
  //
  //   1. Operator wiring up a credential — wants the provider list.
  //   2. Operator reasoning about routing + privacy + budgets — wants
  //      the Phase 1.14.A inference config (lives at /admin/ai/config).
  //   3. Operator tracking spend — wants the usage dashboard
  //      (/admin/ai/usage).
  //
  // Plus the bridge layer (this PR) introduces tag provenance, which
  // the operator may want documentation on rather than a settings
  // toggle. So this landing is a tile grid pointing at the four
  // surfaces, each with a one-line blurb explaining what lives there.

  import { t } from '$stores/lang.svelte';

  import { site } from '$stores/site.svelte';
  interface Tile {
    key: string;
    href: string;
    external?: boolean;
  }

  const TILES: Tile[] = [
    { key: 'providers',   href: '/admin/system/ai/providers' },
    { key: 'mcp_clients', href: '/admin/ai/mcp-clients' },
    { key: 'inference',   href: '/admin/ai/config' },
    { key: 'usage',       href: '/admin/ai/usage' },
    { key: 'provenance',  href: '/docs/observability/ai-provenance.md', external: true },
  ];
</script>

<svelte:head><title>{t('admin.system.ai_landing.title')} — {site.name}</title></svelte:head>

<header class="mb-6">
  <h2 class="text-xl font-semibold">{t('admin.system.ai_landing.title')}</h2>
  <p class="mt-1 text-sm text-fg-muted">{t('admin.system.ai_landing.intro')}</p>
</header>

<div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
  {#each TILES as tile (tile.key)}
    {@const titleK = `admin.system.ai_landing.tiles.${tile.key}.title`}
    {@const blurbK = `admin.system.ai_landing.tiles.${tile.key}.blurb`}
    <a
      href={tile.href}
      class="block rounded-lg border border-border bg-surface-elevated p-4 transition hover:border-border-strong hover:bg-surface"
      target={tile.external ? '_blank' : undefined}
      rel={tile.external ? 'noopener noreferrer' : undefined}
    >
      <div class="flex items-start justify-between gap-2">
        <h3 class="text-sm font-medium text-fg">{t(titleK)}</h3>
        {#if tile.external}
          <span class="shrink-0 rounded-full border border-border bg-surface px-2 py-0.5 text-[10px] font-medium text-fg-muted">
            {t('admin.system.ai_landing.external')}
          </span>
        {/if}
      </div>
      <p class="mt-1 text-xs text-fg-muted">{t(blurbK)}</p>
    </a>
  {/each}
</div>

<section class="mt-8 max-w-3xl rounded-lg border border-border bg-surface-elevated p-4">
  <h3 class="text-sm font-medium text-fg">{t('admin.system.ai_landing.merge_section.title')}</h3>
  <p class="mt-2 text-xs text-fg-muted">{t('admin.system.ai_landing.merge_section.intro')}</p>
  <dl class="mt-3 grid grid-cols-1 gap-2 text-xs sm:grid-cols-3">
    <div class="rounded border border-border bg-surface p-3">
      <dt class="font-medium text-fg">{t('admin.system.ai_landing.merge_section.manual_term')}</dt>
      <dd class="mt-1 text-fg-muted">{t('admin.system.ai_landing.merge_section.manual_def')}</dd>
    </div>
    <div class="rounded border border-border bg-surface p-3">
      <dt class="font-medium text-fg">{t('admin.system.ai_landing.merge_section.import_term')}</dt>
      <dd class="mt-1 text-fg-muted">{t('admin.system.ai_landing.merge_section.import_def')}</dd>
    </div>
    <div class="rounded border border-border bg-surface p-3">
      <dt class="font-medium text-fg">{t('admin.system.ai_landing.merge_section.ai_term')}</dt>
      <dd class="mt-1 text-fg-muted">{t('admin.system.ai_landing.merge_section.ai_def')}</dd>
    </div>
  </dl>
</section>
