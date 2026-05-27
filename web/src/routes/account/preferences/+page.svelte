<script lang="ts">
  import { theme } from '$stores/theme.svelte';
  import { lang, t } from '$stores/lang.svelte';

  let saved = $state(false);

  function pickTheme(p: 'light' | 'dark' | 'system') {
    theme.set(p);
    saved = true;
    setTimeout(() => (saved = false), 2000);
  }

  async function pickLocale(code: string) {
    await lang.set(code);
    saved = true;
    setTimeout(() => (saved = false), 2000);
  }
</script>

<svelte:head><title>{t('account.preferences.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('account.preferences.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('account.preferences.intro')}</p>

<div class="max-w-xl space-y-6">
  <section>
    <h3 class="mb-2 text-sm font-medium text-fg">{t('account.preferences.theme_label')}</h3>
    <div class="flex flex-wrap gap-2">
      {#each [
        { code: 'system' as const, label: t('user_menu.theme_system') },
        { code: 'light' as const,  label: t('user_menu.theme_light') },
        { code: 'dark' as const,   label: t('user_menu.theme_dark') },
      ] as opt (opt.code)}
        <button
          type="button"
          onclick={() => pickTheme(opt.code)}
          class={`rounded-md border px-3 py-1.5 text-sm ${theme.pref === opt.code ? 'border-accent bg-accent/10 text-accent' : 'border-border bg-surface text-fg-muted hover:text-fg'}`}
          aria-pressed={theme.pref === opt.code}
        >
          {opt.label}
        </button>
      {/each}
    </div>
    <p class="mt-1 text-xs text-fg-muted">{t('account.preferences.theme_system_help')}</p>
  </section>

  <section>
    <h3 class="mb-2 text-sm font-medium text-fg">{t('account.preferences.language_label')}</h3>
    <div class="flex flex-wrap gap-2">
      <button
        type="button"
        onclick={() => pickLocale('')}
        class={`rounded-md border px-3 py-1.5 text-sm ${lang.pref === '' ? 'border-accent bg-accent/10 text-accent' : 'border-border bg-surface text-fg-muted hover:text-fg'}`}
        aria-pressed={lang.pref === ''}
      >
        {t('user_menu.theme_system')}
      </button>
      {#each lang.locales as l (l.code)}
        <button
          type="button"
          onclick={() => pickLocale(l.code)}
          class={`rounded-md border px-3 py-1.5 text-sm ${lang.pref === l.code ? 'border-accent bg-accent/10 text-accent' : 'border-border bg-surface text-fg-muted hover:text-fg'}`}
          aria-pressed={lang.pref === l.code}
        >
          {l.nativeName}
          {#if l.completionPct < 100}
            <span class="text-xs text-fg-muted">({l.completionPct}%)</span>
          {/if}
        </button>
      {/each}
    </div>
    <p class="mt-1 text-xs text-fg-muted">{t('account.preferences.language_system_help')}</p>
  </section>

  {#if saved}
    <p class="rounded border border-emerald-500/40 bg-emerald-500/10 px-3 py-2 text-sm text-emerald-700">
      {t('account.preferences.saved')}
    </p>
  {/if}
</div>
