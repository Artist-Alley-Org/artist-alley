<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // CollectionPicker — modal to pick (or create) a collection to add
  // one-or-many assets into.
  //
  // Used by:
  //   - AssetViewer Edit menu ("Add to collection…") for the current
  //     asset
  //   - AssetPlaylist toolbarActions ("Add all to collection") for the
  //     whole playlist
  //
  // Behaviour:
  //   - Lists the caller's writable collections (tab=mine).
  //     Free-text search filters server-side via `q=`.
  //   - "+ New collection" inline form creates one and selects it.
  //   - On confirm: POSTs each asset to /collections/{id}/resources
  //     in parallel (small enough that we don't bother with batching
  //     for now — 10 assets × 1 round-trip each is fine).
  //   - Toast-style banner with success / partial-failure summary.
  //   - Closes on ESC, backdrop click, ×, and on successful add.

  import { onMount, onDestroy } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface Props {
    /** Asset ids to add. One element for the single-asset case, many
        for the bulk-from-playlist case. */
    assetIds: string[];
    /** Called when the user closes the modal (× / ESC / backdrop /
        successful add). */
    onClose: () => void;
    /** Optional callback fired after a successful add so the host
        can refresh its data (e.g. re-fetch the collection page when
        adding from within a CollectionHost). */
    onAdded?: (collectionId: string, addedCount: number) => void;
  }

  let { assetIds, onClose, onAdded }: Props = $props();

  interface CollectionRow {
    id: string;
    name: string;
    description?: string;
    visibility: 'private' | 'shared' | 'public';
    item_count?: number;
  }

  let dialogEl: HTMLDialogElement | undefined = $state();
  let searchEl: HTMLInputElement | undefined = $state();
  let collections = $state<CollectionRow[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let query = $state('');

  // Adding state.
  let busy = $state(false);
  let addError = $state<string | null>(null);

  // Inline-create state.
  let creating = $state(false);
  let newName = $state('');
  let newError = $state<string | null>(null);

  // Debounced search reload.
  let searchTimer: ReturnType<typeof setTimeout> | undefined;

  onMount(() => {
    dialogEl?.showModal();
    document.body.classList.add('overflow-hidden');
    queueMicrotask(() => searchEl?.focus());
    void load();
  });

  onDestroy(() => {
    document.body.classList.remove('overflow-hidden');
    if (searchTimer) clearTimeout(searchTimer);
  });

  async function load() {
    loading = true;
    loadError = null;
    try {
      const { data, error } = await api.GET('/collections', {
        params: {
          query: {
            tab: 'mine',
            q: query || undefined,
            limit: 100,
          },
        },
      });
      if (error) throw new Error((error as { error?: string }).error ?? 'Failed to load collections');
      collections = ((data as { items?: CollectionRow[] }).items ?? []) as CollectionRow[];
    } catch (e) {
      loadError = e instanceof Error ? e.message : 'Failed to load';
      collections = [];
    } finally {
      loading = false;
    }
  }

  function onSearchInput() {
    if (searchTimer) clearTimeout(searchTimer);
    searchTimer = setTimeout(() => void load(), 200);
  }

  async function addToCollection(col: CollectionRow) {
    if (busy) return;
    busy = true;
    addError = null;
    let added = 0;
    const failures: string[] = [];
    // Sequential adds — keeps the failure mode legible. assetIds is
    // small (1 for per-asset, 1–dozens for bulk-from-playlist); for a
    // future 1000-asset bulk we'd switch to a server-side batch endpoint.
    for (const assetId of assetIds) {
      const { error } = await api.POST('/collections/{id}/resources', {
        params: { path: { id: col.id } },
        body: {
          asset_id: assetId,
          // openapi-typescript generates these as required even
          // though the spec has defaults; pass them explicitly.
          sort_order: 0,
          pinned: true,
        },
      });
      if (error) {
        failures.push(assetId);
      } else {
        added += 1;
      }
    }
    busy = false;
    if (failures.length === assetIds.length) {
      addError = t('collection_picker.add_failed_all');
      return;
    }
    onAdded?.(col.id, added);
    if (failures.length > 0) {
      // Partial success — keep the modal open so the user sees the
      // count and can retry / pick a different collection. The host
      // refreshes via onAdded above.
      addError = t('collection_picker.add_failed_partial', {
        added: String(added),
        total: String(assetIds.length),
      });
      return;
    }
    handleClose();
  }

  async function submitCreate(e: SubmitEvent) {
    e.preventDefault();
    const name = newName.trim();
    if (!name) return;
    creating = true;
    newError = null;
    try {
      const { data, error } = await api.POST('/collections', {
        body: {
          name,
          visibility: 'private',
          membership: 'manual',
          featured: false,
        },
      });
      if (error || !data) {
        throw new Error((error as { error?: string } | undefined)?.error ?? 'Failed to create');
      }
      const created = data as CollectionRow;
      // Add the assets straight into the freshly-created collection
      // so the user doesn't have to click twice.
      newName = '';
      await addToCollection(created);
    } catch (e) {
      newError = e instanceof Error ? e.message : 'Failed to create';
    } finally {
      creating = false;
    }
  }

  function handleClose() {
    dialogEl?.close();
    onClose();
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === dialogEl) handleClose();
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      handleClose();
    }
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<dialog
  bind:this={dialogEl}
  onclick={handleBackdropClick}
  class="collection-picker m-0 max-h-none max-w-none w-full h-full bg-transparent p-0 backdrop:bg-black/70 backdrop:backdrop-blur-sm"
  aria-labelledby="collection-picker-title"
>
  <div
    class="relative mx-auto my-auto flex max-h-[80vh] w-[28rem] max-w-[90vw] flex-col overflow-hidden rounded-lg border border-border bg-surface text-fg shadow-2xl"
    role="presentation"
    style="margin-top: 10vh"
  >
    <header class="flex shrink-0 items-center justify-between border-b border-border px-4 py-3">
      <h2 id="collection-picker-title" class="text-sm font-medium">
        {assetIds.length === 1
          ? t('collection_picker.title_single')
          : t('collection_picker.title_bulk', { n: String(assetIds.length) })}
      </h2>
      <button
        type="button"
        onclick={handleClose}
        class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-surface-elevated hover:text-fg"
        aria-label={t('viewer_menu.close')}
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M18 6 6 18M6 6l12 12" />
        </svg>
      </button>
    </header>

    <div class="border-b border-border p-3">
      <input
        bind:this={searchEl}
        bind:value={query}
        oninput={onSearchInput}
        type="search"
        placeholder={t('collection_picker.search_placeholder')}
        class="w-full rounded border border-border bg-surface-elevated px-2 py-1.5 text-sm focus:border-accent focus:outline-none"
      />
    </div>

    <div class="min-h-0 flex-1 overflow-y-auto">
      {#if loading}
        <div class="p-4 text-center text-sm text-fg-muted">{t('collection_picker.loading')}</div>
      {:else if loadError}
        <div role="alert" class="m-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-xs text-danger">
          {loadError}
        </div>
      {:else if collections.length === 0}
        <div class="p-4 text-center text-sm text-fg-muted">
          {query ? t('collection_picker.no_match') : t('collection_picker.empty')}
        </div>
      {:else}
        <ul>
          {#each collections as col (col.id)}
            <li>
              <button
                type="button"
                onclick={() => addToCollection(col)}
                disabled={busy}
                class="flex w-full items-start gap-3 border-b border-border px-3 py-2 text-left text-sm transition-colors hover:bg-surface-elevated disabled:opacity-50"
              >
                <span class="min-w-0 flex-1">
                  <span class="block truncate font-medium text-fg">{col.name}</span>
                  {#if col.description}
                    <span class="block truncate text-xs text-fg-muted">{col.description}</span>
                  {/if}
                </span>
                <span class="shrink-0 text-xs text-fg-muted" title={col.visibility}>
                  {col.visibility}
                </span>
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </div>

    {#if addError}
      <div role="alert" class="border-t border-border bg-danger-container px-3 py-2 text-xs text-danger">
        {addError}
      </div>
    {/if}

    <footer class="border-t border-border p-3">
      <form onsubmit={submitCreate} class="flex gap-2">
        <input
          bind:value={newName}
          type="text"
          placeholder={t('collection_picker.new_placeholder')}
          maxlength="200"
          disabled={creating || busy}
          class="min-w-0 flex-1 rounded border border-border bg-surface-elevated px-2 py-1.5 text-sm focus:border-accent focus:outline-none disabled:opacity-50"
        />
        <button
          type="submit"
          disabled={!newName.trim() || creating || busy}
          class="rounded bg-accent px-3 py-1.5 text-xs font-medium text-on-accent hover:bg-accent/90 disabled:opacity-50"
        >
          {creating ? t('collection_picker.creating') : t('collection_picker.create_and_add')}
        </button>
      </form>
      {#if newError}
        <p class="mt-2 text-xs text-danger">{newError}</p>
      {/if}
    </footer>
  </div>
</dialog>

<style>
  dialog.collection-picker {
    border: none;
    inset: 0;
  }
  dialog.collection-picker:not([open]) {
    display: none;
  }
</style>
