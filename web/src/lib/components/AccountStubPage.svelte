<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Shared layout for /account/* stub pages: title, intro, and a
  // "coming in Phase X" placeholder card. The dynamic catch-all route
  // /account/[stub]/+page.svelte renders this for every item in the
  // account sections registry whose status is 'stub' or 'future'.
  //
  // Static account pages that have their own content (profile,
  // preferences, tokens) don't go through this — they win the route
  // precedence because static beats dynamic in SvelteKit.

  import { t } from '$stores/lang.svelte';

  import { site } from '$stores/site.svelte';
  interface Props {
    slug: string;       // i18n lookup key under `account.items`
    phase?: string;     // optional phase label shown as a pill
  }

  let { slug, phase }: Props = $props();

  const titleKey = $derived(`account.items.${slug}.title`);
  const blurbKey = $derived(`account.items.${slug}.blurb`);
</script>

<svelte:head><title>{t(titleKey)} — {site.name}</title></svelte:head>

<header class="mb-4 flex items-start justify-between gap-3">
  <div>
    <h2 class="text-xl font-semibold">{t(titleKey)}</h2>
    <p class="mt-1 text-sm text-fg-muted">{t(blurbKey)}</p>
  </div>
  {#if phase}
    <span class="shrink-0 rounded-full bg-surface-elevated px-2.5 py-0.5 text-xs font-medium text-fg-muted">
      {t('admin.status.phase', { phase })}
    </span>
  {/if}
</header>

<div class="max-w-xl rounded-lg border border-dashed border-border bg-surface/40 p-4 text-sm text-fg-muted">
  <p>{t('account.stub.coming_soon')}</p>
</div>
