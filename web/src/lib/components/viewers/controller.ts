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
  | '3d'
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
const IMAGE_EXTS = new Set([
  'jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'tiff', 'tif',
  'avif', 'heic', 'heif', 'svg',
]);
const VIDEO_EXTS = new Set([
  'mp4', 'mov', 'mkv', 'webm', 'avi', 'wmv', 'mpg', 'mpeg', '3gp',
  'flv', 'm4v', 'ts',
]);
const AUDIO_EXTS = new Set([
  'mp3', 'wav', 'flac', 'ogg', 'oga', 'm4a', 'aac', 'opus',
]);
const PDF_EXTS = new Set(['pdf']);
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
  'md2', 'md3', 'mdl',
  'mb', 'ma', 'max',
]);

export function kindForExtension(ext: string | null | undefined): ViewKind {
  if (!ext) return 'placeholder';
  const e = ext.toLowerCase().replace(/^\./, '');
  if (IMAGE_EXTS.has(e)) return 'image';
  if (VIDEO_EXTS.has(e)) return 'video';
  if (AUDIO_EXTS.has(e)) return 'audio';
  if (PDF_EXTS.has(e)) return 'pdf';
  if (MODEL_EXTS.has(e)) return '3d';
  return 'placeholder';
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
