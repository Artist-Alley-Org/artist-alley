<script lang="ts">
  // Edit existing collection metadata. Wraps PATCH /collections/{id}.

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import CollectionModal from './CollectionModal.svelte';
  import CollectionFieldsSection from './CollectionFieldsSection.svelte';

  interface Collection {
    id: string;
    name: string;
    description: string;
    visibility: string;
    featured: boolean;
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
  let featured = $state(false);
  let submitting = $state(false);
  let error = $state<string | null>(null);

  $effect(() => {
    if (open) {
      name = collection.name;
      description = collection.description;
      visibility = collection.visibility as 'private' | 'org-only' | 'followers' | 'explicit-share';
      featured = collection.featured;
      error = null;
    }
  });

  async function submit() {
    if (!name.trim() || submitting) return;
    submitting = true;
    error = null;
    try {
      const { data, error: apiErr } = await api.PATCH('/collections/{id}', {
        params: { path: { id: collection.id } },
        body: {
          name: name.trim(),
          description,
          visibility,
          featured,
        },
      });
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
</script>

<CollectionModal title={t('collections.edit_title')} {open} {onclose}>
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
        class="w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
      />
    </label>
    <label class="block">
      <span class="mb-1 block text-xs font-medium text-fg-muted">{t('collections.description')}</span>
      <textarea
        bind:value={description}
        rows="3"
        maxlength="2000"
        class="w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
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
    <label class="flex items-center gap-2 text-sm">
      <input type="checkbox" bind:checked={featured} class="h-4 w-4 rounded border-border" />
      <span>{t('collections.featured_flag')}</span>
    </label>

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
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {submitting ? t('collections.saving') : t('common.save')}
    </button>
  {/snippet}
</CollectionModal>
