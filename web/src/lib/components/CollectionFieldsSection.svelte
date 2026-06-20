<script lang="ts">
  // Phase 1.9.B — collection custom-fields editor.
  //
  // Drops into EditCollectionModal (and eventually a dedicated
  // /collections/[id]/edit page) to surface every operator-defined
  // collection field for inline editing. Fields render grouped by
  // display_group; values save individually via PUT
  // /collections/{id}/fields/{field_id}.
  //
  // The component is self-contained — it loads the field definitions
  // (filtered to subject_kind=collection) and the existing values on
  // mount, then handles saves on a per-field debounced flush. The
  // parent only owns the collection id and an optional onsaved hook.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import FieldValueInput from './FieldValueInput.svelte';

  interface FieldDef {
    id: string;
    code: string;
    label: string;
    type:
      | 'text' | 'longtext' | 'rich_text' | 'number' | 'boolean'
      | 'date' | 'datetime' | 'select' | 'multi_select' | 'tree' | 'reference';
    required: boolean;
    display_group?: string;
    options?: Record<string, unknown>;
    write_capability?: string | null;
  }

  interface Value {
    value_text?: string | null;
    value_num?: number | null;
    value_date?: string | null;
    value_options?: string[] | null;
    value_ref?: string | null;
  }

  interface Props {
    collectionId: string;
    canEdit?: boolean;
  }
  let { collectionId, canEdit = true }: Props = $props();

  let definitions = $state<FieldDef[]>([]);
  let values = $state<Record<string, Value>>({});
  let dirty = $state<Record<string, Value>>({});
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  onMount(() => void load());

  async function load() {
    loading = true;
    try {
      const [defsRes, valuesRes] = await Promise.all([
        api.GET('/fields', { params: { query: { subject_kind: 'collection' } } }),
        api.GET('/collections/{id}/fields', { params: { path: { id: collectionId } } }),
      ]);
      definitions = (defsRes.data ?? []) as FieldDef[];
      const initial: Record<string, Value> = {};
      for (const v of (valuesRes.data ?? []) as Array<{ field_id: string } & Value>) {
        initial[v.field_id] = {
          value_text: v.value_text ?? null,
          value_num: v.value_num ?? null,
          value_date: v.value_date ?? null,
          value_options: v.value_options ?? null,
          value_ref: v.value_ref ?? null,
        };
      }
      values = initial;
      dirty = {};
    } catch {
      error = t('collection_fields.save_error');
    } finally {
      loading = false;
    }
  }

  function onFieldChange(fieldId: string, patch: Value) {
    dirty[fieldId] = { ...(dirty[fieldId] ?? {}), ...patch };
    values[fieldId] = { ...(values[fieldId] ?? {}), ...patch };
    saved = false;
  }

  async function save() {
    if (saving || Object.keys(dirty).length === 0) return;
    saving = true;
    saved = false;
    error = null;
    try {
      const entries = Object.entries(dirty);
      const results = await Promise.allSettled(
        entries.map(([fieldId, val]) =>
          api.PUT('/collections/{id}/fields/{field_id}', {
            params: { path: { id: collectionId, field_id: fieldId } },
            body: {
              value_text: val.value_text ?? undefined,
              value_num: val.value_num ?? undefined,
              value_date: val.value_date ?? undefined,
              value_options: val.value_options ?? undefined,
              value_ref: val.value_ref ?? undefined,
            } as never,
          })
        )
      );
      const failed = results.find((r) => r.status === 'rejected' ||
        (r.status === 'fulfilled' && (r.value as { error?: unknown }).error));
      if (failed) {
        error = t('collection_fields.save_error');
        return;
      }
      saved = true;
      dirty = {};
    } finally {
      saving = false;
    }
  }

  // Group definitions by display_group for the rendered layout.
  type Group = { name: string; defs: FieldDef[] };
  const grouped = $derived.by(() => {
    const buckets = new Map<string, FieldDef[]>();
    for (const d of definitions) {
      const k = d.display_group || 'general';
      if (!buckets.has(k)) buckets.set(k, []);
      buckets.get(k)!.push(d);
    }
    const out: Group[] = [];
    for (const [name, defs] of buckets) out.push({ name, defs });
    return out;
  });
</script>

<section class="space-y-3" data-testid="collection-fields-section">
  <h3 class="text-sm font-semibold text-fg">{t('collection_fields.section_title')}</h3>
  {#if loading}
    <p class="text-sm text-fg-muted">{t('common.loading')}</p>
  {:else if definitions.length === 0}
    <p class="text-sm text-fg-muted" data-testid="collection-fields-empty">{t('collection_fields.no_fields')}</p>
  {:else}
    {#each grouped as group (group.name)}
      <fieldset class="space-y-2 rounded border border-border bg-surface p-3">
        <legend class="px-1 text-xs font-medium uppercase tracking-wider text-fg-muted">{group.name}</legend>
        {#each group.defs as def (def.id)}
          <FieldValueInput
            {def}
            value={values[def.id] ?? {}}
            disabled={!canEdit}
            onchange={(v) => onFieldChange(def.id, v)}
          />
        {/each}
      </fieldset>
    {/each}

    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger" data-testid="collection-fields-error">{error}</p>
    {/if}
    {#if saved}
      <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success" data-testid="collection-fields-saved">{t('collection_fields.saved')}</p>
    {/if}

    {#if canEdit}
      <button
        type="button"
        onclick={save}
        disabled={saving || Object.keys(dirty).length === 0}
        data-testid="collection-fields-save"
        class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
      >
        {saving ? t('common.loading') : t('collection_fields.save')}
      </button>
    {/if}
  {/if}
</section>
