<script lang="ts">
  // Debounced search input. Two-way bound via $bindable so the parent
  // owns the canonical query string. Emits an `onsearch` callback when
  // the debounce settles so the parent can fire the actual fetch.
  //
  // Phase 1.16.B-1 adds a history dropdown backed by localStorage:
  //   - opens on focus with an empty input; shows the recent 10
  //     unique queries (most recent first)
  //   - keyboard-navigable (Arrow Up/Down + Enter to commit; Escape
  //     closes the dropdown)
  //   - Clear history removes the localStorage key + collapses the
  //     dropdown
  // History persistence lives on-device only in B-1; server-side
  // history is an optional B-4 add.

  interface Props {
    value: string;
    onsearch?: (q: string) => void;
    placeholder?: string;
    debounceMs?: number;
  }

  let {
    value = $bindable(''),
    onsearch,
    placeholder = 'Search',
    debounceMs = 250,
  }: Props = $props();

  const HISTORY_KEY = 'search_history';
  const HISTORY_LIMIT = 10;
  const SUGGEST_DEBOUNCE_MS = 150;

  type Suggestion = { value: string; kind: string; similarity: number };

  let timer: ReturnType<typeof setTimeout> | null = null;
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

  function scheduleCommit() {
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => {
      if (value !== lastCommitted) {
        lastCommitted = value;
        onsearch?.(value);
        pushHistory(value);
      }
    }, debounceMs);
  }

  function commitNow() {
    if (timer) clearTimeout(timer);
    if (value !== lastCommitted) {
      lastCommitted = value;
      onsearch?.(value);
      pushHistory(value);
    }
    dropdownOpen = false;
    highlight = -1;
  }

  function onInput() {
    dropdownOpen = (value === '' && history.length > 0) || value !== '';
    scheduleCommit();
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
        const resp = await fetch(`/api/v1/search/suggest?prefix=${encodeURIComponent(value)}&limit=6`, {
          credentials: 'include',
        });
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
      if (history.length === 0) return;
      if (e.key === 'ArrowDown') {
        highlight = (highlight + 1) % history.length;
      } else {
        highlight = highlight <= 0 ? history.length - 1 : highlight - 1;
      }
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (dropdownOpen && highlight >= 0 && highlight < history.length) {
        pickHistory(history[highlight]);
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

  function pickHistory(q: string) {
    value = q;
    lastCommitted = q;
    onsearch?.(q);
    pushHistory(q);
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
    aria-label="Search"
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
      aria-label="Clear search"
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
          Suggestions
        </div>
        {#each suggestions as sug (sug.kind + ':' + sug.value)}
          <button
            type="button"
            role="option"
            aria-selected="false"
            onmousedown={() => pickHistory(sug.value)}
            class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
            data-testid="search-suggestion"
          >
            <span class="rounded bg-info/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-info">{sug.kind}</span>
            {sug.value}
          </button>
        {/each}
      {/if}
      {#if history.length > 0}
      <div class="flex items-center justify-between border-b border-border px-3 py-1.5 text-[10px] uppercase tracking-wide text-fg-muted">
        <span>Recent searches</span>
        <button
          type="button"
          onclick={clearHistory}
          class="rounded px-1 text-fg-muted hover:text-fg hover:bg-surface-elevated"
        >Clear</button>
      </div>
      {#each history as h, i (h)}
        <button
          type="button"
          role="option"
          aria-selected={i === highlight}
          onmousedown={() => pickHistory(h)}
          class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
          class:bg-surface-elevated={i === highlight}
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
