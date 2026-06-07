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
    aria-label="Encrypted federation status"
  >
    <div class="flex items-start gap-2">
      <span class="mt-0.5 text-warning" aria-hidden="true">⚠</span>
      <div class="flex-1 space-y-1">
        {#if mode === 'page'}
          <p class="font-medium">Encrypted federation is not yet supported.</p>
          <p class="text-fg-muted">
            Activities that target <strong>restricted</strong> or <strong>embargo</strong>
            sensitivity objects will not federate to peers until Phase 1.22.I lands
            encrypted delivery. The share records you grant today are persisted, so
            when encrypted federation ships those backlogged activities will federate
            automatically &mdash; but until then, expect zero outbound federation for
            sensitive objects.
          </p>
        {:else}
          <p class="font-medium">
            This object is marked <strong>{sensitivity}</strong> &mdash;
            encrypted federation is not yet supported.
          </p>
          <p class="text-fg-muted">
            Sharing now will create the share record but no activities will
            federate until Phase 1.22.I ships encrypted delivery. You can:
          </p>
          <ul class="ml-5 list-disc text-fg-muted">
            <li>Share anyway, knowing activity federation is paused for this object until 1.22.I; or</li>
            <li>Cancel and downgrade the object&rsquo;s sensitivity to a non-restricted tier first.</li>
          </ul>
        {/if}
      </div>
    </div>
  </div>
{/if}
