<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The restricted-member tile (#883).
  //
  // A post or collection can contain an asset the VIEWER is not entitled
  // to. The server does not drop it from the array — it sends a
  // placeholder carrying `restricted: true`, the membership's own
  // columns, and the owner's display name, and nothing else. This is
  // that placeholder rendered.
  //
  // WHY IT IS VISIBLE RATHER THAN ABSENT. Omitting the member would hide
  // that a restriction exists at all, and "request access" (#881) needs
  // something to hang off. The viewer is meant to be able to tell that
  // something is there that they cannot see, and whose it is.
  //
  // WHAT IT MAY SAY. Exactly two things: that it is restricted, and the
  // owner's display name. Not the title — that is the owner's rule, and
  // it is why this is a separate component rather than a CardFallback
  // variant: CardFallback's whole job is to state the format, the kind
  // and the title, and every one of those is withheld here. Sharing the
  // component would mean a `restricted` flag threaded through three
  // conditionals, which is one edit away from printing a title again.
  //
  // The frame is CardFallback's — same matte, same hatch, same size
  // container and tier breakpoints — so a restricted tile sits in a grid
  // of no-preview tiles without reading as a different kind of object.
  //
  // ## The ask (#881)
  //
  // A placeholder that says "no" now also says "you can ask". The button
  // is rendered only when the caller passed an `assetId` AND someone is
  // signed in — an anonymous visitor has no account to file a request
  // against, and a disabled control that never explains itself is worse
  // than none.
  //
  // The button carries no asset text. Its label and aria-label are the
  // fixed string "Request access"; the id goes into the POST and never
  // into the DOM. Everything the dialog is allowed to say is in
  // RequestAccessDialog's header.

  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import RequestAccessDialog from './RequestAccessDialog.svelte';

  interface Props {
    /** The asset owner's display name, or null when the server could not
     *  resolve one (unowned asset, deleted profile). Null renders the
     *  neutral line rather than an empty one — a blank where a name
     *  belongs reads as a rendering bug. */
    ownerName?: string | null;
    /** The restricted asset's id, when the caller wants the tile to
     *  offer "request access". Null (the default) renders the plate
     *  alone.
     *
     *  Opt-in rather than derived from the id the tile already has,
     *  because "there is a restricted asset here" and "asking about it
     *  is the sensible next move" are different claims. A PostCard whose
     *  COVER is restricted is a visible post the viewer can open; the
     *  ask belongs on the member inside it, not on a tile that navigates
     *  somewhere else. */
    assetId?: string | null;
  }

  let { ownerName = null, assetId = null }: Props = $props();

  const ownerLine = $derived(
    ownerName ? t('card.restricted.owner', { owner: ownerName }) : t('card.restricted.owner_unknown'),
  );

  let dialogOpen = $state(false);
  let asked = $state(false);

  const canAsk = $derived(!!assetId && !!auth.user);
</script>

<div class="plate absolute inset-0 text-fg-muted" data-card-restricted="true">
  <div class="stack">
    <span class="glyph" aria-hidden="true">
      <svg
        xmlns="http://www.w3.org/2000/svg"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="1.3"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <rect x="3" y="11" width="18" height="11" rx="2" />
        <path d="M7 11V7a5 5 0 0 1 10 0v4" />
      </svg>
    </span>
    <span class="label">{t('card.restricted.label')}</span>
    <span class="owner">{ownerLine}</span>
    {#if canAsk}
      <!-- z-10 clears the tile's own frame layers. The button is the
           only interactive thing on a restricted tile — AssetCard
           renders no link, no menu and no checkbox here — so nothing
           competes with it for the click. -->
      <button
        type="button"
        data-testid="request-access-open"
        class="ask relative z-10"
        disabled={asked}
        aria-label={asked ? t('card.restricted.asked') : t('card.restricted.ask')}
        onclick={(e) => {
          e.preventDefault();
          e.stopPropagation();
          dialogOpen = true;
        }}
      >
        {asked ? t('card.restricted.asked') : t('card.restricted.ask')}
      </button>
    {/if}
  </div>
  <span class="sr-only">{t('card.restricted.sr')}</span>
</div>

{#if assetId}
  <RequestAccessDialog
    {assetId}
    {ownerName}
    open={dialogOpen}
    onclose={() => (dialogOpen = false)}
    onsubmitted={() => (asked = true)}
  />
{/if}

<style>
  /* Geometry is CardFallback's, deliberately — see the header note. The
     duplication is the point: the two plates must look like siblings,
     and a shared stylesheet would couple a security surface to a
     presentational one. */
  .plate {
    container-type: size;
    container-name: plate;
  }

  .plate::before {
    content: '';
    position: absolute;
    inset: 0;
    background-image: repeating-linear-gradient(135deg, currentColor 0 1px, transparent 1px 9px);
    opacity: 0.055;
    pointer-events: none;
  }

  .stack {
    position: absolute;
    inset: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    text-align: center;
    flex-direction: row;
    gap: 0.5ch;
    /* Clear of the checkbox + ⋮ menu band on a floor-tier tile. */
    padding-inline: 3.25rem;
  }

  .glyph {
    display: none;
    color: color-mix(in oklab, currentColor 55%, transparent);
  }
  .glyph svg {
    width: clamp(1.5rem, 21cqmin, 4.5rem);
    height: clamp(1.5rem, 21cqmin, 4.5rem);
  }

  .label {
    font-family: var(
      --font-mono,
      ui-monospace,
      SFMono-Regular,
      Menlo,
      Consolas,
      'Liberation Mono',
      monospace
    );
    font-size: clamp(0.75rem, 10cqmin, 2.25rem);
    line-height: 1.1;
    letter-spacing: 0.14em;
    margin-inline-end: -0.14em;
    color: var(--color-fg);
    white-space: nowrap;
    text-transform: uppercase;
  }

  .owner {
    font-size: clamp(0.6875rem, 4.5cqmin, 0.875rem);
    line-height: 1.2;
    letter-spacing: 0.02em;
    min-width: 0;
    max-width: 22ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .owner::before {
    content: '·';
    margin-inline-end: 0.5ch;
  }

  /* The ask. Sized in container units like the rest of the plate so it
     shrinks with the tile rather than overflowing a floor-tier one, and
     hidden entirely below the tier where the plate stops being a column
     (see the container query) — at that size the row is label + owner
     on one line and a third element would push the owner out. */
  .ask {
    display: none;
    border-radius: 0.375rem;
    border: 1px solid color-mix(in oklab, currentColor 35%, transparent);
    padding: 0.3em 0.85em;
    font-size: clamp(0.6875rem, 3.2cqmin, 0.8125rem);
    line-height: 1.2;
    color: var(--color-fg);
    white-space: nowrap;
  }
  .ask:hover:not(:disabled) {
    background: color-mix(in oklab, currentColor 12%, transparent);
  }
  .ask:disabled {
    opacity: 0.65;
    cursor: default;
  }

  @container plate (min-height: 7rem) {
    .stack {
      flex-direction: column;
      gap: 0.45em;
      padding-inline: 1.25rem;
    }
    .glyph {
      display: block;
      margin-bottom: 0.15em;
    }
    .owner::before {
      content: none;
      margin: 0;
    }
    .owner {
      font-size: clamp(0.6875rem, 3.4cqmin, 0.8125rem);
      letter-spacing: 0.08em;
    }
    .ask {
      display: inline-block;
      margin-top: 0.35em;
    }
  }
</style>
