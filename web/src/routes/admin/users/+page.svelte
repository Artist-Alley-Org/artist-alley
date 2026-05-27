<script lang="ts">
  // Admin users list — pulls a paginated set of UserPublic rows.
  //
  // Phase 1.16 ships this read-only with a click-through to a per-user
  // role assignment page. Group memberships, suspension, etc. land
  // when the corresponding API endpoints do.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import Avatar from '$components/Avatar.svelte';

  interface UserPublic {
    ref: number;
    username: string;
    display_name: string;
    fullname?: string | null;
    avatar_url?: string | null;
    location?: string;
    member_since: string;
    post_count: number;
  }

  let users = $state<UserPublic[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    error = null;
    try {
      // No global "list users" endpoint yet — we use the legacy /users
      // surface when it lands. Until then, pull from /posts via the
      // author_ref index isn't right either. For now show a placeholder.
      // TODO: wire `GET /admin/users` (paginated) in a follow-up.
      users = [];
    } finally {
      loading = false;
    }
  }
</script>

<svelte:head><title>{t('admin.users.title')} — artist-alley</title></svelte:head>

<h2 class="mb-4 text-xl font-semibold">{t('admin.users.title')}</h2>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-600">{error}</p>
{:else}
  <p class="rounded-md bg-surface-elevated px-4 py-6 text-center text-sm text-fg-muted">
    {t('admin.users.no_users')} — listing endpoint lands in a follow-up phase. For now you can edit role assignments at /admin/users/{'\u007B'}ref{'\u007D'} once you know a user's ref.
  </p>
  {#if users.length > 0}
    <table class="w-full text-sm">
      <thead class="text-left text-xs uppercase tracking-wider text-fg-muted">
        <tr>
          <th class="py-2"></th>
          <th class="py-2">{t('admin.users.username')}</th>
          <th class="py-2">{t('admin.users.fullname')}</th>
          <th class="py-2">Posts</th>
          <th class="py-2">{t('admin.users.actions')}</th>
        </tr>
      </thead>
      <tbody>
        {#each users as u (u.ref)}
          <tr class="border-t border-border">
            <td class="py-2"><Avatar name={u.display_name} src={u.avatar_url} sizeClass="h-6 w-6" /></td>
            <td class="py-2">@{u.username}</td>
            <td class="py-2 text-fg-muted">{u.fullname ?? ''}</td>
            <td class="py-2 text-fg-muted">{u.post_count}</td>
            <td class="py-2">
              <a href="/admin/users/{u.ref}" class="text-accent hover:underline">{t('common.edit')}</a>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
{/if}
