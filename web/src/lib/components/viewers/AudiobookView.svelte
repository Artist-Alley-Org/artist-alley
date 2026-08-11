<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Audiobookshelf-style audiobook player. Replaces MediaView for
  // the audiobook kind so the surface can lean into the right
  // affordances: prominent cover art, chapter strip on the
  // scrubber, big circular skip buttons (±10s / ±30s by default —
  // configurable in the side-panel tool), inline speed selector,
  // and a resume-where-I-was-with-auto-rewind behaviour.
  //
  // All state lives on the shared AudiobookSession the side-panel
  // AudiobookTool also binds; both ends mutate the same $state and
  // the renderer reacts via $effects.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';
  import type {
    AudiobookSessionInstance,
    AudiobookChapter,
  } from '$lib/audiobook/session.svelte';
  import {
    fmtClock,
    persistAudiobookResume,
  } from '$lib/audiobook/session.svelte';

  type Asset = import('./controller').ViewAsset;

  interface Props {
    asset: Asset;
    controller: ViewController;
    session: AudiobookSessionInstance;
  }
  let { asset, controller = $bindable(), session = $bindable<AudiobookSessionInstance>() }: Props = $props();

  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);
  const ext = $derived((asset.file_extension || '').toLowerCase().replace(/^\./, ''));

  // sessionStorage key for the sibling-jump autoplay handoff. When
  // the user clicks a track in the panel (or auto-advance fires at
  // end-of-audio), the OUTGOING view stamps this with the incoming
  // asset id; the INCOMING view checks it on loaded-metadata and
  // calls play() so the audiobook reader keeps going without
  // requiring a manual play press on every track.
  const SIBLING_AUTOPLAY_KEY = 'aa.audiobook.autoplay.next';

  let audioEl: HTMLAudioElement | undefined = $state();

  // Audio source: for .m4b we send Content-Type audio/mp4 from the
  // backend; browser plays AAC inside MP4 natively. .aax falls back
  // to a friendly "needs Audible decryption" message since we
  // don't ship activation-bytes handling.
  const srcType = $derived(
    ext === 'm4b' ? 'audio/mp4' :
    ext === 'm4a' ? 'audio/mp4' :
    ext === 'mp3' ? 'audio/mpeg' :
    '',
  );
  const isPlayable = $derived(ext !== 'aax');

  // Cover URL — backend extracts an embedded album-art "cover"
  // variant during preview. Falls back to a generic book icon when
  // the asset hasn't been processed yet (worker is async).
  const coverUrl = $derived(`/api/v1/assets/${asset.id}/variants/cover`);

  // Pull metadata from asset.metadata.audio (the preview pipeline
  // stamps it there). Title / author / narrator come from ID3 or
  // MP4 atom tags; chapters come from the new -show_chapters probe
  // in preview/audio.go.
  interface AudioMetaWire {
    duration_s?: number;
    chapters?: { id: number; start_s: number; end_s: number; title?: string }[];
    chapter_source?: string;
    tags?: Record<string, string>;
    has_cover?: boolean;
    album?: {
      title?: string;
      artist?: string;
      album_artist?: string;
      genre?: string;
      year?: string;
      summary?: string;
      runtime_s?: number;
      mb_album_id?: string;
      tracks?: { position: number; title?: string; duration_s?: number }[];
    };
  }
  interface SiblingWire {
    asset_id: string;
    /** ABSENT on a member the viewer may not see (#883). */
    asset?: { id: string; title?: string };
    restricted?: boolean;
    sort_order?: number;
  }
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const audioMeta = $derived<AudioMetaWire>((asset.metadata?.audio as any) ?? {});

  // Chapter index for the current time. Updates live as playback
  // advances. -1 when the audio has no chapters at all (we still
  // render the controls; just hide the chapter strip).
  const currentChapterIdx = $derived.by<number>(() => {
    const t = session.currentTime;
    for (let i = 0; i < session.chapters.length; i++) {
      const c = session.chapters[i];
      if (t >= c.start && t < c.end) return i;
    }
    return session.chapters.length - 1;
  });
  const currentChapter = $derived<AudiobookChapter | null>(
    currentChapterIdx >= 0 ? session.chapters[currentChapterIdx] ?? null : null,
  );

  // Time remaining at the current speed (for the side-panel ETA).
  const remainingAtSpeed = $derived(
    Math.max(0, (session.durationS - session.currentTime) / Math.max(0.1, session.speed)),
  );

  // ── Lifecycle ──────────────────────────────────────────────
  let resumePersistTimer: ReturnType<typeof setTimeout> | null = null;
  let sleepInterval: ReturnType<typeof setInterval> | null = null;
  let endOfChapterArm = $state(false);

  onMount(() => {
    controller.kind = 'audiobook';
    controller.hasTimeline = true;
    controller.fps = 1000; // audio convention — 1 frame = 1 ms
    controller.formatAnchor = (frame) => fmtClock(frame / 1000);
    controller.hudExtra = ext.toUpperCase();
    controller.spritesUrl = null;
    controller.spritesVttUrl = null;

    // Wire transport functions through to <audio>. The shell's
    // transport bar uses these (Play/Pause/Step etc.) so users have
    // both the in-canvas controls and the bottom rail.
    controller.play = () => { audioEl?.play().catch(() => {}); };
    controller.pause = () => { audioEl?.pause(); };
    controller.togglePlay = () => {
      if (!audioEl) return;
      if (audioEl.paused) controller.play();
      else controller.pause();
    };
    controller.seekToFrame = (frame) => {
      if (!audioEl) return;
      audioEl.currentTime = frame / 1000;
    };
    controller.stepFrames = (n) => {
      // The shell ships ±1 / ±10 step buttons + ,/. and Shift+,/.
      // hotkeys for every timeline kind. For audiobook those frame
      // deltas are useless (1 frame = 1 ms), so we re-map them to
      // the user's configured skip-back / skip-fwd intervals:
      //   ±1   → ±skipBackS / ±skipFwdS  (button: −/+10s default)
      //   ±10  → ±30s              (Shift+button: bigger jump)
      // The shell label still reads "−10 / +10" but the action
      // matches Audiobookshelf muscle memory.
      if (!audioEl) return;
      const mag = Math.abs(n);
      const dir = n < 0 ? -1 : 1;
      let deltaS = 0;
      if (mag === 1)       deltaS = dir < 0 ? -session.skipBackS : session.skipFwdS;
      else if (mag === 10) deltaS = dir < 0 ? -30 : 30;
      else                 deltaS = n; // fall back to seconds for any other delta
      audioEl.currentTime = Math.max(0, audioEl.currentTime + deltaS);
    };
    controller.setRate = (r) => {
      session.setSpeed(r);
    };

    // Publish view-published callbacks so the panel + selection
    // toolbar can drive the player without reaching for audioEl.
    session.seekTo = (s) => { if (audioEl) audioEl.currentTime = s; };
    session.togglePlay = () => controller.togglePlay();
    session.skipRelative = (delta) => {
      if (!audioEl) return;
      audioEl.currentTime = Math.max(0, Math.min(session.durationS || audioEl.duration || 0, audioEl.currentTime + delta));
    };
    session.goToChapter = (idx) => {
      const c = session.chapters[idx];
      if (c && audioEl) audioEl.currentTime = c.start;
    };
    session.goToSibling = (assetId) => {
      // Stash a handoff token in sessionStorage so the next track's
      // AudiobookView mount knows to auto-play (continuous-listen
      // expectation — clicking a track or hitting end-of-audio
      // should pour straight into the next file without the user
      // having to hit play again).
      try { sessionStorage.setItem(SIBLING_AUTOPLAY_KEY, assetId); } catch { /* ignore */ }
      // Custom event the AssetPlaylist host listens for to mutate
      // its cursor. Falls back to a URL hop when no listener
      // intercepts (e.g. a future standalone /assets/{id} route).
      const ev = new CustomEvent('aa-audiobook-advance', {
        detail: { assetId },
        bubbles: true,
        cancelable: true,
      });
      const accepted = !window.dispatchEvent(ev) || ev.defaultPrevented;
      if (!accepted) {
        const u = new URL(window.location.href);
        u.searchParams.set('asset', assetId);
        window.history.replaceState({}, '', u.toString());
      }
    };

    // Pour any prop-side metadata in first (rare — most hosts pass
    // thin asset summaries without `metadata`), then go fetch the
    // full asset row so we get chapter + tags + has_cover.
    applyAudioMeta(audioMeta);
    void fetchFullMetadata();
    void fetchSiblings();
  });

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  function applyAudioMeta(m: any) {
    if (!m) return;
    session.durationS = m.duration_s ?? session.durationS;
    if (Array.isArray(m.chapters) && m.chapters.length > 0) {
      session.chapters = m.chapters.map((c: { id: number; start_s: number; end_s: number; title?: string }) => ({
        id: c.id,
        start: c.start_s,
        end: c.end_s,
        title: c.title ?? '',
      }));
    }
    if (typeof m.chapter_source === 'string') session.chapterSource = m.chapter_source;
    const tags: Record<string, string> = m.tags ?? {};
    // Album-level info from a .nfo companion wins over per-file
    // tags — it's the canonical "this is what this audiobook is"
    // pulled from a curated metadata source. We still fall back
    // through the ID3 chain for files without a companion.
    const al: AudioMetaWire['album'] = m.album;
    if (al) {
      session.album = {
        title: al.title ?? '',
        artist: al.artist ?? '',
        albumArtist: al.album_artist ?? '',
        genre: al.genre ?? '',
        year: al.year ?? '',
        summary: al.summary ?? '',
        runtimeS: al.runtime_s ?? 0,
        mbAlbumId: al.mb_album_id ?? '',
        tracks: (al.tracks ?? []).map((t) => ({
          position: t.position,
          title: t.title ?? '',
          durationS: t.duration_s ?? 0,
        })),
      };
    }
    session.title = (session.album?.title)
      || tags['title'] || tags['album']
      || asset.title || '';
    session.author = (session.album?.albumArtist || session.album?.artist)
      || tags['artist'] || tags['album_artist'] || tags['author']
      || '';
    session.narrator = tags['composer'] || tags['narrator'] || '';
    session.coverUrl = m.has_cover ? coverUrl : null;
  }

  async function fetchFullMetadata() {
    try {
      const r = await fetch(`/api/v1/assets/${asset.id}`, { credentials: 'include' });
      if (!r.ok) return;
      const full = await r.json();
      const m = (full.metadata && full.metadata.audio) || null;
      if (m) applyAudioMeta(m);
    } catch { /* ignore — keep whatever we had from prop / browser */ }
  }

  // Sibling-track discovery — when the URL carries ?post=ID and the
  // post has multiple audio members (Dark Tower style: one MP3 per
  // disk), populate session.siblings so the panel + autoplay-on-end
  // can hop between tracks.
  async function fetchSiblings() {
    try {
      const params = new URLSearchParams(window.location.search);
      const postId = params.get('post');
      if (!postId) return;
      const r = await fetch(`/api/v1/posts/${postId}`, { credentials: 'include' });
      if (!r.ok) return;
      const post = await r.json();
      const members: SiblingWire[] = post.members ?? [];
      // Treat all post members as a play queue when the host post
      // has > 1 member. For per-member kind gating we'd need
      // asset_type but the post member shape doesn't carry it
      // reliably; the audio kind filter that gated AudiobookView's
      // mount is good enough.
      // #883 — a restricted member is DROPPED from the play queue rather
      // than shown as a locked track. This queue exists to autoplay
      // through the post; a position that can never play would stall it
      // on every pass, and there is no track title to name it by anyway.
      // The tile/filmstrip surfaces still show the placeholder, so the
      // restriction is visible where a person can act on it.
      const sorted = [...members]
        .filter((m) => !m.restricted)
        .sort((a, b) => (a.sort_order ?? 0) - (b.sort_order ?? 0));
      if (sorted.length < 2) return;
      session.siblings = sorted.map((m, i) => ({
        assetId: m.asset?.id ?? m.asset_id,
        title: (m.asset?.title ?? '').trim() || `Track ${i + 1}`,
        position: i + 1,
      }));
      session.currentSiblingIndex = session.siblings.findIndex((s) => s.assetId === asset.id);
    } catch { /* ignore — single-file audiobook fallback is fine */ }
  }

  onDestroy(() => {
    if (resumePersistTimer) clearTimeout(resumePersistTimer);
    if (sleepInterval) clearInterval(sleepInterval);
    session.seekTo = undefined;
    session.togglePlay = undefined;
    session.skipRelative = undefined;
    session.goToChapter = undefined;
    session.goToSibling = undefined;
  });

  // ── Audio event handlers ───────────────────────────────────
  function onLoadedMetadata() {
    if (!audioEl) return;
    session.durationS = audioEl.duration || session.durationS;
    controller.duration = session.durationS;
    controller.totalFrames = Math.round(session.durationS * 1000);
    audioEl.playbackRate = session.speed;
    // Restore from persisted resume position — auto-rewind a few
    // seconds so the user doesn't drop straight into the middle of
    // a sentence.
    const restore = Math.max(0, session.resumePos - session.autoRewindS);
    if (restore > 0 && restore < session.durationS - 1) {
      audioEl.currentTime = restore;
    }
    session.loading = false;

    // If we arrived here from a sibling jump (user clicked the next
    // track or auto-advance fired), the outgoing view stamped our
    // asset id in sessionStorage; auto-play so the listen is
    // continuous. Clear the flag immediately so a page reload
    // doesn't autoplay surprise-style.
    try {
      const want = sessionStorage.getItem(SIBLING_AUTOPLAY_KEY);
      if (want === asset.id) {
        sessionStorage.removeItem(SIBLING_AUTOPLAY_KEY);
        audioEl.play().catch(() => { /* user-gesture block — fine */ });
      }
    } catch { /* private mode — ignore */ }
  }

  function onTimeUpdate() {
    if (!audioEl) return;
    session.currentTime = audioEl.currentTime;
    controller.currentFrame = Math.round(audioEl.currentTime * 1000);
    // Throttle persistence so we don't write storage every tick.
    if (!resumePersistTimer) {
      resumePersistTimer = setTimeout(() => {
        resumePersistTimer = null;
        persistAudiobookResume(asset.id, session.currentTime);
        session.resumePos = session.currentTime;
      }, 5000);
    }
    // End-of-chapter sleep timer.
    if (endOfChapterArm && currentChapter && audioEl.currentTime >= currentChapter.end - 0.2) {
      endOfChapterArm = false;
      session.cancelSleepTimer();
      audioEl.pause();
    }
  }

  function onPlay() {
    session.playing = true;
    controller.playing = true;
    // Sleep timer start when in numeric mode.
    if (session.sleepTimer !== 'off' && session.sleepTimer !== 'end-of-chapter' && !sleepInterval) {
      sleepInterval = setInterval(() => {
        if (session.sleepRemaining == null) return;
        session.sleepRemaining = Math.max(0, session.sleepRemaining - 1);
        if (session.sleepRemaining <= 0) {
          audioEl?.pause();
          session.cancelSleepTimer();
        }
      }, 1000);
    }
    if (session.sleepTimer === 'end-of-chapter') endOfChapterArm = true;
  }
  function onPause() {
    session.playing = false;
    controller.playing = false;
    if (sleepInterval) { clearInterval(sleepInterval); sleepInterval = null; }
    // Sync resume position immediately on pause — the throttle
    // could otherwise miss the user closing the tab.
    persistAudiobookResume(asset.id, session.currentTime);
    session.resumePos = session.currentTime;
  }
  function onError() {
    session.loadError = audioEl?.error
      ? `Audio error (${audioEl.error.code})`
      : 'Audio failed to load';
    session.loading = false;
  }
  function onEnded() {
    // Multi-file auto-advance — when the user has set autoAdvance
    // and the current asset is a member of a larger playlist (e.g.
    // Dark Tower's 6-MP3 set), jump to the next member. The host
    // playlist (AssetPlaylist) listens for aa-audiobook-advance
    // and bumps its cursor.
    if (!session.autoAdvance) return;
    const next = session.currentSiblingIndex + 1;
    if (next < 0 || next >= session.siblings.length) return;
    const nextId = session.siblings[next].assetId;
    if (nextId) session.goToSibling?.(nextId);
  }

  // Apply speed live when the session changes.
  $effect(() => {
    if (audioEl) audioEl.playbackRate = session.speed;
  });

  // Sleep-timer mode changes need to (re)arm correctly.
  $effect(() => {
    void session.sleepTimer;
    if (session.sleepTimer === 'off' || session.sleepTimer === 'end-of-chapter') {
      if (sleepInterval) { clearInterval(sleepInterval); sleepInterval = null; }
    }
    if (session.sleepTimer === 'end-of-chapter') {
      endOfChapterArm = session.playing;
    } else {
      endOfChapterArm = false;
    }
  });

  // The in-canvas scrubber + bookmark marks were retired — the
  // AssetViewer shell's bottom rail is the sole transport surface
  // now. Chapter ticks live there too (see AssetViewer.svelte's
  // scrubber when kind === 'audiobook'), and bookmark jump comes
  // from the side-panel AudiobookTool list.
</script>

<div class="relative flex h-full w-full flex-col overflow-hidden bg-gradient-to-b from-zinc-950 to-black text-white">
  <!-- Using <source> inside <audio> so the browser sees the
       explicit MIME — bare <audio src> can mis-guess m4b as
       octet-stream and refuse to decode. -->
  <audio
    bind:this={audioEl}
    preload="metadata"
    onloadedmetadata={onLoadedMetadata}
    ontimeupdate={onTimeUpdate}
    onplay={onPlay}
    onpause={onPause}
    onerror={onError}
    onended={onEnded}
  >
    <source src={fileUrl} type={srcType} />
  </audio>

  <div class="flex flex-1 flex-col items-center justify-center gap-6 px-6 py-8">
    <!-- Cover art — large, centered. Falls back to a styled book
         icon when no embedded art is available yet. -->
    <div class="relative h-56 w-56 shrink-0 overflow-hidden rounded-md border border-white/10 shadow-2xl md:h-72 md:w-72">
      {#if session.coverUrl}
        <img
          src={session.coverUrl}
          alt={session.title || 'Audiobook cover'}
          class="absolute inset-0 h-full w-full object-cover"
          onerror={() => { session.coverUrl = null; }}
        />
      {:else}
        <div class="absolute inset-0 flex items-center justify-center bg-gradient-to-br from-zinc-700 to-zinc-900">
          <svg xmlns="http://www.w3.org/2000/svg" width="96" height="96" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round" class="text-white/40">
            <path d="M2 3h6a4 4 0 0 1 4 4v14a3 3 0 0 0-3-3H2z" />
            <path d="M22 3h-6a4 4 0 0 0-4 4v14a3 3 0 0 1 3-3h7z" />
          </svg>
        </div>
      {/if}
    </div>

    <!-- Title + author / narrator strip -->
    <div class="flex w-full max-w-xl flex-col items-center text-center">
      <div class="truncate text-lg font-medium md:text-xl">
        {session.title || asset.title || 'Untitled audiobook'}
      </div>
      {#if session.author || session.narrator}
        <div class="mt-1 text-xs text-white/60">
          {#if session.author}<span>by {session.author}</span>{/if}
          {#if session.author && session.narrator} · {/if}
          {#if session.narrator}<span>narrated by {session.narrator}</span>{/if}
        </div>
      {/if}
      {#if currentChapter}
        <div class="mt-2 inline-flex items-center gap-1 rounded-full bg-white/10 px-3 py-0.5 text-[11px] font-medium text-white/85">
          <span>Ch&nbsp;{currentChapterIdx + 1}/{session.chapters.length}</span>
          <span class="text-white/40">·</span>
          <span class="truncate max-w-xs">{currentChapter.title || `Chapter ${currentChapterIdx + 1}`}</span>
        </div>
      {/if}
    </div>

    <!-- Sleep-timer indicator + remaining-at-speed pill. The actual
         transport (play / pause / skip / speed) lives in the
         AssetViewer shell's bottom rail so one set of controls
         covers video, audio, and audiobook. For audiobook we
         override controller.stepFrames so the shell's ±1 / ±10
         buttons + hotkeys land as Audiobookshelf-style ±10s / ±30s
         skips instead of useless ±1ms / ±10ms frame steps. -->
    <div class="flex flex-wrap items-center justify-center gap-2 text-xs text-white/70">
      {#if session.sleepTimer !== 'off'}
        <div class="flex items-center gap-1 rounded-full bg-white/10 px-2.5 py-0.5 text-yellow-300">
          <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>
          {#if session.sleepRemaining != null}
            <span class="font-mono">{fmtClock(session.sleepRemaining)}</span>
          {:else}
            <span>end of chapter</span>
          {/if}
          <button
            type="button"
            onclick={() => session.cancelSleepTimer()}
            class="ml-1 text-yellow-300/70 hover:text-yellow-100"
            aria-label="Cancel sleep timer"
          >×</button>
        </div>
      {/if}
      <div class="font-mono text-white/50">
        ≈ {fmtClock(remainingAtSpeed)} remaining
        {#if Math.abs(session.speed - 1) > 0.01}
          <span class="ml-1 rounded bg-white/10 px-1 text-[10px]">{session.speed.toFixed(2)}×</span>
        {/if}
      </div>
    </div>
  </div>

  {#if !isPlayable}
    <div class="pointer-events-none absolute inset-0 flex items-center justify-center bg-black/60 text-sm text-white">
      <div class="rounded-md border border-white/10 bg-black/80 p-4 text-center">
        <p class="font-medium">.aax can't play in the browser</p>
        <p class="mt-1 text-xs text-white/60">
          Audible's encryption needs per-account activation bytes —
          convert to .m4b first (ffmpeg or AAXtoMP3).
        </p>
      </div>
    </div>
  {/if}

  {#if session.loadError}
    <div class="pointer-events-none absolute inset-0 flex items-center justify-center bg-black/70 text-sm text-red-300">
      <p>{session.loadError}</p>
    </div>
  {/if}
</div>
