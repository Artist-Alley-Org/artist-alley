<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin teams list + create form (Phase 1.17.E).
  //
  // The 11 team endpoints already exist server-side; this page is
  // the missing frontend gap. Inline create form at the top, table
  // below with click-through to /admin/teams/{id} for member +
  // parent + name/description management.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { type Team, isValidSlug, slugify } from '$lib/admin/teams';
  import { relativeAgo } from '$lib/admin/users';

  let teams = $state<Team[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Create form state.
  let newSlug = $state('');
  let newName = $state('');
  let newDesc = $state('');
  let slugTouched = $state(false);
  let creating = $state(false);

  onMount(() => { void load(); });

  // Auto-fill slug from name until the admin types in the slug field
  // themselves (then we stop overwriting).
  $effect(() => {
    if (!slugTouched && newName) {
      newSlug = slugify(newName);
    }
  });

  async function load() {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/teams', { params: { query: { limit: 200 } } });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to load teams.';
        return;
      }
      const page = r.data as unknown as { items: Team[] };
      teams = page.items;
    } finally {
      loading = false;
    }
  }

  async function create(e: SubmitEvent) {
    e.preventDefault();
    if (creating) return;
    error = null;
    if (!isValidSlug(newSlug)) {
      error = 'Slug must be lowercase alphanumeric or hyphen, max 80 chars.';
      return;
    }
    creating = true;
    try {
      const r = await api.POST('/teams', {
        body: { slug: newSlug, name: newName, description: newDesc || undefined },
      });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to create team.';
        return;
      }
      newSlug = '';
      newName = '';
      newDesc = '';
      slugTouched = false;
      await load();
    } finally {
      creating = false;
    }
  }
</script>

<svelte:head><title>{t('admin.teams.title')} — {site.name}</title></svelte:head>

<header class="mb-2 flex flex-wrap items-baseline justify-between gap-2">
  <h2 class="text-xl font-semibold">{t('admin.teams.title')}</h2>
  {#if !loading}
    <p class="text-xs text-fg-muted">{t('admin.teams.team_count', { count: teams.length })}</p>
  {/if}
</header>
<p class="mb-6 text-sm text-fg-muted">{t('admin.teams.intro')}</p>

<section class="mb-6 max-w-2xl rounded-lg border border-border bg-surface-elevated p-4">
  <h3 class="mb-3 text-sm font-medium text-fg">{t('admin.teams.create_section')}</h3>
  <form onsubmit={create} class="space-y-3">
    <div class="grid grid-cols-1 gap-3 md:grid-cols-2">
      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">{t('admin.teams.create_name')}</span>
        <input
          type="text"
          bind:value={newName}
          placeholder={t('admin.teams.create_name_placeholder')}
          required
          maxlength="200"
          class="w-full rounded border border-border bg-surface px-2 py-1 text-sm focus:border-accent focus:outline-none"
        />
      </label>
      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">{t('admin.teams.create_slug')}</span>
        <input
          type="text"
          bind:value={newSlug}
          onfocus={() => { slugTouched = true; }}
          placeholder={t('admin.teams.create_slug_placeholder')}
          required
          maxlength="80"
          class="w-full rounded border border-border bg-surface px-2 py-1 font-mono text-sm focus:border-accent focus:outline-none"
        />
      </label>
    </div>
    <label class="block text-xs">
      <span class="mb-1 block text-fg-muted">{t('admin.teams.create_description')}</span>
      <textarea
        bind:value={newDesc}
        maxlength="500"
        rows="2"
        class="w-full rounded border border-border bg-surface px-2 py-1 text-sm focus:border-accent focus:outline-none"
      ></textarea>
    </label>
    <button
      type="submit"
      disabled={creating || !newName || !newSlug}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {creating ? t('admin.teams.creating') : t('admin.teams.create_submit')}
    </button>
  </form>
  {#if error}
    <p role="alert" class="mt-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {/if}
</section>

<h3 class="mb-2 text-sm font-medium text-fg">{t('admin.teams.list_section')}</h3>

{#if loading}
  <p class="text-fg-muted">{t('admin.teams.loading')}</p>
{:else if teams.length === 0}
  <p class="rounded-md bg-surface-elevated px-4 py-6 text-center text-sm text-fg-muted">
    {t('admin.teams.no_teams')}
  </p>
{:else}
  <div class="overflow-x-auto rounded-md border border-border">
    <table class="w-full text-sm">
      <thead class="bg-surface-elevated text-left text-xs uppercase tracking-wider text-fg-muted">
        <tr>
          <th class="px-2 py-2">{t('admin.teams.col_name')}</th>
          <th class="px-2 py-2">{t('admin.teams.col_slug')}</th>
          <th class="px-2 py-2">{t('admin.teams.col_description')}</th>
          <th class="px-2 py-2">{t('admin.teams.col_created')}</th>
        </tr>
      </thead>
      <tbody>
        {#each teams as team (team.id)}
          <tr class="border-t border-border hover:bg-surface-elevated/60">
            <td class="px-2 py-2">
              <a
                href="/admin/teams/{team.id}"
                class="font-medium text-accent hover:underline"
                aria-label={t('admin.teams.open_team', { name: team.name })}
              >
                {team.name}
              </a>
            </td>
            <td class="px-2 py-2 font-mono text-xs text-fg-muted">{team.slug}</td>
            <td class="px-2 py-2 text-fg-muted">{team.description}</td>
            <td class="px-2 py-2 text-fg-muted" title={team.created_at}>{relativeAgo(team.created_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
