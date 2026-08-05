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

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  export interface QueuedRequest {
    id: string;
    requester_user_ref: number;
    target_asset_id: string;
    requested_capability: string;
    reason?: string;
    state: string;
    requested_at: string;
  }

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
      if (decision === 'granted' && decisionExpires) {
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
    <li
      class="rounded-lg border border-border bg-surface-elevated p-3"
      data-testid="{testidPrefix}-row-{r.id}"
    >
      <div class="flex items-center justify-between gap-3">
        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2">
            <code class="text-xs text-fg">{r.requested_capability}</code>
            <span class="text-xs text-fg-muted">user_ref={r.requester_user_ref}</span>
          </div>
          {#if r.reason}
            <p class="mt-1 text-sm text-fg">{r.reason}</p>
          {/if}
          <p class="mt-1 text-xs text-fg-muted">
            asset:
            <a href={`/assets/${r.target_asset_id}`} class="text-accent hover:underline"
              >{r.target_asset_id.slice(0, 8)}…</a
            >
          </p>
        </div>
        {#if openId === r.id}
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
            {t('requests.grant_does_not_unlock')}
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
          <label class="block text-xs">
            <span class="mb-1 block text-fg-muted">{t('admin.requests.expires_at')}</span>
            <input
              type="datetime-local"
              bind:value={decisionExpires}
              class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm"
            />
          </label>
          <div class="flex flex-wrap gap-2">
            <button
              type="button"
              disabled={deciding}
              onclick={() => decide(r.id, 'granted')}
              class="rounded border border-success bg-success/10 px-3 py-1 text-xs font-medium text-success hover:bg-success/20 disabled:opacity-50"
              data-testid="{testidPrefix}-grant-{r.id}">{t('admin.requests.grant')}</button
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
