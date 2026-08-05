<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Edit existing collection metadata. Wraps PATCH /collections/{id}.

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import Modal from './Modal.svelte';
  import CollectionFieldsSection from './CollectionFieldsSection.svelte';

  interface Collection {
    id: string;
    name: string;
    description: string;
    visibility: string;
    owner_user_ref: number;
    created_at: string;
    updated_at: string;
  }

  interface Props {
    open: boolean;
    collection: Collection;
    onclose: () => void;
    onsaved?: (c: Collection) => void;
  }

  let { open, collection, onclose, onsaved }: Props = $props();

  let name = $state('');
  let description = $state('');
  let visibility = $state<'private' | 'org-only' | 'followers' | 'explicit-share'>('private');
  let submitting = $state(false);
  let error = $state<string | null>(null);
  // Phase 1.16 edit-safety: capture updated_at on open so the
  // PATCH can detect "someone else edited this while my modal
  // was open" + show a reload-prompt instead of silently
  // clobbering.
  let baselineUpdatedAt = $state<string>('');
  let conflict = $state<{ updatedAt: string } | null>(null);

  $effect(() => {
    if (open) {
      name = collection.name;
      description = collection.description;
      visibility = collection.visibility as 'private' | 'org-only' | 'followers' | 'explicit-share';
      baselineUpdatedAt = collection.updated_at;
      error = null;
      conflict = null;
    }
  });

  async function submit() {
    if (!name.trim() || submitting) return;
    submitting = true;
    error = null;
    try {
      const { data, error: apiErr, response } = await api.PATCH('/collections/{id}', {
        params: { path: { id: collection.id } },
        body: {
          name: name.trim(),
          description,
          visibility,
          if_unchanged_since: baselineUpdatedAt || undefined,
        },
      });
      if (response.status === 409) {
        const c = apiErr as { error?: string; updated_at?: string } | undefined;
        conflict = { updatedAt: c?.updated_at ?? '' };
        return;
      }
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('collections.error_save');
        return;
      }
      onsaved?.(data as Collection);
      onclose();
    } finally {
      submitting = false;
    }
  }

  // Operator clicked "reload + keep my edits" — refresh the
  // baseline timestamp so the next submit doesn't 409 again.
  // The edited form values stay in-place so the operator can
  // choose to keep them, merge manually, or hit Cancel.
  function acknowledgeConflict() {
    if (conflict) {
      baselineUpdatedAt = conflict.updatedAt;
      conflict = null;
    }
  }
</script>

<Modal title={t('collections.edit_title')} {open} {onclose}>
  <div class="space-y-3">
    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
        {error}
      </p>
    {/if}
    {#if conflict}
      <div role="alert" class="rounded border border-warning/40 bg-warning/10 px-3 py-2 text-sm">
        <p class="font-medium text-warning">{t('collections.conflict_heading')}</p>
        <p class="mt-1 text-xs text-fg-muted">{t('collections.conflict_body')}</p>
        <button
          type="button"
          onclick={acknowledgeConflict}
          class="mt-2 rounded border border-warning/60 px-2 py-1 text-xs font-medium text-warning hover:bg-warning/20"
        >{t('collections.conflict_overwrite')}</button>
      </div>
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
    <fieldset>
      <legend class="mb-1 block text-xs font-medium text-fg-muted">{t('collections.visibility')}</legend>
      <div class="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {#each ['private', 'org-only', 'followers', 'explicit-share'] as v}
          <label class="cursor-pointer rounded border border-border bg-surface px-3 py-2 text-center text-sm hover:border-border-strong"
                 class:border-accent={visibility === v}
                 class:text-accent={visibility === v}>
            <input type="radio" name="vis_edit" value={v} bind:group={visibility} class="sr-only" />
            {t(`collections.vis_${v.replace('-', '_')}`)}
          </label>
        {/each}
      </div>
    </fieldset>
    <CollectionFieldsSection collectionId={collection.id} />
  </div>

  {#snippet footer()}
    <button
      type="button"
      onclick={onclose}
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
      {submitting ? t('collections.saving') : t('common.save')}
    </button>
  {/snippet}
</Modal>
