<script lang="ts">
  import { t } from '$stores/lang.svelte';

  interface Tile {
    href: string;
    title: () => string;
    blurb: () => string;
  }

  const tiles: Tile[] = [
    { href: '/admin/system/site', title: () => t('admin.system.site.title'), blurb: () => t('admin.system.tile.site') },
    { href: '/admin/system/smtp', title: () => t('admin.system.smtp.title'), blurb: () => t('admin.system.tile.smtp') },
    { href: '/admin/system/auth', title: () => t('admin.system.auth.title'), blurb: () => t('admin.system.tile.auth') },
    { href: '/admin/system/ai',   title: () => t('admin.system.ai.title'),   blurb: () => t('admin.system.tile.ai') },
    { href: '/admin/system/log',  title: () => t('admin.system.log.title'),  blurb: () => t('admin.system.tile.log') },
  ];
</script>

<svelte:head><title>{t('admin.system.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.system.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('admin.system.intro')}</p>

<div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
  {#each tiles as tile (tile.href)}
    <a
      href={tile.href}
      class="rounded-lg border border-border bg-surface p-4 transition-colors hover:border-accent/50"
    >
      <h3 class="text-base font-semibold text-fg">{tile.title()}</h3>
      <p class="mt-1 text-sm text-fg-muted">{tile.blurb()}</p>
    </a>
  {/each}
</div>
