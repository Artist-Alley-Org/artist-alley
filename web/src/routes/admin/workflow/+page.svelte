<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface State {
    id: string;
    code: string;
    label: string;
    is_initial: boolean;
    is_terminal: boolean;
    icon: string;
    color: string;
  }

  let domain = $state('post');
  let states = $state<State[]>([]);
  let loading = $state(false);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/workflow/states', {
        params: { query: { domain } },
      });
      states = (data ?? []) as State[];
    } finally {
      loading = false;
    }
  }
</script>

<svelte:head><title>{t('admin.workflow.title')} — {site.name}</title></svelte:head>

<h2 class="mb-4 text-xl font-semibold">{t('admin.workflow.title')}</h2>

<div class="mb-4 max-w-md">
  <label class="block text-sm">
    <span class="block text-xs text-fg-muted">{t('admin.workflow.domain_label')}</span>
    <select
      bind:value={domain}
      onchange={() => void load()}
      class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
    >
      <option value="post">post</option>
      <option value="asset:1">asset:1 (Photo)</option>
      <option value="asset:2">asset:2 (Document)</option>
      <option value="asset:3">asset:3 (Video)</option>
    </select>
  </label>
</div>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if states.length === 0}
  <p class="rounded-md bg-surface-elevated px-4 py-6 text-center text-fg-muted">{t('admin.workflow.no_states')}</p>
{:else}
  <div class="space-y-2">
    {#each states as s (s.id)}
      <article class="flex items-center gap-3 rounded-lg border border-border bg-surface-elevated p-3">
        <span
          class="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-full"
          style:background-color={s.color || '#64748b'}
        ></span>
        <div class="min-w-0 flex-1">
          <p class="text-sm font-medium text-fg">{s.label}</p>
          <p class="font-mono text-xs text-fg-muted">{s.code}</p>
        </div>
        <div class="text-xs text-fg-muted">
          {#if s.is_initial}<span class="rounded bg-success-container px-2 py-0.5 text-success">initial</span>{/if}
          {#if s.is_terminal}<span class="ml-1 rounded bg-danger-container px-2 py-0.5 text-danger">terminal</span>{/if}
        </div>
      </article>
    {/each}
  </div>
{/if}
