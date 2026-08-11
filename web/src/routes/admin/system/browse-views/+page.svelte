<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // #709 — which browse layouts this install offers.
  //
  // Checkboxes rather than a multi-select: five items, all visible at
  // once, and the invariant "at least one stays on" is legible when you
  // can see the whole set. The last enabled box is disabled in place so
  // the rule is visible BEFORE you try to break it — the server refuses
  // the empty set either way (that is the real gate), but a control
  // that lets you build a request the server will reject is a control
  // that taught you nothing.
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { site } from '$stores/site.svelte';
  import { t } from '$stores/lang.svelte';
  import { auth } from '$stores/auth.svelte';
  import { browseView, type ViewMode } from '$stores/browseView.svelte';

  // Order matches the store's VALID_MODES, which is the order the
  // server canonicalises to — so what you see here is the order the
  // switcher renders.
  const ALL_MODES: ViewMode[] = ['grid', 'masonry', 'thumbnail', 'list', 'feed'];

  let enabled = $state<ViewMode[]>([...ALL_MODES]);
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  const canWrite = $derived(auth.can('system.config.write'));
  const lastOne = $derived(enabled.length === 1);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/admin/system/browse-views');
      if (data?.enabled?.length) {
        enabled = data.enabled.filter((m): m is ViewMode =>
          (ALL_MODES as string[]).includes(m));
      }
    } finally {
      loading = false;
    }
  }

  function toggle(mode: ViewMode) {
    if (!enabled.includes(mode)) {
      enabled = ALL_MODES.filter((m) => m === mode || enabled.includes(m));
      return;
    }
    // Refuse to empty the set here as well as at the API. The button is
    // already disabled in this state; this is the guard for every other
    // way the handler can fire (keyboard, a label click, a future
    // "disable all" affordance nobody has written yet).
    if (lastOne) return;
    enabled = enabled.filter((m) => m !== mode);
  }

  async function save() {
    if (saving || enabled.length === 0) return;
    saving = true;
    saved = false;
    error = null;
    try {
      const { data, error: apiErr } = await api.PATCH('/admin/system/browse-views', {
        body: { enabled },
      });
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('errors.save_failed');
        return;
      }
      // Redraw from what was STORED, not from what we sent — the server
      // canonicalises order and drops duplicates.
      const stored = (data.enabled ?? []).filter((m): m is ViewMode =>
        (ALL_MODES as string[]).includes(m));
      if (stored.length > 0) {
        enabled = stored;
        // Apply to this session immediately so the admin's own switcher
        // and preferences dropdown agree with what they just saved,
        // without a reload. Same move lang.applyOverride makes after an
        // admin string write.
        browseView.setEnabledModes(stored);
      }
      saved = true;
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>{t('admin.system.browse_views.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.system.browse_views.title')}</h2>
<p class="mb-6 max-w-3xl text-sm text-fg-muted">{t('admin.system.browse_views.description')}</p>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else}
  <div class="max-w-3xl">
    <fieldset class="mb-4 space-y-2 rounded border border-border bg-surface p-4">
      <legend class="px-1 text-sm font-medium text-fg">
        {t('admin.system.browse_views.legend')}
      </legend>
      {#each ALL_MODES as mode (mode)}
        {@const on = enabled.includes(mode)}
        <label class="flex items-start gap-3 rounded px-1 py-1.5 text-sm">
          <input
            type="checkbox"
            class="mt-0.5 h-4 w-4"
            checked={on}
            disabled={!canWrite || saving || (on && lastOne)}
            onchange={() => toggle(mode)}
          />
          <span>
            <span class="block font-medium text-fg">{t(`browse.view.${mode}`)}</span>
            <span class="block text-xs text-fg-muted">
              {t(`admin.system.browse_views.mode_${mode}`)}
            </span>
          </span>
        </label>
      {/each}
      <p class="pt-1 text-xs text-fg-muted">{t('admin.system.browse_views.min_one')}</p>
    </fieldset>

    {#if error}
      <p class="mb-3 text-sm text-danger" role="alert">{error}</p>
    {/if}

    <div class="flex items-center gap-3">
      <button
        type="button"
        onclick={save}
        disabled={!canWrite || saving}
        class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-on-accent transition-colors hover:bg-accent/90 disabled:cursor-not-allowed disabled:opacity-50"
      >
        {saving ? t('common.saving') : t('common.save')}
      </button>
      {#if saved}
        <span class="text-sm text-success">{t('admin.system.browse_views.saved')}</span>
      {/if}
      {#if !canWrite}
        <span class="text-sm text-fg-muted">{t('admin.system.browse_views.read_only')}</span>
      {/if}
    </div>
  </div>
{/if}
