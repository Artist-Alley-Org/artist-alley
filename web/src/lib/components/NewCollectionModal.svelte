<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // New-collection composer. Wraps POST /collections. Re-used by
  // both the hub header and the "New" entry inside any user menu.

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import Modal from './Modal.svelte';

  interface CreatedCollection {
    id: string;
    name: string;
    description: string;
    visibility: string;
    owner_user_ref: number;
    created_at: string;
  }

  interface Props {
    open: boolean;
    onclose: () => void;
    oncreate?: (c: CreatedCollection) => void;
  }

  let { open, onclose, oncreate }: Props = $props();

  // #914 — the composer no longer asks for visibility. `CollectionCreate`
  // is `required: [name]` and defaults `visibility` to `private`, which
  // is what this fieldset was pre-selecting anyway: four buttons whose
  // only job was to let you re-pick the value the server would have
  // chosen. `private` is the safe direction to be wrong in, and the
  // choice is fully editable straight afterwards from EditCollectionModal
  // — so the question belongs where the answer can be informed by an
  // existing collection, not in front of an empty one.
  let name = $state('');
  let description = $state('');
  let submitting = $state(false);
  let error = $state<string | null>(null);

  function reset() {
    name = '';
    description = '';
    error = null;
  }

  async function submit() {
    if (!name.trim() || submitting) return;
    submitting = true;
    error = null;
    try {
      const { data, error: apiErr } = await api.POST('/collections', {
        body: {
          name: name.trim(),
          description: description.trim(),
          // Both of these restate a server default rather than carrying
          // a user's answer, and neither is asked for any more.
          // openapi-typescript makes a property with a `default` NON-
          // optional in the generated body type (`--default-non-nullable`,
          // on by default), so `CollectionCreate`'s `required: [name]`
          // does not survive into TypeScript and omitting them will not
          // compile. Sending the server's own default is byte-identical
          // to omitting it — `CreateCollection` reads
          // `if in.Visibility != nil` and falls back to exactly this.
          visibility: 'private',
          membership: 'manual',
        },
      });
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('collections.error_create');
        return;
      }
      oncreate?.(data as CreatedCollection);
      reset();
      onclose();
    } finally {
      submitting = false;
    }
  }
</script>

<Modal title={t('collections.new_title')} {open} {onclose}>
  <div class="space-y-3">
    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
        {error}
      </p>
    {/if}
    <label class="block">
      <span class="mb-1 block text-xs font-medium text-fg-muted">{t('collections.name')}</span>
      <input
        type="text"
        bind:value={name}
        maxlength="200"
        class="w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>
    <label class="block">
      <span class="mb-1 block text-xs font-medium text-fg-muted">{t('collections.description')}</span>
      <textarea
        bind:value={description}
        rows="3"
        maxlength="2000"
        class="w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      ></textarea>
    </label>
    <!-- Say what you get instead of asking for it (#914). Dropping the
         fieldset without this would silently change the answer for
         anyone who used to pick. -->
    <p class="text-xs text-fg-muted" data-testid="new-collection-visibility-note">
      {t('collections.new_visibility_note')}
    </p>
  </div>

  {#snippet footer()}
    <button
      type="button"
      onclick={() => { reset(); onclose(); }}
      class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated"
    >
      {t('common.cancel')}
    </button>
    <button
      type="button"
      onclick={submit}
      disabled={!name.trim() || submitting}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {submitting ? t('collections.creating') : t('collections.create')}
    </button>
  {/snippet}
</Modal>
