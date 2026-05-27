<script lang="ts">
  // /account/* layout — sidebar + content slot.
  //
  // Auth gate: redirect to /login if not signed in. (The +layout.svelte
  // chrome already hides itself on /login, but the route still resolves;
  // we redirect so the user lands somewhere meaningful.)

  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';

  let { children } = $props();

  onMount(() => {
    if (!auth.user) void goto('/login?next=' + encodeURIComponent(page.url.pathname));
  });

  interface NavLink {
    href: string;
    label: () => string;
  }

  const links: NavLink[] = [
    { href: '/account/profile',     label: () => t('account.sections.profile') },
    { href: '/account/preferences', label: () => t('account.sections.preferences') },
    { href: '/account/preferences/ai', label: () => t('account.sections.ai') },
    { href: '/account/password',    label: () => t('account.sections.password') },
    { href: '/account/tokens',      label: () => t('account.sections.tokens') },
    { href: '/account/messages',    label: () => t('account.sections.messages') },
  ];
</script>

<div class="mx-auto w-full max-w-6xl px-6 py-6">
  <header class="mb-6">
    <h1 class="text-2xl font-semibold">{t('account.title')}</h1>
  </header>

  <div class="grid grid-cols-1 gap-6 md:grid-cols-[14rem_1fr]">
    <nav class="space-y-1 text-sm">
      <a
        href="/account"
        class={`block rounded-md px-3 py-1.5 ${page.url.pathname === '/account' ? 'bg-surface-elevated text-fg' : 'text-fg-muted hover:bg-surface-elevated/60 hover:text-fg'}`}
      >
        Overview
      </a>
      {#each links as l (l.href)}
        {@const active = page.url.pathname.startsWith(l.href)}
        <a
          href={l.href}
          class={`block rounded-md px-3 py-1.5 ${active ? 'bg-surface-elevated text-fg' : 'text-fg-muted hover:bg-surface-elevated/60 hover:text-fg'}`}
        >
          {l.label()}
        </a>
      {/each}
    </nav>

    <section>
      {@render children?.()}
    </section>
  </div>
</div>
