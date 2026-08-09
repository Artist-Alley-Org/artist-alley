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
  // There is deliberately NO "request restoration" button yet. #931
  // owns that flow; a button here that went nowhere would be the same
  // dead end wearing a different label.
  //
  // Layout is a stacked card list rather than a table. Trash is a
  // read-and-act surface with four facts per row, and a four-column
  // table at 390px is a horizontal scrollbar over content the phone
  // build should be able to act on directly.

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
    purge_after?: string | null;
  }

  const PAGE = 50;

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

  async function load(cursor: string | null): Promise<void> {
    if (cursor) loadingMore = true;
    else loading = true;
    error = null;
    try {
      const query: Record<string, string | number> = { limit: PAGE };
      if (cursor) query.cursor = cursor;
      const r = await api.GET('/account/trash', { params: { query: query as never } });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('account.trash.load_error');
        return;
      }
      const pageItems = (r.data.items ?? []) as TrashItem[];
      items = cursor ? [...items, ...pageItems] : pageItems;
      nextCursor = (r.data.next_cursor as string | null) ?? null;
    } catch (e) {
      error = e instanceof Error ? e.message : t('account.trash.load_error');
    } finally {
      loading = false;
      loadingMore = false;
      loaded = true;
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

<header class="mb-6">
  <h2 class="text-2xl font-semibold">{t('account.trash.title')}</h2>
  <p class="mt-1 text-sm text-fg-muted">{t('account.trash.intro')}</p>
</header>

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
    <p class="font-medium text-fg">{t('account.trash.empty')}</p>
    <p class="mt-1 text-sm text-fg-muted">{t('account.trash.empty_hint')}</p>
    <a
      href={profileHref}
      class="mt-4 inline-block rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover"
    >
      {t('account.trash.empty_action')}
    </a>
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
            {:else}
              <p class="text-xs text-fg-muted" data-testid="trash-not-restorable">
                {t('account.trash.admin_deleted')}
              </p>
            {/if}
          </div>
        </div>
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
