<script lang="ts">
  // AudioView — native HTML5 <audio> player + the waveform PNG
  // (rendered server-side by preview.audio) as the backdrop.
  // Track metadata (artist / title / album / duration) is read out of
  // the asset's metadata JSONB.
  //
  // The waveform itself is interactive — click anywhere on it to
  // seek the audio to that proportional position. A scrub cursor
  // tracks the current playhead so the user can see where they
  // are at a glance even when the standard transport controls
  // scroll out of view on small screens.
  //
  // Future: spectrogram toggle; ID3 album-art extraction.

  import type { ViewController } from './controller';
  import { defaultController } from './controller';
  import { onMount } from 'svelte';

  interface AudioMetadata {
    duration_s?: number;
    codec?: string;
    bitrate_kbps?: number;
    sample_rate_hz?: number;
    channels?: number;
    tags?: Record<string, string>;
  }

  // Shared with AssetViewer + every other ViewBody via controller.ts.
  type Asset = import('./controller').ViewAsset;

  let { asset, controller = $bindable<ViewController>() }: {
    asset: Asset;
    controller: ViewController;
  } = $props();

  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);
  const waveUrl = $derived(`/api/v1/assets/${asset.id}/variants/screen`);
  const meta = $derived<AudioMetadata>(
    ((asset.metadata as Record<string, unknown> | null | undefined)?.audio as AudioMetadata | undefined) ?? {},
  );

  // Per-track display strings, picked from the common tag keys both
  // ID3 (uppercase ARTIST/TITLE) and Vorbis (lowercase artist/title)
  // funnel into via probeMetadata().
  const tags = $derived(meta.tags ?? {});
  const displayTitle = $derived(tags.title ?? asset.title ?? '(untitled)');
  const displayArtist = $derived(tags.artist ?? tags.album_artist ?? '');
  const displayAlbum = $derived(tags.album ?? '');
  const displayYear = $derived(tags.date ?? tags.year ?? '');

  function fmtDuration(s: number | undefined): string {
    if (!s || !isFinite(s)) return '';
    const m = Math.floor(s / 60);
    const sec = Math.floor(s % 60);
    return `${m}:${sec.toString().padStart(2, '0')}`;
  }

  onMount(() => {
    const hudParts = [
      meta.codec?.toUpperCase(),
      meta.bitrate_kbps ? `${meta.bitrate_kbps} kbps` : null,
      fmtDuration(meta.duration_s),
    ].filter(Boolean);
    controller = {
      ...defaultController(),
      hudExtra: hudParts.join(' · '),
    };
  });

  // Audio element + waveform region — bound so the seek handler
  // can compute the proportional click position and the scrub
  // cursor can track playback progress.
  let audioEl = $state<HTMLAudioElement | null>(null);
  let cursorPct = $state(0);

  function onWaveClick(e: MouseEvent) {
    if (!audioEl) return;
    // The audio element may be still loading metadata — duration
    // is NaN until then. Silently no-op rather than seeking to
    // an invalid time (which Safari throws on).
    const dur = audioEl.duration;
    if (!Number.isFinite(dur) || dur <= 0) return;
    const region = e.currentTarget as HTMLElement;
    const rect = region.getBoundingClientRect();
    const ratio = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    audioEl.currentTime = ratio * dur;
    // Auto-play on click so the seek behaves like a YouTube-style
    // scrubber — click means "jump there and listen".
    if (audioEl.paused) {
      void audioEl.play().catch(() => {});
    }
  }

  function onWaveKey(e: KeyboardEvent) {
    if (!audioEl) return;
    if (e.key === ' ' || e.key === 'Enter') {
      e.preventDefault();
      audioEl.paused ? void audioEl.play().catch(() => {}) : audioEl.pause();
    } else if (e.key === 'ArrowLeft') {
      audioEl.currentTime = Math.max(0, audioEl.currentTime - 5);
    } else if (e.key === 'ArrowRight') {
      audioEl.currentTime = Math.min(audioEl.duration || 0, audioEl.currentTime + 5);
    }
  }

  function onAudioTime() {
    if (!audioEl) return;
    const dur = audioEl.duration;
    if (!Number.isFinite(dur) || dur <= 0) return;
    cursorPct = (audioEl.currentTime / dur) * 100;
  }
</script>

<div class="relative flex h-full w-full items-center justify-center bg-black/40">
  <!-- Backdrop: clickable waveform that doubles as a scrubber.
       The host shell may pass a minimal asset object (id + title +
       file_extension only); the /variants/screen endpoint just
       404s in that case and the <img> falls back to transparent —
       no UI crash. -->
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div
    role="slider"
    tabindex="0"
    aria-label="Seek audio"
    aria-valuemin={0}
    aria-valuemax={100}
    aria-valuenow={Math.round(cursorPct)}
    class="absolute inset-0 cursor-pointer focus:outline-none"
    onclick={onWaveClick}
    onkeydown={onWaveKey}
  >
    <img
      src={waveUrl}
      alt={`${displayTitle} waveform`}
      class="absolute inset-0 h-full w-full object-cover opacity-60 pointer-events-none"
      loading="lazy"
      onerror={(e) => { (e.currentTarget as HTMLImageElement).style.display = 'none'; }}
    />
    <div class="absolute inset-0 bg-gradient-to-b from-black/30 via-transparent to-black/70 pointer-events-none"></div>
    <!-- Scrub cursor: a vertical bar at the current playhead. -->
    <div
      class="absolute top-0 bottom-0 w-0.5 bg-accent shadow-[0_0_8px_2px_rgba(99,102,241,0.5)] pointer-events-none transition-[left] duration-100"
      style:left={`${cursorPct}%`}
    ></div>
  </div>

  <!-- Card: title + artist + native audio controls. -->
  <div class="relative z-10 flex flex-col items-center gap-4 rounded-lg bg-black/60 px-8 py-6 backdrop-blur-sm max-w-[min(640px,90%)] pointer-events-none">
    <div class="text-center">
      <div class="text-2xl font-semibold text-white">{displayTitle}</div>
      {#if displayArtist}
        <div class="mt-1 text-sm text-white/70">
          {displayArtist}
          {#if displayAlbum} — {displayAlbum}{/if}
          {#if displayYear} ({displayYear}){/if}
        </div>
      {/if}
    </div>
    <audio
      bind:this={audioEl}
      controls
      preload="metadata"
      src={fileUrl}
      ontimeupdate={onAudioTime}
      onloadedmetadata={onAudioTime}
      class="w-full max-w-md pointer-events-auto"
    >
      Your browser doesn't support HTML5 audio.
    </audio>
    {#if meta.codec || meta.bitrate_kbps || meta.sample_rate_hz || meta.channels}
      <div class="text-xs text-white/50">
        {#if meta.codec}{meta.codec.toUpperCase()}{/if}
        {#if meta.bitrate_kbps} · {meta.bitrate_kbps} kbps{/if}
        {#if meta.sample_rate_hz} · {(meta.sample_rate_hz / 1000).toFixed(1)} kHz{/if}
        {#if meta.channels} · {meta.channels === 1 ? 'mono' : meta.channels === 2 ? 'stereo' : `${meta.channels}ch`}{/if}
      </div>
    {/if}
  </div>
</div>
