<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The selection's one visible handle: how many are selected, a Clear,
  // and the batch action that makes a selection worth building.
  //
  // # WHY THIS IS MOUNTED IN THE LAYOUT AND NOT IN A PAGE
  //
  // It used to render inline on browse, and browse alone. Selection is
  // a GLOBAL singleton that deliberately survives navigation, and nine
  // other surfaces can put things in it: the two profile URL aliases,
  // /search, /teams/{id}, /collections/{id}, /account/shared,
  // /posts/{id}, /posts/by-asset/{id}, /assets/{id} and
  // /assets/{id}/usage. On every one of them a reader could tick
  // cards and then have no count, no Clear and no action anywhere on
  // the page: an ORPHANED SELECTION, accumulating silently.
  //
  // Mounting once in the shell answers all of them at once, and it
  // answers the harder case too. A selection CARRIED to a route that
  // has no selectable cards of its own is still reachable, because the
  // bar follows the selection rather than the surface.
  //
  // Fixed to the bottom of the viewport rather than sticky inside a
  // page: there is no one page to be sticky inside any more, and the
  // bottom is where a selection bar belongs on a wall you scroll. It
  // renders ONLY while a selection is active, so a page with nothing
  // selected is exactly as it was.

  import { selection } from '$stores/selection.svelte';
  import { auth } from '$stores/auth.svelte';
  import { site } from '$stores/site.svelte';
  import { t } from '$stores/lang.svelte';
  import BatchFieldEditModal from './BatchFieldEditModal.svelte';

  let editOpen = $state(false);

  // The same two conditions that gate the checkbox itself (#515). Not
  // an authorization decision about the batch (the server owns that
  // and answers 403 `bulk_capability_required` when the caller may not
  // reach for the instrument), but the same reachability gate that
  // produced the selection in the first place.
  const canAct = $derived(!!auth.user && !site.demoMode);
</script>

{#if selection.active}
  <div
    role="status"
    data-testid="selection-bar"
    class="pointer-events-auto fixed inset-x-0 bottom-4 z-40 mx-auto flex w-[min(36rem,calc(100%-2rem))]
           items-center justify-between gap-3 rounded-lg border border-border bg-surface-elevated
           px-4 py-2 text-sm shadow-lg"
  >
    <span class="font-medium text-fg" data-testid="selection-count">
      {t('selection.count', { count: String(selection.count) })}
    </span>
    <div class="flex items-center gap-1">
      {#if canAct}
        <button
          type="button"
          onclick={() => (editOpen = true)}
          data-testid="selection-batch-edit"
          class="inline-flex h-9 items-center rounded-md bg-accent px-3 text-sm font-medium text-on-accent
                 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {t('selection.edit_field')}
        </button>
      {/if}
      <button
        type="button"
        onclick={() => selection.clear()}
        data-testid="selection-clear"
        class="inline-flex h-9 items-center rounded-md px-3 text-sm font-medium text-fg-muted transition-colors hover:bg-state-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {t('selection.clear')}
      </button>
    </div>
  </div>
{/if}

<!-- OUTSIDE the `selection.active` block on purpose: the modal owns a
     committed apply's result, and that result stays readable even if the
     operator clears the selection while it is open. Nothing here clears
     it for them.
     `|| editOpen` and not an unconditional mount, though. Modal arms a
     document keydown listener for as long as it exists, and this
     component sits in the app shell, so an unconditional mount would put
     one on every page in the product for a dialog nobody has opened. -->
{#if selection.active || editOpen}
  <BatchFieldEditModal open={editOpen} onclose={() => (editOpen = false)} />
{/if}
