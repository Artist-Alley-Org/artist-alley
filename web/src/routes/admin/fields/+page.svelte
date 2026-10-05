<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The fields INDEX (#854).
  //
  // This page used to be the whole feature: nine columns, and three
  // separate editors expanded inside its own cells. The owner's call
  // on 2026-08-02 was that it is too cramped, and each addition to the
  // fields arc made the cell denser. So editing moved to
  // /admin/fields/{code} and what is left here is what an index is
  // for — scanning, filtering, and getting to the one you want.
  //
  // Five columns, and every one of them answers a question you ask
  // WHILE scanning. `applies_to`, `display_group`, the extraction
  // wiring and the default summary all answer questions you ask about
  // ONE field, so they live on that field's page.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface Field {
    id: string;
    code: string;
    label: string;
    type: string;
    subject_kind: 'asset' | 'collection';
    required: boolean;
    show_on_card?: boolean;
    show_in_advanced_search?: boolean;
    show_on_upload?: boolean;
    mirrors_column?: string | null;
    updated_at: string;
  }

  let fields = $state<Field[]>([]);
  let loading = $state(true);
  let subjectFilter = $state<'all' | 'asset' | 'collection'>('all');

  // Create form state.
  let showCreate = $state(false);
  let creating = $state(false);
  let createError = $state<string | null>(null);
  let newCode = $state('');
  let newLabel = $state('');
  let newType = $state<Field['type']>('text');
  let newSubjectKind = $state<'asset' | 'collection'>('asset');
  let newRequired = $state(false);

  const TYPES = ['text', 'longtext', 'rich_text', 'number', 'boolean', 'date', 'datetime', 'select', 'multi_select', 'tree', 'reference'] as const;

  onMount(() => void load());

  async function load(silent = false) {
    if (!silent) loading = true;
    try {
      const query: Record<string, string> = { status: 'active' };
      if (subjectFilter !== 'all') query.subject_kind = subjectFilter;
      const { data } = await api.GET('/fields', { params: { query } });
      fields = (data ?? []) as Field[];
    } finally {
      if (!silent) loading = false;
    }
  }

  async function reloadOnFilterChange(next: 'all' | 'asset' | 'collection') {
    subjectFilter = next;
    await load();
  }

  async function createField(e: Event) {
    e.preventDefault();
    if (creating) return;
    creating = true;
    createError = null;
    try {
      const { data, error: apiErr } = await api.POST('/fields', {
        body: {
          code: newCode.trim(),
          label: newLabel.trim(),
          type: newType,
          subject_kind: newSubjectKind,
          required: newRequired,
        } as never,
      });
      if (apiErr || !data) {
        createError = (apiErr as { error?: string } | undefined)?.error ?? t('admin.fields.create_error');
        return;
      }
      showCreate = false;
      newCode = '';
      newLabel = '';
      newType = 'text';
      newSubjectKind = 'asset';
      newRequired = false;
      await load();
    } finally {
      creating = false;
    }
  }
</script>

<svelte:head><title>{t('admin.fields.title')} — {site.name}</title></svelte:head>

<div class="mb-4 flex items-center justify-between gap-4">
  <h2 class="text-xl font-semibold">{t('admin.fields.title')}</h2>
  <button
    type="button"
    onclick={() => (showCreate = !showCreate)}
    data-testid="admin-fields-create-button"
    class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent"
  >
    {t('admin.fields.create_button')}
  </button>
</div>

<!-- subject_kind filter chips -->
<div class="mb-4 flex flex-wrap items-center gap-2 text-sm">
  <span class="text-fg-muted">{t('admin.fields.subject_filter_label')}:</span>
  {#each [{ k: 'all' as const, label: t('admin.fields.subject_filter_all') }, { k: 'asset' as const, label: t('admin.fields.subject_filter_asset') }, { k: 'collection' as const, label: t('admin.fields.subject_filter_collection') }] as chip (chip.k)}
    <button
      type="button"
      onclick={() => reloadOnFilterChange(chip.k)}
      data-testid="admin-fields-filter-{chip.k}"
      class="rounded-full border px-3 py-1 text-xs transition-colors"
      class:border-accent={subjectFilter === chip.k}
      class:bg-accent={subjectFilter === chip.k}
      class:text-white={subjectFilter === chip.k}
      class:border-border={subjectFilter !== chip.k}
      class:bg-surface={subjectFilter !== chip.k}
    >
      {chip.label}
    </button>
  {/each}
</div>

{#if showCreate}
  <form
    onsubmit={createField}
    data-testid="admin-fields-create-form"
    class="mb-4 max-w-2xl space-y-3 rounded border border-border bg-surface p-4"
  >
    <h3 class="text-sm font-semibold">{t('admin.fields.create_title')}</h3>
    <label class="block">
      <span class="text-xs text-fg-muted">{t('admin.fields.code')}</span>
      <input
        type="text"
        bind:value={newCode}
        placeholder={t('admin.fields.code_placeholder')}
        pattern="^[a-z][a-z0-9_]*$"
        required
        data-testid="admin-fields-create-code"
        class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 font-mono text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>
    <label class="block">
      <span class="text-xs text-fg-muted">{t('admin.fields.label')}</span>
      <input
        type="text"
        bind:value={newLabel}
        required
        data-testid="admin-fields-create-label"
        class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>
    <label class="block">
      <span class="text-xs text-fg-muted">{t('admin.fields.type')}</span>
      <select
        bind:value={newType}
        data-testid="admin-fields-create-type"
        class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      >
        {#each TYPES as opt (opt)}
          <option value={opt}>{opt}</option>
        {/each}
      </select>
    </label>
    <fieldset>
      <legend class="text-xs text-fg-muted">{t('admin.fields.subject_kind')}</legend>
      <p class="mt-1 text-xs text-fg-muted">{t('admin.fields.create_help_subject')}</p>
      <div class="mt-2 grid grid-cols-2 gap-2">
        {#each ['asset', 'collection'] as kind (kind)}
          <label class="cursor-pointer rounded border bg-surface px-3 py-2 text-center text-sm"
                 class:border-accent={newSubjectKind === kind}
                 class:text-accent={newSubjectKind === kind}
                 class:border-border={newSubjectKind !== kind}>
            <input
              type="radio"
              name="subject_kind"
              value={kind}
              bind:group={newSubjectKind}
              data-testid="admin-fields-create-subject-{kind}"
              class="sr-only"
            />
            {kind === 'asset' ? t('admin.fields.subject_asset') : t('admin.fields.subject_collection')}
          </label>
        {/each}
      </div>
    </fieldset>
    <label class="flex items-center gap-2 text-sm">
      <input
        type="checkbox"
        bind:checked={newRequired}
        data-testid="admin-fields-create-required"
        class="h-4 w-4 rounded border-border-strong"
      />
      <span>{t('admin.fields.create_help_required')}</span>
    </label>
    {#if createError}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{createError}</p>
    {/if}
    <div class="flex gap-2">
      <button
        type="button"
        onclick={() => (showCreate = false)}
        class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm"
      >{t('common.cancel')}</button>
      <button
        type="submit"
        disabled={creating}
        data-testid="admin-fields-create-submit"
        class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
      >{creating ? t('common.loading') : t('admin.fields.create_submit')}</button>
    </div>
  </form>
{/if}

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if fields.length === 0}
  <p class="rounded-md bg-surface-elevated px-4 py-6 text-center text-fg-muted">{t('admin.fields.no_fields')}</p>
{:else}
  <!--
    Five columns at a desk, three on a phone. A phone REDUCES rather
    than shrinks: `code` folds under the label (where it is still
    readable and still copyable) and `subject` goes, because the two
    filter chips above already answer "asset or collection" and the
    field's own page answers it again. Nothing lands off-screen behind
    a sideways scroll nobody discovers — the failure the old
    nine-column row had at this width.
  -->
  <table class="w-full table-fixed text-sm">
    <thead class="text-left text-xs uppercase tracking-wider text-fg-muted">
      <tr>
        <th class="py-2">{t('admin.fields.label')}</th>
        <th class="hidden py-2 sm:table-cell">{t('admin.fields.code')}</th>
        <th class="py-2">{t('admin.fields.type')}</th>
        <th class="hidden py-2 sm:table-cell">{t('admin.fields.subject_kind')}</th>
        <th class="py-2">{t('admin.fields.flags')}</th>
      </tr>
    </thead>
    <tbody>
      {#each fields as f (f.id)}
        <tr class="border-t border-border align-top" data-testid="admin-fields-row-{f.code}">
          <td class="py-2 pr-2">
            <!-- The whole row's job is to get you to the page. A real
                 <a>, so middle-click, ⌘-click and "copy link" all work
                 — the point of the route is that a field is somewhere
                 you can SEND someone. -->
            <a
              href="/admin/fields/{f.code}"
              data-testid="admin-fields-open-{f.code}"
              class="block break-words font-medium text-accent hover:underline"
            >{f.label}</a>
            <span class="block break-all font-mono text-xs text-fg-muted sm:hidden">{f.code}</span>
          </td>
          <td class="hidden break-all py-2 pr-2 font-mono text-xs text-fg-muted sm:table-cell">{f.code}</td>
          <td class="break-words py-2 pr-2 text-fg-muted">{f.type}</td>
          <td class="hidden py-2 pr-2 text-fg-muted sm:table-cell">
            {f.subject_kind === 'collection' ? t('admin.fields.subject_collection') : t('admin.fields.subject_asset')}
          </td>
          <td class="py-2">
            <span class="flex flex-wrap gap-1 text-[10px] uppercase tracking-wider">
              {#if f.required}
                <span class="rounded border border-border bg-surface px-1.5 py-0.5 text-fg-muted"
                >{t('admin.fields.edit_required')}</span>
              {/if}
              {#if f.show_on_card}
                <span
                  data-testid="admin-fields-card-badge-{f.code}"
                  class="rounded border border-border bg-surface px-1.5 py-0.5 text-fg-muted"
                >{t('admin.fields.flag_on_card')}</span>
              {/if}
              <!--
                Participation badges mark the EXCEPTION, never the rule
                (#1173). Both flags default on, so a badge per enabled
                field would put two badges on every row of a 200-row
                table and say nothing. What an operator scans this list
                for is "which ones did I turn off", so only `false`
                renders.
              -->
              {#if f.show_in_advanced_search === false}
                <span
                  data-testid="admin-fields-no-advanced-badge-{f.code}"
                  class="rounded border border-border bg-surface px-1.5 py-0.5 text-fg-muted"
                >{t('admin.fields.flag_not_in_advanced_search')}</span>
              {/if}
              {#if f.show_on_upload === false}
                <span
                  data-testid="admin-fields-no-upload-badge-{f.code}"
                  class="rounded border border-border bg-surface px-1.5 py-0.5 text-fg-muted"
                >{t('admin.fields.flag_not_on_upload')}</span>
              {/if}
              {#if f.mirrors_column}
                <span
                  data-testid="admin-fields-mirror-badge-{f.code}"
                  class="rounded border border-accent/40 bg-accent/10 px-1.5 py-0.5 text-accent"
                >{t('admin.field_detail.mirror_badge')}</span>
              {/if}
            </span>
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
{/if}
