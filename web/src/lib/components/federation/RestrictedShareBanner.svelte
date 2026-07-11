<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<!--
  Sender-side banner per the 1.22.D §5.5 addition 3 lock-in.

  When an operator shares restricted- or embargo-tier content
  with a federated peer, this banner makes the encrypted-
  federation gap visible at share time. The share record still
  gets created (so when 1.22.I ships encrypted-federation, the
  backlog backfills naturally), but the operator knows nothing
  actually federates today.

  Two render modes:
    - mode="page"   compact banner at the top of /admin/federation/shares
                    explaining the general v1 → 1.22.I gap (always
                    visible, no per-object check). For operator
                    awareness.
    - mode="grant"  per-share warning inside a grant dialog. Takes
                    `sensitivity` + only renders when sensitivity is
                    restricted | embargo. Future grant form slots
                    this in next to the submit button.

  When 1.22.I lands, both modes either drop entirely or pivot to
  a "this share will federate via encrypted delivery" success
  notice. The component is intentionally narrow so the swap is
  trivial.
-->

<script lang="ts">
  import { t } from '$stores/lang.svelte';

  type Props = {
    mode?: 'page' | 'grant';
    /** restricted | embargo trigger the grant-mode banner; others render nothing */
    sensitivity?: string;
  };

  const { mode = 'page', sensitivity = '' }: Props = $props();

  const needsBanner = $derived(
    mode === 'page' ||
      (mode === 'grant' && (sensitivity === 'restricted' || sensitivity === 'embargo'))
  );
</script>

{#if needsBanner}
  <div
    class="rounded-md border border-warning/40 bg-warning/10 px-4 py-3 text-sm text-fg"
    role="note"
    aria-label={t('federation.banner_aria_label')}
  >
    <div class="flex items-start gap-2">
      <span class="mt-0.5 text-warning" aria-hidden="true">⚠</span>
      <div class="flex-1 space-y-1">
        {#if mode === 'page'}
          <p class="font-medium">{t('federation.not_supported_headline')}</p>
          <p class="text-fg-muted">
            {t('federation.page_body')}
          </p>
        {:else}
          <p class="font-medium">
            {t('federation.grant_headline', { sensitivity })}
          </p>
          <p class="text-fg-muted">
            {t('federation.grant_body')}
          </p>
          <ul class="ml-5 list-disc text-fg-muted">
            <li>{t('federation.opt_share_anyway')}</li>
            <li>{t('federation.opt_cancel')}</li>
          </ul>
        {/if}
      </div>
    </div>
  </div>
{/if}
