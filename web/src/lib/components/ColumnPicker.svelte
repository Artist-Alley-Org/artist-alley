<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Dropdown that lets the user toggle which list-view columns are
  // visible. Sits in the table toolbar. Each checkbox flips a column
  // ID in/out of browseView.listColumns; the picker reads back via
  // `visible` so the checked state stays in sync after a reset.

  import Menu from '$components/Menu.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import { LIST_COLUMNS } from '$stores/browseView.svelte';
  import { t } from '$stores/lang.svelte';

  const visible = $derived(new Set(browseView.listColumns));
</script>

<Menu
  align="right"
  triggerClass="inline-flex rounded-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
>
  {#snippet trigger({ open })}
    <span
      class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-3 py-1.5 text-sm text-fg hover:bg-state-hover"
      aria-label={t('browse.columns')}
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <rect x="3" y="3" width="7" height="18" rx="1" />
        <rect x="14" y="3" width="7" height="18" rx="1" />
      </svg>
      <span>{t('browse.columns')}</span>
      <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" class="text-fg-muted">
        <path d={open ? 'm18 15-6-6-6 6' : 'm6 9 6 6 6-6'} />
      </svg>
    </span>
  {/snippet}

  <!-- Unhideable columns are omitted rather than shown disabled
       (#1127). A greyed-out row invites the reader to work out why it
       cannot be turned off; the selection column has no "off" state
       worth explaining, so it is simply not a setting. -->
  <div class="w-56 py-1">
    {#each LIST_COLUMNS.filter((c) => c.hideable !== false) as col (col.id)}
      <label
        class="flex cursor-pointer items-center gap-2 px-3 py-1.5 text-sm text-fg hover:bg-state-hover"
      >
        <input
          type="checkbox"
          checked={visible.has(col.id)}
          onchange={() => browseView.toggleColumn(col.id)}
          class="h-4 w-4 accent-accent"
        />
        <span>{t(col.labelKey)}</span>
      </label>
    {/each}
    <div class="my-1 border-t border-border"></div>
    <button
      type="button"
      onclick={() => browseView.resetColumns()}
      class="block w-full px-3 py-1.5 text-left text-xs text-fg-muted hover:text-fg"
    >
      {t('browse.reset_columns')}
    </button>
  </div>
</Menu>
