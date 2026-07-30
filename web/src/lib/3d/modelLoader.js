// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The ONE model load-and-normalise path (#689, ADR 0069's WYSIWYG intent).
//
// Two surfaces render 3D models and ADR 0069 promises they agree:
//   * the interactive viewer  — web/src/lib/components/viewers/ModelView.svelte
//   * the browse-grid thumbnail — scripts/threejs/render.html, driven headless
//     by scripts/threejs/worker.mjs
//
// They used to be two hand-written loaders, and they drifted: the
// thumbnail renderer had no MTLLoader and no material-upgrade pass, so
// every OBJ rendered as untextured white and every KHR_materials_unlit
// glTF rendered as a flat unlit silhouette, while the viewer showed both
// correctly. That is #689. This module is the fix: both surfaces import
// it, so "the thumbnail loads the same code the viewer does" is now
// true instead of aspirational.
//
// It is PLAIN JS on purpose. render.html is a static page served to a
// headless Chromium with an importmap and no build step, so it cannot
// import TypeScript. Vite/SvelteKit typechecks this file via
// tsconfig's checkJs, so the JSDoc types below are enforced.
//
// How each surface reaches it:
//   * viewer  — `await import('$lib/3d/modelLoader.js')` (dynamic, so
//     three stays out of the main bundle for non-3D sessions).
//   * worker  — worker.mjs serves this directory at /shared/, and the
//     Dockerfiles copy the file to /app/threejs/shared/. render.html
//     imports '/shared/modelLoader.js'.
//
// Bare `three` + `three/examples/jsm/*` specifiers resolve through the
// npm package's exports map under Vite and through render.html's
// importmap in the browser, so the same import lines work in both.

import * as THREE from 'three';

/**
 * A companion (sidecar) file the model may reference: the declared
 * relative path as the author uploaded it, plus the URL to fetch it from.
 *
 * @typedef {{ path: string, url: string }} Companion
 */

/**
 * @typedef {object} LoadModelOptions
 * @property {string} url                  URL of the model file itself.
 * @property {string} ext                  File extension, with or without a leading dot.
 * @property {Companion[]} [companions]    Sidecars (MTL, textures, .bin) for the model.
 * @property {any} [manager]               THREE.LoadingManager. The viewer passes one whose
 *                                         setURLModifier maps relative references onto the
 *                                         companions API; the worker relies on plain relative
 *                                         resolution against the staged work dir.
 */

/**
 * @typedef {object} LoadedModel
 * @property {any} object       Object3D ready to add to a scene, materials normalised.
 * @property {any[]} animations AnimationClips the file carried (glTF / FBX), else [].
 */

/** Extensions this module can load. Mirrored by threeJSExts in
 *  app/internal/preview/model.go and covered by scripts/threejs/smoke.mjs.
 *  @type {string[]} */
export const LOADABLE_EXTS = ['glb', 'gltf', 'fbx', 'obj', 'stl', 'ply', 'dae'];

/** @param {string} ext @returns {string} */
export function normaliseExt(ext) {
  return (ext || '').toLowerCase().replace(/^\./, '');
}

/**
 * Geometry-only formats (STL / PLY) hand back a BufferGeometry rather
 * than a scene graph, so wrap it in a mesh with a neutral PBR material.
 * Vertex colours are honoured when the file carries them (common in
 * scanned PLY); normals are computed when it doesn't.
 *
 * @param {any} geometry
 * @returns {any}
 */
export function meshFromGeometry(geometry) {
  if (!geometry.attributes.normal) geometry.computeVertexNormals();
  const material = new THREE.MeshStandardMaterial({
    color: 0xcccccc,
    roughness: 0.75,
    metalness: 0.0,
    vertexColors: !!geometry.attributes.color,
    side: THREE.DoubleSide,
  });
  return new THREE.Mesh(geometry, material);
}

/**
 * Normalise one material to MeshStandardMaterial.
 *
 * FBX/OBJ produce Phong/Basic materials and KHR_materials_unlit glTFs
 * produce MeshBasicMaterial; none of those respond to image-based
 * lighting, so a scene lit only by IBL + directionals renders them as
 * flat silhouettes. Materials that are already Standard/Physical (the
 * normal glTF case) are returned untouched.
 *
 * @param {any} m
 * @returns {any}
 */
export function upgradeMaterial(m) {
  if (!m) {
    return new THREE.MeshStandardMaterial({ color: 0x9a9a9a, roughness: 0.55, metalness: 0 });
  }
  const isStandard = m.type === 'MeshStandardMaterial' || m.type === 'MeshPhysicalMaterial';
  if (isStandard) return m;
  const color = m.color?.isColor ? m.color.clone() : new THREE.Color(0x9a9a9a);
  if (color.r === 0 && color.g === 0 && color.b === 0) {
    color.setHex(m.map ? 0xffffff : 0x9a9a9a);
  }
  const hasMetalness = typeof m.metalness === 'number';
  const hasRoughness = typeof m.roughness === 'number';
  const phongShininess = typeof m.shininess === 'number' ? m.shininess : null;
  const derivedRoughness = phongShininess != null
    ? Math.max(0.05, 1 - Math.sqrt(phongShininess / 100))
    : 0.55;
  return new THREE.MeshStandardMaterial({
    color,
    map: m.map ?? null,
    normalMap: m.normalMap ?? null,
    normalScale: m.normalScale?.clone?.() ?? new THREE.Vector2(1, 1),
    aoMap: m.aoMap ?? null,
    metalnessMap: m.metalnessMap ?? null,
    roughnessMap: m.roughnessMap ?? null,
    emissive: m.emissive?.isColor ? m.emissive.clone() : new THREE.Color(0x000000),
    emissiveMap: m.emissiveMap ?? null,
    roughness: hasRoughness ? m.roughness : derivedRoughness,
    metalness: hasMetalness ? m.metalness : (m.metalnessMap ? 1 : 0),
    transparent: m.transparent ?? false,
    opacity: m.opacity ?? 1,
    side: m.side ?? THREE.FrontSide,
  });
}

/**
 * Walk a loaded tree once and normalise every material, honouring
 * multi-material meshes. Also opts every mesh into shadows — a no-op
 * where the renderer has no shadow map (the thumbnail), meaningful in
 * the viewer.
 *
 * Upgrades are memoised per source material, so meshes that shared a
 * material still share one afterwards. Without that, an OBJ with 4
 * materials over 8 groups ended up with 8 distinct materials — 8 texture
 * uploads and 8 rows in the viewer's Materials panel for 4 real ones.
 *
 * @param {any} root
 * @returns {any} root
 */
export function upgradeMaterials(root) {
  /** @type {Map<string, any>} */
  const done = new Map();
  /** @param {any} m @returns {any} */
  const once = (m) => {
    if (!m?.uuid) return upgradeMaterial(m);
    const hit = done.get(m.uuid);
    if (hit) return hit;
    const up = upgradeMaterial(m);
    done.set(m.uuid, up);
    return up;
  };
  root.traverse((/** @type {any} */ obj) => {
    if (!obj.isMesh) return;
    if (Array.isArray(obj.material)) {
      obj.material = obj.material.map(once);
    } else {
      obj.material = once(obj.material);
    }
    obj.castShadow = true;
    obj.receiveShadow = true;
  });
  return root;
}

/**
 * Wrap a LoadingManager so callers can await "every fetch this manager
 * knows about has finished".
 *
 * Needed because MTLLoader's textures load ASYNCHRONOUSLY: preload()
 * kicks them off and OBJLoader resolves long before any image has
 * decoded. The interactive viewer never noticed — it renders in an
 * animation loop, so textures simply appeared a frame later — but the
 * thumbnail renderer captures its PNGs once and immediately, so it wrote
 * untextured frames even after it grew an MTLLoader (#689).
 *
 * @param {any} manager THREE.LoadingManager
 * @returns {(timeoutMs?: number) => Promise<void>}
 */
function trackPending(manager) {
  let pending = 0;
  /** @type {Array<() => void>} */
  let waiters = [];
  const start = manager.itemStart.bind(manager);
  const end = manager.itemEnd.bind(manager);
  manager.itemStart = (/** @type {string} */ url) => { pending++; start(url); };
  manager.itemEnd = (/** @type {string} */ url) => {
    end(url);
    pending--;
    if (pending <= 0) { const w = waiters; waiters = []; for (const f of w) f(); }
  };
  return (timeoutMs = 30000) => {
    if (pending <= 0) return Promise.resolve();
    return new Promise((resolve) => {
      const t = setTimeout(() => {
        console.warn(`[modelLoader] ${pending} resource(s) still loading after ${timeoutMs}ms`);
        resolve();
      }, timeoutMs);
      waiters.push(() => { clearTimeout(t); resolve(); });
    });
  };
}

/** @param {Companion[]|undefined} companions @returns {Companion|null} */
function findMtl(companions) {
  for (const c of companions ?? []) {
    if (c.path.toLowerCase().endsWith('.mtl')) return c;
  }
  return null;
}

/**
 * Load a model file into a normalised Object3D.
 *
 * glb/gltf/fbx/obj stay statically importable per-loader; stl/ply/dae
 * import lazily so a loader that fails to resolve costs only its own
 * format instead of the whole page.
 *
 * @param {LoadModelOptions} opts
 * @returns {Promise<LoadedModel>}
 */
export async function loadModel(opts) {
  const ext = normaliseExt(opts.ext);
  const url = opts.url;
  const manager = opts.manager ?? new THREE.LoadingManager();
  const settled = trackPending(manager);

  /** @param {any} loader @param {string} target @returns {Promise<any>} */
  const run = (loader, target) => new Promise((resolve, reject) => {
    loader.load(target, resolve, undefined, reject);
  });

  const loaded = await dispatch();
  // Every fetch the loaders started has to finish before the caller
  // renders: MTL textures in particular resolve after the OBJ does.
  await settled();
  return loaded;

  /** @returns {Promise<LoadedModel>} */
  async function dispatch() {
    if (ext === 'glb' || ext === 'gltf') {
      const { GLTFLoader } = await import('three/examples/jsm/loaders/GLTFLoader.js');
      const gltf = await run(new GLTFLoader(manager), url);
      return { object: upgradeMaterials(gltf.scene), animations: gltf.animations || [] };
    }

    if (ext === 'fbx') {
      const { FBXLoader } = await import('three/examples/jsm/loaders/FBXLoader.js');
      const object = await run(new FBXLoader(manager), url);
      return { object: upgradeMaterials(object), animations: object.animations || [] };
    }

    if (ext === 'obj') {
      const { OBJLoader } = await import('three/examples/jsm/loaders/OBJLoader.js');
      const objLoader = new OBJLoader(manager);
      // OBJ carries no materials of its own — the geometry references an
      // .mtl by name (`mtllib`) which OBJLoader does NOT fetch. Load it
      // ourselves and hand the parsed materials over, or every OBJ renders
      // as untextured white (that half of #689). MTLLoader resolves its
      // own map_Kd paths relative to the .mtl URL, through `manager`.
      const mtl = findMtl(opts.companions);
      if (mtl) {
        try {
          const { MTLLoader } = await import('three/examples/jsm/loaders/MTLLoader.js');
          const materials = await run(new MTLLoader(manager), mtl.url);
          materials.preload();
          objLoader.setMaterials(materials);
        } catch {
          /* OBJ still loads; the upgrade pass gives it PBR grey. */
        }
      }
      return { object: upgradeMaterials(await run(objLoader, url)), animations: [] };
    }

    if (ext === 'stl') {
      const { STLLoader } = await import('three/examples/jsm/loaders/STLLoader.js');
      const geometry = await run(new STLLoader(manager), url);
      return { object: meshFromGeometry(geometry), animations: [] };
    }

    if (ext === 'ply') {
      const { PLYLoader } = await import('three/examples/jsm/loaders/PLYLoader.js');
      const geometry = await run(new PLYLoader(manager), url);
      return { object: meshFromGeometry(geometry), animations: [] };
    }

    if (ext === 'dae') {
      const { ColladaLoader } = await import('three/examples/jsm/loaders/ColladaLoader.js');
      const collada = await run(new ColladaLoader(manager), url);
      return { object: upgradeMaterials(collada.scene), animations: collada.animations || [] };
    }

    throw new Error(`unsupported ext: ${ext}`);
  }
}

/**
 * Build a LoadingManager that rewrites relative resource URLs onto a
 * companion list. Used by BOTH surfaces:
 *
 *   * the viewer, where the model is served from /api/v1/assets/<id>/file
 *     and its sidecars from /api/v1/assets/<id>/companions/<companion-id>
 *     — nothing a loader can derive by relative resolution;
 *   * the thumbnail worker, whose companions ARE staged on disk under
 *     their declared relative paths, but where relative resolution alone
 *     still misses a format: three.js's FBXLoader reduces
 *     `Textures\barrel.png` to its basename before requesting it, so it
 *     asks for `barrel.png` next to the model and 404s (#753).
 *
 * Matching is longest-suffix first, then basename, both case-insensitive
 * (Windows-authored MTLs routinely disagree with the archive on case).
 * Backslashes are normalised on both sides. Companion paths are stored
 * POSIX-relative as of #753, so the entry side should never carry one —
 * this is belt-and-braces for a row written before that, and for the URL
 * side it costs one regex to stop a `Textures\x.png` request computing
 * the whole string as its basename. Measured, FBXLoader asks for the
 * bare basename and DAE/OBJ loaders ask for the full relative path;
 * neither passes a separator through today.
 *
 * @param {Companion[]} companions
 * @returns {any} THREE.LoadingManager
 */
export function companionLoadingManager(companions) {
  const manager = new THREE.LoadingManager();
  if (companions.length === 0) return manager;
  const entries = companions.map((c) => ({
    path: c.path.toLowerCase().replace(/\\/g, '/'),
    url: c.url,
  }));
  manager.setURLModifier((/** @type {string} */ url) => {
    const lower = url.toLowerCase().replace(/\\/g, '/');
    for (const e of entries) {
      if (lower.endsWith('/' + e.path) || lower === e.path) return e.url;
    }
    const lastSlash = lower.lastIndexOf('/');
    const basename = lastSlash >= 0 ? lower.slice(lastSlash + 1) : lower;
    for (const e of entries) {
      if (e.path.slice(e.path.lastIndexOf('/') + 1) === basename) return e.url;
    }
    return url;
  });
  return manager;
}
