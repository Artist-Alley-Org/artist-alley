// Sprite-sheet auto-detection — find non-background pixel regions
// in a sprite sheet, return a list of bounding boxes the playback
// engine can use as frames. Inspired by Spriters Resource's Sprite
// Splitter (https://tools.spriters-resource.com/#sprite-splitter)
// but reimplemented from scratch — pure algorithm, no UI here.
//
// Algorithm:
//   1. Classify every pixel as background or foreground. Background
//      = alpha < 8 when no chroma key is set, OR within `bgTolerance`
//      RGB distance of `bgColor` when one is set.
//   2. Flood-fill (iterative BFS, 8-connected) every unvisited
//      foreground pixel into a connected component; record each
//      component's bounding box.
//   3. Merge boxes that sit within `mergeGap` pixels of each other —
//      catches sprites whose head + body are technically disconnected
//      (e.g. a hat with empty pixels between it and the body).
//   4. Filter by min/max width + height.
//   5. Sort by the caller's chosen mode.
//
// Performance notes:
// - Uses Uint8Array for the visited + isBg masks so a 1024×1024
//   sheet is ~2 MB of typed-array memory total. Fine for any web
//   sprite sheet you'd reasonably encounter.
// - BFS uses an Int32Array as a stack to avoid recursion + per-push
//   GC pressure. Stack capacity is bounded by the component size,
//   not the whole image.
// - Merge pass is O(N² × passes). Typical sheets have < 100 boxes
//   post-detect; even 500+ converges in tens of ms.

export interface DetectOptions {
  /** RGB chroma-key colour. null = use the alpha channel for
   *  background detection (the common case for modern PNG sprite
   *  sheets that ship a real alpha channel). */
  bgColor: { r: number; g: number; b: number } | null;
  /** Euclidean RGB tolerance when `bgColor` is set. 0 = exact match
   *  only; ~30 catches mild JPEG-style colour drift on supposedly-
   *  uniform backgrounds. */
  bgTolerance: number;
  /** Merge boxes whose rects sit within this many pixels of each
   *  other (Chebyshev distance — both axes count). 0 = no merging.
   *  ~4 is a good default for game-rip sheets with floating hat or
   *  accessory pixels detached from the body. */
  mergeGap: number;
  /** Filter — drop boxes smaller than this on either axis. Keeps
   *  one-pixel speckle out of the result. */
  minW: number;
  minH: number;
  /** Filter — drop boxes larger than this on either axis. Catches
   *  the rare case where the whole sheet is one connected region
   *  (transparent-channel sheet with a giant background bar). */
  maxW: number;
  maxH: number;
}

export type SortMode =
  | 'position'      // y first, then x — reading order
  | 'animationRows' // group by y-overlap row, then x within row
  | 'sizeDesc'      // biggest area first
  | 'widthAsc'
  | 'heightAsc';

export interface DetectedBox { x: number; y: number; w: number; h: number; }

/** Run the full detection pipeline on a fully-loaded image. Returns
 *  the filtered + sorted boxes. The image must already be drawn
 *  onto an offscreen canvas so we can read getImageData; the caller
 *  passes the canvas so this module stays browser-agnostic about
 *  the rendering context. */
export function detectSprites(
  imageData: ImageData,
  opts: DetectOptions,
  sort: SortMode = 'position',
): DetectedBox[] {
  const { width, height, data } = imageData;
  if (width === 0 || height === 0) return [];

  // ── Phase 1: classify each pixel ────────────────────────────
  // isBg[i] = 1 when pixel i is background, 0 otherwise.
  const isBg = new Uint8Array(width * height);
  const useAlpha = opts.bgColor === null;
  if (useAlpha) {
    for (let i = 0; i < isBg.length; i++) {
      isBg[i] = data[i * 4 + 3] < 8 ? 1 : 0;
    }
  } else {
    const { r: br, g: bg, b: bb } = opts.bgColor!;
    const tolSq = opts.bgTolerance * opts.bgTolerance;
    for (let i = 0; i < isBg.length; i++) {
      const dr = data[i * 4] - br;
      const dg = data[i * 4 + 1] - bg;
      const db = data[i * 4 + 2] - bb;
      isBg[i] = (dr * dr + dg * dg + db * db) <= tolSq ? 1 : 0;
    }
  }

  // ── Phase 2: flood-fill connected components ────────────────
  const visited = new Uint8Array(width * height);
  const stack = new Int32Array(width * height);
  const boxes: DetectedBox[] = [];
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      const seed = y * width + x;
      if (visited[seed] || isBg[seed]) continue;
      let sp = 0;
      stack[sp++] = seed;
      visited[seed] = 1;
      let minX = x, minY = y, maxX = x, maxY = y;
      while (sp > 0) {
        const cur = stack[--sp];
        const cx = cur % width;
        const cy = (cur / width) | 0;
        if (cx < minX) minX = cx;
        if (cy < minY) minY = cy;
        if (cx > maxX) maxX = cx;
        if (cy > maxY) maxY = cy;
        // 8-connected neighbourhood — diagonal connectivity catches
        // anti-aliased edges that would otherwise split one sprite
        // into multiple thin components.
        for (let dy = -1; dy <= 1; dy++) {
          for (let dx = -1; dx <= 1; dx++) {
            if (dx === 0 && dy === 0) continue;
            const nx = cx + dx;
            const ny = cy + dy;
            if (nx < 0 || ny < 0 || nx >= width || ny >= height) continue;
            const nidx = ny * width + nx;
            if (visited[nidx] || isBg[nidx]) continue;
            visited[nidx] = 1;
            stack[sp++] = nidx;
          }
        }
      }
      boxes.push({ x: minX, y: minY, w: maxX - minX + 1, h: maxY - minY + 1 });
    }
  }

  // ── Phase 3: iterative bbox merge ──────────────────────────
  // Two boxes merge if their padded rects overlap (Chebyshev gap
  // ≤ mergeGap). Loop until a full pass produces no merges.
  if (opts.mergeGap > 0) {
    let changed = true;
    while (changed) {
      changed = false;
      for (let i = 0; i < boxes.length; i++) {
        let j = i + 1;
        while (j < boxes.length) {
          if (boxesWithinGap(boxes[i], boxes[j], opts.mergeGap)) {
            boxes[i] = unionBox(boxes[i], boxes[j]);
            boxes.splice(j, 1);
            changed = true;
          } else {
            j++;
          }
        }
      }
    }
  }

  // ── Phase 4: filter by size ────────────────────────────────
  const filtered = boxes.filter(
    (b) => b.w >= opts.minW && b.h >= opts.minH && b.w <= opts.maxW && b.h <= opts.maxH,
  );

  // ── Phase 5: sort ──────────────────────────────────────────
  return sortBoxes(filtered, sort);
}

function boxesWithinGap(a: DetectedBox, b: DetectedBox, gap: number): boolean {
  const aRight = a.x + a.w;
  const aBottom = a.y + a.h;
  const bRight = b.x + b.w;
  const bBottom = b.y + b.h;
  const horizGap = Math.max(0, Math.max(a.x - bRight, b.x - aRight));
  const vertGap = Math.max(0, Math.max(a.y - bBottom, b.y - aBottom));
  return horizGap <= gap && vertGap <= gap;
}

function unionBox(a: DetectedBox, b: DetectedBox): DetectedBox {
  const x = Math.min(a.x, b.x);
  const y = Math.min(a.y, b.y);
  const xMax = Math.max(a.x + a.w, b.x + b.w);
  const yMax = Math.max(a.y + a.h, b.y + b.h);
  return { x, y, w: xMax - x, h: yMax - y };
}

export function sortBoxes(boxes: DetectedBox[], mode: SortMode): DetectedBox[] {
  const out = [...boxes];
  switch (mode) {
    case 'position':
      out.sort((a, b) => (a.y - b.y) || (a.x - b.x));
      break;
    case 'animationRows':
      // Group boxes whose y-extents overlap by ≥ 50 % into one row,
      // sort rows by min-y, then sort within each row by x. Handles
      // sheets where a single row has slightly-different-height
      // sprites (idle vs jump frames) without splitting them.
      out.sort((a, b) => (a.y - b.y) || (a.x - b.x));
      // Reorder so rows are clusters: walk in y-order, when the
      // current box's y is past the cluster's max-y by >50%, start
      // a new cluster. Within a cluster, x-sort.
      {
        const clusters: DetectedBox[][] = [];
        for (const box of out) {
          const last = clusters[clusters.length - 1];
          if (!last) { clusters.push([box]); continue; }
          const lastMaxY = Math.max(...last.map((b) => b.y + b.h));
          // Threshold: half the cluster's average height — boxes
          // whose top sits above that are still the same row.
          const avgH = last.reduce((s, b) => s + b.h, 0) / last.length;
          if (box.y < lastMaxY - avgH * 0.5) last.push(box);
          else clusters.push([box]);
        }
        out.length = 0;
        for (const c of clusters) {
          c.sort((a, b) => a.x - b.x);
          out.push(...c);
        }
      }
      break;
    case 'sizeDesc':
      out.sort((a, b) => (b.w * b.h) - (a.w * a.h));
      break;
    case 'widthAsc':
      out.sort((a, b) => a.w - b.w);
      break;
    case 'heightAsc':
      out.sort((a, b) => a.h - b.h);
      break;
  }
  return out;
}

// ── Image analysis (Sprite Analyzer's Overview tab parity) ────

export interface SheetAnalysis {
  width: number;
  height: number;
  totalPixels: number;
  transparentPixels: number;
  /** Pixels whose alpha is below 255 but above 0 — anti-aliased
   *  edges; useful for "is this a clean pixel-art sheet" check. */
  semiTransparentPixels: number;
  /** Distinct RGBA values used (capped at 4096 to keep the Map
   *  bounded on photographic sheets that wouldn't be pixel art
   *  anyway). */
  uniqueColors: number;
  /** Up to the first MAX palette entries in usage-frequency order.
   *  Each is `{ r, g, b, a, count }`. */
  palette: { r: number; g: number; b: number; a: number; count: number }[];
}

const MAX_UNIQUE_COLORS = 4096;
const MAX_PALETTE_ENTRIES = 256;

export function analyzeSheet(imageData: ImageData): SheetAnalysis {
  const { width, height, data } = imageData;
  const total = width * height;
  let transparent = 0;
  let semi = 0;
  const counts = new Map<number, number>();
  for (let i = 0; i < total; i++) {
    const r = data[i * 4];
    const g = data[i * 4 + 1];
    const b = data[i * 4 + 2];
    const a = data[i * 4 + 3];
    if (a === 0) { transparent++; continue; }
    if (a < 255) semi++;
    // Pack into one int32 — JS Maps key on numbers, packed key
    // beats string conversion by a wide margin for hot loops.
    const key = (r << 24) | (g << 16) | (b << 8) | a;
    counts.set(key, (counts.get(key) ?? 0) + 1);
    if (counts.size > MAX_UNIQUE_COLORS) {
      // Bail to a placeholder — sheet is photographic, palette
      // extraction isn't meaningful. UI surfaces this as
      // "1000+ colours" so the user knows it's an estimate.
      return {
        width, height, totalPixels: total,
        transparentPixels: transparent,
        semiTransparentPixels: semi,
        uniqueColors: counts.size,
        palette: [],
      };
    }
  }
  const palette = Array.from(counts.entries())
    .sort((a, b) => b[1] - a[1])
    .slice(0, MAX_PALETTE_ENTRIES)
    .map(([key, count]) => ({
      r: (key >>> 24) & 0xff,
      g: (key >>> 16) & 0xff,
      b: (key >>> 8)  & 0xff,
      a: key & 0xff,
      count,
    }));
  return {
    width, height, totalPixels: total,
    transparentPixels: transparent,
    semiTransparentPixels: semi,
    uniqueColors: counts.size,
    palette,
  };
}
