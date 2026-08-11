<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // A queue of pending resource requests with an inline decision panel
  // per row (grant / deny + reason + optional expires_at on grant).
  //
  // Extracted from /admin/requests when #881 gave OWNERS a queue of
  // their own at /account/requests. Two surfaces, one decision UI: the
  // panel enforces a 409-on-race contract and a "granting does not
  // unlock" caveat, and a second copy would be a second place for both
  // to drift. The pages differ only in which endpoint fills `items` —
  // the decide POST is the same for both, because the gate behind it
  // accepts an approver OR the asset's owner.
  //
  // ## Two kinds of request (#931)
  //
  // Since restoration appeals ride the same table and the same decide
  // endpoint, a row can now be one of two things, and the difference is
  // visible where it matters:
  //
  //   the TARGET may be a post or a collection, not just an asset, so
  //     the row links by kind instead of hardcoding /assets/;
  //   the CONSEQUENCE differs — granting an appeal puts the item back
  //     immediately, which is the opposite of the "granting does not
  //     unlock" caveat the access branch has to state;
  //   the EXPIRY field is meaningless on an appeal (a performed restore
  //     cannot expire — the server 400s it), so it is not offered.
  //
  // `decidable_by_caller` comes from the server, computed by the gate
  // the decide POST will apply. This queue is reachable by people who
  // may not act on every row in it — `share.grant` sees appeals and
  // cannot decide them — so row presence stopped being a usable proxy
  // for "you can act on this", and drawing the panel from it would
  // offer a button that 403s.

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  export interface QueuedRequest {
    id: string;
    requester_user_ref: number;
    target_kind: 'asset' | 'post' | 'collection';
    target_id: string;
    requested_capability: string;
    reason?: string;
    state: string;
    requested_at: string;
    decidable_by_caller?: boolean;
  }

  /** The marker capability a restoration appeal names (#931). */
  const RESTORE_REQUEST = 'content.restore.request';

  const TARGET_HREF: Record<QueuedRequest['target_kind'], string> = {
    asset: '/assets/',
    post: '/posts/',
    collection: '/collections/',
  };

  interface Props {
    items: QueuedRequest[];
    /** Called after a decision lands so the page can refetch, with the
     *  confirmation text to show.
     *
     *  The message goes UP rather than staying here because deciding
     *  the last pending request empties `items` — and on the owner's
     *  page that unmounts the whole section, taking any confirmation
     *  rendered inside it with it. Driven in a browser, granting the
     *  only request made the panel silently disappear with nothing to
     *  say it had worked. */
    ondecided: (message: string) => void | Promise<void>;
    /** Prefix for the per-row test ids, so the two surfaces stay
     *  addressable apart in a driven test. */
    testidPrefix?: string;
  }

  let { items, ondecided, testidPrefix = 'request' }: Props = $props();

  let openId = $state<string | null>(null);
  let decisionReason = $state('');
  let decisionExpires = $state('');
  let deciding = $state(false);
  // Errors stay in the panel — the row is still on screen to retry from.
  // Success goes up (see ondecided).
  let decideMsg = $state<{ kind: 'err'; text: string } | null>(null);

  function isRestore(id: string): boolean {
    return items.find((r) => r.id === id)?.requested_capability === RESTORE_REQUEST;
  }

  function openDecide(id: string) {
    openId = id;
    decisionReason = '';
    decisionExpires = '';
    decideMsg = null;
  }

  async function decide(id: string, decision: 'granted' | 'denied') {
    if (deciding) return;
    deciding = true;
    decideMsg = null;
    try {
      const body: Record<string, unknown> = { decision };
      if (decisionReason) body.reason = decisionReason;
      // Never on an appeal, and the field is not rendered for one
      // either — a performed restore has nothing to expire, and the
      // server answers 400 rather than accepting a date it would
      // ignore.
      if (decision === 'granted' && decisionExpires && !isRestore(id)) {
        body.expires_at = new Date(decisionExpires).toISOString();
      }
      const r = await api.POST('/admin/requests/{id}/decide', {
        params: { path: { id } },
        body: body as never,
      });
      if (r.error || !r.data) {
        decideMsg = { kind: 'err', text: (r.error as { error?: string }).error ?? 'Failed.' };
        return;
      }
      openId = null;
      await ondecided(t('admin.requests.decided', { decision }));
    } finally {
      deciding = false;
    }
  }
</script>

<ul class="space-y-2" data-testid="{testidPrefix}-queue">
  {#each items as r (r.id)}
    {@const restore = r.requested_capability === RESTORE_REQUEST}
    <li
      class="rounded-lg border border-border bg-surface-elevated p-3"
      data-testid="{testidPrefix}-row-{r.id}"
    >
      <div class="flex items-center justify-between gap-3">
        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-2">
            {#if restore}
              <span
                class="rounded border border-accent/50 px-1.5 py-0.5 text-[11px] uppercase tracking-wide text-accent"
                data-testid="{testidPrefix}-restore-badge"
              >
                {t('admin.requests.restore_appeal')}
              </span>
            {:else}
              <code class="text-xs text-fg">{r.requested_capability}</code>
            {/if}
            <span class="text-xs text-fg-muted">user_ref={r.requester_user_ref}</span>
          </div>
          {#if r.reason}
            <p class="mt-1 text-sm text-fg">{r.reason}</p>
          {/if}
          <!-- One literal key per branch, rather than one key built by
               interpolating target_kind into the suffix.
               The coverage sweep DOES resolve a built key against the
               key FAMILY, so the shorter form would pass — but it
               passes as soon as ONE sibling key exists, so a typo in a
               branch nobody renders in dev ships silently. Three lines
               buy three real assertions.
               (The sweep reads raw source, comments included, so this
               note deliberately does not spell the call it describes.) -->
          <p class="mt-1 text-xs text-fg-muted">
            {#if r.target_kind === 'post'}
              {t('admin.requests.target_post')}
            {:else if r.target_kind === 'collection'}
              {t('admin.requests.target_collection')}
            {:else}
              {t('admin.requests.target_asset')}
            {/if}:
            <a href={`${TARGET_HREF[r.target_kind]}${r.target_id}`} class="text-accent hover:underline"
              >{r.target_id.slice(0, 8)}…</a
            >
          </p>
        </div>
        {#if r.decidable_by_caller === false}
          <!-- Visible, not actionable. The admin queue lists every
               pending row, and an appeal is decidable only by whoever
               deleted the item — so a share.grant approver reading this
               page needs to be told why there is no button, rather than
               shown one that 403s. -->
          <p class="max-w-[14rem] text-right text-xs text-fg-muted" data-testid="{testidPrefix}-not-mine-{r.id}">
            {t('admin.requests.not_yours_to_decide')}
          </p>
        {:else if openId === r.id}
          <button
            type="button"
            class="text-xs text-fg-muted hover:underline"
            onclick={() => (openId = null)}>{t('common.cancel')}</button
          >
        {:else}
          <button
            type="button"
            class="rounded border border-border bg-surface px-3 py-1 text-xs hover:border-accent"
            onclick={() => openDecide(r.id)}
            data-testid="{testidPrefix}-decide-{r.id}">{t('admin.requests.decide')}</button
          >
        {/if}
      </div>

      {#if openId === r.id}
        <div class="mt-3 space-y-2 border-t border-border pt-3">
          <!-- Said before the decision, not after. A grant records that
               the decider agreed; it does not currently reveal the asset
               to the requester, because there is no per-object
               capability scope to hang that on (#912, ADR 0064). An
               approver who believes otherwise would tell the requester
               to go and look. -->
          <p class="rounded-md border border-border bg-surface px-3 py-2 text-xs text-fg-muted">
            {#if restore}
              {t('admin.requests.restore_grant_restores')}
            {:else}
              {t('requests.grant_does_not_unlock')}
            {/if}
          </p>
          <label class="block text-xs">
            <span class="mb-1 block text-fg-muted">{t('admin.requests.reason')}</span>
            <input
              type="text"
              bind:value={decisionReason}
              maxlength="1000"
              class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm"
            />
          </label>
          <!-- No expiry on an appeal. There is nothing scheduled to
               re-delete a restored item, so the field would be a
               deadline that never fires; the server refuses it. -->
          {#if !restore}
            <label class="block text-xs">
              <span class="mb-1 block text-fg-muted">{t('admin.requests.expires_at')}</span>
              <input
                type="datetime-local"
                bind:value={decisionExpires}
                class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm"
              />
            </label>
          {/if}
          <div class="flex flex-wrap gap-2">
            <button
              type="button"
              disabled={deciding}
              onclick={() => decide(r.id, 'granted')}
              class="rounded border border-success bg-success/10 px-3 py-1 text-xs font-medium text-success hover:bg-success/20 disabled:opacity-50"
              data-testid="{testidPrefix}-grant-{r.id}"
              >{restore ? t('admin.requests.grant_restore') : t('admin.requests.grant')}</button
            >
            <button
              type="button"
              disabled={deciding}
              onclick={() => decide(r.id, 'denied')}
              class="rounded border border-danger bg-danger/10 px-3 py-1 text-xs font-medium text-danger hover:bg-danger/20 disabled:opacity-50"
              data-testid="{testidPrefix}-deny-{r.id}">{t('admin.requests.deny')}</button
            >
          </div>
          {#if decideMsg}
            <p role="alert" class="text-sm text-danger">{decideMsg.text}</p>
          {/if}
        </div>
      {/if}
    </li>
  {/each}
</ul>
