<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // FontView — live specimen page rendered IN the uploaded typeface
  // via the browser FontFace API. Sections, top-to-bottom:
  //
  //   1. **Try it** — textarea + size slider + bg toggle; the right
  //      half live-renders whatever the user types in the font.
  //      The headline interaction; specimens below back it up.
  //   2. Display row — full name set big
  //   3. Pangram + a weight ladder
  //   4. ASCII grid — glyph coverage at a glance
  //   5. Metadata strip
  //
  // Compared to the col thumbnail (a single static specimen card
  // baked by preview.font), this view interactively renders the
  // actual font in the browser so it reflects whatever subpixel
  // hinting / antialiasing the user's display does.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';
  import { defaultController } from './controller';

  type Asset = import('./controller').ViewAsset;

  let { asset, controller = $bindable<ViewController>() }: {
    asset: Asset;
    controller: ViewController;
  } = $props();

  interface FontMeta {
    family?: string;
    subfamily?: string;
    full_name?: string;
    version?: string;
    copyright?: string;
    designer?: string;
    license?: string;
    num_glyphs?: number;
    units_per_em?: number;
  }
  const meta = $derived<FontMeta>(
    ((asset.metadata as Record<string, unknown> | null | undefined)?.font as FontMeta | undefined) ?? {},
  );

  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);

  // Each upload gets a unique family name so two open FontView
  // instances don't collide in document.fonts.
  const familyId = $derived(`aa-font-${asset.id}`);

  let loaded = $state(false);
  let loadError = $state<string | null>(null);
  let loadedFace: FontFace | null = null;

  // Display strings the specimen renders. Pulled out so the
  // template stays tidy and i18n can swap them later.
  const pangrams = [
    'The quick brown fox jumps over the lazy dog',
    'Pack my box with five dozen liquor jugs',
    'Sphinx of black quartz, judge my vow',
  ];
  const glyphGrid = [
    'ABCDEFGHIJKLMNOPQRSTUVWXYZ',
    'abcdefghijklmnopqrstuvwxyz',
    '0123456789',
    '!@#$%^&*()-_=+[]{}<>?/',
  ];

  // ── "Try it" interactive state ──────────────────────────────────
  // User types on the left, the right side renders in the loaded
  // typeface. Size + bg toggle + a few preset strings give a quick
  // way to evaluate the font without typing from scratch.
  const TRY_IT_PRESETS: { label: string; text: string }[] = [
    { label: 'Display',  text: 'Hamburgefonstiv' },
    { label: 'Pangram',  text: 'The quick brown fox jumps over the lazy dog' },
    { label: 'Numerals', text: '0123456789 \u00a3$\u20ac\u00a5 +-\u00d7\u00f7 (12.5%)' },
    { label: 'Lorem',    text: 'Lorem ipsum dolor sit amet, consectetur adipiscing elit. Sed do eiusmod tempor incididunt ut labore et dolore magna aliqua.' },
  ];
  let tryText = $state('Hamburgefonstiv');
  let trySize = $state(96);
  let tryBg = $state<'dark' | 'light'>('dark');
  // Reset textarea on asset change (otherwise typing on font A is
  // still visible when navigating to font B).
  let lastAssetId = '';
  $effect(() => {
    if (asset.id !== lastAssetId) {
      lastAssetId = asset.id;
      tryText = 'Hamburgefonstiv';
      trySize = 96;
    }
  });

  onMount(async () => {
    try {
      const face = new FontFace(familyId, `url(${fileUrl})`);
      await face.load();
      document.fonts.add(face);
      loadedFace = face;
      loaded = true;
      controller = {
        ...defaultController(),
        hudExtra: [
          meta.full_name || meta.family || asset.title || '',
          meta.num_glyphs ? `${meta.num_glyphs} glyphs` : '',
        ].filter(Boolean).join(' · '),
      };
    } catch (e) {
      loadError = e instanceof Error ? e.message : 'Failed to load font.';
    }
  });

  onDestroy(() => {
    if (loadedFace) {
      try { document.fonts.delete(loadedFace); } catch { /* ignore */ }
    }
  });

  const displayTitle = $derived(meta.full_name || meta.family || asset.title || '(font)');
  const subTitle = $derived(
    [meta.subfamily, asset.file_extension?.toUpperCase()].filter(Boolean).join(' · '),
  );
</script>

<div class="h-full w-full overflow-y-auto bg-[#16181f] px-6 py-6 text-[#e6e7ec] lg:px-10 lg:py-8">
  {#if loadError}
    <div class="mx-auto max-w-3xl text-center">
      <p class="text-sm text-danger">Couldn't load font</p>
      <p class="mt-1 font-mono text-xs text-white/40">{loadError}</p>
      <a
        href={fileUrl}
        download
        class="mt-3 inline-block text-xs text-accent hover:underline"
      >Download original</a>
    </div>
  {:else if !loaded}
    <div class="flex h-full items-center justify-center text-sm text-white/40">
      Loading font…
    </div>
  {:else}
    <div class="space-y-8">
      <!-- ── Try it — interactive specimen ─────────────────────────
           Left half: textarea + size + bg toggle + preset strings.
           Right half: the typed text rendered in the loaded font,
           scrollable if the user picks a huge size + long string.
           Collapses to a stack on narrow viewports. -->
      <section class="rounded-lg border border-white/10 bg-white/5 p-4">
        <div class="mb-3 flex items-center justify-between gap-2">
          <h2 class="text-xs uppercase tracking-wider text-white/40">Try it</h2>
          <div class="flex items-center gap-1">
            {#each TRY_IT_PRESETS as p (p.label)}
              <button
                type="button"
                onclick={() => (tryText = p.text)}
                class="rounded border border-white/15 px-2 py-0.5 text-[10px] uppercase tracking-wide text-white/60 hover:border-white/30 hover:text-white"
                title={`Load: ${p.text.slice(0, 60)}${p.text.length > 60 ? '\u2026' : ''}`}
              >{p.label}</button>
            {/each}
          </div>
        </div>
        <div class="grid gap-3 lg:grid-cols-2">
          <!-- Editor side -->
          <div class="flex flex-col gap-2">
            <textarea
              bind:value={tryText}
              rows="6"
              spellcheck="false"
              class="min-h-[8rem] w-full resize-y rounded border border-white/10 bg-[#0f1117] px-3 py-2 font-mono text-sm text-white/90 focus:border-accent focus:outline-none"
              placeholder="Type something to see it in the font\u2026"
            ></textarea>
            <div class="flex items-center gap-3 text-xs text-white/60">
              <label class="flex flex-1 items-center gap-2">
                <span class="w-10 text-white/40">Size</span>
                <input
                  type="range"
                  min="12"
                  max="240"
                  step="1"
                  bind:value={trySize}
                  class="flex-1 accent-accent"
                />
                <span class="w-10 text-right font-mono text-white/70">{trySize}px</span>
              </label>
              <div class="flex items-center overflow-hidden rounded border border-white/15">
                <button
                  type="button"
                  onclick={() => (tryBg = 'dark')}
                  class={`px-2 py-1 text-[10px] uppercase tracking-wide ${tryBg === 'dark' ? 'bg-white/10 text-white' : 'text-white/50 hover:text-white/80'}`}
                  title="Dark background"
                >Dark</button>
                <button
                  type="button"
                  onclick={() => (tryBg = 'light')}
                  class={`px-2 py-1 text-[10px] uppercase tracking-wide ${tryBg === 'light' ? 'bg-white/10 text-white' : 'text-white/50 hover:text-white/80'}`}
                  title="Light background"
                >Light</button>
              </div>
            </div>
          </div>
          <!-- Render side -->
          <div
            class={`min-h-[8rem] overflow-auto rounded border p-4 ${tryBg === 'dark' ? 'border-white/10 bg-[#0f1117] text-white/90' : 'border-black/10 bg-white text-black'}`}
          >
            <div
              class="whitespace-pre-wrap break-words leading-tight"
              style:font-family={`"${familyId}", sans-serif`}
              style:font-size={`${trySize}px`}
            >{tryText || '\u00a0'}</div>
          </div>
        </div>
      </section>

      <!-- Display row -->
      <header class="border-b border-white/10 pb-6">
        <h1
          class="text-5xl leading-tight md:text-6xl"
          style:font-family={`"${familyId}", sans-serif`}
        >{displayTitle}</h1>
        {#if subTitle}
          <p class="mt-2 text-sm text-white/40">{subTitle}</p>
        {/if}
      </header>

      <!-- Pangrams at descending sizes for a weight/size feel -->
      <section class="space-y-4">
        {#each pangrams as p, idx}
          <p
            class:text-2xl={idx === 0}
            class:text-xl={idx === 1}
            class:text-base={idx === 2}
            class="leading-snug text-white/85"
            style:font-family={`"${familyId}", sans-serif`}
          >{p}</p>
        {/each}
      </section>

      <!-- ASCII grid: visible coverage at a glance -->
      <section class="space-y-2">
        <h2 class="text-xs uppercase tracking-wider text-white/40">Glyph coverage</h2>
        {#each glyphGrid as row}
          <p
            class="break-all text-2xl tracking-wide text-white/85"
            style:font-family={`"${familyId}", sans-serif`}
          >{row}</p>
        {/each}
      </section>

      <!-- Metadata strip -->
      {#if meta.designer || meta.version || meta.copyright || meta.license}
        <section class="border-t border-white/10 pt-6">
          <h2 class="text-xs uppercase tracking-wider text-white/40">Metadata</h2>
          <dl class="mt-3 grid grid-cols-1 gap-2 text-xs sm:grid-cols-2">
            {#if meta.designer}
              <div><dt class="text-white/40">Designer</dt><dd>{meta.designer}</dd></div>
            {/if}
            {#if meta.version}
              <div><dt class="text-white/40">Version</dt><dd class="font-mono">{meta.version}</dd></div>
            {/if}
            {#if meta.units_per_em}
              <div><dt class="text-white/40">Units / em</dt><dd class="font-mono">{meta.units_per_em}</dd></div>
            {/if}
            {#if meta.num_glyphs}
              <div><dt class="text-white/40">Glyphs</dt><dd class="font-mono">{meta.num_glyphs}</dd></div>
            {/if}
            {#if meta.copyright}
              <div class="sm:col-span-2"><dt class="text-white/40">Copyright</dt><dd>{meta.copyright}</dd></div>
            {/if}
            {#if meta.license}
              <div class="sm:col-span-2"><dt class="text-white/40">License</dt><dd class="whitespace-pre-line">{meta.license}</dd></div>
            {/if}
          </dl>
        </section>
      {/if}
    </div>
  {/if}
</div>
