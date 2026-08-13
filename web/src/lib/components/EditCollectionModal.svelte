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
    // #1027 — the curator's CHOSEN cover, or null for the derived
    // mosaic. Read off the collection rather than off `covers`: the two
    // answer different questions, and `covers` deliberately falls back
    // to the mosaic for a reader who may not picture the chosen asset,
    // so a picker seeded from it would show the wrong thing to exactly
    // the people allowed to change it.
    cover_asset_id?: string | null;
  }

  // One choosable picture in the cover picker.
  //
  // CollectionResource joins the asset's columns in FLAT, not nested
  // under an `asset` object — and on a member this reader may not see,
  // #883 makes every one of them ABSENT rather than null, with
  // `restricted` as the flag saying which shape the row is. So both
  // fields below are optional and both have to be checked.
  interface CoverChoice {
    asset_id: string;
    restricted?: boolean;
    preview_available?: boolean;
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

  // #1027 — cover choice. `null` means "use the derived mosaic", which
  // is a real selection the curator can make, not merely the absence of
  // one, so it is a nullable value rather than an undefined.
  let coverAssetId = $state<string | null>(null);
  let coverChoices = $state<CoverChoice[]>([]);
  let coverLoading = $state(false);

  $effect(() => {
    if (open) {
      name = collection.name;
      description = collection.description;
      visibility = collection.visibility as 'private' | 'org-only' | 'followers' | 'explicit-share';
      baselineUpdatedAt = collection.updated_at;
      coverAssetId = collection.cover_asset_id ?? null;
      error = null;
      conflict = null;
      void loadCoverChoices(collection.id);
    }
  });

  // The members are the picker's options. The API accepts ANY asset the
  // curator may picture — that is what lets a cover outlive the member
  // it was chosen from — but the members are the set that can be shown
  // as pictures without inventing a whole asset browser in a modal, and
  // "upload a banner, add it, pick it" reaches the rest.
  //
  // A member this reader may not picture is dropped rather than shown
  // disabled: the server would refuse it, and offering a control that
  // cannot succeed is worse than not offering it.
  async function loadCoverChoices(id: string) {
    coverLoading = true;
    try {
      const { data } = await api.GET('/collections/{id}/resources', {
        params: { path: { id }, query: { limit: 60 } },
      });
      coverChoices = ((data?.items ?? []) as unknown as CoverChoice[]).filter(
        (m) => !m.restricted && m.preview_available === true,
      );
    } catch {
      // A picker that failed to load must not block renaming the
      // collection, so this is not surfaced as a form error.
      coverChoices = [];
    } finally {
      coverLoading = false;
    }
  }

  // A cover chosen from outside the member list still has to be shown as
  // the current selection, or saving an unrelated edit would silently
  // look like it cleared the cover.
  const coverIsExternal = $derived(
    coverAssetId !== null && !coverChoices.some((c) => c.asset_id === coverAssetId),
  );

  function coverUrl(assetId: string) {
    return `/api/v1/assets/${assetId}/variants/col`;
  }

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
          // The tri-state, spelled out. `cover_asset_id: null` cannot
          // mean "remove" on a partial update — null is already "leave
          // alone" for every other property — so removal is the
          // companion flag, exactly as `clear_default` works on a field
          // definition. Sending BOTH is a 400, so these are exclusive
          // branches rather than two independent keys.
          ...(coverAssetId === null
            ? collection.cover_asset_id
              ? { clear_cover: true }
              : {}
            : { cover_asset_id: coverAssetId }),
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
    <fieldset>
      <legend class="mb-1 block text-xs font-medium text-fg-muted">{t('collections.cover')}</legend>
      <p class="mb-2 text-xs text-fg-muted">{t('collections.cover_hint')}</p>
      {#if coverLoading}
        <p class="text-xs text-fg-muted">{t('collections.cover_loading')}</p>
      {:else if coverChoices.length === 0 && !coverIsExternal}
        <p class="text-xs text-fg-muted">{t('collections.cover_none')}</p>
      {:else}
        <!-- The choices SCROLL inside a fixed height. A collection can
             hold hundreds of members, and letting the grid size itself
             pushes the modal's Save button below the fold — the control
             the whole form exists to reach. Its own scroll container
             keeps the footer where it was for a two-member collection
             and a two-hundred-member one alike. -->
        <div class="grid max-h-56 grid-cols-3 gap-2 overflow-y-auto rounded border border-border p-1 sm:grid-cols-5">
          <!-- "Use mosaic" is a CHOICE in the same grid as the pictures,
               not a clear button off to one side: reverting to the
               derived cover is what the collection does by default, so
               it reads as one of the options rather than an undo. -->
          <button
            type="button"
            onclick={() => (coverAssetId = null)}
            aria-pressed={coverAssetId === null}
            class="flex aspect-square flex-col items-center justify-center rounded border-2 bg-surface p-1 text-center text-[10px] leading-tight text-fg-muted hover:border-border-strong"
            class:border-accent={coverAssetId === null}
            class:border-border={coverAssetId !== null}
          >
            <span class="font-medium">{t('collections.cover_derived')}</span>
            <span class="mt-0.5 opacity-70">{t('collections.cover_derived_hint')}</span>
          </button>

          {#if coverIsExternal && coverAssetId}
            <button
              type="button"
              aria-pressed="true"
              title={t('collections.cover_current_external')}
              class="relative aspect-square overflow-hidden rounded border-2 border-accent"
            >
              <img
                src={coverUrl(coverAssetId)}
                alt={t('collections.cover_current_external')}
                loading="lazy"
                class="h-full w-full object-cover"
              />
            </button>
          {/if}

          {#each coverChoices as choice (choice.asset_id)}
            <button
              type="button"
              onclick={() => (coverAssetId = choice.asset_id)}
              aria-pressed={coverAssetId === choice.asset_id}
              class="relative aspect-square overflow-hidden rounded border-2 hover:border-border-strong"
              class:border-accent={coverAssetId === choice.asset_id}
              class:border-border={coverAssetId !== choice.asset_id}
            >
              <img
                src={coverUrl(choice.asset_id)}
                alt=""
                loading="lazy"
                class="h-full w-full object-cover"
              />
            </button>
          {/each}
        </div>
      {/if}
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
