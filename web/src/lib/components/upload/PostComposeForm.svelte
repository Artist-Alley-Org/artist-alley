<script lang="ts">
  // The compose form at the bottom of the upload modal. Title,
  // description, visibility, tags, post-mode toggle, workflow state,
  // collection prefill, and the "Just upload as assets — no post"
  // escape hatch.
  //
  // All bindings flow into upload.compose, which the store reads at
  // submit() time. State is intentionally lightweight: no validation
  // beyond what the server enforces, no async dependencies beyond
  // the workflow-states fetch.

  import { upload } from '$stores/upload.svelte';
  import { api } from '$api/client';

  // Workflow states for the post domain. Loaded once when the form
  // mounts; the API call is cached server-side so this is cheap.
  interface WorkflowState {
    id: string;
    code: string;
    label: string;
    sort_order: number;
    is_initial: boolean;
    is_terminal: boolean;
    icon: string;
    color: string;
    requires_note: boolean;
  }

  let states = $state<WorkflowState[]>([]);
  let statesLoaded = $state(false);

  $effect(() => {
    void loadStates();
  });

  async function loadStates() {
    try {
      const { data } = await api.GET('/workflow/states', {
        params: { query: { domain: 'post' } },
      });
      if (data) states = data as WorkflowState[];
    } catch {
      // Soft fail — the dropdown just doesn't render. Default state
      // (NULL → server's is_initial) still works.
    } finally {
      statesLoaded = true;
    }
  }

  // Show the state dropdown only when there's a real choice. With 1
  // state in the domain there's nothing meaningful for the user to
  // pick.
  const showStates = $derived(states.length > 1);

  // Tag chip input.
  let tagDraft = $state('');
  function commitTag() {
    const t = tagDraft.trim().toLowerCase();
    if (!t) return;
    if (upload.compose.tags.includes(t)) {
      tagDraft = '';
      return;
    }
    upload.compose.tags = [...upload.compose.tags, t];
    tagDraft = '';
  }
  function removeTag(t: string) {
    upload.compose.tags = upload.compose.tags.filter((x) => x !== t);
  }
  function handleTagKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      commitTag();
    } else if (e.key === 'Backspace' && tagDraft === '' && upload.compose.tags.length > 0) {
      upload.compose.tags = upload.compose.tags.slice(0, -1);
    }
  }
</script>

<div class="space-y-4 rounded-lg border border-border bg-surface p-4">
  <!-- "Create a post" toggle. When off, the rest of the form
       collapses — the upload still happens, just no post wraps the
       resulting assets. -->
  <label class="flex items-center gap-2 text-sm">
    <input
      type="checkbox"
      bind:checked={upload.compose.enabled}
      class="h-4 w-4 rounded border-border accent-accent"
    />
    <span class="font-medium text-fg">Create a post from these assets</span>
  </label>

  {#if upload.compose.enabled}
    <!-- Title + description -->
    <div class="space-y-2">
      <input
        type="text"
        bind:value={upload.compose.title}
        placeholder="Post title (optional)"
        maxlength="500"
        class="w-full rounded border border-border bg-surface-elevated px-3 py-2 text-sm focus-visible:border-border-strong focus:outline-none"
        aria-label="Post title"
      />
      <textarea
        bind:value={upload.compose.description}
        placeholder="Describe your post (optional)"
        rows="2"
        class="w-full resize-y rounded border border-border bg-surface-elevated px-3 py-2 text-sm focus-visible:border-border-strong focus:outline-none"
        aria-label="Post description"
      ></textarea>
    </div>

    <!-- Visibility + post mode + workflow state — three pickers in a row. -->
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 md:grid-cols-3">
      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">Visibility</span>
        <select
          bind:value={upload.compose.visibility}
          class="w-full rounded border border-border bg-surface-elevated px-2 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
        >
          <option value="public">Public</option>
          <option value="followers">Followers</option>
          <option value="private">Private</option>
        </select>
      </label>

      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">Post mode</span>
        <select
          bind:value={upload.compose.mode}
          class="w-full rounded border border-border bg-surface-elevated px-2 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
        >
          <option value="one-post">One post with all files</option>
          <option value="one-per-file">One post per file</option>
        </select>
      </label>

      {#if showStates}
        <label class="block text-xs">
          <span class="mb-1 block text-fg-muted">Workflow state</span>
          <select
            bind:value={upload.compose.stateId}
            class="w-full rounded border border-border bg-surface-elevated px-2 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
          >
            <option value={null}>Default</option>
            {#each states as s (s.id)}
              <option value={s.id}>{s.label}</option>
            {/each}
          </select>
        </label>
      {/if}
    </div>

    <!-- Tags -->
    <div>
      <p class="mb-1 text-xs text-fg-muted">Tags</p>
      <div class="flex flex-wrap items-center gap-1.5 rounded border border-border bg-surface-elevated px-2 py-1.5">
        {#each upload.compose.tags as tag (tag)}
          <span class="inline-flex items-center gap-1 rounded-full bg-surface px-2 py-0.5 text-xs text-fg">
            #{tag}
            <button
              type="button"
              onclick={() => removeTag(tag)}
              class="text-fg-muted hover:text-fg"
              aria-label="Remove tag {tag}"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
                <path d="M18 6 6 18M6 6l12 12" />
              </svg>
            </button>
          </span>
        {/each}
        <input
          type="text"
          bind:value={tagDraft}
          onkeydown={handleTagKeydown}
          onblur={commitTag}
          placeholder={upload.compose.tags.length === 0 ? 'press Enter to add' : '+'}
          class="min-w-[8rem] flex-1 bg-transparent px-1 py-0.5 text-sm placeholder:text-fg-muted/60 focus:outline-none"
        />
      </div>
    </div>

    {#if upload.compose.collectionId}
      <p class="rounded bg-accent/10 px-3 py-2 text-xs text-accent">
        Will be added to the current collection.
      </p>
    {/if}
  {/if}
</div>
