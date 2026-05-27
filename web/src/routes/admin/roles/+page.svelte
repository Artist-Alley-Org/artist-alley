<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface Role {
    id: string;
    name: string;
    description?: string;
    capabilities?: string[];
  }

  let roles = $state<Role[]>([]);
  let loading = $state(true);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/auth/roles');
      const arr = (data as { items?: Role[] })?.items ?? (data as unknown as Role[]) ?? [];
      roles = arr;
    } finally {
      loading = false;
    }
  }
</script>

<svelte:head><title>{t('admin.roles.title')} — artist-alley</title></svelte:head>

<h2 class="mb-4 text-xl font-semibold">{t('admin.roles.title')}</h2>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if roles.length === 0}
  <p class="rounded-md bg-surface-elevated px-4 py-6 text-center text-fg-muted">{t('admin.roles.no_roles')}</p>
{:else}
  <div class="space-y-3">
    {#each roles as r (r.id)}
      <article class="rounded-lg border border-border bg-surface p-4">
        <h3 class="text-base font-semibold text-fg">{r.name}</h3>
        {#if r.description}
          <p class="mt-1 text-sm text-fg-muted">{r.description}</p>
        {/if}
        {#if r.capabilities && r.capabilities.length > 0}
          <div class="mt-3">
            <p class="text-xs uppercase tracking-wider text-fg-muted">{t('admin.roles.capabilities')}</p>
            <div class="mt-1 flex flex-wrap gap-1">
              {#each r.capabilities as cap (cap)}
                <span class="rounded bg-surface-elevated px-2 py-0.5 text-xs text-fg">{cap}</span>
              {/each}
            </div>
          </div>
        {/if}
      </article>
    {/each}
  </div>
{/if}
