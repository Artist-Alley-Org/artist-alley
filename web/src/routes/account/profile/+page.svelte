<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
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

  interface SelfEditGates {
    display_name: boolean;
    bio: boolean;
    avatar_url: boolean;
    location: boolean;
    website_url: boolean;
  }

  let user = $state<UserPublic | null>(null);
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  // Per-field gate snapshot. Defaults to all-editable so a load
  // failure leaves the form usable; the backend still enforces.
  let gates = $state<SelfEditGates>({
    display_name: true,
    bio: true,
    avatar_url: true,
    location: true,
    website_url: true,
  });

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
      const [profileRes, gatesRes] = await Promise.all([
        api.GET('/users/{ref}', { params: { path: { ref: auth.user.ref } } }),
        api.GET('/account/selfedit-gates'),
      ]);
      if (profileRes.data) {
        user = profileRes.data as UserPublic;
        // The display_name field on the response is the resolved
        // string. If it matches fullname (the next-tier fallback),
        // treat the actual profile column as empty for the form.
        displayName = user.display_name === (user.fullname ?? '') ? '' : (user.display_name ?? '');
        bio = user.bio ?? '';
        avatarUrl = user.avatar_url ?? '';
        location = user.location ?? '';
        websiteUrl = user.website_url ?? '';
      }
      if (gatesRes.data) {
        gates = gatesRes.data as SelfEditGates;
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
      // Only PATCH fields the operator allows. Stripping locked
      // fields means a user who never touched them can't trip the
      // gate either — the server's 422 is the backstop for tamper.
      const body: Record<string, string | null> = {};
      if (gates.display_name) body.display_name = displayName;
      if (gates.bio)          body.bio          = bio;
      if (gates.avatar_url)   body.avatar_url   = avatarUrl || null;
      if (gates.location)     body.location     = location;
      if (gates.website_url)  body.website_url  = websiteUrl || null;

      const { data, error: apiErr, response } = await api.PATCH('/users/{ref}', {
        params: { path: { ref: auth.user.ref } },
        body: body as never,
      });
      if (apiErr || !data) {
        // 422 from a gated field: show the field-specific message.
        const apiErrObj = apiErr as { error?: string; reason?: string; field?: string } | undefined;
        if (response?.status === 422 && apiErrObj?.reason === 'field_disabled_by_operator' && apiErrObj.field) {
          error = t('account.profile.field_disabled_by_operator', { field: apiErrObj.field });
        } else {
          error = apiErrObj?.error ?? t('errors.save_failed');
        }
        return;
      }
      user = data as UserPublic;
      saved = true;
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>{t('account.profile.title')} — {site.name}</title></svelte:head>

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
        disabled={!gates.display_name}
        data-testid="profile-display-name"
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        maxlength="100"
      />
      {#if !gates.display_name}
        <span class="mt-1 block text-xs text-fg-muted" data-testid="profile-display-name-locked">{t('account.profile.locked_hint')}</span>
      {:else}
        <span class="mt-1 block text-xs text-fg-muted">{t('account.profile.display_name_help')}</span>
      {/if}
    </label>

    <label class="block">
      <span class="text-sm text-fg-muted">{t('account.profile.bio')}</span>
      <textarea
        bind:value={bio}
        disabled={!gates.bio}
        data-testid="profile-bio"
        rows="3"
        class="mt-1 w-full resize-y rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        maxlength="1000"
      ></textarea>
      {#if !gates.bio}
        <span class="mt-1 block text-xs text-fg-muted" data-testid="profile-bio-locked">{t('account.profile.locked_hint')}</span>
      {/if}
    </label>

    <label class="block">
      <span class="text-sm text-fg-muted">{t('account.profile.avatar_url')}</span>
      <input
        type="url"
        bind:value={avatarUrl}
        disabled={!gates.avatar_url}
        data-testid="profile-avatar-url"
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        maxlength="1024"
      />
      {#if !gates.avatar_url}
        <span class="mt-1 block text-xs text-fg-muted" data-testid="profile-avatar-url-locked">{t('account.profile.locked_hint')}</span>
      {/if}
    </label>

    <label class="block">
      <span class="text-sm text-fg-muted">{t('account.profile.location')}</span>
      <input
        type="text"
        bind:value={location}
        disabled={!gates.location}
        data-testid="profile-location"
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        maxlength="100"
      />
      {#if !gates.location}
        <span class="mt-1 block text-xs text-fg-muted" data-testid="profile-location-locked">{t('account.profile.locked_hint')}</span>
      {/if}
    </label>

    <label class="block">
      <span class="text-sm text-fg-muted">{t('account.profile.website_url')}</span>
      <input
        type="url"
        bind:value={websiteUrl}
        disabled={!gates.website_url}
        data-testid="profile-website-url"
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none disabled:cursor-not-allowed disabled:opacity-50"
        maxlength="500"
      />
      {#if !gates.website_url}
        <span class="mt-1 block text-xs text-fg-muted" data-testid="profile-website-url-locked">{t('account.profile.locked_hint')}</span>
      {/if}
    </label>

    {#if error}
      <p role="alert" data-testid="profile-error" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
        {error}
      </p>
    {/if}
    {#if saved}
      <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success">
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
