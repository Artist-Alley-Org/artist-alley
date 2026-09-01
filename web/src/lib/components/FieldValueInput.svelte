<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Typed input renderer for one field value. Used by both the
  // asset edit surface (future) and the collection edit modal —
  // single place to handle text / longtext / rich_text / number /
  // boolean / date / datetime / select / multi_select / tree /
  // reference. Phase 1.9.B introduces it; the asset side adopts
  // it in a follow-up sweep.
  //
  // The component is presentational — it owns the bind:value
  // surface for one input, doesn't speak to the API. Parents
  // collect changes and POST/PUT themselves so the modal's
  // dirty-state tracking stays in one place.

  import { t } from '$stores/lang.svelte';
  import {
    decodeBoolean,
    encodeBoolean,
    normalizeOptions,
    selectableOptions,
    selectableTreeOptions,
  } from '$lib/fieldOptions';
  import VocabularyCombobox from './VocabularyCombobox.svelte';

  interface FieldDef {
    id: string;
    code: string;
    label: string;
    /** The operator's own note about what belongs in this field
     *  (#1173). Authored on /admin/fields and, until sprint 19, shown
     *  nowhere the person entering a value could read it. Reused
     *  as-is: no new storage, and its authoring semantics are
     *  unchanged. */
    description?: string;
    type:
      | 'text'
      | 'longtext'
      | 'rich_text'
      | 'number'
      | 'boolean'
      | 'date'
      | 'datetime'
      | 'select'
      | 'multi_select'
      | 'tree'
      | 'reference';
    required: boolean;
    display_group?: string;
    options?: Record<string, unknown>;
    /** The vocabulary grows from what is written to it (#830).
        multi_select only — the server ignores it elsewhere. */
    open_vocabulary?: boolean;
  }

  interface Value {
    value_text?: string | null;
    value_num?: number | null;
    value_date?: string | null;
    value_options?: string[] | null;
    value_ref?: string | null;
  }

  interface Props {
    def: FieldDef;
    value: Value;
    disabled?: boolean;
    /**
     * Let a vocabulary control ask the SERVER for matches rather than
     * filtering the options it was handed (#1119, ADR 0092 §1).
     *
     * Off by default so every existing caller is unchanged. The create
     * page turns it on, because a page carrying twenty vocabulary
     * fields would otherwise ship every term of all twenty before the
     * artist touched a control.
     */
    serverVocabulary?: boolean;
    onchange: (v: Value) => void;
  }

  let { def, value, disabled = false, serverVocabulary = false, onchange }: Props = $props();

  // Snapshot local copies of the bound fields so Svelte's reactivity
  // tracks them per-input. The component is uncontrolled at the
  // <input> level but reports every change via onchange — parents
  // own the canonical value.
  let textVal = $state('');
  let numVal = $state<number | null>(null);
  let dateVal = $state('');
  let optionsVal = $state<string[]>([]);
  let refVal = $state('');

  $effect(() => {
    textVal = value.value_text ?? '';
    numVal = value.value_num ?? null;
    dateVal = value.value_date ?? '';
    optionsVal = value.value_options ?? [];
    refVal = value.value_ref ?? '';
  });

  function emitText(v: string) {
    textVal = v;
    onchange({ value_text: v || null });
  }
  function emitNum(v: string) {
    const n = v === '' ? null : Number(v);
    numVal = n;
    onchange({ value_num: n });
  }
  function emitBool(v: boolean) {
    // 1/0 in value_num (ADR 0012). This emitted the string "true" /
    // "false" into value_text until #791, which the asset write
    // endpoint rejects outright — it has always required value_num.
    numVal = encodeBoolean(v);
    onchange({ value_num: numVal });
  }
  function emitDate(v: string) {
    dateVal = v;
    onchange({ value_date: v || null });
  }
  function emitSelect(v: string) {
    textVal = v;
    onchange({ value_text: v || null });
  }
  function emitMultiSelect(selected: string[]) {
    optionsVal = selected;
    onchange({ value_options: selected.length ? selected : null });
  }
  function emitRef(v: string) {
    refVal = v;
    onchange({ value_ref: v || null });
  }

  // Select options come from field_definition.options.values per
  // ADR 0012. Entries are bare slug strings OR {value, label} objects
  // — normalizeOptions handles both. This file previously assumed the
  // object form only, which meant every seeded vocabulary (all bare
  // strings) rendered as blank options.
  const allOptions = $derived(normalizeOptions(def.options));

  // Deprecated and archived terms are not offered for NEW values, but
  // a value the record ALREADY holds stays in the list — dropping it
  // would blank the field on a record nobody edited, which is exactly
  // what ADR 0012 forbids.
  //
  // `tree` reads its held value from textVal, not optionsVal: a tree
  // value is one slug in value_text, the same as `select`. This
  // component used to group it with multi_select and emit
  // value_options, which put a collection's tree value in a different
  // column from an asset's and made it unreadable by the detail
  // surface either way (#778).
  const held = $derived(
    def.type === 'multi_select' ? optionsVal : textVal ? [textVal] : [],
  );
  const selectOptions = $derived(selectableOptions(allOptions, held));

  // The tree picker offers every term at every depth — a branch is a
  // legitimate answer, not just a leaf — indented so the hierarchy is
  // legible in a plain <select>. A dedicated tree widget is #779; this
  // is the minimum that makes the value settable and correct.
  const treeOptions = $derived(selectableTreeOptions(allOptions, held));
  const INDENT = '    ';
</script>

<!-- The field's visible name. A snippet because multi_select's
     wrapper is a <div> and every other type's is a <label> — see the
     note at that branch — and the name reads the same in both. -->
{#snippet fieldName()}
  <span class="text-sm text-fg-muted">
    {def.label}
    {#if def.required}
      <span class="ml-1 text-xs text-fg-muted" data-testid="required-marker">({t('collection_fields.required_marker')})</span>
    {/if}
  </span>
{/snippet}

<!--
  The operator's guidance for this field, from the definition's own
  `description` (#1173). BELOW the control rather than above it, so it
  reads as help for the box the person is in rather than as a second
  label; and rendered only when there is something to say, so a field
  nobody documented is laid out exactly as it is today.

  Not a <span> inside the <label>: on the multi_select branch the
  wrapper is a <div>, and the guidance has to read the same in both.
-->
{#snippet fieldHelp()}
  {#if (def.description ?? '').trim()}
    <p class="mt-1 text-xs text-fg-muted" data-testid="field-help-{def.code}">
      {def.description}
    </p>
  {/if}
{/snippet}

<div class="space-y-1">
  {#if def.type === 'multi_select'}
    <!--
      A <div>, not a <label>: the combobox renders a remove button per
      chip and a <button> is labelable, so the implicit association
      would bind the field's name to the first chip's remove button
      rather than to the text box. The control names itself instead.
    -->
    <div class="block">
      {@render fieldName()}
      <VocabularyCombobox
        options={allOptions}
        value={optionsVal}
        open={def.open_vocabulary === true}
        {disabled}
        label={def.label}
        testid={def.code}
        fieldId={serverVocabulary ? def.id : null}
        onchange={emitMultiSelect}
      />
      {@render fieldHelp()}
    </div>
  {:else}
  <label class="block">
    {@render fieldName()}

    {#if def.type === 'text' || def.type === 'longtext' || def.type === 'rich_text'}
      {#if def.type === 'longtext' || def.type === 'rich_text'}
        <textarea
          bind:value={textVal}
          oninput={(e) => emitText((e.currentTarget as HTMLTextAreaElement).value)}
          {disabled}
          rows="3"
          maxlength="10000"
          data-testid="field-input-{def.code}"
          class="mt-1 w-full resize-y rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        ></textarea>
      {:else}
        <input
          type="text"
          bind:value={textVal}
          oninput={(e) => emitText((e.currentTarget as HTMLInputElement).value)}
          {disabled}
          maxlength="500"
          data-testid="field-input-{def.code}"
          class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        />
      {/if}
    {:else if def.type === 'number'}
      <input
        type="number"
        value={numVal ?? ''}
        oninput={(e) => emitNum((e.currentTarget as HTMLInputElement).value)}
        {disabled}
        data-testid="field-input-{def.code}"
        class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
      />
    {:else if def.type === 'boolean'}
      <label class="mt-1 flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={decodeBoolean(numVal) === true}
          onchange={(e) => emitBool((e.currentTarget as HTMLInputElement).checked)}
          {disabled}
          data-testid="field-input-{def.code}"
          class="h-4 w-4 rounded border-border-strong"
        />
        <span>{def.label}</span>
      </label>
    {:else if def.type === 'date' || def.type === 'datetime'}
      <input
        type={def.type === 'datetime' ? 'datetime-local' : 'date'}
        bind:value={dateVal}
        onchange={(e) => emitDate((e.currentTarget as HTMLInputElement).value)}
        {disabled}
        data-testid="field-input-{def.code}"
        class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
      />
    {:else if def.type === 'select'}
      <select
        bind:value={textVal}
        onchange={(e) => emitSelect((e.currentTarget as HTMLSelectElement).value)}
        {disabled}
        data-testid="field-input-{def.code}"
        class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
      >
        <option value=""></option>
        {#each selectOptions as opt (opt.value)}
          <option value={opt.value}>{opt.status === 'active' ? opt.label : t('common.option_deprecated', { label: opt.label })}</option>
        {/each}
      </select>
    {:else if def.type === 'tree'}
      <select
        bind:value={textVal}
        onchange={(e) => emitSelect((e.currentTarget as HTMLSelectElement).value)}
        {disabled}
        data-testid="field-input-{def.code}"
        class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 font-mono text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
      >
        <option value=""></option>
        {#each treeOptions as opt (opt.value)}
          <option value={opt.value} title={opt.path.join(' / ')}>
            {INDENT.repeat(opt.depth)}{opt.status === 'active'
              ? opt.label
              : t('common.option_deprecated', { label: opt.label })}
          </option>
        {/each}
      </select>
    {:else if def.type === 'reference'}
      <input
        type="text"
        bind:value={refVal}
        oninput={(e) => emitRef((e.currentTarget as HTMLInputElement).value)}
        placeholder="UUID"
        {disabled}
        data-testid="field-input-{def.code}"
        class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 font-mono text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
      />
    {/if}
  </label>
  {@render fieldHelp()}
  {/if}
</div>
