<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The full-page create surface (#1119 slice 1).
  //
  // ## What this is, and what it is not
  //
  // It REPLACES the upload modal for composing a post properly, and it
  // does not retire it: the modal stays as the quick path and both
  // surfaces drive the same store. Nothing about the pipeline is new
  // here — bytes, asset rows, field values, companions and post
  // creation are all upload.svelte.ts's, unchanged. What is new is the
  // page, and two things it is the first consumer of.
  //
  // ## ⛔ THE FRICTION LINE GOVERNS AND IS NOT NEGOTIABLE
  //
  // From empty: drop a file, press Publish. TWO actions, and no field
  // the artist did not choose to fill. Every section below that is not
  // those two is progressive — a disclosure, or an optional control
  // with a working default. The reference design ASKS a lot; this one
  // OFFERS a lot and REQUIRES almost nothing.
  //
  // That is why:
  //   - the post is created unconditionally (no "also make a post?"
  //     checkbox to find and tick),
  //   - the title is optional,
  //   - visibility and publish-status carry the defaults the modal
  //     already used,
  //   - and the Publish button is live the moment one file is ready.
  //
  // ## First consumer #1: `show_on_upload`
  //
  // #1173 gave every field a participation flag and nothing outside the
  // admin editor ever read it. This page does, via
  // `fieldsOfferedOnUpload`. Participation is OPT-OUT — absent or true
  // means offered — so an install that has never configured a field
  // sees exactly what it saw before.
  //
  // ## First consumer #2: the REAL asset type
  //
  // The modal asks for `fieldsForAssetType(1)` with the comment
  // "// Photo for MVP": one hardcoded type for every file, whatever it
  // is. The fix is NOT to mirror the server's extension table in the
  // browser — that table (`assetTypeFor`, assets/handler.go) is the
  // rule, and a copy of a rule is a second rule free to disagree with
  // the first. So this page ASKS: `POST /assets` returns the type the
  // server assigned, the store records it on the row, and the fields
  // offered are the ones for that type. Files of different types in one
  // drop each get their own set.
  //
  // ## Scheduling is NOT here, and its absence is deliberate
  //
  // #1119 says scheduled publish "rides the ADR 0020 scheduled-actions
  // machinery". It does not, yet: every mutating executor opens with
  // `assetTarget(a)` and refuses a post target, so a scheduled publish
  // would enqueue cleanly and fail at fire time. Tracked as #1238. Not
  // even a disabled control — an affordance that cannot work is worse
  // than none.

  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { site } from '$stores/site.svelte';
  import { t } from '$stores/lang.svelte';
  import {
    upload,
    fieldsForAssetType,
    fieldsOfferedOnUpload,
    type FieldDef,
    type UploadRow,
  } from '$stores/upload.svelte';
  import { VALUE_COLUMN } from '$lib/fieldOptions';
  import FieldValueInput from '$components/FieldValueInput.svelte';
  import AiProvenanceControl from '$components/AiProvenanceControl.svelte';
  import ThumbnailPicker from '$components/upload/ThumbnailPicker.svelte';
  import CompanionRequirementsNote from '$components/upload/CompanionRequirementsNote.svelte';

  let fileInputEl = $state<HTMLInputElement | null>(null);
  let dragOver = $state(false);
  let submitting = $state(false);
  let tagDraft = $state('');

  // Collections the caller can post into ("Albums" in the epic).
  interface CollectionRow {
    id: string;
    name: string;
  }
  let collections = $state<CollectionRow[]>([]);

  // ref → display name, for the media-type row. Fetched rather than
  // mapped in the browser for the same reason the asset type itself is:
  // the names are OPERATOR-CONFIGURED (`asset_types.name`), so a
  // hardcoded table here would be wrong on any install that renamed or
  // added one.
  let assetTypeNames = $state<Record<number, string>>({});

  onMount(() => {
    // Claim the shared store. The modal and this page are two views of
    // one queue, so arriving here with a half-finished modal drop would
    // otherwise silently adopt it — and `upload.open` stays false, so
    // the modal does not pop up over the page.
    upload.reset();
    upload.compose.enabled = true;
    // ⚠️ reset() PRESERVES collectionId on purpose — the modal is often
    // reopened from the collection page that spawned it. Arriving fresh
    // on this page is the other case, and inheriting a collection the
    // artist chose in some earlier modal would silently file their work
    // somewhere they were not looking. So the album comes from the URL
    // or from nowhere; `?collection=<id>` is how a collection page
    // hands off into the full flow.
    upload.compose.collectionId = page.url.searchParams.get('collection');
    void loadCollections();
    void loadAssetTypeNames();
  });

  async function loadAssetTypeNames() {
    try {
      const { data } = await api.GET('/asset_types', {});
      const out: Record<number, string> = {};
      for (const ty of (data ?? []) as { ref: number; name?: string | null }[]) {
        if (ty.name) out[ty.ref] = ty.name;
      }
      assetTypeNames = out;
    } catch {
      // The row falls back to nothing rather than to a guess.
    }
  }

  async function loadCollections() {
    try {
      const { data } = await api.GET('/collections', {
        params: { query: { tab: 'mine', limit: 100 } },
      });
      collections = ((data as { items?: CollectionRow[] } | null)?.items ?? []).map((c) => ({
        id: c.id,
        name: c.name,
      }));
    } catch {
      // Albums are optional; an empty list just means no picker.
    }
  }

  // ── files ────────────────────────────────────────────────────────

  function addFiles(files: FileList | File[] | null) {
    if (!files || files.length === 0) return;
    upload.addFiles(files);
  }

  function onDrop(e: DragEvent) {
    e.preventDefault();
    dragOver = false;
    addFiles(e.dataTransfer?.files ?? null);
  }

  const rows = $derived(upload.rows);
  const ready = $derived(upload.readyRows);

  // ── fields, per asset type actually present in the drop ──────────
  //
  // Keyed by asset type so a mixed drop (a PNG and a GLB) offers each
  // file's own fields rather than one arbitrary set for both.
  let fieldsByType = $state<Record<number, FieldDef[]>>({});
  let fieldsError = $state(false);

  // The distinct server-assigned types across the ready rows.
  const activeTypes = $derived(
    Array.from(
      new Set(
        rows
          .map((r) => r.assetType)
          .filter((v): v is number => typeof v === 'number'),
      ),
    ).sort((a, b) => a - b),
  );

  // Types already asked for. A PLAIN Set, deliberately not $state: the
  // effect below writes `fieldsByType`, so reading it there too would
  // make the effect its own dependency and re-run it on every answer.
  // That is the shape behind the 1,694-request loop — it terminates
  // here only because of the guard, which is not a property worth
  // relying on. The non-reactive ledger breaks the cycle outright.
  const requestedTypes = new Set<number>();

  $effect(() => {
    // Read the dependency in THIS frame. A $state read that only
    // happens inside a callee — or after an await — is not collected as
    // a dependency, and the effect would then never re-run when a new
    // asset type appears in the drop.
    const types = activeTypes;
    for (const ty of types) {
      if (requestedTypes.has(ty)) continue;
      requestedTypes.add(ty);
      void (async () => {
        try {
          const defs = await fieldsForAssetType(ty);
          fieldsByType = { ...fieldsByType, [ty]: fieldsOfferedOnUpload(defs) };
        } catch {
          // Let a retry happen if the artist adds another file of this
          // kind: a transient failure must not permanently hide the
          // fields for a whole asset type.
          requestedTypes.delete(ty);
          fieldsError = true;
        }
      })();
    }
  });

  /** The rows of one asset type, so a field section can name its files. */
  function rowsOfType(ty: number): UploadRow[] {
    return rows.filter((r) => r.assetType === ty);
  }

  /**
   * Field values live on the ROW (that is what the store writes), but
   * this page offers ONE control per field per asset type — asking an
   * artist the same question once per file is the batch "save-and-next"
   * UX in disguise, which is a hard no. So a change writes through to
   * every row of that type.
   */
  function fieldValueOf(ty: number, def: FieldDef) {
    const first = rowsOfType(ty)[0];
    const pending = first?.fieldValues.get(def.id);
    return {
      value_text: pending?.valueText ?? null,
      value_num: pending?.valueNum ?? null,
      value_date: pending?.valueDate ?? null,
      value_options: pending?.valueOptions ?? null,
      value_ref: pending?.valueRef ?? null,
    };
  }

  function setFieldValue(
    ty: number,
    def: FieldDef,
    v: {
      value_text?: string | null;
      value_num?: number | null;
      value_date?: string | null;
      value_options?: string[] | null;
      value_ref?: string | null;
    },
  ) {
    const column = VALUE_COLUMN[def.type];
    const empty =
      (v.value_text === null || v.value_text === undefined || v.value_text === '') &&
      (v.value_num === null || v.value_num === undefined) &&
      (v.value_date === null || v.value_date === undefined || v.value_date === '') &&
      (v.value_options === null || v.value_options === undefined || v.value_options.length === 0) &&
      (v.value_ref === null || v.value_ref === undefined || v.value_ref === '');

    for (const row of rowsOfType(ty)) {
      const next = new Map(row.fieldValues);
      if (empty) {
        // Emptying a control must REMOVE the pending write, not send a
        // blank one — the value endpoint refuses an empty value on a
        // required field, and a blank we never asked for would fail the
        // whole submit.
        next.delete(def.id);
      } else {
        next.set(def.id, {
          fieldId: def.id,
          label: def.label,
          type: def.type,
          valueText: column === 'value_text' ? (v.value_text ?? null) : null,
          valueNum: column === 'value_num' ? (v.value_num ?? null) : null,
          valueDate: column === 'value_date' ? (v.value_date ?? null) : null,
          valueOptions: column === 'value_options' ? (v.value_options ?? null) : null,
          valueRef: column === 'value_ref' ? (v.value_ref ?? null) : null,
        });
      }
      row.fieldValues = next;
      row.fieldsWritten = false;
    }
  }

  // ── the two self-labels ──────────────────────────────────────────
  //
  // Both are per-asset in the data model and asked ONCE here, applying
  // to every file in the drop. That is the artist's mental model — they
  // are describing the work, not each file of it — and it is what keeps
  // the interaction count at one.

  const matureAll = $derived(rows.length > 0 && rows.every((r) => r.mature));
  function setMature(v: boolean) {
    for (const r of rows) r.mature = v;
  }

  // The STORE owns this, not the page. Two reasons, and the first is a
  // bug this page had: writing only through to `rows` made the control
  // inert before any file was dropped, so an artist who declared first
  // and dropped second lost their answer without being told. The store
  // holds it on `compose` and every row inherits it at enqueue. The
  // second reason is ThumbnailPicker, which needs the same answer to
  // stamp a standalone cover — two copies would be two answers.
  const aiAll = $derived(upload.sharedAiProvenance);
  function setAi(v: 'none' | 'assisted' | 'generated' | null) {
    upload.setAiProvenance(v);
  }

  // ── tags ─────────────────────────────────────────────────────────

  function commitTag() {
    const raw = tagDraft.trim().toLowerCase();
    tagDraft = '';
    if (!raw) return;
    if (upload.compose.tags.includes(raw)) return;
    upload.compose.tags = [...upload.compose.tags, raw];
  }
  function removeTag(tag: string) {
    upload.compose.tags = upload.compose.tags.filter((x) => x !== tag);
  }
  function onTagKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      commitTag();
    } else if (e.key === 'Backspace' && tagDraft === '' && upload.compose.tags.length > 0) {
      upload.compose.tags = upload.compose.tags.slice(0, -1);
    }
  }

  // ── submit ───────────────────────────────────────────────────────

  const canSubmit = $derived(!submitting && !upload.composeBusy && ready.length > 0);

  async function publish(asDraft: boolean) {
    if (!canSubmit) return;
    upload.compose.draft = asDraft;
    submitting = true;
    try {
      const ok = await upload.submit();
      if (!ok) return;
      // Go to what was just made. A full-page create flow that drops
      // you back on an empty form has thrown away the only thing you
      // wanted from it.
      const id = upload.createdPostIds[0];
      await goto(id ? `/posts/${id}` : '/');
    } finally {
      submitting = false;
    }
  }
</script>

<svelte:head>
  <title>{t('create.title')} — {site.name}</title>
</svelte:head>

<div class="w-full px-4 py-6 sm:px-6" data-testid="create-page">
  <header class="mb-5">
    <nav class="text-xs text-fg-muted" aria-label={t('create.breadcrumb_aria')}>
      <!-- #1119's mock crumbs this "Manage portfolio". No such landing
           exists yet (`/account/drafts` is still a stub), and a crumb
           that 404s is worse than an honest one, so it points at the
           account surface that does exist. -->
      <a href="/account" class="hover:underline">{t('create.crumb_portfolio')}</a>
      <span class="px-1">/</span>
      <span>{t('create.crumb_new_post')}</span>
    </nav>
    <h1 class="mt-2 text-2xl font-semibold text-fg">{t('create.title')}</h1>
    <p class="mt-1 max-w-3xl text-sm text-fg-muted">{t('create.subtitle')}</p>
  </header>

  {#if !auth.user}
    <p class="rounded border border-border bg-surface-elevated p-4 text-sm" data-testid="create-signed-out">
      {t('create.signed_out')}
    </p>
  {:else}
    <div class="grid grid-cols-1 gap-6 lg:grid-cols-[minmax(0,1fr)_20rem]">
      <!-- ══════════ main column ══════════ -->
      <div class="min-w-0 space-y-6">
        <!-- Title. Optional, and says so — an artist who has nothing to
             call it yet must not be stopped here. -->
        <label class="block">
          <span class="mb-1 block text-xs font-medium text-fg-muted">
            {t('create.post_title')}
          </span>
          <input
            type="text"
            maxlength="500"
            bind:value={upload.compose.title}
            placeholder={t('create.post_title_placeholder')}
            data-testid="create-title"
            class="w-full rounded border border-border bg-surface px-3 py-2 text-sm text-fg"
          />
        </label>

        <!-- Dropzone + queue -->
        <section>
          <h2 class="mb-1 text-xs font-medium text-fg-muted">{t('create.files')}</h2>

          <!-- The media-type row (#1119).
               ⚠️ It REPORTS, it does not ASK. The reference design has
               the artist pick a media type before uploading; here the
               server already knows — `assetTypeFor` promotes by
               extension on POST /assets — so asking would be a second
               expression of that rule, free to disagree with it, and a
               question with a knowable answer is friction by
               definition. So this states what the file WAS taken to be,
               which is the part the artist actually needs (it decides
               which fields are offered below) and cannot be wrong. -->
          {#if activeTypes.length > 0}
            <div class="mb-2 flex flex-wrap gap-1.5" data-testid="create-media-types">
              {#each activeTypes as ty (ty)}
                <span
                  class="rounded-full bg-surface-elevated px-2 py-0.5 text-xs text-fg-muted"
                  data-testid="create-media-type-{ty}"
                >
                  {assetTypeNames[ty] ?? t('create.media_type_unknown')}
                  · {rowsOfType(ty).length}
                </span>
              {/each}
            </div>
          {/if}
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <div
            role="group"
            aria-label={t('create.files')}
            ondragover={(e) => {
              e.preventDefault();
              dragOver = true;
            }}
            ondragleave={() => (dragOver = false)}
            ondrop={onDrop}
            class="rounded border-2 border-dashed p-4 transition-colors {dragOver
              ? 'border-accent bg-accent-container/30'
              : 'border-border'}"
            data-testid="create-dropzone"
          >
            <input
              bind:this={fileInputEl}
              type="file"
              multiple
              class="hidden"
              data-testid="create-file-input"
              onchange={(e) => {
                addFiles((e.currentTarget as HTMLInputElement).files);
                (e.currentTarget as HTMLInputElement).value = '';
              }}
            />
            <button
              type="button"
              onclick={() => fileInputEl?.click()}
              class="w-full rounded px-3 py-6 text-sm text-fg-muted hover:text-fg"
              data-testid="create-add-files"
            >
              {t('create.dropzone')}
            </button>

            {#if rows.length > 0}
              <ul class="mt-3 space-y-2" data-testid="create-file-list">
                {#each rows as row (row.id)}
                  <li
                    class="rounded border border-border bg-surface p-2"
                    data-testid="create-file-row"
                  >
                    <div class="flex items-start gap-3">
                      <!-- Only an IMAGE has a renderable object URL. A
                           .glb, a .fbx or a .zip pointed at an <img>
                           paints the browser's broken-image glyph,
                           which reads as "your upload failed" beside a
                           row that says Ready. Show the extension
                           instead — it is the true thing we know. -->
                      {#if row.file.type.startsWith('image/')}
                        <img
                          src={row.objectUrl}
                          alt=""
                          class="h-12 w-12 shrink-0 rounded object-cover"
                        />
                      {:else}
                        <span
                          class="flex h-12 w-12 shrink-0 items-center justify-center rounded bg-surface-elevated text-[10px] font-medium uppercase text-fg-muted"
                          aria-hidden="true"
                        >
                          {row.file.name.includes('.')
                            ? row.file.name.slice(row.file.name.lastIndexOf('.') + 1)
                            : '?'}
                        </span>
                      {/if}
                      <div class="min-w-0 flex-1">
                        <input
                          type="text"
                          bind:value={row.title}
                          aria-label={t('create.file_title_aria')}
                          class="w-full truncate rounded bg-transparent text-sm text-fg"
                        />
                        <p class="text-xs text-fg-muted" data-testid="create-file-state">
                          {row.state === 'errored' ? (row.error ?? '') : t(`create.state_${row.state}`)}
                        </p>
                      </div>
                      <button
                        type="button"
                        onclick={() => upload.removeRow(row.id)}
                        aria-label={t('create.remove_file_aria')}
                        class="shrink-0 rounded px-2 py-1 text-xs text-fg-muted hover:text-fg"
                      >
                        ×
                      </button>
                    </div>
                    <!-- #754 — what this model still needs, by name. -->
                    <div class="mt-2">
                      <CompanionRequirementsNote requirements={row.requirements} testid={row.id} />
                    </div>
                  </li>
                {/each}
              </ul>
            {/if}
          </div>
        </section>

        <!-- Description -->
        <label class="block">
          <span class="mb-1 block text-xs font-medium text-fg-muted">
            {t('create.description')}
          </span>
          <textarea
            rows="5"
            bind:value={upload.compose.description}
            placeholder={t('create.description_placeholder')}
            data-testid="create-description"
            class="w-full rounded border border-border bg-surface px-3 py-2 text-sm text-fg"
          ></textarea>
        </label>

        <!-- The two self-labels. NOT inside a disclosure: a label
             nobody opens is a label nobody sets, and both of these are
             single decisions with an honest empty state. -->
        <section class="rounded border border-border p-3" data-testid="create-about-work">
          <h2 class="mb-2 text-xs font-medium text-fg-muted">{t('create.about_work')}</h2>
          <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div>
              <!-- Hidden when the operator has switched mature content
                   off: the server refuses `mature: true` with a 400
                   there, so the control would offer a choice that fails
                   on save. -->
              {#if auth.user?.matureContentAllowed}
                <span class="mb-1 block text-xs font-medium text-fg-muted">
                  {t('create.mature_legend')}
                </span>
                <label class="inline-flex items-start gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={matureAll}
                    onchange={(e) => setMature((e.currentTarget as HTMLInputElement).checked)}
                    data-testid="create-mature"
                    class="mt-0.5 h-4 w-4 shrink-0 accent-accent"
                  />
                  <span class="text-fg-muted">{t('upload.file_row.mature')}</span>
                </label>
              {/if}
            </div>
            <AiProvenanceControl value={aiAll} testid="create" onchange={setAi} />
          </div>
        </section>

        <!-- Categorization / software used / everything else the
             operator configured. These ARE progressive: a disclosure,
             closed, because the set can be large and none of it is
             required. -->
        {#if activeTypes.length > 0}
          <details class="rounded border border-border" data-testid="create-fields">
            <summary class="cursor-pointer px-3 py-2 text-sm font-medium text-fg">
              {t('create.fields_summary')}
            </summary>
            <div class="space-y-5 border-t border-border p-3">
              {#if fieldsError}
                <p class="text-sm text-danger" data-testid="create-fields-error">
                  {t('upload.file_row.fields_load_error')}
                </p>
              {/if}
              {#each activeTypes as ty (ty)}
                {@const defs = fieldsByType[ty] ?? []}
                <div data-testid="create-fields-type-{ty}">
                  {#if activeTypes.length > 1}
                    <p class="mb-2 text-xs text-fg-muted">
                      {t('create.fields_for_type', { count: rowsOfType(ty).length })}
                    </p>
                  {/if}
                  {#if defs.length === 0}
                    <p class="text-sm text-fg-muted" data-testid="create-fields-empty-{ty}">
                      {t('upload.file_row.no_fields')}
                    </p>
                  {:else}
                    <div class="space-y-4">
                      {#each defs as def (def.id)}
                        <div data-testid="create-field-{def.code}">
                          <FieldValueInput
                            def={{ ...def, required: def.required === true }}
                            value={fieldValueOf(ty, def)}
                            serverVocabulary
                            onchange={(v) => setFieldValue(ty, def, v)}
                          />
                        </div>
                      {/each}
                    </div>
                  {/if}
                </div>
              {/each}
            </div>
          </details>
        {/if}

        <!-- Tags -->
        <section>
          <span class="mb-1 block text-xs font-medium text-fg-muted">{t('create.tags')}</span>
          <div
            class="flex flex-wrap items-center gap-1 rounded border border-border bg-surface px-2 py-1.5"
          >
            {#each upload.compose.tags as tag (tag)}
              <span class="inline-flex items-center gap-1 rounded bg-surface-elevated px-2 py-0.5 text-xs">
                {tag}
                <button
                  type="button"
                  onclick={() => removeTag(tag)}
                  aria-label={t('create.remove_tag_aria', { tag })}
                  class="text-fg-muted hover:text-fg">×</button
                >
              </span>
            {/each}
            <input
              type="text"
              bind:value={tagDraft}
              onkeydown={onTagKeydown}
              onblur={commitTag}
              placeholder={t('create.tags_placeholder')}
              data-testid="create-tag-input"
              class="min-w-24 flex-1 bg-transparent text-sm text-fg outline-none"
            />
          </div>
        </section>
      </div>

      <!-- ══════════ right rail ══════════ -->
      <aside class="space-y-4 lg:sticky lg:top-4 lg:self-start" data-testid="create-rail">
        <!-- Visibility. Default `org-only` — the hidden-by-default
             posture at upload, kept deliberately: the reference DAM
             hides access and status on upload by default and so do we.
             The artist opens up on purpose, never by omission. -->
        <label class="block">
          <span class="mb-1 block text-xs font-medium text-fg-muted">
            {t('create.visibility')}
          </span>
          <select
            bind:value={upload.compose.visibility}
            data-testid="create-visibility"
            class="w-full rounded border border-border bg-surface px-2 py-1.5 text-sm text-fg"
          >
            <option value="org-only">{t('create.vis_org_only')}</option>
            <option value="followers">{t('create.vis_followers')}</option>
            <option value="private">{t('create.vis_private')}</option>
            <option value="public">{t('create.vis_public')}</option>
          </select>
          <span class="mt-1 block text-xs text-fg-muted">{t('create.visibility_help')}</span>
        </label>

        <!-- Project thumbnail — a POINTER at an asset (ADR 0088), never
             a bespoke upload path. ThumbnailPicker owns both modes. -->
        <details class="rounded border border-border" data-testid="create-thumbnail">
          <summary class="cursor-pointer px-3 py-2 text-sm font-medium text-fg">
            {t('create.thumbnail_summary')}
          </summary>
          <div class="border-t border-border p-3">
            <ThumbnailPicker />
          </div>
        </details>

        <!-- Albums. A collection holds POSTS (ADR 0091), and this page
             makes one by construction, so membership is a plain select
             rather than the "you need a post first" dance. -->
        {#if collections.length > 0}
          <label class="block" data-testid="create-album">
            <span class="mb-1 block text-xs font-medium text-fg-muted">{t('create.album')}</span>
            <select
              value={upload.compose.collectionId ?? ''}
              onchange={(e) =>
                (upload.compose.collectionId =
                  (e.currentTarget as HTMLSelectElement).value || null)}
              data-testid="create-album-select"
              class="w-full rounded border border-border bg-surface px-2 py-1.5 text-sm text-fg"
            >
              <option value="">{t('create.album_none')}</option>
              {#each collections as c (c.id)}
                <option value={c.id}>{c.name}</option>
              {/each}
            </select>
          </label>
        {/if}

        <!-- Publish. Draft or now — NOT schedule (#1238). -->
        <div class="space-y-2 rounded border border-border p-3">
          <span class="block text-xs font-medium text-fg-muted">{t('create.publish_status')}</span>
          {#if upload.composeError}
            <p class="text-sm text-danger" role="alert" data-testid="create-error">
              {upload.composeError}
            </p>
          {/if}
          <button
            type="button"
            disabled={!canSubmit}
            onclick={() => publish(false)}
            data-testid="create-publish"
            class="w-full rounded bg-accent px-3 py-2 text-sm font-medium text-on-accent disabled:opacity-50"
          >
            {t('create.publish_now')}
          </button>
          <button
            type="button"
            disabled={!canSubmit}
            onclick={() => publish(true)}
            data-testid="create-save-draft"
            class="w-full rounded border border-border px-3 py-2 text-sm text-fg disabled:opacity-50"
          >
            {t('create.save_draft')}
          </button>
          <p class="text-xs text-fg-muted">{t('create.publish_help')}</p>
        </div>
      </aside>
    </div>
  {/if}
</div>
