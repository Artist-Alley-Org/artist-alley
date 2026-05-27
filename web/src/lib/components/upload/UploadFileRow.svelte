<script lang="ts">
  // One row per pending upload. Thumb + filename + progress bar +
  // state badge + remove. Per-file title is editable inline (the
  // post composer's title applies to the post, not the assets);
  // tags are a chip input.
  //
  // While the row is uploading the progress bar is the focal point;
  // when it's ready the row collapses to a compact summary so the
  // user can scan a queue of many files.

  import type { UploadRow } from '$stores/upload.svelte';
  import { upload } from '$stores/upload.svelte';

  interface Props {
    row: UploadRow;
  }
  let { row }: Props = $props();

  const pct = $derived(Math.round(row.progress * 100));

  const stateLabel = $derived(
    row.state === 'queued' ? 'Queued'
    : row.state === 'uploading' ? `Uploading ${pct}%`
    : row.state === 'asset-creating' ? 'Finalizing'
    : row.state === 'ready' ? (row.deduped ? 'Already uploaded' : 'Ready')
    : row.state === 'errored' ? 'Failed'
    : row.state,
  );

  const stateClass = $derived(
    row.state === 'ready' ? 'bg-emerald-500/15 text-emerald-700 dark:text-emerald-400'
    : row.state === 'errored' ? 'bg-red-500/15 text-red-600 dark:text-red-400'
    : row.state === 'queued' ? 'bg-surface-elevated text-fg-muted'
    : 'bg-accent/15 text-accent',
  );

  let tagDraft = $state('');

  function commitTag() {
    const t = tagDraft.trim().toLowerCase();
    if (!t) return;
    if (row.tags.includes(t)) {
      tagDraft = '';
      return;
    }
    row.tags = [...row.tags, t];
    tagDraft = '';
  }

  function removeTag(t: string) {
    row.tags = row.tags.filter((x) => x !== t);
  }

  function handleTagKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      commitTag();
    } else if (e.key === 'Backspace' && tagDraft === '' && row.tags.length > 0) {
      row.tags = row.tags.slice(0, -1);
    }
  }

  function humanSize(n: number): string {
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
    if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
    return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`;
  }
</script>

<div class="flex gap-3 rounded-lg border border-border bg-surface p-3">
  <!-- Thumb: the file as an inline blob URL. For non-image files
       the browser will fail to load — we fall back to a generic
       icon via onerror. -->
  <div class="relative h-16 w-16 shrink-0 overflow-hidden rounded bg-surface-elevated">
    <!-- svelte-ignore a11y_img_redundant_alt -->
    <img
      src={row.objectUrl}
      alt=""
      class="h-full w-full object-cover"
      onerror={(e) => {
        const img = e.currentTarget as HTMLImageElement;
        img.style.display = 'none';
      }}
    />
    <div class="absolute inset-0 -z-10 flex items-center justify-center text-fg-muted/40">
      <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
        <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
        <polyline points="14 2 14 8 20 8" />
      </svg>
    </div>
  </div>

  <div class="min-w-0 flex-1 space-y-2">
    <!-- Title (editable) + state badge + remove -->
    <div class="flex items-start gap-2">
      <input
        type="text"
        bind:value={row.title}
        placeholder={row.file.name}
        class="min-w-0 flex-1 rounded border border-border bg-surface-elevated px-2 py-1 text-sm focus:border-accent focus:outline-none"
        aria-label="Asset title"
      />
      <span class="inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium {stateClass}">
        {stateLabel}
      </span>
      <button
        type="button"
        onclick={() => upload.removeRow(row.id)}
        class="shrink-0 rounded p-1 text-fg-muted hover:bg-surface-elevated hover:text-fg"
        title="Remove from queue"
        aria-label="Remove"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <path d="M18 6 6 18M6 6l12 12" />
        </svg>
      </button>
    </div>

    <!-- Progress bar + size -->
    {#if row.state === 'uploading' || row.state === 'asset-creating' || row.state === 'queued'}
      <div class="h-1.5 w-full overflow-hidden rounded-full bg-surface-elevated">
        <div
          class="h-full bg-accent transition-[width] duration-150"
          style="width: {row.state === 'queued' ? 0 : row.state === 'asset-creating' ? 100 : pct}%"
        ></div>
      </div>
    {/if}

    <div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-muted">
      <span>{humanSize(row.file.size)}</span>
      {#if row.deduped}
        <span class="text-emerald-600 dark:text-emerald-400">· Dedup'd from existing bytes</span>
      {/if}
      {#if row.error}
        <span class="text-red-600 dark:text-red-400">· {row.error}</span>
        <button
          type="button"
          onclick={() => upload.retryRow(row.id)}
          class="rounded border border-border px-2 py-0.5 text-xs hover:bg-surface-elevated"
        >
          Retry
        </button>
      {/if}
    </div>

    <!-- Tags (chip input). Collapsible feel via small footprint;
         per-asset tags are optional. -->
    <div class="flex flex-wrap items-center gap-1.5">
      {#each row.tags as tag (tag)}
        <span class="inline-flex items-center gap-1 rounded-full bg-surface-elevated px-2 py-0.5 text-xs text-fg">
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
        placeholder={row.tags.length === 0 ? 'Add tag…' : '+'}
        class="min-w-[6rem] flex-1 rounded border border-transparent bg-transparent px-1.5 py-0.5 text-xs text-fg placeholder:text-fg-muted/60 focus:border-border focus:bg-surface-elevated focus:outline-none"
      />
    </div>
  </div>
</div>
