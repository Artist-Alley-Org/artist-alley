<script lang="ts">
  // 3D model body for the AssetViewer.
  //
  // Native-viewer-first per the roadmap:
  //   * glb / gltf  → <model-viewer> web component (Apache 2.0).
  //                   Self-contained, PBR-correct, animations + AR
  //                   button for free.
  //   * obj         → three.js OBJLoader (geometry only for now;
  //                   sibling .mtl + texture lookup is 1.18.B-11).
  //   * fbx         → three.js FBXLoader (handles embedded textures
  //                   in the common case; multi-file FBX likewise).
  //   * mview       → Marmoset Toolbag self-contained player (1.18.B-11).
  //   * mb / ma /   → no open converter; show the placeholder body
  //     max / blend   instead (handled at AssetViewer's dispatch).
  //
  // Everything is dynamically imported so the 3D libs don't bloat
  // the main bundle. Cards still render the col variant (worker
  // can generate a turntable poster later).

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
  // Refs the reviewMode reactive effect needs to find again after mount.
  let mvEl: HTMLElement | null = null;
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
      if (ext === 'glb' || ext === 'gltf') {
        await mountModelViewer();
      } else if (ext === 'fbx') {
        await mountThree('fbx');
      } else if (ext === 'obj') {
        await mountThree('obj');
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
  // glb / gltf — Google's <model-viewer> web component handles everything.
  // -----------------------------------------------------------------------

  async function mountModelViewer() {
    await import('@google/model-viewer');
    if (!container) return;
    const mv = document.createElement('model-viewer');
    mv.setAttribute('src', fileUrl);
    if (reviewMode) mv.setAttribute('camera-controls', '');
    mv.setAttribute('auto-rotate', '');
    mv.setAttribute('shadow-intensity', '1');
    mv.setAttribute('exposure', '1.0');
    mv.setAttribute('environment-image', 'neutral');
    mv.setAttribute('crossorigin', 'use-credentials');
    mv.style.cssText = 'width:100%;height:100%;background:#0b0b0c;';
    mv.addEventListener('error', () => { loadError = 'model failed to load'; });
    container.appendChild(mv);
    mvEl = mv;
    cleanupFn = () => {
      mvEl = null;
      try { container?.removeChild(mv); } catch { /* ignore */ }
    };
  }

  // -----------------------------------------------------------------------
  // FBX / OBJ via three.js. Imports stay dynamic so we don't ship three
  // (~600KB) for users who never open a 3D asset.
  // -----------------------------------------------------------------------

  async function mountThree(kind: 'fbx' | 'obj') {
    const THREE = await import('three');
    let model: any;
    if (kind === 'fbx') {
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

    // Frame the model — fit camera to bounding box.
    const box = new THREE.Box3().setFromObject(model);
    const size = box.getSize(new THREE.Vector3());
    const center = box.getCenter(new THREE.Vector3());
    model.position.sub(center);
    scene.add(model);

    const maxDim = Math.max(size.x, size.y, size.z) || 1;

    const camera = new THREE.PerspectiveCamera(45, w / h, maxDim / 1000, maxDim * 100);
    const dist = maxDim * 2.2;
    camera.position.set(dist, dist * 0.6, dist);
    camera.lookAt(0, 0, 0);

    const renderer = new THREE.WebGLRenderer({ antialias: true });
    renderer.setPixelRatio(window.devicePixelRatio);
    renderer.setSize(w, h);
    renderer.outputColorSpace = THREE.SRGBColorSpace;
    renderer.toneMapping = THREE.ACESFilmicToneMapping;
    renderer.toneMappingExposure = 1.0;
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

    // Orbit controls for camera-around-target interaction.
    const { OrbitControls } = await import('three/examples/jsm/controls/OrbitControls.js');
    const controls = new OrbitControls(camera, renderer.domElement);
    controls.enableDamping = true;
    controls.dampingFactor = 0.1;
    controls.target.set(0, 0, 0);
    controls.enabled = reviewMode;
    controls.update();
    threeControls = controls;

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

  // React to reviewMode flips after mount: toggle camera-controls on
  // model-viewer (glb/gltf) and OrbitControls.enabled on three (fbx/obj).
  $effect(() => {
    if (mvEl) {
      if (reviewMode) mvEl.setAttribute('camera-controls', '');
      else mvEl.removeAttribute('camera-controls');
    }
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
