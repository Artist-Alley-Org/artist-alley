<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  let host = $state('');
  let port = $state(587);
  let username = $state('');
  let password = $state('');
  let encryption = $state<'none' | 'starttls' | 'tls'>('starttls');
  let fromAddress = $state('');
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/admin/system/smtp');
      if (data) {
        const d = data as { host?: string; port?: number; username?: string; encryption?: string; from_address?: string };
        host = d.host ?? '';
        port = d.port ?? 587;
        username = d.username ?? '';
        encryption = (d.encryption as typeof encryption) ?? 'starttls';
        fromAddress = d.from_address ?? '';
      }
    } finally {
      loading = false;
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
        <input type="text" bind:value={host} class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none" />
      </label>
      <label class="block">
        <span class="text-sm text-fg-muted">{t('admin.system.smtp.port')}</span>
        <input type="number" bind:value={port} min="1" max="65535" class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none" />
      </label>
    </div>
    <label class="block">
      <span class="text-sm text-fg-muted">{t('admin.system.smtp.encryption')}</span>
      <select bind:value={encryption} class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none">
        <option value="none">none</option>
        <option value="starttls">STARTTLS</option>
        <option value="tls">TLS</option>
      </select>
    </label>
    <label class="block">
      <span class="text-sm text-fg-muted">{t('admin.system.smtp.username')}</span>
      <input type="text" bind:value={username} class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none" />
    </label>
    <label class="block">
      <span class="text-sm text-fg-muted">{t('admin.system.smtp.password')}</span>
      <input type="password" bind:value={password} placeholder="(unchanged)" class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none" />
    </label>
    <label class="block">
      <span class="text-sm text-fg-muted">{t('admin.system.smtp.from_address')}</span>
      <input type="email" bind:value={fromAddress} placeholder="artist-alley <noreply@example.com>" class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none" />
    </label>

    {#if error}
      <p role="alert" class="rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-600">{error}</p>
    {/if}
    {#if saved}
      <p class="rounded border border-emerald-500/40 bg-emerald-500/10 px-3 py-2 text-sm text-emerald-700">{t('admin.system.smtp.saved')}</p>
    {/if}

    <button type="submit" disabled={saving} class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40">
      {saving ? t('common.loading') : t('admin.system.smtp.save')}
    </button>
  </form>
{/if}
