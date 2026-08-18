// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Upload store — runed singleton, owns the upload modal's state.
//
// Design goals (locked in the Phase 1.13.D-2b plan):
//   1. The progress bar IS the upload. The moment a file appears in
//      the queue the runner starts pushing bytes. No "click upload"
//      step; the click that opens the modal is the only commit.
//   2. Drag files anywhere on the page → modal opens with the files
//      pre-loaded. Listeners are global, mounted once by the layout.
//   3. Background uploading. Closing the modal does NOT abort
//      in-flight uploads — they finish in the background; queued
//      files that haven't started are dropped (the user changed
//      their mind before any byte left the wire).
//   4. Per-file state machine. Reactive at the file level so
//      changing one row's title doesn't trigger a list-wide
//      re-render.
//
// The runner uses XMLHttpRequest because `fetch` still doesn't
// expose upload-progress events as of writing. Concurrency cap of 3
// is empirical — enough to saturate a typical home upstream without
// stalling slow connections behind faster ones.

import { api } from '$api/client';
import { t } from '$stores/lang.svelte';
import type { components } from '$api/schema';
import type { FieldDefault } from '$lib/fieldDefaults';
import { putStorageObject } from '$lib/util/storageUpload';

type AssetCreate = components['schemas']['AssetCreate'];

// What a per-asset field value carries before the asset exists.
// Mirrors AssetFieldValueWrite but indexed by field_id locally so the
// upload row can carry its set of pending writes and we apply them
// after the asset is created.
export interface PendingFieldValue {
  fieldId: string;
  /**
   * The field's human label, carried so a refusal can name the field
   * the way the row does. The 422 body names it by CODE, and
   * "mtv_keywords refused that" is not what the person typing into a
   * box labelled "Keywords" needs to read.
   */
  label?: string;
  type:
    | 'text' | 'longtext' | 'rich_text' | 'number' | 'boolean'
    | 'date' | 'datetime' | 'select' | 'multi_select' | 'tree' | 'reference';
  valueText?: string | null;
  valueNum?: number | null;
  valueDate?: string | null;
  valueOptions?: string[] | null;
  valueRef?: string | null;
}

// ---- Types ----------------------------------------------------------------

export type UploadRowState =
  | 'queued'       // accepted, not yet started
  | 'uploading'    // PUT /storage/objects in flight
  | 'hashed'       // bytes uploaded, asset row about to be created
  | 'asset-creating' // POST /assets in flight
  | 'ready'        // asset_id assigned, this row is done
  | 'errored';     // see `error`; retry button surfaces in the UI

export interface UploadRow {
  /** Stable per-row id. Used as the key in Svelte each blocks. */
  readonly id: string;
  /** The original File object — drives the inline thumbnail via URL.createObjectURL. */
  readonly file: File;
  /** Object URL for the preview thumb. Revoked when the row is removed. */
  readonly objectUrl: string;

  // Mutable, reactive fields.
  state: UploadRowState;
  /** 0..1 — drives the progress bar. */
  progress: number;
  /** sha256 of the uploaded bytes, set on `hashed`. */
  hash: string | null;
  /** Asset UUID after POST /assets succeeds. */
  assetId: string | null;
  /** Server said this hash was already pinned somewhere. Cosmetic — we still got an asset. */
  deduped: boolean;
  /** User-overrideable per-row title (defaults to filename without extension). */
  title: string;
  /** Per-row tag chips. */
  tags: string[];
  /**
   * The artist's own MATURE label for this asset (#1116, ADR 0090).
   *
   * ONE checkbox, default false — the minimal-friction bar the upload
   * flow is held to. It is a RATING, not a clearance: it says what the
   * work is, not who may see it, and it is orthogonal to sensitivity.
   *
   * Sent only when the instance allows mature content. On an install
   * that disallows it the control is not rendered, so this stays false
   * and the create body carries `mature: false` — which every instance
   * accepts, unlike `true`, which is a 400.
   */
  mature: boolean;
  /** Last error message, for the retry surface. */
  error: string | null;
  /**
   * Per-asset metadata field values. Optional: empty by default, the
   * user opens the "Metadata" disclosure on this row to populate it.
   * Keyed by field_id. Written via PUT /assets/{id}/fields/{field_id}
   * after the asset is created — runRow handles the sequencing.
   */
  fieldValues: Map<string, PendingFieldValue>;
  /** True after EVERY per-asset field value has been written. */
  fieldsWritten: boolean;
  /**
   * Why individual field writes were refused, keyed by field id.
   * Rendered next to the offending input and summarised on the row —
   * #843. Before it, writeFieldValues discarded the server's answer
   * without reading it, so a 422 from the vocabulary gate vanished
   * while the upload reported success.
   */
  fieldErrors: Map<string, string>;
  /**
   * Companion files (OBJ → MTL + textures, glTF → .bin + textures,
   * etc.) the user attaches alongside a 3D model upload. Each
   * companion gets POSTed to /assets/{assetId}/companions after
   * the main asset row is created. Empty for non-3D uploads.
   */
  companions: PendingCompanion[];
  /** True after all companions have been uploaded. */
  companionsWritten: boolean;
}

export interface PendingCompanion {
  /** Stable id for Svelte each blocks. */
  readonly id: string;
  /** The companion's bytes — required. */
  readonly file: File;
  /** Relative path the model file references this companion at.
      Defaults to file.name; user can edit to add subdirectories
      ('textures/foo.png'). */
  path: string;
  state: 'pending' | 'uploading' | 'done' | 'errored';
  error: string | null;
}

export type PostMode = 'one-post' | 'one-per-file' | 'no-post';

export interface PostComposeState {
  enabled: boolean;          // false = "Just upload as assets — no post"
  mode: PostMode;            // when enabled
  title: string;
  description: string;
  /** Post visibility tier.
   *
   *  'public' was missing from this union while PostComposeForm's
   *  <select> has always offered it (#1176). Nothing narrowed at
   *  runtime — TS types are erased, and svelte-check does not check a
   *  `bind:value` against the <option> values it is bound to, so the
   *  mismatch was silent in both directions. The value it named was
   *  nonetheless refused end to end: POST /posts answered 400 because
   *  the server's write gate reserved the tier, and the schema this
   *  union mirrors did not list it either. Widening all three is what
   *  makes the option real. */
  visibility: 'public' | 'private' | 'org-only' | 'followers' | 'explicit-share';
  tags: string[];
  /** Optional collection to add the post(s) to. */
  collectionId: string | null;
  /** Workflow state UUID (post domain). Null = use server default. */
  stateId: string | null;
  /** Thumbnail strategy. 'member' = first ready row; 'separate' = the standalone-cover asset. */
  thumbMode: 'member' | 'separate';
  /** Which member-row's asset to use as cover when thumbMode === 'member'. */
  thumbMemberRowId: string | null;
  /** Asset id of the "uploaded as a custom cover" row when thumbMode === 'separate'. */
  thumbSeparateAssetId: string | null;
}

interface OpenContext {
  /** When opened on a collection page, prefill the compose form. */
  collectionId?: string | null;
  /** When opened on a team-scoped surface. */
  teamId?: string | null;
}

// ---- Constants ------------------------------------------------------------

const CONCURRENCY = 3;

// Default asset_type. Photo = 1 (legacy convention); we don't have a smarter
// MIME-to-asset_type mapping yet, so everything goes in as Photo
// for the MVP. The processing pipeline will set the right one once
// it lands.
export const DEFAULT_ASSET_TYPE = 1;

// ---- The store ------------------------------------------------------------

class UploadState {
  /** Modal open state. */
  open = $state(false);

  /** Active drag count. >0 = drop overlay visible. */
  dragDepth = $state(0);

  /** Files in the queue + post compose form. Reactive. */
  rows = $state<UploadRow[]>([]);

  compose = $state<PostComposeState>({
    enabled: true,
    mode: 'one-post',
    title: '',
    description: '',
    visibility: 'org-only',
    tags: [],
    collectionId: null,
    stateId: null,
    thumbMode: 'member',
    thumbMemberRowId: null,
    thumbSeparateAssetId: null,
  });

  /** Set by openWithFiles({ collectionId, teamId }) — informational, used at post-create time. */
  contextTeamId = $state<string | null>(null);

  // Final-step state — the POST /posts call(s) after every file is ready.
  composeBusy = $state(false);
  composeError = $state<string | null>(null);

  /** Rows that have finished uploading and have asset_ids. */
  get readyRows(): UploadRow[] {
    return this.rows.filter((r) => r.state === 'ready' && r.assetId);
  }

  /** True while any row is still in-flight or queued. */
  get anyInFlight(): boolean {
    return this.rows.some(
      (r) => r.state === 'queued' || r.state === 'uploading' || r.state === 'asset-creating',
    );
  }

  // ---- Open / close -----------------------------------------------------

  /** Open the modal without preloading any files (Upload button click). */
  open_(ctx: OpenContext = {}): void {
    this.applyContext(ctx);
    this.open = true;
  }

  /** Drop-anywhere path — opens with files queued and uploads started. */
  openWithFiles(files: FileList | File[], ctx: OpenContext = {}): void {
    this.applyContext(ctx);
    this.open = true;
    this.enqueue(files);
  }

  /** Add more files to an already-open modal (the drop-zone inside the modal). */
  addFiles(files: FileList | File[]): void {
    this.enqueue(files);
  }

  /** Close the modal. In-flight uploads continue; queued rows are dropped. */
  close(): void {
    this.open = false;
    // Drop everything that hasn't started or has finished. Keep
    // in-flight rows around so their progress can be inspected if
    // the modal is reopened — though today nothing surfaces them
    // outside the modal, so this is largely defensive.
    this.rows = this.rows.filter(
      (r) => r.state === 'uploading' || r.state === 'asset-creating',
    );
    this.resetCompose();
  }

  /** Hard reset — drops every row + posts state. Called after a successful submit. */
  reset(): void {
    for (const r of this.rows) {
      URL.revokeObjectURL(r.objectUrl);
    }
    this.rows = [];
    this.composeBusy = false;
    this.composeError = null;
    this.resetCompose();
    this.open = false;
  }

  removeRow(id: string): void {
    const row = this.rows.find((r) => r.id === id);
    if (!row) return;
    URL.revokeObjectURL(row.objectUrl);
    this.rows = this.rows.filter((r) => r.id !== id);
    // Compose references may now point at a dead row; clear them.
    if (this.compose.thumbMemberRowId === id) this.compose.thumbMemberRowId = null;
  }

  retryRow(id: string): void {
    const row = this.rows.find((r) => r.id === id);
    if (!row || row.state !== 'errored') return;
    row.error = null;
    row.progress = 0;
    row.state = 'queued';
    this.kick();
  }

  // ---- Per-row companion helpers ----------------------------------------

  /** Append companion files to a row. Path defaults to the filename;
      the user can rename it inline before the row uploads. */
  addCompanions(rowId: string, files: FileList | File[]): void {
    const row = this.rows.find((r) => r.id === rowId);
    if (!row) return;
    const incoming = Array.from(files);
    for (const file of incoming) {
      row.companions.push({
        id: globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36).slice(2),
        file,
        path: file.name,
        state: 'pending',
        error: null,
      });
    }
  }

  removeCompanion(rowId: string, companionId: string): void {
    const row = this.rows.find((r) => r.id === rowId);
    if (!row) return;
    row.companions = row.companions.filter((c) => c.id !== companionId);
  }

  setCompanionPath(rowId: string, companionId: string, path: string): void {
    const row = this.rows.find((r) => r.id === rowId);
    if (!row) return;
    const c = row.companions.find((c) => c.id === companionId);
    if (c) c.path = path;
  }

  // ---- Drag handling (window-level) -------------------------------------

  /**
   * Wire global dragenter / dragover / dragleave / drop listeners.
   * Call once from +layout.svelte (idempotent via the dedupe flag).
   * The drop opens the modal with the dropped files; drag enter/leave
   * count drives the visible overlay.
   */
  installGlobalDragListeners(): () => void {
    if (this._dragListenersInstalled) return () => {};
    this._dragListenersInstalled = true;

    const onDragEnter = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      this.dragDepth += 1;
    };
    const onDragOver = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
    };
    const onDragLeave = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      this.dragDepth = Math.max(0, this.dragDepth - 1);
    };
    const onDrop = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      this.dragDepth = 0;
      const files = e.dataTransfer?.files;
      if (files && files.length > 0) {
        this.openWithFiles(files);
      }
    };

    window.addEventListener('dragenter', onDragEnter);
    window.addEventListener('dragover', onDragOver);
    window.addEventListener('dragleave', onDragLeave);
    window.addEventListener('drop', onDrop);

    return () => {
      window.removeEventListener('dragenter', onDragEnter);
      window.removeEventListener('dragover', onDragOver);
      window.removeEventListener('dragleave', onDragLeave);
      window.removeEventListener('drop', onDrop);
      this._dragListenersInstalled = false;
    };
  }

  // ---- Final submit -----------------------------------------------------

  /**
   * Run the post-creation step(s) after every row is ready. The user
   * clicked the submit button. Returns true on success (and resets
   * + closes the modal); false on failure (and leaves the modal open
   * with composeError set).
   */
  async submit(): Promise<boolean> {
    if (this.composeBusy) return false;
    if (this.anyInFlight) {
      this.composeError = t('upload.err_wait_finish');
      return false;
    }
    const ready = this.readyRows;
    if (ready.length === 0) {
      this.composeError = t('upload.err_no_files');
      return false;
    }

    this.composeError = null;
    this.composeBusy = true;
    try {
      // Flush per-row field values BEFORE creating any posts. Each
      // row's writes are independent — one bad field doesn't abort the
      // rest — but a refusal STOPS the submit (#843).
      //
      // It has to. The refusals are rendered on the rows, and a
      // successful submit resets the modal: reporting the problem and
      // then destroying the surface reporting it is the silent failure
      // this is fixing, one step further down. So the modal stays open
      // with the offending fields marked, and the operator fixes the
      // value and submits again — the writes are idempotent PUTs, so
      // the retry re-sends the whole row and the ones that already
      // landed simply land again.
      let refused = false;
      for (const row of ready) {
        if (row.fieldValues.size > 0 && !row.fieldsWritten) {
          row.fieldsWritten = await this.writeFieldValues(row);
          if (!row.fieldsWritten) refused = true;
        }
      }
      if (refused) {
        this.composeError = t('upload.err_field_values');
        return false;
      }
      if (this.compose.enabled) {
        await this.createPosts(ready);
      }
      this.reset();
      return true;
    } catch (e) {
      this.composeError = e instanceof Error ? e.message : t('upload.err_create_post');
      return false;
    } finally {
      this.composeBusy = false;
    }
  }

  // ---------------------------------------------------------------------
  // Internals
  // ---------------------------------------------------------------------

  private _dragListenersInstalled = false;

  private applyContext(ctx: OpenContext): void {
    if (ctx.collectionId !== undefined) this.compose.collectionId = ctx.collectionId;
    if (ctx.teamId !== undefined) this.contextTeamId = ctx.teamId ?? null;
  }

  private resetCompose(): void {
    this.compose = {
      enabled: true,
      mode: 'one-post',
      title: '',
      description: '',
      visibility: 'org-only',
      tags: [],
      collectionId: this.compose.collectionId, // preserve context across resets
      stateId: null,
      thumbMode: 'member',
      thumbMemberRowId: null,
      thumbSeparateAssetId: null,
    };
  }

  private enqueue(files: FileList | File[]): void {
    const arr = Array.from(files);
    if (arr.length === 0) return;
    const additions: UploadRow[] = [];
    for (const file of arr) {
      const id = (globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36).slice(2));
      additions.push({
        id,
        file,
        objectUrl: URL.createObjectURL(file),
        state: 'queued',
        progress: 0,
        hash: null,
        assetId: null,
        deduped: false,
        title: defaultTitleFromFilename(file.name),
        tags: [],
        // OFF by default, per ADR 0090 §2 and the owner's minimal-
        // friction bar. A default of true would mislabel the library.
        mature: false,
        error: null,
        fieldValues: new Map(),
        fieldsWritten: false,
        fieldErrors: new Map(),
        companions: [],
        companionsWritten: false,
      });
    }
    this.rows = [...this.rows, ...additions];
    this.kick();
  }

  /** Run the concurrency runner — fill empty slots from the queued backlog. */
  private kick(): void {
    let active = this.rows.filter((r) => r.state === 'uploading' || r.state === 'asset-creating').length;
    for (const row of this.rows) {
      if (active >= CONCURRENCY) break;
      if (row.state !== 'queued') continue;
      active += 1;
      void this.runRow(row);
    }
  }

  private async runRow(row: UploadRow): Promise<void> {
    try {
      row.state = 'uploading';
      row.progress = 0;
      const result = await this.uploadBytes(row);
      row.hash = result.hash;
      row.deduped = !!result.deduped;
      row.state = 'asset-creating';

      const body: AssetCreate = {
        title: row.title || row.file.name,
        asset_type: DEFAULT_ASSET_TYPE,
        status: 'draft',
        file_hash: row.hash,
        file_extension: extensionOf(row.file.name),
        tags: row.tags,
        // #1116 — the self-label. Always sent, including as `false`:
        // sending nothing would be indistinguishable from an older
        // client, and `false` is accepted on every instance while `true`
        // is refused with a 400 where the operator has switched the
        // feature off.
        mature: row.mature,
        // Legacy-derived: stuff the upload context into the asset's
        // metadata JSONB so it's preserved even before the proper
        // field_value extraction lands. This mirrors what the legacy
        // resource_log + autocomplete macros capture at upload
        // time (filename, size). Real EXIF / IPTC / XMP parsing
        // lives in the async pipeline (Phase 1.15).
        metadata: {
          original_filename: row.file.name,
          original_size_bytes: row.file.size,
        },
      };
      const { data, error } = await api.POST('/assets', { body });
      if (error || !data) {
        throw new Error(extractError(error) ?? t('upload.err_create_asset'));
      }
      row.assetId = data.id;
      // Companions get uploaded immediately so the asset is ready
      // to render (the preview worker queues on field-value-write
      // time, and a 3D worker needs its textures staged before
      // Blender runs). Per-companion failures are non-fatal — the
      // asset still ends in `ready`; the user can re-add a missing
      // companion later.
      if (row.companions.length > 0) {
        await this.uploadCompanions(row);
      }
      // Per-asset metadata: the user may keep editing the field
      // values after the row is ready (the disclosure is collapsed
      // by default so most users open it AFTER the upload finishes).
      // We flush field values at submit time instead of here.
      row.state = 'ready';
    } catch (e) {
      row.error = e instanceof Error ? e.message : t('upload.err_upload_failed');
      row.state = 'errored';
    } finally {
      this.kick();
    }
  }

  /**
   * Apply the row's pending per-asset field values via
   * PUT /assets/{id}/fields/{field_id}. Returns true when every write
   * landed.
   *
   * #843. This used to `await fetch(...)` inside a bare try/catch and
   * never look at the result — so a 422 from the vocabulary gate (and
   * every other refusal the endpoint can return) was discarded on the
   * floor while the upload went on to report success. The comment
   * excusing it promised the operator could "edit the field value from
   * the asset detail page later", which is not true: there is no asset
   * edit surface yet (#549), so a value dropped here was dropped for
   * good, silently.
   *
   * Each write is still independent — one refused field does not stop
   * the others being attempted, because the operator wants to see ALL
   * of the problems, not the first one. The caller decides what a
   * refusal means for the submit.
   */
  private async writeFieldValues(row: UploadRow): Promise<boolean> {
    const aid = row.assetId;
    if (!aid) return false;
    const errors = new Map<string, string>();
    for (const v of row.fieldValues.values()) {
      const body: Record<string, unknown> = { set_by: 'manual' };
      if (typeof v.valueText === 'string') body.value_text = v.valueText;
      if (typeof v.valueNum === 'number') body.value_num = v.valueNum;
      if (typeof v.valueDate === 'string') body.value_date = v.valueDate;
      if (Array.isArray(v.valueOptions)) body.value_options = v.valueOptions;
      if (typeof v.valueRef === 'string') body.value_ref = v.valueRef;
      const named = v.label || v.fieldId;
      try {
        // Plain fetch — openapi-fetch's PUT for this endpoint hit a
        // type/runtime mismatch we couldn't track down quickly. The
        // shape is small and stable so this is fine.
        const res = await fetch(`/api/v1/assets/${aid}/fields/${v.fieldId}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify(body),
        });
        if (!res.ok) {
          errors.set(v.fieldId, await describeFieldRefusal(res, named));
        }
      } catch {
        // The request never completed — offline, or the tab lost the
        // network mid-submit. Honest and generic; we know nothing more
        // than that it did not go.
        errors.set(v.fieldId, t('upload.field_error.network', { field: named }));
      }
    }
    row.fieldErrors = errors;
    return errors.size === 0;
  }

  /**
   * Single-file upload. The XHR itself lives in
   * `$lib/util/storageUpload` since #1207 gave the cover editor a
   * second caller — see that module for why the bytes are shared and
   * the AssetCreate body deliberately is not.
   */
  private uploadBytes(row: UploadRow): Promise<{ hash: string; deduped?: boolean }> {
    return putStorageObject(row.file, {
      onProgress: (f) => {
        row.progress = f;
      },
      networkMessage: t('upload.err_network'),
      abortMessage: t('upload.err_aborted'),
    });
  }

  /**
   * Sequentially upload each pending companion to
   * POST /assets/{assetId}/companions with the companion's bytes
   * + X-Companion-Path + X-Content-Type headers. Each companion is
   * attempted independently — one failure doesn't block the rest
   * or fail the parent asset (the model just renders without that
   * texture).
   */
  private async uploadCompanions(row: UploadRow): Promise<void> {
    const aid = row.assetId;
    if (!aid) return;
    for (const c of row.companions) {
      if (c.state === 'done') continue;
      c.state = 'uploading';
      c.error = null;
      try {
        const ct = c.file.type || 'application/octet-stream';
        const res = await fetch(`/api/v1/assets/${aid}/companions`, {
          method: 'POST',
          credentials: 'include',
          headers: {
            'Content-Type': 'application/octet-stream',
            'X-Companion-Path': c.path,
            'X-Content-Type': ct,
          },
          body: c.file,
        });
        if (!res.ok) {
          const body = await res.text();
          throw new Error(body || `HTTP ${res.status}`);
        }
        c.state = 'done';
      } catch (e) {
        c.error = e instanceof Error ? e.message : t('upload.err_companion_failed');
        c.state = 'errored';
      }
    }
    row.companionsWritten = true;
  }

  /**
   * Final-step orchestrator. Branches on compose.mode:
   *   - one-post:      one POST /posts with every ready row as a member
   *   - one-per-file:  N POST /posts, one per ready row
   *   - (no-post is handled by checking compose.enabled in submit())
   *
   * Cover thumbnail resolution:
   *   - thumbMode 'member'    → cover_asset_id = the row's asset_id;
   *                             cover_thumbnail_asset_id omitted.
   *   - thumbMode 'separate'  → cover_asset_id = first member (default);
   *                             cover_thumbnail_asset_id = the standalone
   *                             asset uploaded for this purpose.
   */
  private async createPosts(ready: UploadRow[]): Promise<void> {
    const c = this.compose;
    if (c.mode === 'one-per-file') {
      for (const row of ready) {
        await this.createOnePost([row]);
      }
    } else {
      await this.createOnePost(ready);
    }
  }

  private async createOnePost(members: UploadRow[]): Promise<void> {
    const c = this.compose;
    const memberIds = members.map((r, i) => ({
      asset_id: r.assetId as string,
      sort_order: i,
    }));

    let coverAssetId: string | undefined;
    let coverThumbnailAssetId: string | undefined;

    if (c.thumbMode === 'member' && c.thumbMemberRowId) {
      const r = members.find((m) => m.id === c.thumbMemberRowId);
      if (r?.assetId) coverAssetId = r.assetId;
    }
    if (c.thumbMode === 'separate' && c.thumbSeparateAssetId) {
      coverThumbnailAssetId = c.thumbSeparateAssetId;
    }

    const body = {
      title: c.title || undefined,
      description: c.description || undefined,
      visibility: c.visibility,
      cover_asset_id: coverAssetId,
      cover_thumbnail_asset_id: coverThumbnailAssetId,
      state_id: c.stateId ?? undefined,
      members: memberIds,
      tags: c.tags.length ? c.tags : undefined,
      collection_id: c.collectionId ?? undefined,
      team_id: this.contextTeamId ?? undefined,
    };
    const { data, error } = await api.POST('/posts', { body });
    if (error || !data) {
      throw new Error(extractError(error) ?? t('upload.err_create_post'));
    }
  }
}

// ---- Helpers --------------------------------------------------------------

function hasFiles(e: DragEvent): boolean {
  const types = e.dataTransfer?.types;
  if (!types) return false;
  for (const t of types) {
    if (t === 'Files' || t === 'application/x-moz-file') return true;
  }
  return false;
}

function defaultTitleFromFilename(name: string): string {
  const dot = name.lastIndexOf('.');
  const base = dot > 0 ? name.slice(0, dot) : name;
  // Collapse separators to spaces and trim; the user can edit further.
  return base.replace(/[._-]+/g, ' ').trim() || name;
}

function extensionOf(name: string): string | undefined {
  const dot = name.lastIndexOf('.');
  if (dot <= 0 || dot === name.length - 1) return undefined;
  return name.slice(dot + 1).toLowerCase();
}

/**
 * Turn a refused field-value write into a sentence naming the field
 * and the reason (#843).
 *
 * 422 carries FieldValueUnprocessable — `{error, reason, field,
 * option}`, the ONE body the asset and collection writers share
 * (app/internal/metadata/options.go, rejectionBody). `reason` and
 * `option` are what a client is meant to read; `error` is the server's
 * English and names the field by CODE, so it is the fallback rather
 * than the answer. All four reasons are handled: the enum has
 * `value_type_mismatch` and `field_not_for_collection` alongside the
 * two vocabulary ones, and an unhandled reason would render as a bare
 * key.
 *
 * Anything else gets an honest generic — we know the field and the
 * status and genuinely nothing else.
 */
const FIELD_REFUSAL_KEYS: Record<string, string> = {
  unknown_option: 'upload.field_error.unknown_option',
  option_not_offerable: 'upload.field_error.option_not_offerable',
  value_type_mismatch: 'upload.field_error.value_type_mismatch',
  field_not_for_collection: 'upload.field_error.field_not_for_collection',
};

interface RefusalBody {
  error?: unknown;
  reason?: unknown;
  option?: unknown;
}

async function describeFieldRefusal(res: Response, field: string): Promise<string> {
  let body: RefusalBody | null = null;
  try {
    body = (await res.json()) as RefusalBody;
  } catch {
    // Not JSON — a proxy error page, or an empty body. Falls through
    // to the generic, which is all we can honestly say.
    body = null;
  }
  if (res.status === 422 && body && typeof body.reason === 'string') {
    const key = FIELD_REFUSAL_KEYS[body.reason];
    if (key) {
      const option = typeof body.option === 'string' ? body.option : '';
      return t(key, { field, option });
    }
  }
  if (body && typeof body.error === 'string' && body.error) {
    return t('upload.field_error.reported', { field, detail: body.error });
  }
  return t('upload.field_error.generic', { field, status: res.status });
}

function extractError(err: unknown): string | undefined {
  if (err && typeof err === 'object' && 'error' in err) {
    const v = (err as { error: unknown }).error;
    if (typeof v === 'string') return v;
  }
  return undefined;
}

// Singleton export. Same pattern as auth.svelte.ts.
export const upload = new UploadState();

// ---- Field-definition cache ----------------------------------------------
//
// Shared by every UploadFileRow's metadata editor. The field list
// changes rarely; a per-asset_type in-memory cache for the
// session is fine — no need to thread through the cache.Registry
// equivalent on the frontend.

const fieldsCache = new Map<number, Promise<FieldDef[]>>();

export interface FieldDef {
  id: string;
  code: string;
  label: string;
  description?: string;
  type: PendingFieldValue['type'];
  // Entries under `values` are bare slugs OR option objects carrying
  // a label and a lifecycle status — see $lib/fieldOptions, which
  // normalises both. Typing this as `{ values?: string[] }` is what
  // let the two option consumers drift apart.
  options?: Record<string, unknown>;
  /**
   * The field's vocabulary grows from what is written to it (#830).
   * Honoured for `multi_select` only — see FieldDefinition in
   * openapi.yaml. Drives whether the picker offers to CREATE a term
   * the field does not have.
   */
  open_vocabulary?: boolean;
  required?: boolean;
  display_order: number;
  display_group: string;
  // The upload default the server will apply if this field is left
  // alone (#793). Carried so the row can SAY so — a default the
  // artist cannot see is a decision made on their behalf without
  // telling them, which is a different thing from one they did not
  // have to make.
  //
  // Deliberately not pre-filled into row.fieldValues: a value the
  // artist did not choose must not be sent as set_by='manual', or the
  // extraction pipeline will treat it as a decision and never improve
  // on it.
  default_value?: FieldDefault | null;
}

/**
 * Returns the field definitions visible to the upload form for a
 * given asset_type. Cached for the session.
 */
export function fieldsForAssetType(resourceType: number): Promise<FieldDef[]> {
  const cached = fieldsCache.get(resourceType);
  if (cached) return cached;
  const p = (async () => {
    const { data, error } = await api.GET('/fields', {
      params: { query: { status: 'active', asset_type: resourceType } },
    });
    if (error || !data) {
      throw new Error(extractError(error) ?? t('upload.err_load_fields'));
    }
    return data as FieldDef[];
  })();
  fieldsCache.set(resourceType, p);
  // Drop the cache entry on failure so the next attempt can retry.
  p.catch(() => fieldsCache.delete(resourceType));
  return p;
}
