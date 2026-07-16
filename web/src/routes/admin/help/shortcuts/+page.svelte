<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/help/shortcuts — keyboard shortcut cheatsheet.
  //
  // Static reference. The keys mirror the handlers in the asset viewer
  // (web/src/lib/components/viewers/AssetViewer.svelte, handleKey) and
  // the global search bar; keep this in sync when those change.

  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';

  // Grouped cheatsheet. `keys` renders as <kbd> chips; `descKey` is an
  // i18n key for the action label.
  const groups: { titleKey: string; rows: { keys: string[]; descKey: string }[] }[] = [
    {
      titleKey: 'admin.help.shortcuts.group_playback',
      rows: [
        { keys: ['Space'], descKey: 'admin.help.shortcuts.play_pause' },
        { keys: ['L'], descKey: 'admin.help.shortcuts.play' },
        { keys: ['K'], descKey: 'admin.help.shortcuts.pause' },
        { keys: ['J'], descKey: 'admin.help.shortcuts.step_back' },
        { keys: ['←', ','], descKey: 'admin.help.shortcuts.frame_back' },
        { keys: ['→', '.'], descKey: 'admin.help.shortcuts.frame_fwd' },
        { keys: ['1', '2', '3', '4', '5'], descKey: 'admin.help.shortcuts.rate' },
      ],
    },
    {
      titleKey: 'admin.help.shortcuts.group_loop',
      rows: [
        { keys: ['I'], descKey: 'admin.help.shortcuts.loop_in' },
        { keys: ['O'], descKey: 'admin.help.shortcuts.loop_out' },
        { keys: ['Backspace'], descKey: 'admin.help.shortcuts.loop_clear' },
      ],
    },
    {
      titleKey: 'admin.help.shortcuts.group_view',
      rows: [
        { keys: ['F'], descKey: 'admin.help.shortcuts.fullscreen' },
        { keys: ['R'], descKey: 'admin.help.shortcuts.reset_view' },
        { keys: ['G'], descKey: 'admin.help.shortcuts.goto_frame' },
        { keys: ['T'], descKey: 'admin.help.shortcuts.tile_mode' },
        { keys: ['Esc'], descKey: 'admin.help.shortcuts.exit_fullscreen' },
      ],
    },
    {
      titleKey: 'admin.help.shortcuts.group_search',
      rows: [
        { keys: ['↑', '↓'], descKey: 'admin.help.shortcuts.search_nav' },
        { keys: ['Enter'], descKey: 'admin.help.shortcuts.search_go' },
        { keys: ['Esc'], descKey: 'admin.help.shortcuts.search_close' },
      ],
    },
  ];
</script>

<svelte:head><title>{t('admin.help.shortcuts.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.help.shortcuts.title')}</h2>
<p class="mb-4 max-w-2xl text-sm text-fg-muted">{t('admin.help.shortcuts.intro')}</p>

<div class="grid max-w-3xl gap-4 sm:grid-cols-2">
  {#each groups as g (g.titleKey)}
    <section class="rounded-lg border border-border bg-surface-elevated p-4">
      <h3 class="mb-3 text-sm font-semibold text-fg">{t(g.titleKey)}</h3>
      <dl class="space-y-2">
        {#each g.rows as row (row.descKey)}
          <div class="flex items-center justify-between gap-3">
            <dt class="text-sm text-fg-muted">{t(row.descKey)}</dt>
            <dd class="flex shrink-0 gap-1">
              {#each row.keys as key (key)}
                <kbd class="rounded border border-border bg-surface px-1.5 py-0.5 font-mono text-[11px] text-fg">{key}</kbd>
              {/each}
            </dd>
          </div>
        {/each}
      </dl>
    </section>
  {/each}
</div>
