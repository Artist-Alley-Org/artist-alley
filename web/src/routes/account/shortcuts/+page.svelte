<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/shortcuts — keyboard shortcut cheatsheet (#600).
  //
  // Same catalogue the operator copy at /admin/help/shortcuts renders
  // ($lib/help/shortcuts.ts). This route exists because the /admin
  // shell is capability-gated: a user with no admin capability gets a
  // "no permission" panel there, so the cheatsheet was unreachable for
  // exactly the people most likely to want it.

  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';
  import ShortcutsCheatsheet from '$components/ShortcutsCheatsheet.svelte';
</script>

<svelte:head><title>{t('shortcuts.title')} — {site.name}</title></svelte:head>

<section class="space-y-4">
  <header>
    <h1 class="text-2xl font-semibold text-fg">{t('shortcuts.title')}</h1>
    <p class="mt-1 max-w-2xl text-sm text-fg-muted">{t('shortcuts.intro')}</p>
  </header>

  <ShortcutsCheatsheet />
</section>
