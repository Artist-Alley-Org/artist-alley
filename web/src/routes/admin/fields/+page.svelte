<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import ExtractionConfigPicker from '$components/ExtractionConfigPicker.svelte';

  interface Field {
    id: string;
    code: string;
    label: string;
    type: string;
    subject_kind: 'asset' | 'collection';
    required: boolean;
    applies_to: number[];
    display_group?: string;
    extraction_source?: string;
    extraction_mode?: string;
  }

  // Per-row toggle for the extraction picker. Keyed by field id.
  let expanded = $state<Record<string, boolean>>({});
  function toggleExtraction(id: string) { expanded[id] = !expanded[id]; }

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

  async function load() {
    loading = true;
    try {
      const query: Record<string, string> = { status: 'active' };
      if (subjectFilter !== 'all') query.subject_kind = subjectFilter;
      const { data } = await api.GET('/fields', { params: { query } });
      fields = (data ?? []) as Field[];
    } finally {
      loading = false;
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
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 font-mono text-sm focus-visible:border-border-strong focus:outline-none"
      />
    </label>
    <label class="block">
      <span class="text-xs text-fg-muted">{t('admin.fields.label')}</span>
      <input
        type="text"
        bind:value={newLabel}
        required
        data-testid="admin-fields-create-label"
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
      />
    </label>
    <label class="block">
      <span class="text-xs text-fg-muted">{t('admin.fields.type')}</span>
      <select
        bind:value={newType}
        data-testid="admin-fields-create-type"
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
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
        class="h-4 w-4 rounded border-border"
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
  <table class="w-full text-sm">
    <thead class="text-left text-xs uppercase tracking-wider text-fg-muted">
      <tr>
        <th class="py-2">{t('admin.fields.code')}</th>
        <th class="py-2">{t('admin.fields.label')}</th>
        <th class="py-2">{t('admin.fields.type')}</th>
        <th class="py-2">{t('admin.fields.subject_kind')}</th>
        <th class="py-2">{t('admin.fields.applies_to')}</th>
        <th class="py-2">{t('admin.fields.group')}</th>
        <th class="py-2">{t('admin.fields.extraction')}</th>
      </tr>
    </thead>
    <tbody>
      {#each fields as f (f.id)}
        <tr class="border-t border-border" data-testid="admin-fields-row-{f.code}">
          <td class="py-2 font-mono text-xs">{f.code}</td>
          <td class="py-2">{f.label}</td>
          <td class="py-2 text-fg-muted">{f.type}</td>
          <td class="py-2 text-fg-muted">
            {f.subject_kind === 'collection' ? t('admin.fields.subject_collection') : t('admin.fields.subject_asset')}
          </td>
          <td class="py-2 text-fg-muted">{f.applies_to?.length ? f.applies_to.join(', ') : 'all'}</td>
          <td class="py-2 text-fg-muted">{f.display_group ?? ''}</td>
          <td class="py-2">
            <button
              type="button"
              onclick={() => toggleExtraction(f.id)}
              class="rounded border border-border px-2 py-0.5 text-xs text-fg-muted hover:bg-state-hover"
              data-testid="admin-fields-extraction-toggle-{f.code}"
            >
              {#if f.extraction_source}
                <code class="text-xs">{f.extraction_source}</code> · {f.extraction_mode || 'skip_if_set'}
              {:else}
                — wire —
              {/if}
            </button>
          </td>
        </tr>
        {#if expanded[f.id]}
          <tr class="border-t border-border/30 bg-bg-soft/40">
            <td class="px-2 py-2" colspan="7">
              <ExtractionConfigPicker
                fieldId={f.id}
                initialSource={f.extraction_source ?? ''}
                initialMode={f.extraction_mode ?? ''}
                onSaved={() => load()}
              />
            </td>
          </tr>
        {/if}
      {/each}
    </tbody>
  </table>
{/if}
