<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/trash — your own soft-deleted items, with restore (#937).
  //
  // Backed by GET /account/trash, which is owner-scoped: it lists what
  // you OWN and is currently soft-deleted, whoever deleted it. Nothing
  // on this page can show you an item you did not create, so there is
  // no visibility question for the client to re-litigate.
  //
  // Two row states, and the difference is the whole point:
  //
  //   restorable_by_caller = true   → a Restore button. You deleted it
  //                                   (or you are an administrator), so
  //                                   the endpoint will say yes.
  //   restorable_by_caller = false  → plain prose saying someone else
  //                                   removed it and restoring needs an
  //                                   administrator.
  //
  // That copy says "someone else" rather than "an administrator"
  // because the flag covers three causes and only one of them is
  // moderation: an admin deleted it, a team-mate with mutation rights
  // deleted it, or no deleter was recorded at all (pre-00037 rows and
  // system retention deletes, both of which fail closed to
  // system.admin). The server returns one boolean and deliberately
  // does not disclose WHO — so naming the administrator here would be
  // a guess the response cannot support.
  //
  // The flag is computed server-side by the SAME predicate the restore
  // endpoints gate on, so the button and the endpoint cannot disagree.
  // We never render a button that would 403 — that is the failure this
  // page exists to avoid, and it is why the second state is a sentence
  // rather than a disabled control with a tooltip.
  //
  // ## The appeal (#931)
  //
  // `restorable_by_caller = false` used to be the end of the row: one
  // sentence saying someone else removed it, and nowhere to go. That
  // was the dead end #931 names. It now carries two more things:
  //
  //   the REASON the deleter typed, if any. #985's delete dialog
  //     promises whoever writes one that "the owner will be shown what
  //     you write here" — and until now the field was stored and never
  //     projected, so the promise was made to every moderator and kept
  //     for nobody.
  //
  //   an APPEAL, addressed to the person who deleted it (or to an
  //     administrator when no deleter was recorded). Filing one is a
  //     resource_request naming an inert marker capability; granting it
  //     performs the restore. The button is offered here and nowhere
  //     else because this is the only surface that can name a deleted
  //     item the caller owns.
  //
  // The third row state, `restore_requested`, is server-computed like
  // the first — the same discipline, for the same reason. Submit
  // coalesces, so a client that tracked "I clicked it" locally would
  // show a live button again after a reload and file nothing when
  // pressed.
  //
  // Layout is a stacked card list rather than a table. Trash is a
  // read-and-act surface with four facts per row, and a four-column
  // table at 390px is a horizontal scrollbar over content the phone
  // build should be able to act on directly.
  //
  // ## The second tab (#981)
  //
  // "Deleted by me" is the same list under `scope=deleted_by_me`:
  // things the caller removed that belong to SOMEONE ELSE. It exists
  // because the restore rule and the listing rule disagreed. Restore
  // is granted to the DELETER (auth.CanRestoreDeleted), so a team lead
  // who removes a colleague's asset may put it back — but the owner-
  // scoped listing showed it in the OWNER's trash, where it renders as
  // non-restorable, and in nobody's as restorable. The right existed
  // and had no surface.
  //
  // Every row in that tab comes back `restorable_by_caller: true`, by
  // the same server-side predicate, so the tab needs no special-casing
  // here: it is the same row renderer with a different query. The two
  // scopes are disjoint server-side (`owner IS DISTINCT FROM caller`),
  // so nothing double-lists.
  //
  // The tabs are LOCAL state, not a URL param. Trash is a transient
  // housekeeping surface — nobody links to a tab of it — and a query
  // string would make the browser Back button walk tab switches
  // instead of leaving the page.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { auth } from '$stores/auth.svelte';

  type TrashKind = 'asset' | 'post' | 'collection';

  interface TrashItem {
    kind: TrashKind;
    id: string;
    title: string;
    deleted_at: string;
    restorable_by_caller: boolean;
    restore_requested: boolean;
    deleted_reason?: string | null;
    purge_after?: string | null;
  }

  const PAGE = 50;

  type Scope = 'owned_by_me' | 'deleted_by_me';

  let scope = $state<Scope>('owned_by_me');
  let items = $state<TrashItem[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(true);
  let loadingMore = $state(false);
  let loaded = $state(false);
  let error = $state<string | null>(null);
  let restoring = $state<string | null>(null);
  let restored = $state<string | null>(null);

  onMount(() => {
    void load(null);
  });

  /** Switch tabs. Wipes the accumulated pages rather than keeping them
   *  behind the other tab: a cursor is scoped to the query that
   *  produced it, so a stale `nextCursor` would page the wrong list. */
  function selectScope(next: Scope): void {
    if (next === scope) return;
    scope = next;
    items = [];
    nextCursor = null;
    loaded = false;
    restored = null;
    void load(null);
  }

  async function load(cursor: string | null): Promise<void> {
    if (cursor) loadingMore = true;
    else loading = true;
    error = null;
    // Captured so a slow response for the tab the user just left
    // cannot commit its rows into the tab they are now looking at.
    const requested = scope;
    try {
      const query: Record<string, string | number> = { limit: PAGE, scope: requested };
      if (cursor) query.cursor = cursor;
      const r = await api.GET('/account/trash', { params: { query: query as never } });
      if (requested !== scope) return;
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('account.trash.load_error');
        return;
      }
      const pageItems = (r.data.items ?? []) as TrashItem[];
      items = cursor ? [...items, ...pageItems] : pageItems;
      nextCursor = (r.data.next_cursor as string | null) ?? null;
    } catch (e) {
      if (requested === scope) {
        error = e instanceof Error ? e.message : t('account.trash.load_error');
      }
    } finally {
      // Guarded for the same reason as the commit above: a stale
      // response must not flip the CURRENT tab out of its loading
      // state, which would flash the empty placeholder over a fetch
      // that is still in flight.
      if (requested === scope) {
        loading = false;
        loadingMore = false;
        loaded = true;
      }
    }
  }

  // The restore endpoints are per-kind and still sit under /admin —
  // that path is historical (#936 opened them to the deleter); it is
  // not a claim that this page needs admin rights.
  const RESTORE_PATH = {
    asset: '/admin/assets/{id}/restore',
    post: '/admin/posts/{id}/restore',
    collection: '/admin/collections/{id}/restore',
  } as const;

  async function restore(item: TrashItem): Promise<void> {
    if (restoring) return;
    restoring = item.id;
    error = null;
    restored = null;
    try {
      const r = await api.POST(RESTORE_PATH[item.kind] as '/admin/assets/{id}/restore', {
        params: { path: { id: item.id } },
      });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('account.trash.restore_error');
        return;
      }
      // 204 — it is live again, so it is no longer trash. Drop the row
      // rather than re-fetching: a re-fetch would reset the pages the
      // user already loaded.
      items = items.filter((i) => i.id !== item.id);
      restored = item.title || t(`account.trash.kind_${item.kind}`);
    } catch (e) {
      error = e instanceof Error ? e.message : t('account.trash.restore_error');
    } finally {
      restoring = null;
    }
  }

  // ── Appealing a delete you cannot undo (#931) ────────────────────
  //
  // The other half of the row. `restorable_by_caller: false` used to be
  // a full stop — one sentence saying someone else removed it, and
  // nothing to do about it. Now it opens a short form addressed to
  // whoever that was.
  //
  // Three states per row, and they come from the SERVER, not from what
  // this page remembers doing:
  //
  //   restore_requested = false → the Appeal button
  //   restore_requested = true  → "Restoration requested", no button
  //   (after a successful send)  → the same, because we set the flag on
  //                                the row rather than tracking a
  //                                separate "I just sent one" set.
  //
  // That last point is why the optimistic update writes
  // `restore_requested` instead of a local Set: a reload has to land on
  // the same state a fresh page would, and Submit COALESCES server-side
  // (a second appeal returns the first, unchanged), so a button that
  // stayed live would look broken rather than idempotent.
  let appealFor = $state<string | null>(null);
  let appealReason = $state('');
  let appealing = $state(false);

  function rowKey(item: TrashItem): string {
    return item.kind + ':' + item.id;
  }

  function openAppeal(item: TrashItem): void {
    appealFor = rowKey(item);
    appealReason = '';
    error = null;
  }

  async function sendAppeal(item: TrashItem): Promise<void> {
    if (appealing) return;
    appealing = true;
    error = null;
    try {
      const r = await api.POST('/account/trash/{kind}/{id}/request-restore', {
        params: { path: { kind: item.kind, id: item.id } },
        body: appealReason ? { reason: appealReason } : {},
      });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('account.trash.appeal_error');
        return;
      }
      // 200 and 201 both mean "there is a pending appeal on this row" —
      // 200 is the coalesce. The row renders the same either way, which
      // is the honest answer to a double-click.
      items = items.map((i) =>
        rowKey(i) === rowKey(item) ? { ...i, restore_requested: true } : i,
      );
      appealFor = null;
    } catch (e) {
      error = e instanceof Error ? e.message : t('account.trash.appeal_error');
    } finally {
      appealing = false;
    }
  }

  function formatDate(iso: string): string {
    try {
      return new Date(iso).toLocaleDateString(undefined, {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
      });
    } catch {
      return iso;
    }
  }

  // Whole days from now until the retention GC may hard-delete this.
  // Null when the server did not state a window — we say nothing then
  // rather than inventing a number.
  function daysLeft(iso: string | null | undefined): number | null {
    if (!iso) return null;
    const at = new Date(iso).getTime();
    if (Number.isNaN(at)) return null;
    return Math.max(0, Math.ceil((at - Date.now()) / 86_400_000));
  }

  const showEmpty = $derived(loaded && items.length === 0 && !error);
  const profileHref = $derived(auth.user ? `/users/by-ref/${auth.user.ref}` : '/');
</script>

<svelte:head><title>{t('account.trash.title')} — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">{t('account.trash.title')}</h2>
  <p class="mt-1 text-sm text-fg-muted">
    {scope === 'deleted_by_me'
      ? t('account.trash.tab_deleted_by_me_hint')
      : t('account.trash.intro')}
  </p>
</header>

<!-- Two tabs, always both rendered. The second is not conditional on
     having anything in it: a tab that appears only once you have
     deleted someone else's work is a tab nobody discovers, and its
     empty state is the sentence that explains what it is for. -->
<div class="mb-6 flex gap-1 border-b border-border" role="tablist" data-testid="trash-tabs">
  {#each [{ id: 'owned_by_me', label: t('account.trash.tab_mine') }, { id: 'deleted_by_me', label: t('account.trash.tab_deleted_by_me') }] as tab (tab.id)}
    <button
      type="button"
      role="tab"
      aria-selected={scope === tab.id}
      data-testid="trash-tab-{tab.id}"
      onclick={() => selectScope(tab.id as Scope)}
      class="-mb-px border-b-2 px-3 py-2 text-sm
             {scope === tab.id
        ? 'border-accent font-medium text-fg'
        : 'border-transparent text-fg-muted hover:text-fg'}"
    >
      {tab.label}
    </button>
  {/each}
</div>

{#if error}
  <p role="alert" class="mb-4 rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
    {error}
  </p>
{/if}
{#if restored}
  <p
    role="status"
    class="mb-4 rounded border border-border bg-surface-elevated px-3 py-2 text-sm text-fg"
    data-testid="trash-restored"
  >
    {t('account.trash.restored')}
  </p>
{/if}

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if showEmpty}
  <div class="rounded-xl border border-dashed border-border p-12 text-center" data-testid="trash-empty">
    {#if scope === 'deleted_by_me'}
      <p class="font-medium text-fg">{t('account.trash.empty_deleted_by_me')}</p>
      <p class="mt-1 text-sm text-fg-muted">{t('account.trash.empty_hint_deleted_by_me')}</p>
    {:else}
      <p class="font-medium text-fg">{t('account.trash.empty')}</p>
      <p class="mt-1 text-sm text-fg-muted">{t('account.trash.empty_hint')}</p>
      <a
        href={profileHref}
        class="mt-4 inline-block rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover"
      >
        {t('account.trash.empty_action')}
      </a>
    {/if}
  </div>
{:else}
  <ul class="space-y-3" data-testid="trash-list">
    {#each items as item (item.kind + item.id)}
      {@const left = daysLeft(item.purge_after)}
      <li
        class="rounded-lg border border-border bg-surface p-4"
        data-testid="trash-row"
        data-kind={item.kind}
        data-id={item.id}
      >
        <div class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
          <div class="min-w-0">
            <div class="flex flex-wrap items-center gap-2">
              <span
                class="rounded border border-border px-1.5 py-0.5 text-[11px] uppercase tracking-wide text-fg-muted"
              >
                {t(`account.trash.kind_${item.kind}`)}
              </span>
              <span class="truncate font-medium text-fg">
                {item.title || t('account.trash.untitled')}
              </span>
            </div>
            <p class="mt-1 text-xs text-fg-muted">
              {t('account.trash.deleted_on')}
              {formatDate(item.deleted_at)}
              {#if left !== null}
                &middot; {t('account.trash.purges_in')}
                {left}
                {left === 1 ? t('account.trash.day') : t('account.trash.days')}
              {/if}
            </p>
            <!-- The reason, kept next to the item rather than in the
                 action column: it explains the ROW, and #985's dialog
                 promised the owner would be shown it. Quoted rather
                 than paraphrased — it is somebody's sentence, not our
                 summary of one. Who wrote it is still not disclosed;
                 the server does not return that. -->
            {#if item.deleted_reason}
              <p class="mt-2 border-l-2 border-border-strong pl-3 text-sm text-fg" data-testid="trash-deleted-reason">
                <span class="mr-1 text-xs uppercase tracking-wide text-fg-muted"
                  >{t('account.trash.deleted_reason_label')}</span
                >
                {item.deleted_reason}
              </p>
            {/if}
          </div>

          <div class="shrink-0 sm:max-w-[18rem] sm:text-right">
            {#if item.restorable_by_caller}
              <button
                type="button"
                class="w-full rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover disabled:opacity-50 sm:w-auto"
                data-testid="trash-restore"
                onclick={() => void restore(item)}
                disabled={restoring !== null}
              >
                {restoring === item.id ? t('account.trash.restoring') : t('account.trash.restore')}
              </button>
            {:else if item.restore_requested}
              <!-- Quiet, and deliberately not a button. The appeal is
                   with someone else now; the only honest thing this
                   column can offer is the fact that it was sent. -->
              <p class="text-xs text-fg" data-testid="trash-appeal-sent">
                {t('account.trash.appeal_sent')}
              </p>
              <p class="mt-1 text-xs text-fg-muted">{t('account.trash.appeal_sent_hint')}</p>
            {:else}
              <p class="text-xs text-fg-muted" data-testid="trash-not-restorable">
                {t('account.trash.admin_deleted')}
              </p>
              {#if appealFor !== rowKey(item)}
                <!-- #931. The sentence above is still true and stays;
                     what changes is that it is no longer the end of the
                     row. -->
                <button
                  type="button"
                  class="mt-2 w-full rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover disabled:opacity-50 sm:w-auto"
                  data-testid="trash-appeal"
                  onclick={() => openAppeal(item)}
                  disabled={appealing}
                >
                  {t('account.trash.appeal')}
                </button>
              {/if}
            {/if}
          </div>
        </div>

        <!-- The form sits BELOW both columns, full width, rather than
             inside the narrow action column: a reason someone will
             read deserves more than an 18rem box, and at 390px the
             action column is the whole width anyway. -->
        {#if appealFor === rowKey(item)}
          <div class="mt-3 space-y-2 border-t border-border pt-3" data-testid="trash-appeal-form">
            <label class="block text-xs">
              <span class="mb-1 block text-fg-muted">{t('account.trash.appeal_reason_label')}</span>
              <input
                type="text"
                bind:value={appealReason}
                maxlength="1000"
                placeholder={t('account.trash.appeal_reason_placeholder')}
                data-testid="trash-appeal-reason"
                class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm"
              />
            </label>
            <div class="flex flex-wrap gap-2">
              <button
                type="button"
                class="rounded-md border border-accent bg-accent/10 px-4 py-2 text-sm font-medium text-accent hover:bg-accent/20 disabled:opacity-50"
                data-testid="trash-appeal-send"
                disabled={appealing}
                onclick={() => void sendAppeal(item)}
              >
                {appealing ? t('account.trash.appealing') : t('account.trash.appeal_send')}
              </button>
              <button
                type="button"
                class="rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover"
                onclick={() => (appealFor = null)}
              >
                {t('account.trash.appeal_cancel')}
              </button>
            </div>
          </div>
        {/if}
      </li>
    {/each}
  </ul>

  {#if nextCursor}
    <div class="mt-4 flex justify-center">
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover disabled:opacity-50"
        onclick={() => void load(nextCursor)}
        disabled={loadingMore}
      >
        {loadingMore ? t('common.loading') : t('account.trash.load_more')}
      </button>
    </div>
  {/if}
{/if}
