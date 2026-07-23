// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// ModelSession — shared reactive state between ModelView (the
// canvas-area three.js renderer) and ModelTool (the side-panel
// toolbox). Mirrors the EbookSession / SpriteSession pattern:
// object-literal $state holding every field both sides touch, plus
// helper methods bound on directly. Both ends bind the same
// instance so flipping the environment preset in the panel updates
// the renderer without an event bus.
//
// Persistence split:
//   * Per-asset (localStorage keyed on asset id):
//       savedView   — camera bookmark the user explicitly "Saved"
//   * Per-tab (localStorage, global key):
//       reading prefs the user wants applied to EVERY model:
//       env preset, tone mapping, exposure, lighting preset, render
//       mode, grid/axes, auto-rotate, projection / FOV / up axis.
//       Material overrides DON'T persist — they're per-asset and
//       per-session (the user can re-apply or save with the asset
//       later via the "save companion" path that lands with custom
//       materials).
//
// State the VIEWER fills in (not user-controlled, not persisted):
//   materials, clips, currentClip, animationTime, stats. Side panel
//   reads these to render its surfaces but never writes them.
//
// Whenever you add a field: decide which bucket (per-asset / global
// / viewer-fill) and update read+write helpers below.

import type { EnvPresetId, ToneMappingId } from './environments';
import {
  DEFAULT_KEY_INTENSITY,
  DEFAULT_FILL_INTENSITY,
  DEFAULT_RIM_INTENSITY,
  DEFAULT_TONE_MAPPING,
  DEFAULT_EXPOSURE,
} from './defaultLighting';

export type LightingPresetId =
  | 'three-point' | 'studio' | 'outdoor' | 'showroom' | 'custom';
export type RenderModeId =
  | 'solid' | 'wireframe' | 'overlay' | 'xray' | 'normals' | 'matcap';
export type ProjectionId = 'perspective' | 'orthographic';
export type UpAxisId = 'y' | 'z';
export type CameraPresetId =
  | 'iso' | 'front' | 'back' | 'left' | 'right' | 'top' | 'bottom';

export interface SavedView {
  pos: [number, number, number];
  target: [number, number, number];
  zoom: number;
  fov: number;
}

export interface MaterialEntry {
  /** Stable id assigned by the viewer (UUID per encounter). */
  id: string;
  /** Material.name from the source file, or "(unnamed)". */
  name: string;
  /** How many meshes reference this material. */
  meshCount: number;
  /** Original PBR values, for "Reset" to restore. */
  baseColor: string;     // hex
  baseMetalness: number;
  baseRoughness: number;
}

export interface MaterialOverride {
  color?: string;
  metalness?: number;
  roughness?: number;
}

export interface ClipEntry { name: string; duration: number; }

export interface ModelStats {
  vertices: number;
  triangles: number;
  meshes: number;
  materials: number;
  textures: number;
  drawCalls: number;
  fileSize: number | null; // bytes from /file HEAD; null if unknown
}

export interface ModelSession {
  // ── Camera ───────────────────────────────────────────────────
  cameraPreset: CameraPresetId | 'custom';
  projection: ProjectionId;
  /** Perspective field-of-view (degrees). Ignored when projection
   *  is orthographic; we keep the value so toggling back restores
   *  the user's choice. */
  fov: number;
  /** User-saved camera bookmark for this asset (Persistent across
   *  re-opens via localStorage). null = nothing saved. */
  savedView: SavedView | null;
  upAxis: UpAxisId;
  autoRotate: boolean;
  autoRotateSpeed: number;

  // ── Environment ──────────────────────────────────────────────
  envPreset: EnvPresetId;
  /** Multiplier on `scene.environmentIntensity` (three r163+).
   *  Range 0–3; default 1. */
  envIntensity: number;
  /** When true the environment renders as the scene background;
   *  when false the background falls back to the panel-defined
   *  flat color so the model pops without env clutter. */
  backgroundVisible: boolean;
  toneMapping: ToneMappingId;
  exposure: number;

  // ── Lighting ─────────────────────────────────────────────────
  lightingPreset: LightingPresetId;
  keyEnabled: boolean;
  keyIntensity: number;
  /** Spherical angles for the key light, both in degrees:
   *  azimuth = 0 → +Z, 90 → +X (looking down from above);
   *  elevation = 0 → horizon, 90 → straight up. */
  keyAzimuth: number;
  keyElevation: number;
  keyColor: string;
  fillEnabled: boolean;
  fillIntensity: number;
  rimEnabled: boolean;
  rimIntensity: number;
  shadows: boolean;
  shadowSoftness: number;     // 0..1 → maps to PCF radius
  groundPlane: boolean;
  contactShadow: boolean;

  // ── Display ──────────────────────────────────────────────────
  renderMode: RenderModeId;
  showGrid: boolean;
  showAxes: boolean;
  showBoundingBox: boolean;

  // ── Materials (viewer-populated, panel-edits-via-overrides) ──
  materials: MaterialEntry[];
  /** Per-material edits keyed on MaterialEntry.id. */
  materialOverrides: Record<string, MaterialOverride>;

  // ── Animation (viewer-populated, panel-drives transport) ─────
  clips: ClipEntry[];
  /** Index into clips; -1 = none (model has no clips, or "Off"). */
  currentClip: number;
  animationPlaying: boolean;
  animationSpeed: number;
  animationLoop: boolean;
  /** Seconds into the current clip. Viewer writes on every frame
   *  during playback; panel writes when the user scrubs. */
  animationTime: number;

  // ── Stats (viewer-populated, read-only from panel) ───────────
  stats: ModelStats | null;
  /** Loading lifecycle visible to the panel so it can render a
   *  spinner / disable controls during model load. */
  loading: boolean;
  loadError: string | null;
  /** Which backend the current asset rendered through. `three` is
   *  the GLTF/FBX/OBJ pipeline that drives every panel section
   *  reactively. `marmoset` is Marmoset Toolbag's closed-source
   *  WebViewer — it only exposes resetCamera/resize, so the panel
   *  hides sections it can't drive and points the user at
   *  Marmoset's built-in in-canvas chrome instead. `null` while
   *  loading. */
  backend: 'three' | 'marmoset' | null;
}

export interface ModelSessionOpts { assetId: string; }

export interface ModelSessionMethods {
  // Camera
  setCameraPreset(p: CameraPresetId): void;
  setProjection(p: ProjectionId): void;
  setFov(deg: number): void;
  setUpAxis(a: UpAxisId): void;
  saveCurrentView(view: SavedView): void;
  clearSavedView(): void;
  frameAll(): void;            // requests viewer to re-frame
  resetCamera(): void;         // requests viewer to reset to initial pose
  toggleAutoRotate(): void;
  setAutoRotateSpeed(v: number): void;

  // Environment
  setEnvPreset(p: EnvPresetId): void;
  setEnvIntensity(v: number): void;
  toggleBackgroundVisible(): void;
  setToneMapping(t: ToneMappingId): void;
  setExposure(v: number): void;

  // Lighting
  setLightingPreset(p: LightingPresetId): void;
  setKeyEnabled(v: boolean): void;
  setKeyIntensity(v: number): void;
  setKeyAzimuth(deg: number): void;
  setKeyElevation(deg: number): void;
  setKeyColor(hex: string): void;
  setFillEnabled(v: boolean): void;
  setFillIntensity(v: number): void;
  setRimEnabled(v: boolean): void;
  setRimIntensity(v: number): void;
  toggleShadows(): void;
  setShadowSoftness(v: number): void;
  toggleGroundPlane(): void;
  toggleContactShadow(): void;

  // Display
  setRenderMode(m: RenderModeId): void;
  toggleGrid(): void;
  toggleAxes(): void;
  toggleBoundingBox(): void;

  // Materials
  setMaterialOverride(id: string, patch: MaterialOverride): void;
  resetMaterialOverride(id: string): void;
  resetAllMaterials(): void;

  // Animation (panel commands)
  selectClip(idx: number): void;
  toggleAnimationPlaying(): void;
  setAnimationSpeed(v: number): void;
  toggleAnimationLoop(): void;
  /** Panel-initiated scrub. Viewer's effect on animationTime
   *  syncs the mixer when the value comes from outside. */
  scrubAnimation(seconds: number): void;

  /** Viewer-side reset on remount — called when a new asset
   *  loads to clear material/clip lists + stats. */
  resetForReload(): void;
  /** Full user-facing "reset to factory defaults". Snaps every
   *  persisted preference back to the canonical baseline (env,
   *  lighting, display, camera, materials) AND clears the
   *  per-asset saved view. Used by the panel's prominent reset
   *  button so a user who's been tinkering can get back to the
   *  out-of-the-box look without manually un-toggling each
   *  control. The viewer's $effects pick the changes up
   *  reactively. */
  resetAll(): void;

  /** Viewer-published callbacks — populated by ModelView when the
   *  three.js scene mounts. Undefined while loading or for the
   *  marmoset path (which doesn't expose camera state). The panel
   *  checks `if (session.snapshotView)` before calling. */
  snapshotView?: () => SavedView;
  restoreSavedView?: () => void;
}

export type ModelSessionInstance =
  ModelSession & ModelSessionMethods & { assetId: string };

// localStorage scoping: per-asset keys for things that only make
// sense for one model; global keys for the "I always read in
// sepia" style display prefs the user wants applied everywhere.
const VIEW_KEY = (id: string) => `aa.model.${id}.savedView`;

const G_ENV          = 'aa.model.envPreset';
const G_ENV_INT      = 'aa.model.envIntensity';
const G_BG_VIS       = 'aa.model.backgroundVisible';
const G_TONE         = 'aa.model.toneMapping';
const G_EXPOSURE     = 'aa.model.exposure';
const G_LIGHT_PRESET = 'aa.model.lightingPreset';
const G_KEY_ENABLED  = 'aa.model.keyEnabled';
const G_KEY_INTENS   = 'aa.model.keyIntensity';
const G_KEY_AZ       = 'aa.model.keyAzimuth';
const G_KEY_EL       = 'aa.model.keyElevation';
const G_KEY_COLOR    = 'aa.model.keyColor';
const G_FILL_EN      = 'aa.model.fillEnabled';
const G_FILL_INT     = 'aa.model.fillIntensity';
const G_RIM_EN       = 'aa.model.rimEnabled';
const G_RIM_INT      = 'aa.model.rimIntensity';
const G_SHADOWS      = 'aa.model.shadows';
const G_SHADOW_SOFT  = 'aa.model.shadowSoftness';
const G_GROUND       = 'aa.model.groundPlane';
const G_CONTACT      = 'aa.model.contactShadow';
const G_RENDER_MODE  = 'aa.model.renderMode';
const G_GRID         = 'aa.model.showGrid';
const G_AXES         = 'aa.model.showAxes';
const G_BBOX         = 'aa.model.showBoundingBox';
const G_PROJECTION   = 'aa.model.projection';
const G_FOV          = 'aa.model.fov';
const G_UP_AXIS      = 'aa.model.upAxis';
const G_AUTO_ROTATE  = 'aa.model.autoRotate';
const G_AUTO_SPEED   = 'aa.model.autoRotateSpeed';

// Factory defaults. Centralised so the createModelSession initial
// reads AND the user-facing "Reset to defaults" button agree on
// the canonical starting point. Anything tweaked through the panel
// drifts away from these values; resetAll() snaps everything back.
const DEFAULTS = {
  projection: 'perspective' as ProjectionId,
  fov: 45,
  upAxis: 'y' as UpAxisId,
  autoRotate: false,
  autoRotateSpeed: 2.0,
  envPreset: 'studio' as EnvPresetId,
  envIntensity: 1.0,
  backgroundVisible: false,
  toneMapping: DEFAULT_TONE_MAPPING,
  exposure: DEFAULT_EXPOSURE,
  lightingPreset: 'three-point' as LightingPresetId,
  keyEnabled: true,
  // Intensities come from the shared default rig so a freshly-opened
  // model reads like its browse-grid thumbnail (#509). See
  // defaultLighting.ts — keep in sync with scripts/threejs/render.html.
  keyIntensity: DEFAULT_KEY_INTENSITY,
  keyAzimuth: 45,
  keyElevation: 55,
  keyColor: '#ffffff',
  fillEnabled: true,
  fillIntensity: DEFAULT_FILL_INTENSITY,
  rimEnabled: true,
  rimIntensity: DEFAULT_RIM_INTENSITY,
  shadows: true,
  shadowSoftness: 0.5,
  groundPlane: false,
  contactShadow: true,
  renderMode: 'solid' as RenderModeId,
  showGrid: false,
  showAxes: false,
  showBoundingBox: false,
};

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

export function createModelSession(opts: ModelSessionOpts): ModelSessionInstance {
  const state = $state<ModelSession>({
    // Camera
    cameraPreset: 'iso',
    projection: readLS<ProjectionId>(G_PROJECTION, DEFAULTS.projection),
    fov: readLS<number>(G_FOV, DEFAULTS.fov),
    savedView: readLS<SavedView | null>(VIEW_KEY(opts.assetId), null),
    upAxis: readLS<UpAxisId>(G_UP_AXIS, DEFAULTS.upAxis),
    autoRotate: readLS<boolean>(G_AUTO_ROTATE, DEFAULTS.autoRotate),
    autoRotateSpeed: readLS<number>(G_AUTO_SPEED, DEFAULTS.autoRotateSpeed),

    // Environment
    envPreset: readLS<EnvPresetId>(G_ENV, DEFAULTS.envPreset),
    envIntensity: readLS<number>(G_ENV_INT, DEFAULTS.envIntensity),
    backgroundVisible: readLS<boolean>(G_BG_VIS, DEFAULTS.backgroundVisible),
    toneMapping: readLS<ToneMappingId>(G_TONE, DEFAULTS.toneMapping),
    exposure: readLS<number>(G_EXPOSURE, DEFAULTS.exposure),

    // Lighting
    lightingPreset: readLS<LightingPresetId>(G_LIGHT_PRESET, DEFAULTS.lightingPreset),
    keyEnabled: readLS<boolean>(G_KEY_ENABLED, DEFAULTS.keyEnabled),
    keyIntensity: readLS<number>(G_KEY_INTENS, DEFAULTS.keyIntensity),
    keyAzimuth: readLS<number>(G_KEY_AZ, DEFAULTS.keyAzimuth),
    keyElevation: readLS<number>(G_KEY_EL, DEFAULTS.keyElevation),
    keyColor: readLS<string>(G_KEY_COLOR, DEFAULTS.keyColor),
    fillEnabled: readLS<boolean>(G_FILL_EN, DEFAULTS.fillEnabled),
    fillIntensity: readLS<number>(G_FILL_INT, DEFAULTS.fillIntensity),
    rimEnabled: readLS<boolean>(G_RIM_EN, DEFAULTS.rimEnabled),
    rimIntensity: readLS<number>(G_RIM_INT, DEFAULTS.rimIntensity),
    shadows: readLS<boolean>(G_SHADOWS, DEFAULTS.shadows),
    shadowSoftness: readLS<number>(G_SHADOW_SOFT, DEFAULTS.shadowSoftness),
    groundPlane: readLS<boolean>(G_GROUND, DEFAULTS.groundPlane),
    contactShadow: readLS<boolean>(G_CONTACT, DEFAULTS.contactShadow),

    // Display
    renderMode: readLS<RenderModeId>(G_RENDER_MODE, DEFAULTS.renderMode),
    showGrid: readLS<boolean>(G_GRID, DEFAULTS.showGrid),
    showAxes: readLS<boolean>(G_AXES, DEFAULTS.showAxes),
    showBoundingBox: readLS<boolean>(G_BBOX, DEFAULTS.showBoundingBox),

    // Materials / Animation / Stats — viewer fills these in.
    materials: [],
    materialOverrides: {},
    clips: [],
    currentClip: -1,
    animationPlaying: false,
    animationSpeed: 1.0,
    animationLoop: true,
    animationTime: 0,
    stats: null,
    loading: true,
    loadError: null,
    backend: null,
  });

  // Sentinel actions the panel emits to ask the viewer for an
  // imperative move (Frame all / Reset camera / Save view). Implemented
  // as a monotonically-incrementing counter the viewer watches via
  // $effect — same idiom the EbookSession's reload trigger uses but
  // shorter to read at the call site.
  const triggers = $state({ frameAll: 0, resetCamera: 0 });

  // ─── Camera helpers ──────────────────────────────────────────
  function setCameraPreset(p: CameraPresetId) { state.cameraPreset = p; }
  function setProjection(p: ProjectionId) {
    state.projection = p;
    writeLS(G_PROJECTION, p);
  }
  function setFov(deg: number) {
    const clamped = Math.max(15, Math.min(90, Math.round(deg)));
    state.fov = clamped;
    writeLS(G_FOV, clamped);
  }
  function setUpAxis(a: UpAxisId) {
    state.upAxis = a;
    writeLS(G_UP_AXIS, a);
  }
  function saveCurrentView(view: SavedView) {
    state.savedView = view;
    writeLS(VIEW_KEY(opts.assetId), view);
  }
  function clearSavedView() {
    state.savedView = null;
    writeLS(VIEW_KEY(opts.assetId), null);
  }
  function frameAll() { triggers.frameAll++; }
  function resetCamera() { triggers.resetCamera++; }
  function toggleAutoRotate() {
    state.autoRotate = !state.autoRotate;
    writeLS(G_AUTO_ROTATE, state.autoRotate);
  }
  function setAutoRotateSpeed(v: number) {
    const clamped = Math.max(0.1, Math.min(8.0, v));
    state.autoRotateSpeed = clamped;
    writeLS(G_AUTO_SPEED, clamped);
  }

  // ─── Environment ─────────────────────────────────────────────
  function setEnvPreset(p: EnvPresetId) {
    state.envPreset = p;
    writeLS(G_ENV, p);
  }
  function setEnvIntensity(v: number) {
    const clamped = Math.max(0, Math.min(3, v));
    state.envIntensity = clamped;
    writeLS(G_ENV_INT, clamped);
  }
  function toggleBackgroundVisible() {
    state.backgroundVisible = !state.backgroundVisible;
    writeLS(G_BG_VIS, state.backgroundVisible);
  }
  function setToneMapping(t: ToneMappingId) {
    state.toneMapping = t;
    writeLS(G_TONE, t);
  }
  function setExposure(v: number) {
    const clamped = Math.max(0.1, Math.min(3, v));
    state.exposure = clamped;
    writeLS(G_EXPOSURE, clamped);
  }

  // ─── Lighting ────────────────────────────────────────────────
  function setLightingPreset(p: LightingPresetId) {
    state.lightingPreset = p;
    writeLS(G_LIGHT_PRESET, p);
    // Each preset stamps a set of toggles / intensities so the
    // user doesn't have to flip 6 controls one-by-one. Switching
    // back to 'custom' preserves their tweaks.
    if (p === 'three-point') {
      // The canonical three-point rig — the same look as the fresh
      // default, which IS 'three-point' (#509). Sourced from the shared
      // default-lighting constants so selecting the preset reproduces
      // the thumbnail-matching default and can't drift from it.
      state.keyEnabled = true;  state.keyIntensity = DEFAULT_KEY_INTENSITY;
      state.fillEnabled = true; state.fillIntensity = DEFAULT_FILL_INTENSITY;
      state.rimEnabled = true;  state.rimIntensity = DEFAULT_RIM_INTENSITY;
      state.shadows = true;
    } else if (p === 'studio') {
      state.keyEnabled = true;  state.keyIntensity = 1.4;
      state.fillEnabled = true; state.fillIntensity = 0.7;
      state.rimEnabled = false; state.rimIntensity = 0;
      state.shadows = true;
    } else if (p === 'outdoor') {
      state.keyEnabled = true;  state.keyIntensity = 2.2;
      state.fillEnabled = false; state.fillIntensity = 0;
      state.rimEnabled = false;  state.rimIntensity = 0;
      state.shadows = true;
    } else if (p === 'showroom') {
      state.keyEnabled = true;  state.keyIntensity = 1.0;
      state.fillEnabled = true; state.fillIntensity = 0.9;
      state.rimEnabled = true;  state.rimIntensity = 1.2;
      state.shadows = false;
    }
  }
  function setKeyEnabled(v: boolean) { state.keyEnabled = v; writeLS(G_KEY_ENABLED, v); }
  function setKeyIntensity(v: number) { const c = Math.max(0, Math.min(8, v)); state.keyIntensity = c; writeLS(G_KEY_INTENS, c); }
  function setKeyAzimuth(d: number) { const c = ((d % 360) + 360) % 360; state.keyAzimuth = c; writeLS(G_KEY_AZ, c); }
  function setKeyElevation(d: number) { const c = Math.max(-90, Math.min(90, d)); state.keyElevation = c; writeLS(G_KEY_EL, c); }
  function setKeyColor(hex: string) { state.keyColor = hex; writeLS(G_KEY_COLOR, hex); }
  function setFillEnabled(v: boolean) { state.fillEnabled = v; writeLS(G_FILL_EN, v); }
  function setFillIntensity(v: number) { const c = Math.max(0, Math.min(4, v)); state.fillIntensity = c; writeLS(G_FILL_INT, c); }
  function setRimEnabled(v: boolean) { state.rimEnabled = v; writeLS(G_RIM_EN, v); }
  function setRimIntensity(v: number) { const c = Math.max(0, Math.min(4, v)); state.rimIntensity = c; writeLS(G_RIM_INT, c); }
  function toggleShadows() { state.shadows = !state.shadows; writeLS(G_SHADOWS, state.shadows); }
  function setShadowSoftness(v: number) { const c = Math.max(0, Math.min(1, v)); state.shadowSoftness = c; writeLS(G_SHADOW_SOFT, c); }
  function toggleGroundPlane() { state.groundPlane = !state.groundPlane; writeLS(G_GROUND, state.groundPlane); }
  function toggleContactShadow() { state.contactShadow = !state.contactShadow; writeLS(G_CONTACT, state.contactShadow); }

  // ─── Display ─────────────────────────────────────────────────
  function setRenderMode(m: RenderModeId) {
    state.renderMode = m;
    writeLS(G_RENDER_MODE, m);
  }
  function toggleGrid() { state.showGrid = !state.showGrid; writeLS(G_GRID, state.showGrid); }
  function toggleAxes() { state.showAxes = !state.showAxes; writeLS(G_AXES, state.showAxes); }
  function toggleBoundingBox() { state.showBoundingBox = !state.showBoundingBox; writeLS(G_BBOX, state.showBoundingBox); }

  // ─── Materials ───────────────────────────────────────────────
  function setMaterialOverride(id: string, patch: MaterialOverride) {
    state.materialOverrides = {
      ...state.materialOverrides,
      [id]: { ...state.materialOverrides[id], ...patch },
    };
  }
  function resetMaterialOverride(id: string) {
    const next = { ...state.materialOverrides };
    delete next[id];
    state.materialOverrides = next;
  }
  function resetAllMaterials() {
    state.materialOverrides = {};
  }

  // ─── Animation ───────────────────────────────────────────────
  function selectClip(idx: number) {
    state.currentClip = idx;
    state.animationTime = 0;
    state.animationPlaying = idx >= 0; // auto-start on selection
  }
  function toggleAnimationPlaying() { state.animationPlaying = !state.animationPlaying; }
  function setAnimationSpeed(v: number) { state.animationSpeed = Math.max(0.1, Math.min(4, v)); }
  function toggleAnimationLoop() { state.animationLoop = !state.animationLoop; }
  function scrubAnimation(seconds: number) {
    state.animationTime = Math.max(0, seconds);
  }

  function resetForReload() {
    state.materials = [];
    state.materialOverrides = {};
    state.clips = [];
    state.currentClip = -1;
    state.animationPlaying = false;
    state.animationTime = 0;
    state.stats = null;
    state.loading = true;
    state.loadError = null;
    state.cameraPreset = 'iso';
    state.backend = null;
  }

  function resetAll() {
    // Re-stamp every persisted field from the canonical defaults.
    // Setters write through to localStorage so a fresh page load
    // sees the factory state, not the user's old picks.
    setProjection(DEFAULTS.projection);
    setFov(DEFAULTS.fov);
    setUpAxis(DEFAULTS.upAxis);
    if (state.autoRotate !== DEFAULTS.autoRotate) toggleAutoRotate();
    setAutoRotateSpeed(DEFAULTS.autoRotateSpeed);

    setEnvPreset(DEFAULTS.envPreset);
    setEnvIntensity(DEFAULTS.envIntensity);
    if (state.backgroundVisible !== DEFAULTS.backgroundVisible) toggleBackgroundVisible();
    setToneMapping(DEFAULTS.toneMapping);
    setExposure(DEFAULTS.exposure);

    // setLightingPreset stamps key/fill/rim/shadows from the preset
    // recipe — call it FIRST, then explicitly write the individual
    // knobs the defaults want (so users who tweaked elevation /
    // azimuth / colors get them snapped back too).
    setLightingPreset(DEFAULTS.lightingPreset);
    setKeyEnabled(DEFAULTS.keyEnabled);
    setKeyIntensity(DEFAULTS.keyIntensity);
    setKeyAzimuth(DEFAULTS.keyAzimuth);
    setKeyElevation(DEFAULTS.keyElevation);
    setKeyColor(DEFAULTS.keyColor);
    setFillEnabled(DEFAULTS.fillEnabled);
    setFillIntensity(DEFAULTS.fillIntensity);
    setRimEnabled(DEFAULTS.rimEnabled);
    setRimIntensity(DEFAULTS.rimIntensity);
    if (state.shadows !== DEFAULTS.shadows) toggleShadows();
    setShadowSoftness(DEFAULTS.shadowSoftness);
    if (state.groundPlane !== DEFAULTS.groundPlane) toggleGroundPlane();
    if (state.contactShadow !== DEFAULTS.contactShadow) toggleContactShadow();

    setRenderMode(DEFAULTS.renderMode);
    if (state.showGrid !== DEFAULTS.showGrid) toggleGrid();
    if (state.showAxes !== DEFAULTS.showAxes) toggleAxes();
    if (state.showBoundingBox !== DEFAULTS.showBoundingBox) toggleBoundingBox();

    resetAllMaterials();
    clearSavedView();
    // Camera position — let the viewer respond to the trigger.
    state.cameraPreset = 'iso';
    resetCamera();
  }

  return Object.assign(state as ModelSessionInstance, {
    assetId: opts.assetId,
    // Trigger counters exposed on the instance so ModelView's
    // $effect can read them as reactive dependencies.
    get _frameAllTrigger() { return triggers.frameAll; },
    get _resetCameraTrigger() { return triggers.resetCamera; },
    setCameraPreset, setProjection, setFov, setUpAxis,
    saveCurrentView, clearSavedView, frameAll, resetCamera,
    toggleAutoRotate, setAutoRotateSpeed,
    setEnvPreset, setEnvIntensity, toggleBackgroundVisible,
    setToneMapping, setExposure,
    setLightingPreset, setKeyEnabled, setKeyIntensity,
    setKeyAzimuth, setKeyElevation, setKeyColor,
    setFillEnabled, setFillIntensity, setRimEnabled, setRimIntensity,
    toggleShadows, setShadowSoftness, toggleGroundPlane, toggleContactShadow,
    setRenderMode, toggleGrid, toggleAxes, toggleBoundingBox,
    setMaterialOverride, resetMaterialOverride, resetAllMaterials,
    selectClip, toggleAnimationPlaying, setAnimationSpeed,
    toggleAnimationLoop, scrubAnimation,
    resetForReload, resetAll,
  });
}
