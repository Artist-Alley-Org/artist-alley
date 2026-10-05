<script lang="ts">
  /**
   * The maker's AI declaration — ONE three-position control (#1167,
   * ADR 0094 §5).
   *
   * ## Why three positions and not a checkbox
   *
   * The issue asked for a checkbox. Three positions is the same SINGLE
   * interaction — one decision, one click — at higher resolution, so it
   * costs the friction budget nothing. What it buys is the distinction
   * people actually argue about: "I upscaled a texture" and "a model
   * made this" are different claims about authorship, and a boolean
   * collapses them irreversibly. IPTC found the same split necessary
   * (`compositeWithTrainedAlgorithmicMedia` vs
   * `trainedAlgorithmicMedia`).
   *
   * ## ⚠️ Why there is no pre-selected option
   *
   * `null` — nothing chosen — is a real, storable state meaning
   * UNDECLARED: nobody was asked. It is NOT `none`. `none` is a
   * positive claim ("no generative AI was involved") and defaulting the
   * control to it would have the form make that claim on the artist's
   * behalf before they touched anything — a fabricated disclaimer, on
   * the one topic where a false disclaimer is the worst available
   * error. So the group starts with nothing checked and stores nothing
   * unless a person picks.
   *
   * A "clear" affordance appears once something IS picked, because a
   * declaration is a statement a person makes and a person may have
   * made it by accident. On the edit surface that maps to
   * `clear_ai_provenance`; at create it simply goes back to omitting
   * the key.
   *
   * ## It is a filter, never a gate
   *
   * Nothing about this value changes who may see the work. It does not
   * blur, withhold, or interact with sensitivity or clearance (ADR 0094
   * §4). The control therefore renders for everyone, unconditionally —
   * unlike the mature checkbox beside it, which is hidden when the
   * instance disallows mature content because the server would refuse
   * the write.
   */
  import { t } from '$stores/lang.svelte';
  import type { AiProvenance } from '$lib/aiProvenance';

  interface Props {
    value: AiProvenance;
    /** Suffix for the data-testid hooks, so two instances can coexist. */
    testid?: string;
    disabled?: boolean;
    /** Renders the group's own heading. Off when the caller supplies one. */
    heading?: boolean;
    onchange: (v: AiProvenance) => void;
  }

  let { value, testid = 'ai-provenance', disabled = false, heading = true, onchange }: Props =
    $props();

  const options: { key: Exclude<AiProvenance, null>; label: string; hint: string }[] = [
    { key: 'none', label: 'ai_provenance.none', hint: 'ai_provenance.none_hint' },
    { key: 'assisted', label: 'ai_provenance.assisted', hint: 'ai_provenance.assisted_hint' },
    { key: 'generated', label: 'ai_provenance.generated', hint: 'ai_provenance.generated_hint' },
  ];

  // Unique per instance so two groups on one page do not share a
  // radio name and steal each other's selection.
  const groupName = `ai-provenance-${testid}`;
</script>

<fieldset
  class="min-w-0"
  data-testid="ai-provenance-{testid}"
  data-value={value ?? 'undeclared'}
>
  {#if heading}
    <legend class="mb-1 block text-xs font-medium text-fg-muted">
      {t('ai_provenance.legend')}
    </legend>
  {/if}

  <div class="flex flex-col gap-1.5">
    {#each options as opt (opt.key)}
      <label class="inline-flex items-start gap-2 text-sm">
        <input
          type="radio"
          name={groupName}
          value={opt.key}
          checked={value === opt.key}
          {disabled}
          onchange={() => onchange(opt.key)}
          data-testid="ai-provenance-{testid}-{opt.key}"
          class="mt-0.5 h-4 w-4 shrink-0 accent-accent"
        />
        <span class="min-w-0">
          <span class="block text-fg">{t(opt.label)}</span>
          <span class="block text-xs text-fg-muted">{t(opt.hint)}</span>
        </span>
      </label>
    {/each}
  </div>

  {#if value !== null}
    <button
      type="button"
      {disabled}
      onclick={() => onchange(null)}
      data-testid="ai-provenance-{testid}-clear"
      class="mt-1.5 text-xs text-fg-muted underline-offset-2 hover:text-fg hover:underline"
    >
      {t('ai_provenance.clear')}
    </button>
  {:else}
    <!-- Says what the empty state MEANS. Without this the group reads
         as an unanswered question the artist forgot, rather than as a
         question they are entitled not to answer. -->
    <p class="mt-1.5 text-xs text-fg-muted" data-testid="ai-provenance-{testid}-undeclared">
      {t('ai_provenance.undeclared_hint')}
    </p>
  {/if}
</fieldset>
