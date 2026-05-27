<script lang="ts">
  // Collection detail. Minimal in this phase — name + description +
  // an "Upload here" CTA. The upload modal's context prefill picks
  // up the collection_id from the URL pattern (NavUploadButton's
  // regex), so any drop-anywhere or button click while on this
  // page lands the new post into THIS collection.
  //
  // A richer view (posts/resources grid, member management, ACL
  // editor, removal) lands in the dedicated collections phase.

  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { upload } from '$stores/upload.svelte';

  interface Collection {
    id: string;
    name: string;
    description: string;
    visibility: string;
    owner_user_ref: number;
    created_at: string;
    updated_at: string;
  }

  let collection = $state<Collection | null>(null);
  let loading = $state(true);
  let error = $state<string | null>(null);

  const id = $derived(page.params.id ?? '');

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    error = null;
    try {
      const { data, error: apiErr } = await api.GET('/collections/{id}', {
        params: { path: { id } },
      });
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? 'Collection not found.';
        return;
      }
      collection = data as Collection;
    } finally {
      loading = false;
    }
  }

  function uploadHere() {
    upload.open_({ collectionId: id });
  }
</script>

<svelte:head>
  <title>{collection?.name ?? 'Collection'} — artist-alley</title>
</svelte:head>

<div class="mx-auto w-full max-w-5xl px-6 py-6">
  {#if loading}
    <p class="text-fg-muted">Loading…</p>
  {:else if error}
    <p role="alert" class="rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-600 dark:text-red-300">
      {error}
    </p>
  {:else if collection}
    <header class="mb-6">
      <div class="flex items-start justify-between gap-4">
        <div class="min-w-0 flex-1">
          <p class="text-xs text-fg-muted">
            <a href="/collections" class="hover:underline">Collections</a> ·
            {collection.visibility}
          </p>
          <h1 class="mt-1 text-2xl font-semibold">{collection.name}</h1>
          {#if collection.description}
            <p class="mt-2 text-fg-muted">{collection.description}</p>
          {/if}
        </div>
        <button
          type="button"
          onclick={uploadHere}
          class="inline-flex shrink-0 items-center gap-1.5 rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-white shadow-sm hover:bg-accent/90"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
            <polyline points="17 8 12 3 7 8" />
            <line x1="12" y1="3" x2="12" y2="15" />
          </svg>
          Upload to this collection
        </button>
      </div>
    </header>

    <!-- Contents listing is the dedicated collections phase. For
         now, the meta is the page. -->
    <section class="rounded-lg border border-border bg-surface-elevated p-6 text-center text-fg-muted">
      <p class="text-sm">Contents listing lands in the collections phase.</p>
      <p class="mt-1 text-xs">
        For now, use "Upload to this collection" to drop new posts into it, or
        drag files anywhere on this page.
      </p>
    </section>
  {/if}
</div>
