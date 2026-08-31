<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The CONTRIBUTOR picker (#1173, sprint 18d) — "who made the work".
   *
   * # ⛔ THE SELECTION IS PAGE STATE, NEVER DERIVED FROM THE SUGGESTIONS
   *
   * This is the property the whole component is shaped around. The
   * suggestion list is QUERY-RELATIVE: it re-runs whenever any other
   * dimension changes, and the server reduces it by
   * `Selection.ForFacet(FacetOwner)` so an OR dimension does not filter
   * itself out of existence. That means a contributor the caller has
   * already ticked can legitimately DISAPPEAR from the list — narrow by
   * a file type they never used and they are simply not a contributor to
   * the narrowed set any more.
   *
   * If the chips were rendered from the response, that disappearance
   * would silently REMOVE an active filter, and the URL would keep
   * carrying an `owner:` term with no control on screen. So `selected`
   * comes in as a prop, is written only by a click, and is never
   * reconciled against what came back.
   *
   * # The identity is `user_ref`
   *
   * Not the username. `user_username_uniq_idx` is unique
   * CASE-SENSITIVELY while the `owner:` predicate matches
   * `LOWER(fu.username) = LOWER($n)`, so two users can satisfy one term;
   * `username` is nullable; and a numeric username collides with the
   * same predicate's `owner_user_ref::TEXT = $n` arm.
   *
   * # The prefix is OPTIONAL
   *
   * An empty box browses the whole qualifying population, page by page.
   * That is a correctness property rather than a nicety: a contributor
   * with no stored name at all renders as `user <ref>` through ADR
   * 0070's rung 4, which is INVENTED rather than stored, so no prefix
   * can ever match it. Requiring one would make them permanently
   * unreachable.
   */
  import { t } from '$stores/lang.svelte';

  type Contributor = { user_ref: number; display_name: string };

  let {
    /**
     * The rest of the query, already serialized — the SAME parameters
     * the Search button navigates to, minus nothing. The server drops
     * this dimension's own terms; the client must not second-guess it.
     */
    query = '',
    selected = [] as { ref: number; label: string }[],
    onchange = (_v: { ref: number; label: string }[]) => {},
  }: {
    query?: string;
    selected?: { ref: number; label: string }[];
    onchange?: (v: { ref: number; label: string }[]) => void;
  } = $props();

  let prefix = $state('');
  let options = $state<Contributor[]>([]);
  let cursor = $state('');
  let loading = $state(false);
  let loadingMore = $state(false);
  /**
   * ⛔ An error is NOT an empty list. `/search/contributors` refuses an
   * anonymous caller, and a page that rendered that 401 as "no
   * contributors" would be stating an authoritative absence it has no
   * evidence for.
   */
  let failed = $state(false);
  let loaded = $state(false);

  const SUGGEST_DEBOUNCE_MS = 250;

  function url(cur: string): string {
    const p = new URLSearchParams(query);
    p.set('prefix', prefix.trim());
    if (cur) p.set('cursor', cur);
    return `/api/v1/search/contributors?${p.toString()}`;
  }

  let gen = 0;

  /**
   * Reload from the FIRST page whenever the prefix or the surrounding
   * query changes — continuation is only meaningful within one candidate
   * set, and resuming a cursor issued against a different one would
   * skip and duplicate rows in equal measure.
   *
   * Both dependencies are read SYNCHRONOUSLY, before the timer: Svelte
   * collects an effect's dependencies from the reads made in its own
   * frame, so anything read inside the callback would not register.
   */
  $effect(() => {
    const p = prefix;
    const q = query;
    void p;
    void q;
    const g = ++gen;
    loading = true;
    const timer = setTimeout(async () => {
      try {
        const res = await fetch(url(''), { credentials: 'include' });
        if (g !== gen) return;
        if (!res.ok) {
          options = [];
          cursor = '';
          failed = true;
          loaded = true;
          return;
        }
        const data = (await res.json()) as {
          contributors?: Contributor[];
          next_cursor?: string;
        };
        if (g !== gen) return;
        options = data.contributors ?? [];
        cursor = data.next_cursor ?? '';
        failed = false;
        loaded = true;
      } catch {
        if (g === gen) {
          options = [];
          cursor = '';
          failed = true;
          loaded = true;
        }
      } finally {
        if (g === gen) loading = false;
      }
    }, SUGGEST_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  });

  /**
   * Continue. APPENDS rather than replaces, because the boundary
   * property being relied on is that the next page starts exactly where
   * this one stopped — no duplicate and no skipped contributor.
   */
  async function loadMore() {
    if (!cursor || loadingMore) return;
    const g = gen;
    loadingMore = true;
    try {
      const res = await fetch(url(cursor), { credentials: 'include' });
      if (g !== gen || !res.ok) return;
      const data = (await res.json()) as { contributors?: Contributor[]; next_cursor?: string };
      if (g !== gen) return;
      options = [...options, ...(data.contributors ?? [])];
      cursor = data.next_cursor ?? '';
    } catch {
      /* the list stands; the button can be pressed again */
    } finally {
      loadingMore = false;
    }
  }

  function pick(c: Contributor) {
    if (selected.some((s) => s.ref === c.user_ref)) return;
    onchange([...selected, { ref: c.user_ref, label: c.display_name }]);
  }

  function drop(ref: number) {
    onchange(selected.filter((s) => s.ref !== ref));
  }

  const selectedRefs = $derived(new Set(selected.map((s) => s.ref)));
</script>

<div data-testid="advanced-contributor">
  <div id="advanced-contributor-label" class="mb-1.5 text-sm font-medium text-fg">
    {t('search.advanced_page.contributor_heading')}
  </div>
  <p class="mb-2 text-xs text-fg-muted">{t('search.advanced_page.contributor_hint')}</p>

  {#if selected.length > 0}
    <div class="mb-2 flex flex-wrap gap-1.5" data-testid="advanced-contributor-chips">
      {#each selected as s (s.ref)}
        <span
          data-testid="advanced-contributor-chip-{s.ref}"
          class="inline-flex items-center gap-1 rounded-full border border-accent bg-accent px-2.5 py-1 text-xs text-on-accent"
        >
          {s.label}
          <button
            type="button"
            onclick={() => drop(s.ref)}
            data-testid="advanced-contributor-remove-{s.ref}"
            aria-label={t('search.advanced_page.contributor_remove', { name: s.label })}
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
    bind:value={prefix}
    aria-labelledby="advanced-contributor-label"
    placeholder={t('search.advanced_page.contributor_placeholder')}
    data-testid="advanced-contributor-input"
    class="min-h-11 w-full rounded-md border border-border-strong bg-surface px-3 py-2 text-sm"
  />

  <div class="mt-2">
    {#if failed}
      <p class="text-xs text-danger" data-testid="advanced-contributor-error">
        {t('search.advanced_page.contributor_error')}
      </p>
    {:else if loading && !loaded}
      <p class="text-xs text-fg-muted">{t('search.advanced_page.contributor_loading')}</p>
    {:else if options.length === 0}
      <p class="text-xs text-fg-muted" data-testid="advanced-contributor-empty">
        {t('search.advanced_page.contributor_empty')}
      </p>
    {:else}
      <div
        class="flex max-h-56 flex-col gap-0.5 overflow-y-auto"
        data-testid="advanced-contributor-options"
      >
        {#each options as c (c.user_ref)}
          {@const on = selectedRefs.has(c.user_ref)}
          <button
            type="button"
            onclick={() => pick(c)}
            aria-pressed={on}
            disabled={on}
            data-testid="advanced-contributor-option-{c.user_ref}"
            class="shrink-0 rounded px-2 py-2 text-left text-sm leading-5 transition-colors
                   {on
              ? 'cursor-default text-fg-muted opacity-60'
              : 'text-fg hover:bg-state-hover'}"
          >
            {c.display_name}
          </button>
        {/each}
      </div>
      {#if cursor}
        <button
          type="button"
          onclick={loadMore}
          disabled={loadingMore}
          data-testid="advanced-contributor-more"
          class="mt-1 rounded border border-border px-2 py-1 text-xs text-fg-muted hover:bg-state-hover hover:text-fg disabled:opacity-50"
        >
          {t('search.advanced_page.contributor_more')}
        </button>
      {/if}
    {/if}
  </div>
</div>
