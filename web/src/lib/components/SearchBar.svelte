<script lang="ts">
  // Debounced search input. Two-way bound via $bindable so the parent
  // owns the canonical query string. Emits an `onsearch` callback when
  // the debounce settles so the parent can fire the actual fetch.
  //
  // We use a separate `committed` mirror so the parent gets one event
  // per pause rather than one per keystroke. Debounce of 250ms is fast
  // enough to feel live and slow enough to avoid hammering the API
  // on every character.

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

  let timer: ReturnType<typeof setTimeout> | null = null;
  let lastCommitted = value;

  function scheduleCommit() {
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => {
      if (value !== lastCommitted) {
        lastCommitted = value;
        onsearch?.(value);
      }
    }, debounceMs);
  }

  function commitNow() {
    if (timer) clearTimeout(timer);
    if (value !== lastCommitted) {
      lastCommitted = value;
      onsearch?.(value);
    }
  }

  function onInput() {
    scheduleCommit();
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault();
      commitNow();
    } else if (e.key === 'Escape') {
      value = '';
      commitNow();
    }
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
    type="search"
    {placeholder}
    bind:value
    oninput={onInput}
    onkeydown={onKey}
    aria-label="Search"
    class="w-full rounded-full border border-border bg-surface-elevated pl-10 pr-10 py-2 text-sm text-fg
           placeholder:text-fg-muted/60
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
</div>
