<script lang="ts">
  // /admin/system/themes — per-install brand & typography picker.
  //
  // Four slots (brand / display / sans / mono) bind to entries from
  // the curated font catalogue (web/src/lib/fonts/catalogue.ts).
  // Picking a value calls appearance.preview() which writes the
  // --font-* CSS vars on <html> immediately — the whole page
  // reflows in the new font so the admin can audit before saving.
  // "Save appearance" PATCHes /admin/system/appearance and persists
  // the picks across users + browsers + sessions.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import {
    appearance,
    fontsForSlot,
    FONTS,
    DEFAULT_BY_SLOT,
    type AppearancePicks,
  } from '$stores/appearance.svelte';
  import type { FontSlot } from '$lib/fonts/catalogue';

  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  // Local form state (controlled inputs). Mirrors appearance.picks
  // until Save commits the change.
  let picks = $state<AppearancePicks>({
    brand_font: '',
    display_font: '',
    body_font: '',
    mono_font: '',
  });

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/admin/system/appearance');
      if (data) {
        picks = {
          brand_font: data.brand_font ?? '',
          display_font: data.display_font ?? '',
          body_font: data.body_font ?? '',
          mono_font: data.mono_font ?? '',
        };
      }
    } finally {
      loading = false;
    }
  }

  // Slot pickers update the form and live-preview via the store. The
  // empty string means "use the default" — we render that as a real
  // option in the select.
  function setSlot(slot: FontSlot, id: string) {
    const key = `${slot === 'sans' ? 'body' : slot}_font` as keyof AppearancePicks;
    picks = { ...picks, [key]: id };
    appearance.preview({ [key]: id });
    saved = false;
  }

  async function save() {
    if (saving) return;
    saving = true;
    saved = false;
    error = null;
    try {
      await appearance.save(picks);
      saved = true;
    } catch (e) {
      error = (e as { error?: string }).error ?? t('errors.save_failed');
    } finally {
      saving = false;
    }
  }

  function resetToDefaults() {
    picks = { brand_font: '', display_font: '', body_font: '', mono_font: '' };
    appearance.preview(picks);
    saved = false;
  }

  // Helper: current pick for a slot, falling back to default.
  function currentId(slot: FontSlot): string {
    const key = `${slot === 'sans' ? 'body' : slot}_font` as keyof AppearancePicks;
    return picks[key] || DEFAULT_BY_SLOT[slot];
  }

  // Render a font family name in its own face — used for the preview
  // chip beside each picker so the admin sees the font before picking.
  function previewFamilyFor(id: string): string {
    const f = FONTS.find((x) => x.id === id);
    return f?.family ?? 'inherit';
  }

  const slots: Array<{ slot: FontSlot; titleKey: string; helpKey: string; sample: string }> = [
    { slot: 'brand',   titleKey: 'admin.system.themes.brand_label',   helpKey: 'admin.system.themes.brand_help',   sample: 'artist-alley' },
    { slot: 'display', titleKey: 'admin.system.themes.display_label', helpKey: 'admin.system.themes.display_help', sample: 'A heading in display' },
    { slot: 'sans',    titleKey: 'admin.system.themes.sans_label',    helpKey: 'admin.system.themes.sans_help',    sample: 'Body text and UI labels.' },
    { slot: 'mono',    titleKey: 'admin.system.themes.mono_label',    helpKey: 'admin.system.themes.mono_help',    sample: 'const x = 42;' },
  ];
</script>

<svelte:head><title>{t('admin.system.themes.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.system.themes.title')}</h2>
<p class="mb-6 text-sm text-fg-muted">{t('admin.system.themes.intro')}</p>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else}
  <form onsubmit={(e) => { e.preventDefault(); void save(); }} class="max-w-3xl space-y-4">
    {#each slots as s (s.slot)}
      {@const id = currentId(s.slot)}
      <section class="rounded-lg border border-border bg-surface-elevated p-4">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div class="min-w-0">
            <h3 class="text-sm font-medium text-fg">{t(s.titleKey)}</h3>
            <p class="mt-0.5 text-xs text-fg-muted">{t(s.helpKey)}</p>
          </div>
          <select
            value={picks[`${s.slot === 'sans' ? 'body' : s.slot}_font` as keyof AppearancePicks]}
            onchange={(e) => setSlot(s.slot, (e.currentTarget as HTMLSelectElement).value)}
            class="rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <option value="">{t('admin.system.themes.use_default')} ({DEFAULT_BY_SLOT[s.slot]})</option>
            {#each fontsForSlot(s.slot) as f (f.id)}
              <option value={f.id}>{f.label}</option>
            {/each}
          </select>
        </div>

        <!-- Live preview chip — renders the current pick in its own
             face. Steps down to bg-surface so it reads as a "well"
             inside the elevated card. -->
        <div
          class="mt-3 rounded-md border border-border-subtle bg-surface px-4 py-3 text-xl"
          style="font-family: {previewFamilyFor(id)}"
        >
          {s.sample}
        </div>
      </section>
    {/each}

    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container">{error}</p>
    {/if}
    {#if saved}
      <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-on-success-container">{t('admin.system.themes.saved')}</p>
    {/if}

    <div class="flex items-center gap-2">
      <button
        type="submit"
        disabled={saving}
        class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
      >
        {saving ? t('common.loading') : t('admin.system.themes.save')}
      </button>
      <button
        type="button"
        onclick={resetToDefaults}
        disabled={saving}
        class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm text-fg-muted hover:bg-state-hover hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
      >
        {t('admin.system.themes.reset')}
      </button>
    </div>
  </form>
{/if}
