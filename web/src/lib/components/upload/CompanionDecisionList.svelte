<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The files reconciliation REFUSED to place (#1408).
   *
   * ## Why a refusal gets a surface
   *
   * A flat multi-file drop carries no directory information at all —
   * `DataTransfer.files` gives basenames and nothing else. So when two
   * models in one drop declare `wood/diffuse.png` and
   * `metal/diffuse.png`, and one file called `diffuse.png` arrives,
   * there is genuinely no fact available that says which model wanted
   * it. Guessing gets it right half the time and is WRONG SILENTLY the
   * other half: the model renders with someone else's texture, which
   * reads as an art mistake rather than as an upload one.
   *
   * The same is true one level down. An .obj names .mtl libraries and
   * each .mtl names its own textures, so a batch containing an .obj
   * cannot conclude that a leftover file is unrelated to it.
   *
   * Both are questions, and this is where they get asked. Nothing on
   * this list has been uploaded as anything yet — that is the whole
   * reason the answer is still worth something.
   *
   * Mounted on BOTH upload surfaces (the modal and /create) because the
   * question belongs to the batch, not to the window it was dropped in.
   */
  import { upload } from '$stores/upload.svelte';
  import { t } from '$stores/lang.svelte';

  interface Props {
    /** Distinguishes the modal's copy from /create's in the DOM. */
    surface?: string;
  }
  let { surface = 'modal' }: Props = $props();

  const held = $derived(upload.heldCandidates);
  const undecided = $derived(upload.undecidedCandidates);

  // Per-candidate answer, keyed by candidate id. Seeded from the first
  // option the matcher offered — a SUGGESTION sitting in a control the
  // artist has to press, never an answer applied on their behalf.
  let choice = $state<Record<string, { rowId: string; path: string }>>({});

  function current(id: string, options: { rowId: string; path: string }[]) {
    return choice[id] ?? options[0] ?? { rowId: '', path: '' };
  }

  function pickRow(id: string, rowId: string, options: { rowId: string; path: string }[]) {
    const opt = options.find((o) => o.rowId === rowId);
    choice[id] = { rowId, path: opt?.path ?? current(id, options).path };
  }

  function setPath(id: string, path: string, options: { rowId: string; path: string }[]) {
    choice[id] = { rowId: current(id, options).rowId, path };
  }
</script>

{#if held.length > 0}
  <p
    class="rounded border border-border bg-surface-elevated px-2 py-1.5 text-xs text-fg-muted"
    role="status"
    data-testid="companion-holding-{surface}"
  >
    {t('companions.checking', { count: held.length })}
  </p>
{/if}

{#if undecided.length > 0}
  <div
    class="rounded border border-warning bg-warning-container px-2 py-2 text-xs text-on-warning-container"
    role="status"
    data-testid="companion-decisions-{surface}"
  >
    <p class="font-medium">{t('companions.decide_heading', { count: undecided.length })}</p>
    <ul class="mt-2 space-y-2">
      {#each undecided as c (c.id)}
        <li class="rounded border border-border bg-surface p-2 text-fg" data-testid="companion-decision">
          <p class="break-all font-mono text-xs" data-testid="companion-decision-name">{c.path}</p>
          <p class="mt-1 text-xs text-fg-muted" data-testid="companion-decision-reason">
            {c.reason === 'incomplete'
              ? t('companions.decide_incomplete')
              : t('companions.decide_ambiguous')}
          </p>
          {#if c.options.length > 0}
            <div class="mt-2 flex flex-wrap items-center gap-2">
              <select
                aria-label={t('companions.decide_target_aria')}
                data-testid="companion-decision-target"
                class="min-w-0 flex-1 rounded border border-border bg-surface px-2 py-1 text-xs text-fg"
                value={current(c.id, c.options).rowId}
                onchange={(e) =>
                  pickRow(c.id, (e.currentTarget as HTMLSelectElement).value, c.options)}
              >
                {#each c.options as o (o.rowId + o.path)}
                  <option value={o.rowId}>{o.title || o.path}</option>
                {/each}
              </select>
              <input
                type="text"
                aria-label={t('companions.decide_path_aria')}
                data-testid="companion-decision-path"
                class="min-w-0 flex-1 rounded border border-border bg-surface px-2 py-1 font-mono text-xs text-fg"
                value={current(c.id, c.options).path}
                oninput={(e) =>
                  setPath(c.id, (e.currentTarget as HTMLInputElement).value, c.options)}
              />
              <button
                type="button"
                data-testid="companion-decision-attach"
                class="rounded bg-accent px-2 py-1 text-xs font-medium text-on-accent"
                onclick={() => {
                  const a = current(c.id, c.options);
                  void upload.assignCandidate(c.id, a.rowId, a.path);
                }}
              >
                {t('companions.decide_attach')}
              </button>
            </div>
          {/if}
          <div class="mt-2 flex flex-wrap gap-2">
            <button
              type="button"
              data-testid="companion-decision-separate"
              class="rounded border border-border px-2 py-1 text-xs text-fg-muted hover:text-fg"
              onclick={() => upload.releaseCandidate(c.id)}
            >
              {t('companions.decide_separate')}
            </button>
            <button
              type="button"
              data-testid="companion-decision-discard"
              aria-label={t('companions.decide_discard_aria')}
              class="rounded border border-border px-2 py-1 text-xs text-fg-muted hover:text-fg"
              onclick={() => upload.discardCandidate(c.id)}
            >
              {t('companions.decide_discard')}
            </button>
          </div>
        </li>
      {/each}
    </ul>
    {#if undecided.length > 1}
      <button
        type="button"
        data-testid="companion-decision-separate-all"
        class="mt-2 rounded border border-border px-2 py-1 text-xs"
        onclick={() => upload.releaseAllCandidates()}
      >
        {t('companions.decide_separate_all')}
      </button>
    {/if}
  </div>
{/if}
