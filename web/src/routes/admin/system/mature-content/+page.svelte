<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // #1116, ADR 0090 — whether this install allows mature content.
  //
  // ONE checkbox, and it is the operator's half of a three-part rule:
  // a reader sees mature work only when they are signed in AND have
  // opted in AND this is on. Turning it on therefore shows nothing to
  // anybody by itself, which the help text says out loud — an operator
  // who expects a flood after ticking it has misread the control.
  //
  // The page states the two consequences of turning it OFF, because
  // both are surprising and only one is reversible-looking:
  //   * flagged work disappears for everyone except its owner and a
  //     site admin — the artist keeps their own library, and a
  //     moderator keeps the ability to moderate what the switch hid;
  //   * nothing is erased. Flags survive, so switching back on restores
  //     the library exactly. Saying so is what makes the switch safe to
  //     experiment with; without it an operator has to guess whether
  //     they are about to destroy their artists' labels.
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { site } from '$stores/site.svelte';
  import { t } from '$stores/lang.svelte';
  import { auth } from '$stores/auth.svelte';

  // TRUE is the unconfigured default, matching the server's
  // absent-means-allowed reading. Seeding this `false` would flash
  // "disallowed" on every load before the GET lands, which reads as the
  // install having been configured that way.
  let allowed = $state(true);
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  const canWrite = $derived(auth.can('system.config.write'));

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/admin/system/mature-content');
      if (data) allowed = data.allowed !== false;
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
      const { data, error: apiErr } = await api.PATCH('/admin/system/mature-content', {
        body: { allowed },
      });
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('errors.save_failed');
        return;
      }
      // Redraw from what was STORED, not from what we sent.
      allowed = data.allowed !== false;
      // The switch rides the session response as one of the three
      // conjuncts (CurrentUser.mature_content_allowed), and this admin
      // is also a reader: re-pull /auth/me so their own account page
      // and upload form stop offering a control this install no longer
      // honours, without a reload.
      void auth.refresh();
      saved = true;
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>{t('admin.system.mature_content.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.system.mature_content.title')}</h2>
<p class="mb-6 max-w-3xl text-sm text-fg-muted">{t('admin.system.mature_content.description')}</p>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else}
  <div class="max-w-3xl">
    <fieldset class="mb-4 space-y-2 rounded border border-border bg-surface p-4">
      <legend class="px-1 text-sm font-medium text-fg">
        {t('admin.system.mature_content.legend')}
      </legend>
      <label class="flex items-start gap-3 rounded px-1 py-1.5 text-sm">
        <input
          type="checkbox"
          class="mt-0.5 h-4 w-4"
          data-testid="admin-mature-allowed"
          bind:checked={allowed}
          disabled={!canWrite || saving}
        />
        <span>
          <span class="block font-medium text-fg">{t('admin.system.mature_content.allow')}</span>
          <span class="block text-xs text-fg-muted">
            {t('admin.system.mature_content.allow_help')}
          </span>
        </span>
      </label>
      <p class="pt-1 text-xs text-fg-muted">{t('admin.system.mature_content.off_note')}</p>
    </fieldset>

    {#if error}
      <p class="mb-3 text-sm text-danger" role="alert">{error}</p>
    {/if}

    <div class="flex items-center gap-3">
      <button
        type="button"
        onclick={save}
        disabled={!canWrite || saving}
        class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-on-accent transition-colors hover:bg-accent/90 disabled:cursor-not-allowed disabled:opacity-50"
      >
        {saving ? t('common.saving') : t('common.save')}
      </button>
      {#if saved}
        <span class="text-sm text-success">{t('admin.system.mature_content.saved')}</span>
      {/if}
      {#if !canWrite}
        <span class="text-sm text-fg-muted">{t('admin.system.mature_content.read_only')}</span>
      {/if}
    </div>
  </div>
{/if}
