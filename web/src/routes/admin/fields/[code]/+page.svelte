<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // One field, one page (#854).
  //
  // /admin/fields was a nine-column table that expanded three separate
  // editors inside its own cells. The owner's call on 2026-08-02 was
  // that it is too cramped, and the fields arc kept adding to the same
  // cell. So the table went back to being an index and everything a
  // field IS lives here, at full width, at a URL you can send someone.
  //
  // Nothing was rewritten to get here. FieldEditor, FieldDefaultEditor
  // and ExtractionConfigPicker were already standalone components with
  // `fieldId` + initial-value props; this page is where they are laid
  // out instead of a <td colspan="9">.
  //
  // The route key is `code`, not `id`. `code` is UNIQUE in the schema
  // (field_definition_code_key), server-validated as
  // ^[a-z][a-z0-9_]*$ so it needs no escaping in a URL, and absent
  // from FieldDefinitionUpdate — there is no PATCH that can move a
  // field's code out from under a bookmark. The API keys every field
  // endpoint on the UUID, so the code is resolved to a row here and
  // the components below still take the id.

  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import ExtractionConfigPicker from '$components/ExtractionConfigPicker.svelte';
  import FieldEditor from '$components/FieldEditor.svelte';
  import FieldDefaultEditor from '$components/FieldDefaultEditor.svelte';
  import { typeSupportsDefault, type FieldDefault } from '$lib/fieldDefaults';

  interface Field {
    id: string;
    code: string;
    label: string;
    description?: string;
    type: string;
    subject_kind: 'asset' | 'collection';
    required: boolean;
    applies_to: number[];
    display_group?: string;
    display_order?: number;
    extraction_source?: string;
    extraction_mode?: string;
    options?: Record<string, unknown>;
    open_vocabulary?: boolean;
    show_on_card?: boolean;
    show_in_advanced_search?: boolean;
    show_on_upload?: boolean;
    edit_tab?: string | null;
    searchable?: boolean;
    read_only?: boolean;
    regexp_filter?: string | null;
    read_capability?: string | null;
    write_capability?: string | null;
    mirrors_column?: string | null;
    status?: string;
    default_value?: FieldDefault | null;
    created_at?: string;
    updated_at: string;
  }
  interface AssetType {
    ref: number;
    name?: string | null;
  }

  const code = $derived(page.params.code ?? '');

  let field = $state<Field | null>(null);
  let assetTypes = $state<AssetType[]>([]);
  let loading = $state(true);
  let notFound = $state(false);

  onMount(() => void load());

  // `silent` refreshes WITHOUT flipping `loading`, for the same reason
  // the index does it: swapping the page for a spinner after a save
  // would take the "saved" confirmation — and any unsaved state in a
  // sibling editor — with it. The editors keep their own state across
  // a prop change, so re-reading the row here updates the header and
  // the read-only facts without disturbing an open form.
  async function load(silent = false) {
    if (!silent) loading = true;
    try {
      const [fResp, tResp] = await Promise.all([
        api.GET('/fields', { params: { query: {} } }),
        api.GET('/asset_types'),
      ]);
      let found = ((fResp.data ?? []) as Field[]).find((f) => f.code === code);
      if (!found) {
        // The default list is the LIVE schema — archived rows are
        // excluded. A bookmark to a field somebody has since archived
        // should still open, and say so, rather than 404 as though the
        // field never existed.
        const arch = await api.GET('/fields', { params: { query: { status: 'archived' } } });
        found = ((arch.data ?? []) as Field[]).find((f) => f.code === code);
      }
      field = found ?? null;
      notFound = !found;
      assetTypes = ((tResp.data ?? []) as AssetType[]).slice().sort((a, b) => a.ref - b.ref);
    } finally {
      if (!silent) loading = false;
    }
  }

  const appliesToLabel = $derived.by(() => {
    if (!field || field.subject_kind !== 'asset') return '';
    if (!field.applies_to?.length) return t('admin.fields.applies_to_all');
    const byRef = new Map(assetTypes.map((a) => [a.ref, a.name ?? `#${a.ref}`]));
    return field.applies_to.map((r) => byRef.get(r) ?? `#${r}`).join(', ');
  });

  function when(iso: string | undefined): string {
    if (!iso) return '—';
    const d = new Date(iso);
    return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
  }
</script>

<svelte:head>
  <title>{field ? field.label : code} — {t('admin.fields.title')} — {site.name}</title>
</svelte:head>

<p class="mb-3 text-xs">
  <a href="/admin/fields" data-testid="field-detail-back" class="text-accent hover:underline">
    {t('admin.field_detail.back')}
  </a>
</p>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if notFound || !field}
  <p
    role="alert"
    data-testid="field-detail-not-found"
    class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger"
  >
    {t('admin.field_detail.not_found', { code })}
  </p>
{:else}
  <header class="mb-4" data-testid="field-detail-header">
    <h2 class="text-xl font-semibold" data-testid="field-detail-label">{field.label}</h2>
    <p class="mt-1 font-mono text-xs text-fg-muted" data-testid="field-detail-code">{field.code}</p>
    <div class="mt-2 flex flex-wrap items-center gap-1.5 text-[10px] uppercase tracking-wider">
      <span class="rounded border border-border bg-surface px-1.5 py-0.5 text-fg-muted">{field.type}</span>
      <span class="rounded border border-border bg-surface px-1.5 py-0.5 text-fg-muted">
        {field.subject_kind === 'collection'
          ? t('admin.fields.subject_collection')
          : t('admin.fields.subject_asset')}
      </span>
      {#if field.status && field.status !== 'active'}
        <span
          data-testid="field-detail-status"
          class="rounded border border-warning/40 bg-warning/10 px-1.5 py-0.5 text-warning"
        >{field.status}</span>
      {/if}
      {#if field.mirrors_column}
        <span
          data-testid="field-detail-mirror-badge"
          class="rounded border border-accent/40 bg-accent/10 px-1.5 py-0.5 text-accent"
        >{t('admin.field_detail.mirror_badge')}</span>
      {/if}
    </div>
  </header>

  {#if field.mirrors_column}
    <!--
      A mirrored field is a VIEW onto a column of the asset row (#822,
      ADR 0012's second 2026-08-10 amendment). Which columns are
      mirrorable is a CHECK constraint, so `mirrors_column` is
      readOnly over the API — there is deliberately no control here,
      and the point of the callout is that an operator finds that out
      from the page instead of from a support thread.
    -->
    <section
      data-testid="field-detail-mirror"
      class="mb-4 space-y-2 rounded border border-accent/40 bg-accent/5 px-3 py-3 text-sm"
    >
      <h3 class="font-semibold">
        {t('admin.field_detail.mirror_heading', { column: field.mirrors_column })}
      </h3>
      <p class="text-fg-muted">
        {t('admin.field_detail.mirror_body', { column: field.mirrors_column })}
      </p>
      <p class="text-fg-muted">{t('admin.field_detail.mirror_auth')}</p>
      <dl class="grid gap-1 text-xs sm:grid-cols-[10rem_1fr]">
        <dt class="text-fg-muted">{t('admin.field_detail.mirror_column_label')}</dt>
        <dd class="font-mono" data-testid="field-detail-mirrors-column">{field.mirrors_column}</dd>
      </dl>
      <p class="text-xs text-fg-muted" data-testid="field-detail-mirror-readonly">
        {t('admin.field_detail.mirror_readonly')}
      </p>
    </section>
  {/if}

  <FieldEditor
    fieldId={field.id}
    fieldType={field.type}
    initialLabel={field.label}
    initialDescription={field.description ?? ''}
    initialRequired={field.required}
    initialOptions={field.options}
    initialOpenVocabulary={field.open_vocabulary === true}
    initialShowOnCard={field.show_on_card === true}
    initialShowInAdvancedSearch={field.show_in_advanced_search !== false}
    initialShowOnUpload={field.show_on_upload !== false}
    initialEditTab={field.edit_tab ?? null}
    initialSearchable={field.searchable !== false}
    initialReadOnly={field.read_only === true}
    initialRegexpFilter={field.regexp_filter ?? null}
    initialMirrorsColumn={field.mirrors_column ?? null}
    initialReadCapability={field.read_capability ?? null}
    initialWriteCapability={field.write_capability ?? null}
    initialDisplayGroup={field.display_group ?? ''}
    initialDisplayOrder={field.display_order ?? 0}
    initialAppliesTo={field.applies_to ?? []}
    subjectKind={field.subject_kind}
    {assetTypes}
    initialUpdatedAt={field.updated_at}
    onSaved={() => load(true)}
  />

  <section class="mt-4 space-y-3 rounded border border-border bg-bg-soft p-3 text-sm">
    <h3 class="text-sm font-semibold">{t('admin.fields.extraction')}</h3>
    <p class="text-xs text-fg-muted">{t('admin.field_detail.extraction_help')}</p>
    <ExtractionConfigPicker
      fieldId={field.id}
      initialSource={field.extraction_source ?? ''}
      initialMode={field.extraction_mode ?? ''}
      onSaved={() => load(true)}
    />
  </section>

  {#if field.subject_kind === 'asset' && typeSupportsDefault(field.type)}
    <section class="mt-4 space-y-3 rounded border border-border bg-bg-soft p-3 text-sm">
      <h3 class="text-sm font-semibold">{t('admin.fields.default')}</h3>
      <FieldDefaultEditor
        fieldId={field.id}
        fieldType={field.type}
        initialDefault={field.default_value}
        initialOptions={field.options}
        onSaved={() => load(true)}
      />
    </section>
  {/if}

  <section
    class="mt-4 rounded border border-border bg-bg-soft p-3 text-sm"
    data-testid="field-detail-facts"
  >
    <h3 class="mb-2 text-sm font-semibold">{t('admin.field_detail.facts')}</h3>
    <dl class="grid gap-1 text-xs sm:grid-cols-[10rem_1fr]">
      <dt class="text-fg-muted">{t('admin.fields.code')}</dt>
      <dd class="font-mono break-all">{field.code}</dd>
      <dt class="text-fg-muted">{t('admin.fields.type')}</dt>
      <dd class="font-mono">{field.type}</dd>
      <dt class="text-fg-muted">{t('admin.field_detail.field_id')}</dt>
      <dd class="font-mono break-all" data-testid="field-detail-id">{field.id}</dd>
      <dt class="text-fg-muted">{t('admin.fields.applies_to')}</dt>
      <dd>{field.subject_kind === 'asset' ? appliesToLabel : '—'}</dd>
      <dt class="text-fg-muted">{t('admin.field_detail.status')}</dt>
      <dd data-testid="field-detail-status-fact">{field.status ?? '—'}</dd>
      <dt class="text-fg-muted">{t('admin.field_detail.created')}</dt>
      <dd>{when(field.created_at)}</dd>
      <dt class="text-fg-muted">{t('admin.field_detail.updated')}</dt>
      <dd>{when(field.updated_at)}</dd>
    </dl>
    <p class="mt-2 text-xs text-fg-muted">{t('admin.field_detail.lifecycle_help')}</p>
  </section>
{/if}
