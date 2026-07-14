<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Phase 1.53.A — /admin/ai/mcp-clients
  //
  // List of registered MCP servers + a register-new form. Detail
  // editor + tool-grant table live on /admin/ai/mcp-clients/[id].
  // Health pulses are written by the per-server health-check
  // goroutine (mcp_dispatch.HealthChecker); the badge here just
  // reads last_health_status off the row.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import type { components } from '$api/schema';

  type MCPServer = components['schemas']['MCPServer'];
  type MCPServerCreate = components['schemas']['MCPServerCreate'];

  let servers = $state<MCPServer[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);

  let form = $state<MCPServerCreate>({
    name: '',
    url: '',
    transport: 'http',
    auth_kind: 'none',
    auth_secret_ref: '',
    auth_header_name: '',
    privacy_class: 'cloud',
    enabled: false,
    rate_limit_per_second: 2,
    rate_limit_per_minute: 60,
    health_check_interval_s: 60,
  });
  let registering = $state(false);
  let registerError = $state<string | null>(null);
  let registerOk = $state(false);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    loadError = null;
    try {
      const { data, error } = await api.GET('/admin/ai/mcp-clients');
      if (error) {
        loadError = (error as { error?: string }).error ?? t('admin.system.mcp_clients.load_error');
        return;
      }
      servers = data?.items ?? [];
    } catch (e) {
      loadError = (e as Error).message;
    } finally {
      loading = false;
    }
  }

  async function register(e: Event) {
    e.preventDefault();
    if (registering) return;
    registering = true;
    registerError = null;
    registerOk = false;
    try {
      const payload: MCPServerCreate = { ...form };
      // Empty strings collapse to undefined so the backend keeps its defaults.
      if (!payload.auth_secret_ref) delete payload.auth_secret_ref;
      if (!payload.auth_header_name) delete payload.auth_header_name;
      const { error, response } = await api.POST('/admin/ai/mcp-clients', { body: payload });
      if (error) {
        if (response?.status === 409) {
          registerError = t('admin.system.mcp_clients.duplicate_name');
        } else {
          registerError = (error as { error?: string }).error ?? t('admin.system.mcp_clients.save_failed');
        }
        return;
      }
      registerOk = true;
      form = {
        name: '', url: '', transport: 'http', auth_kind: 'none',
        auth_secret_ref: '', auth_header_name: '', privacy_class: 'cloud',
        enabled: false, rate_limit_per_second: 2, rate_limit_per_minute: 60,
        health_check_interval_s: 60,
      };
      await load();
    } finally {
      registering = false;
    }
  }

  async function remove(id: string, name: string) {
    if (!confirm(t('admin.system.mcp_clients.confirm_delete'))) return;
    const { error } = await api.DELETE('/admin/ai/mcp-clients/{id}', {
      params: { path: { id } },
    });
    if (error) {
      loadError = (error as { error?: string }).error ?? t('admin.system.mcp_clients.delete_failed');
      return;
    }
    servers = servers.filter((s) => s.id !== id);
    void name;
  }

  function healthLabel(status?: string | null): string {
    switch (status) {
      case 'healthy':     return t('admin.system.mcp_clients.health_healthy');
      case 'degraded':    return t('admin.system.mcp_clients.health_degraded');
      case 'unreachable': return t('admin.system.mcp_clients.health_unreachable');
      default:            return t('admin.system.mcp_clients.health_unknown');
    }
  }

  function healthClass(status?: string | null): string {
    switch (status) {
      case 'healthy':     return 'border-success/40 bg-success-container text-success';
      case 'degraded':    return 'border-warning/40 bg-warning-container text-warning';
      case 'unreachable': return 'border-danger/40 bg-danger-container text-danger';
      default:            return 'border-border bg-surface text-fg-muted';
    }
  }
</script>

<svelte:head><title>{t('admin.system.mcp_clients.title')} — {site.name}</title></svelte:head>

<nav class="mb-3 text-xs text-fg-muted">
  <a href="/admin/system/ai" class="hover:underline">{t('admin.system.ai_landing.title')}</a>
  <span aria-hidden="true">/</span>
  <span>{t('admin.system.mcp_clients.title')}</span>
</nav>

<header class="mb-6">
  <h2 class="text-xl font-semibold">{t('admin.system.mcp_clients.title')}</h2>
  <p class="mt-1 max-w-3xl text-sm text-fg-muted">{t('admin.system.mcp_clients.intro')}</p>
</header>

{#if loading}
  <p class="text-fg-muted">{t('admin.system.mcp_clients.loading')}</p>
{:else}
  {#if loadError}
    <p role="alert" class="mb-4 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{loadError}</p>
  {/if}

  <section class="mb-8 overflow-hidden rounded-lg border border-border bg-surface-elevated">
    {#if servers.length === 0}
      <p class="px-4 py-6 text-sm text-fg-muted">{t('admin.system.mcp_clients.no_servers')}</p>
    {:else}
      <table class="w-full text-left text-sm">
        <thead class="bg-surface text-xs text-fg-muted">
          <tr>
            <th class="px-3 py-2 font-medium">{t('admin.system.mcp_clients.col_name')}</th>
            <th class="px-3 py-2 font-medium">{t('admin.system.mcp_clients.col_url')}</th>
            <th class="px-3 py-2 font-medium">{t('admin.system.mcp_clients.col_privacy')}</th>
            <th class="px-3 py-2 font-medium">{t('admin.system.mcp_clients.col_enabled')}</th>
            <th class="px-3 py-2 font-medium">{t('admin.system.mcp_clients.col_health')}</th>
            <th class="px-3 py-2 font-medium text-right">{t('admin.system.mcp_clients.col_actions')}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-border">
          {#each servers as s (s.id)}
            <tr>
              <td class="px-3 py-2 font-medium text-fg">{s.name}</td>
              <td class="px-3 py-2 font-mono text-xs text-fg-muted">{s.url}</td>
              <td class="px-3 py-2 text-xs text-fg-muted">{s.privacy_class}</td>
              <td class="px-3 py-2 text-xs">
                {#if s.enabled}
                  <span class="rounded border border-success/40 bg-success-container px-1.5 py-0.5 text-success">on</span>
                {:else}
                  <span class="rounded border border-border bg-surface px-1.5 py-0.5 text-fg-muted">off</span>
                {/if}
              </td>
              <td class="px-3 py-2 text-xs">
                <span class="rounded border px-1.5 py-0.5 {healthClass(s.last_health_status)}" title={s.last_health_error ?? ''}>
                  {healthLabel(s.last_health_status)}
                </span>
              </td>
              <td class="px-3 py-2 text-right text-xs">
                <a href={`/admin/ai/mcp-clients/${s.id}`} class="mr-2 text-accent hover:underline">{t('admin.system.mcp_clients.action_view')}</a>
                <button type="button" onclick={() => remove(s.id, s.name)} class="text-danger hover:underline">
                  {t('admin.system.mcp_clients.action_delete')}
                </button>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </section>

  <section class="max-w-3xl rounded-lg border border-border bg-surface-elevated p-4">
    <header class="mb-3">
      <h3 class="text-sm font-medium text-fg">{t('admin.system.mcp_clients.register_heading')}</h3>
      <p class="mt-1 text-xs text-fg-muted">{t('admin.system.mcp_clients.register_blurb')}</p>
    </header>

    <form onsubmit={register} class="space-y-3 text-sm">
      <div class="grid grid-cols-1 gap-3 md:grid-cols-2">
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_name')}</span>
          <input type="text" required bind:value={form.name}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
          <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.mcp_clients.field_name_hint')}</span>
        </label>
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_url')}</span>
          <input type="url" required bind:value={form.url}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
          <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.mcp_clients.field_url_hint')}</span>
        </label>

        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_transport')}</span>
          <select bind:value={form.transport}
                  class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none">
            <option value="http">http</option>
            <option value="stdio">stdio</option>
          </select>
        </label>

        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_privacy')}</span>
          <select bind:value={form.privacy_class}
                  class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none">
            <option value="cloud">cloud</option>
            <option value="local">local</option>
          </select>
          <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.mcp_clients.field_privacy_hint')}</span>
        </label>

        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_auth_kind')}</span>
          <select bind:value={form.auth_kind}
                  class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none">
            <option value="none">none</option>
            <option value="bearer">bearer</option>
            <option value="header">header</option>
            <option value="mtls">mtls</option>
          </select>
        </label>

        {#if form.auth_kind === 'bearer' || form.auth_kind === 'header'}
          <label class="block">
            <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_auth_secret_ref')}</span>
            <input type="password" bind:value={form.auth_secret_ref}
                   class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
            <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.mcp_clients.field_auth_secret_ref_hint')}</span>
          </label>
        {/if}

        {#if form.auth_kind === 'header'}
          <label class="block">
            <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_auth_header_name')}</span>
            <input type="text" bind:value={form.auth_header_name} placeholder="X-API-Key"
                   class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
            <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.mcp_clients.field_auth_header_name_hint')}</span>
          </label>
        {/if}

        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_rate_limit_per_second')}</span>
          <input type="number" min="1" bind:value={form.rate_limit_per_second}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
        </label>
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_rate_limit_per_minute')}</span>
          <input type="number" min="1" bind:value={form.rate_limit_per_minute}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
        </label>
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_health_interval')}</span>
          <input type="number" min="10" bind:value={form.health_check_interval_s}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
        </label>
      </div>

      {#if registerError}
        <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{registerError}</p>
      {/if}
      {#if registerOk}
        <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success">{t('admin.system.mcp_clients.register_saved')}</p>
      {/if}

      <button type="submit" disabled={registering}
              class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40">
        {registering ? t('common.loading') : t('admin.system.mcp_clients.register_submit')}
      </button>
    </form>
  </section>
{/if}
