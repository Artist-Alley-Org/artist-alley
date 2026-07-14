<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin home — top-level tile grid mirroring the admin menu
  // structure. Each tile is a section landing page.

  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';
  import AdminIcon from '$components/AdminIcon.svelte';
  import { ADMIN_SECTIONS } from '$lib/admin/sections';
</script>

<svelte:head><title>{t('admin.title')} — {site.name}</title></svelte:head>

<p class="mb-4 text-sm text-fg-muted">{t('admin.intro')}</p>

<div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
  {#each ADMIN_SECTIONS as section (section.slug)}
    <a
      href={`/admin/${section.slug}`}
      class="rounded-lg border border-border bg-surface-elevated p-4 transition-colors hover:border-accent hover:bg-state-hover"
    >
      <div class="flex items-start gap-3">
        <span class="mt-0.5 text-fg-muted">
          <AdminIcon name={section.iconKey} size={22} />
        </span>
        <div class="min-w-0">
          <h2 class="text-base font-semibold text-fg">
            {t(`admin.sections.${section.slug}.title`)}
          </h2>
          <p class="mt-1 text-sm text-fg-muted">{t(`admin.sections.${section.slug}.intro`)}</p>
        </div>
      </div>
    </a>
  {/each}

  <a
    href="/admin/about"
    class="rounded-lg border border-border bg-surface-elevated p-4 transition-colors hover:border-accent hover:bg-state-hover"
  >
    <div class="flex items-start gap-3">
      <span class="mt-0.5 text-fg-muted">
        <AdminIcon name="about" size={22} />
      </span>
      <div class="min-w-0">
        <h2 class="text-base font-semibold text-fg">{t('admin.about.title')}</h2>
        <p class="mt-1 text-sm text-fg-muted">{t('admin.about.intro')}</p>
      </div>
    </div>
  </a>
</div>
