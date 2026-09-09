// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// On-disk fixtures for #1408's same-batch reconciliation spec.
//
// ## ⚠️ Everything here is BUILT, never checked in
//
// Storage is CONTENT-ADDRESSED and this stack's database PERSISTS
// between runs. A checked-in model or texture uploads once and
// deduplicates forever after: the second run's "upload" resolves to the
// hash already stored, and a spec asserting "the right texture landed
// on the right model" passes on whatever the first run left behind.
// Every file built here carries a per-run NONCE in its bytes, so each
// run genuinely uploads new content and each assertion is about content
// this run made.
//
// ## Why a real DIRECTORY on disk
//
// The half of #1408 that cannot be faked is the RELATIVE PATH. A flat
// multi-file selection gives basenames and an empty
// `webkitRelativePath`, which is exactly the case the matcher must
// refuse to guess about. Directory information only exists when the
// browser produces it: a `webkitdirectory` input, or a folder drop
// read through `webkitGetAsEntry`. Playwright can drive the first by
// pointing `setInputFiles` at a real directory, so the tree has to be
// real.

import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

/** A .gltf JSON document declaring external images, with a nonce. */
export function buildGltf(declared: string[], nonce: string): Buffer {
  const doc = {
    asset: { version: '2.0', generator: 'artist-alley dogfood fixture' },
    // `extras` is application-specific and every parser ignores it, so
    // the nonce changes the content hash without changing the
    // reference list under test.
    extras: { dogfoodNonce: nonce },
    // No `uri` on the buffer: an embedded buffer needs no companion, so
    // a spec asserting "these are the declared files" cannot pass by
    // accidentally counting geometry.
    buffers: [{ byteLength: 4 }],
    images: declared.map((uri) => ({ uri })),
  };
  return Buffer.from(JSON.stringify(doc));
}

/**
 * A file whose bytes SAY which one it is.
 *
 * The whole no-cross-wiring claim is an identity claim: two textures
 * called `diffuse.png` in different directories must land on different
 * models. Asserting two companion rows exist proves nothing about
 * that. A swap is also two rows. So each carries a marker the spec
 * reads back out of the stored bytes.
 */
export function markerFile(marker: string): Buffer {
  return Buffer.from(`AA-1408-FIXTURE:${marker}:${'x'.repeat(32)}`);
}

export interface FixtureTree {
  /** Absolute path of the tree root. Hand this to setInputFiles. */
  root: string;
  /** Absolute path of one member, by its tree-relative path. */
  path(rel: string): string;
  cleanup(): void;
}

/** Write `files` (tree-relative path → bytes) into a fresh temp dir. */
export function writeTree(files: Record<string, Buffer>): FixtureTree {
  const root = mkdtempSync(join(tmpdir(), 'aa-1408-'));
  for (const [rel, bytes] of Object.entries(files)) {
    const abs = join(root, rel);
    mkdirSync(dirname(abs), { recursive: true });
    writeFileSync(abs, bytes);
  }
  return {
    root,
    path: (rel: string) => join(root, rel),
    cleanup: () => rmSync(root, { recursive: true, force: true }),
  };
}

/**
 * The title the upload store derives from a filename.
 *
 * Mirrors `defaultTitleFromFilename` in upload.svelte.ts, and is how a
 * spec finds the asset id for a file it uploaded through the BROWSER:
 * the POST happens in the page and the response never reaches the test
 * except by watching the traffic.
 */
export function titleForFilename(name: string): string {
  const dot = name.lastIndexOf('.');
  const base = dot > 0 ? name.slice(0, dot) : name;
  return base.replace(/[._-]+/g, ' ').trim() || name;
}
