// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Getting the RELATIVE PATH out of a drop or a file picker (#1408).
//
// ## Why this exists
//
// `DataTransfer.files` is a flat `FileList` and every `File` in it has
// an EMPTY `webkitRelativePath`. Drop a folder called `wood/` holding
// `model.gltf` and `textures/diffuse.png` and the drop handler is
// handed two basenames with nothing saying which directory either came
// from. A model declaring `textures/diffuse.png` therefore cannot be
// matched against the file that satisfies it, and two models declaring
// `wood/diffuse.png` and `metal/diffuse.png` cannot be told apart at
// all.
//
// The directory information IS available, through a different door:
// `DataTransferItem.webkitGetAsEntry()` returns a `FileSystemEntry`
// tree that can be walked. It has two hard constraints and both shape
// this module:
//
//   1. The items are NEUTERED when the event turn ends. So the entries
//      are taken SYNCHRONOUSLY, before the first `await`, and the walk
//      happens afterwards on the captured handles.
//   2. `FileSystemDirectoryReader.readEntries` returns a PAGE, not the
//      directory. It has to be called until it answers empty, or a
//      folder of more than ~100 files silently arrives truncated,
//      which would present as "some of my textures attached and some
//      became separate assets".
//
// The click-to-select half is simpler: a plain `<input type="file">`
// gives basenames, and an `<input webkitdirectory>` fills in
// `webkitRelativePath`. Both are read here so callers get one shape.
//
// When none of that is available (a browser without the entry API, a
// drop that yields no entries) this falls back to the flat file list,
// with `hasPath: false`, which is a HONEST answer the matcher then
// handles deliberately, rather than a basename quietly presented as a
// path.

import { normalizeRelPath } from './companionMatch';

export interface UploadEntry {
  file: File;
  /** Batch-relative path, normalised. The bare filename when unknown. */
  path: string;
  /** True only when the browser really supplied directory information. */
  hasPath: boolean;
}

/**
 * A drop of a deep tree should not be able to hang the tab. The cap is
 * generous next to any real model directory and small next to a home
 * folder dropped by accident; hitting it stops the walk rather than
 * failing the drop, so the files already read still upload.
 */
const MAX_ENTRIES = 5000;
const MAX_DEPTH = 24;

/** Files from an `<input type="file">`, honouring `webkitdirectory`. */
export function entriesFromFiles(files: FileList | File[] | UploadEntry[]): UploadEntry[] {
  const arr = Array.from(files as ArrayLike<File | UploadEntry>);
  return arr.map((f) => {
    if (isEntry(f)) return f;
    const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath ?? '';
    if (rel) return { file: f, path: normalizeRelPath(rel), hasPath: true };
    return { file: f, path: normalizeRelPath(f.name), hasPath: false };
  });
}

function isEntry(v: File | UploadEntry): v is UploadEntry {
  return typeof (v as UploadEntry).path === 'string' && (v as UploadEntry).file instanceof File;
}

// The entry API is prefixed and only partially typed in lib.dom, so the
// shape this module actually uses is declared here rather than cast at
// each call site.
interface FsEntryLike {
  isFile: boolean;
  isDirectory: boolean;
  name: string;
  file?: (cb: (f: File) => void, err?: (e: unknown) => void) => void;
  createReader?: () => { readEntries: (cb: (e: FsEntryLike[]) => void, err?: (e: unknown) => void) => void };
}

/**
 * Read a drop, preserving relative directories where the browser
 * exposes them.
 *
 * ⚠️ MUST be called with the live `DataTransfer` from inside the drop
 * handler, and the caller must not `await` anything before it: the
 * synchronous `webkitGetAsEntry()` sweep at the top is the only chance
 * to take the handles.
 */
export async function entriesFromDataTransfer(dt: DataTransfer | null): Promise<UploadEntry[]> {
  if (!dt) return [];
  const flat = dt.files ? Array.from(dt.files) : [];

  // Synchronous capture. See the header.
  const roots: FsEntryLike[] = [];
  let sawItems = false;
  try {
    const items = dt.items ? Array.from(dt.items) : [];
    for (const it of items) {
      if (it.kind !== 'file') continue;
      sawItems = true;
      const getter = (it as DataTransferItem & { webkitGetAsEntry?: () => FsEntryLike | null })
        .webkitGetAsEntry;
      const entry = typeof getter === 'function' ? getter.call(it) : null;
      if (entry) roots.push(entry);
    }
  } catch {
    // Some engines throw on `items` access outside the event turn.
    // Falling through to the flat list is the honest degradation.
  }

  if (roots.length === 0) {
    // No entry API, or the drop carried no readable entries. The flat
    // list is all there is, and it is reported as pathless.
    return entriesFromFiles(flat);
  }

  const out: UploadEntry[] = [];
  for (const root of roots) {
    if (out.length >= MAX_ENTRIES) break;
    await walk(root, '', out, 0);
  }

  if (out.length === 0 && sawItems && flat.length > 0) return entriesFromFiles(flat);
  return out;
}

async function walk(
  entry: FsEntryLike,
  prefix: string,
  out: UploadEntry[],
  depth: number,
): Promise<void> {
  if (out.length >= MAX_ENTRIES || depth > MAX_DEPTH) return;
  if (entry.isFile) {
    const file = await readFile(entry);
    if (!file) return;
    out.push({
      file,
      // A top-level dropped FILE has no directory information and must
      // say so; a file found INSIDE a dropped directory does.
      path: prefix ? normalizeRelPath(`${prefix}/${entry.name}`) : normalizeRelPath(entry.name),
      hasPath: prefix !== '',
    });
    return;
  }
  if (!entry.isDirectory || typeof entry.createReader !== 'function') return;
  const nextPrefix = prefix ? `${prefix}/${entry.name}` : entry.name;
  const reader = entry.createReader();
  // readEntries pages. Loop until it answers empty (see the header).
  for (;;) {
    const batch = await readBatch(reader);
    if (batch.length === 0) break;
    for (const child of batch) {
      if (out.length >= MAX_ENTRIES) return;
      await walk(child, nextPrefix, out, depth + 1);
    }
  }
}

function readFile(entry: FsEntryLike): Promise<File | null> {
  return new Promise((resolve) => {
    if (typeof entry.file !== 'function') {
      resolve(null);
      return;
    }
    entry.file(
      (f) => resolve(f),
      () => resolve(null),
    );
  });
}

function readBatch(reader: {
  readEntries: (cb: (e: FsEntryLike[]) => void, err?: (e: unknown) => void) => void;
}): Promise<FsEntryLike[]> {
  return new Promise((resolve) => {
    reader.readEntries(
      (e) => resolve(e ?? []),
      () => resolve([]),
    );
  });
}
