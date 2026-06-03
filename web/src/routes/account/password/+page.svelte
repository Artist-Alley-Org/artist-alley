<script lang="ts">
  // /account/password — self-service password change (Phase 1.17.D).
  //
  // Three inputs (current / new / confirm) + an opt-in "sign out
  // everywhere else" checkbox (default on per security best-practice).
  // The server applies the complexity policy + history check; we
  // surface its error message verbatim.

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  let current = $state('');
  let next = $state('');
  let confirm = $state('');
  let revokeOthers = $state(true);
  let submitting = $state(false);
  let error = $state<string | null>(null);
  let success = $state<string | null>(null);

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    if (submitting) return;
    error = null;
    success = null;

    if (next !== confirm) {
      error = t('account.password.mismatch');
      return;
    }

    submitting = true;
    try {
      const r = await api.PUT('/account/password', {
        body: {
          current_password: current,
          new_password: next,
          revoke_other_sessions: revokeOthers,
        },
      });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed.';
        return;
      }
      const result = r.data as unknown as { sessions_revoked?: number };
      const revoked = result.sessions_revoked ?? 0;
      success = revoked > 0
        ? t('account.password.success_with_revokes', { count: revoked })
        : t('account.password.success');
      current = '';
      next = '';
      confirm = '';
    } finally {
      submitting = false;
    }
  }
</script>

<svelte:head><title>{t('account.password.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('account.password.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('account.password.intro')}</p>

<form onsubmit={submit} class="max-w-xl space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
  <label class="block text-sm">
    <span class="mb-1 block text-fg-muted">{t('account.password.current_label')}</span>
    <input
      type="password"
      autocomplete="current-password"
      bind:value={current}
      required
      class="w-full rounded border border-border bg-surface px-2 py-1.5 text-sm focus:border-accent focus:outline-none"
    />
  </label>

  <label class="block text-sm">
    <span class="mb-1 block text-fg-muted">{t('account.password.new_label')}</span>
    <input
      type="password"
      autocomplete="new-password"
      bind:value={next}
      required
      class="w-full rounded border border-border bg-surface px-2 py-1.5 text-sm focus:border-accent focus:outline-none"
    />
  </label>

  <label class="block text-sm">
    <span class="mb-1 block text-fg-muted">{t('account.password.confirm_label')}</span>
    <input
      type="password"
      autocomplete="new-password"
      bind:value={confirm}
      required
      class="w-full rounded border border-border bg-surface px-2 py-1.5 text-sm focus:border-accent focus:outline-none"
    />
  </label>

  <label class="flex items-center gap-2 text-sm text-fg-muted">
    <input
      type="checkbox"
      bind:checked={revokeOthers}
      class="h-4 w-4 accent-accent"
    />
    <span>{t('account.password.revoke_others_label')}</span>
  </label>

  <button
    type="submit"
    disabled={submitting}
    class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
  >
    {submitting ? t('account.password.submitting') : t('account.password.submit')}
  </button>

  {#if error}
    <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {/if}
  {#if success}
    <p class="rounded border border-success/40 bg-success/10 px-3 py-2 text-sm text-success">{success}</p>
  {/if}
</form>
