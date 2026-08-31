<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The FILE TYPE picker (#1173, sprint 18d) — `filter=extension:<ext>`.
   *
   * # ⭐ THE SUGGESTIONS ARE THE EXISTING FACET VOCABULARY
   *
   * `GET /search/facets?facets=extension` already computes exactly this
   * list, under this caller's visibility, against this query, reduced by
   * `Selection.ForFacet(FacetExtension)` so a ticked extension does not
   * collapse its own bucket list to one entry. There is deliberately no
   * static registry, no second lookup endpoint, no filesystem-derived
   * vocabulary: those would each be a second answer to "what file types
   * exist here", and the facet layer's answer is the one the rail on
   * /search already shows.
   *
   * # ⛔ FREE ENTRY IS REQUIRED, NOT A CONVENIENCE
   *
   * The buckets are capped at `LIMIT 25` and are query-dependent, so the
   * list is a suggestion and never an enumeration. A person looking for
   * a format outside the current result set has to be able to type it.
   *
   * # ⛔ THE SELECTION IS PAGE STATE
   *
   * Same reason as the contributor picker: `FacetExtension` is not
   * conjunctive, so `ForFacet` drops its own terms when the buckets are
   * computed, and a selected extension can legitimately leave the
   * suggestion list. Rendering the chips from the response would delete
   * an active filter the moment that happened.
   *
   * # Normalization is the FRONTEND's, deliberately
   *
   * See `$lib/extensionFilter` — `FacetExtension` has no
   * `CanonicalValue` case, so canonicalising server-side would change
   * `Selection.CacheKey` for every saved search already carrying an
   * extension term.
   */
  import { t } from '$stores/lang.svelte';
  import { addExtension, normalizeExtension } from '$lib/extensionFilter';

  let {
    /** The rest of the query, serialized. */
    query = '',
    selected = [] as string[],
    onchange = (_v: string[]) => {},
  }: {
    query?: string;
    selected?: string[];
    onchange?: (v: string[]) => void;
  } = $props();

  let draft = $state('');
  let buckets = $state<string[]>([]);
  let failed = $state(false);

  const SUGGEST_DEBOUNCE_MS = 250;
  let gen = 0;

  $effect(() => {
    const q = query;
    void q;
    const g = ++gen;
    const timer = setTimeout(async () => {
      try {
        const p = new URLSearchParams(q);
        p.set('facets', 'extension');
        const res = await fetch(`/api/v1/search/facets?${p.toString()}`, {
          credentials: 'include',
        });
        if (g !== gen) return;
        if (!res.ok) {
          buckets = [];
          failed = true;
          return;
        }
        const data = (await res.json()) as {
          facets?: Record<string, { buckets?: { value: string }[] }>;
        };
        if (g !== gen) return;
        buckets = (data.facets?.extension?.buckets ?? [])
          .map((b) => normalizeExtension(b.value))
          .filter((v): v is string => v !== null);
        failed = false;
      } catch {
        if (g === gen) {
          buckets = [];
          failed = true;
        }
      }
    }, SUGGEST_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  });

  function commit() {
    const next = addExtension(selected, draft);
    draft = '';
    if (next.length !== selected.length) onchange(next);
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      commit();
    }
  }

  function toggle(ext: string) {
    onchange(
      selected.includes(ext) ? selected.filter((v) => v !== ext) : addExtension(selected, ext),
    );
  }

  const selectedSet = $derived(new Set(selected));
</script>

<div data-testid="advanced-extension">
  <div id="advanced-extension-label" class="mb-1.5 text-sm font-medium text-fg">
    {t('search.advanced_page.extension_heading')}
  </div>
  <p class="mb-2 text-xs text-fg-muted">{t('search.advanced_page.extension_hint')}</p>

  {#if selected.length > 0}
    <div class="mb-2 flex flex-wrap gap-1.5" data-testid="advanced-extension-chips">
      {#each selected as ext (ext)}
        <span
          data-testid="advanced-extension-chip-{ext}"
          class="inline-flex items-center gap-1 rounded-full border border-accent bg-accent px-2.5 py-1 text-xs text-on-accent"
        >
          {ext}
          <button
            type="button"
            onclick={() => toggle(ext)}
            data-testid="advanced-extension-remove-{ext}"
            aria-label={t('search.advanced_page.extension_remove', { name: ext })}
            class="leading-none opacity-80 hover:opacity-100"
          >
            ×
          </button>
        </span>
      {/each}
    </div>
  {/if}

  <input
    type="search"
    bind:value={draft}
    onkeydown={onKey}
    onblur={commit}
    aria-labelledby="advanced-extension-label"
    placeholder={t('search.advanced_page.extension_placeholder')}
    data-testid="advanced-extension-input"
    class="min-h-11 w-full rounded-md border border-border-strong bg-surface px-3 py-2 text-sm"
  />

  {#if failed}
    <p class="mt-2 text-xs text-danger" data-testid="advanced-extension-error">
      {t('search.advanced_page.extension_error')}
    </p>
  {:else if buckets.length > 0}
    <div class="mt-2 flex flex-wrap gap-1.5" data-testid="advanced-extension-options">
      {#each buckets as ext (ext)}
        {@const on = selectedSet.has(ext)}
        <button
          type="button"
          onclick={() => toggle(ext)}
          aria-pressed={on}
          data-testid="advanced-extension-option-{ext}"
          class="rounded-full border px-2.5 py-1 text-xs transition-colors
                 {on
            ? 'border-accent bg-accent text-on-accent'
            : 'border-border bg-surface text-fg-muted hover:bg-state-hover hover:text-fg'}"
        >
          {ext}
        </button>
      {/each}
    </div>
  {/if}
</div>
