<script lang="ts">
  // FontView — live specimen page rendered IN the uploaded typeface
  // via the browser FontFace API. Three sections:
  //
  //   1. Display row — full name at 96 pt
  //   2. Pangram + a weight ladder
  //   3. ASCII grid — visible glyph table for quick coverage check
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

<div class="h-full w-full overflow-y-auto bg-[#16181f] px-8 py-10 text-[#e6e7ec]">
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
    <div class="mx-auto max-w-4xl space-y-10">
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
