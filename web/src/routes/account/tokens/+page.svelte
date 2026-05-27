<script lang="ts">
  // Personal access tokens: list / create / revoke. Backed by the
  // existing /auth/tokens endpoints.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface TokenSummary {
    id: string;
    name: string;
    scopes: string[];
    created_at: string;
    expires_at?: string | null;
    last_used_at?: string | null;
  }

  let tokens = $state<TokenSummary[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // New-token form
  let creating = $state(false);
  let newName = $state('');
  let newScopes = $state('');
  let justCreatedToken = $state<string | null>(null);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    error = null;
    try {
      const { data, error: apiErr } = await api.GET('/auth/tokens');
      if (apiErr) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('errors.save_failed');
        return;
      }
      tokens = ((data ?? []) as TokenSummary[]).sort(
        (a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime(),
      );
    } finally {
      loading = false;
    }
  }

  async function create() {
    if (!newName.trim() || creating) return;
    creating = true;
    error = null;
    justCreatedToken = null;
    try {
      const scopes = newScopes
        .split(',')
        .map((s) => s.trim())
        .filter((s) => s.length > 0);
      const { data, error: apiErr } = await api.POST('/auth/tokens', {
        body: { name: newName.trim(), scopes },
      });
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('errors.save_failed');
        return;
      }
      justCreatedToken = (data as { token: string }).token;
      newName = '';
      newScopes = '';
      await load();
    } finally {
      creating = false;
    }
  }

  async function revoke(id: string) {
    if (!confirm(t('account.tokens.revoke') + '?')) return;
    try {
      await api.DELETE('/auth/tokens/{id}', { params: { path: { id } } });
      await load();
    } catch {
      // soft fail
    }
  }
</script>

<svelte:head><title>{t('account.tokens.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('account.tokens.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('account.tokens.intro')}</p>

<section class="mb-6 rounded-lg border border-border bg-surface-elevated p-4">
  <h3 class="mb-2 text-sm font-medium text-fg">{t('account.tokens.create')}</h3>
  <form
    onsubmit={(e) => {
      e.preventDefault();
      void create();
    }}
    class="grid grid-cols-1 gap-2 md:grid-cols-[2fr_3fr_auto]"
  >
    <input
      type="text"
      bind:value={newName}
      placeholder={t('account.tokens.name')}
      class="rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
      required
    />
    <input
      type="text"
      bind:value={newScopes}
      placeholder={t('account.tokens.scopes') + ' (comma-separated, optional)'}
      class="rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
    />
    <button
      type="submit"
      disabled={creating || !newName.trim()}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {creating ? t('common.loading') : t('account.tokens.create')}
    </button>
  </form>
  {#if justCreatedToken}
    <div class="mt-3 rounded border border-warning/40 bg-warning-container p-3 text-sm">
      <p class="font-medium text-warning">Save this token — it is never shown again:</p>
      <code class="mt-1 block break-all rounded bg-surface px-2 py-1 font-mono text-xs">{justCreatedToken}</code>
    </div>
  {/if}
  {#if error}
    <p role="alert" class="mt-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {/if}
</section>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if tokens.length === 0}
  <p class="rounded-md bg-surface-elevated px-4 py-6 text-center text-fg-muted">{t('account.tokens.no_tokens')}</p>
{:else}
  <table class="w-full text-sm">
    <thead class="text-left text-xs uppercase tracking-wider text-fg-muted">
      <tr>
        <th class="py-2">{t('account.tokens.name')}</th>
        <th class="py-2">{t('account.tokens.scopes')}</th>
        <th class="py-2">{t('account.tokens.expires_at')}</th>
        <th class="py-2">{t('account.tokens.last_used')}</th>
        <th class="py-2"></th>
      </tr>
    </thead>
    <tbody>
      {#each tokens as tk (tk.id)}
        <tr class="border-t border-border">
          <td class="py-2">{tk.name}</td>
          <td class="py-2 text-fg-muted">{tk.scopes.length ? tk.scopes.join(', ') : '—'}</td>
          <td class="py-2 text-fg-muted">{tk.expires_at ? new Date(tk.expires_at).toLocaleDateString() : '—'}</td>
          <td class="py-2 text-fg-muted">{tk.last_used_at ? new Date(tk.last_used_at).toLocaleDateString() : '—'}</td>
          <td class="py-2 text-right">
            <button
              type="button"
              onclick={() => void revoke(tk.id)}
              class="rounded-md border border-danger/40 px-3 py-1 text-xs text-danger hover:bg-danger-container"
            >
              {t('account.tokens.revoke')}
            </button>
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
{/if}
