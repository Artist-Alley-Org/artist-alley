<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<!--
  Instance-logo card for /admin/system/themes (#517).

  Upload a mark, or pick one back up from the last 5. The default tile
  is always first and always selectable — it ships in the bundle, so it
  is the one option that cannot fail, which is exactly why it is the
  guaranteed way back out of a bad state.

  Entries whose image data has gone missing render as explicitly
  unavailable rather than as a broken thumbnail, and cannot be
  selected. The list exists so an operator can recover a logo they no
  longer have the file for, so "this one is gone" has to be legible.

  The upload uses raw fetch rather than the typed client: the body is
  an image, and openapi-fetch serialises JSON.
-->
<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { appearance } from '$stores/appearance.svelte';
  import type { components } from '$api/schema';

  type InstanceLogo = components['schemas']['InstanceLogo'];
  type AppearanceConfig = components['schemas']['AppearanceConfig'];

  // Mirrors the server's limits so an obviously-bad file is refused
  // before it crosses the network. The server re-checks everything —
  // this is UX, not a control.
  const MAX_BYTES = 2 * 1024 * 1024;
  const ACCEPTED = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];

  const DEFAULT_MARK = '/logo.svg';

  let history = $state<InstanceLogo[]>([]);
  let activeUrl = $state('');
  let busy = $state(false);
  let error = $state<string | null>(null);
  let fileInput = $state<HTMLInputElement | null>(null);

  // The default is active exactly when no uploaded entry is.
  const defaultActive = $derived(!history.some((l) => l.active));

  onMount(() => void load());

  function apply(cfg: AppearanceConfig | undefined) {
    if (!cfg) return;
    history = cfg.logo_history ?? [];
    activeUrl = cfg.logo_url ?? '';
    // Push to the shared store so the navbar mark updates immediately,
    // without a reload.
    appearance.setLogoUrl(activeUrl);
  }

  async function load() {
    const { data } = await api.GET('/admin/system/appearance');
    apply(data);
  }

  async function onFileChosen(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    const file = input.files?.[0];
    // Reset immediately so choosing the same file twice re-fires.
    input.value = '';
    if (!file) return;

    error = null;
    if (file.size > MAX_BYTES) {
      error = t('admin.system.themes.logo_too_large');
      return;
    }
    if (file.type && !ACCEPTED.includes(file.type)) {
      error = t('admin.system.themes.logo_bad_type');
      return;
    }

    busy = true;
    try {
      const res = await fetch('/api/v1/admin/system/appearance/logo', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/octet-stream' },
        body: file,
      });
      const body = (await res.json()) as AppearanceConfig & { error?: string };
      if (!res.ok) {
        error = body?.error ?? t('admin.system.themes.logo_failed');
        return;
      }
      apply(body);
    } catch {
      error = t('admin.system.themes.logo_failed');
    } finally {
      busy = false;
    }
  }

  async function select(hash: string) {
    if (busy) return;
    busy = true;
    error = null;
    try {
      const { data, error: err } = await api.POST('/admin/system/appearance/logo/select', {
        body: { hash },
      });
      if (err) {
        error = (err as { error?: string }).error ?? t('admin.system.themes.logo_failed');
        return;
      }
      apply(data);
    } finally {
      busy = false;
    }
  }

  async function useDefault() {
    if (busy) return;
    busy = true;
    error = null;
    try {
      const { data, error: err } = await api.DELETE('/admin/system/appearance/logo', {});
      if (err) {
        error = (err as { error?: string }).error ?? t('admin.system.themes.logo_failed');
        return;
      }
      apply(data);
    } finally {
      busy = false;
    }
  }
</script>

<section class="rounded-lg border border-border bg-surface-elevated p-4" data-testid="instance-logo-card">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <div class="min-w-0">
      <h3 class="text-sm font-medium text-fg">{t('admin.system.themes.logo_label')}</h3>
      <p class="mt-0.5 text-xs text-fg-muted">{t('admin.system.themes.logo_help')}</p>
    </div>
    <div>
      <input
        bind:this={fileInput}
        type="file"
        accept={ACCEPTED.join(',')}
        class="hidden"
        data-testid="logo-file-input"
        onchange={onFileChosen}
      />
      <button
        type="button"
        disabled={busy}
        onclick={() => fileInput?.click()}
        data-testid="logo-upload-button"
        class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
      >
        {busy
          ? t('admin.system.themes.logo_uploading')
          : activeUrl
            ? t('admin.system.themes.logo_replace')
            : t('admin.system.themes.logo_upload')}
      </button>
    </div>
  </div>

  {#if error}
    <p
      role="alert"
      data-testid="logo-error"
      class="mt-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-on-danger-container"
    >
      {error}
    </p>
  {/if}

  <p class="mt-4 text-xs font-medium text-fg">{t('admin.system.themes.logo_recent')}</p>
  <p class="mt-0.5 text-xs text-fg-muted">{t('admin.system.themes.logo_recent_help')}</p>

  <!-- Wraps rather than scrolls: six tiles at ~7rem fit two rows even
       at 390px, so the operator never has to discover hidden options. -->
  <ul class="mt-3 flex flex-wrap gap-3" data-testid="logo-history">
    <li>
      <button
        type="button"
        disabled={busy || defaultActive}
        onclick={useDefault}
        data-testid="logo-tile-default"
        aria-pressed={defaultActive}
        title={t('admin.system.themes.logo_use_default')}
        class="flex w-28 flex-col items-center gap-2 rounded-lg border-2 p-2 text-center transition
               {defaultActive
          ? 'border-accent bg-accent/10'
          : 'border-border-subtle bg-surface hover:border-border-strong'}
               disabled:cursor-default"
      >
        <img
          src={DEFAULT_MARK}
          alt={t('admin.system.themes.logo_default_alt')}
          class="h-14 w-14 object-contain"
        />
        <span class="text-xs text-fg-muted">{t('admin.system.themes.logo_default')}</span>
        {#if defaultActive}
          <span class="text-[0.65rem] font-medium text-accent">{t('admin.system.themes.logo_in_use')}</span>
        {/if}
      </button>
    </li>

    {#each history as entry (entry.hash)}
      <li>
        <button
          type="button"
          disabled={busy || entry.active || !entry.available}
          onclick={() => select(entry.hash)}
          data-testid="logo-tile"
          data-hash={entry.hash}
          data-available={entry.available}
          aria-pressed={entry.active}
          title={entry.available
            ? t('admin.system.themes.logo_select')
            : t('admin.system.themes.logo_unavailable_help')}
          class="flex w-28 flex-col items-center gap-2 rounded-lg border-2 p-2 text-center transition
                 {entry.active
            ? 'border-accent bg-accent/10'
            : 'border-border-subtle bg-surface hover:border-border-strong'}
                 {!entry.available ? 'cursor-not-allowed opacity-60' : ''}
                 disabled:cursor-default"
        >
          {#if entry.available}
            <img src={entry.url} alt="" class="h-14 w-14 object-contain" />
          {:else}
            <!-- Deliberately NOT an <img> at a URL we know 404s: a
                 broken-image glyph reads as "the page is broken", and
                 the operator needs to read "this file is gone". -->
            <span
              class="flex h-14 w-14 items-center justify-center rounded border border-dashed border-border-strong text-fg-muted"
              aria-hidden="true"
            >
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" class="h-6 w-6">
                <path d="M3 3l18 18M21 15l-5-5L5 21M3 5a2 2 0 012-2h14a2 2 0 012 2v14" stroke-linecap="round" />
              </svg>
            </span>
          {/if}
          <span class="text-xs text-fg-muted">{entry.width}×{entry.height}</span>
          {#if entry.active}
            <span class="text-[0.65rem] font-medium text-accent">{t('admin.system.themes.logo_in_use')}</span>
          {:else if !entry.available}
            <span class="text-[0.65rem] font-medium text-danger">
              {t('admin.system.themes.logo_unavailable')}
            </span>
          {/if}
        </button>
      </li>
    {/each}
  </ul>
</section>
