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
  };
}

function noop() { /* default impl */ }

// ---------------------------------------------------------------------------
// Kind detection — single source of truth so PostModal / PostCard /
// AssetCard agree on what kind of asset they're looking at.
// ---------------------------------------------------------------------------

const IMAGE_EXTS = new Set([
  'jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'tiff', 'tif',
  'avif', 'heic', 'heif',
]);
const VIDEO_EXTS = new Set([
  'mp4', 'mov', 'mkv', 'webm', 'avi', 'wmv', 'mpg', 'mpeg', '3gp',
  'flv', 'm4v', 'ts',
]);
const AUDIO_EXTS = new Set([
  'mp3', 'wav', 'flac', 'ogg', 'oga', 'm4a', 'aac', 'opus',
]);
const PDF_EXTS = new Set(['pdf']);
const MODEL_EXTS = new Set([
  'glb', 'gltf', 'obj', 'fbx', 'usdz', 'usd', 'usda', 'usdc', 'blend',
  'mb', 'ma', 'max', 'mview',
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
