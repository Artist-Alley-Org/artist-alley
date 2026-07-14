<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  let host = $state('');
  let port = $state(587);
  let username = $state('');
  let password = $state('');
  let passwordSet = $state(false);
  let encryption = $state<'none' | 'starttls' | 'tls'>('starttls');
  let fromAddress = $state('');
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  // Test-send state (Phase 1.19.A-1, commit 4).
  let testTo = $state('');
  let testing = $state(false);
  let testResult = $state<string | null>(null);
  let testError = $state<string | null>(null);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/admin/system/smtp');
      if (data) {
        const d = data as { host?: string; port?: number; username?: string; encryption?: string; from_address?: string; password_set?: boolean };
        host = d.host ?? '';
        port = d.port ?? 587;
        username = d.username ?? '';
        encryption = (d.encryption as typeof encryption) ?? 'starttls';
        fromAddress = d.from_address ?? '';
        passwordSet = d.password_set ?? false;
      }
    } finally {
      loading = false;
    }
  }

  async function sendTest() {
    if (testing) return;
    testing = true;
    testResult = null;
    testError = null;
    try {
      const body: Record<string, unknown> = {};
      if (testTo.trim()) body.to = testTo.trim();
      const r = await api.POST('/admin/system/smtp/test', { body: body as never });
      if (r.error) {
        testError = (r.error as { error?: string }).error ?? t('admin.system.smtp.test_failed');
        return;
      }
      const d = r.data as { sent: boolean; mode: string; recipient: string; message?: string };
      testResult = d.message
        ? `${d.recipient}: ${d.message}`
        : `${d.recipient}: ${d.sent ? 'sent' : 'not sent'} (mode=${d.mode})`;
    } finally {
      testing = false;
    }
  }

  async function save() {
    if (saving) return;
    saving = true;
    saved = false;
    error = null;
    try {
      const body: Record<string, unknown> = {
        host, port, encryption, from_address: fromAddress, username,
      };
      if (password) body.password = password;
      const { error: apiErr } = await api.PATCH('/admin/system/smtp', { body: body as never });
      if (apiErr) {
        error = (apiErr as { error?: string }).error ?? t('errors.save_failed');
        return;
      }
      saved = true;
      password = '';
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>{t('admin.system.smtp.title')} — artist-alley</title></svelte:head>

<h2 class="mb-4 text-xl font-semibold">{t('admin.system.smtp.title')}</h2>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else}
  <form onsubmit={(e) => { e.preventDefault(); void save(); }} class="max-w-xl space-y-4">
    <div class="grid grid-cols-[1fr_8rem] gap-2">
      <label class="block">
        <span class="text-sm text-fg-muted">{t('admin.system.smtp.host')}</span>
        <input type="text" bind:value={host} class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none" />
      </label>
      <label class="block">
        <span class="text-sm text-fg-muted">{t('admin.system.smtp.port')}</span>
        <input type="number" bind:value={port} min="1" max="65535" class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none" />
      </label>
    </div>
    <label class="block">
      <span class="text-sm text-fg-muted">{t('admin.system.smtp.encryption')}</span>
      <select bind:value={encryption} class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none">
        <option value="none">none</option>
        <option value="starttls">STARTTLS</option>
        <option value="tls">TLS</option>
      </select>
    </label>
    <label class="block">
      <span class="text-sm text-fg-muted">{t('admin.system.smtp.username')}</span>
      <input type="text" bind:value={username} class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none" />
    </label>
    <label class="block">
      <span class="text-sm text-fg-muted">{t('admin.system.smtp.password')}</span>
      <input
        type="password"
        bind:value={password}
        placeholder={passwordSet ? t('admin.system.smtp.password_on_file') : t('admin.system.smtp.password_unset')}
        autocomplete="new-password"
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
      />
      <p class="mt-1 text-xs text-fg-muted">{t('admin.system.smtp.password_help')}</p>
    </label>
    <label class="block">
      <span class="text-sm text-fg-muted">{t('admin.system.smtp.from_address')}</span>
      <input type="email" bind:value={fromAddress} placeholder="artist-alley <noreply@example.com>" class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none" />
    </label>

    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
    {/if}
    {#if saved}
      <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success">{t('admin.system.smtp.saved')}</p>
    {/if}

    <button type="submit" disabled={saving} class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40">
      {saving ? t('common.loading') : t('admin.system.smtp.save')}
    </button>
  </form>

  <section class="mt-8 max-w-xl rounded border border-border bg-bg-soft p-4">
    <h3 class="text-sm font-semibold">{t('admin.system.smtp.test_title')}</h3>
    <p class="mt-1 text-xs text-fg-muted">{t('admin.system.smtp.test_help')}</p>
    <div class="mt-3 flex flex-wrap items-end gap-2">
      <label class="flex flex-1 flex-col gap-1 text-sm">
        <span class="text-xs text-fg-muted">{t('admin.system.smtp.test_to')}</span>
        <input
          type="email"
          bind:value={testTo}
          placeholder={t('admin.system.smtp.test_to_placeholder')}
          class="rounded border border-border bg-bg px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
        />
      </label>
      <button
        type="button"
        onclick={sendTest}
        disabled={testing}
        class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
      >{testing ? t('common.loading') : t('admin.system.smtp.test_send')}</button>
    </div>
    {#if testError}
      <p role="alert" class="mt-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{testError}</p>
    {/if}
    {#if testResult}
      <p class="mt-3 rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success">{testResult}</p>
    {/if}
  </section>
{/if}
