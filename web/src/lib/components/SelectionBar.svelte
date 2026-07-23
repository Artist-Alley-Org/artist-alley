<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Minimal "N selected · Clear" indicator (#515 slice 3). Deliberately
  // NOT the floating bulk-action bar — that, and every bulk action
  // (edit / tag / delete / move / export / contact sheet), is #39. This
  // is just enough to make the shared selection primitive independently
  // usable + verifiable: it shows the live count and offers a Clear.
  //
  // A thin inline strip, shown only while a selection is active. Mounted
  // by the grid surfaces that own selectable cards (browse today).

  import { selection } from '$stores/selection.svelte';
  import { t } from '$stores/lang.svelte';
</script>

{#if selection.active}
  <div
    role="status"
    class="flex items-center justify-between gap-3 rounded-lg border border-border bg-surface-elevated px-4 py-2 text-sm shadow-sm"
  >
    <span class="font-medium text-fg">{t('selection.count', { count: String(selection.count) })}</span>
    <button
      type="button"
      onclick={() => selection.clear()}
      class="inline-flex h-9 items-center rounded-md px-3 text-sm font-medium text-fg-muted transition-colors hover:bg-state-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      {t('selection.clear')}
    </button>
  </div>
{/if}
