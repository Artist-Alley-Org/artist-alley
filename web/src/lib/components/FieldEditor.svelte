<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<!--
  Edits one field definition — its label, whether it is required, and
  (for select / multi_select) its controlled vocabulary. ADR 0012 plus
  its 2026-07-30 amendment.

  Deletion is deliberately absent: hard-deleting an option orphans the
  values assets already store, and the orphan surfaces as a blank on an
  asset nobody edited. Retire a term with `deprecated` (stops being
  offered, keeps resolving) or `archived` (hard retire) instead.

  The save is conflict-detectable — it sends the `updated_at` the row
  was loaded with as `if_unchanged_since` and surfaces a 409 rather
  than silently retrying, because a silent retry is the clobber this
  guard exists to prevent.
-->
<script lang="ts">
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import {
    normalizeOptions,
    serializeOptions,
    type FieldOption,
    type OptionStatus,
  } from '$lib/fieldOptions';

  let {
    fieldId,
    fieldType,
    initialLabel,
    initialRequired,
    initialOptions,
    initialUpdatedAt,
    onSaved = () => {},
  }: {
    fieldId: string;
    fieldType: string;
    initialLabel: string;
    initialRequired: boolean;
    initialOptions: Record<string, unknown> | undefined;
    initialUpdatedAt: string;
    onSaved?: () => void;
  } = $props();

  const STATUSES: OptionStatus[] = ['active', 'deprecated', 'archived'];
  const hasVocabulary = fieldType === 'select' || fieldType === 'multi_select';

  let label = $state(initialLabel);
  let required = $state(initialRequired);
  let opts = $state<FieldOption[]>(normalizeOptions(initialOptions));
  // Baseline for the optimistic-concurrency guard. Re-based (not
  // reset) after a save so consecutive edits keep working.
  let baseline = $state(initialUpdatedAt);
  let saving = $state(false);
  let error = $state('');
  let savedMsg = $state('');
  let conflict = $state(false);
  let newSlug = $state('');

  let snapshot = $state(JSON.stringify(serializeOptions(normalizeOptions(initialOptions))));
  let labelSnapshot = $state(initialLabel);
  let requiredSnapshot = $state(initialRequired);
  const dirty = $derived(
    JSON.stringify(serializeOptions(opts)) !== snapshot ||
      label !== labelSnapshot ||
      required !== requiredSnapshot,
  );

  // Only active siblings make sense as a successor — pointing a
  // deprecation at another deprecation just moves the problem.
  const successorChoices = $derived(opts.filter((o) => o.status === 'active'));

  function addOption() {
    const slug = newSlug.trim();
    if (!slug) return;
    if (opts.some((o) => o.value === slug)) {
      error = t('admin.fields.options_duplicate', { slug });
      return;
    }
    opts = [...opts, { value: slug, label: slug, status: 'active' }];
    newSlug = '';
    error = '';
  }

  function move(i: number, delta: number) {
    const j = i + delta;
    if (j < 0 || j >= opts.length) return;
    const next = [...opts];
    [next[i], next[j]] = [next[j], next[i]];
    opts = next;
  }

  function setStatus(i: number, status: OptionStatus) {
    const next = [...opts];
    next[i] = { ...next[i], status };
    // A successor only means anything on a retired term.
    if (status === 'active') delete next[i].replaced_by;
    opts = next;
  }

  function setReplacedBy(i: number, slug: string) {
    const next = [...opts];
    if (slug) next[i] = { ...next[i], replaced_by: slug };
    else {
      next[i] = { ...next[i] };
      delete next[i].replaced_by;
    }
    opts = next;
  }

  // Discard the local edits and adopt the server's current state.
  // Offered on a conflict as the alternative to overwriting.
  async function reloadFromServer() {
    const { data } = await api.GET('/fields/{id}', { params: { path: { id: fieldId } } });
    if (!data) return;
    const cur = data as {
      updated_at: string;
      label: string;
      required: boolean;
      options?: Record<string, unknown>;
    };
    opts = normalizeOptions(cur.options);
    label = cur.label;
    required = cur.required;
    baseline = cur.updated_at;
    snapshot = JSON.stringify(serializeOptions(opts));
    labelSnapshot = cur.label;
    requiredSnapshot = cur.required;
    conflict = false;
    error = '';
    savedMsg = '';
    onSaved();
  }

  async function save() {
    if (saving) return;
    saving = true;
    error = '';
    savedMsg = '';
    conflict = false;
    try {
      const body: Record<string, unknown> = {
        if_unchanged_since: baseline,
        label: label.trim(),
        required,
      };
      // Only vocabulary types carry a values document; sending one for
      // a number field would overwrite its min/max constraints.
      if (hasVocabulary) body.options = { values: serializeOptions(opts) };
      const { data, error: apiErr, response } = await api.PATCH('/fields/{id}', {
        params: { path: { id: fieldId } },
        body: body as never,
      });
      if (response?.status === 409) {
        // Visible, not silent. The operator's edits stay in the form;
        // acknowledging re-baselines so a deliberate retry overwrites
        // on purpose rather than by accident.
        const c = apiErr as { updated_at?: string } | undefined;
        if (c?.updated_at) baseline = c.updated_at;
        conflict = true;
        return;
      }
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('admin.fields.options_save_error');
        return;
      }
      const saved = data as {
        updated_at: string;
        label: string;
        required: boolean;
        options?: Record<string, unknown>;
      };
      baseline = saved.updated_at;
      opts = normalizeOptions(saved.options);
      label = saved.label;
      required = saved.required;
      snapshot = JSON.stringify(serializeOptions(opts));
      labelSnapshot = saved.label;
      requiredSnapshot = saved.required;
      savedMsg = t('admin.fields.options_saved');
      onSaved();
    } finally {
      saving = false;
    }
  }
</script>

<div
  class="min-w-0 space-y-3 rounded border border-border bg-bg-soft p-3 text-sm"
  data-testid="field-editor"
>
  <div class="flex flex-wrap items-end gap-3">
    <label class="w-full min-w-0 sm:flex-1">
      <span class="block text-xs text-fg-muted">{t('admin.fields.label')}</span>
      <input
        type="text"
        bind:value={label}
        data-testid="field-edit-label"
        class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>
    <label class="flex min-h-11 items-center gap-2 text-sm">
      <input
        type="checkbox"
        bind:checked={required}
        data-testid="field-edit-required"
        class="h-4 w-4 rounded border-border-strong"
      />
      <span>{t('admin.fields.edit_required')}</span>
    </label>
  </div>

  {#if hasVocabulary}
    <p class="text-xs text-fg-muted">{t('admin.fields.options_help')}</p>

    {#if opts.length === 0}
      <p class="text-xs text-fg-muted" data-testid="field-options-empty">
        {t('admin.fields.options_none')}
      </p>
    {/if}

    <ul class="space-y-2">
      {#each opts as o, i (o.value)}
        <li
          class="flex flex-wrap items-end gap-2 rounded border border-border bg-surface p-2"
          class:opacity-70={o.status !== 'active'}
          data-testid="field-option-row-{o.value}"
        >
          <span class="w-full font-mono text-xs text-fg-muted sm:w-auto">{o.value}</span>

          <label class="w-full min-w-0 sm:flex-1">
            <span class="block text-xs text-fg-muted">{t('admin.fields.options_label')}</span>
            <input
              type="text"
              bind:value={o.label}
              data-testid="field-option-label-{o.value}"
              class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
            />
          </label>

          <label class="min-w-0 flex-1 sm:flex-none">
            <span class="block text-xs text-fg-muted">{t('admin.fields.options_status')}</span>
            <select
              value={o.status}
              onchange={(e) => setStatus(i, (e.currentTarget as HTMLSelectElement).value as OptionStatus)}
              data-testid="field-option-status-{o.value}"
              class="mt-0.5 w-full max-w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none sm:w-auto"
            >
              {#each STATUSES as s (s)}
                <option value={s}>{t(`admin.fields.options_status_${s}`)}</option>
              {/each}
            </select>
          </label>

          {#if o.status !== 'active'}
            <label class="min-w-0 flex-1 sm:flex-none">
              <span class="block text-xs text-fg-muted">{t('admin.fields.options_replaced_by')}</span>
              <select
                value={o.replaced_by ?? ''}
                onchange={(e) => setReplacedBy(i, (e.currentTarget as HTMLSelectElement).value)}
                data-testid="field-option-replaced-by-{o.value}"
                class="mt-0.5 w-full max-w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none sm:w-auto"
              >
                <option value="">{t('admin.fields.options_replaced_by_none')}</option>
                {#each successorChoices as c (c.value)}
                  {#if c.value !== o.value}
                    <option value={c.value}>{c.label}</option>
                  {/if}
                {/each}
              </select>
            </label>
          {/if}

          <div class="flex gap-1">
            <button
              type="button"
              onclick={() => move(i, -1)}
              disabled={i === 0}
              aria-label={t('admin.fields.options_move_up')}
              data-testid="field-option-up-{o.value}"
              class="min-h-11 min-w-11 rounded border border-border px-2 text-fg-muted hover:bg-state-hover disabled:opacity-30"
            >↑</button>
            <button
              type="button"
              onclick={() => move(i, 1)}
              disabled={i === opts.length - 1}
              aria-label={t('admin.fields.options_move_down')}
              data-testid="field-option-down-{o.value}"
              class="min-h-11 min-w-11 rounded border border-border px-2 text-fg-muted hover:bg-state-hover disabled:opacity-30"
            >↓</button>
          </div>
        </li>
      {/each}
    </ul>

    <div class="flex flex-wrap items-end gap-2">
      <label class="w-full min-w-0 sm:flex-1">
        <span class="block text-xs text-fg-muted">{t('admin.fields.options_add_label')}</span>
        <input
          type="text"
          bind:value={newSlug}
          onkeydown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              addOption();
            }
          }}
          placeholder={t('admin.fields.options_add_placeholder')}
          data-testid="field-option-new"
          class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 font-mono text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        />
      </label>
      <button
        type="button"
        onclick={addOption}
        disabled={!newSlug.trim()}
        data-testid="field-option-add"
        class="min-h-11 rounded border border-border bg-surface px-3 py-1.5 text-sm disabled:opacity-40"
      >{t('admin.fields.options_add')}</button>
    </div>
  {:else}
  <p class="text-xs text-fg-muted" data-testid="field-options-type-note">
    {t('admin.fields.options_type_note', { type: fieldType })}
  </p>
  {/if}

  {#if conflict}
    <div
      role="alert"
      data-testid="field-options-conflict"
      class="space-y-2 rounded border border-warning/40 bg-warning-container px-3 py-2 text-sm text-warning"
    >
      <p class="font-medium">{t('admin.fields.options_conflict_heading')}</p>
      <p>{t('admin.fields.options_conflict_body')}</p>
      <div class="flex flex-wrap gap-2">
        <button
          type="button"
          onclick={() => void reloadFromServer()}
          data-testid="field-options-conflict-reload"
          class="min-h-11 rounded border border-border bg-surface px-3 py-1.5 text-sm"
        >{t('admin.fields.options_conflict_reload')}</button>
        <button
          type="button"
          onclick={() => { conflict = false; void save(); }}
          data-testid="field-options-conflict-overwrite"
          class="min-h-11 rounded border border-border bg-surface px-3 py-1.5 text-sm"
        >{t('admin.fields.options_conflict_overwrite')}</button>
      </div>
    </div>
  {/if}

  {#if error}
    <p role="alert" data-testid="field-options-error" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {/if}
  {#if savedMsg}
    <p data-testid="field-options-saved" class="text-sm text-success">{savedMsg}</p>
  {/if}

  <button
    type="button"
    onclick={save}
    disabled={saving || !dirty}
    data-testid="field-options-save"
    class="min-h-11 rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
  >{saving ? t('common.loading') : t('admin.fields.options_save')}</button>
</div>
