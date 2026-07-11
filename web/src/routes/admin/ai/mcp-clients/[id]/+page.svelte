<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Phase 1.53.A — /admin/ai/mcp-clients/[id]
  //
  // Per-server detail: edit registration fields + manage the tool
  // whitelist (UPSERT/DELETE on mcp_server_tool_grant). The tool
  // whitelist is the security primitive — only tools listed here can
  // be invoked, and each tool can require an extra capability beyond
  // the umbrella mcp.client.use cap.

  import { onMount } from 'svelte';
  import { page } from '$app/stores';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import type { components } from '$api/schema';

  type MCPServer = components['schemas']['MCPServer'];
  type MCPServerUpdate = components['schemas']['MCPServerUpdate'];
  type MCPServerToolGrant = components['schemas']['MCPServerToolGrant'];
  type MCPServerToolGrantUpsert = components['schemas']['MCPServerToolGrantUpsert'];

  let id = $derived($page.params.id ?? '');

  let server = $state<MCPServer | null>(null);
  let grants = $state<MCPServerToolGrant[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);

  // Editor mirrors the loaded row so the user can mutate without
  // touching server until save.
  let edit = $state<MCPServerUpdate>({});
  let saving = $state(false);
  let saved = $state(false);
  let saveError = $state<string | null>(null);

  // New-tool form state.
  let newTool = $state({
    tool_name: '',
    additional_capability: '',
    cost_estimate_micros: 0,
    enabled: true,
  });
  let addingTool = $state(false);
  let toolError = $state<string | null>(null);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    loadError = null;
    try {
      // Server list endpoint, then locate by id — keeps the API
      // surface narrow (no single-server GET in v1).
      const { data, error } = await api.GET('/admin/ai/mcp-clients');
      if (error) {
        loadError = (error as { error?: string }).error ?? t('admin.system.mcp_clients.load_error');
        return;
      }
      const found = data?.items?.find((s) => s.id === id) ?? null;
      if (!found) {
        loadError = t('admin.system.mcp_clients.load_error');
        return;
      }
      server = found;
      edit = {
        url: found.url,
        transport: found.transport,
        auth_kind: found.auth_kind,
        auth_secret_ref: found.auth_secret_ref ?? '',
        auth_header_name: found.auth_header_name ?? '',
        privacy_class: found.privacy_class,
        enabled: found.enabled,
        rate_limit_per_second: found.rate_limit_per_second,
        rate_limit_per_minute: found.rate_limit_per_minute,
        health_check_interval_s: found.health_check_interval_s,
      };

      const { data: g, error: gErr } = await api.GET('/admin/ai/mcp-clients/{id}/tools', {
        params: { path: { id } },
      });
      if (gErr) {
        loadError = (gErr as { error?: string }).error ?? t('admin.system.mcp_clients.load_error');
        return;
      }
      grants = g ?? [];
    } catch (e) {
      loadError = (e as Error).message;
    } finally {
      loading = false;
    }
  }

  async function save(e: Event) {
    e.preventDefault();
    if (saving) return;
    saving = true;
    saved = false;
    saveError = null;
    try {
      const { error } = await api.PATCH('/admin/ai/mcp-clients/{id}', {
        params: { path: { id } },
        body: edit,
      });
      if (error) {
        saveError = (error as { error?: string }).error ?? t('admin.system.mcp_clients.save_failed');
        return;
      }
      saved = true;
      await load();
    } finally {
      saving = false;
    }
  }

  async function addTool(e: Event) {
    e.preventDefault();
    if (addingTool || !newTool.tool_name) return;
    addingTool = true;
    toolError = null;
    try {
      const body: MCPServerToolGrantUpsert = {
        additional_capability: newTool.additional_capability || undefined,
        cost_estimate_micros: Number(newTool.cost_estimate_micros) || 0,
        enabled: newTool.enabled,
      };
      const { error } = await api.PUT('/admin/ai/mcp-clients/{id}/tools/{tool}', {
        params: { path: { id, tool: newTool.tool_name } },
        body,
      });
      if (error) {
        toolError = (error as { error?: string }).error ?? t('admin.system.mcp_clients.save_failed');
        return;
      }
      newTool = { tool_name: '', additional_capability: '', cost_estimate_micros: 0, enabled: true };
      await load();
    } finally {
      addingTool = false;
    }
  }

  async function toggleTool(g: MCPServerToolGrant) {
    const body: MCPServerToolGrantUpsert = {
      additional_capability: g.additional_capability ?? undefined,
      cost_estimate_micros: g.cost_estimate_micros,
      enabled: !g.enabled,
    };
    const { error } = await api.PUT('/admin/ai/mcp-clients/{id}/tools/{tool}', {
      params: { path: { id, tool: g.tool_name } },
      body,
    });
    if (error) {
      toolError = (error as { error?: string }).error ?? t('admin.system.mcp_clients.save_failed');
      return;
    }
    await load();
  }

  async function removeTool(tool: string) {
    if (!confirm(t('admin.system.mcp_clients.confirm_delete'))) return;
    const { error } = await api.DELETE('/admin/ai/mcp-clients/{id}/tools/{tool}', {
      params: { path: { id, tool } },
    });
    if (error) {
      toolError = (error as { error?: string }).error ?? t('admin.system.mcp_clients.delete_failed');
      return;
    }
    grants = grants.filter((x) => x.tool_name !== tool);
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

<svelte:head><title>{server?.name ?? t('admin.system.mcp_clients.title')} — artist-alley</title></svelte:head>

<nav class="mb-3 text-xs text-fg-muted">
  <a href="/admin/system/ai" class="hover:underline">{t('admin.system.ai_landing.title')}</a>
  <span aria-hidden="true">/</span>
  <a href="/admin/ai/mcp-clients" class="hover:underline">{t('admin.system.mcp_clients.title')}</a>
  <span aria-hidden="true">/</span>
  <span>{server?.name ?? id}</span>
</nav>

{#if loading}
  <p class="text-fg-muted">{t('admin.system.mcp_clients.loading')}</p>
{:else if loadError || !server}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
    {loadError ?? t('admin.system.mcp_clients.load_error')}
  </p>
  <p class="mt-3 text-sm">
    <a href="/admin/ai/mcp-clients" class="text-accent hover:underline">{t('admin.system.mcp_clients.back_to_list')}</a>
  </p>
{:else}
  <header class="mb-6 flex items-baseline justify-between gap-4">
    <h2 class="text-xl font-semibold">{server.name}</h2>
    <span class="rounded border px-2 py-0.5 text-xs {healthClass(server.last_health_status)}" title={server.last_health_error ?? ''}>
      {healthLabel(server.last_health_status)}
    </span>
  </header>

  <section class="mb-8 max-w-3xl rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="mb-3 text-sm font-medium text-fg">{t('admin.system.mcp_clients.edit_heading')}</h3>
    <form onsubmit={save} class="space-y-3 text-sm">
      <div class="grid grid-cols-1 gap-3 md:grid-cols-2">
        <label class="block md:col-span-2">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_url')}</span>
          <input type="url" bind:value={edit.url}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
        </label>
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_transport')}</span>
          <select bind:value={edit.transport}
                  class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none">
            <option value="http">http</option>
            <option value="stdio">stdio</option>
          </select>
        </label>
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_privacy')}</span>
          <select bind:value={edit.privacy_class}
                  class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none">
            <option value="cloud">cloud</option>
            <option value="local">local</option>
          </select>
        </label>

        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_auth_kind')}</span>
          <select bind:value={edit.auth_kind}
                  class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none">
            <option value="none">none</option>
            <option value="bearer">bearer</option>
            <option value="header">header</option>
            <option value="mtls">mtls</option>
          </select>
        </label>
        {#if edit.auth_kind === 'bearer' || edit.auth_kind === 'header'}
          <label class="block">
            <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_auth_secret_ref')}</span>
            <input type="password" bind:value={edit.auth_secret_ref}
                   class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
          </label>
        {/if}
        {#if edit.auth_kind === 'header'}
          <label class="block md:col-span-2">
            <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_auth_header_name')}</span>
            <input type="text" bind:value={edit.auth_header_name}
                   class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
          </label>
        {/if}

        <label class="inline-flex items-end gap-2">
          <input type="checkbox" bind:checked={edit.enabled} class="h-4 w-4 accent-accent" />
          <span class="pb-1 text-xs text-fg-muted">{t('admin.system.mcp_clients.field_enabled')}</span>
        </label>

        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_rate_limit_per_second')}</span>
          <input type="number" min="1" bind:value={edit.rate_limit_per_second}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
        </label>
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_rate_limit_per_minute')}</span>
          <input type="number" min="1" bind:value={edit.rate_limit_per_minute}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
        </label>
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.field_health_interval')}</span>
          <input type="number" min="10" bind:value={edit.health_check_interval_s}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
        </label>
      </div>

      {#if saveError}
        <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{saveError}</p>
      {/if}
      {#if saved}
        <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success">{t('admin.system.mcp_clients.saved')}</p>
      {/if}

      <button type="submit" disabled={saving}
              class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40">
        {saving ? t('common.loading') : t('admin.system.mcp_clients.save_changes')}
      </button>
    </form>
  </section>

  <section class="max-w-3xl rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="mb-1 text-sm font-medium text-fg">{t('admin.system.mcp_clients.tools_heading')}</h3>
    <p class="mb-3 text-xs text-fg-muted">{t('admin.system.mcp_clients.tools_intro')}</p>

    {#if grants.length === 0}
      <p class="mb-4 rounded border border-border bg-surface px-3 py-2 text-xs text-fg-muted">{t('admin.system.mcp_clients.no_tools')}</p>
    {:else}
      <table class="mb-4 w-full text-left text-sm">
        <thead class="bg-surface text-xs text-fg-muted">
          <tr>
            <th class="px-2 py-1 font-medium">{t('admin.system.mcp_clients.tool_col_name')}</th>
            <th class="px-2 py-1 font-medium">{t('admin.system.mcp_clients.tool_col_capability')}</th>
            <th class="px-2 py-1 font-medium text-right">{t('admin.system.mcp_clients.tool_col_cost')}</th>
            <th class="px-2 py-1 font-medium">{t('admin.system.mcp_clients.tool_col_enabled')}</th>
            <th class="px-2 py-1 font-medium text-right">{t('admin.system.mcp_clients.tool_col_actions')}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-border">
          {#each grants as g (g.tool_name)}
            <tr>
              <td class="px-2 py-1 font-mono text-xs text-fg">{g.tool_name}</td>
              <td class="px-2 py-1 font-mono text-xs text-fg-muted">{g.additional_capability ?? '—'}</td>
              <td class="px-2 py-1 text-right text-xs">{g.cost_estimate_micros}</td>
              <td class="px-2 py-1 text-xs">
                <label class="inline-flex items-center gap-1">
                  <input type="checkbox" checked={g.enabled} onchange={() => toggleTool(g)} class="h-3.5 w-3.5 accent-accent" />
                </label>
              </td>
              <td class="px-2 py-1 text-right text-xs">
                <button type="button" onclick={() => removeTool(g.tool_name)} class="text-danger hover:underline">
                  {t('admin.system.mcp_clients.tool_remove')}
                </button>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}

    <form onsubmit={addTool} class="space-y-2 border-t border-border pt-3 text-sm">
      <h4 class="text-xs font-medium uppercase tracking-wide text-fg-muted">{t('admin.system.mcp_clients.tool_add_heading')}</h4>
      <div class="grid grid-cols-1 gap-2 md:grid-cols-2">
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.tool_field_name')}</span>
          <input type="text" required bind:value={newTool.tool_name}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 font-mono focus-visible:border-border-strong focus:outline-none" />
          <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.mcp_clients.tool_field_name_hint')}</span>
        </label>
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.tool_field_capability')}</span>
          <input type="text" bind:value={newTool.additional_capability} placeholder="mcp.client.images.write"
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 font-mono focus-visible:border-border-strong focus:outline-none" />
          <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.mcp_clients.tool_field_capability_hint')}</span>
        </label>
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.system.mcp_clients.tool_field_cost')}</span>
          <input type="number" min="0" bind:value={newTool.cost_estimate_micros}
                 class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 focus-visible:border-border-strong focus:outline-none" />
          <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.mcp_clients.tool_field_cost_hint')}</span>
        </label>
        <label class="inline-flex items-end gap-2">
          <input type="checkbox" bind:checked={newTool.enabled} class="h-4 w-4 accent-accent" />
          <span class="pb-1 text-xs text-fg-muted">{t('admin.system.mcp_clients.field_enabled')}</span>
        </label>
      </div>

      {#if toolError}
        <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{toolError}</p>
      {/if}

      <button type="submit" disabled={addingTool}
              class="rounded-md bg-accent px-3 py-1 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40">
        {addingTool ? t('common.loading') : t('admin.system.mcp_clients.tool_add_submit')}
      </button>
    </form>
  </section>
{/if}
