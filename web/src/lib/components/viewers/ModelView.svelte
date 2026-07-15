<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // 3D model body for the AssetViewer.
  //
  // One unified three.js path for every supported 3D format:
  //   * glb / gltf  → GLTFLoader (PBR-native; animations on result.animations)
  //   * fbx         → FBXLoader  + material upgrade pass
  //   * obj         → OBJLoader  + material upgrade pass (.mtl in 1.18.B-12c)
  //   * mview       → Marmoset Toolbag self-contained player (1.18.B-11)
  //
  // Everything user-controllable (env / lighting / display / camera /
  // materials / animation) flows through a shared ModelSession that
  // the ModelTool side-panel binds to as well — both ends mutate the
  // same $state object, no event bus. The viewer reacts to session
  // mutations via $effects below.
  //
  // Everything is dynamically imported so the 3D libs don't bloat
  // the main bundle for non-3D users.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';
  import type { ModelSessionInstance } from '$lib/3d/session.svelte';
  import {
    buildEnvironment,
    buildDefaultMatcap,
    toneMappingValue,
    type EnvPresetId,
  } from '$lib/3d/environments';

  type Asset = import('./controller').ViewAsset;

  interface Props {
    asset: Asset;
    controller: ViewController;
    /** Shared reactive state with the ModelTool side panel. The
     *  AssetViewer builds one per asset + binds both sides. */
    session: ModelSessionInstance;
    /** When false, camera interaction is disabled so the parent (e.g.
        the PostModal scroll-snap) can take wheel + drag. Auto-rotate
        on glb/gltf still runs — that's animation, not input. */
    reviewMode?: boolean;
  }

  let {
    asset,
    controller = $bindable(),
    session = $bindable<ModelSessionInstance>(),
    reviewMode = true,
  }: Props = $props();

  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);
  const ext = $derived((asset.file_extension || '').toLowerCase().replace(/^\./, ''));

  let container: HTMLDivElement | undefined = $state();
  // Cleanup state (so onDestroy can dispose three.js resources).
  let cleanupFn: (() => void) | null = null;
  // Ref the reviewMode reactive effect needs to find again after mount.
  let threeControls: { enabled: boolean } | null = null;
  // ModelView host bag the $effects below talk to. Populated in
  // mountThree() — wrapped in $state so the moment it transitions
  // from null → bag every dependent $effect re-runs and applies the
  // user's persisted session state. Without this, the effects bailed
  // on first mount (host still null while mountThree's async work
  // ran) and never re-fired, leaving stored toggles inert.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let host = $state<any>(null);

  onMount(() => {
    controller.kind = '3d';
    controller.hasTimeline = false;
    controller.totalFrames = 0;
    controller.fps = 0;
    controller.duration = 0;
    controller.playing = false;
    controller.spritesUrl = null;
    controller.spritesVttUrl = null;
    controller.formatAnchor = () => '';
    controller.hudExtra = ext.toUpperCase();
    controller.play = () => {};
    controller.pause = () => {};
    controller.togglePlay = () => {};
    controller.seekToFrame = () => {};
    controller.stepFrames = () => {};
    controller.setRate = () => {};
    controller.tools = null;
    session.resetForReload();
    void mount();
  });

  onDestroy(() => {
    if (cleanupFn) cleanupFn();
  });

  async function mount() {
    if (!container) return;
    session.loadError = null;
    session.loading = true;
    try {
      if (ext === 'glb' || ext === 'gltf' || ext === 'fbx' || ext === 'obj') {
        await mountThree(ext);
      } else if (ext === 'mview') {
        await mountMarmoset();
      } else {
        session.loadError = `${ext} viewer not yet implemented`;
      }
    } catch (e) {
      session.loadError = e instanceof Error ? e.message : 'load failed';
    } finally {
      session.loading = false;
    }
  }

  // -----------------------------------------------------------------------
  // .mview via Marmoset's WebViewer. Closed source — we script-tag it on
  // demand so the main bundle doesn't ship a network dep for users who
  // never open one. Limited API: only frameAll is meaningfully wired.
  // -----------------------------------------------------------------------

  async function mountMarmoset() {
    await ensureMarmosetScript();
    if (!container) return;
    const w = container.clientWidth || 800;
    const h = container.clientHeight || 600;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const mv: any = (window as unknown as { marmoset?: unknown }).marmoset;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    if (!mv || typeof (mv as any).WebViewer !== 'function') {
      session.loadError = 'marmoset.js failed to expose WebViewer';
      return;
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const viewer = new (mv as any).WebViewer(w, h, fileUrl);
    container.appendChild(viewer.domRoot);
    try { viewer.loadScene?.(); } catch { /* ignore */ }
    controller.hudExtra = 'MVIEW';

    // Wire the only session command Marmoset gives us a path for.
    host = {
      kind: 'marmoset',
      frameAll: () => { try { viewer.resetCamera?.(); } catch { /* ignore */ } },
    };
    session.backend = 'marmoset';

    const ro = new ResizeObserver(() => {
      if (!container) return;
      const W = container.clientWidth || w;
      const H = container.clientHeight || h;
      try { viewer.resize?.(W, H); } catch { /* ignore */ }
    });
    ro.observe(container);

    cleanupFn = () => {
      ro.disconnect();
      try { viewer.unload?.(); } catch { /* ignore */ }
      try { container?.removeChild(viewer.domRoot); } catch { /* ignore */ }
      host = null;
    };
  }

  let marmosetScriptPromise: Promise<void> | null = null;
  function ensureMarmosetScript(): Promise<void> {
    if ((window as unknown as { marmoset?: unknown }).marmoset) return Promise.resolve();
    if (marmosetScriptPromise) return marmosetScriptPromise;
    marmosetScriptPromise = new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = 'https://viewer.marmoset.co/main/marmoset.js';
      s.crossOrigin = 'anonymous';
      s.onload = () => resolve();
      s.onerror = () => reject(new Error('marmoset.js failed to load'));
      document.head.appendChild(s);
    });
    return marmosetScriptPromise;
  }

  // -----------------------------------------------------------------------
  // Unified three.js path. Dynamic imports keep the three bundle out of
  // the main chunk for sessions that never open a 3D asset.
  // -----------------------------------------------------------------------

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  async function mountThree(kind: 'glb' | 'gltf' | 'fbx' | 'obj') {
    const THREE = await import('three');

    // ─── Companion lookup ────────────────────────────────────────────
    // Fetch the asset's sidecar files BEFORE loading. The viewer's
    // LoadingManager rewrites every relative resource URL the loader
    // asks for through these entries — textures, MTL, etc.
    const companions = new Map<string, string>();
    try {
      const r = await fetch(`/api/v1/assets/${asset.id}/companions`, { credentials: 'include' });
      if (r.ok) {
        const list = (await r.json()) as Array<{ id: string; path: string }>;
        for (const c of list) companions.set(c.path, `/api/v1/assets/${asset.id}/companions/${c.id}`);
      }
    } catch { /* soft fail — companions are an enhancement */ }

    // ─── File size for stats ────────────────────────────────────────
    // The /file endpoint only allows GET (server-side guard), so a
    // HEAD probe was logging a console 405. We piggyback on the
    // bytes the loader is about to fetch anyway: Range: bytes=0-0
    // returns a 206 with Content-Range "bytes 0-0/<total>". Servers
    // that don't honour Range fall back to a clean null.
    let fileSize: number | null = null;
    try {
      const probe = await fetch(fileUrl, {
        method: 'GET',
        headers: { Range: 'bytes=0-0' },
        credentials: 'include',
      });
      const cr = probe.headers.get('content-range');
      const m = cr?.match(/\/(\d+)$/);
      if (m) fileSize = parseInt(m[1], 10);
      else if (probe.ok) {
        const len = probe.headers.get('content-length');
        if (len) fileSize = parseInt(len, 10);
      }
      // Drain so the connection can be reused.
      void probe.body?.cancel();
    } catch { /* ignore */ }

    const manager = new THREE.LoadingManager();
    if (companions.size > 0) {
      const lowerEntries = Array.from(companions, ([k, v]) => [k.toLowerCase(), v] as const);
      manager.setURLModifier((url) => {
        const lower = url.toLowerCase();
        for (const [path, companionUrl] of lowerEntries) {
          if (lower.endsWith('/' + path) || lower === path) return companionUrl;
        }
        const lastSlash = lower.lastIndexOf('/');
        const basename = lastSlash >= 0 ? lower.slice(lastSlash + 1) : lower;
        for (const [path, companionUrl] of lowerEntries) {
          const cBasename = path.slice(path.lastIndexOf('/') + 1);
          if (cBasename === basename) return companionUrl;
        }
        return url;
      });
    }

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let model: any;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let rawAnimations: any[] = [];
    if (kind === 'glb' || kind === 'gltf') {
      const { GLTFLoader } = await import('three/examples/jsm/loaders/GLTFLoader.js');
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const result: any = await new Promise((res, rej) => {
        new GLTFLoader(manager).load(fileUrl, res, undefined, rej);
      });
      model = result.scene;
      rawAnimations = result.animations || [];
    } else if (kind === 'fbx') {
      const { FBXLoader } = await import('three/examples/jsm/loaders/FBXLoader.js');
      model = await new Promise((res, rej) => {
        new FBXLoader(manager).load(fileUrl, res, undefined, rej);
      });
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      rawAnimations = (model as any).animations || [];
    } else {
      const { OBJLoader } = await import('three/examples/jsm/loaders/OBJLoader.js');
      const objLoader = new OBJLoader(manager);
      let mtlCompanionUrl: string | null = null;
      for (const [path, url] of companions) {
        if (path.toLowerCase().endsWith('.mtl')) { mtlCompanionUrl = url; break; }
      }
      if (mtlCompanionUrl) {
        try {
          const { MTLLoader } = await import('three/examples/jsm/loaders/MTLLoader.js');
          const mtlLoader = new MTLLoader(manager);
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          const materials: any = await new Promise((res, rej) => {
            mtlLoader.load(mtlCompanionUrl!, res, undefined, rej);
          });
          materials.preload();
          objLoader.setMaterials(materials);
        } catch { /* OBJ still loads as untextured; upgrade gives PBR grey */ }
      }
      model = await new Promise((res, rej) => {
        objLoader.load(fileUrl, res, undefined, rej);
      });
    }

    if (!container) return;
    const w = container.clientWidth || 800;
    const h = container.clientHeight || 600;

    const scene = new THREE.Scene();

    // ─── Material normalisation ────────────────────────────────────
    // FBX/OBJ produce Phong/Basic materials; neither responds to IBL.
    // Walk the tree once and upgrade to MeshStandardMaterial.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const upgradeMaterial = (m: any): any => {
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
    };
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    model.traverse((obj: any) => {
      if (!obj.isMesh) return;
      if (Array.isArray(obj.material)) {
        obj.material = obj.material.map(upgradeMaterial);
      } else {
        obj.material = upgradeMaterial(obj.material);
      }
      obj.castShadow = true;
      obj.receiveShadow = true;
    });

    // ─── Material catalogue (for the side-panel Materials section) ─
    // Walk once after the upgrade pass, collect unique material refs,
    // and snapshot their base PBR values so "Reset" can restore them
    // without re-loading. Keyed by uuid so per-mesh-instance overrides
    // stay accurate even if names collide.
    //
    // Naming fallback chain — most glTFs ship empty material.name, so
    // "(unnamed)" everywhere is useless to the user. Try, in order:
    //   1. material.name (when authored)
    //   2. first mesh-using-this-material's name (often the part name
    //      from the DCC, e.g. "Wood_Door_Frame")
    //   3. numbered sequence "Material 1 / 2 / 3 …" so the user at
    //      least has distinct labels to click between
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const materialMap = new Map<string, { mat: any; meshCount: number; firstMeshName: string; entry: import('$lib/3d/session.svelte').MaterialEntry }>();
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    model.traverse((obj: any) => {
      if (!obj.isMesh) return;
      const meshName = (obj.name?.trim() as string) || '';
      const mats = Array.isArray(obj.material) ? obj.material : [obj.material];
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      mats.forEach((m: any) => {
        if (!m) return;
        const ex = materialMap.get(m.uuid);
        if (ex) { ex.meshCount++; return; }
        materialMap.set(m.uuid, {
          mat: m, meshCount: 1, firstMeshName: meshName,
          entry: {
            id: m.uuid,
            name: '', // resolved below once we know the index
            meshCount: 1,
            baseColor: '#' + (m.color?.getHexString?.() ?? '9a9a9a'),
            baseMetalness: typeof m.metalness === 'number' ? m.metalness : 0,
            baseRoughness: typeof m.roughness === 'number' ? m.roughness : 0.55,
          },
        });
      });
    });
    let matIdx = 0;
    for (const v of materialMap.values()) {
      matIdx++;
      v.entry.meshCount = v.meshCount;
      const authored = (v.mat.name?.trim() as string) || '';
      v.entry.name = authored || v.firstMeshName || `Material ${matIdx}`;
    }
    // Disambiguate collisions — when two materials end up with the
    // same label (common when a model has multiple unnamed mats on
    // the same mesh, or several meshes share names), suffix with
    // " (1) / (2) / …" so each row in the panel is uniquely
    // identifiable.
    const nameCounts = new Map<string, number>();
    for (const e of materialMap.values()) {
      nameCounts.set(e.entry.name, (nameCounts.get(e.entry.name) ?? 0) + 1);
    }
    const seen = new Map<string, number>();
    for (const e of materialMap.values()) {
      if ((nameCounts.get(e.entry.name) ?? 0) > 1) {
        const n = (seen.get(e.entry.name) ?? 0) + 1;
        seen.set(e.entry.name, n);
        e.entry.name = `${e.entry.name} (${n})`;
      }
    }
    session.materials = Array.from(materialMap.values(), (v) => v.entry);

    // Frame the model — fit camera to bounding box.
    const box = new THREE.Box3().setFromObject(model);
    const size = box.getSize(new THREE.Vector3());
    const center = box.getCenter(new THREE.Vector3());
    model.position.sub(center);
    scene.add(model);
    const maxDim = Math.max(size.x, size.y, size.z) || 1;
    const minY = -size.y / 2;

    // Initial camera pose — kept as a Frame All target.
    const initialCamPos = new THREE.Vector3();
    const initialTarget = new THREE.Vector3(0, 0, 0);
    const dist = maxDim * 2.2;
    initialCamPos.set(dist, dist * 0.6, dist);

    const perspectiveCam = new THREE.PerspectiveCamera(session.fov, w / h, maxDim / 1000, maxDim * 100);
    perspectiveCam.position.copy(initialCamPos);
    perspectiveCam.lookAt(initialTarget);
    const orthoCam = new THREE.OrthographicCamera(-1, 1, 1, -1, maxDim / 1000, maxDim * 100);
    orthoCam.position.copy(initialCamPos);
    orthoCam.lookAt(initialTarget);
    function syncOrthoFrustum(c: typeof orthoCam, distance: number, aspect: number) {
      // Match perspective-equivalent extent so toggling projection
      // doesn't visually pop. zoom=1 covers ~maxDim*1.3 vertically.
      const halfH = Math.max(0.5, distance / 2.4);
      c.top = halfH; c.bottom = -halfH;
      c.left = -halfH * aspect; c.right = halfH * aspect;
      c.updateProjectionMatrix();
    }
    syncOrthoFrustum(orthoCam, initialCamPos.length(), w / h);
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let camera: any = session.projection === 'orthographic' ? orthoCam : perspectiveCam;

    const renderer = new THREE.WebGLRenderer({ antialias: true });
    renderer.setPixelRatio(window.devicePixelRatio);
    renderer.setSize(w, h);
    renderer.outputColorSpace = THREE.SRGBColorSpace;
    renderer.toneMapping = toneMappingValue(session.toneMapping);
    renderer.toneMappingExposure = session.exposure;
    renderer.shadowMap.enabled = session.shadows;
    // three r185 deprecated PCFSoftShadowMap in the WebGL renderer — it
    // now silently converts it to PCFShadowMap. Set PCFShadowMap
    // directly for the identical result without the console warning.
    renderer.shadowMap.type = THREE.PCFShadowMap;
    container.appendChild(renderer.domElement);

    // ─── PMREMGenerator + initial environment ──────────────────────
    const pmrem = new THREE.PMREMGenerator(renderer);
    pmrem.compileEquirectangularShader();
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let envTexture: any = null;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let envBackground: any = null;
    async function applyEnv(id: EnvPresetId) {
      envTexture?.dispose?.();
      // Don't dispose Color backgrounds — they're tiny POJOs.
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      if ((envBackground as any)?.isTexture) envBackground.dispose?.();
      const { env, background } = await buildEnvironment(id, pmrem);
      envTexture = env;
      envBackground = background;
      scene.environment = env;
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (scene as any).environmentIntensity = session.envIntensity;
      scene.background = session.backgroundVisible ? background : new THREE.Color(0x0b0b0c);
    }
    await applyEnv(session.envPreset);

    // ─── Lighting rig (three-point) ─────────────────────────────
    // Three lights + helpers. Each light's transform is recomputed
    // from session sliders. Intensities can drive to zero so the
    // light "disabled" just zeros it (lights stay in scene to avoid
    // re-graph churn when the user toggles back).
    const keyLight = new THREE.DirectionalLight(0xffffff, session.keyIntensity);
    keyLight.castShadow = session.shadows;
    keyLight.shadow.mapSize.set(2048, 2048);
    keyLight.shadow.camera.near = 0.1;
    keyLight.shadow.camera.far = maxDim * 10;
    keyLight.shadow.camera.left = -maxDim * 2;
    keyLight.shadow.camera.right = maxDim * 2;
    keyLight.shadow.camera.top = maxDim * 2;
    keyLight.shadow.camera.bottom = -maxDim * 2;
    keyLight.shadow.bias = -0.00005;
    keyLight.shadow.radius = 4;
    scene.add(keyLight);
    const fillLight = new THREE.DirectionalLight(0xffffff, session.fillIntensity);
    scene.add(fillLight);
    const rimLight = new THREE.DirectionalLight(0xffffff, session.rimIntensity);
    scene.add(rimLight);

    function applyLightingFromSession() {
      const r = Math.max(maxDim * 3, 1);
      // Key from session.azimuth/elevation.
      const az = (session.keyAzimuth * Math.PI) / 180;
      const el = (session.keyElevation * Math.PI) / 180;
      keyLight.position.set(
        Math.cos(el) * Math.sin(az) * r,
        Math.sin(el) * r,
        Math.cos(el) * Math.cos(az) * r,
      );
      keyLight.intensity = session.keyEnabled ? session.keyIntensity : 0;
      keyLight.color.set(session.keyColor);
      keyLight.castShadow = session.shadows && session.keyEnabled;
      keyLight.shadow.radius = 1 + session.shadowSoftness * 8;
      // Fill opposite the key, slightly elevated.
      fillLight.position.set(-keyLight.position.x, Math.abs(keyLight.position.y) * 0.7, -keyLight.position.z);
      fillLight.intensity = session.fillEnabled ? session.fillIntensity : 0;
      // Rim behind the model relative to camera.
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const camPos = (camera as any).position;
      const back = camPos.clone().normalize().multiplyScalar(-r);
      back.y = Math.abs(back.y) + r * 0.4;
      rimLight.position.copy(back);
      rimLight.intensity = session.rimEnabled ? session.rimIntensity : 0;
    }
    applyLightingFromSession();

    // ─── Ground plane + grid + axes + bbox helpers ─────────────
    const groundGeo = new THREE.PlaneGeometry(maxDim * 6, maxDim * 6);
    const groundMat = new THREE.ShadowMaterial({ opacity: 0.35, transparent: true });
    const ground = new THREE.Mesh(groundGeo, groundMat);
    ground.rotation.x = -Math.PI / 2;
    ground.position.y = minY;
    ground.receiveShadow = true;
    ground.visible = session.contactShadow;
    scene.add(ground);

    // Optional opaque ground for product viz (vs the shadow-only one).
    const groundOpaqueMat = new THREE.MeshStandardMaterial({ color: 0x1c1c20, roughness: 0.9, metalness: 0 });
    const groundOpaque = new THREE.Mesh(groundGeo, groundOpaqueMat);
    groundOpaque.rotation.x = -Math.PI / 2;
    groundOpaque.position.y = minY - 0.001; // sit just below contact-shadow plane
    groundOpaque.receiveShadow = true;
    groundOpaque.visible = session.groundPlane;
    scene.add(groundOpaque);

    const gridSize = maxDim * 4;
    const gridHelper = new THREE.GridHelper(gridSize, 20, 0x707070, 0x303030);
    gridHelper.position.y = minY;
    gridHelper.visible = session.showGrid;
    scene.add(gridHelper);

    const axesHelper = new THREE.AxesHelper(maxDim * 1.2);
    axesHelper.visible = session.showAxes;
    scene.add(axesHelper);

    const bboxHelper = new THREE.Box3Helper(
      new THREE.Box3().setFromObject(model),
      new THREE.Color(0xffaa00),
    );
    bboxHelper.visible = session.showBoundingBox;
    scene.add(bboxHelper);

    // ─── Render-mode plumbing ──────────────────────────────────
    // Store the upgraded standard material for every mesh so the
    // alt-mode paths (Normals / Matcap / X-Ray) can swap and snap
    // back without re-loading the asset.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const meshes: any[] = [];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const baseMaterialOf = new Map<any, any | any[]>();
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    model.traverse((obj: any) => {
      if (obj.isMesh) {
        meshes.push(obj);
        baseMaterialOf.set(obj, Array.isArray(obj.material) ? obj.material.slice() : obj.material);
      }
    });

    const matcapTex = buildDefaultMatcap();

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let overlayGroup: any | null = null;
    function buildOverlay() {
      const group = new THREE.Group();
      meshes.forEach((m) => {
        const edges = new THREE.EdgesGeometry(m.geometry, 30);
        const line = new THREE.LineSegments(
          edges,
          new THREE.LineBasicMaterial({ color: 0xffffff, transparent: true, opacity: 0.4 }),
        );
        m.matrixWorld.decompose(line.position, line.quaternion, line.scale);
        group.add(line);
      });
      return group;
    }
    function disposeOverlay() {
      if (!overlayGroup) return;
      scene.remove(overlayGroup);
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      overlayGroup.traverse((o: any) => {
        o.geometry?.dispose?.();
        o.material?.dispose?.();
      });
      overlayGroup = null;
    }
    function applyRenderMode(mode: import('$lib/3d/session.svelte').RenderModeId) {
      disposeOverlay();
      meshes.forEach((m) => {
        const base = baseMaterialOf.get(m);
        if (!base) return;
        // Restore base first so each pass is idempotent.
        m.material = base;
        const apply = (mat: import('three').Material) => {
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          const any = mat as any;
          if ('wireframe' in any) any.wireframe = false;
          if ('opacity' in any) any.opacity = 1;
          if ('transparent' in any) any.transparent = false;
          if ('depthWrite' in any) any.depthWrite = true;
          if ('blending' in any) any.blending = THREE.NormalBlending;
        };
        if (Array.isArray(m.material)) m.material.forEach(apply); else apply(m.material);
      });
      if (mode === 'solid') return;
      if (mode === 'wireframe') {
        meshes.forEach((m) => {
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          const mats = Array.isArray(m.material) ? m.material : [m.material];
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          mats.forEach((mat: any) => { if (mat && 'wireframe' in mat) mat.wireframe = true; });
        });
        return;
      }
      if (mode === 'overlay') {
        overlayGroup = buildOverlay();
        scene.add(overlayGroup);
        return;
      }
      if (mode === 'normals') {
        meshes.forEach((m) => { m.material = new THREE.MeshNormalMaterial(); });
        return;
      }
      if (mode === 'matcap') {
        meshes.forEach((m) => { m.material = new THREE.MeshMatcapMaterial({ matcap: matcapTex }); });
        return;
      }
      if (mode === 'xray') {
        meshes.forEach((m) => {
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          const mats = Array.isArray(m.material) ? m.material : [m.material];
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          mats.forEach((mat: any) => {
            if (!mat) return;
            mat.transparent = true;
            mat.opacity = 0.35;
            mat.depthWrite = false;
            mat.blending = THREE.AdditiveBlending;
          });
        });
      }
    }
    applyRenderMode(session.renderMode);

    // ─── Animations ────────────────────────────────────────────
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let mixer: any = null;
    if (rawAnimations.length > 0) {
      mixer = new THREE.AnimationMixer(model);
      session.clips = rawAnimations.map((c) => ({ name: c.name || '(unnamed clip)', duration: c.duration }));
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    let currentAction: any = null;
    function selectClipImpl(idx: number) {
      if (currentAction) { currentAction.stop(); currentAction = null; }
      if (!mixer || idx < 0 || idx >= rawAnimations.length) return;
      currentAction = mixer.clipAction(rawAnimations[idx]);
      currentAction.setLoop(session.animationLoop ? THREE.LoopRepeat : THREE.LoopOnce, Infinity);
      currentAction.clampWhenFinished = !session.animationLoop;
      currentAction.timeScale = session.animationSpeed;
      currentAction.reset();
      if (session.animationPlaying) currentAction.play();
    }
    // Honor a session-driven auto-select for assets that come back
    // with clips (the panel's default of -1 stays unless user picks).
    if (session.currentClip >= 0 && session.currentClip < rawAnimations.length) {
      selectClipImpl(session.currentClip);
    } else if (rawAnimations.length > 0) {
      // Convenience: auto-pick clip 0 paused, so the user sees the
      // dropdown is populated even before they hit play.
      session.clips = rawAnimations.map((c) => ({ name: c.name || '(unnamed clip)', duration: c.duration }));
    }

    // ─── Stats ─────────────────────────────────────────────────
    let totalVerts = 0, totalTris = 0;
    const seenTextures = new Set<unknown>();
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    model.traverse((obj: any) => {
      if (!obj.isMesh || !obj.geometry) return;
      const g = obj.geometry;
      const pos = g.attributes?.position;
      if (pos) totalVerts += pos.count;
      if (g.index) totalTris += g.index.count / 3;
      else if (pos) totalTris += pos.count / 3;
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const mats = Array.isArray(obj.material) ? obj.material : [obj.material];
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      mats.forEach((m: any) => {
        if (!m) return;
        ['map', 'normalMap', 'aoMap', 'metalnessMap', 'roughnessMap', 'emissiveMap'].forEach((k) => {
          const t = m[k];
          if (t) seenTextures.add(t);
        });
      });
    });
    session.stats = {
      vertices: Math.round(totalVerts),
      triangles: Math.round(totalTris),
      meshes: meshes.length,
      materials: materialMap.size,
      textures: seenTextures.size,
      drawCalls: 0, // populated each frame from renderer.info
      fileSize,
    };

    // ─── OrbitControls ─────────────────────────────────────────
    const { OrbitControls } = await import('three/examples/jsm/controls/OrbitControls.js');
    let controls = new OrbitControls(camera, renderer.domElement);
    controls.enableDamping = true;
    controls.dampingFactor = 0.1;
    controls.target.copy(initialTarget);
    controls.enabled = reviewMode;
    controls.autoRotate = session.autoRotate;
    controls.autoRotateSpeed = session.autoRotateSpeed;
    controls.update();
    threeControls = controls;

    function rebuildControls() {
      const oldTarget = controls.target.clone();
      const enabled = controls.enabled;
      controls.dispose();
      controls = new OrbitControls(camera, renderer.domElement);
      controls.enableDamping = true;
      controls.dampingFactor = 0.1;
      controls.target.copy(oldTarget);
      controls.enabled = enabled;
      controls.autoRotate = session.autoRotate;
      controls.autoRotateSpeed = session.autoRotateSpeed;
      controls.update();
      threeControls = controls;
    }

    function applyFov() {
      // FOV is a perspective-only knob; updating it doesn't change
      // which camera is active, so we skip the controls rebuild
      // (slider drags would otherwise drop input mid-gesture).
      const aspect = renderer.domElement.clientWidth / renderer.domElement.clientHeight;
      perspectiveCam.aspect = aspect;
      perspectiveCam.fov = session.fov;
      perspectiveCam.updateProjectionMatrix();
      syncOrthoFrustum(orthoCam, perspectiveCam.position.length(), aspect);
    }
    function applyProjection() {
      const targetIsOrtho = session.projection === 'orthographic';
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const target: any = targetIsOrtho ? orthoCam : perspectiveCam;
      if (target === camera) {
        // No actual switch — just refresh in case aspect/fov changed.
        applyFov();
        return;
      }
      applyFov();
      // Mirror the live camera's pose onto the target before swap.
      target.position.copy(camera.position);
      target.quaternion.copy(camera.quaternion);
      camera = target;
      rebuildControls();
    }

    function applyCameraPreset(p: import('$lib/3d/session.svelte').CameraPresetId) {
      const r = dist;
      let pos = new THREE.Vector3(r, r * 0.6, r);
      if (p === 'front')  pos = new THREE.Vector3(0, 0, r);
      if (p === 'back')   pos = new THREE.Vector3(0, 0, -r);
      if (p === 'right')  pos = new THREE.Vector3(r, 0, 0);
      if (p === 'left')   pos = new THREE.Vector3(-r, 0, 0);
      if (p === 'top')    pos = new THREE.Vector3(0, r, 0.001);
      if (p === 'bottom') pos = new THREE.Vector3(0, -r, 0.001);
      camera.position.copy(pos);
      controls.target.copy(initialTarget);
      camera.lookAt(initialTarget);
      controls.update();
    }
    function doFrameAll() {
      camera.position.copy(initialCamPos);
      controls.target.copy(initialTarget);
      camera.lookAt(initialTarget);
      controls.update();
    }
    function doResetCamera() {
      doFrameAll();
      session.cameraPreset = 'iso';
    }
    function restoreSavedView() {
      const sv = session.savedView;
      if (!sv) return;
      camera.position.set(sv.pos[0], sv.pos[1], sv.pos[2]);
      controls.target.set(sv.target[0], sv.target[1], sv.target[2]);
      if (session.projection === 'perspective') {
        perspectiveCam.fov = sv.fov;
        perspectiveCam.updateProjectionMatrix();
      } else {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        (camera as any).zoom = sv.zoom || 1;
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        (camera as any).updateProjectionMatrix?.();
      }
      camera.lookAt(controls.target);
      controls.update();
    }
    function snapshotView(): import('$lib/3d/session.svelte').SavedView {
      return {
        pos: [camera.position.x, camera.position.y, camera.position.z],
        target: [controls.target.x, controls.target.y, controls.target.z],
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        zoom: (camera as any).zoom ?? 1,
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        fov: (camera as any).fov ?? perspectiveCam.fov,
      };
    }

    // ─── Material override applier ─────────────────────────────
    function applyMaterialOverrides() {
      for (const e of materialMap.values()) {
        const ov = session.materialOverrides[e.entry.id];
        const mat = e.mat;
        if (ov?.color) mat.color?.set(ov.color);
        else mat.color?.set(e.entry.baseColor);
        if (typeof ov?.metalness === 'number') mat.metalness = ov.metalness;
        else mat.metalness = e.entry.baseMetalness;
        if (typeof ov?.roughness === 'number') mat.roughness = ov.roughness;
        else mat.roughness = e.entry.baseRoughness;
      }
    }

    // ─── Render loop ───────────────────────────────────────────
    let rafId = 0;
    let lastT = performance.now();
    function tick() {
      const now = performance.now();
      const dt = (now - lastT) / 1000;
      lastT = now;
      controls.update();
      if (mixer && currentAction && session.animationPlaying) {
        mixer.update(dt * session.animationSpeed);
        // Mirror viewer-driven time back to the panel scrubber.
        session.animationTime = currentAction.time;
      }
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const camAny = camera as any;
      if (session.envIntensity !== undefined) {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        (scene as any).environmentIntensity = session.envIntensity;
      }
      // Rim direction needs to follow the camera each frame.
      if (rimLight.intensity > 0 && camAny.position) {
        const back = camAny.position.clone().normalize().multiplyScalar(-maxDim * 3);
        back.y = Math.abs(back.y) + maxDim * 1.2;
        rimLight.position.copy(back);
      }
      renderer.render(scene, camera);
      // Pull draw-call info post-render.
      if (session.stats && renderer.info.render.calls !== session.stats.drawCalls) {
        session.stats = { ...session.stats, drawCalls: renderer.info.render.calls };
      }
      rafId = requestAnimationFrame(tick);
    }
    tick();

    // Resize handling.
    const ro = new ResizeObserver(() => {
      if (!container) return;
      const W = container.clientWidth || w;
      const H = container.clientHeight || h;
      renderer.setSize(W, H);
      perspectiveCam.aspect = W / H;
      perspectiveCam.updateProjectionMatrix();
      syncOrthoFrustum(orthoCam, perspectiveCam.position.length(), W / H);
    });
    ro.observe(container);

    // Publish camera-state callbacks onto the session so the side
    // panel can wire "Save view" / "Restore saved" without having to
    // know anything about three.js. Cleared in cleanupFn so a new
    // ModelView mount doesn't see stale closures.
    session.snapshotView = snapshotView;
    session.restoreSavedView = restoreSavedView;
    session.backend = 'three';

    host = {
      kind: 'three',
      applyEnv: (id: EnvPresetId) => applyEnv(id),
      applyRenderMode,
      applyLightingFromSession,
      applyMaterialOverrides,
      applyCameraPreset,
      applyProjection,
      applyFov,
      doFrameAll, doResetCamera,
      snapshotView, restoreSavedView,
      selectClipImpl,
      setAutoRotate: (v: boolean) => { controls.autoRotate = v; },
      setAutoRotateSpeed: (v: number) => { controls.autoRotateSpeed = v; },
      setToneMapping: (id: import('$lib/3d/session.svelte').ModelSession['toneMapping']) => { renderer.toneMapping = toneMappingValue(id); },
      setExposure: (v: number) => { renderer.toneMappingExposure = v; },
      setShadows: (v: boolean) => { renderer.shadowMap.enabled = v; keyLight.castShadow = v && session.keyEnabled; meshes.forEach((m) => { m.castShadow = v; m.receiveShadow = v; }); },
      setBackgroundVisible: (v: boolean) => { scene.background = v ? envBackground : new THREE.Color(0x0b0b0c); },
      setGridVisible: (v: boolean) => { gridHelper.visible = v; },
      setAxesVisible: (v: boolean) => { axesHelper.visible = v; },
      setBboxVisible: (v: boolean) => { bboxHelper.visible = v; },
      setGroundPlaneVisible: (v: boolean) => { groundOpaque.visible = v; },
      setContactShadowVisible: (v: boolean) => { ground.visible = v; },
      setUpAxis: (a: import('$lib/3d/session.svelte').UpAxisId) => {
        // 'z' rotates the model -90° on X so the file's Z-up sits
        // visually as +Y on screen; 'y' leaves it as authored.
        model.rotation.x = a === 'z' ? -Math.PI / 2 : 0;
      },
    };

    cleanupFn = () => {
      cancelAnimationFrame(rafId);
      ro.disconnect();
      controls.dispose();
      threeControls = null;
      disposeOverlay();
      gridHelper.geometry.dispose();
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (gridHelper.material as any).dispose?.();
      groundGeo.dispose();
      groundMat.dispose();
      groundOpaqueMat.dispose();
      matcapTex.dispose();
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (envTexture as any)?.dispose?.();
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      if ((envBackground as any)?.isTexture) envBackground.dispose?.();
      pmrem.dispose();
      renderer.dispose();
      try { container?.removeChild(renderer.domElement); } catch { /* ignore */ }
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      scene.traverse((obj: any) => {
        if (obj.geometry) obj.geometry.dispose?.();
        if (obj.material) {
          if (Array.isArray(obj.material)) obj.material.forEach((m: import('three').Material) => m.dispose?.());
          else obj.material.dispose?.();
        }
      });
      session.snapshotView = undefined;
      session.restoreSavedView = undefined;
      host = null;
    };
  }

  // ─── Session reactivity ───────────────────────────────────────
  // Each $effect mirrors one slice of session state into the host
  // imperative API. Only fires when its specific reactive read
  // changes, so flipping the env doesn't re-run the render-mode
  // pipeline (which is expensive).
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    void host.applyEnv(session.envPreset);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setToneMapping(session.toneMapping);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setExposure(session.exposure);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setBackgroundVisible(session.backgroundVisible);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    // Read every lighting-shaped field so the effect tracks them.
    void session.keyEnabled; void session.keyIntensity;
    void session.keyAzimuth; void session.keyElevation; void session.keyColor;
    void session.fillEnabled; void session.fillIntensity;
    void session.rimEnabled; void session.rimIntensity;
    void session.shadowSoftness;
    host.applyLightingFromSession();
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setShadows(session.shadows);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setGridVisible(session.showGrid);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setAxesVisible(session.showAxes);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setBboxVisible(session.showBoundingBox);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setGroundPlaneVisible(session.groundPlane);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setContactShadowVisible(session.contactShadow);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.applyRenderMode(session.renderMode);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    // Material overrides are a deep object — read every entry so the
    // effect retracks when any field changes (Svelte 5 tracks fine-
    // grained at property level; reading the wrapper is enough).
    void session.materialOverrides;
    host.applyMaterialOverrides();
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    void session.projection;
    host.applyProjection();
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    void session.fov;
    host.applyFov();
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setUpAxis(session.upAxis);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setAutoRotate(session.autoRotate);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.setAutoRotateSpeed(session.autoRotateSpeed);
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    if (session.cameraPreset !== 'custom') {
      host.applyCameraPreset(session.cameraPreset);
    }
  });
  // Trigger-counter effects — react to "imperative" commands that
  // can be repeated (Frame all twice in a row, etc.).
  $effect(() => {
    if (!host) return;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    void (session as any)._frameAllTrigger;
    if (host.kind === 'marmoset') host.frameAll?.();
    else host.doFrameAll?.();
  });
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    void (session as any)._resetCameraTrigger;
    host.doResetCamera();
  });
  // Animation transport.
  $effect(() => {
    if (!host || host.kind !== 'three') return;
    host.selectClipImpl(session.currentClip);
  });
  // Note: clip selection above already calls play/stop based on
  // session.animationPlaying at the moment of selection. Pause /
  // resume after selection is handled here.

  // React to reviewMode flips after mount: enable/disable orbit
  // controls so the host's scroll-snap can take wheel events when
  // the viewer is in display-only mode.
  $effect(() => {
    if (threeControls) threeControls.enabled = reviewMode;
  });
</script>

<div bind:this={container} class="relative h-full w-full bg-zinc-950">
  {#if session.loading}
    <div class="pointer-events-none absolute inset-0 flex items-center justify-center text-zinc-500">
      <div class="flex items-center gap-2 text-xs">
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="animate-spin">
          <line x1="12" y1="2" x2="12" y2="6" />
          <line x1="12" y1="18" x2="12" y2="22" />
          <line x1="4.93" y1="4.93" x2="7.76" y2="7.76" />
          <line x1="16.24" y1="16.24" x2="19.07" y2="19.07" />
          <line x1="2" y1="12" x2="6" y2="12" />
          <line x1="18" y1="12" x2="22" y2="12" />
          <line x1="4.93" y1="19.07" x2="7.76" y2="16.24" />
          <line x1="16.24" y1="7.76" x2="19.07" y2="4.93" />
        </svg>
        Loading {ext.toUpperCase()}…
      </div>
    </div>
  {/if}
  {#if session.loadError}
    <div class="absolute inset-0 flex flex-col items-center justify-center gap-2 text-xs text-zinc-500">
      <p>Couldn't open {ext.toUpperCase()}: {session.loadError}</p>
      <a href={fileUrl} class="text-accent underline" target="_blank">Download original</a>
    </div>
  {/if}
</div>
