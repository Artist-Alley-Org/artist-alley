<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Phase 1.14.A-bridge — per-source tag chip.
  //
  // Reusable primitive that renders one asset tag with a tiny source
  // marker so the operator can see at a glance whether a tag came
  // from a human, a bulk import, or an AI run. When confidence is
  // present (AI tags) we show a 0-100 number after the tag value.
  //
  // The current read APIs ship tags as a flat string[] for
  // backwards-compat; this component is the visual primitive ready
  // for the next phase (1.14.B) which surfaces source/confidence in
  // the JSON projection. Consumers can adopt incrementally — render
  // existing flat tags with source='manual' (the safe default) and
  // upgrade to typed source once the field is wired.
  //
  // Source palette is intentionally muted (border-only): the tag
  // value is the foreground, the source marker is metadata.

  import { t } from '$stores/lang.svelte';

  type Source = 'manual' | 'ai' | 'import';

  interface Props {
    value: string;
    source?: Source;
    confidence?: number | null; // 0..1 when AI provider supplied one
    // Optional dot-source label hover surface. Default uses i18n.
    title?: string;
  }

  let { value, source = 'manual', confidence = null, title }: Props = $props();

  // Tag value: foreground class invariant across sources.
  const baseClass =
    'inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs leading-none';

  // Per-source border + marker tint. The classes are explicit (no
  // template interpolation) so Tailwind's JIT picks them up.
  const sourceClass: Record<Source, string> = {
    manual: 'border-border bg-surface text-fg',
    ai:     'border-accent/40 bg-accent/5 text-fg',
    import: 'border-warning/40 bg-warning/5 text-fg',
  };

  const markerClass: Record<Source, string> = {
    manual: 'bg-fg-muted',
    ai:     'bg-accent',
    import: 'bg-warning',
  };

  // Confidence renders as a tiny percentage when >0. Below 50% the
  // text takes a danger tint — the operator's "low-confidence AI
  // suggestion" eye-catch.
  const confPct = confidence != null && confidence > 0 ? Math.round(confidence * 100) : null;
  const confLowClass = confPct != null && confPct < 50 ? 'text-danger' : 'text-fg-muted';

  const tooltip = title ?? t(`asset.tag_source.${source}`);
</script>

<span class="{baseClass} {sourceClass[source]}" title={tooltip} data-source={source}>
  <span class="h-1.5 w-1.5 shrink-0 rounded-full {markerClass[source]}" aria-hidden="true"></span>
  <span class="font-medium">{value}</span>
  {#if confPct != null}
    <span class="ml-0.5 text-[10px] tabular-nums {confLowClass}">{confPct}%</span>
  {/if}
  <span class="sr-only">{tooltip}</span>
</span>
