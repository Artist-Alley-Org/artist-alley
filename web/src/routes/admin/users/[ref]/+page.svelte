<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Per-user admin detail — profile + role assignment + lifecycle
  // status (Phase 1.17.B).

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import Avatar from '$components/Avatar.svelte';
  import {
    type AdminUser,
    type AdminUserStatus,
    statusBadgeClass,
    validTargetsFrom,
    transitionVerb,
    relativeAgo,
  } from '$lib/admin/users';

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

  // Lifecycle state (1.17.B). We try to read the user's current
  // status from the admin list (one-off scoped fetch keyed by ref);
  // falls back to "active" if the API doesn't return the row (the
  // public profile doesn't expose status, so we have to ask the
  // admin list).
  let status = $state<AdminUserStatus>('active');
  let statusReason = $state('');
  let statusSaving = $state(false);
  let statusMessage = $state<{ kind: 'ok' | 'noop'; text: string } | null>(null);

  // Phase 1.19.D — persistent per-username lockout state. Read from
  // the AdminUser row (which now carries lockout_until +
  // failed_login_count). The badge renders when lockout_until > now;
  // the unlock button renders when locked AND caller has
  // `auth.unlock`. Both are effectively-derived per-render.
  let lockoutUntil = $state<Date | null>(null);
  let failedLoginCount = $state<number>(0);
  let unlockBusy = $state(false);
  let unlockMessage = $state<string | null>(null);
  const isLocked = $derived(lockoutUntil !== null && lockoutUntil > new Date());
  const canUnlock = $derived(auth.can('auth.unlock'));

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    error = null;
    try {
      const [u, rs, adminRow] = await Promise.all([
        api.GET('/users/{ref}', { params: { path: { ref } } }),
        api.GET('/auth/roles'),
        // Fetch only this user via the admin list with a precise
        // search query; the API returns AdminUser which carries
        // status. Limit=1 because we already know the row.
        api.GET('/admin/users', { params: { query: { q: '', limit: 200 } } }),
      ]);
      if (u.error || !u.data) {
        error = (u.error as { error?: string } | undefined)?.error ?? 'User not found.';
        return;
      }
      user = u.data as UserPublic;
      if (rs.data) roles = (rs.data as { items?: Role[] }).items ?? (rs.data as unknown as Role[]);

      if (adminRow.data) {
        const page = adminRow.data as unknown as { items: AdminUser[] };
        const me = page.items.find((u) => u.ref === ref);
        if (me) {
          status = me.status;
          lockoutUntil = me.lockout_until ? new Date(me.lockout_until) : null;
          failedLoginCount = me.failed_login_count ?? 0;
        }
      }
    } finally {
      loading = false;
    }
  }

  // Phase 1.19.D — admin unlock.
  async function unlockAccount() {
    if (unlockBusy) return;
    unlockBusy = true;
    unlockMessage = null;
    try {
      const r = await api.POST('/admin/users/{ref}/unlock-account', {
        params: { path: { ref } },
      });
      if (r.error || !r.data) {
        unlockMessage =
          (r.error as { error?: string } | undefined)?.error ?? t('admin.user_detail.lockout_unlock_failed');
        return;
      }
      const result = r.data as unknown as { unlocked: boolean; prior_failed_count: number };
      lockoutUntil = null;
      failedLoginCount = 0;
      unlockMessage = result.unlocked
        ? t('admin.user_detail.lockout_unlock_result_ok', { count: result.prior_failed_count })
        : t('admin.user_detail.lockout_unlock_result_noop');
    } finally {
      unlockBusy = false;
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

  async function saveStatus(next: AdminUserStatus) {
    if (statusSaving) return;
    statusSaving = true;
    statusMessage = null;
    try {
      const r = await api.PUT('/admin/users/{ref}/status', {
        params: { path: { ref } },
        body: { status: next, reason: statusReason || undefined },
      });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to update status.';
        return;
      }
      const result = r.data as unknown as { status: AdminUserStatus; changed: boolean };
      status = result.status;
      statusReason = '';
      statusMessage = result.changed
        ? { kind: 'ok', text: t('admin.user_detail.status_updated', { status: statusLabel(result.status) }) }
        : { kind: 'noop', text: t('admin.user_detail.status_no_change') };
    } finally {
      statusSaving = false;
    }
  }

  function statusLabel(s: AdminUserStatus): string {
    if (s === 'active') return t('admin.users.status_active');
    if (s === 'pending') return t('admin.users.status_pending');
    if (s === 'archived') return t('admin.users.status_archived');
    return t('admin.users.status_disabled');
  }

  // Phase 1.17.A — typed transition buttons. The matrix lives in
  // $lib/admin/users (mirrors internal/users/userstate.go on the
  // backend); we render one button per legal target with a verb-
  // labelled action so the operator sees "Approve / Disable /
  // Archive / Restore" rather than four ambiguous "Set to X"
  // buttons. Disabled while a save is in flight.
  const transitionActions = $derived(
    validTargetsFrom(status).map((to) => ({
      to,
      verb: transitionVerb(status, to),
      label: t(`admin.user_detail.transition_${transitionVerb(status, to)}`),
    })),
  );

  // Password reset (Phase 1.17.D).
  let resetReason = $state('');
  let resetting = $state(false);
  let resetResult = $state<{ password: string; copied: boolean } | null>(null);

  async function resetPassword() {
    if (resetting) return;
    resetting = true;
    try {
      const r = await api.POST('/admin/users/{ref}/password-reset', {
        params: { path: { ref } },
        body: { reason: resetReason || undefined },
      });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to reset password.';
        return;
      }
      const result = r.data as unknown as { temporary_password: string };
      resetResult = { password: result.temporary_password, copied: false };
      resetReason = '';
    } finally {
      resetting = false;
    }
  }

  async function copyReset() {
    if (!resetResult) return;
    try {
      await navigator.clipboard.writeText(resetResult.password);
      resetResult.copied = true;
    } catch {
      // clipboard write blocked (insecure context, etc.) — leave the
      // password visible so the admin can copy it manually.
    }
  }

  // Impersonation (Phase 1.19.A-2).
  let impReason = $state('');
  let impStarting = $state(false);
  let impError = $state<string | null>(null);

  async function startImpersonation() {
    if (impStarting) return;
    if (!confirm(t('admin.user_detail.impersonate_confirm', { username: user?.username ?? '' }))) return;
    impStarting = true;
    impError = null;
    try {
      const r = await api.POST('/admin/users/{ref}/impersonate', {
        params: { path: { ref } },
        body: { reason: impReason || undefined },
      });
      if (r.error || !r.data) {
        impError = (r.error as { error?: string } | undefined)?.error ?? t('admin.user_detail.impersonate_failed');
        return;
      }
      // Cookie has been rotated server-side. Refresh auth state +
      // navigate to the target's view (anywhere they have
      // permission to land — home is the safe default).
      await auth.refresh();
      await goto('/');
    } finally {
      impStarting = false;
    }
  }

  // Capability grants + revokes (Phase 1.17.F).
  interface CapabilityOverride {
    capability: string;
    team_id?: string | null;
    note?: string;
    granted_by?: number | null;
    granted_at: string;
  }
  interface Capability {
    code: string;
    description: string;
  }

  let allCaps = $state<Capability[]>([]);
  let grants = $state<CapabilityOverride[]>([]);
  let revokes = $state<CapabilityOverride[]>([]);
  let overridesLoading = $state(false);
  let overridesError = $state<string | null>(null);

  let newGrantCap = $state('');
  let newGrantTeam = $state('');
  let newGrantNote = $state('');
  let grantBusy = $state(false);

  let newRevokeCap = $state('');
  let newRevokeTeam = $state('');
  let newRevokeNote = $state('');
  let revokeBusy = $state(false);

  async function loadOverrides() {
    overridesLoading = true;
    overridesError = null;
    try {
      const [caps, overrides] = await Promise.all([
        api.GET('/auth/capabilities'),
        api.GET('/admin/users/{ref}/capabilities', { params: { path: { ref } } }),
      ]);
      if (caps.data) {
        const payload = caps.data as unknown as { items?: Capability[] } | Capability[];
        allCaps = Array.isArray(payload) ? payload : (payload.items ?? []);
      }
      if (overrides.error || !overrides.data) {
        overridesError = (overrides.error as { error?: string } | undefined)?.error
          ?? t('admin.user_detail.overrides_load_error');
        return;
      }
      const o = overrides.data as unknown as { grants: CapabilityOverride[]; revokes: CapabilityOverride[] };
      grants = o.grants ?? [];
      revokes = o.revokes ?? [];
    } finally {
      overridesLoading = false;
    }
  }

  async function addGrant() {
    if (grantBusy || !newGrantCap) return;
    grantBusy = true;
    overridesError = null;
    try {
      const r = await api.POST('/admin/users/{ref}/grants', {
        params: { path: { ref } },
        body: {
          capability: newGrantCap,
          team_id: newGrantTeam || undefined,
          note: newGrantNote || undefined,
        },
      });
      if (r.error) {
        overridesError = (r.error as { error?: string } | undefined)?.error
          ?? t('admin.user_detail.overrides_add_grant_error');
        return;
      }
      newGrantCap = '';
      newGrantTeam = '';
      newGrantNote = '';
      await loadOverrides();
    } finally {
      grantBusy = false;
    }
  }

  async function removeGrant(capability: string, teamID: string | null | undefined) {
    if (grantBusy) return;
    grantBusy = true;
    overridesError = null;
    try {
      const r = await api.DELETE('/admin/users/{ref}/grants/{capability}', {
        params: { path: { ref, capability }, query: teamID ? { team_id: teamID } : {} },
      });
      if (r.error) {
        overridesError = (r.error as { error?: string } | undefined)?.error
          ?? t('admin.user_detail.overrides_remove_grant_error');
        return;
      }
      await loadOverrides();
    } finally {
      grantBusy = false;
    }
  }

  async function addRevoke() {
    if (revokeBusy || !newRevokeCap) return;
    revokeBusy = true;
    overridesError = null;
    try {
      const r = await api.POST('/admin/users/{ref}/revokes', {
        params: { path: { ref } },
        body: {
          capability: newRevokeCap,
          team_id: newRevokeTeam || undefined,
          note: newRevokeNote || undefined,
        },
      });
      if (r.error) {
        overridesError = (r.error as { error?: string } | undefined)?.error
          ?? t('admin.user_detail.overrides_add_revoke_error');
        return;
      }
      newRevokeCap = '';
      newRevokeTeam = '';
      newRevokeNote = '';
      await loadOverrides();
    } finally {
      revokeBusy = false;
    }
  }

  async function removeRevoke(capability: string, teamID: string | null | undefined) {
    if (revokeBusy) return;
    revokeBusy = true;
    overridesError = null;
    try {
      const r = await api.DELETE('/admin/users/{ref}/revokes/{capability}', {
        params: { path: { ref, capability }, query: teamID ? { team_id: teamID } : {} },
      });
      if (r.error) {
        overridesError = (r.error as { error?: string } | undefined)?.error
          ?? t('admin.user_detail.overrides_remove_revoke_error');
        return;
      }
      await loadOverrides();
    } finally {
      revokeBusy = false;
    }
  }

  function shortTeam(id: string | null | undefined): string {
    if (!id) return t('admin.user_detail.overrides_global');
    return id.slice(0, 8);
  }

  // ── Active sessions (admin view; Phase 1.17.C) ──────────────────
  // Same row shape as /account/sessions, minus `current` (the admin is
  // viewing someone else's sessions). Any session is revocable.
  //
  // `ip` is optional because the server OMITS it for callers without
  // `users.pii.read` (#573, ADR 0072) — the same treatment audit gives
  // actor IPs. The row renders it with `{#if s.ip}`, so absence degrades
  // to one fewer line rather than a dash or a blank. Do NOT re-derive
  // the rule with a client-side auth.can(): a second copy of a server
  // rule is free to disagree with the response it is describing.
  interface SessionRow {
    id: string;
    created_at: string;
    last_used_at: string;
    expires_at?: string | null;
    ip?: string | null;
    user_agent?: string | null;
  }

  let sessions = $state<SessionRow[]>([]);
  let sessionsLoading = $state(true);
  let sessionsError = $state<string | null>(null);
  let revokingSession = $state<string | null>(null);

  async function loadSessions() {
    sessionsLoading = true;
    sessionsError = null;
    try {
      const r = await api.GET('/admin/users/{ref}/sessions', { params: { path: { ref } } });
      if (r.error || !r.data) {
        sessionsError = (r.error as { error?: string } | undefined)?.error
          ?? t('admin.user_detail.sessions_load_error');
        return;
      }
      sessions = (r.data as unknown as { items: SessionRow[] }).items ?? [];
    } finally {
      sessionsLoading = false;
    }
  }

  async function revokeSession(id: string) {
    if (revokingSession) return;
    revokingSession = id;
    sessionsError = null;
    try {
      const r = await api.DELETE('/admin/users/{ref}/sessions/{id}', { params: { path: { ref, id } } });
      if (r.error) {
        sessionsError = (r.error as { error?: string } | undefined)?.error
          ?? t('admin.user_detail.sessions_revoke_error');
        return;
      }
      sessions = sessions.filter((s) => s.id !== id);
    } finally {
      revokingSession = null;
    }
  }

  function deviceLabel(ua: string | null | undefined): string {
    if (!ua) return t('admin.user_detail.sessions_ua_unknown');
    if (/iPhone|iPad|iPod/.test(ua)) return 'iOS';
    if (/Android/.test(ua)) return 'Android';
    if (/Macintosh/.test(ua)) return 'macOS';
    if (/Windows/.test(ua)) return 'Windows';
    if (/Linux/.test(ua)) return 'Linux';
    return ua.slice(0, 40);
  }

  onMount(() => { void loadOverrides(); void loadSessions(); });
</script>

<svelte:head><title>User {ref} — {site.name}</title></svelte:head>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
{:else if user}
  <header class="mb-6 flex items-center gap-3">
    <Avatar name={user.display_name} src={user.avatar_url} sizeClass="h-12 w-12" />
    <div>
      <h2 class="text-xl font-semibold">{t('admin.user_detail.title', { username: user.username })}</h2>
      <p class="text-xs text-fg-muted">
        ref {user.ref} · member since {new Date(user.member_since).toLocaleDateString()}
      </p>
    </div>
    <span class={`ml-auto inline-block rounded px-2 py-0.5 text-[10px] uppercase tracking-wider ${statusBadgeClass(status)}`}>
      {statusLabel(status)}
    </span>
  </header>

  <section class="mb-6 max-w-xl space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="text-sm font-medium text-fg">{t('admin.user_detail.role_label')}</h3>
    <select
      bind:value={selectedRole}
      class="w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
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
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {saving ? t('common.loading') : t('admin.user_detail.role_save')}
    </button>
    {#if saved}
      <p class="text-sm text-success">{t('admin.user_detail.role_saved')}</p>
    {/if}
  </section>

  <section class="max-w-xl space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="text-sm font-medium text-fg">{t('admin.user_detail.status_section')}</h3>
    <p class="text-xs text-fg-muted">{t('admin.user_detail.status_intro')}</p>
    <p class="text-xs text-fg-muted">{t('admin.user_detail.status_current', { status: statusLabel(status) })}</p>

    <label class="block text-xs">
      <span class="mb-1 block text-fg-muted">{t('admin.user_detail.status_reason')}</span>
      <input
        type="text"
        bind:value={statusReason}
        placeholder={t('admin.user_detail.status_reason_placeholder')}
        maxlength="500"
        class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>

    <!-- Phase 1.17.A — render one button per LEGAL transition target,
         derived from the typed matrix. Replaces the always-on three
         buttons (which let an operator click "Pending" on an active
         user and get a 400) — operators only see actions that will
         actually work, and the verb is unambiguous (Approve / Disable
         / Archive / Restore vs the old "Set to X" framing). -->
    <div class="flex flex-wrap gap-2" data-testid="admin-user-transitions">
      {#each transitionActions as action (action.to)}
        <button
          type="button"
          onclick={() => saveStatus(action.to)}
          disabled={statusSaving}
          data-testid={`transition-${action.verb}`}
          class={
            'rounded border px-3 py-1 text-xs font-medium disabled:opacity-50 ' +
            (action.verb === 'approve' || action.verb === 'restore'
              ? 'border-success bg-success/10 text-success hover:bg-success/20'
              : action.verb === 'disable'
                ? 'border-danger bg-danger/10 text-danger hover:bg-danger/20'
                : 'border-muted bg-muted/10 text-fg-muted hover:bg-muted/20')
          }
        >
          {action.label}
        </button>
      {/each}
      {#if transitionActions.length === 0}
        <p class="text-xs text-fg-muted">{t('admin.user_detail.status_terminal')}</p>
      {/if}
    </div>

    {#if statusMessage}
      <p class={statusMessage.kind === 'ok' ? 'text-sm text-success' : 'text-sm text-fg-muted'}>{statusMessage.text}</p>
    {/if}
  </section>

  <!-- Phase 1.19.D — account lockout state + admin unlock. Rendered
       whenever the user has a non-zero failed_login_count OR is
       actively locked so admins get situational awareness even for
       users who are just probing badly. Unlock button gates on the
       `auth.unlock` capability (admin-only by seed). -->
  {#if isLocked || failedLoginCount > 0}
    <section
      class="mt-6 max-w-xl space-y-3 rounded-lg border border-border bg-surface-elevated p-4"
      data-testid="admin-user-lockout-section"
    >
      <h3 class="text-sm font-medium text-fg">{t('admin.user_detail.lockout_section')}</h3>
      {#if isLocked}
        <p class="text-sm" data-testid="admin-user-lockout-badge">
          <span class="inline-block rounded bg-danger/15 px-2 py-0.5 text-xs font-medium text-danger">
            {t('admin.user_detail.lockout_badge_locked')}
          </span>
          <span class="ml-2 text-fg-muted">
            {t('admin.user_detail.lockout_auto_clears', {
              when: lockoutUntil?.toLocaleString() ?? '?',
              count: failedLoginCount,
            })}
          </span>
        </p>
      {:else}
        <p class="text-sm text-fg-muted" data-testid="admin-user-lockout-counter">
          {failedLoginCount === 1
            ? t('admin.user_detail.lockout_counter_one', { count: failedLoginCount })
            : t('admin.user_detail.lockout_counter_many', { count: failedLoginCount })}
        </p>
      {/if}
      {#if canUnlock}
        <button
          type="button"
          onclick={unlockAccount}
          disabled={unlockBusy || (!isLocked && failedLoginCount === 0)}
          data-testid="admin-user-unlock"
          class="rounded border border-border bg-surface px-3 py-1 text-xs font-medium hover:border-accent disabled:opacity-50"
        >
          {unlockBusy ? t('admin.user_detail.lockout_unlocking') : t('admin.user_detail.lockout_unlock_button')}
        </button>
      {/if}
      {#if unlockMessage}
        <p class="text-xs text-fg-muted" data-testid="admin-user-unlock-message">{unlockMessage}</p>
      {/if}
    </section>
  {/if}

  <section class="mt-6 max-w-xl space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="text-sm font-medium text-fg">{t('admin.user_detail.reset_section')}</h3>
    <p class="text-xs text-fg-muted">{t('admin.user_detail.reset_intro')}</p>

    <label class="block text-xs">
      <span class="mb-1 block text-fg-muted">{t('admin.user_detail.reset_reason')}</span>
      <input
        type="text"
        bind:value={resetReason}
        maxlength="500"
        class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>

    <button
      type="button"
      onclick={resetPassword}
      disabled={resetting}
      class="rounded border border-warning bg-warning/10 px-3 py-1 text-xs font-medium text-warning hover:bg-warning/20 disabled:opacity-50"
    >
      {resetting ? t('admin.user_detail.resetting') : t('admin.user_detail.reset_button')}
    </button>

    {#if resetResult}
      <div class="mt-2 rounded border border-warning/40 bg-warning/5 p-3">
        <p class="text-xs text-fg-muted">{t('admin.user_detail.reset_result_label')}</p>
        <div class="mt-1 flex items-center gap-2">
          <code class="flex-1 break-all rounded bg-surface px-2 py-1 font-mono text-sm">{resetResult.password}</code>
          <button
            type="button"
            onclick={copyReset}
            class="rounded border border-border bg-surface px-2 py-1 text-xs hover:border-accent"
          >
            {resetResult.copied ? t('admin.user_detail.reset_copied') : t('admin.user_detail.reset_copy')}
          </button>
        </div>
      </div>
    {/if}

    {#if auth.can('auth.impersonate') && user && user.ref !== auth.user?.ref}
      <div class="mt-4 border-t border-border pt-3">
        <h4 class="text-sm font-medium text-fg">{t('admin.user_detail.impersonate_title')}</h4>
        <p class="mt-1 text-xs text-fg-muted">{t('admin.user_detail.impersonate_help')}</p>
        <label class="mt-2 block text-xs text-fg-muted">
          {t('admin.user_detail.impersonate_reason_label')}
          <input
            type="text"
            bind:value={impReason}
            maxlength="500"
            placeholder={t('admin.user_detail.impersonate_reason_placeholder')}
            class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          />
        </label>
        <button
          type="button"
          onclick={startImpersonation}
          disabled={impStarting}
          data-testid="admin-user-impersonate"
          class="mt-2 rounded border border-warning bg-warning/10 px-3 py-1 text-xs font-medium text-warning hover:bg-warning/20 disabled:opacity-50"
        >
          {impStarting ? t('common.loading') : t('admin.user_detail.impersonate_button')}
        </button>
        {#if impError}
          <p role="alert" class="mt-2 rounded border border-danger/40 bg-danger-container px-2 py-1 text-xs text-danger">{impError}</p>
        {/if}
      </div>
    {/if}
  </section>

  <section class="mt-6 max-w-2xl space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="text-sm font-medium text-fg">{t('admin.user_detail.overrides_section')}</h3>
    <p class="text-xs text-fg-muted">{t('admin.user_detail.overrides_intro')}</p>

    {#if overridesError}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{overridesError}</p>
    {/if}

    {#if overridesLoading}
      <p class="text-xs text-fg-muted">{t('common.loading')}</p>
    {:else}
      <div class="grid gap-4 md:grid-cols-2">
        <div>
          <h4 class="mb-2 text-xs font-semibold uppercase tracking-wider text-success">{t('admin.user_detail.overrides_grants_label')}</h4>
          {#if grants.length === 0}
            <p class="text-xs text-fg-muted">{t('admin.user_detail.overrides_no_grants')}</p>
          {:else}
            <ul class="space-y-1">
              {#each grants as g, i (g.capability + (g.team_id ?? '') + i)}
                <li class="flex items-start gap-2 rounded border border-success/30 bg-success/5 px-2 py-1.5 text-xs">
                  <div class="flex-1">
                    <code class="font-mono">{g.capability}</code>
                    <span class="ml-1 text-fg-muted">· {shortTeam(g.team_id)}</span>
                    {#if g.note}<p class="mt-0.5 text-[11px] text-fg-muted italic">{g.note}</p>{/if}
                  </div>
                  <button
                    type="button"
                    onclick={() => removeGrant(g.capability, g.team_id)}
                    disabled={grantBusy}
                    class="rounded border border-border bg-surface px-2 py-0.5 text-[11px] hover:border-danger hover:text-danger disabled:opacity-50"
                  >
                    {t('admin.user_detail.overrides_remove_button')}
                  </button>
                </li>
              {/each}
            </ul>
          {/if}

          <div class="mt-3 space-y-1.5 rounded border border-border bg-surface p-2">
            <label class="block text-[11px]">
              <span class="mb-0.5 block text-fg-muted">{t('admin.user_detail.overrides_capability_label')}</span>
              <select
                bind:value={newGrantCap}
                class="w-full rounded border border-border-strong bg-surface-elevated px-1.5 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
              >
                <option value="">—</option>
                {#each allCaps as c (c.code)}
                  <option value={c.code}>{c.code}</option>
                {/each}
              </select>
            </label>
            <label class="block text-[11px]">
              <span class="mb-0.5 block text-fg-muted">{t('admin.user_detail.overrides_team_label')}</span>
              <input
                type="text"
                bind:value={newGrantTeam}
                placeholder={t('admin.user_detail.overrides_team_placeholder')}
                class="w-full rounded border border-border-strong bg-surface-elevated px-1.5 py-1 font-mono text-[11px] focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
              />
            </label>
            <label class="block text-[11px]">
              <span class="mb-0.5 block text-fg-muted">{t('admin.user_detail.overrides_note_label')}</span>
              <input
                type="text"
                bind:value={newGrantNote}
                maxlength="500"
                class="w-full rounded border border-border-strong bg-surface-elevated px-1.5 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
              />
            </label>
            <button
              type="button"
              onclick={addGrant}
              disabled={grantBusy || !newGrantCap}
              class="w-full rounded border border-success bg-success/10 px-2 py-1 text-xs font-medium text-success hover:bg-success/20 disabled:opacity-50"
            >
              {t('admin.user_detail.overrides_add_grant_button')}
            </button>
          </div>
        </div>

        <div>
          <h4 class="mb-2 text-xs font-semibold uppercase tracking-wider text-danger">{t('admin.user_detail.overrides_revokes_label')}</h4>
          {#if revokes.length === 0}
            <p class="text-xs text-fg-muted">{t('admin.user_detail.overrides_no_revokes')}</p>
          {:else}
            <ul class="space-y-1">
              {#each revokes as r, i (r.capability + (r.team_id ?? '') + i)}
                <li class="flex items-start gap-2 rounded border border-danger/30 bg-danger/5 px-2 py-1.5 text-xs">
                  <div class="flex-1">
                    <code class="font-mono">{r.capability}</code>
                    <span class="ml-1 text-fg-muted">· {shortTeam(r.team_id)}</span>
                    {#if r.note}<p class="mt-0.5 text-[11px] text-fg-muted italic">{r.note}</p>{/if}
                  </div>
                  <button
                    type="button"
                    onclick={() => removeRevoke(r.capability, r.team_id)}
                    disabled={revokeBusy}
                    class="rounded border border-border bg-surface px-2 py-0.5 text-[11px] hover:border-danger hover:text-danger disabled:opacity-50"
                  >
                    {t('admin.user_detail.overrides_remove_button')}
                  </button>
                </li>
              {/each}
            </ul>
          {/if}

          <div class="mt-3 space-y-1.5 rounded border border-border bg-surface p-2">
            <label class="block text-[11px]">
              <span class="mb-0.5 block text-fg-muted">{t('admin.user_detail.overrides_capability_label')}</span>
              <select
                bind:value={newRevokeCap}
                class="w-full rounded border border-border-strong bg-surface-elevated px-1.5 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
              >
                <option value="">—</option>
                {#each allCaps as c (c.code)}
                  <option value={c.code}>{c.code}</option>
                {/each}
              </select>
            </label>
            <label class="block text-[11px]">
              <span class="mb-0.5 block text-fg-muted">{t('admin.user_detail.overrides_team_label')}</span>
              <input
                type="text"
                bind:value={newRevokeTeam}
                placeholder={t('admin.user_detail.overrides_team_placeholder')}
                class="w-full rounded border border-border-strong bg-surface-elevated px-1.5 py-1 font-mono text-[11px] focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
              />
            </label>
            <label class="block text-[11px]">
              <span class="mb-0.5 block text-fg-muted">{t('admin.user_detail.overrides_note_label')}</span>
              <input
                type="text"
                bind:value={newRevokeNote}
                maxlength="500"
                class="w-full rounded border border-border-strong bg-surface-elevated px-1.5 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
              />
            </label>
            <button
              type="button"
              onclick={addRevoke}
              disabled={revokeBusy || !newRevokeCap}
              class="w-full rounded border border-danger bg-danger/10 px-2 py-1 text-xs font-medium text-danger hover:bg-danger/20 disabled:opacity-50"
            >
              {t('admin.user_detail.overrides_add_revoke_button')}
            </button>
          </div>
        </div>
      </div>
    {/if}
  </section>

  <section class="mt-8">
    <h3 class="mb-1 text-lg font-semibold">{t('admin.user_detail.sessions_section')}</h3>
    <p class="mb-3 text-sm text-fg-muted">{t('admin.user_detail.sessions_intro')}</p>
    {#if sessionsLoading}
      <p class="text-sm text-fg-muted">{t('common.loading')}</p>
    {:else if sessionsError}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{sessionsError}</p>
    {:else if sessions.length === 0}
      <p class="text-sm text-fg-muted">{t('admin.user_detail.sessions_none')}</p>
    {:else}
      <ul class="space-y-3">
        {#each sessions as s (s.id)}
          <li class="flex flex-wrap items-center gap-3 rounded-lg border border-border bg-surface-elevated px-4 py-3">
            <div class="min-w-0 flex-1">
              <span class="text-sm font-medium" title={s.user_agent ?? ''}>{deviceLabel(s.user_agent)}</span>
              <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-fg-muted">
                {#if s.ip}<span>{t('admin.user_detail.sessions_ip')}: {s.ip}</span>{/if}
                <span>{t('admin.user_detail.sessions_last_used')}: {relativeAgo(s.last_used_at)}</span>
                <span>{t('admin.user_detail.sessions_created')}: {relativeAgo(s.created_at)}</span>
                <span title={s.expires_at ?? ''}>
                  {t('admin.user_detail.sessions_expires')}: {s.expires_at ? relativeAgo(s.expires_at) : t('admin.user_detail.sessions_never')}
                </span>
              </div>
            </div>
            <button
              type="button"
              onclick={() => revokeSession(s.id)}
              disabled={revokingSession === s.id}
              class="rounded border border-danger bg-danger/10 px-3 py-1 text-xs font-medium text-danger hover:bg-danger/20 disabled:opacity-50"
            >
              {revokingSession === s.id ? t('admin.user_detail.sessions_revoking') : t('admin.user_detail.sessions_revoke')}
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
{/if}
