// ArchiveSession — shared reactive state between ArchiveView (the
// canvas-area file-tree + entry preview) and ArchiveTool (the
// side-panel toolbox). Mirrors the Ebook / Doc / Audiobook session
// pattern: object-literal $state holding every field both ends
// touch, plus helper methods bound on directly.
//
// The manifest itself comes from asset.metadata.archive (cached by
// the preview.archive job — see app/internal/preview/archive.go).
// We mirror it into the session so the view + the panel can both
// read + write search/filter/selection state without prop drilling.
//
// Per-tab persisted prefs (filter glob, hide-dotfiles toggle) live
// in localStorage so a user who reaches for ".js$" once keeps it
// across archive opens.

export interface ArchiveEntry {
  /** Forward-slash path inside the archive ("folder/file.txt"). */
  path: string;
  /** Uncompressed bytes. */
  size: number;
  /** ZIP-only — compressed bytes; 0 for TAR. */
  compressedSize: number;
  modified: string;     // ISO timestamp
  isDir: boolean;
  comment: string;
}

export interface ArchiveManifest {
  format: string;       // "zip" / "tar" / "tar.gz"
  entries: ArchiveEntry[];
  entryCount: number;
  truncated: boolean;
  totalSize: number;
}

export interface ArchiveSession {
  // ── Source (viewer-populated) ───────────────────────────────
  manifest: ArchiveManifest | null;
  loading: boolean;
  loadError: string | null;

  // ── Selection / preview ─────────────────────────────────────
  /** Selected entry path. null when nothing is selected. */
  selectedPath: string | null;
  /** Inline-preview text for the selected entry when it's a
   *  text/code file (size cap applies). Empty for binary kinds. */
  previewText: string;
  /** Preview loading state — the entry endpoint is doing a
   *  single-file fetch. */
  previewLoading: boolean;
  previewError: string | null;
  /** Detected content type for the selected entry. */
  previewMime: string;
  /** Selected entry's size in bytes; mirrored from manifest so
   *  the panel can show "Preview" + "Download" with the right
   *  number without re-walking the entry list. */
  previewSize: number;

  // ── Filter / search ─────────────────────────────────────────
  /** Free-text filter applied to entry paths. Case-insensitive. */
  filter: string;
  /** Hide entries starting with "." or in dot-prefixed folders.
   *  Tar archives from MacOS / git checkouts pile these on; on
   *  by default. */
  hideDotfiles: boolean;
  /** Path-prefix expand state for the file tree. Maps "folder/path"
   *  → true when expanded. The tree builds folders lazily so this
   *  doesn't need to know about every entry. */
  expanded: Record<string, boolean>;
}

export interface ArchiveSessionMethods {
  setFilter(q: string): void;
  toggleHideDotfiles(): void;
  toggleFolder(p: string): void;
  /** Replace the entire expand map (used by Expand-all / Collapse-all). */
  setExpanded(next: Record<string, boolean>): void;
  selectEntry(path: string | null): void;
  /** Open the entry's bytes endpoint in a new tab — used by the
   *  Download button in both the canvas and the panel. */
  downloadEntry(path: string): void;
}

export type ArchiveSessionInstance =
  ArchiveSession & ArchiveSessionMethods & { assetId: string };

export interface ArchiveSessionOpts { assetId: string; }

const G_HIDE_DOT = 'aa.archive.hideDotfiles';

function readLS<T>(key: string, fallback: T): T {
  try {
    const v = localStorage.getItem(key);
    if (v == null) return fallback;
    return JSON.parse(v) as T;
  } catch {
    return fallback;
  }
}
function writeLS(key: string, value: unknown): void {
  try { localStorage.setItem(key, JSON.stringify(value)); } catch { /* ignore */ }
}

/** Human-friendly byte size formatter. */
export function fmtBytes(n: number): string {
  if (n == null || n < 0) return '—';
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

export function createArchiveSession(opts: ArchiveSessionOpts): ArchiveSessionInstance {
  const state = $state<ArchiveSession>({
    manifest: null,
    loading: true,
    loadError: null,
    selectedPath: null,
    previewText: '',
    previewLoading: false,
    previewError: null,
    previewMime: '',
    previewSize: 0,
    filter: '',
    hideDotfiles: readLS<boolean>(G_HIDE_DOT, true),
    expanded: {},
  });

  function setFilter(q: string) { state.filter = q; }
  function toggleHideDotfiles() {
    state.hideDotfiles = !state.hideDotfiles;
    writeLS(G_HIDE_DOT, state.hideDotfiles);
  }
  function toggleFolder(p: string) {
    state.expanded = { ...state.expanded, [p]: !state.expanded[p] };
  }
  function setExpanded(next: Record<string, boolean>) {
    state.expanded = next;
  }
  function selectEntry(path: string | null) {
    state.selectedPath = path;
    state.previewText = '';
    state.previewError = null;
    state.previewMime = '';
    state.previewSize = 0;
    if (!path || !state.manifest) return;
    const e = state.manifest.entries.find((x) => x.path === path);
    if (e) state.previewSize = e.size;
  }
  function downloadEntry(path: string) {
    const u = `/api/v1/assets/${opts.assetId}/archive/entry?path=${encodeURIComponent(path)}`;
    // Use a hidden anchor with download attribute so the browser
    // saves under the entry's basename rather than asking the user
    // every time.
    const a = document.createElement('a');
    a.href = u;
    const slash = path.lastIndexOf('/');
    a.download = slash >= 0 ? path.slice(slash + 1) : path;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
  }

  return Object.assign(state as ArchiveSessionInstance, {
    assetId: opts.assetId,
    setFilter, toggleHideDotfiles, toggleFolder, setExpanded,
    selectEntry, downloadEntry,
  });
}

/** Builds a virtual folder tree from the flat entry list. Returns
 *  the array of root-level nodes; each folder node carries its
 *  children. Walked once per filter/manifest change. */
export interface TreeNode {
  name: string;
  path: string;
  isDir: boolean;
  size: number;
  childCount: number;     // immediate children, including subfolders
  children: TreeNode[];   // empty for files
}

export function buildTree(
  entries: ArchiveEntry[],
  filter: string,
  hideDotfiles: boolean,
): TreeNode[] {
  const root: TreeNode = { name: '', path: '', isDir: true, size: 0, childCount: 0, children: [] };
  const lower = filter.toLowerCase();
  for (const e of entries) {
    if (e.path === '') continue;
    if (hideDotfiles) {
      // Any path segment starting with "." → skip.
      if (e.path.split('/').some((seg) => seg.startsWith('.'))) continue;
    }
    if (lower && !e.path.toLowerCase().includes(lower)) continue;
    insert(root, e);
  }
  computeSize(root);
  sortTree(root);
  return root.children;
}

function insert(root: TreeNode, e: ArchiveEntry) {
  const parts = e.path.replace(/\/$/, '').split('/');
  let cur = root;
  for (let i = 0; i < parts.length; i++) {
    const name = parts[i];
    if (!name) continue;
    const isLeaf = i === parts.length - 1 && !e.isDir;
    const childPath = parts.slice(0, i + 1).join('/');
    let next = cur.children.find((c) => c.name === name);
    if (!next) {
      next = {
        name,
        path: childPath,
        isDir: !isLeaf,
        size: isLeaf ? e.size : 0,
        childCount: 0,
        children: [],
      };
      cur.children.push(next);
    } else if (isLeaf && next.isDir) {
      // Some ZIPs list "folder/" AND "folder/file.txt" — the prior
      // pass treated folder as a dir; the file shouldn't replace it.
    }
    cur = next;
  }
}

function computeSize(n: TreeNode): number {
  if (!n.isDir) return n.size;
  let total = 0;
  for (const c of n.children) total += computeSize(c);
  n.size = total;
  n.childCount = n.children.length;
  return total;
}

function sortTree(n: TreeNode) {
  // Folders first, then files; alpha within each group.
  n.children.sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
  for (const c of n.children) sortTree(c);
}
