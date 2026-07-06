<script lang="ts">
  // Thumbs up/down buttons for a single search-result hit (Phase
  // 1.16.B-5-followup, closes #184).
  //
  // Optimistic UI: click updates the local state instantly, fires the
  // POST in the background; on error, reverts + surfaces a toast.
  // Debounced 300ms so double-clicks don't produce duplicate rows
  // (the backend upsert would flip the direction back to the current
  // state on the second call — visually a no-op but wastes an
  // enqueue).
  //
  // Anonymous callers see no buttons — the auth store's `user`
  // property is the gate. The endpoint would 401 anyway, but hiding
  // the buttons keeps the UX clean.

  import { auth } from '$stores/auth.svelte';

  interface Props {
    dsl: string;
    hitAssetId: string;
    hitPosition: number; // 1-indexed at time of render
  }

  const { dsl, hitAssetId, hitPosition }: Props = $props();

  type Direction = 'up' | 'down' | null;

  let direction = $state<Direction>(null);
  let lastRowId = $state<string | null>(null);
  let submitting = $state(false);
  let debounceHandle: ReturnType<typeof setTimeout> | null = null;
  let errorMessage = $state('');
  let undoToastVisible = $state(false);
  let undoToastHandle: ReturnType<typeof setTimeout> | null = null;

  const isAnonymous = $derived(!auth.user);

  async function vote(next: 'up' | 'down') {
    if (submitting) return;
    if (debounceHandle) return;
    debounceHandle = setTimeout(() => {
      debounceHandle = null;
    }, 300);

    const previous = direction;
    direction = next; // optimistic
    submitting = true;
    errorMessage = '';
    try {
      const resp = await fetch('/api/v1/search/feedback', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({
          dsl,
          hit_asset_id: hitAssetId,
          hit_position: hitPosition,
          direction: next,
        }),
      });
      if (!resp.ok) {
        direction = previous; // revert
        const body = await resp.json().catch(() => ({}));
        if (resp.status === 429) errorMessage = 'Daily feedback limit reached.';
        else if (resp.status === 503) errorMessage = 'Feedback is disabled.';
        else if (resp.status === 401) errorMessage = 'Sign in to leave feedback.';
        else if (resp.status === 403) errorMessage = "You can't leave feedback on this result.";
        else errorMessage = `Feedback failed (${resp.status})`;
        return;
      }
      const data = await resp.json();
      lastRowId = data.id ?? null;
      showUndoToast();
    } catch {
      direction = previous;
      errorMessage = 'Network error — try again.';
    } finally {
      submitting = false;
    }
  }

  async function undo() {
    if (!lastRowId) return;
    if (undoToastHandle) {
      clearTimeout(undoToastHandle);
      undoToastHandle = null;
    }
    undoToastVisible = false;
    const rowID = lastRowId;
    const previous = direction;
    direction = null; // optimistic
    lastRowId = null;
    try {
      const resp = await fetch(`/api/v1/search/feedback/${rowID}`, {
        method: 'DELETE',
        credentials: 'include',
      });
      if (!resp.ok && resp.status !== 404) {
        direction = previous;
        lastRowId = rowID;
        errorMessage = `Undo failed (${resp.status})`;
      }
    } catch {
      direction = previous;
      lastRowId = rowID;
      errorMessage = 'Network error — try again.';
    }
  }

  function showUndoToast() {
    undoToastVisible = true;
    if (undoToastHandle) clearTimeout(undoToastHandle);
    undoToastHandle = setTimeout(() => {
      undoToastVisible = false;
    }, 5000);
  }
</script>

{#if !isAnonymous}
  <div class="inline-flex items-center gap-1" data-testid="thumb-buttons">
    <button
      type="button"
      onclick={() => vote('up')}
      disabled={submitting}
      class="rounded p-1 text-fg-muted transition-colors hover:bg-bg-soft hover:text-fg disabled:opacity-50 {direction === 'up' ? 'text-success' : ''}"
      title="Good result"
      aria-label="Thumbs up"
      aria-pressed={direction === 'up'}
      data-testid="thumb-up"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill={direction === 'up' ? 'currentColor' : 'none'} stroke="currentColor" stroke-width="2">
        <path d="M7 10v12M15 5.88 14 10h5.83a2 2 0 0 1 1.92 2.56l-2.33 8A2 2 0 0 1 17.5 22H7V10c0-.83.4-1.61 1.07-2.09l4.14-2.95A1.5 1.5 0 0 1 15 5.88z"/>
      </svg>
    </button>
    <button
      type="button"
      onclick={() => vote('down')}
      disabled={submitting}
      class="rounded p-1 text-fg-muted transition-colors hover:bg-bg-soft hover:text-fg disabled:opacity-50 {direction === 'down' ? 'text-danger' : ''}"
      title="Bad result"
      aria-label="Thumbs down"
      aria-pressed={direction === 'down'}
      data-testid="thumb-down"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill={direction === 'down' ? 'currentColor' : 'none'} stroke="currentColor" stroke-width="2">
        <path d="M17 14V2M9 18.12 10 14H4.17a2 2 0 0 1-1.92-2.56l2.33-8A2 2 0 0 1 6.5 2H17v12c0 .83-.4 1.61-1.07 2.09l-4.14 2.95A1.5 1.5 0 0 1 9 18.12z"/>
      </svg>
    </button>
    {#if undoToastVisible}
      <button
        type="button"
        onclick={undo}
        class="ml-1 text-xs text-accent underline hover:no-underline"
        data-testid="undo-feedback"
      >Undo</button>
    {/if}
    {#if errorMessage}
      <span class="ml-1 text-xs text-danger" title={errorMessage}>!</span>
    {/if}
  </div>
{/if}
