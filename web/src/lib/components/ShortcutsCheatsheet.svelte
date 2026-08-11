<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The keyboard-shortcut cheatsheet renderer.
  //
  // Content lives in $lib/help/shortcuts.ts; this file is only layout,
  // so /account/shortcuts and /admin/help/shortcuts cannot drift apart.

  import { t } from '$stores/lang.svelte';
  import { SHORTCUT_GROUPS } from '$lib/help/shortcuts';
</script>

<!--
  Masonry-ish column flow rather than a grid: the groups are wildly
  uneven (whiteboard tools has 16 rows, ebook has 1) and a grid leaves
  a column-height of dead space under every short card.
-->
<div class="gap-4 [column-fill:balance] sm:columns-2 xl:columns-3" data-testid="shortcuts-groups">
  {#each SHORTCUT_GROUPS as g (g.id)}
    <section
      class="mb-4 break-inside-avoid rounded-lg border border-border bg-surface-elevated p-4"
      data-testid="shortcut-group-{g.id}"
    >
      <h3 class="text-sm font-semibold text-fg">{t(g.titleKey)}</h3>
      {#if g.noteKey}
        <p class="mb-3 mt-1 text-xs text-fg-muted">{t(g.noteKey)}</p>
      {:else}
        <div class="mb-3"></div>
      {/if}
      <dl class="space-y-2">
        {#each g.rows as row (row.descKey)}
          <div class="flex items-baseline justify-between gap-3">
            <dt class="text-sm text-fg-muted">{t(row.descKey)}</dt>
            <dd class="flex shrink-0 flex-wrap justify-end gap-1">
              {#each row.keys as key (key)}
                <kbd
                  class="rounded border border-border bg-surface px-1.5 py-0.5 font-mono text-[11px] text-fg"
                  >{key}</kbd
                >
              {/each}
            </dd>
          </div>
        {/each}
      </dl>
    </section>
  {/each}
</div>
