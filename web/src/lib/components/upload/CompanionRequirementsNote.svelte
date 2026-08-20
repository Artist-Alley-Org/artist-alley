<script lang="ts">
  /**
   * What a just-uploaded model says it still needs (#754).
   *
   * ## The bug this removes
   *
   * A multi-file model only renders if its siblings are registered as
   * companions. Nothing on the upload path derived that list, so an
   * artist who uploaded a GLB without knowing it names
   * `Textures/planks.png` got an upload that succeeded, a job that
   * succeeded, and a card and viewer that came out grey. The failure is
   * SILENT and reads as a renderer bug — it is the symptom chain #689
   * chased into the renderer before #750 traced it to missing companion
   * rows. Naming the missing files is the smallest thing that removes
   * it.
   *
   * ## Every state says something different, on purpose
   *
   * The one answer this component must never give is a confident
   * "nothing needed" for a file nobody could read. So:
   *
   *   - `unsupported` renders NOTHING. We cannot read this format's
   *     references (STL, PLY, DAE) or it is not a model at all. Saying
   *     "no companions needed" would be a claim we have no basis for,
   *     and saying "we could not read it" about an ordinary PNG is
   *     noise.
   *   - `unreadable` says so out loud. The bytes are a format we do
   *     read and they did not parse.
   *   - `ok` with a non-empty `missing` names each path.
   *   - `ok` with everything satisfied confirms it — a short positive,
   *     because "this model has all its textures" is worth knowing
   *     before you publish.
   *
   * `partial` is OBJ, and it is called out rather than papered over: an
   * .obj names .mtl libraries and each .mtl names its own textures, but
   * that second level lives inside a file that has not been uploaded
   * yet. Under-reporting silently would recreate the original bug one
   * level down.
   */
  import { t } from '$stores/lang.svelte';
  import type { CompanionRequirements } from '$stores/upload.svelte';

  interface Props {
    requirements: CompanionRequirements | null;
    testid?: string;
  }

  let { requirements, testid = 'row' }: Props = $props();

  const missing = $derived(requirements?.missing ?? []);
  const satisfied = $derived(
    requirements?.status === 'ok' && missing.length === 0 && (requirements?.declared.length ?? 0) > 0,
  );
</script>

{#if requirements && requirements.status !== 'unsupported'}
  {#if requirements.status === 'unreadable'}
    <p
      class="rounded border border-border bg-surface-elevated px-2 py-1.5 text-xs text-fg-muted"
      data-testid="companion-req-unreadable-{testid}"
    >
      {t('companions.unreadable')}
    </p>
  {:else if missing.length > 0}
    <div
      class="rounded border border-warning bg-warning-container px-2 py-1.5 text-xs text-on-warning-container"
      role="status"
      data-testid="companion-req-missing-{testid}"
    >
      <p class="font-medium">{t('companions.missing_heading', { count: missing.length })}</p>
      <ul class="mt-1 list-inside list-disc space-y-0.5">
        {#each missing as path (path)}
          <li class="break-all font-mono" data-testid="companion-req-missing-path">{path}</li>
        {/each}
      </ul>
      <p class="mt-1">{t('companions.missing_help')}</p>
      {#if requirements.partial}
        <p class="mt-1" data-testid="companion-req-partial-{testid}">
          {t('companions.partial_help')}
        </p>
      {/if}
    </div>
  {:else if satisfied}
    <p class="text-xs text-fg-muted" data-testid="companion-req-satisfied-{testid}">
      {t('companions.all_present')}
    </p>
  {/if}
{/if}
