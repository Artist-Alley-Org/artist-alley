<script lang="ts">
  import { t } from '$stores/lang.svelte';

  interface Tile {
    href: string;
    title: () => string;
    blurb: () => string;
  }

  const tiles: Tile[] = [
    { href: '/admin/users',          title: () => t('admin_menu.users'),          blurb: () => t('admin.tile.users') },
    { href: '/admin/roles',          title: () => t('admin_menu.roles'),          blurb: () => t('admin.tile.roles') },
    { href: '/admin/workflow',       title: () => t('admin_menu.workflow'),       blurb: () => t('admin.tile.workflow') },
    { href: '/admin/fields',         title: () => t('admin_menu.fields'),         blurb: () => t('admin.tile.fields') },
    { href: '/admin/resource-types', title: () => t('admin_menu.resource_types'), blurb: () => t('admin.tile.resource_types') },
    { href: '/admin/system',         title: () => t('admin_menu.system'),         blurb: () => t('admin.tile.system') },
  ];
</script>

<svelte:head><title>{t('admin.title')} — artist-alley</title></svelte:head>

<p class="mb-4 text-sm text-fg-muted">{t('admin.intro')}</p>

<div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
  {#each tiles as tile (tile.href)}
    <a
      href={tile.href}
      class="rounded-lg border border-border bg-surface p-4 transition-colors hover:border-accent/50"
    >
      <h2 class="text-base font-semibold text-fg">{tile.title()}</h2>
      <p class="mt-1 text-sm text-fg-muted">{tile.blurb()}</p>
    </a>
  {/each}
</div>
