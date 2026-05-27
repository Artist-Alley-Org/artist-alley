<script lang="ts">
  // Collections index. Lists the user's own collections + every
  // collection they can see (the API enforces visibility). Click
  // through to /collections/{id} where the upload modal prefills
  // the collection context.
  //
  // This is intentionally minimal in this phase — a richer
  // collection-browser (search, filter, featured tab, etc.) lands
  // in the dedicated collections phase. For now it's just enough
  // surface that the upload modal's context prefill has somewhere
  // to live.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { upload } from '$stores/upload.svelte';

  interface CollectionRow {
    id: string;
    name: string;
    description: string;
    visibility: string;
    owner_user_ref: number;
    created_at: string;
  }

  let collections = $state<CollectionRow[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Create-collection form state.
  let creating = $state(false);
  let newName = $state('');
  let newDesc = $state('');
  let newVisibility = $state<'private' | 'shared' | 'public'>('private');

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    error = null;
    try {
      const { data, error: apiErr } = await api.GET('/collections', {
        params: { query: { limit: 200 } },
      });
      if (apiErr) {
        error = (apiErr as { error?: string }).error ?? 'Failed to load.';
        return;
      }
      collections = (data?.items ?? []) as CollectionRow[];
    } finally {
      loading = false;
    }
  }

  async function createCollection() {
    if (!newName.trim() || creating) return;
    creating = true;
    try {
      const { data, error: apiErr } = await api.POST('/collections', {
        body: {
          name: newName.trim(),
          description: newDesc.trim(),
          visibility: newVisibility,
          membership: 'manual',
        },
      });
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? 'Failed to create.';
        return;
      }
      collections = [data as CollectionRow, ...collections];
      newName = '';
      newDesc = '';
      newVisibility = 'private';
    } finally {
      creating = false;
    }
  }

  function openUploadHere(id: string) {
    upload.open_({ collectionId: id });
  }
</script>

<svelte:head>
  <title>Collections — artist-alley</title>
</svelte:head>

<div class="mx-auto w-full max-w-6xl px-6 py-6">
  <header class="mb-6 flex items-baseline justify-between">
    <h1 class="text-2xl font-semibold">Collections</h1>
    <p class="text-sm text-fg-muted">
      Group posts and assets together — share, organise, or stage for review.
    </p>
  </header>

  <!-- New collection composer -->
  <section class="mb-6 rounded-lg border border-border bg-surface-elevated p-4">
    <h2 class="mb-3 text-sm font-medium text-fg">New collection</h2>
    <div class="grid grid-cols-1 gap-2 md:grid-cols-[2fr_3fr_auto_auto]">
      <input
        type="text"
        bind:value={newName}
        placeholder="Name"
        class="rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
      />
      <input
        type="text"
        bind:value={newDesc}
        placeholder="Description (optional)"
        class="rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
      />
      <select
        bind:value={newVisibility}
        class="rounded border border-border bg-surface px-2 py-1.5 text-sm focus:border-accent focus:outline-none"
      >
        <option value="private">Private</option>
        <option value="shared">Shared</option>
        <option value="public">Public</option>
      </select>
      <button
        type="button"
        onclick={createCollection}
        disabled={creating || !newName.trim()}
        class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
      >
        {creating ? 'Creating…' : 'Create'}
      </button>
    </div>
  </section>

  {#if loading}
    <p class="text-fg-muted">Loading…</p>
  {:else if error}
    <p role="alert" class="rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-600 dark:text-red-300">
      {error}
    </p>
  {:else if collections.length === 0}
    <p class="rounded-md bg-surface-elevated px-4 py-6 text-center text-fg-muted">
      No collections yet. Create one above to get started.
    </p>
  {:else}
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {#each collections as c (c.id)}
        <article class="rounded-lg border border-border bg-surface p-4">
          <a href="/collections/{c.id}" class="block">
            <h3 class="text-base font-semibold text-fg hover:underline">{c.name}</h3>
            {#if c.description}
              <p class="mt-1 line-clamp-2 text-sm text-fg-muted">{c.description}</p>
            {/if}
            <p class="mt-3 text-xs text-fg-muted">
              {c.visibility} · created {new Date(c.created_at).toLocaleDateString()}
            </p>
          </a>
          {#if auth.user && c.owner_user_ref === auth.user.ref}
            <button
              type="button"
              onclick={() => openUploadHere(c.id)}
              class="mt-3 inline-flex items-center gap-1 rounded-md border border-border bg-surface-elevated px-2.5 py-1 text-xs text-fg-muted hover:text-fg"
              title="Upload directly into this collection"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
                <polyline points="17 8 12 3 7 8" />
                <line x1="12" y1="3" x2="12" y2="15" />
              </svg>
              Upload here
            </button>
          {/if}
        </article>
      {/each}
    </div>
  {/if}
</div>
