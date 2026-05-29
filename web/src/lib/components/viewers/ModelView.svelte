<script lang="ts">
  // 3D model body for the AssetViewer.
  //
  // One unified three.js path for every supported 3D format:
  //   * glb / gltf  → GLTFLoader (PBR-native; animations on result.animations)
  //   * fbx         → FBXLoader  + material upgrade pass
  //   * obj         → OBJLoader  + material upgrade pass (.mtl in 1.18.B-12c)
  //   * mview       → Marmoset Toolbag self-contained player (1.18.B-11)
  //   * mb/ma/max   → no open converter; AssetViewer dispatches to
  //   blend         placeholder for those.
  //
  // We previously routed glb through Google's <model-viewer>, but
  // having a separate renderer for "one of the four" meant the tools
  // panel (grid, wireframe, exposure, etc.) had to be wired twice
  // with different APIs. GLTFLoader gives us a single code path,
  // single material upgrade, single lighting setup. The model-viewer-
  // exclusive AR button isn't critical for a studio review tool.
  //
  // Everything is dynamically imported so the 3D libs don't bloat
  // the main bundle for non-3D users.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';

  interface Asset {
    id: string;
    title?: string | null;
    file_extension?: string | null;
  }

  interface Props {
    asset: Asset;
    controller: ViewController;
    /** When false, camera interaction is disabled so the parent (e.g.
        the PostModal scroll-snap) can take wheel + drag. Auto-rotate
        on glb/gltf still runs — that's animation, not input. */
    reviewMode?: boolean;
  }

  let { asset, controller = $bindable(), reviewMode = false }: Props = $props();

  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);
  const ext = $derived((asset.file_extension || '').toLowerCase().replace(/^\./, ''));

  let container: HTMLDivElement | undefined = $state();
  let loadError = $state<string | null>(null);
  let loading = $state(true);

  // Cleanup state (so onDestroy can dispose three.js resources).
  let cleanupFn: (() => void) | null = null;
  // Ref the reviewMode reactive effect needs to find again after mount.
  let threeControls: { enabled: boolean } | null = null;

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
    // Cleared at mount; mountThree / mountModelViewer populate it
    // once the renderer is up.
    controller.tools = null;

    void mount();
  });

  onDestroy(() => {
    if (cleanupFn) cleanupFn();
  });

  async function mount() {
    if (!container) return;
    loadError = null;
    loading = true;
    try {
      if (ext === 'glb' || ext === 'gltf' || ext === 'fbx' || ext === 'obj') {
        await mountThree(ext);
      } else {
        loadError = `${ext} viewer not yet implemented`;
      }
    } catch (e) {
      loadError = e instanceof Error ? e.message : 'load failed';
    } finally {
      loading = false;
    }
  }

  // -----------------------------------------------------------------------
  // Unified three.js path. Dynamic imports keep the 600KB three bundle
  // out of the main chunk for sessions that never open a 3D asset.
  // -----------------------------------------------------------------------

  async function mountThree(kind: 'glb' | 'gltf' | 'fbx' | 'obj') {
    const THREE = await import('three');
    let model: any;
    if (kind === 'glb' || kind === 'gltf') {
      const { GLTFLoader } = await import('three/examples/jsm/loaders/GLTFLoader.js');
      const result = await new Promise<any>((res, rej) => {
        new GLTFLoader().load(fileUrl, res, undefined, rej);
      });
      model = result.scene;
      // result.animations is available here; B-12b-3 wires AnimationMixer.
    } else if (kind === 'fbx') {
      const { FBXLoader } = await import('three/examples/jsm/loaders/FBXLoader.js');
      model = await new Promise<any>((res, rej) => {
        new FBXLoader().load(fileUrl, res, undefined, rej);
      });
    } else {
      const { OBJLoader } = await import('three/examples/jsm/loaders/OBJLoader.js');
      model = await new Promise<any>((res, rej) => {
        new OBJLoader().load(fileUrl, res, undefined, rej);
      });
    }

    if (!container) return;
    const w = container.clientWidth || 800;
    const h = container.clientHeight || 600;

    const scene = new THREE.Scene();
    scene.background = new THREE.Color(0x0b0b0c);

    // Material normalisation — FBXLoader produces MeshPhongMaterial
    // and OBJLoader (without an MTL) often leaves meshes with no
    // material or a basic one. Neither responds to the RoomEnvironment
    // envmap, so the model renders as a near-black silhouette no
    // matter how bright the lights are. Walk the tree once and upgrade
    // to MeshStandardMaterial (PBR) so IBL actually lands on it.
    // Mirrors what model-viewer + the threejs editor + Sketchfab do
    // on import: "if the source material isn't PBR, give us a PBR
    // proxy that respects the env." Texture maps + colours carry over.
    const upgradeMaterial = (m: any): any => {
      if (!m) {
        return new THREE.MeshStandardMaterial({
          color: 0x9a9a9a, roughness: 0.55, metalness: 0,
        });
      }
      const isStandard = m.type === 'MeshStandardMaterial' || m.type === 'MeshPhysicalMaterial';
      if (isStandard) return m;
      // Pull whatever metadata Phong/Basic shipped with — color, maps,
      // emissive, PBR params if they exist — and rebuild as Standard.
      // Per three.js forum guidance, FBXLoader's MeshPhongMaterial
      // doesn't respond to envmap correctly, which is why every
      // PBR-correct viewer (Sketchfab, model-viewer, threejs editor)
      // upgrades on import.
      const color = m.color?.isColor ? m.color.clone() : new THREE.Color(0x9a9a9a);
      // Some exports stamp the diffuse colour as pure black even when
      // they meant "use the map only". Don't let that win.
      if (color.r === 0 && color.g === 0 && color.b === 0) {
        color.setHex(m.map ? 0xffffff : 0x9a9a9a);
      }
      // If the source did set PBR-ish params, respect them. Phong's
      // `shininess` (0–100) roughly maps to roughness via 1 - sqrt(s/100).
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
        // Default to fully dielectric for unknown materials. The
        // metalness MAP, if present, will modulate this on a
        // per-texel basis, so dielectric default is safe.
        metalness: hasMetalness ? m.metalness : (m.metalnessMap ? 1 : 0),
        transparent: m.transparent ?? false,
        opacity: m.opacity ?? 1,
        side: m.side ?? THREE.FrontSide,
      });
    };
    model.traverse((obj: any) => {
      if (!obj.isMesh) return;
      if (Array.isArray(obj.material)) {
        obj.material = obj.material.map(upgradeMaterial);
      } else {
        obj.material = upgradeMaterial(obj.material);
      }
    });

    // Frame the model — fit camera to bounding box.
    const box = new THREE.Box3().setFromObject(model);
    const size = box.getSize(new THREE.Vector3());
    const center = box.getCenter(new THREE.Vector3());
    model.position.sub(center);
    scene.add(model);

    const maxDim = Math.max(size.x, size.y, size.z) || 1;
    const minY = -size.y / 2;

    // Initial camera pose — kept as a frame-all target. Frame All
    // restores these values; the user reads "the view I had when I
    // opened this" as the canonical reset.
    const initialCamPos = new THREE.Vector3();
    const initialTarget = new THREE.Vector3(0, 0, 0);

    const camera = new THREE.PerspectiveCamera(45, w / h, maxDim / 1000, maxDim * 100);
    const dist = maxDim * 2.2;
    initialCamPos.set(dist, dist * 0.6, dist);
    camera.position.copy(initialCamPos);
    camera.lookAt(initialTarget);

    const renderer = new THREE.WebGLRenderer({ antialias: true });
    renderer.setPixelRatio(window.devicePixelRatio);
    renderer.setSize(w, h);
    renderer.outputColorSpace = THREE.SRGBColorSpace;
    renderer.toneMapping = THREE.ACESFilmicToneMapping;
    // Per the three.js forum's PBR guidance: ACES filmic blows out the
    // highlights at the default 1.0 exposure because the env carries
    // most of the light now. 0.75 is the recommended starting point;
    // user can drive it from the Lighting slider in the tools panel.
    renderer.toneMappingExposure = 0.75;
    container.appendChild(renderer.domElement);

    // Image-based lighting via RoomEnvironment — a procedural studio
    // env generated on the fly. Without an envmap, PBR materials with
    // any metalness look pitch-black (metals reflect the environment,
    // and "no env" = "reflect nothing"); even dielectrics look dull.
    // RoomEnvironment is the same primitive Sketchfab + the three.js
    // editor use for the "general purpose viewer" default look.
    const { RoomEnvironment } = await import('three/examples/jsm/environments/RoomEnvironment.js');
    const pmrem = new THREE.PMREMGenerator(renderer);
    pmrem.compileEquirectangularShader();
    scene.environment = pmrem.fromScene(new RoomEnvironment(), 0.04).texture;

    // Key directional on top of the IBL — adds a crisp shadow term so
    // the model has form even when the env is flat ambient. Intensity
    // is intentionally low; the env carries most of the lighting now.
    const key = new THREE.DirectionalLight(0xffffff, 1.5);
    key.position.set(maxDim * 2, maxDim * 4, maxDim * 2);
    scene.add(key);

    // Studio grid — placed at the model's bottom so it visually
    // grounds the asset. Hidden by default; toggleable via the tools
    // panel. Sized to ~4× the model so the user can pan/orbit a bit
    // and still see grid context.
    const gridSize = maxDim * 4;
    const gridDivisions = 20;
    const gridHelper = new THREE.GridHelper(gridSize, gridDivisions, 0x606060, 0x303030);
    gridHelper.position.y = minY;
    gridHelper.visible = false;
    scene.add(gridHelper);

    // Cache every mesh so wireframe + material modes can re-traverse
    // cheaply without walking the whole graph every toggle.
    const meshes: any[] = [];
    model.traverse((obj: any) => { if (obj.isMesh) meshes.push(obj); });

    // Wireframe overlay group (lazy — only created the first time the
    // user picks 'overlay'). Kept in scope so the tools cycle can
    // toggle it on/off without rebuilding edge geometry every flip.
    let overlayGroup: any | null = null;

    function buildOverlay() {
      const group = new THREE.Group();
      meshes.forEach((m: any) => {
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

    function applyWireframeMode(mode: 'off' | 'on' | 'overlay') {
      // First: clear overlay if any.
      if (overlayGroup) {
        scene.remove(overlayGroup);
        overlayGroup.traverse((o: any) => {
          o.geometry?.dispose?.();
          o.material?.dispose?.();
        });
        overlayGroup = null;
      }
      // Then: set the wireframe flag on every material.
      const showWire = mode === 'on';
      meshes.forEach((m: any) => {
        const mats = Array.isArray(m.material) ? m.material : [m.material];
        mats.forEach((mat: any) => {
          if (mat && 'wireframe' in mat) mat.wireframe = showWire;
        });
      });
      // Finally: add the overlay when in 'overlay' mode.
      if (mode === 'overlay') {
        overlayGroup = buildOverlay();
        scene.add(overlayGroup);
      }
    }

    // Orbit controls for camera-around-target interaction.
    const { OrbitControls } = await import('three/examples/jsm/controls/OrbitControls.js');
    const controls = new OrbitControls(camera, renderer.domElement);
    controls.enableDamping = true;
    controls.dampingFactor = 0.1;
    controls.target.copy(initialTarget);
    controls.enabled = reviewMode;
    controls.autoRotateSpeed = 2.0;
    controls.update();
    threeControls = controls;

    // Wire the tools panel into the controller. The shell reads from
    // controller.tools and renders one section per defined group; we
    // write live values back so sliders/toggles stay in sync if any
    // other surface (hotkeys, future presets) mutates state.
    const wireframeOptions = ['off', 'on', 'overlay'] as const;
    let wireframeIdx = 0;
    controller.tools = {
      exposure: {
        value: 0.75, min: 0.1, max: 5.0, step: 0.05, label: 'Exposure',
        set: (v) => {
          renderer.toneMappingExposure = v;
          if (controller.tools?.exposure) controller.tools.exposure.value = v;
        },
      },
      grid: {
        enabled: false,
        toggle: () => {
          gridHelper.visible = !gridHelper.visible;
          if (controller.tools?.grid) controller.tools.grid.enabled = gridHelper.visible;
        },
      },
      wireframe: {
        mode: 'off',
        options: wireframeOptions,
        cycle: () => {
          wireframeIdx = (wireframeIdx + 1) % wireframeOptions.length;
          const m = wireframeOptions[wireframeIdx];
          applyWireframeMode(m);
          if (controller.tools?.wireframe) controller.tools.wireframe.mode = m;
        },
      },
      autoRotate: {
        enabled: false,
        toggle: () => {
          controls.autoRotate = !controls.autoRotate;
          if (controller.tools?.autoRotate) controller.tools.autoRotate.enabled = controls.autoRotate;
        },
      },
      autoRotateSpeed: {
        value: 2.0, min: 0.5, max: 8.0, step: 0.1, label: 'Spin speed',
        set: (v) => {
          controls.autoRotateSpeed = v;
          if (controller.tools?.autoRotateSpeed) controller.tools.autoRotateSpeed.value = v;
        },
      },
      frameAll: () => {
        camera.position.copy(initialCamPos);
        controls.target.copy(initialTarget);
        controls.update();
      },
    };

    // Animation loop.
    let rafId = 0;
    function tick() {
      controls.update();
      renderer.render(scene, camera);
      rafId = requestAnimationFrame(tick);
    }
    tick();

    // Resize handling.
    const ro = new ResizeObserver(() => {
      if (!container) return;
      const W = container.clientWidth || w;
      const H = container.clientHeight || h;
      renderer.setSize(W, H);
      camera.aspect = W / H;
      camera.updateProjectionMatrix();
    });
    ro.observe(container);

    cleanupFn = () => {
      cancelAnimationFrame(rafId);
      ro.disconnect();
      controls.dispose();
      threeControls = null;
      controller.tools = null;
      if (overlayGroup) {
        scene.remove(overlayGroup);
        overlayGroup.traverse((o: any) => {
          o.geometry?.dispose?.();
          o.material?.dispose?.();
        });
        overlayGroup = null;
      }
      gridHelper.geometry.dispose();
      (gridHelper.material as any).dispose?.();
      scene.environment?.dispose?.();
      pmrem.dispose();
      renderer.dispose();
      try { container?.removeChild(renderer.domElement); } catch { /* ignore */ }
      scene.traverse((obj: any) => {
        if (obj.geometry) obj.geometry.dispose?.();
        if (obj.material) {
          if (Array.isArray(obj.material)) obj.material.forEach((m: any) => m.dispose?.());
          else obj.material.dispose?.();
        }
      });
    };
  }

  // React to reviewMode flips after mount: enable/disable orbit
  // controls so the host's scroll-snap (or future page scroller) can
  // take wheel events when the viewer is in display-only mode.
  $effect(() => {
    if (threeControls) threeControls.enabled = reviewMode;
  });
</script>

<div bind:this={container} class="relative h-full w-full bg-zinc-950">
  {#if loading}
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
  {#if loadError}
    <div class="absolute inset-0 flex flex-col items-center justify-center gap-2 text-xs text-zinc-500">
      <p>Couldn't open {ext.toUpperCase()}: {loadError}</p>
      <a href={fileUrl} class="text-accent underline" target="_blank">Download original</a>
    </div>
  {/if}
</div>
