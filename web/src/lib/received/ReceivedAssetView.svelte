<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // One asset another studio shared with this instance (#1574), by id.
  //
  // GET /federation/received-assets/{id} answers only while the caller may
  // view it (the rule is evaluated on every request); anything else is the
  // same 404, shown as "not available". What the sender declared (file
  // name, type, rights, description) is labelled as declared. Only a
  // verified original carries values this instance established (its size,
  // SHA-256 and detected type); every other file's size and SHA-256 are
  // labelled declared. The download link is offered only when the share
  // carries the original file (original_available), and the download
  // itself is checked again by the server.
  //
  // A record its sending studio withdrew (#1575, spec v1 §17.3) answers
  // 410 with the withdrawal notice, only to a caller who could view it:
  // when it was withdrawn, the studio's own reason if it gave one
  // (labelled as theirs), and what was withdrawn. Nothing of the record
  // is shown and no download is offered.
  //
  // `id` can change while this component stays mounted (a client
  // navigation from one received asset to another reuses the page). Each
  // change clears the previous record AT ONCE, so neither it nor its
  // download link is shown for the new id, and only the newest request
  // may write: a late answer for an earlier id is dropped.
  //
  // When the server shows the asset's latest received metadata (#1577,
  // `current_metadata`), ReceivedMetadata renders it before the original
  // declared details, which are then labelled as the original record's,
  // with that record's own activity and actor. It is part of `detail`, so
  // it disappears with the record, and it never selects files.
  import { untrack } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import ReceivedMetadata from '$lib/received/ReceivedMetadata.svelte';
  import {
    declaredEntries,
    formatBytes,
    originalUrl,
    verifiedHere,
    type ReceivedAssetDetail,
    type ReceivedAssetWithdrawal,
  } from '$lib/received/received';

  let { id }: { id: string } = $props();

  let detail = $state<ReceivedAssetDetail | null>(null);
  let loading = $state(true);
  let missing = $state(false);
  let withdrawn = $state<ReceivedAssetWithdrawal | null>(null);
  let error = $state<string | null>(null);

  // The newest request's number; a response from any other is dropped.
  let seq = 0;

  $effect(() => {
    const want = id;
    untrack(() => void load(want));
    return () => {
      seq++;
    };
  });

  async function load(want: string): Promise<void> {
    const mine = ++seq;
    detail = null;
    missing = false;
    withdrawn = null;
    error = null;
    loading = true;
    try {
      const r = await api.GET('/federation/received-assets/{id}', {
        params: { path: { id: want } },
      });
      if (mine !== seq) return;
      if (r.response?.status === 404) {
        missing = true;
        return;
      }
      if (r.response?.status === 410) {
        withdrawn = r.error as ReceivedAssetWithdrawal;
        return;
      }
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('common.failed_to_load');
        return;
      }
      detail = r.data as ReceivedAssetDetail;
    } catch (e) {
      if (mine !== seq) return;
      error = e instanceof Error ? e.message : t('common.failed_to_load');
    } finally {
      if (mine === seq) loading = false;
    }
  }

  function stateLabel(state: string): string {
    switch (state) {
      case 'verified':
        return t('account.received.state_verified');
      case 'not_transferred':
        return t('account.received.state_not_transferred');
      default:
        return t('account.received.state_pending');
    }
  }

  const original = $derived(detail?.asset.original);
  const claims = $derived(declaredEntries(detail?.declared as Record<string, unknown> | undefined));
  const current = $derived(detail?.current_metadata);
</script>

{#if loading}
  <p class="text-sm text-fg-muted" data-testid="received-loading">{t('common.loading')}</p>
{:else if missing}
  <p class="rounded border border-border px-3 py-2 text-sm text-fg-muted" data-testid="received-missing">
    {t('account.received.not_found')}
  </p>
{:else if withdrawn}
  <section class="rounded border border-border px-4 py-3 text-sm" data-testid="received-withdrawn">
    <h2 class="mb-1 text-lg font-semibold">{t('account.received.withdrawn_title')}</h2>
    <p class="mb-3 text-fg-muted">{t('account.received.withdrawn_body')}</p>
    <dl class="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1">
      <dt class="text-fg-muted">{t('account.received.withdrawn_on')}</dt>
      <dd data-testid="received-withdrawn-deleted">{new Date(withdrawn.deleted).toLocaleString()}</dd>
      {#if withdrawn.reason}
        <dt class="text-fg-muted">{t('account.received.withdrawn_reason')}</dt>
        <dd class="break-words" data-testid="received-withdrawn-reason">{withdrawn.reason}</dd>
      {/if}
      <dt class="text-fg-muted">{t('account.received.source')}</dt>
      <dd class="break-all">{withdrawn.origin_asset_uri}</dd>
      <dt class="text-fg-muted">{t('account.received.withdrawn_last_version')}</dt>
      <dd class="break-all">{withdrawn.last_version}</dd>
    </dl>
  </section>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
    {error}
  </p>
{:else if detail && original}
  <header class="mb-6">
    <h2 class="text-2xl font-semibold" data-testid="received-title">
      {original.declared_original_filename ?? original.sha256.slice(0, 12)}
    </h2>
    {#if original.declared_original_filename}
      <p class="mt-1 text-xs text-fg-muted">{t('account.received.declared_name')}: {t('account.received.declared_label')}</p>
    {/if}
  </header>

  <dl class="mb-6 grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
    <dt class="text-fg-muted">{t('account.received.peer')}</dt>
    <dd>{detail.asset.peer_name ?? detail.asset.peer_url}</dd>
    <dt class="text-fg-muted">{t('account.received.source')}</dt>
    <dd class="break-all" data-testid="received-source">{detail.asset.origin_asset_uri}</dd>
    <dt class="text-fg-muted">{t('account.received.version')}</dt>
    <dd class="break-all">{detail.asset.version_number} ({detail.asset.version_uri})</dd>
    <dt class="text-fg-muted">{t('account.received.received_at')}</dt>
    <dd>{new Date(detail.asset.completed_at).toLocaleString()}</dd>
  </dl>

  <div class="mb-6">
    {#if detail.original_available}
      <a
        href={originalUrl(detail.asset.id)}
        download={original.declared_original_filename ?? undefined}
        class="inline-block rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover"
        data-testid="received-download"
      >
        {t('account.received.download')}
      </a>
    {:else}
      <p class="text-sm text-fg-muted" data-testid="received-download-unavailable">
        {t('account.received.download_unavailable')}
      </p>
    {/if}
  </div>

  <h3 class="mb-1 text-lg font-semibold">{t('account.received.files')}</h3>
  <p class="mb-2 text-xs text-fg-muted">{t('account.received.files_note')}</p>
  <table class="mb-6 w-full text-left text-sm" data-testid="received-files">
    <thead class="text-xs text-fg-muted">
      <tr>
        <th class="py-1 pr-4">{t('account.received.role')}</th>
        <th class="py-1 pr-4">{t('account.received.declared_type')}</th>
        <th class="py-1 pr-4">{t('account.received.detected_type')}</th>
        <th class="py-1 pr-4">{t('account.received.size')}</th>
        <th class="py-1 pr-4">{t('account.received.sha256')}</th>
        <th class="py-1">{t('account.received.file')}</th>
      </tr>
    </thead>
    <tbody>
      {#each detail.files as file (file.position)}
        {@const established = verifiedHere(file)}
        {@const basis = established ? t('account.received.value_verified') : t('account.received.value_declared')}
        <tr
          class="border-t border-border align-top"
          data-testid="received-file"
          data-role={file.role}
          data-verified={established ? 'true' : 'false'}
        >
          <td class="py-1 pr-4">{file.role}</td>
          <td class="py-1 pr-4">{file.declared_media_type}</td>
          <td class="py-1 pr-4" data-testid="received-detected">
            {file.detected_media_type ?? t('account.received.not_detected')}
          </td>
          <td class="py-1 pr-4">
            {formatBytes(file.size_bytes)}
            <span class="block text-xs text-fg-muted" data-testid="received-size-basis">{basis}</span>
          </td>
          <td class="py-1 pr-4">
            <span class="font-mono text-xs break-all">{file.sha256}</span>
            <span class="block text-xs text-fg-muted" data-testid="received-sha256-basis">{basis}</span>
          </td>
          <td class="py-1">{stateLabel(file.state)}</td>
        </tr>
      {/each}
    </tbody>
  </table>

  {#if current}
    <ReceivedMetadata metadata={current} peerName={detail.asset.peer_name ?? detail.asset.peer_url} />
  {/if}

  <h3 class="mb-1 text-lg font-semibold" data-testid="received-claims-title">{current ? t('account.received.metadata.original_title') : t('account.received.declared_claims')}</h3>
  <p class="mb-2 text-xs text-fg-muted">{t('account.received.declared_label')}</p>
  {#if current}
    <dl class="mb-2 grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-xs" data-testid="received-original-provenance">
      <dt class="text-fg-muted">{t('account.received.metadata.original_activity')}</dt>
      <dd class="break-all" data-testid="received-original-activity">{detail.activity_uri}</dd>
      <dt class="text-fg-muted">{t('account.received.metadata.original_actor')}</dt>
      <dd class="break-all" data-testid="received-original-actor">{detail.actor_uri}</dd>
    </dl>
  {/if}
  {#if claims.length === 0}
    <p class="text-sm text-fg-muted">{t('account.received.no_claims')}</p>
  {:else}
    <dl class="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm" data-testid="received-claims">
      {#each claims as c (c.key)}
        <dt class="text-fg-muted">{c.key}</dt>
        <dd class="break-all">{c.value}</dd>
      {/each}
    </dl>
  {/if}
{/if}
