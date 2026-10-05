<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The FILE SIZE bounds (#1173, sprint 18d) —
   * `filter=file_size:>=<bytes>` and `filter=file_size:<=<bytes>`.
   *
   * The wire form is 18b's: the operator LEADS and the bound is a bare
   * base-10 byte count reaching a `::BIGINT` cast, because `file_size`
   * names exactly one column and has nothing to disambiguate.
   *
   * # What this component owns, and what it deliberately does not
   *
   * It owns the UNITS — a person thinks in megabytes and the column is
   * bytes. It does NOT own the arithmetic: that is `$lib/fileSizeBound`,
   * where it is `BigInt`-exact and unit-tested against digit strings,
   * because `assets.file_size_bytes` reaches past 2^53 and a JavaScript
   * `number` stops being able to tell consecutive integers apart there.
   *
   * The two bounds share ONE unit. Two units would let a range read
   * "between 5 MB and 2 GB" as two separately-scaled numbers with no
   * visible relationship, and the range is one thought.
   *
   * # An invalid bound emits NO TERM
   *
   * Not a clamped one and not a zero. A bound that quietly became a
   * different bound is the "looks narrowed and is not" failure the whole
   * `filter=` design exists to avoid, so the value is refused inline and
   * the search runs without it.
   */
  import { t } from '$stores/lang.svelte';
  import {
    FILE_SIZE_UNITS,
    fileSizeToBytes,
    type FileSizeUnit,
  } from '$lib/fileSizeBound';

  let {
    min = '',
    max = '',
    unit = 'MB' as FileSizeUnit,
    onchange = (_v: { min: string; max: string; unit: FileSizeUnit }) => {},
  }: {
    min?: string;
    max?: string;
    unit?: FileSizeUnit;
    onchange?: (v: { min: string; max: string; unit: FileSizeUnit }) => void;
  } = $props();

  /** The failure reason for one end, or '' when there is nothing to say
   *  (a valid bound, or an empty one — empty means "no bound asked
   *  for", which is not an error). */
  function problem(raw: string, edge: 'lower' | 'upper'): string {
    const r = fileSizeToBytes(raw, unit, edge);
    if (r.ok || r.reason === 'empty') return '';
    return r.reason === 'out_of_range'
      ? t('search.advanced_page.size_out_of_range')
      : t('search.advanced_page.size_malformed');
  }

  const minProblem = $derived(problem(min, 'lower'));
  const maxProblem = $derived(problem(max, 'upper'));
</script>

<div data-testid="advanced-filesize">
  <div class="mb-1.5 text-sm font-medium text-fg">
    {t('search.advanced_page.size_heading')}
  </div>
  <p class="mb-2 text-xs text-fg-muted">{t('search.advanced_page.size_hint')}</p>

  <div class="flex flex-wrap items-center gap-2">
    <label class="flex items-center gap-1.5 text-xs text-fg-muted">
      {t('search.advanced_page.size_at_least')}
      <input
        type="text"
        inputmode="decimal"
        value={min}
        oninput={(e) => onchange({ min: e.currentTarget.value, max, unit })}
        data-testid="advanced-filesize-min"
        aria-invalid={minProblem !== ''}
        class="min-h-11 w-28 rounded-md border border-border-strong bg-surface px-2 py-1.5 text-sm"
      />
    </label>
    <label class="flex items-center gap-1.5 text-xs text-fg-muted">
      {t('search.advanced_page.size_at_most')}
      <input
        type="text"
        inputmode="decimal"
        value={max}
        oninput={(e) => onchange({ min, max: e.currentTarget.value, unit })}
        data-testid="advanced-filesize-max"
        aria-invalid={maxProblem !== ''}
        class="min-h-11 w-28 rounded-md border border-border-strong bg-surface px-2 py-1.5 text-sm"
      />
    </label>
    <select
      value={unit}
      onchange={(e) => onchange({ min, max, unit: e.currentTarget.value as FileSizeUnit })}
      data-testid="advanced-filesize-unit"
      aria-label={t('search.advanced_page.size_unit')}
      class="min-h-11 rounded-md border border-border-strong bg-surface px-2 py-1.5 text-sm"
    >
      {#each FILE_SIZE_UNITS as u (u)}
        <option value={u}>{u}</option>
      {/each}
    </select>
  </div>

  {#if minProblem || maxProblem}
    <p class="mt-1.5 text-xs text-danger" data-testid="advanced-filesize-error">
      {minProblem || maxProblem}
    </p>
  {/if}
</div>
