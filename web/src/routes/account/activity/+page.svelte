<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/activity — the caller's own slice of the audit log (#600).
  //
  // Backed by GET /account/activity, which is caller-scoped on both
  // sides: rows where the caller acted, and rows where the caller's
  // account was acted upon. Nothing here can show an event that does
  // not name the signed-in user, so — as on /account/trash — there is
  // no visibility question for the client to re-litigate.
  //
  // ## One list, not two tabs
  //
  // "Things I did" and "things done to me" are the same question asked
  // from two sides, and the answer people actually want is
  // chronological: "what happened around the time I lost access". Two
  // tabs would make the user interleave them by hand. The distinction
  // is carried by the sentence instead — see $lib/account/activityEvents
  // for why the role picks the VOICE rather than printing a badge.
  //
  // ## Never a JSON dump
  //
  // The admin viewer at /admin/audit renders `event_type` in monospace
  // and the metadata as a raw <pre>, which is right for an auditor
  // reading a log and wrong for everyone else. Nothing raw reaches this
  // page: a known type becomes a sentence, an unknown one becomes a
  // sentence naming the type, and metadata is read for the two or three
  // keys that mean something to a person. There is no branch here that
  // prints a payload.
  //
  // The server only sends metadata for rows the caller ACTED on, so the
  // detail line below can only ever describe the caller's own action.
  // The page does not re-derive that rule — it just renders what
  // arrived — but it is the reason a detail line is safe at all.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { activityEventKey, type ActivityRole } from '$lib/account/activityEvents';

  interface ActivityEvent {
    id: string;
    event_type: string;
    occurred_at: string;
    role: ActivityRole;
    metadata?: Record<string, unknown> | null;
  }

  const PAGE = 50;

  let items = $state<ActivityEvent[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(true);
  let loadingMore = $state(false);
  let loaded = $state(false);
  let error = $state<string | null>(null);

  onMount(() => {
    void load(null);
  });

  async function load(cursor: string | null): Promise<void> {
    if (cursor) loadingMore = true;
    else loading = true;
    error = null;
    try {
      const query: Record<string, string | number> = { limit: PAGE };
      if (cursor) query.cursor = cursor;
      const r = await api.GET('/account/activity', { params: { query: query as never } });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('account.activity.load_error');
        return;
      }
      const pageItems = (r.data.items ?? []) as ActivityEvent[];
      items = cursor ? [...items, ...pageItems] : pageItems;
      nextCursor = (r.data.next_cursor as string | null) ?? null;
    } catch (e) {
      error = e instanceof Error ? e.message : t('account.activity.load_error');
    } finally {
      loading = false;
      loadingMore = false;
      loaded = true;
    }
  }

  /** The sentence for one row. Falls back to a sentence naming the raw
   *  type — `t()` returns the KEY when a lookup misses, so an unchecked
   *  dynamic lookup would print an i18n path into the page. */
  function sentence(it: ActivityEvent): string {
    const key = activityEventKey(it.event_type, it.role);
    if (!key) return t('account.activity.unknown_event', { type: it.event_type });
    return t(key);
  }

  /** One muted line of context under the sentence, or null.
   *
   *  Reads only keys whose meaning is stable across the event types
   *  that carry them, and renders at most one — this is an orientation
   *  hint ("which asset?"), not a field dump. Anything unrecognised is
   *  simply not shown; the sentence already said what happened. */
  function detail(it: ActivityEvent): string | null {
    const m = it.metadata;
    if (!m) return null;
    const str = (k: string): string | null => {
      const v = m[k];
      return typeof v === 'string' && v !== '' ? v : null;
    };

    const assetId = str('asset_id');
    if (assetId) return t('account.activity.detail_asset', { id: assetId });
    const capability = str('capability');
    if (capability) return t('account.activity.detail_capability', { capability });
    const reason = str('reason');
    if (reason) return t('account.activity.detail_reason', { reason });
    const revoked = m['sessions_revoked'];
    if (typeof revoked === 'number' && revoked > 0) {
      return t('account.activity.detail_sessions_revoked', { count: String(revoked) });
    }
    return null;
  }

  function whenLabel(iso: string): string {
    try {
      return new Date(iso).toLocaleString();
    } catch {
      return iso;
    }
  }

  /** Day heading for the row, so a long list reads as a timeline rather
   *  than an undifferentiated column of timestamps. */
  function dayLabel(iso: string): string {
    try {
      return new Date(iso).toLocaleDateString(undefined, {
        year: 'numeric',
        month: 'long',
        day: 'numeric',
      });
    } catch {
      return iso;
    }
  }

  const showEmpty = $derived(loaded && items.length === 0 && !error);
</script>

<svelte:head><title>{t('account.activity.title')} — {site.name}</title></svelte:head>

<header class="mb-6">
  <h2 class="text-2xl font-semibold">{t('account.activity.title')}</h2>
  <p class="mt-1 text-sm text-fg-muted">{t('account.activity.intro')}</p>
</header>

{#if error}
  <p role="alert" class="mb-4 rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
    {error}
  </p>
{/if}

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if showEmpty}
  <div class="rounded-xl border border-dashed border-border p-12 text-center" data-testid="activity-empty">
    <p class="font-medium text-fg">{t('account.activity.empty')}</p>
    <p class="mt-1 text-sm text-fg-muted">{t('account.activity.empty_hint')}</p>
  </div>
{:else}
  <ol class="space-y-1" data-testid="activity-list">
    {#each items as it, i (it.id)}
      {@const day = dayLabel(it.occurred_at)}
      {#if i === 0 || day !== dayLabel(items[i - 1].occurred_at)}
        <li class="pb-1 pt-4 text-xs font-medium uppercase tracking-wide text-fg-muted first:pt-0">
          {day}
        </li>
      {/if}
      <li
        class="rounded-lg border border-border bg-surface px-4 py-3"
        data-testid="activity-row"
        data-role={it.role}
        data-event-type={it.event_type}
      >
        <p class="text-sm text-fg">{sentence(it)}</p>
        {#if detail(it)}
          <p class="mt-1 break-all text-xs text-fg-muted" data-testid="activity-detail">
            {detail(it)}
          </p>
        {/if}
        <p class="mt-1 text-xs text-fg-muted">
          <time datetime={it.occurred_at}>{whenLabel(it.occurred_at)}</time>
        </p>
      </li>
    {/each}
  </ol>

  {#if nextCursor}
    <div class="mt-6 flex justify-center">
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover disabled:opacity-50"
        onclick={() => load(nextCursor)}
        disabled={loadingMore}
        data-testid="activity-load-more"
      >
        {loadingMore ? t('common.loading') : t('account.activity.load_more')}
      </button>
    </div>
  {/if}
{/if}
