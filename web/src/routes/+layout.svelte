<script lang="ts">
  import '../app.css';
  import { onMount } from 'svelte';
  import { theme } from '$stores/theme.svelte';
  import { auth } from '$stores/auth.svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import SearchBar from '$components/SearchBar.svelte';

  let { children } = $props();

  onMount(() => {
    theme.init();
  });

  // Pages that show the global nav chrome. Login/setup keep the bare
  // layout; everything else under the auth gate gets the header.
  const showChrome = $derived(
    !!auth.user && page.url.pathname !== '/login' && page.url.pathname !== '/setup'
  );

  // Pages that get the search box rendered into the nav. Currently
  // only the browse page (/) — when other browsable surfaces land
  // (collection detail, user gallery, etc.) we'll widen this gate.
  const showSearch = $derived(showChrome && page.url.pathname === '/');

  // Search query is the source-of-truth in the URL so a refresh or
  // shared link reproduces the same result set.
  const urlQuery = $derived(page.url.searchParams.get('q') ?? '');
  let searchValue = $state('');

  // Keep the local input mirror in sync with the URL whenever the
  // location changes (e.g. back/forward, programmatic navigation).
  $effect(() => {
    searchValue = urlQuery;
  });

  async function handleSearch(q: string) {
    const trimmed = q.trim();
    const target = new URL(page.url);
    if (trimmed === '') {
      target.searchParams.delete('q');
    } else {
      target.searchParams.set('q', trimmed);
    }
    // keepFocus avoids stealing focus from the still-typing input.
    await goto(target.pathname + target.search, { keepFocus: true, noScroll: true });
  }

  async function handleSignOut() {
    await auth.logout();
    await goto('/login');
  }

  function cycleTheme() {
    const next = theme.pref === 'light' ? 'dark' : theme.pref === 'dark' ? 'system' : 'light';
    theme.set(next);
  }
</script>

<div class="min-h-screen flex flex-col bg-surface text-fg">
  {#if showChrome}
    <header class="border-b border-border bg-surface-elevated">
      <div class="flex items-center gap-4 px-6 py-3">
        <a href="/" class="flex items-center gap-2 font-semibold tracking-tight shrink-0">
          <span class="inline-block h-6 w-6 rounded bg-accent"></span>
          artist-alley
        </a>

        {#if showSearch}
          <div class="flex-1 max-w-2xl mx-auto">
            <SearchBar
              bind:value={searchValue}
              onsearch={handleSearch}
              placeholder="Search assets…"
            />
          </div>
        {:else}
          <div class="flex-1"></div>
        {/if}

        <div class="flex items-center gap-2 shrink-0">
          <button
            type="button"
            onclick={cycleTheme}
            class="rounded-md px-2.5 py-1.5 text-xs text-fg-muted hover:text-fg hover:bg-surface transition-colors"
            title="Theme: {theme.pref}"
            aria-label="Cycle theme"
          >
            {theme.pref === 'light' ? '☀' : theme.pref === 'dark' ? '☾' : '◐'}
            <span class="ml-1 capitalize">{theme.pref}</span>
          </button>
          <div class="text-sm text-fg-muted">
            {auth.user?.fullname || auth.user?.username}
          </div>
          <button
            type="button"
            onclick={handleSignOut}
            class="rounded-md px-2.5 py-1.5 text-xs text-fg-muted hover:text-fg hover:bg-surface transition-colors"
          >
            Sign out
          </button>
        </div>
      </div>
    </header>
  {/if}

  <main class="flex-1 flex flex-col">
    {@render children?.()}
  </main>
</div>
