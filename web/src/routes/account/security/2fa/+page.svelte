<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/security/2fa — Phase 1.19.B self-service TOTP page.
  //
  // Three view states:
  //
  //   1. NOT enrolled → show "Enable 2FA" button.
  //   2. Enrolled but NOT confirmed → show the secret + otpauth URL
  //      and the "enter code to confirm" form.
  //   3. Confirmed → show status (enrolled date, last used, codes
  //      remaining) + disable + regenerate-recovery actions.
  //
  // Recovery codes returned by confirm/regenerate are shown ONCE
  // (the server only stores sha256(code)). The page surfaces them
  // in a dismissable panel with copy-to-clipboard.
  //
  // No QR-code rendering yet — the page shows the otpauth:// URL
  // (deep-links on mobile to launch the authenticator app) and the
  // base32 secret for manual paste. QR rendering can land as a
  // follow-up alongside a chosen JS QR lib.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  type Status = {
    enrolled: boolean;
    confirmed: boolean;
    enrolled_at?: string | null;
    confirmed_at?: string | null;
    last_used_at?: string | null;
    recovery_codes_remaining: number;
  };

  let status = $state<Status | null>(null);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Enroll/confirm state.
  let enrollSecret = $state<string | null>(null);
  let otpauthUrl = $state<string | null>(null);
  let confirmCode = $state('');
  let confirming = $state(false);
  let confirmError = $state<string | null>(null);
  let recoveryCodes = $state<string[] | null>(null);

  // Disable/regenerate state.
  let disablePassword = $state('');
  let disabling = $state(false);
  let disableError = $state<string | null>(null);
  let regenPassword = $state('');
  let regenerating = $state(false);
  let regenError = $state<string | null>(null);

  async function loadStatus() {
    loading = true;
    try {
      const r = await api.GET('/account/security/2fa');
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('account.twofa.load_failed');
        return;
      }
      status = r.data as Status;
    } finally {
      loading = false;
    }
  }

  async function startEnroll() {
    error = null;
    const r = await api.POST('/account/security/2fa');
    if (r.error || !r.data) {
      error = (r.error as { error?: string } | undefined)?.error ?? t('account.twofa.enroll_failed');
      return;
    }
    const d = r.data as { secret_base32: string; otpauth_url: string };
    enrollSecret = d.secret_base32;
    otpauthUrl = d.otpauth_url;
    await loadStatus();
  }

  async function confirmEnroll(e: Event) {
    e.preventDefault();
    if (confirming) return;
    confirming = true;
    confirmError = null;
    try {
      const r = await api.POST('/account/security/2fa/confirm', {
        body: { code: confirmCode } as never,
      });
      if (r.error || !r.data) {
        confirmError = (r.error as { error?: string } | undefined)?.error ?? t('account.twofa.confirm_failed');
        return;
      }
      const d = r.data as { recovery_codes: string[] };
      recoveryCodes = d.recovery_codes;
      enrollSecret = null;
      otpauthUrl = null;
      confirmCode = '';
      await loadStatus();
    } finally {
      confirming = false;
    }
  }

  async function disable(e: Event) {
    e.preventDefault();
    if (disabling) return;
    if (!confirm(t('account.twofa.disable_confirm'))) return;
    disabling = true;
    disableError = null;
    try {
      const r = await api.DELETE('/account/security/2fa', {
        body: { current_password: disablePassword } as never,
      });
      if (r.error) {
        disableError = (r.error as { error?: string }).error ?? t('account.twofa.disable_failed');
        return;
      }
      disablePassword = '';
      recoveryCodes = null;
      await loadStatus();
    } finally {
      disabling = false;
    }
  }

  async function regenerate(e: Event) {
    e.preventDefault();
    if (regenerating) return;
    regenerating = true;
    regenError = null;
    try {
      const r = await api.POST('/account/security/2fa/recovery/regenerate', {
        body: { current_password: regenPassword } as never,
      });
      if (r.error || !r.data) {
        regenError = (r.error as { error?: string } | undefined)?.error ?? t('account.twofa.regen_failed');
        return;
      }
      recoveryCodes = (r.data as { recovery_codes: string[] }).recovery_codes;
      regenPassword = '';
      await loadStatus();
    } finally {
      regenerating = false;
    }
  }

  async function copyCodes() {
    if (!recoveryCodes) return;
    try {
      await navigator.clipboard.writeText(recoveryCodes.join('\n'));
    } catch {
      // Clipboard blocked (insecure context, etc.) — codes remain
      // visible in the panel for manual copy.
    }
  }

  function fmtDate(s?: string | null): string {
    if (!s) return '—';
    return new Date(s).toLocaleString();
  }

  onMount(() => { void loadStatus(); });
</script>

<svelte:head><title>{t('account.twofa.title')} — {site.name}</title></svelte:head>

<h2 class="mb-4 text-xl font-semibold">{t('account.twofa.title')}</h2>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
{:else if status}

  <!-- Confirmed: management view. -->
  {#if status.confirmed}
    <section class="mb-6 max-w-xl rounded border border-success/40 bg-success/5 p-4">
      <h3 class="text-sm font-semibold text-success">{t('account.twofa.confirmed_heading')}</h3>
      <dl class="mt-3 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
        <dt class="text-fg-muted">{t('account.twofa.enrolled_at')}</dt>
        <dd>{fmtDate(status.enrolled_at)}</dd>
        <dt class="text-fg-muted">{t('account.twofa.last_used_at')}</dt>
        <dd>{fmtDate(status.last_used_at)}</dd>
        <dt class="text-fg-muted">{t('account.twofa.recovery_remaining')}</dt>
        <dd class={status.recovery_codes_remaining < 3 ? 'text-warning' : ''}>{status.recovery_codes_remaining}</dd>
      </dl>
    </section>

    <section class="mb-6 max-w-xl rounded border border-border bg-surface-elevated p-4">
      <h3 class="text-sm font-semibold">{t('account.twofa.regen_heading')}</h3>
      <p class="mt-1 text-xs text-fg-muted">{t('account.twofa.regen_help')}</p>
      <form onsubmit={regenerate} class="mt-3 space-y-2">
        <label class="block text-xs text-fg-muted">{t('account.twofa.current_password')}
          <input
            type="password"
            bind:value={regenPassword}
            required
            autocomplete="current-password"
            class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 text-sm focus:border-accent focus:outline-none"
          />
        </label>
        <button
          type="submit"
          disabled={regenerating}
          class="rounded border border-accent bg-accent/10 px-3 py-1 text-xs font-medium text-accent hover:bg-accent/20 disabled:opacity-50"
        >{regenerating ? t('common.loading') : t('account.twofa.regen_button')}</button>
        {#if regenError}
          <p role="alert" class="text-xs text-danger">{regenError}</p>
        {/if}
      </form>
    </section>

    <section class="mb-6 max-w-xl rounded border border-danger/40 bg-danger/5 p-4">
      <h3 class="text-sm font-semibold text-danger">{t('account.twofa.disable_heading')}</h3>
      <p class="mt-1 text-xs text-fg-muted">{t('account.twofa.disable_help')}</p>
      <form onsubmit={disable} class="mt-3 space-y-2">
        <label class="block text-xs text-fg-muted">{t('account.twofa.current_password')}
          <input
            type="password"
            bind:value={disablePassword}
            required
            autocomplete="current-password"
            class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 text-sm focus:border-accent focus:outline-none"
          />
        </label>
        <button
          type="submit"
          disabled={disabling}
          class="rounded border border-danger bg-danger/10 px-3 py-1 text-xs font-medium text-danger hover:bg-danger/20 disabled:opacity-50"
        >{disabling ? t('common.loading') : t('account.twofa.disable_button')}</button>
        {#if disableError}
          <p role="alert" class="text-xs text-danger">{disableError}</p>
        {/if}
      </form>
    </section>

  <!-- Enrolled but not confirmed: continue-enroll flow. -->
  {:else if status.enrolled || enrollSecret}
    <section class="mb-6 max-w-xl rounded border border-warning/40 bg-warning/5 p-4">
      <h3 class="text-sm font-semibold">{t('account.twofa.confirm_heading')}</h3>
      <p class="mt-1 text-xs text-fg-muted">{t('account.twofa.confirm_help')}</p>

      {#if otpauthUrl}
        <div class="mt-3">
          <p class="text-xs text-fg-muted">{t('account.twofa.otpauth_url_label')}</p>
          <a
            href={otpauthUrl}
            class="block break-all rounded bg-surface px-2 py-1 font-mono text-xs hover:bg-state-hover"
            title="otpauth:// link — opens in your authenticator app on mobile"
          >{otpauthUrl}</a>
        </div>
      {/if}
      {#if enrollSecret}
        <div class="mt-3">
          <p class="text-xs text-fg-muted">{t('account.twofa.secret_label')}</p>
          <code class="block break-all rounded bg-surface px-2 py-1 font-mono text-sm">{enrollSecret}</code>
        </div>
      {/if}
      {#if !enrollSecret && !otpauthUrl}
        <p class="mt-3 text-xs text-fg-muted">{t('account.twofa.enroll_in_progress_help')}</p>
        <button
          type="button"
          onclick={startEnroll}
          class="mt-2 rounded border border-border bg-surface px-3 py-1 text-xs hover:bg-state-hover"
        >{t('account.twofa.restart_enroll')}</button>
      {/if}

      <form onsubmit={confirmEnroll} class="mt-4 space-y-2">
        <label class="block text-xs text-fg-muted">{t('account.twofa.code_label')}
          <input
            type="text"
            bind:value={confirmCode}
            inputmode="numeric"
            pattern="[0-9]{6}"
            minlength="6"
            maxlength="6"
            required
            autocomplete="one-time-code"
            class="mt-1 w-32 rounded border border-border bg-surface px-2 py-1 text-center font-mono text-lg tracking-widest focus:border-accent focus:outline-none"
          />
        </label>
        <button
          type="submit"
          disabled={confirming}
          class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50"
        >{confirming ? t('common.loading') : t('account.twofa.confirm_button')}</button>
        {#if confirmError}
          <p role="alert" class="text-xs text-danger">{confirmError}</p>
        {/if}
      </form>
    </section>

  <!-- Not enrolled at all. -->
  {:else}
    <section class="mb-6 max-w-xl rounded border border-border bg-surface-elevated p-4">
      <h3 class="text-sm font-semibold">{t('account.twofa.enable_heading')}</h3>
      <p class="mt-1 text-xs text-fg-muted">{t('account.twofa.enable_help')}</p>
      <button
        type="button"
        onclick={startEnroll}
        class="mt-3 rounded bg-accent px-3 py-1.5 text-sm font-medium text-white"
      >{t('account.twofa.enable_button')}</button>
    </section>
  {/if}

{/if}

<!-- Recovery codes panel — shown after confirm OR regenerate. -->
{#if recoveryCodes}
  <section class="mb-6 max-w-xl rounded border border-warning/40 bg-warning/10 p-4">
    <h3 class="text-sm font-semibold text-warning">{t('account.twofa.recovery_heading')}</h3>
    <p class="mt-1 text-xs text-fg-muted">{t('account.twofa.recovery_help')}</p>
    <ul class="mt-3 grid grid-cols-2 gap-1 font-mono text-sm">
      {#each recoveryCodes as code}
        <li class="rounded bg-surface px-2 py-1">{code}</li>
      {/each}
    </ul>
    <div class="mt-3 flex gap-2">
      <button
        type="button"
        onclick={copyCodes}
        class="rounded border border-border bg-surface px-3 py-1 text-xs hover:bg-state-hover"
      >{t('account.twofa.copy_codes')}</button>
      <button
        type="button"
        onclick={() => (recoveryCodes = null)}
        class="rounded border border-border bg-surface px-3 py-1 text-xs hover:bg-state-hover"
      >{t('account.twofa.dismiss_codes')}</button>
    </div>
  </section>
{/if}
