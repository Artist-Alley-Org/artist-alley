<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/help/shortcuts — keyboard shortcut cheatsheet (operator copy).
  //
  // The catalogue itself is $lib/help/shortcuts.ts and the layout is
  // ShortcutsCheatsheet.svelte; the same pair backs /account/shortcuts.
  // The /admin shell is capability-gated, so a plain user can only
  // reach the /account copy — hence two routes over one catalogue
  // rather than one route both link to.

  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';
  import ShortcutsCheatsheet from '$components/ShortcutsCheatsheet.svelte';
</script>

<svelte:head><title>{t('shortcuts.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('shortcuts.title')}</h2>
<p class="mb-4 max-w-2xl text-sm text-fg-muted">{t('shortcuts.intro')}</p>

<ShortcutsCheatsheet />
