<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The asset's latest received metadata (#1577, spec v1 §18.4): the
  // sending studio's newest descriptive snapshot, shown apart from the
  // original declared details it never replaces.
  //
  // Every value is the sender's text, not verified here, and is shown as
  // text: no markup, Markdown, links, images or fetches, and nothing is
  // trimmed or shortened. The four claims are always listed; a null one
  // says it was not supplied in this snapshot (it clears that claim) and
  // never falls back to the original details. The provenance is what was
  // received with the snapshot; it is not a verification of the claims.
  // `published` is the server's exact string, nanoseconds included.
  import { t } from '$stores/lang.svelte';
  import {
    languageEntries,
    type ReceivedAssetCurrentMetadata,
    type ReceivedAssetMetadataLanguageMap,
  } from '$lib/received/received';

  let { metadata, peerName }: { metadata: ReceivedAssetCurrentMetadata; peerName: string } = $props();

  const s = $derived(metadata.snapshot);
  const provenance = $derived([
    { key: 'revision', label: t('account.received.metadata.revision'), value: metadata.revision_id },
    { key: 'activity', label: t('account.received.metadata.activity'), value: metadata.activity_uri },
    { key: 'actor', label: t('account.received.metadata.actor'), value: metadata.actor_uri },
    { key: 'bound_version', label: t('account.received.metadata.bound_version'), value: metadata.version_uri },
    { key: 'file_set', label: t('account.received.metadata.file_set'), value: metadata.file_set_sha256 },
    { key: 'origin_key', label: t('account.received.metadata.origin_key'), value: metadata.origin_signer_key_id },
    {
      key: 'signature_algorithm',
      label: t('account.received.metadata.signature_algorithm'),
      value: metadata.origin_signature_algorithm,
    },
    { key: 'origin_signature', label: t('account.received.metadata.origin_signature'), value: metadata.origin_signature_value },
    { key: 'recipient_key', label: t('account.received.metadata.recipient_key'), value: metadata.recipient_signer_key_id },
    { key: 'received_at', label: t('account.received.metadata.received_at'), value: metadata.received_at },
  ]);
</script>

{#snippet claim(value: string | null | undefined)}
  {#if value == null}
    <span class="italic text-fg-muted" data-testid="received-metadata-empty">{t('account.received.metadata.empty_value')}</span>
  {:else}
    <span class="whitespace-pre-wrap break-words" dir="auto" data-testid="received-metadata-value">{value}</span>
  {/if}
{/snippet}

{#snippet part(map: ReceivedAssetMetadataLanguageMap, which: 'label' | 'value', heading: string)}
  <div class="mb-1" data-testid="received-metadata-statement-{which}">
    <p class="text-xs text-fg-muted">{heading}</p>
    {#each languageEntries(map) as [tag, texts] (tag)}
      <div class="mt-1" data-testid="received-metadata-lang" data-lang={tag}>
        <span class="text-xs text-fg-muted">{tag === 'none' ? t('account.received.metadata.no_language') : tag}</span>
        <ul>
          {#each texts as text, i (i)}
            <li class="whitespace-pre-wrap break-words" dir="auto" data-testid="received-metadata-text">{text}</li>
          {/each}
        </ul>
      </div>
    {/each}
  </div>
{/snippet}

<section class="mb-6 rounded border border-border px-4 py-3 text-sm" data-testid="received-metadata">
  <h3 class="mb-1 text-lg font-semibold">{t('account.received.metadata.title')}</h3>
  <p class="mb-3 text-xs text-fg-muted">{t('account.received.metadata.note')}</p>
  <dl class="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2">
    <dt class="text-fg-muted">{t('account.received.peer')}</dt>
    <dd class="break-words" data-testid="received-metadata-peer">{peerName}</dd>
    <dt class="text-fg-muted">{t('account.received.metadata.claim_title')}</dt>
    <dd data-testid="received-metadata-claim" data-claim="title">{@render claim(s.title)}</dd>
    <dt class="text-fg-muted">{t('account.received.metadata.claim_description')}</dt>
    <dd data-testid="received-metadata-claim" data-claim="description">{@render claim(s.description)}</dd>
    <dt class="text-fg-muted">{t('account.received.metadata.claim_rights')}</dt>
    <dd data-testid="received-metadata-claim" data-claim="rights">{@render claim(s.rights)}</dd>
    <dt class="text-fg-muted">{t('account.received.metadata.claim_statement')}</dt>
    <dd data-testid="received-metadata-claim" data-claim="requiredStatement">
      {#if s.requiredStatement == null}
        {@render claim(null)}
      {:else}
        {@render part(s.requiredStatement.label, 'label', t('account.received.metadata.statement_label'))}
        {@render part(s.requiredStatement.value, 'value', t('account.received.metadata.statement_value'))}
      {/if}
    </dd>
    <dt class="text-fg-muted">{t('account.received.metadata.published')}</dt>
    <dd class="break-all" data-testid="received-metadata-published"><time datetime={metadata.published}>{metadata.published}</time></dd>
  </dl>
  <details class="mt-3" data-testid="received-metadata-provenance">
    <summary class="cursor-pointer text-xs text-fg-muted">{t('account.received.metadata.provenance')}</summary>
    <dl class="mt-2 grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-xs">
      {#each provenance as f (f.key)}
        <dt class="text-fg-muted">{f.label}</dt>
        <dd class="break-all font-mono" data-testid="received-metadata-field" data-field={f.key}>{f.value}</dd>
      {/each}
    </dl>
  </details>
</section>
