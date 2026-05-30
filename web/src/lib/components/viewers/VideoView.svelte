<script lang="ts">
  // Video body for the AssetViewer.
  //
  // Owns: <video> element, HLS.js (or native HLS), per-frame
  // callback for the frame counter, sticky muted-autoplay +
  // persistent localStorage volume.
  //
  // Does NOT own: HUD chrome, transport bar, scrubber, fullscreen
  // button, jump-to-frame input, pan/zoom transform — those are the
  // shell's job. We install transport functions on the controller
  // so the shell's buttons + hotkeys drive playback through us.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';

  type Asset = import('./controller').ViewAsset;

  interface Props {
    asset: Asset;
    controller: ViewController;
  }

  let { asset, controller = $bindable() }: Props = $props();

  const masterUrl = $derived(`/api/v1/assets/${asset.id}/variants/hls/master.m3u8`);
  const fallbackUrl = $derived(`/api/v1/assets/${asset.id}/file`);
  const posterUrl = $derived(`/api/v1/assets/${asset.id}/variants/poster`);
  const spriteVTT = $derived(`/api/v1/assets/${asset.id}/variants/sprites.vtt`);
  const spritesJpg = $derived(`/api/v1/assets/${asset.id}/variants/sprites.jpg`);

  let videoEl: HTMLVideoElement | undefined = $state();
  let hls: any = null;

  let detectedFps = $state(24);

  const VOL_KEY = 'video.volume';
  const MUTE_KEY = 'video.muted';
  function readSavedVolume(): { vol: number; muted: boolean } {
    try {
      const v = localStorage.getItem(VOL_KEY);
      const m = localStorage.getItem(MUTE_KEY);
      return {
        vol: v !== null ? Math.max(0, Math.min(1, +v)) : 1,
        muted: m === null ? true : m === '1',
      };
    } catch {
      return { vol: 1, muted: true };
    }
  }

  function tc(frame: number): string {
    if (!Number.isFinite(frame) || frame < 0) frame = 0;
    const fr = Math.round(frame);
    const fpsR = Math.max(1, Math.round(detectedFps));
    const f = fr % fpsR;
    const totSec = Math.floor(fr / fpsR);
    const s = totSec % 60;
    const m = Math.floor(totSec / 60) % 60;
    const h = Math.floor(totSec / 3600);
    const pad = (n: number, w = 2) => n.toString().padStart(w, '0');
    return `${pad(h)}:${pad(m)}:${pad(s)}:${pad(f)}`;
  }

  // Transport implementations
  function play() { videoEl?.play(); }
  function pause() { videoEl?.pause(); }
  function togglePlay() {
    if (!videoEl) return;
    if (videoEl.paused) videoEl.play(); else videoEl.pause();
  }
  function seekToFrame(frame: number) {
    if (!videoEl) return;
    const f = Math.max(0, Math.min(controller.totalFrames, frame));
    videoEl.currentTime = f / detectedFps;
    controller.currentFrame = f;
  }
  function stepFrames(n: number) {
    pause();
    seekToFrame(controller.currentFrame + n);
  }
  function setRate(r: number) {
    if (!videoEl) return;
    videoEl.playbackRate = r;
    controller.rate = r;
  }

  onMount(async () => {
    if (!videoEl) return;

    // Install controller before any async work so the shell's
    // buttons work even if HLS is still resolving.
    controller.kind = 'video';
    controller.hasTimeline = true;
    controller.spritesUrl = spritesJpg;
    controller.spritesVttUrl = spriteVTT;
    controller.formatAnchor = tc;
    controller.play = play;
    controller.pause = pause;
    controller.togglePlay = togglePlay;
    controller.seekToFrame = seekToFrame;
    controller.stepFrames = stepFrames;
    controller.setRate = setRate;

    videoEl.poster = posterUrl;
    const { vol, muted } = readSavedVolume();
    videoEl.volume = vol;
    videoEl.muted = muted;
    videoEl.autoplay = true;
    videoEl.playsInline = true;

    videoEl.addEventListener('play', () => (controller.playing = true));
    videoEl.addEventListener('pause', () => (controller.playing = false));
    videoEl.addEventListener('volumechange', () => {
      if (!videoEl) return;
      try {
        localStorage.setItem(VOL_KEY, String(videoEl.volume));
        localStorage.setItem(MUTE_KEY, videoEl.muted ? '1' : '0');
      } catch { /* ignore */ }
    });

    // HEAD the master playlist so a not-yet-encoded asset goes
    // straight to /file instead of looping inside hls.js's retry
    // path.
    let useHLS = false;
    try {
      const head = await fetch(masterUrl, { method: 'GET', credentials: 'include' });
      useHLS = head.ok;
    } catch {
      useHLS = false;
    }

    const attemptPlay = () => {
      if (!videoEl) return;
      videoEl.play().catch(() => { /* policy gate; user can recover */ });
    };

    if (useHLS && videoEl.canPlayType('application/vnd.apple.mpegurl')) {
      videoEl.src = masterUrl;
      videoEl.addEventListener('loadedmetadata', attemptPlay, { once: true });
    } else if (useHLS) {
      try {
        const mod = await import('hls.js');
        const Hls = mod.default;
        if (Hls.isSupported()) {
          hls = new Hls({ enableWorker: true, lowLatencyMode: false });
          hls.loadSource(masterUrl);
          hls.attachMedia(videoEl);
          hls.on(Hls.Events.MANIFEST_PARSED, attemptPlay);
          hls.on(Hls.Events.ERROR, (_e: unknown, data: any) => {
            if (data?.fatal) {
              try { hls?.destroy(); } catch { /* ignore */ }
              hls = null;
              if (videoEl) {
                videoEl.src = fallbackUrl;
                videoEl.addEventListener('loadedmetadata', attemptPlay, { once: true });
              }
            }
          });
        } else if (videoEl) {
          videoEl.src = fallbackUrl;
          videoEl.addEventListener('loadedmetadata', attemptPlay, { once: true });
        }
      } catch {
        if (videoEl) {
          videoEl.src = fallbackUrl;
          videoEl.addEventListener('loadedmetadata', attemptPlay, { once: true });
        }
      }
    } else {
      videoEl.src = fallbackUrl;
      videoEl.addEventListener('loadedmetadata', attemptPlay, { once: true });
    }

    // requestVideoFrameCallback gives us frame-accurate timing on
    // every painted frame. Fall back to rAF.
    if ('requestVideoFrameCallback' in videoEl) {
      const cb = (_now: number, meta: any) => {
        if (typeof meta?.mediaTime === 'number') {
          controller.currentFrame = Math.round(meta.mediaTime * detectedFps);
        }
        if (videoEl) (videoEl as any).requestVideoFrameCallback(cb);
      };
      (videoEl as any).requestVideoFrameCallback(cb);
    } else {
      const tick = () => {
        if (!videoEl) return;
        controller.currentFrame = Math.round(videoEl.currentTime * detectedFps);
        requestAnimationFrame(tick);
      };
      requestAnimationFrame(tick);
    }
  });

  onDestroy(() => {
    if (hls) {
      try { hls.destroy(); } catch { /* ignore */ }
      hls = null;
    }
  });

  function onLoadedMetadata() {
    if (!videoEl) return;
    controller.duration = videoEl.duration || 0;
    controller.totalFrames = Math.round((videoEl.duration || 0) * detectedFps);
    controller.fps = detectedFps;
    controller.hudExtra = `${detectedFps.toFixed(2)} fps`;
  }
</script>

<video
  bind:this={videoEl}
  class="max-h-full max-w-full"
  style="width: 100%; height: 100%; object-fit: contain;"
  onloadedmetadata={onLoadedMetadata}
  preload="metadata"
  playsinline
>
  <track kind="metadata" src={spriteVTT} default />
</video>
