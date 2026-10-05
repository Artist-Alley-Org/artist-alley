<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The search input. Two-way bound via $bindable so the parent owns the
  // canonical query string. Emits an `onsearch` callback when the caller
  // COMMITS so the parent can fire the actual fetch.
  //
  // # Typing does not search (#1156)
  //
  // This used to commit on a 250ms keystroke debounce: every pause while
  // typing re-queried the feed underneath the dropdown. Owner direction is
  // that suggestions may appear while typing but the feed re-queries only
  // on an EXPLICIT commit. So there are exactly four commits, and no timer
  // among them:
  //
  //   - Enter (with no dropdown row highlighted)
  //   - picking a suggestion or a history row (click or Enter on a
  //     highlighted row)
  //   - the clear (X) button
  //   - Escape with the dropdown already closed, which clears the filter
  //
  // Escape with the dropdown OPEN closes it and commits nothing — closing
  // an overlay is not a search.
  //
  // The suggest fetch keeps its own 150ms debounce. It is a read of a
  // typeahead endpoint, not a feed query, and #1156's measured bar is
  // specifically ZERO feed queries while typing.
  //
  // Phase 1.16.B-1 added the history dropdown backed by localStorage:
  //   - opens on focus with an empty input; shows the recent 10
  //     unique queries (most recent first)
  //   - keyboard-navigable (Arrow Up/Down + Enter to commit; Escape
  //     closes the dropdown)
  //   - Clear history removes the localStorage key + collapses the
  //     dropdown
  // History persistence lives on-device only in B-1; server-side
  // history is an optional B-4 add.

  import { t } from '$stores/lang.svelte';
  import type { CommitTerm } from '$lib/search/commitTarget';

  interface Props {
    value: string;
    /** Fired on COMMIT.
     *
     *  ⭐ `term` is #1077's seam. A commit used to be a string and only a
     *  string, so a picked suggestion arrived here having ALREADY thrown
     *  away the one fact that made it different from something the user
     *  typed: which DIMENSION it came from. The payload has carried
     *  `kind` since the dropdown was built and the chip beside each row
     *  renders it; `pick` took `(q: string)` and dropped it at the last
     *  step, so a picked tag was committed as free text — against a
     *  TSVECTOR that contains no tag-only word.
     *
     *  So a typed pick now hands the parent the dimension too, and the
     *  parent applies the structured filter. `term` is undefined for
     *  every other commit (typing + Enter, the clear button, Escape, a
     *  history row), which are free text and unchanged. */
    onsearch?: (q: string, term?: CommitTerm) => void;
    placeholder?: string;
    /** Which corpus the COMMIT will be executed against (#1155). The
     *  suggest endpoint completes only terms that would return a result
     *  under that surface's match rule, so a suggestion the caller picks
     *  can never land on an empty feed. The parent derives this from the
     *  same predicate that decides where a commit navigates. */
    scope?: 'browse' | 'search';
  }

  let {
    value = $bindable(''),
    onsearch,
    placeholder = t('nav.search_placeholder'),
    scope = 'browse',
  }: Props = $props();

  const HISTORY_KEY = 'search_history';
  const HISTORY_LIMIT = 10;
  const SUGGEST_DEBOUNCE_MS = 150;

  type Suggestion = { value: string; kind: string; similarity: number };

  let suggestTimer: ReturnType<typeof setTimeout> | null = null;
  let lastCommitted = value;
  let history = $state<string[]>(loadHistory());
  let dropdownOpen = $state(false);
  let highlight = $state<number>(-1);
  let inputEl = $state<HTMLInputElement | null>(null);
  // Phase 1.16.B-2 autocomplete: pg_trgm-backed suggestions from
  // /search/suggest fetched with a 150ms debounce. Merged above
  // the history list when the prefix is non-empty; history alone
  // when prefix is empty.
  let suggestions = $state<Suggestion[]>([]);
  let suggestLoading = $state(false);

  function loadHistory(): string[] {
    if (typeof localStorage === 'undefined') return [];
    try {
      const raw = localStorage.getItem(HISTORY_KEY);
      if (!raw) return [];
      const parsed = JSON.parse(raw);
      if (!Array.isArray(parsed)) return [];
      return parsed.filter((x) => typeof x === 'string').slice(0, HISTORY_LIMIT);
    } catch {
      return [];
    }
  }

  function persistHistory(next: string[]) {
    history = next;
    if (typeof localStorage !== 'undefined') {
      localStorage.setItem(HISTORY_KEY, JSON.stringify(next));
    }
  }

  function pushHistory(q: string) {
    const trimmed = q.trim();
    if (!trimmed) return;
    // De-dupe: drop any existing occurrence + prepend so the most
    // recent instance always wins.
    const filtered = history.filter((h) => h !== trimmed);
    filtered.unshift(trimmed);
    persistHistory(filtered.slice(0, HISTORY_LIMIT));
  }

  function clearHistory() {
    persistHistory([]);
    dropdownOpen = false;
    highlight = -1;
  }

  function commitNow() {
    if (value !== lastCommitted) {
      lastCommitted = value;
      onsearch?.(value);
      pushHistory(value);
    }
    dropdownOpen = false;
    highlight = -1;
  }

  // #1156 — typing schedules SUGGESTIONS and nothing else. There is no
  // commit timer here any more; the feed is re-queried only by the four
  // explicit commits listed at the top of this file.
  function onInput() {
    dropdownOpen = (value === '' && history.length > 0) || value !== '';
    highlight = -1;
    scheduleSuggest();
  }

  function scheduleSuggest() {
    if (suggestTimer) clearTimeout(suggestTimer);
    if (value === '') {
      suggestions = [];
      return;
    }
    suggestTimer = setTimeout(async () => {
      suggestLoading = true;
      try {
        const resp = await fetch(
          `/api/v1/search/suggest?prefix=${encodeURIComponent(value)}&limit=6&scope=${scope}`,
          { credentials: 'include' },
        );
        if (!resp.ok) {
          suggestions = [];
          return;
        }
        const data = await resp.json();
        suggestions = Array.isArray(data.suggestions) ? data.suggestions : [];
      } catch {
        suggestions = [];
      } finally {
        suggestLoading = false;
      }
    }, SUGGEST_DEBOUNCE_MS);
  }

  function onKey(e: KeyboardEvent) {
    if (dropdownOpen && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
      e.preventDefault();
      if (rows.length === 0) return;
      if (e.key === 'ArrowDown') {
        highlight = (highlight + 1) % rows.length;
      } else {
        highlight = highlight <= 0 ? rows.length - 1 : highlight - 1;
      }
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (dropdownOpen && highlight >= 0 && highlight < rows.length) {
        pick(rows[highlight]);
      } else {
        commitNow();
      }
    } else if (e.key === 'Escape') {
      if (dropdownOpen) {
        dropdownOpen = false;
        highlight = -1;
      } else {
        value = '';
        commitNow();
      }
    }
  }

  function onFocus() {
    if (value === '' && history.length > 0) {
      dropdownOpen = true;
      highlight = -1;
    }
  }

  function onBlur() {
    // Delay so a click on a history row registers first.
    setTimeout(() => { dropdownOpen = false; highlight = -1; }, 120);
  }

  // The dropdown's rows IN RENDER ORDER: suggestions first (only while
  // the box is non-empty, matching the markup's own guard), then history.
  //
  // `highlight` indexes THIS array, not `history` (#1156). It used to
  // index `history` while the markup rendered suggestions above it, so
  // Arrow Down onto the first visible row selected a history entry that
  // was several rows lower — harmless while typing also searched, and a
  // real defect now that the dropdown is the only way to refine.
  const rows = $derived<Array<{ value: string; kind?: string }>>([
    ...(value !== '' ? suggestions.map((s) => ({ value: s.value, kind: s.kind })) : []),
    ...history.map((h) => ({ value: h })),
  ]);

  /** The structured term a picked row carries, or undefined for a row
   *  that is just text (#1077).
   *
   *  Only `tag` maps to a dimension today. The other three kinds the
   *  endpoint emits — `collection`, `post`, `asset` — name a THING with a
   *  page of its own rather than a filter, and until that navigation
   *  exists they commit as free text exactly as they did before. That is
   *  a correct fallback rather than a gap: a post or asset TITLE is
   *  indexed into its own document, so free text finds it; a tag-only
   *  word is in no document at all, which is why it is the one that had
   *  to change. */
  function termOf(row: { value: string; kind?: string }): CommitTerm | undefined {
    return row.kind === 'tag' ? { dimension: 'tag', value: row.value } : undefined;
  }

  /** Commit a dropdown row. This is one of the four commits. */
  function pick(row: { value: string; kind?: string }) {
    const term = termOf(row);
    value = row.value;
    lastCommitted = row.value;
    onsearch?.(row.value, term);
    // ⚠️ A STRUCTURED PICK IS NOT PUSHED INTO HISTORY, and the omission
    // is the point rather than an oversight. Every history row commits
    // as FREE TEXT — that is all a `string[]` in localStorage can carry —
    // so storing a picked tag here would put the exact query #1077 is
    // about back in front of the user one keystroke later, under a
    // "Recent" heading that promises it worked before. The filter itself
    // survives in the URL, which is the shareable, back-navigable record
    // this app already uses for a result set.
    if (!term) pushHistory(row.value);
    dropdownOpen = false;
    highlight = -1;
    inputEl?.focus();
  }

  function clear() {
    value = '';
    commitNow();
  }
</script>

<div class="relative flex items-center">
  <span class="absolute left-3 pointer-events-none text-fg-muted" aria-hidden="true">
    <svg
      xmlns="http://www.w3.org/2000/svg"
      width="18"
      height="18"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
    >
      <circle cx="11" cy="11" r="7" />
      <path d="m21 21-4.3-4.3" />
    </svg>
  </span>
  <input
    bind:this={inputEl}
    type="search"
    {placeholder}
    bind:value
    oninput={onInput}
    onkeydown={onKey}
    onfocus={onFocus}
    onblur={onBlur}
    aria-label={t('common.search')}
    aria-autocomplete="list"
    aria-controls="search-history"
    aria-expanded={dropdownOpen}
    data-testid="nav-search"
    class="w-full rounded-full border border-border-strong bg-surface pl-10 pr-10 py-2 text-sm text-fg
           placeholder:text-fg-muted
           focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
  />
  {#if value}
    <button
      type="button"
      onclick={clear}
      aria-label={t('search.clear')}
      class="absolute right-3 rounded-full p-0.5 text-fg-muted hover:text-fg hover:bg-surface"
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        width="14"
        height="14"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2.5"
        stroke-linecap="round"
        stroke-linejoin="round"
      >
        <path d="M18 6 6 18" />
        <path d="m6 6 12 12" />
      </svg>
    </button>
  {/if}

  {#if dropdownOpen && (suggestions.length > 0 || history.length > 0)}
    <div
      id="search-history"
      role="listbox"
      class="absolute left-0 right-0 top-full z-10 mt-1 max-h-96 overflow-y-auto rounded-md border border-border bg-surface shadow-lg"
      data-testid="search-history"
    >
      {#if value !== '' && suggestions.length > 0}
        <div class="border-b border-border px-3 py-1.5 text-[10px] uppercase tracking-wide text-fg-muted">
          {t('search.suggestions_heading')}
        </div>
        {#each suggestions as sug, i (sug.kind + ':' + sug.value)}
          <button
            type="button"
            role="option"
            aria-selected={i === highlight}
            onmousedown={() => pick({ value: sug.value, kind: sug.kind })}
            class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
            class:bg-surface-elevated={i === highlight}
            data-testid="search-suggestion"
          >
            <span class="rounded bg-info/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-info">{sug.kind}</span>
            {sug.value}
          </button>
        {/each}
      {/if}
      {#if history.length > 0}
      <div class="flex items-center justify-between border-b border-border px-3 py-1.5 text-[10px] uppercase tracking-wide text-fg-muted">
        <span>{t('search.recent_heading')}</span>
        <button
          type="button"
          onclick={clearHistory}
          class="rounded px-1 text-fg-muted hover:text-fg hover:bg-surface-elevated"
        >{t('search.clear_history')}</button>
      </div>
      <!-- The history rows sit BELOW the suggestions, so their index into
           `rows` is offset by however many suggestions are showing. -->
      {#each history as h, i (h)}
        {@const idx = i + (value !== '' ? suggestions.length : 0)}
        <button
          type="button"
          role="option"
          aria-selected={idx === highlight}
          onmousedown={() => pick({ value: h })}
          class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
          class:bg-surface-elevated={idx === highlight}
          data-testid="search-history-item"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-fg-muted">
            <circle cx="12" cy="12" r="10" />
            <path d="M12 6v6l4 2" />
          </svg>
          {h}
        </button>
      {/each}
      {/if}
    </div>
  {/if}
</div>
