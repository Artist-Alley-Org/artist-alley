<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Share dialog — the ACL editor for anything that has an ACL table.
  //
  // Was ShareCollectionModal, collection-only. #667 / #875 / #876 built
  // the whole RECIPIENT half of post sharing — a `post_acls` row confers
  // read, the grantee is notified, the guest list stays private — and
  // nothing in the product could create the row. `GET/POST/DELETE
  // /posts/{id}/acls` had zero frontend consumers.
  //
  // The fix is this component taking `{ kind, id }` rather than a second
  // post-only dialog. The two API surfaces are already symmetric — same
  // `AclCreate` request schema, same `AclEntry` response, same
  // `/{principal_type}/{principal_id}/{permission}` revoke sub-path — so
  // the only thing that actually varies is which path literal to call
  // and which permalink to build. A parallel implementation would have
  // diverged at the first edit (epic #665).
  //
  // Two things this surface does that the collection-only version did
  // not, both because a grant editor that lies is worse than no grant
  // editor:
  //
  //   1. EXPIRY. `AclCreate.expires_at` has always been settable by the
  //      API and both read rules enforce it (`expires_at IS NULL OR
  //      > NOW()`), but no UI offered it. A time-boxed share is what
  //      makes sharing safe to do casually: it stops granting when it
  //      lapses, with nothing to remember to revoke.
  //
  //   2. USER PRINCIPALS ONLY, resolved from a username. Both read
  //      rules hard-gate `principal_type = 'user'`
  //      (visibility.PostLiveGrantSQL, and predicate.go's collection
  //      branch) — role and team are ADR 0010 Layer 5 and grant
  //      nothing, notify nobody. Offering them in the picker wrote a
  //      row that LOOKED like access and was not, and generalising that
  //      to posts would have doubled the lie. Existing role / team rows
  //      still RENDER, marked inert — hiding a row that exists is the
  //      opposite failure.
  //
  //      And the principal id for a user grant is the BIGINT user.ref,
  //      which nobody knows. The old free-text field was placeholdered
  //      "id or username": typing a username inserted a row whose
  //      principal_id never matches `$n::BIGINT::TEXT`, so it granted
  //      nothing and notified nobody (the handler logs
  //      `posts.acl.notify.bad_principal` and moves on). So the field
  //      takes a username and resolves it through the public profile
  //      endpoint before the grant is written — the id we send is one
  //      the read rule can match, or we do not send one.
  //
  // The "copy link" affordance is still a stub for the access_links
  // table (Phase 1.11.C). For now it copies the canonical web URL.

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import Modal from './Modal.svelte';

  interface Props {
    open: boolean;
    /** Which ACL surface to drive. Selects the API path + permalink. */
    kind: 'post' | 'collection';
    /** UUID of the post / collection. */
    id: string;
    onclose: () => void;
  }

  let { open, kind, id, onclose }: Props = $props();

  interface Acl {
    principal_type: 'user' | 'role' | 'team';
    principal_id: string;
    permission: 'read' | 'write' | 'admin';
    granted_at: string;
    expires_at?: string | null;
  }

  let acls = $state<Acl[]>([]);
  /** principal_id → resolved profile, for `user` rows. The API returns
   *  the raw BIGINT ref, which is exactly as unreadable in the guest
   *  list as it was in the input field: an owner looking at
   *  `user:32 READ` cannot tell whether they shared with the right
   *  person, which makes the revoke button a guess. Resolved lazily
   *  after each load; a ref that no longer resolves (deleted user)
   *  falls back to the raw form rather than hiding the row. */
  let names = $state<Record<string, { username: string; display_name: string }>>({});
  let loading = $state(false);
  let error = $state<string | null>(null);
  let copied = $state(false);

  let username = $state('');
  let permission = $state<'read' | 'write' | 'admin'>('read');
  /** Relative expiry presets, in hours. `0` = no expiry. */
  let expiryPreset = $state<'0' | '1' | '24' | '168' | '720' | 'custom'>('0');
  let expiryCustom = $state('');
  let adding = $state(false);

  const permalinkPath = $derived(`/${kind === 'post' ? 'posts' : 'collections'}/${id}`);
  const permalink = $derived(
    typeof location === 'undefined' ? permalinkPath : `${location.origin}${permalinkPath}`,
  );

  $effect(() => {
    if (open) {
      void load();
    } else {
      acls = [];
      error = null;
      copied = false;
      username = '';
      expiryPreset = '0';
      expiryCustom = '';
    }
  });

  // The two ACL surfaces differ only in the path literal. openapi-fetch
  // types each path independently, so the branch is a ternary over two
  // literals rather than a computed string — that keeps the request and
  // response types checked instead of casting them away.
  function listAcls() {
    return kind === 'post'
      ? api.GET('/posts/{id}/acls', { params: { path: { id } } })
      : api.GET('/collections/{id}/acls', { params: { path: { id } } });
  }

  function createAcl(body: {
    principal_type: 'user';
    principal_id: string;
    permission: 'read' | 'write' | 'admin';
    expires_at?: string;
  }) {
    return kind === 'post'
      ? api.POST('/posts/{id}/acls', { params: { path: { id } }, body })
      : api.POST('/collections/{id}/acls', { params: { path: { id } }, body });
  }

  function destroyAcl(a: Acl) {
    const path = {
      id,
      principal_type: a.principal_type,
      principal_id: a.principal_id,
      permission: a.permission,
    };
    return kind === 'post'
      ? api.DELETE('/posts/{id}/acls/{principal_type}/{principal_id}/{permission}', {
          params: { path },
        })
      : api.DELETE('/collections/{id}/acls/{principal_type}/{principal_id}/{permission}', {
          params: { path },
        });
  }

  async function load() {
    loading = true;
    error = null;
    try {
      const { data, error: apiErr } = await listAcls();
      if (apiErr) {
        error = (apiErr as { error?: string }).error ?? t('share.error_load');
        return;
      }
      acls = (data ?? []) as Acl[];
      void resolveNames();
    } finally {
      loading = false;
    }
  }

  /** Fill `names` for every user principal we have not seen yet. Fired
   *  and forgotten: the list renders from the raw refs immediately and
   *  upgrades in place, so a slow or failing profile lookup delays
   *  nothing and loses no row. */
  async function resolveNames() {
    const wanted = [
      ...new Set(
        acls.filter((a) => a.principal_type === 'user').map((a) => a.principal_id),
      ),
    ].filter((ref) => !(ref in names) && /^\d+$/.test(ref));
    await Promise.all(
      wanted.map(async (ref) => {
        const { data } = await api.GET('/users/by-ref/{ref}', {
          params: { path: { ref: Number(ref) } },
        });
        if (data) names[ref] = { username: data.username, display_name: data.display_name };
      }),
    );
  }

  /** ISO instant for the chosen expiry, or undefined for a permanent
   *  grant. Presets are relative to now; "custom" is a datetime-local
   *  value, which `new Date()` reads in local time and `toISOString()`
   *  normalises to UTC for the API. */
  function expiresAt(): string | undefined {
    if (expiryPreset === 'custom') {
      if (!expiryCustom) return undefined;
      const d = new Date(expiryCustom);
      return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
    }
    const hours = Number(expiryPreset);
    if (!hours) return undefined;
    return new Date(Date.now() + hours * 3600_000).toISOString();
  }

  /** Resolve what the user typed into a `user.ref` the read rule can
   *  match. A username goes through the public profile endpoint; an
   *  all-digits input is taken as a ref but still verified to exist, so
   *  a typo becomes an error here rather than a dead row nobody
   *  notices. Returns null and sets `error` when unresolvable. */
  async function resolveUserRef(input: string): Promise<string | null> {
    const raw = input.trim().replace(/^@/, '');
    if (!raw) return null;
    if (/^\d+$/.test(raw)) {
      const { data, error: apiErr } = await api.GET('/users/by-ref/{ref}', {
        params: { path: { ref: Number(raw) } },
      });
      if (apiErr || !data) {
        error = t('share.error_no_such_user', { name: raw });
        return null;
      }
      return String(data.ref);
    }
    const { data, error: apiErr } = await api.GET('/users/by-username/{username}', {
      params: { path: { username: raw } },
    });
    if (apiErr || !data) {
      error = t('share.error_no_such_user', { name: raw });
      return null;
    }
    return String(data.ref);
  }

  async function addAcl() {
    if (!username.trim() || adding) return;
    adding = true;
    error = null;
    try {
      const principalId = await resolveUserRef(username);
      if (!principalId) return;
      const expires = expiresAt();
      const { error: apiErr } = await createAcl({
        principal_type: 'user',
        principal_id: principalId,
        permission,
        ...(expires ? { expires_at: expires } : {}),
      });
      if (apiErr) {
        error = (apiErr as { error?: string }).error ?? t('share.error_add');
        return;
      }
      username = '';
      expiryPreset = '0';
      expiryCustom = '';
      await load();
    } finally {
      adding = false;
    }
  }

  async function removeAcl(a: Acl) {
    const { error: apiErr } = await destroyAcl(a);
    if (apiErr) {
      error = (apiErr as { error?: string }).error ?? t('share.error_remove');
      return;
    }
    await load();
  }

  async function copyLink() {
    try {
      await navigator.clipboard.writeText(permalink);
      copied = true;
      setTimeout(() => (copied = false), 2000);
    } catch {
      // No-op: clipboard unavailable. The visible URL stays on
      // screen so the user can copy manually.
    }
  }

  function isExpired(a: Acl): boolean {
    return !!a.expires_at && new Date(a.expires_at).getTime() <= Date.now();
  }

  /** Role / team rows confer nothing today — see the header note. They
   *  are listed anyway, labelled, rather than hidden. */
  function isInert(a: Acl): boolean {
    return a.principal_type !== 'user';
  }
</script>

<Modal
  title={kind === 'post' ? t('share.title_post') : t('share.title_collection')}
  {open}
  {onclose}
  panelClass="max-w-xl"
>
  <div class="space-y-4">
    <!-- Copy link -->
    <section class="rounded-md border border-border bg-surface px-3 py-2">
      <div class="flex items-center justify-between gap-2 text-xs">
        <span class="truncate font-mono text-fg-muted">{permalink}</span>
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

    <!-- Add grant. User principals only: role / team grants are
         recorded by the API but confer no read (ADR 0010 Layer 5), so
         offering them here would be a control that does nothing. -->
    <section>
      <h3 class="mb-2 text-xs font-semibold uppercase tracking-wide text-fg-muted">{t('share.add_heading')}</h3>
      <div class="flex flex-wrap gap-2 sm:flex-nowrap">
        <input
          type="text"
          bind:value={username}
          placeholder={t('share.username_placeholder')}
          aria-label={t('share.username_label')}
          data-testid="share-username"
          class="min-w-0 flex-1 rounded border border-border-strong bg-surface px-2 py-1 text-sm"
        />
        <select
          bind:value={permission}
          aria-label={t('share.permission_label')}
          data-testid="share-permission"
          class="rounded border border-border-strong bg-surface px-2 py-1 text-sm"
        >
          <option value="read">{t('share.perm_read')}</option>
          <option value="write">{t('share.perm_write')}</option>
          <option value="admin">{t('share.perm_admin')}</option>
        </select>
        <button
          type="button"
          onclick={addAcl}
          disabled={!username.trim() || adding}
          data-testid="share-add"
          class="rounded-md bg-accent px-3 py-1 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
        >
          {adding ? '…' : t('common.add')}
        </button>
      </div>

      <!-- Expiry. A lapsed grant stops granting on its own — the read
           rule checks expires_at — so there is nothing to revoke. -->
      <div class="mt-2 flex flex-wrap items-center gap-2">
        <label for="share-expiry" class="text-xs text-fg-muted">{t('share.expiry_label')}</label>
        <select
          id="share-expiry"
          bind:value={expiryPreset}
          data-testid="share-expiry"
          class="rounded border border-border-strong bg-surface px-2 py-1 text-sm"
        >
          <option value="0">{t('share.expiry_never')}</option>
          <option value="1">{t('share.expiry_1h')}</option>
          <option value="24">{t('share.expiry_24h')}</option>
          <option value="168">{t('share.expiry_7d')}</option>
          <option value="720">{t('share.expiry_30d')}</option>
          <option value="custom">{t('share.expiry_custom')}</option>
        </select>
        {#if expiryPreset === 'custom'}
          <input
            type="datetime-local"
            bind:value={expiryCustom}
            aria-label={t('share.expiry_custom')}
            data-testid="share-expiry-custom"
            class="rounded border border-border-strong bg-surface px-2 py-1 text-sm"
          />
        {/if}
      </div>
      <p class="mt-1 text-xs text-fg-muted">{t('share.expiry_hint')}</p>
    </section>

    <!-- Existing grants -->
    <section>
      <h3 class="mb-2 text-xs font-semibold uppercase tracking-wide text-fg-muted">{t('share.existing_heading')}</h3>
      {#if loading}
        <p class="text-sm text-fg-muted">{t('common.loading')}</p>
      {:else if acls.length === 0}
        <p class="text-sm text-fg-muted" data-testid="share-empty">{t('share.empty')}</p>
      {:else}
        <ul class="divide-y divide-border rounded border border-border" data-testid="share-acl-list">
          {#each acls as a (`${a.principal_type}:${a.principal_id}:${a.permission}`)}
            <li class="flex items-center justify-between gap-2 px-3 py-2 text-sm">
              <span class="min-w-0">
                {#if a.principal_type === 'user' && names[a.principal_id]}
                  <span class="font-medium">{names[a.principal_id].display_name}</span>
                  <span class="ml-1 font-mono text-xs text-fg-muted">@{names[a.principal_id].username}</span>
                {:else}
                  <span class="font-mono text-fg-muted">{a.principal_type}:{a.principal_id}</span>
                {/if}
                <span class="ml-2 rounded bg-surface-elevated px-1.5 py-0.5 text-xs uppercase">{a.permission}</span>
                {#if isInert(a)}
                  <span class="ml-2 rounded bg-warning-container px-1.5 py-0.5 text-xs text-warning">
                    {t('share.inert_principal')}
                  </span>
                {/if}
                {#if a.expires_at}
                  <span
                    class="mt-0.5 block text-xs {isExpired(a) ? 'text-danger' : 'text-fg-muted'}"
                    title={new Date(a.expires_at).toLocaleString()}
                  >
                    {isExpired(a)
                      ? t('share.expired_at', { when: new Date(a.expires_at).toLocaleString() })
                      : t('share.expires_at', { when: new Date(a.expires_at).toLocaleString() })}
                  </span>
                {/if}
              </span>
              <button
                type="button"
                onclick={() => removeAcl(a)}
                class="shrink-0 text-xs text-fg-muted hover:text-danger"
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
</Modal>
