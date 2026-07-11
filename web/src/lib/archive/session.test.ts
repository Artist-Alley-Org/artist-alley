// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Unit tests for the pure helpers exported by session.svelte.ts.
// The store factory itself relies on $state runes which need a
// Svelte-compiled .svelte.ts file at runtime — covered by component
// tests later; this file pins the helpers a regression in the tree
// builder would silently break, since both ArchiveView and
// ArchiveTool depend on the shape it produces.

import { describe, expect, it } from 'vitest';
import type { ArchiveEntry, TreeNode } from './session.svelte';
import { buildTree, fmtBytes } from './session.svelte';

function entry(path: string, isDir = false, size = 0): ArchiveEntry {
  return {
    path,
    size,
    compressedSize: 0,
    modified: '',
    isDir,
    comment: '',
  };
}

function findNode(nodes: TreeNode[], path: string): TreeNode | undefined {
  for (const n of nodes) {
    if (n.path === path) return n;
    const inChild = findNode(n.children, path);
    if (inChild) return inChild;
  }
  return undefined;
}

describe('fmtBytes', () => {
  it.each([
    [0, '0 B'],
    [512, '512 B'],
    [1024, '1.0 KB'],
    [1024 * 1024, '1.0 MB'],
    [1024 * 1024 * 1024, '1.00 GB'],
  ])('formats %i as %s', (n, want) => {
    expect(fmtBytes(n)).toBe(want);
  });

  it('returns em dash for negative or null input', () => {
    expect(fmtBytes(-1)).toBe('—');
    // @ts-expect-error — runtime nulls happen when manifest field is absent.
    expect(fmtBytes(null)).toBe('—');
  });
});

describe('buildTree', () => {
  it('projects a flat list into a folder/file tree', () => {
    const tree = buildTree(
      [
        entry('readme.txt', false, 100),
        entry('src/main.go', false, 200),
        entry('src/util/helper.go', false, 50),
      ],
      '',
      false,
    );

    // Folders sort before files at each level.
    expect(tree.map((n) => n.name)).toEqual(['src', 'readme.txt']);

    const src = findNode(tree, 'src');
    expect(src?.isDir).toBe(true);
    // Folder size rolls up the descendant file sizes.
    expect(src?.size).toBe(250);
    expect(src?.childCount).toBe(2); // main.go + util/

    const helper = findNode(tree, 'src/util/helper.go');
    expect(helper?.isDir).toBe(false);
    expect(helper?.size).toBe(50);
  });

  it('applies the case-insensitive filter against the full path', () => {
    const entries = [
      entry('README.md', false, 1),
      entry('src/main.go', false, 1),
      entry('src/util/helper.go', false, 1),
    ];
    const tree = buildTree(entries, 'helper', false);
    // Only the helper.go path matches — but its ancestor folders
    // still render so the tree stays navigable.
    expect(findNode(tree, 'src/util/helper.go')).toBeDefined();
    expect(findNode(tree, 'README.md')).toBeUndefined();
    expect(findNode(tree, 'src/main.go')).toBeUndefined();
  });

  it('hides dotfile-rooted paths when hideDotfiles is true', () => {
    const tree = buildTree(
      [
        entry('.git/HEAD', false, 1),
        entry('src/.DS_Store', false, 1),
        entry('src/main.go', false, 1),
      ],
      '',
      true,
    );
    expect(findNode(tree, '.git/HEAD')).toBeUndefined();
    expect(findNode(tree, 'src/.DS_Store')).toBeUndefined();
    expect(findNode(tree, 'src/main.go')).toBeDefined();
  });

  it('keeps dotfile entries when hideDotfiles is false', () => {
    const tree = buildTree(
      [entry('.git/HEAD', false, 1)],
      '',
      false,
    );
    expect(findNode(tree, '.git/HEAD')).toBeDefined();
  });

  it('treats explicit directory entries as folders', () => {
    const tree = buildTree(
      [entry('empty-folder/', true, 0)],
      '',
      false,
    );
    expect(tree).toHaveLength(1);
    expect(tree[0].name).toBe('empty-folder');
    expect(tree[0].isDir).toBe(true);
  });
});
