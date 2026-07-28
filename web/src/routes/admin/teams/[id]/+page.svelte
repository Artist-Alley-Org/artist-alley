<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin team detail (Phase 1.17.E) — edit name/description, manage
  // parents (DAG edges, cycle-rejected by the trigger), manage
  // members (per-team, no inheritance from descendants).

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { type Team, type TeamMember, isValidUUID } from '$lib/admin/teams';
  import { relativeAgo } from '$lib/admin/users';

  // SvelteKit types route params as `string | undefined`; the
  // matcher guarantees we have it here, so the `?? ''` is just to
  // satisfy the type checker without a runtime assert.
  const id = $derived(page.params.id ?? '');

  let team = $state<Team | null>(null);
  let members = $state<TeamMember[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Edit-details state.
  let editName = $state('');
  let editDescription = $state('');
  let saving = $state(false);
  let savedMsg = $state<string | null>(null);

  let newParentId = $state('');
  let parentBusy = $state(false);

  let newMemberRef = $state('');
  let memberBusy = $state(false);

  let deleting = $state(false);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    error = null;
    try {
      const [tResp, mResp] = await Promise.all([
        api.GET('/teams/{id}', { params: { path: { id } } }),
        api.GET('/teams/{id}/members', { params: { path: { id } } }),
      ]);
      if (tResp.error || !tResp.data) {
        error = (tResp.error as { error?: string } | undefined)?.error ?? 'Team not found.';
        return;
      }
      team = tResp.data as unknown as Team;
      editName = team.name;
      editDescription = team.description;
      if (mResp.data) {
        members = (mResp.data as unknown as { items: TeamMember[] }).items ?? [];
      }
    } finally {
      loading = false;
    }
  }

  async function saveDetails(e: SubmitEvent) {
    e.preventDefault();
    if (saving) return;
    saving = true;
    savedMsg = null;
    try {
      const r = await api.PATCH('/teams/{id}', {
        params: { path: { id } },
        body: { name: editName, description: editDescription },
      });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to save.';
        return;
      }
      team = r.data as unknown as Team;
      savedMsg = t('admin.team_detail.edit_saved');
    } finally {
      saving = false;
    }
  }

  async function addParent() {
    if (parentBusy || !isValidUUID(newParentId)) return;
    parentBusy = true;
    error = null;
    try {
      const r = await api.POST('/teams/{id}/parents', {
        params: { path: { id } },
        body: { parent_id: newParentId },
      });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to add parent.';
        return;
      }
      newParentId = '';
      await load();
    } finally {
      parentBusy = false;
    }
  }

  async function removeParent(parentID: string) {
    if (parentBusy) return;
    parentBusy = true;
    error = null;
    try {
      await api.DELETE('/teams/{id}/parents/{parent_id}', {
        params: { path: { id, parent_id: parentID } },
      });
      await load();
    } finally {
      parentBusy = false;
    }
  }

  async function addMember() {
    const ref = parseInt(newMemberRef, 10);
    if (memberBusy || isNaN(ref) || ref <= 0) return;
    memberBusy = true;
    error = null;
    try {
      const r = await api.POST('/teams/{id}/members', {
        params: { path: { id } },
        body: { user_ref: ref },
      });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to add member.';
        return;
      }
      newMemberRef = '';
      await load();
    } finally {
      memberBusy = false;
    }
  }

  async function removeMember(ref: number) {
    if (memberBusy) return;
    memberBusy = true;
    error = null;
    try {
      await api.DELETE('/teams/{id}/members/{user_ref}', {
        params: { path: { id, user_ref: ref } },
      });
      await load();
    } finally {
      memberBusy = false;
    }
  }

  async function deleteTeam() {
    if (deleting) return;
    if (!confirm(t('admin.team_detail.delete_confirm'))) return;
    deleting = true;
    try {
      const r = await api.DELETE('/teams/{id}', { params: { path: { id } } });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to delete.';
        deleting = false;
        return;
      }
      // Hard nav so the list refetches without stale state.
      window.location.href = '/admin/teams';
    } finally {
      // deleting stays true so the button is not re-clickable while
      // navigation is in flight.
    }
  }
</script>

<svelte:head>
  <title>{team ? t('admin.team_detail.title', { name: team.name }) : 'Team'} — {site.name}</title>
</svelte:head>

<p class="mb-3 text-xs">
  <a href="/admin/teams" class="text-accent hover:underline">{t('admin.team_detail.back')}</a>
</p>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if error && !team}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
{:else if team}
  <header class="mb-6">
    <h2 class="text-xl font-semibold">{t('admin.team_detail.title', { name: team.name })}</h2>
    <p class="mt-1 font-mono text-xs text-fg-muted">
      {t('admin.team_detail.header_meta', { slug: team.slug, id: team.id })}
    </p>
  </header>

  {#if error}
    <p role="alert" class="mb-4 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {/if}

  <section class="mb-6 max-w-2xl rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="mb-3 text-sm font-medium text-fg">{t('admin.team_detail.edit_section')}</h3>
    <form onsubmit={saveDetails} class="space-y-3">
      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">{t('admin.team_detail.edit_name')}</span>
        <input
          type="text"
          bind:value={editName}
          required
          maxlength="200"
          class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        />
      </label>
      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">{t('admin.team_detail.edit_description')}</span>
        <textarea
          bind:value={editDescription}
          maxlength="500"
          rows="2"
          class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        ></textarea>
      </label>
      <button
        type="submit"
        disabled={saving}
        class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
      >
        {saving ? t('admin.team_detail.edit_saving') : t('admin.team_detail.edit_save')}
      </button>
      {#if savedMsg}<p class="text-sm text-success">{savedMsg}</p>{/if}
    </form>
  </section>

  <section class="mb-6 max-w-2xl rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="mb-2 text-sm font-medium text-fg">{t('admin.team_detail.parents_section')}</h3>
    <p class="mb-3 text-xs text-fg-muted">{t('admin.team_detail.parents_intro')}</p>
    {#if (team.parent_ids ?? []).length === 0}
      <p class="text-xs text-fg-muted">{t('admin.team_detail.no_parents')}</p>
    {:else}
      <ul class="space-y-1">
        {#each (team.parent_ids ?? []) as parentID (parentID)}
          <li class="flex items-center gap-2 text-xs">
            <a href="/admin/teams/{parentID}" class="flex-1 font-mono text-accent hover:underline">{parentID}</a>
            <button
              type="button"
              onclick={() => removeParent(parentID)}
              disabled={parentBusy}
              class="rounded border border-border bg-surface px-2 py-1 text-[11px] hover:border-danger hover:text-danger disabled:opacity-50"
            >
              {t('admin.team_detail.remove_parent_button')}
            </button>
          </li>
        {/each}
      </ul>
    {/if}
    <div class="mt-3 flex gap-2">
      <input
        type="text"
        bind:value={newParentId}
        placeholder="00000000-0000-0000-0000-000000000000"
        class="flex-1 rounded border border-border-strong bg-surface px-2 py-1 font-mono text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
      <button
        type="button"
        onclick={addParent}
        disabled={parentBusy || !isValidUUID(newParentId)}
        class="rounded border border-accent bg-accent/15 px-3 py-1 text-xs font-medium text-fg hover:bg-accent/25 disabled:opacity-50"
      >
        {t('admin.team_detail.add_parent_button')}
      </button>
    </div>
  </section>

  <section class="mb-6 max-w-2xl rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="mb-2 text-sm font-medium text-fg">{t('admin.team_detail.members_section')}</h3>
    <p class="mb-3 text-xs text-fg-muted">{t('admin.team_detail.members_intro')}</p>
    {#if members.length === 0}
      <p class="text-xs text-fg-muted">{t('admin.team_detail.no_members')}</p>
    {:else}
      <ul class="space-y-1">
        {#each members as m (m.user_ref)}
          <li class="flex items-center gap-2 text-xs">
            <a href="/admin/users/{m.user_ref}" class="flex-1 text-accent hover:underline">
              {m.display_name || m.username || `user ${m.user_ref}`}
            </a>
            <span class="text-fg-muted">{t('admin.team_detail.added_at')} {relativeAgo(m.added_at)}</span>
            <button
              type="button"
              onclick={() => removeMember(m.user_ref)}
              disabled={memberBusy}
              class="rounded border border-border bg-surface px-2 py-1 text-[11px] hover:border-danger hover:text-danger disabled:opacity-50"
            >
              {t('admin.team_detail.remove_member_button')}
            </button>
          </li>
        {/each}
      </ul>
    {/if}
    <div class="mt-3 flex gap-2">
      <input
        type="text"
        bind:value={newMemberRef}
        placeholder={t('admin.team_detail.add_member_placeholder')}
        class="flex-1 rounded border border-border-strong bg-surface px-2 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
      <button
        type="button"
        onclick={addMember}
        disabled={memberBusy || !newMemberRef}
        class="rounded border border-accent bg-accent/15 px-3 py-1 text-xs font-medium text-fg hover:bg-accent/25 disabled:opacity-50"
      >
        {t('admin.team_detail.add_member_button')}
      </button>
    </div>
  </section>

  <section class="max-w-2xl rounded-lg border border-danger/30 bg-danger/5 p-4">
    <button
      type="button"
      onclick={deleteTeam}
      disabled={deleting}
      class="rounded border border-danger bg-danger/10 px-3 py-1 text-xs font-medium text-danger hover:bg-danger/20 disabled:opacity-50"
    >
      {deleting ? t('admin.team_detail.deleting') : t('admin.team_detail.delete_button')}
    </button>
  </section>
{/if}
