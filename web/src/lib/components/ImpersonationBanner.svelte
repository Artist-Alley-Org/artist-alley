<script lang="ts">
  // Persistent "you are acting as @target" banner — Phase 1.19.A-2.
  //
  // Visible on every page whenever the resolved Identity carries
  // an `impersonated_by` block. One-click "End impersonation"
  // calls POST /auth/impersonation/end, which:
  //   1. Revokes the impersonation session server-side.
  //   2. Mints a fresh session for the original admin.
  //   3. Rotates the session cookie back to the admin.
  // After the rotation we refresh the auth store so the rest of
  // the chrome reflects the admin's identity immediately.

  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';

  let ending = $state(false);
  let error = $state<string | null>(null);

  const ib = $derived(auth.user?.impersonatedBy ?? null);

  async function endImpersonation() {
    if (ending) return;
    ending = true;
    error = null;
    try {
      const r = await api.POST('/auth/impersonation/end');
      if (r.error) {
        error = (r.error as { error?: string }).error ?? t('impersonation.end_failed');
        return;
      }
      await auth.refresh();
      void goto('/');
    } finally {
      ending = false;
    }
  }
</script>

{#if ib}
  <div
    role="status"
    aria-live="polite"
    class="border-b border-warning/40 bg-warning/15 text-warning"
    data-testid="impersonation-banner"
  >
    <div class="flex flex-wrap items-center justify-between gap-3 px-6 py-2 text-sm">
      <div>
        <strong>{t('impersonation.banner_acting_as_label')}</strong>
        <code class="mx-1 rounded bg-warning/20 px-1.5 py-0.5">@{auth.user?.username}</code>
        <span class="text-fg-muted">
          {t('impersonation.banner_started_by', { admin: '@' + ib.username })}
        </span>
      </div>
      <button
        type="button"
        onclick={endImpersonation}
        disabled={ending}
        class="rounded-md border border-warning/60 bg-warning/20 px-3 py-1 text-xs font-medium hover:bg-warning/30 disabled:opacity-50"
        data-testid="impersonation-end"
      >{ending ? t('common.loading') : t('impersonation.end_button')}</button>
    </div>
    {#if error}
      <div class="border-t border-warning/40 bg-danger-container px-6 py-1.5 text-xs text-danger">{error}</div>
    {/if}
  </div>
{/if}
