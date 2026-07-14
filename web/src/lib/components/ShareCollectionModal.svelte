<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Share dialog — minimal ACL editor.
  //
  // Lists current ACL rows and lets the owner add/revoke principal
  // grants. Principals are typed (user / role / team) and the id is
  // a free-text field for the skeleton; a future iteration will swap
  // in autocompletes against /users + /roles + /teams.
  //
  // The "copy link" affordance is a stub for the access_links table
  // (Phase 1.11.C). For now it copies the canonical web URL.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import CollectionModal from './CollectionModal.svelte';

  interface Props {
    open: boolean;
    collectionId: string;
    onclose: () => void;
  }

  let { open, collectionId, onclose }: Props = $props();

  interface Acl {
    principal_type: 'user' | 'role' | 'team';
    principal_id: string;
    permission: 'read' | 'write' | 'admin';
    granted_at: string;
  }

  let acls = $state<Acl[]>([]);
  let loading = $state(false);
  let error = $state<string | null>(null);
  let copied = $state(false);

  let principalType = $state<'user' | 'role' | 'team'>('user');
  let principalId = $state('');
  let permission = $state<'read' | 'write' | 'admin'>('read');
  let adding = $state(false);

  $effect(() => {
    if (open) {
      void load();
    } else {
      acls = [];
      error = null;
      copied = false;
    }
  });

  async function load() {
    loading = true;
    error = null;
    try {
      const { data, error: apiErr } = await api.GET('/collections/{id}/acls', {
        params: { path: { id: collectionId } },
      });
      if (apiErr) {
        error = (apiErr as { error?: string }).error ?? t('collections.error_load_share');
        return;
      }
      acls = (data ?? []) as Acl[];
    } finally {
      loading = false;
    }
  }

  async function addAcl() {
    if (!principalId.trim() || adding) return;
    adding = true;
    error = null;
    try {
      const { error: apiErr } = await api.POST('/collections/{id}/acls', {
        params: { path: { id: collectionId } },
        body: {
          principal_type: principalType,
          principal_id: principalId.trim(),
          permission,
        },
      });
      if (apiErr) {
        error = (apiErr as { error?: string }).error ?? t('collections.error_add_share');
        return;
      }
      principalId = '';
      await load();
    } finally {
      adding = false;
    }
  }

  async function removeAcl(a: Acl) {
    const { error: apiErr } = await api.DELETE('/collections/{id}/acls/{principal_type}/{principal_id}/{permission}', {
      params: {
        path: {
          id: collectionId,
          principal_type: a.principal_type,
          principal_id: a.principal_id,
          permission: a.permission,
        },
      },
    });
    if (apiErr) {
      error = (apiErr as { error?: string }).error ?? t('collections.error_remove_share');
      return;
    }
    await load();
  }

  async function copyLink() {
    const link = `${location.origin}/collections/${collectionId}`;
    try {
      await navigator.clipboard.writeText(link);
      copied = true;
      setTimeout(() => (copied = false), 2000);
    } catch {
      // No-op: clipboard unavailable. The visible URL stays on
      // screen so the user can copy manually.
    }
  }
</script>

<CollectionModal title={t('collections.share_title')} {open} {onclose} panelClass="max-w-xl">
  <div class="space-y-4">
    <!-- Copy link -->
    <section class="rounded-md border border-border bg-surface px-3 py-2">
      <div class="flex items-center justify-between gap-2 text-xs">
        <span class="truncate font-mono text-fg-muted">{location.origin}/collections/{collectionId}</span>
        <button
          type="button"
          onclick={copyLink}
          class="shrink-0 rounded border border-border bg-surface-elevated px-2 py-1 text-xs hover:bg-surface"
        >
          {copied ? t('common.copied') : t('common.copy_link')}
        </button>
      </div>
    </section>

    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
        {error}
      </p>
    {/if}

    <!-- Add grant -->
    <section>
      <h3 class="mb-2 text-xs font-semibold uppercase tracking-wide text-fg-muted">{t('collections.share_add_heading')}</h3>
      <div class="grid grid-cols-[auto_1fr_auto_auto] gap-2">
        <select bind:value={principalType} class="rounded border border-border bg-surface px-2 py-1 text-sm">
          <option value="user">{t('collections.principal_user')}</option>
          <option value="role">{t('collections.principal_role')}</option>
          <option value="team">{t('collections.principal_team')}</option>
        </select>
        <input
          type="text"
          bind:value={principalId}
          placeholder={t('collections.principal_id_placeholder')}
          class="rounded border border-border bg-surface px-2 py-1 text-sm"
        />
        <select bind:value={permission} class="rounded border border-border bg-surface px-2 py-1 text-sm">
          <option value="read">{t('collections.perm_read')}</option>
          <option value="write">{t('collections.perm_write')}</option>
          <option value="admin">{t('collections.perm_admin')}</option>
        </select>
        <button
          type="button"
          onclick={addAcl}
          disabled={!principalId.trim() || adding}
          class="rounded-md bg-accent px-3 py-1 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
        >
          {adding ? '…' : t('common.add')}
        </button>
      </div>
    </section>

    <!-- Existing grants -->
    <section>
      <h3 class="mb-2 text-xs font-semibold uppercase tracking-wide text-fg-muted">{t('collections.share_existing_heading')}</h3>
      {#if loading}
        <p class="text-sm text-fg-muted">{t('common.loading')}</p>
      {:else if acls.length === 0}
        <p class="text-sm text-fg-muted">{t('collections.share_empty')}</p>
      {:else}
        <ul class="divide-y divide-border rounded border border-border">
          {#each acls as a (`${a.principal_type}:${a.principal_id}:${a.permission}`)}
            <li class="flex items-center justify-between gap-2 px-3 py-2 text-sm">
              <span>
                <span class="font-mono text-fg-muted">{a.principal_type}:{a.principal_id}</span>
                <span class="ml-2 rounded bg-surface-elevated px-1.5 py-0.5 text-xs uppercase">{a.permission}</span>
              </span>
              <button
                type="button"
                onclick={() => removeAcl(a)}
                class="text-xs text-fg-muted hover:text-danger"
                aria-label={t('common.remove')}
              >
                {t('common.remove')}
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </section>
  </div>

  {#snippet footer()}
    <button
      type="button"
      onclick={onclose}
      class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated"
    >
      {t('common.done')}
    </button>
  {/snippet}
</CollectionModal>
