<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // One row per pending upload. Thumb + filename + progress bar +
  // state badge + remove. Per-file title is editable inline (the
  // post composer's title applies to the post, not the assets);
  // tags are a chip input.
  //
  // While the row is uploading the progress bar is the focal point;
  // when it's ready the row collapses to a compact summary so the
  // user can scan a queue of many files.
  //
  // Per-asset metadata: an optional "Metadata" disclosure lets the
  // user fill in field_values BEFORE the asset is created — they're
  // PUT to /assets/{id}/fields/{field_id} after the asset row
  // exists. Closed by default to keep the row compact; expanding
  // lazy-loads the field defs for the asset_type.

  import type { UploadRow, PendingFieldValue } from '$stores/upload.svelte';
  import { upload, fieldsForAssetType } from '$stores/upload.svelte';
  import { t } from '$stores/lang.svelte';
  import { is3DExt } from '../viewers/controller';

  // True when the row's file is a 3D model — drives the companion
  // disclosure visibility. Audio / image / etc. don't need a
  // companion picker.
  function isModelRow(row: UploadRow): boolean {
    const ext = row.file.name.split('.').pop() ?? '';
    return is3DExt(ext);
  }

  // Companion picker — fires only for 3D rows. Click the (+) to
  // open the OS file picker; selected files become pending
  // companions with the bare filename as the default path. The
  // user can tweak the path before upload to put a texture in a
  // subdirectory ('textures/foo.png') if the model file references
  // it that way.
  let companionInput = $state<HTMLInputElement | null>(null);
  function openCompanionPicker() { companionInput?.click(); }
  function onCompanionPick(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    if (input.files && input.files.length > 0) {
      upload.addCompanions(row.id, input.files);
      input.value = ''; // reset so picking the same file again re-fires
    }
  }
  function onCompanionDrop(e: DragEvent) {
    e.preventDefault();
    if (e.dataTransfer?.files?.length) {
      upload.addCompanions(row.id, e.dataTransfer.files);
    }
  }

  interface Props {
    row: UploadRow;
  }
  let { row }: Props = $props();

  // The metadata editor is collapsed by default. Only fetch the
  // field list when the user opens it the first time.
  let metaOpen = $state(false);
  let metaFields = $state<FieldDef[] | null>(null);
  let metaLoading = $state(false);
  let metaError = $state<string | null>(null);

  interface FieldDef {
    id: string;
    code: string;
    label: string;
    description?: string;
    type: PendingFieldValue['type'];
    options?: { values?: string[] };
    required?: boolean;
    display_order: number;
    display_group: string;
  }

  async function toggleMeta() {
    metaOpen = !metaOpen;
    if (metaOpen && metaFields === null) {
      metaLoading = true;
      try {
        metaFields = await fieldsForAssetType(1); // Photo for MVP
      } catch (e) {
        metaError = e instanceof Error ? e.message : t('upload.file_row.fields_load_error');
      } finally {
        metaLoading = false;
      }
    }
  }

  // Grouped view of fields by display_group, ordered by display_order.
  const groupedFields = $derived.by(() => {
    if (!metaFields) return [] as { group: string; fields: FieldDef[] }[];
    const buckets = new Map<string, FieldDef[]>();
    for (const f of metaFields) {
      const g = f.display_group || 'general';
      if (!buckets.has(g)) buckets.set(g, []);
      buckets.get(g)!.push(f);
    }
    return Array.from(buckets.entries())
      .map(([group, fields]) => ({
        group,
        fields: fields.sort((a, b) => a.display_order - b.display_order),
      }))
      .sort((a, b) => a.group.localeCompare(b.group));
  });

  function getPending(f: FieldDef): PendingFieldValue {
    const existing = row.fieldValues.get(f.id);
    if (existing) return existing;
    const created: PendingFieldValue = { fieldId: f.id, type: f.type };
    row.fieldValues.set(f.id, created);
    return created;
  }

  // Save when the user moves out of an input. Triggers a Map
  // re-assign so Svelte's reactive tracking notices (Maps mutate
  // in place, which the reactivity system doesn't see).
  function commitField(f: FieldDef, partial: Partial<PendingFieldValue>) {
    const cur = getPending(f);
    const next = { ...cur, ...partial };
    // If every value_* is empty / null we can drop the entry.
    const empty =
      (next.valueText == null || next.valueText === '') &&
      next.valueNum == null &&
      (next.valueDate == null || next.valueDate === '') &&
      (next.valueOptions == null || next.valueOptions.length === 0) &&
      (next.valueRef == null || next.valueRef === '');
    if (empty) {
      row.fieldValues.delete(f.id);
    } else {
      row.fieldValues.set(f.id, next);
    }
    // Trigger reactivity.
    row.fieldValues = new Map(row.fieldValues);
  }

  const fieldCount = $derived(row.fieldValues.size);

  const pct = $derived(Math.round(row.progress * 100));

  const stateLabel = $derived(
    row.state === 'queued' ? t('upload.file_row.state_queued')
    : row.state === 'uploading' ? t('upload.file_row.state_uploading', { pct })
    : row.state === 'asset-creating' ? t('upload.file_row.state_finalizing')
    : row.state === 'ready' ? (row.deduped ? t('upload.file_row.state_deduped') : t('upload.file_row.state_ready'))
    : row.state === 'errored' ? t('upload.file_row.state_failed')
    : row.state,
  );

  const stateClass = $derived(
    row.state === 'ready' ? 'bg-success-container text-on-success-container'
    : row.state === 'errored' ? 'bg-danger-container text-on-danger-container'
    : row.state === 'queued' ? 'bg-surface-elevated text-fg-muted'
    : 'bg-accent/15 text-accent',
  );

  let tagDraft = $state('');

  function commitTag() {
    const t = tagDraft.trim().toLowerCase();
    if (!t) return;
    if (row.tags.includes(t)) {
      tagDraft = '';
      return;
    }
    row.tags = [...row.tags, t];
    tagDraft = '';
  }

  function removeTag(t: string) {
    row.tags = row.tags.filter((x) => x !== t);
  }

  function handleTagKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      commitTag();
    } else if (e.key === 'Backspace' && tagDraft === '' && row.tags.length > 0) {
      row.tags = row.tags.slice(0, -1);
    }
  }

  function humanSize(n: number): string {
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
    if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
    return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`;
  }
</script>

<div class="flex gap-3 rounded-lg border border-border bg-surface-elevated p-3">
  <!-- Thumb: the file as an inline blob URL. For non-image files
       the browser will fail to load — we fall back to a generic
       icon via onerror. -->
  <div class="relative h-16 w-16 shrink-0 overflow-hidden rounded bg-surface-elevated">
    <!-- svelte-ignore a11y_img_redundant_alt -->
    <img
      src={row.objectUrl}
      alt=""
      class="h-full w-full object-cover"
      onerror={(e) => {
        const img = e.currentTarget as HTMLImageElement;
        img.style.display = 'none';
      }}
    />
    <div class="absolute inset-0 -z-10 flex items-center justify-center text-fg-muted/40">
      <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
        <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
        <polyline points="14 2 14 8 20 8" />
      </svg>
    </div>
  </div>

  <div class="min-w-0 flex-1 space-y-2">
    <!-- Title (editable) + state badge + remove -->
    <div class="flex items-start gap-2">
      <input
        type="text"
        bind:value={row.title}
        placeholder={row.file.name}
        class="min-w-0 flex-1 rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        aria-label={t('upload.file_row.title_aria')}
      />
      <span class="inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium {stateClass}">
        {stateLabel}
      </span>
      <button
        type="button"
        onclick={() => upload.removeRow(row.id)}
        class="shrink-0 rounded p-1 text-fg-muted hover:bg-surface-elevated hover:text-fg"
        title={t('upload.file_row.remove_title')}
        aria-label={t('common.remove')}
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <path d="M18 6 6 18M6 6l12 12" />
        </svg>
      </button>
    </div>

    <!-- Progress bar + size -->
    {#if row.state === 'uploading' || row.state === 'asset-creating' || row.state === 'queued'}
      <div class="h-1.5 w-full overflow-hidden rounded-full bg-surface-elevated">
        <div
          class="h-full bg-accent transition-[width] duration-150"
          style="width: {row.state === 'queued' ? 0 : row.state === 'asset-creating' ? 100 : pct}%"
        ></div>
      </div>
    {/if}

    <div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-muted">
      <span>{humanSize(row.file.size)}</span>
      {#if row.deduped}
        <span class="text-success">· {t('upload.file_row.deduped_note')}</span>
      {/if}
      {#if row.error}
        <span class="text-danger">· {row.error}</span>
        <button
          type="button"
          onclick={() => upload.retryRow(row.id)}
          class="rounded border border-border px-2 py-0.5 text-xs hover:bg-surface-elevated"
        >
          {t('common.retry')}
        </button>
      {/if}
    </div>

    <!-- Tags (chip input). Collapsible feel via small footprint;
         per-asset tags are optional. -->
    <div class="flex flex-wrap items-center gap-1.5">
      {#each row.tags as tag (tag)}
        <span class="inline-flex items-center gap-1 rounded-full bg-surface-elevated px-2 py-0.5 text-xs text-fg">
          #{tag}
          <button
            type="button"
            onclick={() => removeTag(tag)}
            class="text-fg-muted hover:text-fg"
            aria-label={t('upload.file_row.remove_tag_aria', { tag })}
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
              <path d="M18 6 6 18M6 6l12 12" />
            </svg>
          </button>
        </span>
      {/each}
      <input
        type="text"
        bind:value={tagDraft}
        onkeydown={handleTagKeydown}
        onblur={commitTag}
        placeholder={row.tags.length === 0 ? t('upload.file_row.tag_placeholder') : '+'}
        class="min-w-[6rem] flex-1 rounded border border-transparent bg-transparent px-1.5 py-0.5 text-xs text-fg placeholder:text-fg-muted/60 focus-visible:ring-2 focus-visible:ring-ring focus:bg-surface-elevated focus:outline-none"
      />
    </div>

    <!-- Metadata disclosure. Lazy-loads field defs on first open;
         all per-field inputs persist into row.fieldValues which the
         store flushes via PUT /assets/{id}/fields/{field_id} after
         the asset is created. -->
    <details class="-mx-1 border-t border-border pt-2" open={metaOpen}>
      <!-- svelte-ignore a11y_no_redundant_roles -->
      <summary
        onclick={(e) => { e.preventDefault(); void toggleMeta(); }}
        class="cursor-pointer select-none px-1 text-xs text-fg-muted hover:text-fg"
      >
        {t('upload.file_row.metadata_summary')}{fieldCount > 0 ? ` (${fieldCount})` : ''}
      </summary>

      {#if metaOpen}
        <div class="mt-2 space-y-3 px-1">
          {#if metaLoading}
            <p class="text-xs text-fg-muted">{t('upload.file_row.loading_fields')}</p>
          {:else if metaError}
            <p class="text-xs text-danger">{metaError}</p>
          {:else if metaFields && metaFields.length === 0}
            <p class="text-xs text-fg-muted">{t('upload.file_row.no_fields')}</p>
          {:else}
            {#each groupedFields as g (g.group)}
              <fieldset class="space-y-1.5">
                <legend class="text-[10px] uppercase tracking-wider text-fg-muted">{g.group}</legend>
                {#each g.fields as f (f.id)}
                  {@const pending = row.fieldValues.get(f.id)}
                  <label class="block">
                    <span class="block text-xs text-fg-muted">{f.label}</span>
                    {#if f.type === 'text' || f.type === 'rich_text'}
                      <input
                        type="text"
                        value={pending?.valueText ?? ''}
                        onchange={(e) => commitField(f, { valueText: (e.currentTarget as HTMLInputElement).value })}
                        class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                      />
                    {:else if f.type === 'longtext'}
                      <textarea
                        value={pending?.valueText ?? ''}
                        onchange={(e) => commitField(f, { valueText: (e.currentTarget as HTMLTextAreaElement).value })}
                        rows="2"
                        class="mt-0.5 w-full resize-y rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                      ></textarea>
                    {:else if f.type === 'number'}
                      <input
                        type="number"
                        value={pending?.valueNum ?? ''}
                        onchange={(e) => {
                          const v = (e.currentTarget as HTMLInputElement).value;
                          commitField(f, { valueNum: v === '' ? null : Number(v) });
                        }}
                        class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                      />
                    {:else if f.type === 'boolean'}
                      <label class="mt-0.5 inline-flex items-center gap-2 text-sm">
                        <input
                          type="checkbox"
                          checked={pending?.valueText === 'true'}
                          onchange={(e) => commitField(f, { valueText: (e.currentTarget as HTMLInputElement).checked ? 'true' : 'false' })}
                          class="h-4 w-4 accent-accent"
                        />
                        <span class="text-fg-muted">{pending?.valueText === 'true' ? t('common.yes') : t('common.no')}</span>
                      </label>
                    {:else if f.type === 'date'}
                      <input
                        type="date"
                        value={pending?.valueDate?.slice(0, 10) ?? ''}
                        onchange={(e) => {
                          const v = (e.currentTarget as HTMLInputElement).value;
                          commitField(f, { valueDate: v === '' ? null : new Date(v).toISOString() });
                        }}
                        class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                      />
                    {:else if f.type === 'datetime'}
                      <input
                        type="datetime-local"
                        value={pending?.valueDate ? pending.valueDate.slice(0, 16) : ''}
                        onchange={(e) => {
                          const v = (e.currentTarget as HTMLInputElement).value;
                          commitField(f, { valueDate: v === '' ? null : new Date(v).toISOString() });
                        }}
                        class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                      />
                    {:else if f.type === 'select'}
                      <select
                        value={pending?.valueText ?? ''}
                        onchange={(e) => commitField(f, { valueText: (e.currentTarget as HTMLSelectElement).value || null })}
                        class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                      >
                        <option value="">—</option>
                        {#each (f.options?.values ?? []) as opt (opt)}
                          <option value={opt}>{opt}</option>
                        {/each}
                      </select>
                    {:else if f.type === 'multi_select'}
                      <!-- Render as a checkbox list — simple, no chip
                           UI dependency. For long option lists we'd
                           swap to a search-box later. -->
                      <div class="mt-0.5 flex flex-wrap gap-2 rounded border border-border bg-surface-elevated px-2 py-1.5">
                        {#each (f.options?.values ?? []) as opt (opt)}
                          {@const checked = (pending?.valueOptions ?? []).includes(opt)}
                          <label class="inline-flex items-center gap-1 text-xs">
                            <input
                              type="checkbox"
                              {checked}
                              onchange={(e) => {
                                const on = (e.currentTarget as HTMLInputElement).checked;
                                const cur = new Set(pending?.valueOptions ?? []);
                                if (on) cur.add(opt); else cur.delete(opt);
                                commitField(f, { valueOptions: cur.size === 0 ? null : Array.from(cur) });
                              }}
                              class="h-3 w-3 accent-accent"
                            />
                            {opt}
                          </label>
                        {/each}
                      </div>
                    {:else}
                      <p class="mt-0.5 rounded bg-surface-elevated px-2 py-1 text-xs text-fg-muted">
                        {t('upload.file_row.field_type_not_editable', { type: f.type })}
                      </p>
                    {/if}
                  </label>
                {/each}
              </fieldset>
            {/each}
          {/if}
        </div>
      {/if}
    </details>

    {#if isModelRow(row)}
      <!-- Companion files (OBJ → MTL + textures, glTF → .bin +
           textures, etc). Drag-drop or click-to-add; each pending
           companion shows its filename + editable relative path
           that the parent model file will reference at render
           time. Uploads happen sequentially after the parent asset
           is created — see uploadCompanions() in the store. -->
      <details class="-mx-1 border-t border-border pt-2" open={row.companions.length > 0}>
        <!-- svelte-ignore a11y_no_redundant_roles -->
        <summary class="cursor-pointer select-none px-1 text-xs text-fg-muted hover:text-fg">
          {t('upload.file_row.companions_summary')}{row.companions.length > 0 ? ` (${row.companions.length})` : ''}
        </summary>

        <div
          class="mt-2 space-y-2 px-1"
          ondragover={(e) => { e.preventDefault(); }}
          ondrop={onCompanionDrop}
        >
          <input
            bind:this={companionInput}
            type="file"
            multiple
            class="hidden"
            onchange={onCompanionPick}
          />

          <p class="text-[11px] leading-snug text-fg-muted">
            {t('upload.file_row.companions_help')}<code class="font-mono">textures/wood.png</code>).
          </p>

          {#each row.companions as c (c.id)}
            <div class="flex items-center gap-2 rounded bg-surface-elevated px-2 py-1.5">
              <span class="truncate text-xs text-fg-muted" title={c.file.name}>{c.file.name}</span>
              <input
                type="text"
                value={c.path}
                oninput={(e) => upload.setCompanionPath(row.id, c.id, (e.currentTarget as HTMLInputElement).value)}
                disabled={c.state === 'uploading' || c.state === 'done'}
                class="ml-auto w-44 rounded border border-border-strong bg-surface px-1.5 py-0.5 font-mono text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none disabled:opacity-60"
              />
              <span class="w-14 text-right text-[10px] uppercase tracking-wider"
                class:text-fg-muted={c.state === 'pending'}
                class:text-accent={c.state === 'uploading'}
                class:text-success={c.state === 'done'}
                class:text-danger={c.state === 'errored'}
                title={c.error ?? ''}
              >{c.state}</span>
              <button
                type="button"
                onclick={() => upload.removeCompanion(row.id, c.id)}
                disabled={c.state === 'uploading'}
                class="text-fg-muted hover:text-danger disabled:opacity-40"
                aria-label={t('upload.file_row.remove_companion_aria')}
              >×</button>
            </div>
          {/each}

          <button
            type="button"
            onclick={openCompanionPicker}
            class="text-xs text-accent hover:underline"
          >{t('upload.file_row.add_companion')}</button>
        </div>
      </details>
    {/if}
  </div>
</div>
