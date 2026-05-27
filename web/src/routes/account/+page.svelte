<script lang="ts">
  import { t } from '$stores/lang.svelte';

  interface Tile {
    href: string;
    title: () => string;
    blurb: string;
  }

  const tiles: Tile[] = [
    { href: '/account/profile',     title: () => t('account.sections.profile'),     blurb: 'Display name, bio, avatar, links.' },
    { href: '/account/preferences', title: () => t('account.sections.preferences'), blurb: 'Theme, language.' },
    { href: '/account/preferences/ai', title: () => t('account.sections.ai'),       blurb: 'Per-user AI provider override.' },
    { href: '/account/password',    title: () => t('account.sections.password'),    blurb: 'Change password.' },
    { href: '/account/tokens',      title: () => t('account.sections.tokens'),      blurb: 'Personal access tokens.' },
    { href: '/account/messages',    title: () => t('account.sections.messages'),    blurb: 'Inbox & notifications.' },
  ];
</script>

<svelte:head><title>Account — artist-alley</title></svelte:head>

<p class="mb-4 text-sm text-fg-muted">{t('account.overview.intro')}</p>

<div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
  {#each tiles as tile (tile.href)}
    <a
      href={tile.href}
      class="rounded-lg border border-border bg-surface p-4 transition-colors hover:border-accent/50"
    >
      <h2 class="text-base font-semibold text-fg">{tile.title()}</h2>
      <p class="mt-1 text-sm text-fg-muted">{tile.blurb}</p>
    </a>
  {/each}
</div>
