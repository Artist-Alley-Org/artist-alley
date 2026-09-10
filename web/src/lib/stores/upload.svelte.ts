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
// #1243 — the declaration's type moved to the module that owns its
// DISPLAY rule, so the write side and the read side cannot disagree
// about what the four states are.
import type { AiProvenance } from '$lib/aiProvenance';
// #1408: same-batch companion reconciliation. The MATCHING rule lives
// in its own pure module so it can be reasoned about (and tested)
// without a store; the PATH CAPTURE lives in another because reading a
// relative path out of a drop is a browser-API problem, not a queue
// one.
import {
  reconcileCompanions,
  suggestCompanionPath,
  baseName,
  normalizeRelPath,
} from '$lib/upload/companionMatch';
import { entriesFromDataTransfer, entriesFromFiles } from '$lib/upload/dropEntries';
import type { UploadEntry } from '$lib/upload/dropEntries';
import { is3DExt } from '$components/viewers/controller';

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


/** What a stored model declares it needs, and what of that is missing (#754). */
export interface CompanionRequirements {
  status: 'ok' | 'unsupported' | 'unreadable';
  /** True when only the first level of references is knowable — OBJ. */
  partial: boolean;
  declared: string[];
  missing: string[];
  attached: string[];
  /** Why the file could not be read; set only when status is 'unreadable'. */
  detail?: string;
}

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
  /**
   * Where this file sat in the batch it arrived in, relative to what
   * was dropped or picked: `wood/model.gltf`, not `model.gltf` (#1408).
   *
   * A model's declared references are relative to ITS OWN directory, so
   * without this the batch cannot tell `wood/textures/diffuse.png` from
   * `metal/textures/diffuse.png`. Falls back to the bare filename when
   * the browser supplied no directory information, which
   * `relPathKnown` records honestly rather than papering over.
   */
  readonly relPath: string;
  /** True when the browser really told us the directory (see relPath). */
  readonly relPathKnown: boolean;
  /** The drop/select this row arrived in, when that batch held a model. */
  readonly batchId: string | null;
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
  /**
   * The maker's AI declaration for this asset (#1167, ADR 0094).
   *
   * ⚠️ `null` MEANS UNDECLARED — nobody was asked — and it is NOT
   * `'none'`. `'none'` is a positive claim ("no generative AI was
   * involved") and must only ever be set because a person chose it.
   * The create body therefore OMITS the key when this is null, rather
   * than sending a zero value the way `mature` above sends `false`.
   * The two fields look alike and this is where they differ.
   */
  aiProvenance: AiProvenance;
  /**
   * What `mature` and `aiProvenance` were when the asset row was
   * CREATED — i.e. what the server was actually told.
   *
   * ⚠️ These exist because the upload starts the instant a file is
   * added, so `POST /assets` has usually already gone by the time the
   * artist reaches the self-label controls. Before this, a label set
   * after the row went `ready` was written to a local field nothing
   * ever sent: the box stayed ticked, the submit succeeded, and the
   * asset was stored unlabelled. That is true of `mature` in shipped
   * code as well as of the declaration — same mechanism, same silence.
   *
   * Null until the row is created; compared at submit so only a real
   * difference costs a PATCH.
   */
  sentMature: boolean | null;
  sentAiProvenance: AiProvenance | undefined;
  /**
   * The asset type the SERVER assigned, read back from POST /assets.
   *
   * Not a guess. The frontend has no mime→asset_type mapping and must
   * not grow one: `assetTypeFor` in the Go handler is the rule, it
   * promotes whatever generic type we send to the real one by
   * extension, and a client-side mirror of that table is a second
   * expression of it that is free to disagree. So the create surface
   * asks for the answer instead of computing it, and uses it to decide
   * which field definitions to offer (#1119). Null until the row is
   * ready.
   */
  assetType: number | null;
  /**
   * What the stored model says it still needs (#754). Populated after
   * the asset row exists, for model files only. Null means "not asked
   * yet or not applicable" — the UI distinguishes that from an answer
   * of `unsupported`, which means "we cannot read this format".
   */
  requirements: CompanionRequirements | null;
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
  /**
   * The path this companion was last STORED under, or null if it has
   * not been stored yet. Editing `path` after a successful upload used
   * to leave the server holding the old one. The row said
   * `textures/foo.png`, the asset had `foo.png`, and the requirement
   * stayed missing with nothing on screen saying why (#1408).
   */
  uploadedPath: string | null;
  /** True while this companion was placed by reconciliation, not by hand. */
  auto: boolean;
}

/**
 * A file that arrived in a batch alongside a MODEL and is being held
 * until the model says whether it wants it (#1408).
 *
 * It is deliberately NOT an `UploadRow`: a row uploads the instant it
 * exists, and the whole defect is that a texture uploaded as its own
 * asset. A candidate has not become anything yet. It ends up as exactly
 * one of three things: a companion on a model, an ordinary asset row,
 * or a question for the artist, and never guesses which.
 */
export interface PendingCandidate {
  readonly id: string;
  readonly file: File;
  /** Batch-relative path, normalised; the bare filename when unknown. */
  readonly path: string;
  /** True only when the browser supplied directory information. */
  readonly hasPath: boolean;
  readonly batchId: string;
  /**
   * `held` = waiting on the models in this batch to report what they
   * declare. `undecided` = reconciliation refused to guess and the
   * artist has to answer.
   */
  status: 'held' | 'undecided';
  reason: 'ambiguous' | 'incomplete' | null;
  /** Models this file could belong to, with the path each would use. */
  options: { rowId: string; title: string; path: string }[];
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
  /** Save as a draft instead of publishing (ADR 0091 decision 7).
   *
   *  Replaces `stateId`, which was a raw workflow-state UUID picked
   *  from a dropdown of every state in the `post` domain. That control
   *  asked the artist a question about the state machine ("wip or
   *  published?") to answer a question about their own work, and the
   *  server took the UUID without validating which domain it belonged
   *  to. Publication is now one boolean on both sides. */
  draft: boolean;
  /**
   * The AI declaration for the WHOLE composition (#1167, ADR 0094).
   *
   * ⚠️ It lives here and not only on the rows, and that is a bug fix
   * rather than a convenience. The create page asks this ONCE — the
   * artist is describing the work, not each file of it — and a control
   * that only wrote through to `rows` was INERT before any file had
   * been dropped: an artist who declared first and dropped second had
   * their answer discarded, silently, on the one axis where silence is
   * worst. Rows inherit this at enqueue time, so the order of the two
   * acts stops mattering.
   *
   * Null is UNDECLARED and is the default. Never `'none'`, which is a
   * positive claim the artist has to actually make.
   *
   * The MODAL does not use this: its control is per-file and writes
   * `row.aiProvenance` directly, which `sharedAiProvenance` falls back
   * to reading.
   */
  aiProvenance: AiProvenance;
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

/**
 * What one successful `submit()` actually produced (#1407).
 *
 * The modal is mounted ONCE, globally, in `routes/+layout.svelte`, so
 * the route the artist was standing on when they published is not
 * something this store knows or should know. It therefore does not
 * refresh anything itself; it says what happened and leaves each
 * surface to re-ask the server in whatever way that surface's own
 * paging, ordering and snapshot semantics allow.
 *
 * ⚠️ CAPTURED BEFORE `reset()`. `reset()` drops every row, which is
 * where the asset ids live, and `resetCompose()` is the only thing
 * that could later disagree about the collection. A subscriber that
 * read the store instead of this payload would be reading a store
 * that has already been emptied.
 */
export interface UploadSuccess {
  /** Post ids created, in creation order. Empty for an asset-only upload. */
  postIds: string[];
  /** Asset ids of every row that was published, in queue order. */
  assetIds: string[];
  /** The collection the posts were published into, or null. */
  collectionId: string | null;
  /** The team context the modal was opened with, or null. */
  teamId: string | null;
}

/** A surface's reaction to a successful publish. */
export type UploadSuccessListener = (result: UploadSuccess) => void;

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

  /**
   * Non-model files from a batch that contained a model, held until the
   * models say what they declare (#1408). See `PendingCandidate`.
   */
  candidates = $state<PendingCandidate[]>([]);

  /**
   * Per-batch bookkeeping. A batch reconciles ONCE, when every model in
   * it has finished asking the server what it needs. Reconciling
   * per-model would let the first model claim a colliding basename that
   * the second model was also going to declare, which is the exact
   * cross-wiring this feature must not do.
   */
  private batches = new Map<string, { modelRowIds: string[]; settled: Set<string> }>();

  compose = $state<PostComposeState>({
    enabled: true,
    mode: 'one-post',
    title: '',
    description: '',
    visibility: 'org-only',
    tags: [],
    collectionId: null,
    draft: false,
    aiProvenance: null,
    thumbMode: 'member',
    thumbMemberRowId: null,
    thumbSeparateAssetId: null,
  });

  /** Set by openWithFiles({ collectionId, teamId }) — informational, used at post-create time. */
  contextTeamId = $state<string | null>(null);

  // Final-step state — the POST /posts call(s) after every file is ready.
  /**
   * Post ids created by the last successful submit(), in creation
   * order. Read immediately after submit() resolves true — reset()
   * empties it at the START of the next run, not the end of this one,
   * so the caller always has a window to read it.
   */
  createdPostIds = $state<string[]>([]);
  composeBusy = $state(false);
  composeError = $state<string | null>(null);

  /**
   * Surfaces waiting to hear that a publish landed (#1407).
   *
   * A plain listener set rather than a `$state` counter watched by an
   * `$effect`, and that is not a style preference. Svelte 5 collects
   * dependencies THROUGH CALL FRAMES, so an effect that read a counter
   * and then called a route's own `loadPosts()` would subscribe itself
   * to every piece of state that refetch touches and re-run on its own
   * results. A callback is invoked outside any tracking scope, so a
   * refetch cannot become its own trigger.
   */
  private successListeners = new Set<UploadSuccessListener>();

  /** Rows that have finished uploading and have asset_ids. */
  get readyRows(): UploadRow[] {
    return this.rows.filter((r) => r.state === 'ready' && r.assetId);
  }

  /** Files still waiting on their batch's models to report. */
  get heldCandidates(): PendingCandidate[] {
    return this.candidates.filter((c) => c.status === 'held');
  }

  /** Files reconciliation refused to place. The artist must answer. */
  get undecidedCandidates(): PendingCandidate[] {
    return this.candidates.filter((c) => c.status === 'undecided');
  }

  /**
   * True while a file in this batch is still waiting on the model, or
   * still waiting on the ARTIST (#1408).
   *
   * Both surfaces disable their publish control on it. A button that
   * looks ready and then refuses is the weaker half of surfacing a
   * question: the decision panel sits directly above it saying which
   * file needs an answer, so the button being unavailable reads as a
   * consequence of that rather than as a fault.
   */
  get blockedByCompanions(): boolean {
    return this.candidates.length > 0;
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
  openWithFiles(files: FileList | File[] | UploadEntry[], ctx: OpenContext = {}): void {
    this.applyContext(ctx);
    this.open = true;
    this.enqueue(entriesFromFiles(files));
  }

  /** Add more files to an already-open modal (the drop-zone inside the modal). */
  addFiles(files: FileList | File[] | UploadEntry[]): void {
    this.enqueue(entriesFromFiles(files));
  }

  /**
   * Add a DROP, preserving relative directories where the browser
   * exposes them (#1408).
   *
   * ⚠️ Hand it the live `DataTransfer` straight out of the drop
   * handler and do not await anything first. `webkitGetAsEntry` is
   * only valid inside the event turn (see dropEntries.ts).
   */
  async addDrop(dt: DataTransfer | null, ctx: OpenContext = {}): Promise<void> {
    const entries = await entriesFromDataTransfer(dt);
    if (entries.length === 0) return;
    this.applyContext(ctx);
    this.enqueue(entries);
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

  /**
   * Hear about successful publishes for as long as the caller is
   * mounted (#1407). Returns the unsubscribe function, which is what
   * `onMount` wants returned:
   *
   * ```svelte
   * onMount(() => upload.onSuccess(() => void loadPosts()));
   * ```
   *
   * ⛔ A subscriber must NOT manufacture rows out of the payload. The
   * ids are there to say WHAT landed, not to be rendered: the server
   * decides things the client guessed at (asset type is promoted
   * server side), so the surface re-asks and shows what comes back.
   *
   * # Who listens, and who deliberately does not
   *
   * Every production surface that lists posts or assets:
   * `routes/+page.svelte` (the feed), `routes/collections/[id]`,
   * `routes/teams/[id]` (both tabs) and `components/UserProfile`,
   * which is what `/users/by-ref/[ref]` and
   * `/users/by-username/[username]` mount.
   *
   * `routes/search` does NOT, and that is a decision rather than an
   * omission. Its only refresh primitive is `runSearch`, which carries
   * ADR 0056 §3c's mandated scroll reset: refining is a new address, so
   * a non-append run puts the reader back at the first hit. Firing that
   * from a background event would yank a reader down a result list to
   * the top of it, which is the destructive half of this bug rather
   * than a fix for it. A search result is an answer to a question the
   * reader asked, not the place their new work lives, and the surfaces
   * where it does live are all listed above.
   *
   * `routes/create` is not a consumer either: it is a full-page flow
   * that navigates to what it made (#1119). It calls `submit()` like
   * the modal does, so this fires there too, and finds nobody home,
   * which is correct.
   */
  onSuccess(fn: UploadSuccessListener): () => void {
    this.successListeners.add(fn);
    return () => {
      this.successListeners.delete(fn);
    };
  }

  /**
   * Announce a landed publish.
   *
   * Iterates a COPY, so a subscriber that unsubscribes itself while
   * being called cannot skip the next one. Each call is isolated: a
   * surface whose refetch throws must not turn a publish that
   * genuinely succeeded into `composeError`, which would tell the
   * artist their work was not saved when it was.
   */
  private emitSuccess(result: UploadSuccess): void {
    for (const fn of [...this.successListeners]) {
      try {
        fn(result);
      } catch {
        // Deliberately swallowed. See above.
      }
    }
  }

  /** Hard reset — drops every row + posts state. Called after a successful submit. */
  reset(): void {
    for (const r of this.rows) {
      URL.revokeObjectURL(r.objectUrl);
    }
    this.rows = [];
    this.candidates = [];
    this.batches.clear();
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
    // Removing a model must not strand the files held for it (#1408).
    this.settleBatchMember(row);
    for (const c of this.candidates) {
      if (c.status !== 'undecided') continue;
      c.options = c.options.filter((o) => o.rowId !== id);
    }
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

  /**
   * Attach companion files to a row BY HAND.
   *
   * Two things used to go wrong here and both were silent.
   *
   * `path` defaulted to `file.name`. The server satisfies a requirement
   * by EXACT string match of the stored path against the declared path,
   * so a file attached as `img.jpg` never satisfied a declared
   * `textures/img.jpg`. The artist attached the right file, watched
   * the warning not move, and had no way to know a path they never saw
   * was the reason. `suggestCompanionPath` prefers a path the model
   * actually declared, taken from where the file sits in a picked
   * directory or from being the only file with that name.
   *
   * And nothing uploaded. See `attachCompanions`.
   */
  addCompanions(rowId: string, files: FileList | File[] | UploadEntry[]): void {
    const row = this.rows.find((r) => r.id === rowId);
    if (!row) return;
    const declared = row.requirements?.declared ?? [];
    const items = entriesFromFiles(files).map((e) => ({
      file: e.file,
      path: suggestCompanionPath(declared, row.relPath, e),
    }));
    for (const it of items) {
      row.companions.push({
        id: newId(),
        file: it.file,
        path: it.path,
        state: 'pending',
        error: null,
        uploadedPath: null,
        auto: false,
      });
    }
    void this.flushCompanions(row);
  }

  /** Drop of companion files onto a row, preserving relative directories. */
  async addCompanionDrop(rowId: string, dt: DataTransfer | null): Promise<void> {
    const entries = await entriesFromDataTransfer(dt);
    if (entries.length > 0) this.addCompanions(rowId, entries);
  }

  removeCompanion(rowId: string, companionId: string): void {
    const row = this.rows.find((r) => r.id === rowId);
    if (!row) return;
    const c = row.companions.find((x) => x.id === companionId);
    row.companions = row.companions.filter((x) => x.id !== companionId);
    // A companion already on the server has to come OFF it. Dropping
    // only the local row would leave the asset carrying a file the
    // artist just removed, and `attached` still counting it.
    if (c?.uploadedPath && row.assetId) {
      void this.detachCompanionPath(row, c.uploadedPath);
    }
  }

  /**
   * Rename the relative path of a pending or already-stored companion.
   *
   * Editing a path AFTER the bytes went up used to change the label and
   * nothing else: the server still held the old path, the declared path
   * still went unmatched, and the row read as if the artist had fixed
   * it. So a stored companion whose path changes is re-sent under the
   * new one and detached from the old.
   */
  setCompanionPath(rowId: string, companionId: string, path: string): void {
    const row = this.rows.find((r) => r.id === rowId);
    if (!row) return;
    const c = row.companions.find((x) => x.id === companionId);
    if (!c) return;
    const next = normalizeRelPath(path);
    if (next === c.path) return;
    c.path = next;
    if (c.state === 'done' && c.uploadedPath && c.uploadedPath !== next) {
      c.state = 'pending';
    }
  }

  /** Re-send anything this row still owes, then re-ask what it needs. */
  commitCompanionPaths(rowId: string): void {
    const row = this.rows.find((r) => r.id === rowId);
    if (row) void this.flushCompanions(row);
  }

  /** Remove the companion stored at `path` from this row's asset. */
  private async detachCompanionPath(row: UploadRow, path: string): Promise<void> {
    const aid = row.assetId;
    if (!aid) return;
    try {
      const res = await fetch(`/api/v1/assets/${aid}/companions`, { credentials: 'include' });
      if (!res.ok) return;
      const list = (await res.json()) as { id: string; path: string }[];
      const hit = list.find((x) => x.path === path);
      if (!hit) return;
      await fetch(`/api/v1/assets/${aid}/companions/${hit.id}`, {
        method: 'DELETE',
        credentials: 'include',
      });
      await this.loadRequirements(row);
    } catch {
      // Best effort. A companion left behind at an unreferenced path is
      // inert (it satisfies no declaration), so failing loudly here
      // would report a problem the artist cannot act on.
    }
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
      const dt = e.dataTransfer;
      if (!dt) return;
      // #1408: `dt.files` is a flat FileList whose members all carry an
      // EMPTY webkitRelativePath, so a directory drop arrived as
      // basenames and `wood/diffuse.png` was indistinguishable from
      // `metal/diffuse.png`. addDrop reads the entry API instead, and
      // must be handed the LIVE DataTransfer inside this turn.
      this.open = true;
      void this.addDrop(dt);
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
    // #1408: files reconciliation REFUSED to place are still files the
    // artist chose. Publishing over the top of them would either lose
    // them silently or guess where they go, and the whole point of
    // surfacing the question is that neither is acceptable.
    if (this.undecidedCandidates.length > 0) {
      this.composeError = t('upload.err_undecided_companions', {
        n: this.undecidedCandidates.length,
      });
      return false;
    }
    if (this.heldCandidates.length > 0) {
      this.composeError = t('upload.err_companions_pending');
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
      // Self-labels the artist set after the row was created. Before
      // the posts, so a post is never made around an asset carrying a
      // label its owner thinks they applied and the server never saw.
      for (const row of ready) {
        await this.flushSelfLabels(row);
      }
      this.createdPostIds = [];
      if (this.compose.enabled) {
        await this.createPosts(ready);
      }
      // #1407: read the outcome BEFORE the teardown that destroys it.
      // `reset()` empties `rows`, so `ready[].assetId` is only
      // available here, and `resetCompose()` runs from inside it.
      const result: UploadSuccess = {
        postIds: [...this.createdPostIds],
        assetIds: ready.map((r) => r.assetId).filter((v): v is string => !!v),
        collectionId: this.compose.collectionId,
        teamId: this.contextTeamId,
      };
      this.reset();
      // AFTER `reset()`, and the ordering is load-bearing against
      // #1408. `reset()` tears down the companion lifecycle:
      // `candidates`, `batches`, the undecided questions. A
      // subscriber that ran before it would be looking at a modal that
      // is still open, still holding rows, and still mid-reconciliation.
      // Emitting afterwards means a surface only ever sees a store that
      // has finished.
      this.emitSuccess(result);
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
    // Uploading INTO a collection means making a post there (#1161,
    // ADR 0091 decision 3: publication is never a side effect, and a
    // collection holds posts).
    //
    // The "just upload as assets" escape hatch cannot apply on this
    // path, and turning it off is not a UI preference here — it is the
    // difference between the files landing in the collection and
    // landing nowhere the user was looking. Before the resources
    // endpoints were retired the escape hatch quietly wrote a
    // `collection_resources` row that no surface displayed; now it
    // would write nothing at all. Either way the artist dropped files
    // on a collection page and got an empty collection.
    if (this.compose.collectionId) this.compose.enabled = true;
  }

  /**
   * The AI declaration every queued row shares, or null when they
   * disagree or nothing was declared (#1167, ADR 0094).
   *
   * Read by ThumbnailPicker so a STANDALONE COVER carries the same
   * statement as the work it fronts. That is not tidiness: a cover is a
   * CONTRIBUTOR to the post's derived value (migration 00060 follows
   * 00054's completed rule, because a card shows its cover first), and
   * the derivation's negative arm requires unanimity. Leaving the cover
   * undeclared would take a post whose every member declared `none`
   * back to UNDECLARED — the artist would have answered the question
   * and had the answer quietly dropped by an unrelated control.
   */
  get sharedAiProvenance(): AiProvenance {
    // The composition-level answer wins where there is one: the create
    // page sets it, and it is meaningful with ZERO rows queued.
    if (this.compose.aiProvenance !== null) return this.compose.aiProvenance;
    // Otherwise fall back to the rows agreeing, which is how the MODAL
    // reaches an answer — its control is per-file and never touches
    // `compose`.
    if (this.rows.length === 0) return null;
    const first = this.rows[0].aiProvenance;
    return this.rows.every((r) => r.aiProvenance === first) ? first : null;
  }

  /**
   * Declare AI involvement for the whole composition, writing through to
   * every row already queued (#1167).
   *
   * Both halves are necessary, and the bug that produced this method
   * needed both. Storing it makes the choice survive until files
   * arrive; writing through applies it to files that arrived first.
   * With only the write-through the control was INERT on an empty page
   * — an artist who declared before dropping had their answer
   * discarded, silently, on the one axis where silence is worst.
   */
  setAiProvenance(v: AiProvenance): void {
    this.compose.aiProvenance = v;
    for (const row of this.rows) row.aiProvenance = v;
  }

  /** True when the modal was opened from a collection, in which case
   *  the post is not optional — see applyContext. Read by the compose
   *  form to explain the disabled toggle rather than just disabling it. */
  get postRequired(): boolean {
    return !!this.compose.collectionId;
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
      draft: false,
      aiProvenance: null,
      thumbMode: 'member',
      thumbMemberRowId: null,
      thumbSeparateAssetId: null,
    };
  }

  /**
   * Take one drop / one selection and decide what each file IS (#1408).
   *
   * ## The defect this replaces
   *
   * Every `File` became its own `UploadRow` and every row uploads the
   * moment it exists. A model plus three textures was therefore four
   * unrelated assets, the model's `companions` stayed empty, and #754's
   * missing-file warning had no way to ever go away without the artist
   * re-attaching each file by hand at a path they had to guess.
   *
   * ## What changed
   *
   * A batch with NO model file behaves exactly as before, every file
   * is a row, immediately. That is the ordinary upload and it must not
   * acquire a delay or a decision.
   *
   * A batch WITH a model file splits: the models become rows and start
   * uploading at once, and the rest are HELD as candidates. Holding is
   * the point: a texture that has already become an asset cannot be
   * un-become one, so the decision has to happen before the bytes are
   * committed to an asset row. Nothing is held for long: the models are
   * uploading in parallel and the batch reconciles as soon as they have
   * all answered `GET /assets/{id}/companion-requirements`.
   */
  private enqueue(entries: UploadEntry[]): void {
    if (entries.length === 0) return;

    const models = entries.filter((e) => isModelEntry(e));
    if (models.length === 0) {
      this.addRows(entries, null);
      return;
    }

    const rest = entries.filter((e) => !isModelEntry(e));
    const batchId = newId();

    // ⚠️ REGISTERED BEFORE THE ROWS EXIST. `addRows` starts the runner,
    // and a model whose upload fails SYNCHRONOUSLY settles inside that
    // call, against a batch that would not be there yet, stranding
    // every file held beside it with nothing to release them.
    // `modelRowIds` is filled in below, and `settleBatchMember` refuses
    // to conclude anything while it is still empty.
    const batch = { modelRowIds: [] as string[], settled: new Set<string>() };
    if (rest.length > 0) this.batches.set(batchId, batch);

    const rows = this.addRows(models, batchId);
    batch.modelRowIds = rows.map((r) => r.id);
    if (rest.length === 0) return;

    this.candidates = [
      ...this.candidates,
      ...rest.map((e) => ({
        id: newId(),
        file: e.file,
        path: e.path,
        hasPath: e.hasPath,
        batchId,
        status: 'held' as const,
        reason: null,
        options: [],
      })),
    ];

    // The membership is only knowable now, so a settlement that
    // happened during addRows has to be re-examined against it.
    if (batch.modelRowIds.every((id) => batch.settled.has(id))) {
      void this.reconcileBatch(batchId);
    }
  }

  /** Turn entries into upload rows and start the runner. */
  private addRows(entries: UploadEntry[], batchId: string | null): UploadRow[] {
    const additions: UploadRow[] = [];
    for (const entry of entries) {
      const file = entry.file;
      const id = newId();
      additions.push({
        id,
        file,
        relPath: entry.path,
        relPathKnown: entry.hasPath,
        batchId,
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
        // Inherited from the composition, which is UNDECLARED unless the
        // artist has said otherwise. A default of 'none' would have the
        // form disclaim AI on their behalf before they touched anything,
        // which is the one thing ADR 0094 exists to stop.
        aiProvenance: this.compose.aiProvenance,
        sentMature: null,
        sentAiProvenance: undefined,
        assetType: null,
        requirements: null,
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
    return additions;
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
        // #1167 — OMITTED when undeclared, never sent as a zero value.
        // `mature: false` above is a true statement about an unlabelled
        // work; `ai_provenance: 'none'` would be a disclaimer nobody
        // made, so absence is the only honest wire form for it.
        ...(row.aiProvenance ? { ai_provenance: row.aiProvenance } : {}),
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
      // Remember what the create request carried, so submit can tell a
      // label the artist set AFTERWARDS from one that already landed.
      row.sentMature = body.mature ?? false;
      row.sentAiProvenance = row.aiProvenance;
      // What the server actually decided this file is (#1119). The
      // request asked for DEFAULT_ASSET_TYPE and the handler promotes
      // it by extension, so this is the first point at which the real
      // type is known — and it is the type whose field definitions the
      // create page must offer.
      row.assetType = typeof data.asset_type === 'number' ? data.asset_type : null;
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
      // #754 — ask the model what it still needs. AFTER the row is
      // ready and deliberately not awaited into the row's state: this
      // is advisory, and an upload must not be reported as failed
      // because the advice could not be fetched.
      //
      // #1408, and it is what the batch waits on. A model that never
      // answers must still SETTLE, or the files held beside it are held
      // for good; `finally` is load-bearing here, not tidiness.
      void this.loadRequirements(row).finally(() => this.settleBatchMember(row));
    } catch (e) {
      row.error = e instanceof Error ? e.message : t('upload.err_upload_failed');
      row.state = 'errored';
      this.settleBatchMember(row);
    } finally {
      this.kick();
    }
  }

  // ---- Same-batch companion reconciliation (#1408) ----------------------

  /**
   * One model in a batch has finished asking what it needs. When they
   * ALL have, reconcile.
   *
   * Waiting for the whole batch is not an optimisation. Reconciling
   * model-by-model would let the first model to answer claim a file by
   * basename that the second model was about to declare too, and the
   * collision that makes the match ambiguous would never be visible,
   * because by then it would already be resolved wrongly.
   */
  private settleBatchMember(row: UploadRow): void {
    const batchId = row.batchId;
    if (!batchId) return;
    const batch = this.batches.get(batchId);
    if (!batch) return;
    batch.settled.add(row.id);
    // Membership not wired yet (see enqueue). The caller re-checks.
    if (batch.modelRowIds.length === 0) return;
    if (batch.modelRowIds.some((id) => !batch.settled.has(id))) return;
    void this.reconcileBatch(batchId);
  }

  /**
   * Place every held file in a batch against what the batch's models
   * DECLARE.
   *
   * The declaration is the server's, fetched per model. Nothing here
   * inspects a file's type, and every path sent to the server is a
   * declared path copied verbatim. The server satisfies a requirement
   * by exact string match, so a path this invented could only ever miss.
   */
  private async reconcileBatch(batchId: string): Promise<void> {
    const batch = this.batches.get(batchId);
    if (!batch) return;
    this.batches.delete(batchId);

    const held = this.candidates.filter((c) => c.batchId === batchId && c.status === 'held');
    if (held.length === 0) return;

    const modelRows = batch.modelRowIds
      .map((id) => this.rows.find((r) => r.id === id))
      .filter((r): r is UploadRow => !!r && !!r.assetId);

    if (modelRows.length === 0) {
      // Every model in the batch failed to upload. The files beside it
      // are ordinary files again. Releasing them is the only answer
      // that loses nothing.
      this.releaseCandidates(held);
      return;
    }

    const result = reconcileCompanions(
      modelRows.map((r) => ({
        rowId: r.id,
        modelPath: r.relPath,
        declared: r.requirements?.declared ?? [],
        // An .obj is `partial` and an unreadable or unparsed model told
        // us nothing. In both, "this leftover file is unrelated" is a
        // claim with no basis, so the batch must not make it.
        complete: r.requirements?.status === 'ok' && !r.requirements.partial,
      })),
      held.map((c) => ({ id: c.id, path: c.path, hasPath: c.hasPath })),
    );

    const byId = new Map(held.map((c) => [c.id, c]));
    const placed = new Set<string>();

    // ⛔ Every candidate this pass touches LEAVES the held list. It is
    // not bookkeeping: `heldCandidates` is what the "checking what the
    // model needs" banner reads and what blocks submit, so a candidate
    // left behind after it was successfully attached leaves the artist
    // looking at a permanent progress note over a Publish button that
    // refuses. Caught by driving a real 17-file folder through the
    // modal. Every companion said DONE and the banner never went.
    const clearHeld = (ids: Iterable<string>) => {
      const gone = new Set(ids);
      this.candidates = this.candidates.filter((c) => !gone.has(c.id));
    };

    // Group by row so each model is written once, in one pass.
    const perRow = new Map<string, { file: File; path: string }[]>();
    for (const a of result.assignments) {
      const c = byId.get(a.candidateId);
      if (!c) continue;
      placed.add(a.candidateId);
      const list = perRow.get(a.rowId) ?? [];
      list.push({ file: c.file, path: a.path });
      perRow.set(a.rowId, list);
    }

    for (const u of result.undecided) {
      const c = byId.get(u.candidateId);
      if (!c) continue;
      placed.add(u.candidateId);
      c.reason = u.reason;
      c.options = u.options.map((o) => ({
        rowId: o.rowId,
        title: this.rows.find((r) => r.id === o.rowId)?.title ?? '',
        path: o.path,
      }));
      c.status = 'undecided';
    }

    const release = result.unrelated
      .map((id) => byId.get(id))
      .filter((c): c is PendingCandidate => !!c);
    for (const c of release) placed.add(c.id);
    if (release.length > 0) this.releaseCandidates(release);

    // Anything the matcher did not speak about is released rather than
    // silently dropped: a file the artist chose must always end up
    // SOMEWHERE.
    const orphans = held.filter((c) => !placed.has(c.id));
    if (orphans.length > 0) this.releaseCandidates(orphans);

    clearHeld(result.assignments.map((a) => a.candidateId));

    for (const [rowId, items] of perRow) {
      await this.attachCompanions(rowId, items);
    }
  }

  /** Turn candidates back into ordinary upload rows. */
  private releaseCandidates(list: PendingCandidate[]): void {
    if (list.length === 0) return;
    const ids = new Set(list.map((c) => c.id));
    this.candidates = this.candidates.filter((c) => !ids.has(c.id));
    this.addRows(
      list.map((c) => ({ file: c.file, path: c.path, hasPath: c.hasPath })),
      null,
    );
  }

  /** Artist's answer to an ambiguous file: this model, at this path. */
  async assignCandidate(candidateId: string, rowId: string, path: string): Promise<void> {
    const c = this.candidates.find((x) => x.id === candidateId);
    if (!c) return;
    const clean = normalizeRelPath(path) || baseName(c.path);
    this.candidates = this.candidates.filter((x) => x.id !== candidateId);
    await this.attachCompanions(rowId, [{ file: c.file, path: clean }]);
  }

  /** Artist's answer: this file is its own upload after all. */
  releaseCandidate(candidateId: string): void {
    const c = this.candidates.find((x) => x.id === candidateId);
    if (c) this.releaseCandidates([c]);
  }

  /** Artist's answer for the whole list at once. */
  releaseAllCandidates(): void {
    this.releaseCandidates(this.undecidedCandidates);
  }

  discardCandidate(candidateId: string): void {
    this.candidates = this.candidates.filter((x) => x.id !== candidateId);
  }

  /**
   * Attach companions to a row and, when the row already exists on the
   * server, ACTUALLY UPLOAD THEM.
   *
   * ⛔ The bug this removes is silent. `addCompanions` only ever pushed
   * onto `row.companions`, and `uploadCompanions` was called from one
   * place: `runRow`, before the row reached `ready`. So a companion
   * added afterwards (which is when the artist adds one, because the
   * #754 warning naming the missing file only appears once the row is
   * ready) sat in the list looking attached, was never sent, and the
   * warning it was meant to clear stayed exactly as it was.
   */
  async attachCompanions(rowId: string, items: { file: File; path: string }[]): Promise<void> {
    const row = this.rows.find((r) => r.id === rowId);
    if (!row || items.length === 0) return;
    for (const it of items) {
      row.companions.push({
        id: newId(),
        file: it.file,
        path: it.path,
        state: 'pending',
        error: null,
        uploadedPath: null,
        auto: true,
      });
    }
    await this.flushCompanions(row);
  }

  /**
   * Send whatever this row's companion list still owes the server, then
   * ask the model again what it needs.
   *
   * The re-ask is the other half of the fix: `missing` is a live
   * subtraction the server recomputes per request, so the note only
   * goes away if somebody asks again, and it must go away without a
   * page reload, because a reload is not something the artist should
   * have to discover.
   */
  async flushCompanions(row: UploadRow): Promise<void> {
    if (row.state !== 'ready' || !row.assetId) return;
    const owed = row.companions.some((c) => c.state !== 'done' || c.uploadedPath !== c.path);
    if (!owed) return;
    await this.uploadCompanions(row);
    await this.loadRequirements(row);
  }

  /**
   * Send self-labels the artist set AFTER the asset row was created.
   *
   * ⛔ THE BUG THIS FIXES IS SILENT AND PRE-EXISTING. The upload starts
   * the moment a file is added, so `POST /assets` has normally already
   * gone by the time anybody reaches the mature checkbox or the AI
   * control. Their values were only ever read in the create body, so a
   * label applied a second later went nowhere: the control stayed set,
   * the submit reported success, and the stored asset carried nothing.
   * `mature` has behaved this way since #1115.
   *
   * A refusal ABORTS the submit rather than being swallowed. On these
   * two axes in particular the artist has to learn their label did not
   * take — an accepted-but-inert write is how a library fills with
   * flags nothing enforces, and on the AI axis the artist's reasonable
   * conclusion is that they failed to disclose.
   */
  private async flushSelfLabels(row: UploadRow): Promise<void> {
    if (!row.assetId) return;
    const body: Record<string, unknown> = {};
    if (row.sentMature !== null && row.mature !== row.sentMature) {
      body.mature = row.mature;
    }
    if (row.sentAiProvenance !== undefined && row.aiProvenance !== row.sentAiProvenance) {
      // Two words for two different changes: the column is nullable and
      // `null` on this body already means "leave alone".
      if (row.aiProvenance === null) body.clear_ai_provenance = true;
      else body.ai_provenance = row.aiProvenance;
    }
    if (Object.keys(body).length === 0) return;

    const { error } = await api.PATCH('/assets/{id}', {
      params: { path: { id: row.assetId } },
      body: body as never,
    });
    if (error) {
      throw new Error(extractError(error) ?? t('upload.err_create_asset'));
    }
    row.sentMature = row.mature;
    row.sentAiProvenance = row.aiProvenance;
  }

  /**
   * Ask the server what this model still needs (#754).
   *
   * Advisory and best-effort by construction: an upload is complete
   * whether or not this answers, and every failure path leaves
   * `row.requirements` null, which the UI reads as "nothing to say"
   * rather than as "nothing needed". The distinction matters — a
   * silent "nothing needed" for a model that actually references three
   * textures is precisely the failure this endpoint exists to remove.
   *
   * Companions the user attached BY HAND in the same flow are already
   * on the asset by the time this runs (uploadCompanions is awaited
   * above), so they show up as `attached` and not as `missing`.
   */
  async loadRequirements(row: UploadRow): Promise<void> {
    if (!row.assetId) return;
    try {
      const { data, error } = await api.GET('/assets/{id}/companion-requirements', {
        params: { path: { id: row.assetId } },
      });
      if (error || !data) return;
      row.requirements = {
        status: data.status,
        partial: !!data.partial,
        declared: data.declared ?? [],
        missing: data.missing ?? [],
        attached: data.attached ?? [],
        detail: data.detail,
      };
    } catch {
      // Advisory. Leave it null.
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
      // `uploadedPath` and not just `state`: a companion whose path the
      // artist edited after it landed is `done` at the WRONG path, and
      // skipping it there is how the edit became cosmetic (#1408).
      if (c.state === 'done' && c.uploadedPath === c.path) continue;
      const stale = c.uploadedPath && c.uploadedPath !== c.path ? c.uploadedPath : null;
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
        c.uploadedPath = c.path;
        if (stale) await this.detachCompanionPath(row, stale);
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
      draft: c.draft,
      members: memberIds,
      tags: c.tags.length ? c.tags : undefined,
      collection_id: c.collectionId ?? undefined,
      team_id: this.contextTeamId ?? undefined,
    };
    const { data, error } = await api.POST('/posts', { body });
    if (error || !data) {
      throw new Error(extractError(error) ?? t('upload.err_create_post'));
    }
    // Kept so a caller can go TO what it just made (#1119). The modal
    // never needed it — it closes onto whatever page you were on — but
    // a full-page create flow that drops you back on an empty form has
    // thrown away the only thing you wanted.
    this.createdPostIds.push(data.id);
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

function newId(): string {
  return globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36).slice(2);
}

/**
 * Is this batch member a 3D MODEL?
 *
 * By extension only, through the SAME table the viewer uses to decide
 * what body to mount. It answers "does this file have companions at
 * all", never "is this file a texture". The second question is the one
 * #1408 must not ask, because answering it by type is how an unrelated
 * illustration gets attached to a model that never named it.
 */
function isModelEntry(entry: UploadEntry): boolean {
  return is3DExt(extensionOf(baseName(entry.path)) ?? '');
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
  /**
   * Whether this field is OFFERED on the upload / create surface
   * (#1173, ADR 0092 §3, consumed by #1119).
   *
   * ⚠️ READ IT AS `!== false`, NEVER AS `=== true`. It defaults TRUE on
   * the server precisely so that shipping the flag mid-release changes
   * nothing, and a field that predates the column — or one served by an
   * older peer — arrives with the key ABSENT. Reading absent as "off"
   * would empty the create form on every install at once.
   *
   * It is a form-composition hint and not access control: a hidden
   * field's values are unaffected and can still be written directly.
   */
  show_on_upload?: boolean;
  /**
   * The column this field MIRRORS, when it is a mirror of one (`title`,
   * `description`).
   *
   * A surface that already renders the column with a first-class
   * control must skip the mirrored field rather than offer a second
   * editor for the same value — two inputs writing one column is a
   * last-write-wins race the artist can see and cannot explain.
   */
  mirrors_column?: string | null;
  /**
   * Which tab of a composition surface this field sits in (#1173,
   * ADR 0092 §3, consumed by #1119). `null`/absent = unassigned, which
   * lands in the DEFAULT bucket.
   *
   * On /create the buckets nest INSIDE the asset type, and two asset
   * types that both name a tab "Print" get two separate tabs: those are
   * two operator decisions about two different kinds of thing, and
   * merging them would file a field under a heading its own asset type
   * never mentioned.
   */
  edit_tab?: string | null;
  /**
   * When this field should be offered at all (#1119, ADR 0099).
   * `null`/absent = always.
   *
   * ⚠️ On /create there is no subject, so conditions evaluate against the
   * LOCAL PENDING VALUES in the form. There is deliberately no server
   * fetch of existing protected values for create-time evaluation: there
   * is nothing to fetch them from, and inventing one would build the
   * oracle ADR 0099 §5 exists to prevent.
   */
  display_condition?: string[] | null;
}

/**
 * The fields a create surface should offer for an asset type.
 *
 * Two subtractions on top of `fieldsForAssetType`, and both are #1119's
 * job rather than the server's — `show_on_upload` is stored and served
 * but deliberately not enforced, because it governs a FORM and not a
 * permission (see FieldDefinition in openapi.yaml).
 */
export function fieldsOfferedOnUpload(defs: FieldDef[]): FieldDef[] {
  return defs.filter(
    (f) =>
      // Participation is OPT-OUT: unset means unchanged, means offered.
      f.show_on_upload !== false &&
      // A mirrored column already has a first-class control on the page.
      !f.mirrors_column,
  );
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
