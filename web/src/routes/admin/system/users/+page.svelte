<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  let displayName = $state(true);
  let bio = $state(true);
  let avatarUrl = $state(true);
  let location = $state(true);
  let websiteUrl = $state(true);

  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/admin/system/users');
      if (data) {
        const g = data as {
          display_name: boolean;
          bio: boolean;
          avatar_url: boolean;
          location: boolean;
          website_url: boolean;
        };
        displayName = g.display_name;
        bio = g.bio;
        avatarUrl = g.avatar_url;
        location = g.location;
        websiteUrl = g.website_url;
      }
    } catch {
      error = t('admin.system.self_edit_gates.load_error');
    } finally {
      loading = false;
    }
  }

  async function save() {
    if (saving) return;
    saving = true;
    saved = false;
    error = null;
    try {
      const { data, error: apiErr } = await api.PATCH('/admin/system/users', {
        body: {
          display_name: displayName,
          bio,
          avatar_url: avatarUrl,
          location,
          website_url: websiteUrl,
        },
      });
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('errors.save_failed');
        return;
      }
      saved = true;
    } finally {
      saving = false;
    }
  }

  const fields = $derived([
    { key: 'display_name' as const, label: t('admin.system.self_edit_gates.field_display_name'), help: t('admin.system.self_edit_gates.field_help_display_name'), get value() { return displayName; }, set value(v: boolean) { displayName = v; } },
    { key: 'bio' as const,          label: t('admin.system.self_edit_gates.field_bio'),          help: t('admin.system.self_edit_gates.field_help_bio'),          get value() { return bio; },          set value(v: boolean) { bio = v; } },
    { key: 'avatar_url' as const,   label: t('admin.system.self_edit_gates.field_avatar_url'),   help: t('admin.system.self_edit_gates.field_help_avatar_url'),   get value() { return avatarUrl; },   set value(v: boolean) { avatarUrl = v; } },
    { key: 'location' as const,     label: t('admin.system.self_edit_gates.field_location'),     help: t('admin.system.self_edit_gates.field_help_location'),     get value() { return location; },     set value(v: boolean) { location = v; } },
    { key: 'website_url' as const,  label: t('admin.system.self_edit_gates.field_website_url'),  help: t('admin.system.self_edit_gates.field_help_website_url'),  get value() { return websiteUrl; },  set value(v: boolean) { websiteUrl = v; } },
  ]);
</script>

<svelte:head><title>{t('admin.system.self_edit_gates.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.system.self_edit_gates.title')}</h2>
<p class="mb-4 max-w-2xl text-sm text-fg-muted">{t('admin.system.self_edit_gates.intro')}</p>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else}
  <form
    onsubmit={(e) => {
      e.preventDefault();
      void save();
    }}
    class="max-w-2xl space-y-3"
  >
    {#each fields as field (field.key)}
      <label class="flex cursor-pointer items-start gap-3 rounded border border-border bg-surface px-3 py-2">
        <input
          type="checkbox"
          checked={field.value}
          onchange={(e) => (field.value = (e.currentTarget as HTMLInputElement).checked)}
          data-testid="self-edit-gate-{field.key}"
          class="mt-0.5 size-4 accent-accent"
        />
        <span class="flex-1">
          <span class="block text-sm font-medium text-fg">{field.label}</span>
          <span class="block text-xs text-fg-muted">{field.help}</span>
        </span>
      </label>
    {/each}

    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
    {/if}
    {#if saved}
      <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success" data-testid="self-edit-gates-saved">{t('admin.system.self_edit_gates.saved')}</p>
    {/if}

    <button
      type="submit"
      disabled={saving}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {saving ? t('common.loading') : t('admin.system.self_edit_gates.save')}
    </button>
  </form>
{/if}
