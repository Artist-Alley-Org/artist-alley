<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Placeholder body for asset kinds whose dedicated viewer hasn't
  // landed yet (PDF, audio, 3D, fonts, unknown). Shows a kind-
  // specific icon + a "preview pipeline lands in Phase X" note so
  // operators know the shape isn't broken — just not built yet.
  //
  // The shell still wraps this in the AssetViewer chrome; the user
  // can still fullscreen, click through to /file, or open the
  // original. Once a dedicated view ships for a kind, this body
  // stops being mounted for it.

  import { onMount } from 'svelte';
  import type { ViewController, ViewKind } from './controller';
  import { kindForExtension } from './controller';

  type Asset = import('./controller').ViewAsset;

  interface Props {
    asset: Asset;
    controller: ViewController;
  }

  let { asset, controller = $bindable() }: Props = $props();
  const kind: ViewKind = $derived(kindForExtension(asset.file_extension));
  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);

  onMount(() => {
    controller.kind = kind;
    controller.hasTimeline = false;
    controller.totalFrames = 0;
    controller.fps = 0;
    controller.duration = 0;
    controller.playing = false;
    controller.spritesUrl = null;
    controller.spritesVttUrl = null;
    controller.formatAnchor = () => '';
    controller.hudExtra = (asset.file_extension || '').toUpperCase();
    controller.play = () => {};
    controller.pause = () => {};
    controller.togglePlay = () => {};
    controller.seekToFrame = () => {};
    controller.stepFrames = () => {};
    controller.setRate = () => {};
  });

  // `planned` gates the "coming soon" line: true where a dedicated
  // viewer is on the roadmap but not yet built. The specific release is a
  // dev-side detail and is deliberately not surfaced to viewers (#801).
  const labels: Record<ViewKind, { title: string; planned: boolean; icon: string }> = {
    pdf: { title: 'PDF preview', planned: true, icon: 'file-text' },
    audio: { title: 'Audio waveform', planned: true, icon: 'audio' },
    font: { title: 'Font specimen', planned: true, icon: 'placeholder' },
    sprite: { title: 'Sprite viewer', planned: false, icon: 'placeholder' },
    '3d': { title: '3D viewer', planned: true, icon: 'cube' },
    sequence: { title: 'Image sequence', planned: true, icon: 'film' },
    placeholder: { title: 'Preview', planned: false, icon: 'placeholder' },
    image: { title: 'Image', planned: false, icon: 'placeholder' },
    video: { title: 'Video', planned: false, icon: 'placeholder' },
    ebook: { title: 'Ebook reader', planned: false, icon: 'file-text' },
    doc: { title: 'Document viewer', planned: false, icon: 'file-text' },
    audiobook: { title: 'Audiobook reader', planned: false, icon: 'audio' },
    archive: { title: 'Archive viewer', planned: false, icon: 'placeholder' },
  };
  const label = $derived(labels[kind]);
</script>

<div class="flex h-full w-full flex-col items-center justify-center gap-3 px-8 text-center text-fg-muted">
  <svg xmlns="http://www.w3.org/2000/svg" width="72" height="72" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round">
    {#if label.icon === 'file-text'}
      <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
      <polyline points="14 2 14 8 20 8" />
      <line x1="9" y1="13" x2="15" y2="13" />
      <line x1="9" y1="17" x2="15" y2="17" />
    {:else if label.icon === 'audio'}
      <line x1="2" y1="12" x2="6" y2="12" />
      <line x1="6" y1="8" x2="6" y2="16" />
      <line x1="10" y1="4" x2="10" y2="20" />
      <line x1="14" y1="9" x2="14" y2="15" />
      <line x1="18" y1="11" x2="18" y2="13" />
      <line x1="22" y1="12" x2="22" y2="12" />
    {:else if label.icon === 'cube'}
      <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" />
      <polyline points="3.27 6.96 12 12.01 20.73 6.96" />
      <line x1="12" y1="22.08" x2="12" y2="12" />
    {:else if label.icon === 'film'}
      <rect x="2" y="2" width="20" height="20" rx="2.18" />
      <line x1="7" y1="2" x2="7" y2="22" />
      <line x1="17" y1="2" x2="17" y2="22" />
      <line x1="2" y1="12" x2="22" y2="12" />
    {:else}
      <rect x="3" y="3" width="18" height="18" rx="2" />
      <circle cx="9" cy="9" r="2" />
      <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
    {/if}
  </svg>
  <div>
    <p class="text-sm font-medium text-fg">{label.title}</p>
    {#if label.planned}
      <p class="mt-0.5 text-xs">A dedicated viewer is coming in a future release.</p>
    {/if}
    <!-- #899 — an asset whose columns were withheld has no readable
         bytes either; offering the link invites a 404. -->
    {#if !asset.restricted}
      <a href={fileUrl} class="mt-3 inline-block text-xs text-accent underline" target="_blank">Download original</a>
    {/if}
  </div>
</div>
