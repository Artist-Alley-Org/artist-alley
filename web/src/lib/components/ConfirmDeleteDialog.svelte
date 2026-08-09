<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The confirm step in front of every soft delete (#981).
  //
  // WHY A COMPONENT AND NOT `confirm()`. The codebase reaches for the
  // native dialog in ~20 places and that is fine for "cancel this job?"
  // — a yes/no with nothing to say. A delete has three things to say
  // and one thing to collect: what is being removed, where it goes,
  // that it can be brought back, and (when the deleter is not the
  // owner) why. `confirm()` cannot render a textarea, and three
  // hand-rolled dialogs would be three places for the copy to drift
  // apart on the one question users are most likely to get wrong.
  //
  // It is built on Modal.svelte rather than being a fourth bespoke
  // `role="dialog"` — the same reasoning Modal's own header gives, and
  // the reason it can be raised from inside the asset viewer at all
  // (Modal portals out of containing blocks and into the top layer;
  // see $lib/portal).
  //
  // ## The copy is generic about the window, deliberately
  //
  // It says the item goes to your trash and can be restored from
  // there. It does NOT say "for 30 days". The retention window is
  // per-kind operator config (sysconfig soft-delete), /account/trash
  // already prints the exact `purge_after` date per row from the
  // server, and no endpoint hands that config to a client BEFORE the
  // delete. Stating a number here would mean either a new API for one
  // dialog's subtitle or a hardcoded guess the GC does not honour —
  // and a wrong number in a delete confirmation is worse than no
  // number. The trade is: the precise date is one click away, on the
  // page this dialog names.
  //
  // ## The reason box is conditional
  //
  // `askReason` is set by the caller from `shouldAskReason(ownerRef)`:
  // shown only when you are deleting something you do not own. It is
  // OPTIONAL even then, and the copy says the owner will see it,
  // because they will — `deleted_reason` is what #931's appeal flow
  // reads back. Displaying it is that issue's half; capturing it is
  // this one's.

  import Modal from './Modal.svelte';
  import { t } from '$stores/lang.svelte';
  import { REASON_MAX_LEN, type DeletableKind } from '$lib/deletable';

  interface Props {
    open: boolean;
    kind: DeletableKind;
    /** The item's title, shown so the user can tell WHICH thing this
     *  is. Blank falls back to "Untitled" — never to the id. */
    title?: string | null;
    /** Offer the optional reason box. See shouldAskReason. */
    askReason?: boolean;
    /** Runs on confirm with the trimmed reason ('' when not asked or
     *  left blank). The caller owns the request and the errors; this
     *  dialog only stays disabled while `busy` is true. */
    onconfirm: (reason: string) => void | Promise<void>;
    onclose: () => void;
    /** Set by the caller while the DELETE is in flight. */
    busy?: boolean;
    /** Set by the caller when the DELETE came back an error, so the
     *  message lands next to the button that caused it rather than
     *  behind a dialog that has already closed. */
    error?: string | null;
  }

  let {
    open,
    kind,
    title = null,
    askReason = false,
    onconfirm,
    onclose,
    busy = false,
    error = null,
  }: Props = $props();

  let reason = $state('');

  // Clear the box whenever the dialog opens, so a reason typed for one
  // item can never be submitted against the next.
  $effect(() => {
    if (open) reason = '';
  });

  const shownTitle = $derived((title ?? '').trim() || t('common.untitled'));
</script>

<Modal title={t(`delete_confirm.title_${kind}`)} {open} {onclose}>
  <div class="space-y-3 text-sm">
    <p class="text-fg">
      {t(`delete_confirm.body_${kind}`, { title: shownTitle })}
    </p>
    <p
      data-testid="delete-confirm-recoverable"
      class="rounded-md border border-border bg-surface px-3 py-2 text-xs text-fg-muted"
    >
      {t('delete_confirm.recoverable')}
    </p>
    {#if askReason}
      <label class="block">
        <span class="mb-1 block text-xs font-medium text-fg-muted">
          {t('delete_confirm.reason_label')}
        </span>
        <textarea
          bind:value={reason}
          rows="3"
          maxlength={REASON_MAX_LEN}
          data-testid="delete-confirm-reason"
          placeholder={t('delete_confirm.reason_placeholder')}
          class="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm
                 focus:outline-none focus:ring-2 focus:ring-ring"
        ></textarea>
        <span class="mt-1 block text-xs text-fg-muted">
          {t('delete_confirm.reason_hint')}
        </span>
      </label>
    {/if}
    {#if error}
      <p role="alert" data-testid="delete-confirm-error" class="text-sm text-danger">{error}</p>
    {/if}
  </div>

  {#snippet footer()}
    <button
      type="button"
      onclick={onclose}
      disabled={busy}
      class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated disabled:opacity-50"
    >
      {t('common.cancel')}
    </button>
    <button
      type="button"
      onclick={() => void onconfirm(reason.trim())}
      disabled={busy}
      data-testid="delete-confirm-submit"
      class="rounded-md bg-danger px-4 py-1.5 text-sm font-medium text-on-danger
             hover:bg-danger/90 disabled:cursor-not-allowed disabled:bg-danger/40"
    >
      {busy ? t('delete_confirm.deleting') : t('delete_confirm.confirm')}
    </button>
  {/snippet}
</Modal>
