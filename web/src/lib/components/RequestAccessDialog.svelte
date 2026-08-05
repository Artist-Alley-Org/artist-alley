<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // "Ask the owner for access" (#881) — the dialog behind the button on
  // a restricted placeholder.
  //
  // ## What it may say about the asset: NOTHING
  //
  // The client holds exactly `{ id, restricted, owner_display_name }`
  // for a restricted item (#899's allow-list), and this dialog is built
  // from the id and the owner name alone. No title, no filename, no
  // tags, no extension — not in the heading, the body, the button
  // label, an aria-label, a tooltip or the URL. It could not print a
  // title if it wanted to; the props do not carry one, and that is the
  // design. The owner's rule (2026-08-03): "the placeholder should never
  // leak info. Not even title. Only the owner's name."
  //
  // The asset id IS permitted — it is in the allow-list and the request
  // has to name something — but it is never rendered, only posted.
  //
  // ## What it promises: agreement, not access
  //
  // The copy says approval does not reveal the asset yet, because it
  // does not. `user_capability_grants` has no per-object scope, so
  // there is no capability meaning "you may view this one asset";
  // per-asset unlocking is #912 and ADR 0064 records the deferral. A
  // dialog that implied otherwise would send the user away to wait for
  // something that will not happen, which is worse than no button.

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import Modal from './Modal.svelte';

  interface Props {
    /** The restricted asset's id. Posted, never rendered. */
    assetId: string;
    /** The owner's display name, or null when unresolvable. The ONE
     *  piece of asset-adjacent text this dialog is allowed to show,
     *  and it comes from the same placeholder payload the tile does. */
    ownerName?: string | null;
    open: boolean;
    onclose: () => void;
    /** Fired once the server has the ask, so the caller can flip the
     *  button to its "asked" state without a refetch. */
    onsubmitted?: () => void;
  }

  let { assetId, ownerName = null, open, onclose, onsubmitted }: Props = $props();

  let reason = $state('');
  let sending = $state(false);
  let error: string | null = $state(null);

  async function send() {
    if (sending) return;
    sending = true;
    error = null;
    // No `capability` in the body, deliberately. The server stamps the
    // inert marker code (content.access.request). Letting a button on a
    // public tile name a capability is exactly the hazard ADR 0064
    // describes — the requester choosing the value an approver is then
    // asked to grant.
    const body = reason.trim() ? { reason: reason.trim() } : {};
    const { error: apiErr } = await api.POST('/assets/{id}/request-access', {
      params: { path: { id: assetId } },
      body,
    });
    sending = false;
    if (apiErr) {
      error = (apiErr as { error?: string }).error ?? t('request_access.error');
      return;
    }
    reason = '';
    onsubmitted?.();
    onclose();
  }
</script>

<Modal title={t('request_access.title')} {open} {onclose}>
  <div class="space-y-3 text-sm">
    <p class="text-fg-muted">
      {ownerName
        ? t('request_access.body', { owner: ownerName })
        : t('request_access.body_owner_unknown')}
    </p>
    <!-- The honesty line. Not a footnote: a granted request currently
         changes nothing the requester can see, and they are told so
         before they spend the ask. -->
    <p
      data-testid="request-access-no-unlock"
      class="rounded-md border border-border bg-surface px-3 py-2 text-xs text-fg-muted"
    >
      {t('request_access.no_unlock_yet')}
    </p>
    <label class="block">
      <span class="mb-1 block text-xs font-medium text-fg-muted">
        {t('request_access.reason_label')}
      </span>
      <input
        type="text"
        bind:value={reason}
        maxlength="1000"
        data-testid="request-access-reason"
        placeholder={t('request_access.reason_placeholder')}
        class="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm
               focus:outline-none focus:ring-2 focus:ring-ring"
      />
    </label>
    {#if error}
      <p role="alert" class="text-sm text-danger">{error}</p>
    {/if}
  </div>

  {#snippet footer()}
    <button
      type="button"
      onclick={onclose}
      class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated"
    >
      {t('common.cancel')}
    </button>
    <button
      type="button"
      onclick={send}
      disabled={sending}
      data-testid="request-access-send"
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {sending ? t('request_access.sending') : t('request_access.send')}
    </button>
  {/snippet}
</Modal>
