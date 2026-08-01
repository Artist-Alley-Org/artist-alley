<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<!--
  Sets a field's upload default, and each team's override of it
  (#793, ADR 0081 §3).

  Two shapes and no third: a literal value, or a name from a closed set
  the server resolves. There is no free-text box that becomes code — the
  prior art keeps executable PHP in a configuration column, and that is
  the mechanism this design exists to avoid.

  The picker only offers ACTIVE vocabulary terms. The server rejects a
  deprecated or archived one anyway; not offering it means the operator
  never has to discover that by being refused.
-->
<script lang="ts">
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import {
    normalizeOptions,
    selectableOptions,
    selectableTreeOptions,
    VALUE_COLUMN,
    type FieldOption,
  } from '$lib/fieldOptions';
  import {
    contextsForType,
    literalDefault,
    literalList,
    literalText,
    CONTEXT_KEYS,
    type DefaultContext,
    type FieldDefault,
  } from '$lib/fieldDefaults';

  let {
    fieldId,
    fieldType,
    initialDefault,
    initialOptions,
    onSaved = () => {},
  }: {
    fieldId: string;
    fieldType: string;
    initialDefault: FieldDefault | null | undefined;
    initialOptions: Record<string, unknown> | undefined;
    onSaved?: () => void;
  } = $props();

  interface Override {
    team_id: string;
    team_slug: string;
    team_name: string;
    default_value: FieldDefault;
  }
  interface Team {
    id: string;
    name: string;
  }

  type Mode = 'none' | 'literal' | 'context';

  const column = VALUE_COLUMN[fieldType];
  const contexts = contextsForType(fieldType);
  const vocab: FieldOption[] = normalizeOptions(initialOptions);
  const isVocabulary = fieldType === 'select' || fieldType === 'multi_select' || fieldType === 'tree';
  const isMulti = fieldType === 'multi_select';
  // Only ACTIVE terms. `held` is deliberately empty: a default is a NEW
  // value on every asset it touches, so the "a deprecated term you
  // already hold stays offered" rule does not apply — that rule exists
  // for records that would otherwise lose data, and a default has no
  // record behind it.
  // A tree term is shown by its full ancestor path ("Europe / London"),
  // because a bare leaf label is ambiguous to read even though the slug
  // behind it is unique. Flattened to one list of {value, display} so
  // the markup does not have to know which shape it is looking at.
  const terms = $derived<{ value: string; display: string }[]>(
    fieldType === 'tree'
      ? selectableTreeOptions(vocab, []).map((o) => ({
          value: o.value,
          display: o.path.length > 1 ? o.path.join(' / ') : o.label,
        }))
      : selectableOptions(vocab, []).map((o) => ({ value: o.value, display: o.label })),
  );

  function modeOf(d: FieldDefault | null | undefined): Mode {
    if (!d) return 'none';
    return d.kind === 'context' ? 'context' : 'literal';
  }

  let mode = $state<Mode>(modeOf(initialDefault));
  let text = $state(literalText(initialDefault));
  let list = $state<string[]>(literalList(initialDefault));
  let context = $state<DefaultContext | ''>(initialDefault?.context ?? '');

  let saving = $state(false);
  let error = $state('');
  let savedMsg = $state('');

  let overrides = $state<Override[]>([]);
  let teams = $state<Team[]>([]);
  let overridesLoaded = $state(false);
  let newTeam = $state('');

  const inputType = $derived(
    column === 'value_num' ? 'number' : column === 'value_date' ? 'datetime-local' : 'text',
  );

  function currentDocument(): FieldDefault | null {
    if (mode === 'none') return null;
    if (mode === 'context') return context ? { kind: 'context', context } : null;
    return literalDefault(fieldType, isMulti ? list : text);
  }

  function toggleTerm(slug: string) {
    list = list.includes(slug) ? list.filter((s) => s !== slug) : [...list, slug];
  }

  async function save() {
    if (saving) return;
    saving = true;
    error = '';
    savedMsg = '';
    try {
      const doc = currentDocument();
      const body: Record<string, unknown> =
        mode === 'none' ? { clear_default: true } : { default_value: doc };
      if (mode !== 'none' && !doc) {
        error = t('admin.fields.default_incomplete');
        return;
      }
      const { error: apiErr } = await api.PATCH('/fields/{id}', {
        params: { path: { id: fieldId } },
        body: body as never,
      });
      if (apiErr) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('admin.fields.default_save_error');
        return;
      }
      savedMsg = t('admin.fields.default_saved');
      onSaved();
    } finally {
      saving = false;
    }
  }

  async function loadOverrides() {
    if (overridesLoaded) return;
    overridesLoaded = true;
    const [o, m] = await Promise.all([
      api.GET('/fields/{id}/default-overrides', { params: { path: { id: fieldId } } }),
      // GET /teams is PAGINATED — a { items, next_cursor } envelope, not
      // a bare array. Reading it as an array leaves the picker silently
      // empty, which reads as "this instance has no teams".
      api.GET('/teams', { params: { query: { limit: 500 } } }),
    ]);
    overrides = ((o.data ?? []) as Override[]) ?? [];
    teams = ((m.data as { items?: Team[] } | undefined)?.items ?? []) as Team[];
  }

  async function addOverride() {
    if (!newTeam) return;
    const doc = currentDocument();
    if (!doc) {
      error = t('admin.fields.default_override_needs_value');
      return;
    }
    error = '';
    const { error: apiErr } = await api.PUT('/fields/{id}/default-overrides/{team_id}', {
      params: { path: { id: fieldId, team_id: newTeam } },
      body: doc as never,
    });
    if (apiErr) {
      error = (apiErr as { error?: string } | undefined)?.error ?? t('admin.fields.default_save_error');
      return;
    }
    newTeam = '';
    overridesLoaded = false;
    await loadOverrides();
  }

  async function removeOverride(teamId: string) {
    await api.DELETE('/fields/{id}/default-overrides/{team_id}', {
      params: { path: { id: fieldId, team_id: teamId } },
    });
    overridesLoaded = false;
    await loadOverrides();
  }

  function describeOverride(o: Override): string {
    if (o.default_value.kind === 'context') {
      return o.default_value.context ? t(CONTEXT_KEYS[o.default_value.context]) : '';
    }
    if (Array.isArray(o.default_value.value_options)) return o.default_value.value_options.join(', ');
    return String(
      o.default_value.value_text ??
        o.default_value.value_num ??
        o.default_value.value_date ??
        o.default_value.value_ref ??
        '',
    );
  }
</script>

<div
  class="min-w-0 space-y-3 rounded border border-border bg-bg-soft p-3 text-sm"
  data-testid="field-default-editor"
>
  <p class="text-xs text-fg-muted">{t('admin.fields.default_help')}</p>

  <fieldset class="flex flex-wrap gap-2">
    <legend class="sr-only">{t('admin.fields.default_mode')}</legend>
    {#each [{ k: 'none' as const, label: t('admin.fields.default_mode_none') }, { k: 'literal' as const, label: t('admin.fields.default_mode_literal') }, ...(contexts.length ? [{ k: 'context' as const, label: t('admin.fields.default_mode_context') }] : [])] as opt (opt.k)}
      <label
        class="min-h-11 cursor-pointer rounded border bg-surface px-3 py-2 text-sm"
        class:border-accent={mode === opt.k}
        class:text-accent={mode === opt.k}
        class:border-border={mode !== opt.k}
      >
        <input
          type="radio"
          name="default-mode-{fieldId}"
          value={opt.k}
          bind:group={mode}
          data-testid="field-default-mode-{opt.k}"
          class="sr-only"
        />
        {opt.label}
      </label>
    {/each}
  </fieldset>

  {#if mode === 'literal'}
    {#if isVocabulary}
      {#if terms.length === 0}
        <p class="text-xs text-fg-muted" data-testid="field-default-no-terms">
          {t('admin.fields.default_no_terms')}
        </p>
      {:else if isMulti}
        <ul class="space-y-1">
          {#each terms as o (o.value)}
            <li>
              <label class="flex min-h-11 items-center gap-2">
                <input
                  type="checkbox"
                  checked={list.includes(o.value)}
                  onchange={() => toggleTerm(o.value)}
                  data-testid="field-default-term-{o.value}"
                  class="h-4 w-4 rounded border-border-strong"
                />
                <span>{o.display}</span>
              </label>
            </li>
          {/each}
        </ul>
      {:else}
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.fields.default_value')}</span>
          <select
            bind:value={text}
            data-testid="field-default-select"
            class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          >
            <option value="">{t('admin.fields.default_mode_none')}</option>
            {#each terms as o (o.value)}
              <option value={o.value}>{o.display}</option>
            {/each}
          </select>
        </label>
      {/if}
    {:else if fieldType === 'boolean'}
      <label class="block">
        <span class="block text-xs text-fg-muted">{t('admin.fields.default_value')}</span>
        <select
          bind:value={text}
          data-testid="field-default-boolean"
          class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        >
          <option value="">{t('admin.fields.default_mode_none')}</option>
          <!-- 1 / 0 in value_num, ADR 0012's encoding — never the
               strings "true"/"false" in value_text (#791). -->
          <option value="1">{t('common.yes')}</option>
          <option value="0">{t('common.no')}</option>
        </select>
      </label>
    {:else}
      <label class="block">
        <span class="block text-xs text-fg-muted">{t('admin.fields.default_value')}</span>
        <input
          type={inputType}
          bind:value={text}
          data-testid="field-default-literal"
          class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        />
      </label>
    {/if}
  {:else if mode === 'context'}
    <label class="block">
      <span class="block text-xs text-fg-muted">{t('admin.fields.default_context')}</span>
      <select
        bind:value={context}
        data-testid="field-default-context"
        class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      >
        <option value="">{t('admin.fields.default_mode_none')}</option>
        {#each contexts as c (c)}
          <option value={c}>{t(CONTEXT_KEYS[c])}</option>
        {/each}
      </select>
    </label>
    <p class="text-xs text-fg-muted">{t('admin.fields.default_context_help')}</p>
  {/if}

  {#if error}
    <p role="alert" data-testid="field-default-error" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {/if}
  {#if savedMsg}
    <p data-testid="field-default-saved" class="text-sm text-success">{savedMsg}</p>
  {/if}

  <button
    type="button"
    onclick={save}
    disabled={saving}
    data-testid="field-default-save"
    class="min-h-11 rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
  >{saving ? t('common.loading') : t('admin.fields.default_save')}</button>

  <!-- Team overrides. Behind a disclosure because most fields will
       never have one, and two extra requests per row on a page that
       lists every field is a cost nobody asked for. -->
  <details ontoggle={() => void loadOverrides()} data-testid="field-default-overrides">
    <summary class="cursor-pointer text-xs text-fg-muted">{t('admin.fields.default_overrides')}</summary>
    <div class="mt-2 space-y-2">
      <p class="text-xs text-fg-muted">{t('admin.fields.default_overrides_help')}</p>
      {#if overrides.length === 0}
        <p class="text-xs text-fg-muted" data-testid="field-default-overrides-empty">
          {t('admin.fields.default_overrides_none')}
        </p>
      {:else}
        <ul class="space-y-1">
          {#each overrides as o (o.team_id)}
            <li
              class="flex flex-wrap items-center gap-2 rounded border border-border bg-surface p-2"
              data-testid="field-default-override-{o.team_slug}"
            >
              <span class="font-medium">{o.team_name}</span>
              <span class="text-fg-muted">{describeOverride(o)}</span>
              <button
                type="button"
                onclick={() => void removeOverride(o.team_id)}
                data-testid="field-default-override-remove-{o.team_slug}"
                class="ml-auto min-h-11 rounded border border-border px-2 text-xs text-fg-muted hover:bg-state-hover"
              >{t('common.remove')}</button>
            </li>
          {/each}
        </ul>
      {/if}

      <div class="flex flex-wrap items-end gap-2">
        <label class="min-w-0 flex-1">
          <span class="block text-xs text-fg-muted">{t('admin.fields.default_override_team')}</span>
          <select
            bind:value={newTeam}
            data-testid="field-default-override-team"
            class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          >
            <option value="">—</option>
            {#each teams as team (team.id)}
              <option value={team.id}>{team.name}</option>
            {/each}
          </select>
        </label>
        <button
          type="button"
          onclick={() => void addOverride()}
          disabled={!newTeam}
          data-testid="field-default-override-add"
          class="min-h-11 rounded border border-border bg-surface px-3 py-1.5 text-sm disabled:opacity-40"
        >{t('admin.fields.default_override_add')}</button>
      </div>
      <p class="text-xs text-fg-muted">{t('admin.fields.default_override_add_help')}</p>
    </div>
  </details>
</div>
