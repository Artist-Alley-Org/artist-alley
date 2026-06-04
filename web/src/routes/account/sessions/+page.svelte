<script lang="ts">
  // /account/sessions — multi-device session manager (Phase 1.17.C).
  //
  // Lists the caller's active sessions and lets them revoke any
  // except the one authenticating this request (the "current"
  // session — revoking it would 401 the next click and confuse
  // users). The current row is rendered without a revoke button +
  // a "This device" badge.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { relativeAgo } from '$lib/admin/users';

  interface SessionRow {
    id: string;
    created_at: string;
    last_used_at: string;
    expires_at?: string | null;
    ip?: string | null;
    user_agent?: string | null;
    current?: boolean;
  }

  let sessions = $state<SessionRow[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);
  let revoking = $state<string | null>(null); // session id currently being revoked
  let toast = $state<string | null>(null);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/account/sessions');
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to load sessions.';
        return;
      }
      sessions = (r.data as unknown as { items: SessionRow[] }).items;
    } finally {
      loading = false;
    }
  }

  async function revoke(id: string) {
    if (revoking) return;
    revoking = id;
    toast = null;
    try {
      const r = await api.DELETE('/account/sessions/{id}', { params: { path: { id } } });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to revoke.';
        return;
      }
      sessions = sessions.filter((s) => s.id !== id);
      toast = t('account.sessions.revoked');
    } finally {
      revoking = null;
    }
  }

  /** Reduce a UA string to a recognisable device label. The full UA
   *  is shown as the tooltip; the badge gets the short form. */
  function deviceLabel(ua: string | null | undefined): string {
    if (!ua) return t('account.sessions.ua_label');
    if (/iPhone|iPad|iPod/.test(ua)) return 'iOS';
    if (/Android/.test(ua)) return 'Android';
    if (/Macintosh/.test(ua)) return 'macOS';
    if (/Windows/.test(ua)) return 'Windows';
    if (/Linux/.test(ua)) return 'Linux';
    return ua.slice(0, 40);
  }
</script>

<svelte:head><title>{t('account.sessions.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('account.sessions.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('account.sessions.intro')}</p>

{#if loading}
  <p class="text-fg-muted">{t('account.sessions.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
{:else}
  {#if toast}
    <p class="mb-3 rounded border border-success/40 bg-success/10 px-3 py-2 text-sm text-success">{toast}</p>
  {/if}

  <ul class="space-y-3">
    {#each sessions as s (s.id)}
      <li class="flex flex-wrap items-center gap-3 rounded-lg border border-border bg-surface-elevated px-4 py-3">
        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2">
            <span class="text-sm font-medium" title={s.user_agent ?? ''}>{deviceLabel(s.user_agent)}</span>
            {#if s.current}
              <span class="rounded bg-accent/15 px-2 py-0.5 text-[10px] uppercase tracking-wider text-accent">
                {t('account.sessions.current_badge')}
              </span>
            {/if}
          </div>
          <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-fg-muted">
            {#if s.ip}
              <span>{t('account.sessions.ip_label')}: {s.ip}</span>
            {/if}
            <span>{t('account.sessions.last_used')}: {relativeAgo(s.last_used_at)}</span>
            <span>{t('account.sessions.created')}: {relativeAgo(s.created_at)}</span>
            <span title={s.expires_at ?? ''}>
              {t('account.sessions.expires')}: {s.expires_at ? relativeAgo(s.expires_at) : t('account.sessions.never_expires')}
            </span>
          </div>
        </div>
        {#if !s.current}
          <button
            type="button"
            onclick={() => revoke(s.id)}
            disabled={revoking === s.id}
            class="rounded border border-danger bg-danger/10 px-3 py-1 text-xs font-medium text-danger hover:bg-danger/20 disabled:opacity-50"
          >
            {revoking === s.id ? t('account.sessions.revoking') : t('account.sessions.revoke')}
          </button>
        {/if}
      </li>
    {/each}
  </ul>

  {#if sessions.filter((s) => !s.current).length === 0}
    <p class="mt-3 text-xs text-fg-muted">{t('account.sessions.none')}</p>
  {/if}
{/if}
