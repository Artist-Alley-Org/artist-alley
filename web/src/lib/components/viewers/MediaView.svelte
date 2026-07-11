<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // MediaView — unified body for video AND audio.
  //
  // The same shell-integrated transport drives both (play / pause /
  // seek / step / setRate, installed onto the controller exactly as
  // VideoView used to). The only thing that diverges is the visual
  // surface:
  //
  //   video  →  <video> element renders frames + (for audio in MP4/
  //             MOV containers) plays the audio track natively.
  //   audio  →  <audio> element does the playback (invisible); the
  //             surface is the waveform PNG with a SoundCloud-style
  //             progress mask + optional album-cover backdrop. The
  //             shell's scrubber + JKL hotkeys + the bottom transport
  //             bar all work the same because they're wired off the
  //             controller, not the DOM element kind.
  //
  // We use one shared `mediaEl: HTMLMediaElement` ref so the transport
  // wiring is identical for both — HTMLAudioElement / HTMLVideoElement
  // both extend HTMLMediaElement and accept .play() / .pause() /
  // .currentTime / .playbackRate.
  //
  // Subtitle / VTT plumbing: one <track kind="subtitles"> per entry
  // in asset.metadata.{audio,video}.subtitle_tracks (the upcoming
  // Whisper STT phase will populate that array; until then it's empty
  // and zero tracks render). Tracks expect their VTT bytes at
  // /assets/{id}/variants/{variant_key} which the worker writes
  // alongside the cover/wave variants.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewAsset, ViewController } from './controller';
  import { kindForExtension } from './controller';

  interface SubtitleTrack {
    index: number;
    lang?: string;
    title?: string;
    format?: string;
    variant_key?: string;
    source?: string; // "embedded" | "whisper" | "manual"
  }

  interface AudioTags {
    title?: string;
    artist?: string;
    album?: string;
    album_artist?: string;
    date?: string;
    year?: string;
  }
  interface AudioMetadata {
    duration_s?: number;
    codec?: string;
    bitrate_kbps?: number;
    sample_rate_hz?: number;
    channels?: number;
    tags?: AudioTags;
    has_cover?: boolean;
    subtitle_tracks?: SubtitleTrack[];
  }
  interface VideoMetadata {
    subtitle_tracks?: SubtitleTrack[];
  }

  interface Props {
    asset: ViewAsset;
    controller: ViewController;
  }

  let { asset, controller = $bindable() }: Props = $props();

  // ── Derived: kind + per-kind URLs ─────────────────────────────────

  const kind = $derived(kindForExtension(asset.file_extension));
  const isAudio = $derived(kind === 'audio');

  const masterUrl   = $derived(`/api/v1/assets/${asset.id}/variants/hls/master.m3u8`);
  const fallbackUrl = $derived(`/api/v1/assets/${asset.id}/file`);
  const posterUrl   = $derived(`/api/v1/assets/${asset.id}/variants/poster`);
  const coverUrl    = $derived(`/api/v1/assets/${asset.id}/variants/cover`);
  const waveUrl     = $derived(`/api/v1/assets/${asset.id}/variants/screen`);
  const spriteVtt   = $derived(`/api/v1/assets/${asset.id}/variants/sprites.vtt`);
  const spritesJpg  = $derived(`/api/v1/assets/${asset.id}/variants/sprites.jpg`);

  // Per-kind metadata. Empty object fallback so optional-chain calls
  // don't crash before the asset has loaded.
  const audioMeta = $derived<AudioMetadata>(
    ((asset.metadata as Record<string, unknown> | null | undefined)?.audio as AudioMetadata | undefined) ?? {},
  );
  const videoMeta = $derived<VideoMetadata>(
    ((asset.metadata as Record<string, unknown> | null | undefined)?.video as VideoMetadata | undefined) ?? {},
  );

  // Subtitle tracks live next to the codec metadata. Both audio and
  // video can carry them — the player renders <track> for each.
  const legacyTracks = $derived<SubtitleTrack[]>(
    isAudio ? (audioMeta.subtitle_tracks ?? []) : (videoMeta.subtitle_tracks ?? []),
  );

  // Phase 1.18.B-3: top-level Asset.subtitle_tracks (uploaded via
  // POST /assets/{id}/subtitle-tracks). VTT lives in CAS — fetched
  // via /api/v1/storage/objects/{file_hash}. When the asset carries
  // both, the new uploads take priority (they're explicitly user-
  // managed); the legacy metadata array remains as fallback so
  // existing whisper-transcribed tracks keep playing.
  interface TopLevelTrack {
    lang: string;
    label?: string;
    file_hash: string;
    source_format: string;
    confidence: number;
    created_at?: string;
  }
  const topLevelTracks = $derived<TopLevelTrack[]>(
    ((asset as unknown as Record<string, unknown>).subtitle_tracks as TopLevelTrack[] | undefined) ?? [],
  );
  const subtitleTracks = $derived<SubtitleTrack[]>(
    topLevelTracks.length > 0
      ? topLevelTracks.map((t, i) => ({
          index: i,
          lang: t.lang,
          title: t.label || t.lang,
          format: t.source_format,
          variant_key: `__top:${t.file_hash}`,
          source: 'manual',
        }))
      : legacyTracks,
  );

  // Skip the cover layer entirely when the worker said there's no
  // embedded art. The fallback onerror handler still catches a
  // stale/missing variant; this just avoids the needless 404.
  const showCover = $derived(isAudio && audioMeta.has_cover === true);

  const tags          = $derived(audioMeta.tags ?? {});
  const displayTitle  = $derived(tags.title ?? asset.title ?? '');
  const displayArtist = $derived(tags.artist ?? tags.album_artist ?? '');
  const displayAlbum  = $derived(tags.album ?? '');

  // ── Local state ──────────────────────────────────────────────────

  let mediaEl: HTMLMediaElement | undefined = $state();
  let hls: any = null;

  // detectedFps is the "frame rate" the controller surfaces. Video
  // gets the real FPS once metadata loads; audio uses 1000 (1 ms
  // granularity per "frame") so the timecode reads true milliseconds
  // (DAW convention — M:SS.mmm) and scrubbing is sample-precise.
  // Shift+,/. still steps 10 frames, which at fps=1000 is the 10 ms
  // increment most audio review actually wants; the bare ,/. step
  // is 1 ms for the rare "I need one sample over" case.
  let detectedFps = $state(24);

  // Waveform progress 0–100 — updated each rAF tick from the audio
  // element's currentTime. Local copy (not derived) so the CSS mask
  // re-style is cheap and doesn't recompute on every controller field.
  let progressPct = $state(0);

  // Horizontal zoom factor on the waveform (1 = fits viewport, 16 =
  // 16x detail with horizontal scroll). Drives `width: ${zoom * 100}%`
  // on the inner waveform layer; the outer container has
  // overflow-x-auto so the user can pan to any part of the track.
  // Wheel + Ctrl/Meta over the waveform changes this; auto-scroll
  // keeps the playhead in view as audio progresses.
  let waveformZoom = $state(1);
  const WAVEFORM_ZOOM_MIN = 1;
  const WAVEFORM_ZOOM_MAX = 16;
  let waveScrollEl: HTMLDivElement | undefined = $state();

  // Loop derivations from controller — converting frames → percent of
  // total so the same mask + clip-path machinery used for the playhead
  // can render the loop band.
  const loopInPct = $derived(
    controller.totalFrames > 0 && controller.loopIn !== null
      ? Math.max(0, Math.min(100, (controller.loopIn / controller.totalFrames) * 100))
      : 0,
  );
  const loopOutPct = $derived(
    controller.totalFrames > 0 && controller.loopOut !== null
      ? Math.max(0, Math.min(100, (controller.loopOut / controller.totalFrames) * 100))
      : 0,
  );
  const hasLoop = $derived(
    controller.loopIn !== null
      && controller.loopOut !== null
      && controller.loopOut > controller.loopIn,
  );

  // ── Persistent volume / mute ─────────────────────────────────────

  const VOL_KEY = 'media.volume';
  const MUTE_KEY = 'media.muted';
  function readSavedVolume(): { vol: number; muted: boolean } {
    try {
      const v = localStorage.getItem(VOL_KEY);
      const m = localStorage.getItem(MUTE_KEY);
      return {
        vol: v !== null ? Math.max(0, Math.min(1, +v)) : 1,
        // Audio defaults un-muted (the whole point is to listen);
        // video defaults muted to satisfy browser autoplay policy.
        muted: m === null ? !isAudio : m === '1',
      };
    } catch {
      return { vol: 1, muted: !isAudio };
    }
  }

  // ── Anchor formatting (controller surfaces this to the HUD) ──────

  // Video: SMPTE-ish HH:MM:SS:FF.
  function tcVideo(frame: number): string {
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
  // Audio: M:SS.mmm — millisecond precision (DAW idiom). Audio
  // listeners don't think in frames; mm:ss.mmm is the format every
  // serious audio tool (Audacity, Reaper, Pro Tools, Ableton) uses
  // for sub-second positions. We compute from the frame index using
  // detectedFps=1000, so the displayed ms is exact, not rounded.
  function tcAudio(frame: number): string {
    if (!Number.isFinite(frame) || frame < 0) frame = 0;
    const totalMs = Math.round(frame * (1000 / Math.max(1, detectedFps)));
    const m = Math.floor(totalMs / 60000);
    const s = Math.floor(totalMs / 1000) % 60;
    const ms = totalMs % 1000;
    return `${m}:${s.toString().padStart(2, '0')}.${ms.toString().padStart(3, '0')}`;
  }

  // ── Transport implementations (installed onto controller) ────────

  function play() {
    mediaEl?.play().catch(() => { /* autoplay policy gate; user can recover via play btn */ });
  }
  function pause() { mediaEl?.pause(); }
  function togglePlay() {
    if (!mediaEl) return;
    if (mediaEl.paused) play(); else pause();
  }
  function seekToFrame(frame: number) {
    if (!mediaEl) return;
    const f = Math.max(0, Math.min(controller.totalFrames, frame));
    mediaEl.currentTime = f / detectedFps;
    controller.currentFrame = f;
  }
  function stepFrames(n: number) {
    pause();
    seekToFrame(controller.currentFrame + n);
  }
  function setRate(r: number) {
    if (!mediaEl) return;
    mediaEl.playbackRate = r;
    controller.rate = r;
  }

  // ── Lifecycle ────────────────────────────────────────────────────

  onMount(async () => {
    if (!mediaEl) return;

    // Install controller BEFORE async work so the shell's buttons
    // function even while HLS is resolving.
    controller.kind = isAudio ? 'audio' : 'video';
    controller.hasTimeline = true;
    if (isAudio) {
      detectedFps = 1000;
      controller.formatAnchor = tcAudio;
      controller.spritesUrl = '';
      controller.spritesVttUrl = '';
    } else {
      controller.spritesUrl = spritesJpg;
      controller.spritesVttUrl = spriteVtt;
      controller.formatAnchor = tcVideo;
    }
    controller.play = play;
    controller.pause = pause;
    controller.togglePlay = togglePlay;
    controller.seekToFrame = seekToFrame;
    controller.stepFrames = stepFrames;
    controller.setRate = setRate;

    const { vol, muted } = readSavedVolume();
    mediaEl.volume = vol;
    mediaEl.muted = muted;
    mediaEl.autoplay = true;
    (mediaEl as HTMLVideoElement).playsInline = true;
    if (!isAudio) {
      (mediaEl as HTMLVideoElement).poster = posterUrl;
    }

    mediaEl.addEventListener('play', () => (controller.playing = true));
    mediaEl.addEventListener('pause', () => (controller.playing = false));
    mediaEl.addEventListener('volumechange', () => {
      if (!mediaEl) return;
      try {
        localStorage.setItem(VOL_KEY, String(mediaEl.volume));
        localStorage.setItem(MUTE_KEY, mediaEl.muted ? '1' : '0');
      } catch { /* ignore */ }
    });

    // Source attachment. Audio skips HLS (no audio-only ladders); video
    // tries HLS first, falls back to /file on any error.
    if (isAudio) {
      mediaEl.src = fallbackUrl;
      mediaEl.addEventListener('loadedmetadata', () => play(), { once: true });
    } else {
      let useHLS = false;
      try {
        const head = await fetch(masterUrl, { method: 'GET', credentials: 'include' });
        useHLS = head.ok;
      } catch {
        useHLS = false;
      }
      if (useHLS && (mediaEl as HTMLVideoElement).canPlayType('application/vnd.apple.mpegurl')) {
        mediaEl.src = masterUrl;
        mediaEl.addEventListener('loadedmetadata', () => play(), { once: true });
      } else if (useHLS) {
        try {
          const mod = await import('hls.js');
          const Hls = mod.default;
          if (Hls.isSupported()) {
            hls = new Hls({ enableWorker: true, lowLatencyMode: false });
            hls.loadSource(masterUrl);
            hls.attachMedia(mediaEl);
            hls.on(Hls.Events.MANIFEST_PARSED, () => play());
            hls.on(Hls.Events.ERROR, (_e: unknown, data: any) => {
              if (data?.fatal) {
                try { hls?.destroy(); } catch { /* ignore */ }
                hls = null;
                if (mediaEl) {
                  mediaEl.src = fallbackUrl;
                  mediaEl.addEventListener('loadedmetadata', () => play(), { once: true });
                }
              }
            });
          } else if (mediaEl) {
            mediaEl.src = fallbackUrl;
            mediaEl.addEventListener('loadedmetadata', () => play(), { once: true });
          }
        } catch {
          if (mediaEl) {
            mediaEl.src = fallbackUrl;
            mediaEl.addEventListener('loadedmetadata', () => play(), { once: true });
          }
        }
      } else {
        mediaEl.src = fallbackUrl;
        mediaEl.addEventListener('loadedmetadata', () => play(), { once: true });
      }
    }

    // Progress sync. Video uses requestVideoFrameCallback for frame-
    // accurate timing; audio uses a plain rAF tick (no rVFC for
    // HTMLAudioElement). Both write controller.currentFrame so the
    // shell's scrubber tracks the playhead; audio additionally updates
    // progressPct for the waveform mask.
    if (!isAudio && 'requestVideoFrameCallback' in mediaEl) {
      const cb = (_now: number, meta: any) => {
        if (typeof meta?.mediaTime === 'number') {
          controller.currentFrame = Math.round(meta.mediaTime * detectedFps);
        }
        if (mediaEl) (mediaEl as any).requestVideoFrameCallback(cb);
      };
      (mediaEl as any).requestVideoFrameCallback(cb);
    } else {
      const tick = () => {
        if (!mediaEl) return;
        const t = mediaEl.currentTime;
        controller.currentFrame = Math.round(t * detectedFps);
        if (isAudio && controller.totalFrames > 0) {
          progressPct = (controller.currentFrame / controller.totalFrames) * 100;
        }
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
    if (!mediaEl) return;
    controller.duration = mediaEl.duration || 0;
    controller.totalFrames = Math.round((mediaEl.duration || 0) * detectedFps);
    controller.fps = detectedFps;
    if (isAudio) {
      const parts = [
        audioMeta.codec?.toUpperCase(),
        audioMeta.bitrate_kbps ? `${audioMeta.bitrate_kbps} kbps` : null,
        audioMeta.sample_rate_hz ? `${(audioMeta.sample_rate_hz / 1000).toFixed(1)} kHz` : null,
        audioMeta.channels
          ? (audioMeta.channels === 1 ? 'mono' : audioMeta.channels === 2 ? 'stereo' : `${audioMeta.channels}ch`)
          : null,
      ].filter(Boolean);
      controller.hudExtra = parts.join(' · ');
    } else {
      controller.hudExtra = `${detectedFps.toFixed(2)} fps`;
    }
  }

  // ── Audio waveform interactions ──────────────────────────────────
  //
  //   plain click          → seek to that x position (+ play if paused)
  //   shift + drag         → set loop region (in/out clamped to range)
  //   wheel + Ctrl/Meta    → horizontal zoom (1× ↔ 16×); centered on
  //                          the pointer so the user can drill into a
  //                          specific moment without losing their place
  //   wheel (no modifier)  → falls through to the shell's per-frame
  //                          scrub (controller.stepFrames via AssetViewer
  //                          onCanvasWheel), so audio gets the same
  //                          mouse-wheel scrub video has
  //
  // All three are gated on the audio element actually having loaded
  // duration data; before then we no-op rather than seek to NaN.

  function ratioFromEvent(e: MouseEvent): number {
    const inner = (e.currentTarget as HTMLElement);
    const rect = inner.getBoundingClientRect();
    return Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
  }

  function onWaveformMouseDown(e: MouseEvent) {
    if (!mediaEl) return;
    // Stop the shell from interpreting this as a canvas click-to-
    // toggle-play (AssetViewer's mousedown handler defers a togglePlay
    // for any mousedown on a timeline kind).
    e.stopPropagation();
    const dur = mediaEl.duration;
    if (!Number.isFinite(dur) || dur <= 0) return;

    if (e.shiftKey) {
      // Shift-drag = mark loop region. Start a drag tracker that
      // updates loopIn/loopOut as the user drags; commit on mouseup.
      e.preventDefault();
      const inner = e.currentTarget as HTMLElement;
      const rect = inner.getBoundingClientRect();
      const total = controller.totalFrames;
      const startRatio = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
      const startFrame = Math.round(startRatio * total);
      controller.loopIn = startFrame;
      controller.loopOut = startFrame;
      const move = (mv: MouseEvent) => {
        const r = Math.max(0, Math.min(1, (mv.clientX - rect.left) / rect.width));
        const f = Math.round(r * total);
        // Always keep loopIn ≤ loopOut so the enforcer's "if past out
        // jump to in" stays sensible no matter which direction the
        // user dragged.
        if (f < startFrame) {
          controller.loopIn = f;
          controller.loopOut = startFrame;
        } else {
          controller.loopIn = startFrame;
          controller.loopOut = f;
        }
      };
      const up = () => {
        window.removeEventListener('mousemove', move);
        window.removeEventListener('mouseup', up);
        // Tiny drags (< 1% of the track) are almost always misclicks
        // — the user shift-clicked without intending to drag. Clear
        // the loop in that case so they don't get stuck in a 0-second
        // loop.
        if (controller.loopIn !== null
          && controller.loopOut !== null
          && (controller.loopOut - controller.loopIn) < Math.max(1, total / 100)) {
          controller.loopIn = null;
          controller.loopOut = null;
        }
      };
      window.addEventListener('mousemove', move);
      window.addEventListener('mouseup', up);
      return;
    }

    // Plain click → seek. Respect the prior play/pause state — if the
    // user paused, the click should land them at a new position
    // *paused*, not yank them back into playback. (Earlier this
    // auto-played on click; user feedback called it out as wrong.)
    const r = ratioFromEvent(e);
    mediaEl.currentTime = r * dur;
  }

  function onWaveformWheel(e: WheelEvent) {
    // Ctrl/Cmd + wheel = horizontal zoom. Bare wheel falls through to
    // the shell's onCanvasWheel which scrubs the timeline a frame at
    // a time — same mental model as video.
    if (!(e.ctrlKey || e.metaKey)) return;
    e.preventDefault();
    e.stopPropagation();
    if (!waveScrollEl) return;
    const inner = e.currentTarget as HTMLElement;
    const innerRect = inner.getBoundingClientRect();
    // Where the pointer sits in track-percent terms (0–1) before zoom.
    const pointerRatio = Math.max(0, Math.min(1,
      (e.clientX - innerRect.left) / innerRect.width));
    const prevZoom = waveformZoom;
    const next = Math.max(WAVEFORM_ZOOM_MIN,
      Math.min(WAVEFORM_ZOOM_MAX,
        waveformZoom * (e.deltaY > 0 ? 0.8 : 1.25)));
    waveformZoom = next;
    if (prevZoom === next) return;
    // After zooming, keep the pointer's track-position anchored under
    // the cursor: scroll so the same track-percent stays at the same
    // viewport-x. This is the same arithmetic image zoom uses.
    requestAnimationFrame(() => {
      if (!waveScrollEl) return;
      const containerW = waveScrollEl.clientWidth;
      const newInnerW = containerW * next;
      const pointerLocal = e.clientX - waveScrollEl.getBoundingClientRect().left;
      waveScrollEl.scrollLeft = (pointerRatio * newInnerW) - pointerLocal;
    });
  }

  // Auto-scroll: keep the playhead visible inside the scroll viewport
  // when zoomed. Triggers when the playhead is about to leave the
  // visible band (gives a 15% headroom on either side so we're not
  // ping-ponging on every pixel).
  $effect(() => {
    if (!isAudio || !waveScrollEl || waveformZoom <= 1) return;
    const viewportW = waveScrollEl.clientWidth;
    const innerW = viewportW * waveformZoom;
    const playheadX = (progressPct / 100) * innerW;
    const left = waveScrollEl.scrollLeft;
    const right = left + viewportW;
    const margin = viewportW * 0.15;
    if (playheadX < left + margin) {
      waveScrollEl.scrollLeft = Math.max(0, playheadX - margin);
    } else if (playheadX > right - margin) {
      waveScrollEl.scrollLeft = Math.min(innerW - viewportW, playheadX - viewportW + margin);
    }
  });

  function resetWaveformZoom() {
    waveformZoom = 1;
    if (waveScrollEl) waveScrollEl.scrollLeft = 0;
  }
</script>

{#if isAudio}
  <!--
    Audio surface — the "video" is the waveform. Layering top-to-bottom:
      1. Cover art backdrop (z-0): dimmed + blurred fill so the
         waveform reads on top.
      2. Vignette (z-0): subtle gradient so neither pure-white nor
         pure-black covers wash the waveform out.
      3. Title card (z-20): centered name/artist/album block. Pointer-
         events-none so clicks fall through to the waveform.
      4. Waveform (z-30): full-width clickable seek surface at the
         bottom. Two stacked CSS-masked divs — "played" (accent) and
         "unplayed" (muted) — clip-pathed at progressPct. Shared
         waveform PNG as the alpha mask.
      5. <audio> element (sr-only): the actual playback; invisible.
  -->
  <div class="relative h-full w-full overflow-hidden bg-black">
    {#if showCover}
      <img
        src={coverUrl}
        alt={displayAlbum || displayTitle}
        class="absolute inset-0 h-full w-full object-cover opacity-50 blur-sm"
        loading="lazy"
        onerror={(e) => { (e.currentTarget as HTMLImageElement).style.display = 'none'; }}
      />
      <div class="absolute inset-0 bg-gradient-to-b from-black/40 via-black/20 to-black/60"></div>
    {/if}

    <!-- Centered title card. Pointer-events-none so the audio surface
         remains clickable beneath it. -->
    <div class="pointer-events-none absolute inset-x-0 top-1/2 z-20 -translate-y-1/2 px-6 text-center">
      {#if showCover}
        <img
          src={coverUrl}
          alt=""
          class="mx-auto mb-4 max-h-[40vh] max-w-[40vh] rounded-md object-contain shadow-2xl"
          loading="lazy"
          onerror={(e) => { (e.currentTarget as HTMLImageElement).style.display = 'none'; }}
        />
      {/if}
      <div class="mx-auto inline-block max-w-[min(640px,90%)] rounded-md bg-black/55 px-6 py-3 backdrop-blur-sm">
        <div class="truncate text-2xl font-semibold text-white">
          {displayTitle || '(untitled)'}
        </div>
        {#if displayArtist || displayAlbum}
          <div class="mt-1 truncate text-sm text-white/70">
            {displayArtist}
            {#if displayArtist && displayAlbum} — {/if}
            {displayAlbum}
          </div>
        {/if}
      </div>
    </div>

    <!-- Waveform bottom strip — outer container scrolls horizontally
         when the inner waveform is zoomed beyond 1×. All masked
         layers (played / unplayed / loop band / playhead) live on
         the same zoomable inner so they stay perfectly aligned. -->
    <div
      bind:this={waveScrollEl}
      class="absolute inset-x-0 bottom-0 z-30 h-[18vh] overflow-x-auto overflow-y-hidden select-none"
      style:scrollbar-width="thin"
    >
      <!-- Zoom-reset chip — only visible when zoomed; sits inside
           the scroll viewport so it's not pushed off when zoomed in.
           Fixed-position relative to the viewport via sticky. -->
      {#if waveformZoom > 1}
        <button
          type="button"
          onclick={resetWaveformZoom}
          onmousedown={(e) => e.stopPropagation()}
          class="sticky left-2 top-2 z-50 inline-flex items-center gap-1 rounded bg-black/70 px-2 py-0.5 text-[10px] font-mono text-white/90 hover:bg-black/90"
          title="Reset waveform zoom (Ctrl/Cmd + Wheel to zoom)"
        >
          {waveformZoom.toFixed(1)}× — reset
        </button>
      {/if}

      <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
      <div
        class="relative h-full cursor-pointer"
        style:width={`${100 * waveformZoom}%`}
        onmousedown={onWaveformMouseDown}
        onwheel={onWaveformWheel}
        role="slider"
        aria-label="Audio scrubber (shift-drag to set loop region; Ctrl/Cmd + wheel to zoom)"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(progressPct)}
        tabindex="0"
      >
        <!-- Unplayed: muted gray, masked to the waveform shape,
             clipped to the *right* of the playhead. -->
        <div
          class="absolute inset-0 bg-fg-muted/40"
          style:-webkit-mask-image="url({waveUrl})"
          style:mask-image="url({waveUrl})"
          style:-webkit-mask-size="100% 100%"
          style:mask-size="100% 100%"
          style:-webkit-mask-repeat="no-repeat"
          style:mask-repeat="no-repeat"
          style:-webkit-mask-mode="alpha"
          style:mask-mode="alpha"
          style:clip-path={`inset(0 0 0 ${progressPct}%)`}
        ></div>
        <!-- Played: accent, same mask, clipped to the *left* of the
             playhead. -->
        <div
          class="absolute inset-0 bg-accent"
          style:-webkit-mask-image="url({waveUrl})"
          style:mask-image="url({waveUrl})"
          style:-webkit-mask-size="100% 100%"
          style:mask-size="100% 100%"
          style:-webkit-mask-repeat="no-repeat"
          style:mask-repeat="no-repeat"
          style:-webkit-mask-mode="alpha"
          style:mask-mode="alpha"
          style:clip-path={`inset(0 ${100 - progressPct}% 0 0)`}
        ></div>

        <!-- Loop region band — translucent yellow between loop in /
             out, with thin vertical markers at the boundaries.
             Pointer-events-none so the user can still click + drag
             through it to seek or re-mark. -->
        {#if hasLoop}
          <div
            class="pointer-events-none absolute top-0 bottom-0 bg-yellow-400/20 ring-1 ring-yellow-400/60"
            style:left={`${loopInPct}%`}
            style:width={`${loopOutPct - loopInPct}%`}
          ></div>
          <div
            class="pointer-events-none absolute top-0 bottom-0 w-px bg-yellow-300"
            style:left={`${loopInPct}%`}
          ></div>
          <div
            class="pointer-events-none absolute top-0 bottom-0 w-px bg-yellow-300"
            style:left={`${loopOutPct}%`}
          ></div>
        {/if}

        <!-- Playhead bar — precise visual cue regardless of how much
             is played. -->
        <div
          class="pointer-events-none absolute top-0 bottom-0 w-0.5 bg-white/90 shadow-[0_0_6px_2px_rgba(255,255,255,0.45)]"
          style:left={`${progressPct}%`}
        ></div>
      </div>
    </div>

    <!-- Tiny help text below the waveform: shift-drag + zoom hint.
         Only visible until the user has interacted with either; we
         don't have telemetry of that yet, so leave it always-on for
         now (it's small + faint). -->
    <div class="pointer-events-none absolute bottom-[19vh] right-3 z-30 rounded bg-black/55 px-2 py-0.5 text-[10px] text-white/60">
      ⇧-drag = loop · Ctrl/⌘ + wheel = zoom
    </div>

    <!-- Actual playback. sr-only keeps the element interactable to
         assistive tech but visually hidden; the shell drives it via
         the installed controller transport. -->
    <audio
      bind:this={mediaEl}
      onloadedmetadata={onLoadedMetadata}
      preload="metadata"
      class="sr-only"
    >
      {#each subtitleTracks as track (track.index)}
        {#if track.variant_key}
          <track
            kind="subtitles"
            src={track.variant_key.startsWith('__top:')
              ? `/api/v1/storage/objects/${track.variant_key.slice(6)}`
              : `/api/v1/assets/${asset.id}/variants/${track.variant_key}`}
            srclang={track.lang ?? 'en'}
            label={track.title ?? track.lang ?? `Track ${track.index}`}
          />
        {/if}
      {/each}
    </audio>
  </div>
{:else}
  <!-- Video surface — the shell wraps us with its pan/zoom transform
       and HUD, so the markup stays minimal: a sized <video> element
       that fills the canvas. -->
  <video
    bind:this={mediaEl}
    class="max-h-full max-w-full"
    style="width: 100%; height: 100%; object-fit: contain;"
    onloadedmetadata={onLoadedMetadata}
    preload="metadata"
    playsinline
  >
    <track kind="metadata" src={spriteVtt} default />
    {#each subtitleTracks as track (track.index)}
      {#if track.variant_key}
        <track
          kind="subtitles"
          src={track.variant_key.startsWith('__top:')
            ? `/api/v1/storage/objects/${track.variant_key.slice(6)}`
            : `/api/v1/assets/${asset.id}/variants/${track.variant_key}`}
          srclang={track.lang ?? 'en'}
          label={track.title ?? track.lang ?? `Track ${track.index}`}
        />
      {/if}
    {/each}
  </video>
{/if}
