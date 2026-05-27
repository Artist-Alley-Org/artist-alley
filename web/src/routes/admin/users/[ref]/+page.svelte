<script lang="ts">
  // Per-user admin detail — show profile + role assignment.

  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import Avatar from '$components/Avatar.svelte';

  interface UserPublic {
    ref: number;
    username: string;
    display_name: string;
    fullname?: string | null;
    avatar_url?: string | null;
    member_since: string;
  }
  interface Role {
    id: string;
    name: string;
  }

  const ref = $derived(Number(page.params.ref));

  let user = $state<UserPublic | null>(null);
  let roles = $state<Role[]>([]);
  let selectedRole = $state<string>('');
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    error = null;
    try {
      const [u, rs] = await Promise.all([
        api.GET('/users/{ref}', { params: { path: { ref } } }),
        api.GET('/auth/roles'),
      ]);
      if (u.error || !u.data) {
        error = (u.error as { error?: string } | undefined)?.error ?? 'User not found.';
        return;
      }
      user = u.data as UserPublic;
      if (rs.data) roles = (rs.data as { items?: Role[] }).items ?? (rs.data as unknown as Role[]);
    } finally {
      loading = false;
    }
  }

  async function saveRole() {
    if (!selectedRole || saving) return;
    saving = true;
    saved = false;
    try {
      await api.PUT('/auth/users/{ref}/role', {
        params: { path: { ref } },
        body: { role_id: selectedRole },
      });
      saved = true;
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed.';
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>User {ref} — artist-alley</title></svelte:head>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-600">{error}</p>
{:else if user}
  <header class="mb-6 flex items-center gap-3">
    <Avatar name={user.display_name} src={user.avatar_url} sizeClass="h-12 w-12" />
    <div>
      <h2 class="text-xl font-semibold">{t('admin.user_detail.title', { username: user.username })}</h2>
      <p class="text-xs text-fg-muted">ref {user.ref} · member since {new Date(user.member_since).toLocaleDateString()}</p>
    </div>
  </header>

  <section class="max-w-xl space-y-3 rounded-lg border border-border bg-surface p-4">
    <h3 class="text-sm font-medium text-fg">{t('admin.user_detail.role_label')}</h3>
    <select
      bind:value={selectedRole}
      class="w-full rounded border border-border bg-surface-elevated px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
    >
      <option value="">—</option>
      {#each roles as r (r.id)}
        <option value={r.id}>{r.name}</option>
      {/each}
    </select>
    <button
      type="button"
      onclick={saveRole}
      disabled={!selectedRole || saving}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {saving ? t('common.loading') : t('admin.user_detail.role_save')}
    </button>
    {#if saved}
      <p class="text-sm text-emerald-600">{t('admin.user_detail.role_saved')}</p>
    {/if}
  </section>
{/if}
