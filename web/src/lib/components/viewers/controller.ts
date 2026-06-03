// Shared contract between AssetViewer (the shell that owns chrome
// like the HUD, scrubber, transport bar, fullscreen, hotkeys,
// pan+zoom, and — later — annotations + presentation room) and the
// per-asset-kind view bodies (VideoView, ImageView, PDFView, …).
//
// The "anchor" concept is the unifying primitive: every asset kind
// has a position you can scrub to.
//
//   image  — no anchor (single static frame)
//   video  — frame number
//   pdf    — page number
//   audio  — seconds
//   3d     — camera + time-on-track (later)
//   seq    — frame index over the image-sequence
//
// View bodies write into the controller as their internal state
// advances; the shell reads from it to render HUD + transport. Each
// view body also installs the transport actions (play / seek /
// stepFrames / …) so the shell can call them without knowing what
// kind of asset is mounted.

export type ViewKind =
  | 'image'
  | 'video'
  | 'pdf'
  | 'audio'
  | 'sequence'
  | 'font'
  | 'sprite'
  | '3d'
  | 'ebook'
  | 'doc'
  | 'audiobook'
  | 'archive'
  | 'placeholder';

// ViewAsset is the trimmed asset shape every view body accepts as
// its `asset` prop. Hoisted here so AssetViewer + each per-kind
// body reference one canonical type — historically each viewer
// declared its own local `Asset` interface, which made TypeScript
// reject the prop binding with "Type 'Asset' is not assignable to
// type 'Asset'. Two different types with this name exist" whenever
// one of them drifted (the host adding `metadata`, an AudioView
// adding narrower tag types, etc).
//
// Keep this shape loose — view bodies that need richer per-kind
// metadata read it out of `metadata` (a JSONB blob the backend
// stamps via preview workers) under their own namespace, e.g.
// AudioView reads asset.metadata?.audio.
export interface ViewAsset {
  id: string;
  title?: string | null;
  file_extension?: string | null;
  file_hash?: string | null;
  /** Numeric ref into the asset_types table. Overrides the
      extension-based kind detection when set — important for
      kinds whose extensions overlap with another bucket (a
      sprite atlas is a PNG; a texture is a PNG; only the
      asset_type can tell them apart at the viewer layer). */
  asset_type?: number | null;
  /** Asset-level JSONB. Per-kind view bodies read their own
      namespaced keys (audio, pdf, font, video, etc). */
  metadata?: Record<string, unknown> | null;
}

// Per-kind review tools the shell renders in its right pane. Each
// group is optional — view bodies populate only what they support
// (ModelView fills the 3D set, ImageView could later fill a
// `zoomPresets` group, VideoView could expose audio/waveform toggles).
// The shell renders any group that's defined and skips the rest, so
// adding a new tool is "add to the interface + populate from the
// view body" without touching the panel layout.
export interface ViewTools {
  // ── Numeric controls — sliders. value is the live state the panel
  //    reads; set is the mutator the panel calls on drag.
  exposure?: ToolNumeric;
  envIntensity?: ToolNumeric;
  autoRotateSpeed?: ToolNumeric;

  // ── Toggles
  grid?: ToolToggle;
  axes?: ToolToggle;
  autoRotate?: ToolToggle;
  groundShadow?: ToolToggle;

  // ── Cycle (3-or-more-state buttons)
  wireframe?: ToolCycle<'off' | 'on' | 'overlay'>;

  // ── One-shot actions
  frameAll?: () => void;
  resetCamera?: () => void;

  // ── Camera presets (when supported). Closed enum so the shell can
  //    render the icons consistently.
  cameraPreset?: (preset: CameraPreset) => void;

  // ── Animations (3D + video). Populated when the asset has clips;
  //    the shell renders a clip selector + transport bar from this.
  animations?: AnimationState | null;
}

export type CameraPreset = 'front' | 'back' | 'top' | 'bottom' | 'left' | 'right' | 'iso';

export interface ToolNumeric {
  value: number;
  min: number;
  max: number;
  step?: number;
  label?: string;
  set: (v: number) => void;
}

export interface ToolToggle {
  enabled: boolean;
  toggle: () => void;
}

export interface ToolCycle<T extends string> {
  mode: T;
  options: readonly T[];
  cycle: () => void;
}

export interface AnimationClip {
  name: string;
  duration: number;
}

export interface AnimationState {
  clips: AnimationClip[];
  current: number;      // index into clips
  select: (idx: number) => void;
}

export interface ViewController {
  // Identity
  kind: ViewKind;
  // True for anything with a timeline (video / pdf / audio / seq /
  // animated-3d). False for plain images. Drives whether the
  // transport bar + scrubber render at all.
  hasTimeline: boolean;

  // Timeline state
  totalFrames: number;       // total scrub units (frames, pages, seconds*fps)
  currentFrame: number;      // current position in the same units
  fps: number;               // playback rate of the timeline, in frames/sec
  duration: number;          // total seconds, when meaningful

  // Playback state
  playing: boolean;
  rate: number;              // playback speed; 1 = normal

  // Sprite-sheet scrub preview, when one exists for this asset.
  spritesUrl: string | null;
  spritesVttUrl: string | null;

  // Loop region in `currentFrame` units. Both null = no loop. Set
  // via the shell's I/O hotkeys, the transport bar's Loop in/out
  // buttons, or — for audio — a shift-drag on the waveform. The
  // shell's loop enforcer reads them every frame and seeks back to
  // loopIn when currentFrame passes loopOut. View bodies (MediaView)
  // can also write them so per-kind UIs stay in sync with the shell
  // (e.g. the audio waveform's drag-to-set-region gesture).
  loopIn: number | null;
  loopOut: number | null;

  // Transport — view bodies install real implementations on mount.
  play: () => void;
  pause: () => void;
  togglePlay: () => void;
  seekToFrame: (frame: number) => void;
  stepFrames: (n: number) => void;
  setRate: (r: number) => void;

  // Display helpers — the view body produces a HUD label since its
  // formatting differs (timecode vs page X / Y vs MM:SS).
  formatAnchor: (frame: number) => string;
  hudExtra: string;

  // Per-kind review tools. View bodies populate this on mount; the
  // shell's right pane reads from it when reviewMode is on. Null
  // means "no tools for this kind yet" (e.g. placeholder body).
  tools: ViewTools | null;
}

export function defaultController(): ViewController {
  return {
    kind: 'placeholder',
    hasTimeline: false,
    totalFrames: 0,
    currentFrame: 0,
    fps: 24,
    duration: 0,
    playing: false,
    rate: 1,
    spritesUrl: null,
    spritesVttUrl: null,
    loopIn: null,
    loopOut: null,
    play: noop,
    pause: noop,
    togglePlay: noop,
    seekToFrame: noop,
    stepFrames: noop,
    setRate: noop,
    formatAnchor: (n) => String(n),
    hudExtra: '',
    tools: null,
  };
}

function noop() { /* default impl */ }

// ---------------------------------------------------------------------------
// Kind detection — single source of truth so PostModal / PostCard /
// AssetCard agree on what kind of asset they're looking at.
// ---------------------------------------------------------------------------

// Kept in sync with app/internal/assets/handler.go::imageExts. SVG is
// in here so it routes to ImageView (rendered natively by the
// browser via <img>) and gets a rasterized variant ladder produced by
// the backend SVG decoder (oksvg).
// "Image" kind covers anything ImageView can mount via its hires-
// variant fallback. Native browser-renderable formats (jpg/png/gif/
// webp/avif) sit alongside formats that require the backend preview
// pipeline — ImageView fetches /variants/hires which the backend
// rasterises to WEBP for every format a worker handles. The
// Download original link in Details still gives the user the
// source bytes.
//
// Routing groups:
//   raster (native)     : jpg / jpeg / png / gif / webp / bmp / avif
//   raster (heavy)      : tiff / heic / heif / hdr / exr / pic
//   vector              : svg (oksvg), eps / ps (Ghostscript)
//   authoring           : psd / psb (preview/psd.go)
//   document / ebook    : epub / mobi (preview/epub.go) — cover only
//   comic               : cbz / cbr / cb7 (preview/comic.go) — cover only
//
// For ebook + comic the hires variant is the cover page only. A
// proper multi-page reader is its own future view body; for now
// the cover + Details metadata is the same user experience the
// browse-card already provides.
const IMAGE_EXTS = new Set([
  'jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'tiff', 'tif',
  'avif', 'heic', 'heif', 'svg',
  'hdr', 'exr', 'pic',
  // Vector + Photoshop sources — backend preview workers
  // rasterise these to a WEBP hires variant.
  'eps', 'ps', 'psd', 'psb',
  // Comic + MOBI — cover thumbnail only for now. ComicView
  // (page-by-page reader) is its own future view body. MOBI is
  // included optimistically — backend support lands when the
  // ebook worker grows it; until then the viewer falls back to
  // the source file + ImageView's friendly placeholder.
  'mobi',
  'cbz', 'cbr', 'cb7',
  // EPUB used to live here for cover-only display. It now lives
  // under the 'ebook' kind below + routes to EpubView, a real
  // page-by-page reader. Kept this comment as routing-history.
]);

// 'ebook' is the semantic kind for any page-by-page document.
// The view body is picked by extension inside AssetViewer:
//   epub → EpubView (the goreader-backed reader)
//   mobi → future MobiView when backend support lands
// Both share the kind so panel tools / shortcuts can live on the
// kind level without each format duplicating chrome.
const EBOOK_EXTS = new Set(['epub']);

// 'doc' is the semantic kind for any plaintext / code / structured-
// text document. Routes to DocView (CodeMirror 6 — read-only first
// cut, edit later). One kind covers every text-shaped format so the
// reading prefs, find/replace, annotations, and bookmarks live in
// one place and don't need to be wired per-extension.
//
// Kept deliberately broad — anything a text editor would open is
// fair game here. Office documents (.docx / .odt / .rtf) need a
// backend converter so they sit out for now; a future Phase D will
// route them through the doc kind via server-side text extraction.
const DOC_EXTS = new Set([
  // plain
  'txt', 'log', 'csv', 'tsv',
  // markdown / docs
  'md', 'markdown', 'mdx', 'rst', 'adoc', 'org',
  // config / data
  'json', 'jsonc', 'yaml', 'yml', 'toml', 'ini', 'cfg', 'conf',
  'env', 'properties',
  // shell / build
  'sh', 'bash', 'zsh', 'fish', 'ps1',
  'makefile', 'mk', 'dockerfile', 'gitignore', 'gitattributes',
  // programming languages
  'py', 'pyi', 'rb', 'lua', 'pl', 'pm',
  'js', 'mjs', 'cjs', 'jsx', 'ts', 'tsx',
  'go', 'rs', 'java', 'kt', 'kts', 'scala', 'swift', 'dart',
  'c', 'h', 'cpp', 'cc', 'cxx', 'hpp', 'hh', 'm', 'mm', 'cs',
  'php', 'hs', 'erl', 'ex', 'exs', 'clj', 'cljs', 'edn',
  // web (svg lives in IMAGE_EXTS — it renders natively as an
  // image. Users wanting to see svg source can flip the asset
  // type via the metadata panel; future "view as text" override
  // will surface here.)
  'html', 'htm', 'css', 'scss', 'sass', 'less',
  'vue', 'svelte',
  // data + queries
  'sql', 'graphql', 'gql', 'xml', 'plist',
  // patches / diffs
  'patch', 'diff',
]);
// Kept in sync with app/internal/assets/handler.go::videoExts. Camera-
// proxy + broadcast formats included so a GoPro .lrv / Insta360 .insv
// / AVCHD .mts / .m2ts / DVD .vob / broadcast .mxf / Flash .f4v
// upload lands as Video instead of Photo or placeholder.
const VIDEO_EXTS = new Set([
  'mp4', 'mov', 'mkv', 'webm', 'avi', 'wmv', 'mpg', 'mpeg', '3gp',
  'flv', 'm4v', 'ts', 'lrv', 'insv', 'mts', 'm2ts', 'vob', 'f4v',
  'mxf',
]);
const AUDIO_EXTS = new Set([
  'mp3', 'wav', 'flac', 'ogg', 'oga', 'm4a', 'aac', 'opus',
]);
// 'audiobook' is the semantic kind for spoken-word long-form audio.
// .m4b is the de-facto container (AAC inside MP4 with chapter atoms);
// .aax is Audible's encrypted variant — currently a placeholder since
// decryption needs activation bytes per Amazon account.
const AUDIOBOOK_EXTS = new Set(['m4b', 'aax']);
// 'archive' kind covers every container the ArchiveView can browse
// without extraction. Mirrors preview.archive.SupportedExtensions
// + the assets/handler.go archiveExtsHandler dispatcher.
// .cbz / .cbr / .cb7 stay on 'image' (comic-cover preview path);
// the archive viewer ignores them since the comic reader is a
// better fit for sequential image-page browsing.
const ARCHIVE_EXTS = new Set([
  'zip', 'jar', 'war', 'ear', 'apk', 'ipa',
  'tar', 'tgz',
]);
const PDF_EXTS = new Set(['pdf']);
// Kept in sync with app/internal/preview/font.go::fontExts.
const FONT_EXTS = new Set(['ttf', 'otf', 'ttc', 'otc', 'woff', 'woff2']);
// Kept in sync with app/internal/assets/handler.go::modelExts. Anything
// the backend treats as a 3D upload should match here so the browse
// card swaps to the 3D hover-scrub (6×6 turntable sprite) instead of
// the video grid (10×10), and AssetViewer mounts ModelView instead of
// the generic placeholder.
//
// mb / ma / max stay on the frontend list as a viewer placeholder hint
// even though the backend pipeline can't render them yet.
const MODEL_EXTS = new Set([
  'glb', 'gltf', 'obj', 'fbx', 'blend', 'mview',
  'dae', 'ply', 'stl', '3ds', 'x3d', 'wrl',
  'usd', 'usda', 'usdc', 'usdz', 'abc',
  'md2', 'md3', 'mdl', 'ms3d',
  'mb', 'ma', 'max',
]);

export function kindForExtension(ext: string | null | undefined): ViewKind {
  if (!ext) return 'placeholder';
  const e = ext.toLowerCase().replace(/^\./, '');
  if (EBOOK_EXTS.has(e)) return 'ebook';
  if (AUDIOBOOK_EXTS.has(e)) return 'audiobook';
  if (IMAGE_EXTS.has(e)) return 'image';
  if (VIDEO_EXTS.has(e)) return 'video';
  if (AUDIO_EXTS.has(e)) return 'audio';
  if (PDF_EXTS.has(e)) return 'pdf';
  if (FONT_EXTS.has(e)) return 'font';
  if (MODEL_EXTS.has(e)) return '3d';
  if (DOC_EXTS.has(e)) return 'doc';
  if (ARCHIVE_EXTS.has(e)) return 'archive';
  return 'placeholder';
}

// Asset-type refs that override the extension-based kind. A PNG
// uploaded as a Sprite (ref=13) routes to SpriteView even though
// `.png` would otherwise resolve to `image`. Mirror of the
// asset_types table seeded by migrations 00031 / 00033 / 00034.
const ASSET_TYPE_KIND: Record<number, ViewKind> = {
  6: 'archive',
  11: 'audiobook',
  13: 'sprite',
};

/** Resolve the view kind from an asset's full shape. Prefers
 *  asset_type when set (so a sprite atlas PNG opens in SpriteView
 *  even though its extension says image); falls back to the
 *  extension-based detector. */
export function kindForAsset(asset: { asset_type?: number | null; file_extension?: string | null }): ViewKind {
  if (asset.asset_type != null) {
    const k = ASSET_TYPE_KIND[asset.asset_type];
    if (k) return k;
  }
  return kindForExtension(asset.file_extension);
}

export function isImageExt(ext: string | null | undefined): boolean {
  return kindForExtension(ext) === 'image';
}
export function isVideoExt(ext: string | null | undefined): boolean {
  return kindForExtension(ext) === 'video';
}
export function is3DExt(ext: string | null | undefined): boolean {
  return kindForExtension(ext) === '3d';
}
export function isDocExt(ext: string | null | undefined): boolean {
  return kindForExtension(ext) === 'doc';
}
export function isAudiobookExt(ext: string | null | undefined): boolean {
  return kindForExtension(ext) === 'audiobook';
}
export function isArchiveExt(ext: string | null | undefined): boolean {
  return kindForExtension(ext) === 'archive';
}
