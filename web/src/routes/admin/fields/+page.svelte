<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface Field {
    id: string;
    code: string;
    label: string;
    type: string;
    applies_to: number[];
    display_group?: string;
  }

  let fields = $state<Field[]>([]);
  let loading = $state(true);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/fields', { params: { query: { status: 'active' } } });
      fields = (data ?? []) as Field[];
    } finally {
      loading = false;
    }
  }
</script>

<svelte:head><title>{t('admin.fields.title')} — artist-alley</title></svelte:head>

<h2 class="mb-4 text-xl font-semibold">{t('admin.fields.title')}</h2>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if fields.length === 0}
  <p class="rounded-md bg-surface-elevated px-4 py-6 text-center text-fg-muted">{t('admin.fields.no_fields')}</p>
{:else}
  <table class="w-full text-sm">
    <thead class="text-left text-xs uppercase tracking-wider text-fg-muted">
      <tr>
        <th class="py-2">{t('admin.fields.code')}</th>
        <th class="py-2">{t('admin.fields.label')}</th>
        <th class="py-2">{t('admin.fields.type')}</th>
        <th class="py-2">{t('admin.fields.applies_to')}</th>
        <th class="py-2">Group</th>
      </tr>
    </thead>
    <tbody>
      {#each fields as f (f.id)}
        <tr class="border-t border-border">
          <td class="py-2 font-mono text-xs">{f.code}</td>
          <td class="py-2">{f.label}</td>
          <td class="py-2 text-fg-muted">{f.type}</td>
          <td class="py-2 text-fg-muted">{f.applies_to?.length ? f.applies_to.join(', ') : 'all'}</td>
          <td class="py-2 text-fg-muted">{f.display_group ?? ''}</td>
        </tr>
      {/each}
    </tbody>
  </table>
{/if}
