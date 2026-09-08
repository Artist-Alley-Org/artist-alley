<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // THE OPERATOR SURFACE for batch metadata edit (#1119, #1173).
  //
  // Sprint 20c-i shipped the whole server half: preview, token, apply,
  // the six partitions, the five outcomes, the audit envelope. And it
  // had ZERO production callers, because no UI in the app could build
  // the TYPED selection the contract requires. This is that caller.
  //
  // # The shape of the flow, and why it has exactly three steps
  //
  //   COMPOSE → PREVIEW (mandatory) → COMMITTED APPLY
  //
  // The preview is not an optional confirmation dialog bolted on for
  // comfort: it is the only thing that mints the token the apply
  // spends, and the apply carries NO mode and NO value of its own. So
  // there is no path through this component that writes without having
  // first shown the operator what would be written.
  //
  // # WHAT THIS COMPONENT IS NOT ALLOWED TO COMPUTE
  //
  // Everything about the SIZE and SHAPE of the operation is the
  // server's answer and is rendered, never derived:
  //
  //   - the expanded target count. A selected post expands through its
  //     membership AS IT IS NOW, two selected posts sharing a member
  //     contribute that asset ONCE, and a directly selected asset that
  //     is also a member of a selected post is the same single target.
  //     A client that counted would be counting a different set.
  //   - the six partition counts, and the two totals derived from them.
  //   - `empty_posts`, the selected posts holding no members.
  //   - `selection_entry_count`.
  //   - `resolved_value`, the CANONICAL value after vocabulary
  //     canonicalisation and rich-text sanitising. The review step
  //     shows THIS and not the operator's input echo, because they can
  //     legitimately differ (an alias followed to its target, a casing
  //     variant collapsed onto the existing slug) and the value that
  //     matters is the one that will be stored.
  //   - the two ceilings, 500 entries and 1000 expanded targets.
  //
  // # THE APPLY RESULT IS NOT "IT WORKED"
  //
  // HTTP 200 from the apply means COMMITTED, not "everything changed".
  // A committed apply reports `outcome_counts` over five outcomes, and
  // four of them are not `changed`:
  //
  //   conflict              the value moved since the preview; the rest
  //                         of the batch still proceeded
  //   gone                  the target was SOFT-DELETED since the
  //                         preview. An ARCHIVED target is NOT gone and
  //                         WAS written
  //   unauthorized_at_apply the caller's effective permission over that
  //                         target changed; the server names WHICH of
  //                         the three gates, and this surface renders
  //                         that reason and never infers one
  //   error                 an unexpected per-target failure
  //
  // So the result step renders all five counts, and every non-changed
  // target individually. `changed: 0` is still a committed apply (the
  // token is spent and one audit envelope exists) and it is presented
  // as a completed operation that changed nothing, NEVER as a request
  // that did not go through and can be safely retried unchanged.
  //
  // `mintable_terms` (preview) and `committed_terms` (apply) are kept
  // strictly apart for the same reason: a term is minted only if at
  // least one target successfully STORED it, so a preview that listed
  // three new terms and an apply where every target conflicted creates
  // nothing, and this surface must not report otherwise.
  //
  // # REFUSALS
  //
  // A pre-commit refusal, from the preview or from the apply before
  // it writes, is a different kind of thing from a committed partial
  // outcome, and they are rendered in different places on purpose. An
  // apply refusal LEAVES THE TOKEN UNSPENT, so the operator can correct
  // a mistyped confirmation count and retry without re-previewing; the
  // exception is the 409 family (`preview_consumed`, `preview_expired`,
  // `definition_drift`, `vocabulary_drift`, `reference_invalidated`),
  // whose whole remedy is to preview again, so those drop the token.
  //
  // # THE SELECTION SURVIVES ALL OF IT
  //
  // Closing this modal, a refused preview, a refused apply and a
  // COMMITTED apply all leave the selection exactly as it was. There is
  // no silent auto-clear anywhere in this file: an operator who has
  // just changed a field on forty records is frequently about to change
  // a second one on the same forty.

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { selection, type SelectionEntry } from '$stores/selection.svelte';
  import {
    fieldWriteBody,
    fieldWriteValuePresent,
    type FieldWriteType,
    type FieldWriteValue,
  } from '$lib/fieldWriteValue';
  import Modal from './Modal.svelte';
  import FieldValueInput from './FieldValueInput.svelte';

  interface Props {
    open: boolean;
    onclose: () => void;
  }

  let { open, onclose }: Props = $props();

  type Mode = 'overwrite' | 'fill_empties' | 'append' | 'remove';

  interface FieldDef {
    id: string;
    code: string;
    label: string;
    description?: string;
    type: FieldWriteType;
    required: boolean;
    options?: Record<string, unknown>;
    open_vocabulary?: boolean;
    read_only?: boolean;
    regexp_filter?: string | null;
  }

  interface Counts {
    expanded: number;
    eligible: number;
    would_change: number;
    no_op: number;
    refused: number;
    inapplicable: number;
    unreadable: number;
    unauthorized: number;
  }

  interface PreviewTarget {
    asset_id: string;
    partition: string;
    refusal_reason?: string | null;
  }

  interface Preview {
    token: string;
    expires_at: string;
    operation_id: string;
    mode: Mode;
    field_id: string;
    field_code: string;
    field_type: string;
    resolved_value: FieldWriteValue;
    mintable_terms?: string[];
    selection_entry_count: number;
    empty_posts?: string[];
    counts: Counts;
    targets: PreviewTarget[];
  }

  interface ApplyTarget {
    asset_id: string;
    outcome: string;
    unauthorized_reason?: string | null;
  }

  interface ApplyResult {
    operation_id: string;
    mode: Mode;
    field_id: string;
    field_code: string;
    counts: Counts;
    outcome_counts: {
      changed: number;
      conflict: number;
      gone: number;
      unauthorized_at_apply: number;
      error: number;
    };
    targets: ApplyTarget[];
    committed_terms?: string[];
  }

  interface Refusal {
    status: number;
    reason: string;
    error: string;
    field?: string | null;
    expected?: number | null;
    actual?: number | null;
  }

  let step = $state<'compose' | 'review' | 'result'>('compose');
  let defs = $state<FieldDef[]>([]);
  let defsLoading = $state(false);
  let defsError = $state(false);
  let fieldId = $state('');
  let mode = $state<Mode>('overwrite');
  let value = $state<FieldWriteValue>({});
  let preview = $state<Preview | null>(null);
  let reason = $state('');
  let confirmCount = $state('');
  let result = $state<ApplyResult | null>(null);
  /** Pre-commit refusal, from either call. NEVER the same slot as a
   *  committed partial outcome. */
  let refusal = $state<Refusal | null>(null);
  let busy = $state(false);

  const def = $derived(defs.find((d) => d.id === fieldId) ?? null);

  /** `append` and `remove` are multi_select ONLY. Every other type
   *  refuses them batch-wide with 422 `mode_not_supported_for_type`. */
  const modes = $derived<Mode[]>(
    def?.type === 'multi_select'
      ? ['overwrite', 'fill_empties', 'append', 'remove']
      : ['overwrite', 'fill_empties'],
  );

  /** The typed confirmation is REQUIRED for `overwrite` and `remove`
   *  and FORBIDDEN otherwise: supplying it on `fill_empties` or
   *  `append` is refused with 400 `confirm_count_not_applicable`
   *  rather than ignored. So the control is not merely hidden on those
   *  two modes, the member is not sent. */
  const confirmRequired = $derived(
    preview !== null && (preview.mode === 'overwrite' || preview.mode === 'remove'),
  );

  /** Rendering only. The server owns the rule and answers
   *  `confirm_count_mismatch` naming the value it wanted. */
  const canPreview = $derived(
    !!def && selection.count > 0 && fieldWriteValuePresent(def.type, value) && !busy,
  );
  const canApply = $derived(
    preview !== null && reason.trim().length > 0 && (!confirmRequired || confirmCount !== '') && !busy,
  );

  /** Reset to a fresh compose step. Deliberately does NOT touch the
   *  selection: nothing in this component clears it. */
  function resetFlow() {
    step = 'compose';
    preview = null;
    result = null;
    refusal = null;
    reason = '';
    confirmCount = '';
  }

  /** A PLAIN let, deliberately not `$state`.
   *
   *  Svelte 5 collects an effect's dependencies CALL-FRAME DEEP: any
   *  rune the effect's callee reads becomes the effect's dependency
   *  too. A guard of `if (defs.length > 0) return` inside `loadDefs`
   *  would therefore make this effect depend on `defs`, which
   *  `loadDefs` then writes, and the fetch would re-arm itself off its
   *  own result. Keeping the latch out of the reactive graph is what
   *  makes "load the field list once, when this first opens" mean
   *  exactly that. */
  let defsRequested = false;

  $effect(() => {
    if (!open || defsRequested) return;
    defsRequested = true;
    void loadDefs();
  });

  async function loadDefs() {
    defsLoading = true;
    defsError = false;
    try {
      // `subject_kind=asset` EXPLICITLY. Omitting it returns BOTH
      // kinds, and a collection-scoped definition offered here would
      // be a field the batch endpoints can never write, because the targets
      // are assets.
      //
      // NO `asset_type`: one batch may hold heterogeneous assets, and
      // post entries expand into whatever their members are, so there
      // is no single type to narrow by. Mixed applicability is not a
      // client problem to solve: it arrives as the `inapplicable`
      // partition of the preview.
      //
      // NO `status`, which is the live-schema reading (#528): active
      // AND deprecated, archived excluded. A deprecated definition is
      // one an operator stopped wanting NEW values in; it can still
      // hold values on existing rows and batch.go refuses only
      // `archived`, so filtering deprecated out here would be a
      // client-only eligibility rule the server does not have.
      const res = await api.GET('/fields', {
        params: { query: { subject_kind: 'asset' } },
      });
      if (res.error || !res.data) {
        defsError = true;
        return;
      }
      defs = res.data as unknown as FieldDef[];
    } catch {
      defsError = true;
    } finally {
      defsLoading = false;
      // A failed load may be retried by closing and reopening; a
      // successful one is kept for the life of the page.
      if (defsError) defsRequested = false;
    }
  }

  function readRefusal(status: number, err: unknown): Refusal {
    const e = (err ?? {}) as Record<string, unknown>;
    return {
      status,
      reason: typeof e.reason === 'string' ? e.reason : 'unknown',
      error: typeof e.error === 'string' ? e.error : t('batch.refusal_generic'),
      field: typeof e.field === 'string' ? e.field : null,
      expected: typeof e.expected === 'number' ? e.expected : null,
      actual: typeof e.actual === 'number' ? e.actual : null,
    };
  }

  /** The typed selection, straight off the store.
   *
   *  It is a MAP and not a re-derivation: the entries already ARE
   *  `{kind, id}` pairs, which is the whole point of the store holding
   *  the pair rather than attaching a kind here. */
  function selectionPayload(): SelectionEntry[] {
    return selection.entries.map((e) => ({ kind: e.kind, id: e.id }));
  }

  async function submitPreview() {
    if (!def || busy) return;
    busy = true;
    refusal = null;
    try {
      const res = await api.POST('/batch/asset-fields/preview', {
        body: {
          mode,
          field_id: def.id,
          selection: selectionPayload(),
          value: fieldWriteBody(def.type, value),
        } as never,
      });
      if (res.error || !res.data) {
        refusal = readRefusal(res.response.status, res.error);
        preview = null;
        return;
      }
      preview = res.data as unknown as Preview;
      confirmCount = '';
      reason = '';
      step = 'review';
    } finally {
      busy = false;
    }
  }

  async function submitApply() {
    const p = preview;
    if (!p || busy) return;
    busy = true;
    refusal = null;
    try {
      const body: Record<string, unknown> = { token: p.token, reason };
      if (p.mode === 'overwrite' || p.mode === 'remove') {
        body.confirm_count = Number(confirmCount);
      }
      const res = await api.POST('/batch/asset-fields/apply', { body: body as never });
      if (res.error || !res.data) {
        const r = readRefusal(res.response.status, res.error);
        refusal = r;
        // 409 is the "the world moved, preview again" family, and it is
        // the ONLY family that spends or invalidates the token. Every
        // other refusal leaves it usable, so the operator corrects the
        // payload in place.
        if (r.status === 409) {
          preview = null;
          step = 'compose';
        }
        return;
      }
      result = res.data as unknown as ApplyResult;
      step = 'result';
    } finally {
      busy = false;
    }
  }

  /** The canonical value, rendered for reading. Whatever member the
   *  field's type uses. */
  function resolvedText(v: FieldWriteValue): string {
    if (v.value_options != null) return v.value_options.join(', ');
    if (v.value_text != null) return v.value_text;
    if (v.value_num != null) return String(v.value_num);
    if (v.value_date != null) return v.value_date;
    if (v.value_ref != null) return v.value_ref;
    return '';
  }

  const nonChanged = $derived(
    (result?.targets ?? []).filter((tg) => tg.outcome !== 'changed'),
  );

  function close() {
    resetFlow();
    onclose();
  }
</script>

<Modal title={t('batch.title')} {open} onclose={close} panelClass="max-w-2xl">
  <div data-testid="batch-edit-modal" class="space-y-4">
    <p class="text-sm text-fg-muted" data-testid="batch-selection-summary">
      {t('batch.selection_summary', { count: String(selection.count) })}
    </p>

    {#if refusal}
      <!-- A PRE-COMMIT refusal. Nothing was written, no vocabulary term
           was created, no audit envelope exists. Kept in its own region
           so it can never be read as a partial success. -->
      <div
        role="alert"
        data-testid="batch-refusal"
        data-refusal-reason={refusal.reason}
        data-refusal-status={refusal.status}
        class="rounded-lg border border-danger bg-danger-subtle px-3 py-2 text-sm text-danger"
      >
        <span class="font-medium">{t(`batch.reason.${refusal.reason}`)}</span>
        <span class="block text-fg-muted">{refusal.error}</span>
        {#if refusal.expected != null}
          <span class="block" data-testid="batch-refusal-expected">
            {t('batch.refusal_expected', { count: String(refusal.expected) })}
          </span>
        {/if}
      </div>
    {/if}

    {#if step === 'compose'}
      {#if defsLoading}
        <p class="text-sm text-fg-muted" data-testid="batch-fields-loading">{t('batch.loading_fields')}</p>
      {:else if defsError}
        <p class="text-sm text-danger" data-testid="batch-fields-error">{t('batch.fields_error')}</p>
      {:else}
        <label class="block space-y-1">
          <span class="text-sm font-medium text-fg">{t('batch.field_label')}</span>
          <select
            bind:value={fieldId}
            data-testid="batch-field-select"
            class="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm text-fg"
          >
            <option value="">{t('batch.field_placeholder')}</option>
            {#each defs as d (d.id)}
              <option value={d.id}>{d.label}</option>
            {/each}
          </select>
        </label>

        <label class="block space-y-1">
          <span class="text-sm font-medium text-fg">{t('batch.mode_label')}</span>
          <select
            bind:value={mode}
            data-testid="batch-mode-select"
            class="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm text-fg"
          >
            {#each modes as m (m)}
              <option value={m}>{t(`batch.mode.${m}`)}</option>
            {/each}
          </select>
        </label>

        {#if def}
          <div data-testid="batch-value-input" class="space-y-1">
            <span class="text-sm font-medium text-fg">{t('batch.value_label')}</span>
            <FieldValueInput
              {def}
              {value}
              onchange={(v) => (value = { ...value, ...v })}
            />
          </div>
        {/if}
      {/if}
    {/if}

    {#if step === 'review' && preview}
      <!-- THE SERVER'S ANSWER, rendered. Every number below came out of
           the preview body; none of it is recomputed here. -->
      <div class="space-y-3" data-testid="batch-preview-panel">
        <dl class="grid grid-cols-2 gap-x-4 gap-y-1 text-sm sm:grid-cols-4">
          <div><dt class="text-fg-muted">{t('batch.counts.entries')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-selection-entry-count">{preview.selection_entry_count}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.counts.expanded')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-count-expanded">{preview.counts.expanded}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.counts.eligible')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-count-eligible">{preview.counts.eligible}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.counts.would_change')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-count-would-change">{preview.counts.would_change}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.counts.no_op')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-count-no-op">{preview.counts.no_op}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.counts.refused')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-count-refused">{preview.counts.refused}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.counts.inapplicable')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-count-inapplicable">{preview.counts.inapplicable}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.counts.unreadable')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-count-unreadable">{preview.counts.unreadable}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.counts.unauthorized')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-count-unauthorized">{preview.counts.unauthorized}</dd></div>
        </dl>

        <p class="text-sm">
          <span class="text-fg-muted">{t('batch.resolved_value_label')}</span>
          <!-- The CANONICAL value, not the operator's input. -->
          <span class="font-medium text-fg" data-testid="batch-resolved-value">{resolvedText(preview.resolved_value)}</span>
        </p>

        {#if (preview.empty_posts ?? []).length > 0}
          <p class="text-sm text-fg-muted" data-testid="batch-empty-posts" data-count={(preview.empty_posts ?? []).length}>
            {t('batch.empty_posts', { count: String((preview.empty_posts ?? []).length) })}
          </p>
        {/if}

        {#if (preview.mintable_terms ?? []).length > 0}
          <!-- Terms that WOULD be created. Listing them is not creating
               them, and a term is minted at apply only if at least one
               target successfully stores it. -->
          <p class="text-sm text-fg-muted" data-testid="batch-mintable-terms">
            {t('batch.mintable_terms', { terms: (preview.mintable_terms ?? []).join(', ') })}
          </p>
        {/if}

        <label class="block space-y-1">
          <span class="text-sm font-medium text-fg">{t('batch.reason_label')}</span>
          <textarea
            bind:value={reason}
            rows="2"
            data-testid="batch-reason"
            class="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm text-fg"
          ></textarea>
        </label>

        {#if confirmRequired}
          <label class="block space-y-1">
            <span class="text-sm font-medium text-fg">
              {t('batch.confirm_label', { count: String(preview.counts.would_change) })}
            </span>
            <input
              type="number"
              bind:value={confirmCount}
              data-testid="batch-confirm-count"
              class="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm text-fg"
            />
          </label>
        {/if}
      </div>
    {/if}

    {#if step === 'result' && result}
      <!-- A COMMITTED APPLY. The token is spent and exactly one audit
           envelope exists for it, including when `changed` is 0. -->
      <div class="space-y-3" data-testid="batch-result" data-operation-id={result.operation_id}>
        <p class="text-sm font-medium text-fg" data-testid="batch-result-headline">
          {t('batch.committed', { count: String(result.outcome_counts.changed) })}
        </p>
        <dl class="grid grid-cols-2 gap-x-4 gap-y-1 text-sm sm:grid-cols-5">
          <div><dt class="text-fg-muted">{t('batch.outcome.changed')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-outcome-changed">{result.outcome_counts.changed}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.outcome.conflict')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-outcome-conflict">{result.outcome_counts.conflict}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.outcome.gone')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-outcome-gone">{result.outcome_counts.gone}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.outcome.unauthorized_at_apply')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-outcome-unauthorized">{result.outcome_counts.unauthorized_at_apply}</dd></div>
          <div><dt class="text-fg-muted">{t('batch.outcome.error')}</dt>
            <dd class="font-medium text-fg" data-testid="batch-outcome-error">{result.outcome_counts.error}</dd></div>
        </dl>

        {#if nonChanged.length > 0}
          <!-- Every target that did NOT change, named. Hiding these
               behind a generic success is the failure mode this whole
               step exists to prevent. -->
          <ul class="space-y-1 text-sm" data-testid="batch-outcome-targets">
            {#each nonChanged as tg (tg.asset_id)}
              <li
                data-testid="batch-outcome-target"
                data-asset-id={tg.asset_id}
                data-outcome={tg.outcome}
                data-unauthorized-reason={tg.unauthorized_reason ?? ''}
                class="text-fg-muted"
              >
                <span class="font-medium text-fg">{t(`batch.outcome.${tg.outcome}`)}</span>
                <span>{t(`batch.outcome_detail.${tg.outcome}`)}</span>
                {#if tg.unauthorized_reason}
                  <!-- The SERVER's sub-reason. Never inferred here. -->
                  <span>{t(`batch.unauthorized_reason.${tg.unauthorized_reason}`)}</span>
                {/if}
                <span class="font-mono text-xs">{tg.asset_id}</span>
              </li>
            {/each}
          </ul>
        {/if}

        <!-- Terms that WERE created. Empty when no would_change target
             stored one, and the preview's mintable list is NOT reported
             here in its place. -->
        <p class="text-sm text-fg-muted" data-testid="batch-committed-terms" data-count={(result.committed_terms ?? []).length}>
          {(result.committed_terms ?? []).length > 0
            ? t('batch.committed_terms', { terms: (result.committed_terms ?? []).join(', ') })
            : t('batch.committed_terms_none')}
        </p>

        <p class="text-sm text-fg-muted" data-testid="batch-selection-kept">
          {t('batch.selection_kept', { count: String(selection.count) })}
        </p>
      </div>
    {/if}
  </div>

  {#snippet footer()}
    <div class="flex justify-end gap-2">
      <button
        type="button"
        onclick={close}
        class="inline-flex h-10 items-center rounded-md px-3 text-sm font-medium text-fg-muted hover:bg-state-hover hover:text-fg"
      >
        {t('batch.close')}
      </button>
      {#if step === 'compose'}
        <button
          type="button"
          disabled={!canPreview}
          onclick={submitPreview}
          data-testid="batch-preview-submit"
          class="inline-flex h-10 items-center rounded-md bg-accent px-4 text-sm font-medium text-on-accent disabled:opacity-50"
        >
          {busy ? t('batch.previewing') : t('batch.preview')}
        </button>
      {:else if step === 'review'}
        <button
          type="button"
          onclick={resetFlow}
          data-testid="batch-back"
          class="inline-flex h-10 items-center rounded-md px-3 text-sm font-medium text-fg-muted hover:bg-state-hover hover:text-fg"
        >
          {t('batch.back')}
        </button>
        <button
          type="button"
          disabled={!canApply}
          onclick={submitApply}
          data-testid="batch-apply-submit"
          class="inline-flex h-10 items-center rounded-md bg-accent px-4 text-sm font-medium text-on-accent disabled:opacity-50"
        >
          {busy ? t('batch.applying') : t('batch.apply')}
        </button>
      {:else}
        <button
          type="button"
          onclick={resetFlow}
          data-testid="batch-again"
          class="inline-flex h-10 items-center rounded-md bg-accent px-4 text-sm font-medium text-on-accent"
        >
          {t('batch.again')}
        </button>
      {/if}
    </div>
  {/snippet}
</Modal>
