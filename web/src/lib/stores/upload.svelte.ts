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
import type { components } from '$api/schema';

type AssetCreate = components['schemas']['AssetCreate'];

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
  /** Last error message, for the retry surface. */
  error: string | null;
}

export type PostMode = 'one-post' | 'one-per-file' | 'no-post';

export interface PostComposeState {
  enabled: boolean;          // false = "Just upload as assets — no post"
  mode: PostMode;            // when enabled
  title: string;
  description: string;
  visibility: 'private' | 'followers' | 'public';
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

// Default resource_type. RS Photo = 1; we don't have a smarter
// MIME-to-resource_type mapping yet, so everything goes in as Photo
// for the MVP. The processing pipeline will set the right one once
// it lands.
const DEFAULT_RESOURCE_TYPE = 1;

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
    visibility: 'public',
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
      this.composeError = 'Wait for all uploads to finish.';
      return false;
    }
    const ready = this.readyRows;
    if (ready.length === 0) {
      this.composeError = 'No files uploaded.';
      return false;
    }

    this.composeError = null;
    this.composeBusy = true;
    try {
      if (this.compose.enabled) {
        await this.createPosts(ready);
      }
      this.reset();
      return true;
    } catch (e) {
      this.composeError = e instanceof Error ? e.message : 'Failed to create post.';
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
      visibility: 'public',
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
        error: null,
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
        resource_type: DEFAULT_RESOURCE_TYPE,
        file_hash: row.hash,
        file_extension: extensionOf(row.file.name),
        tags: row.tags,
      };
      const { data, error } = await api.POST('/assets', { body });
      if (error || !data) {
        throw new Error(extractError(error) ?? 'Failed to create asset.');
      }
      row.assetId = data.id;
      row.state = 'ready';
    } catch (e) {
      row.error = e instanceof Error ? e.message : 'Upload failed.';
      row.state = 'errored';
    } finally {
      this.kick();
    }
  }

  /**
   * Single-file upload via XHR. POST /storage/objects accepts a raw
   * octet-stream + X-Content-Type. XHR is required because `fetch`
   * doesn't expose upload-progress events.
   */
  private uploadBytes(row: UploadRow): Promise<{ hash: string; deduped?: boolean }> {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open('POST', '/api/v1/storage/objects', true);
      xhr.setRequestHeader('Content-Type', 'application/octet-stream');
      const ct = row.file.type || 'application/octet-stream';
      xhr.setRequestHeader('X-Content-Type', ct);
      xhr.responseType = 'json';
      xhr.withCredentials = true;

      xhr.upload.addEventListener('progress', (e) => {
        if (e.lengthComputable) row.progress = e.loaded / e.total;
      });
      xhr.addEventListener('load', () => {
        if (xhr.status >= 200 && xhr.status < 300 && xhr.response) {
          row.progress = 1;
          resolve(xhr.response as { hash: string; deduped?: boolean });
        } else {
          const err =
            (xhr.response && (xhr.response as { error?: string }).error) ||
            `HTTP ${xhr.status}`;
          reject(new Error(err));
        }
      });
      xhr.addEventListener('error', () => reject(new Error('Network error.')));
      xhr.addEventListener('abort', () => reject(new Error('Upload aborted.')));

      xhr.send(row.file);
    });
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
      throw new Error(extractError(error) ?? 'Failed to create post.');
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

function extractError(err: unknown): string | undefined {
  if (err && typeof err === 'object' && 'error' in err) {
    const v = (err as { error: unknown }).error;
    if (typeof v === 'string') return v;
  }
  return undefined;
}

// Singleton export. Same pattern as auth.svelte.ts.
export const upload = new UploadState();
