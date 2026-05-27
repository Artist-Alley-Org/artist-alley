<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';

  interface UserPublic {
    ref: number;
    display_name: string;
    fullname?: string | null;
    bio?: string;
    avatar_url?: string | null;
    location?: string;
    website_url?: string | null;
  }

  let user = $state<UserPublic | null>(null);
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  // Form state — populated from `user` once it loads.
  let displayName = $state('');
  let bio = $state('');
  let avatarUrl = $state('');
  let location = $state('');
  let websiteUrl = $state('');

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    try {
      if (!auth.user) return;
      const { data } = await api.GET('/users/{ref}', {
        params: { path: { ref: auth.user.ref } },
      });
      if (data) {
        user = data as UserPublic;
        displayName = user.display_name === (user.fullname ?? '') ? '' : (user.display_name ?? '');
        // The display_name field on the response is the resolved
        // string. If it matches fullname (the next-tier fallback),
        // treat the actual profile column as empty for the form.
        bio = user.bio ?? '';
        avatarUrl = user.avatar_url ?? '';
        location = user.location ?? '';
        websiteUrl = user.website_url ?? '';
      }
    } finally {
      loading = false;
    }
  }

  async function save() {
    if (!auth.user || saving) return;
    saving = true;
    saved = false;
    error = null;
    try {
      const { data, error: apiErr } = await api.PATCH('/users/{ref}', {
        params: { path: { ref: auth.user.ref } },
        body: {
          display_name: displayName,
          bio,
          avatar_url: avatarUrl || null,
          location,
          website_url: websiteUrl || null,
        } as never,
      });
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('errors.save_failed');
      } else {
        user = data as UserPublic;
        saved = true;
      }
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>{t('account.profile.title')} — artist-alley</title></svelte:head>

<h2 class="mb-4 text-xl font-semibold">{t('account.profile.title')}</h2>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else}
  <form
    onsubmit={(e) => {
      e.preventDefault();
      void save();
    }}
    class="max-w-xl space-y-4"
  >
    <label class="block">
      <span class="text-sm text-fg-muted">{t('account.profile.display_name')}</span>
      <input
        type="text"
        bind:value={displayName}
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
        maxlength="100"
      />
      <span class="mt-1 block text-xs text-fg-muted">{t('account.profile.display_name_help')}</span>
    </label>

    <label class="block">
      <span class="text-sm text-fg-muted">{t('account.profile.bio')}</span>
      <textarea
        bind:value={bio}
        rows="3"
        class="mt-1 w-full resize-y rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
        maxlength="1000"
      ></textarea>
    </label>

    <label class="block">
      <span class="text-sm text-fg-muted">{t('account.profile.avatar_url')}</span>
      <input
        type="url"
        bind:value={avatarUrl}
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
        maxlength="1024"
      />
    </label>

    <label class="block">
      <span class="text-sm text-fg-muted">{t('account.profile.location')}</span>
      <input
        type="text"
        bind:value={location}
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
        maxlength="100"
      />
    </label>

    <label class="block">
      <span class="text-sm text-fg-muted">{t('account.profile.website_url')}</span>
      <input
        type="url"
        bind:value={websiteUrl}
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
        maxlength="500"
      />
    </label>

    {#if error}
      <p role="alert" class="rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-600">
        {error}
      </p>
    {/if}
    {#if saved}
      <p class="rounded border border-emerald-500/40 bg-emerald-500/10 px-3 py-2 text-sm text-emerald-700">
        {t('account.profile.saved')}
      </p>
    {/if}

    <button
      type="submit"
      disabled={saving}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {saving ? t('common.loading') : t('account.profile.save')}
    </button>
  </form>
{/if}
