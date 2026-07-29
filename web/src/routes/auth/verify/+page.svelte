<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /auth/verify — Phase 1.19.C email-link landing page.
  //
  // Reads the `token` query param + POSTs to the verify
  // endpoint. Three terminal states:
  //   1. success → shows confirmation + a "sign in now" link.
  //   2. expired/used/malformed → shows the failure + a resend
  //      form (asks for the email so we can mint a new token).
  //   3. no token in URL → same as failure, prompts for email.

  import { onMount } from 'svelte';
  import BrandMark from '$lib/components/BrandMark.svelte';
  import { site } from '$stores/site.svelte';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import Button from '$components/Button.svelte';
  import TextField from '$components/TextField.svelte';
  import Alert from '$components/Alert.svelte';

  let working = $state(true);
  let success = $state(false);
  let failure = $state<string | null>(null);
  let verifiedUsername = $state('');

  // Resend form (visible on failure).
  let resendEmail = $state('');
  let resending = $state(false);
  let resendError = $state<string | null>(null);
  let resentMessage = $state<string | null>(null);

  onMount(async () => {
    const token = page.url.searchParams.get('token');
    if (!token) {
      working = false;
      failure = t('verify.no_token');
      return;
    }
    try {
      const r = await api.POST('/auth/verify-email', { body: { token } as never });
      if (r.error || !r.data) {
        failure = (r.error as { error?: string } | undefined)?.error ?? t('verify.failure_generic');
        return;
      }
      const d = r.data as { username?: string };
      verifiedUsername = d.username ?? '';
      success = true;
    } finally {
      working = false;
    }
  });

  async function resend(e: SubmitEvent) {
    e.preventDefault();
    if (resending) return;
    resending = true;
    resendError = null;
    resentMessage = null;
    try {
      const r = await api.POST('/auth/resend-verification', {
        body: { email: resendEmail.trim() } as never,
      });
      if (r.error) {
        resendError = (r.error as { error?: string }).error ?? t('verify.failure_generic');
        return;
      }
      resentMessage = t('verify.resend_sent');
    } finally {
      resending = false;
    }
  }
</script>

<svelte:head><title>{t('verify.title')} — {site.name}</title></svelte:head>

<div class="relative flex-1 flex items-center justify-center px-6 py-12 isolate">
  <div
    class="absolute inset-0 -z-10 bg-cover bg-center bg-no-repeat"
    style="background-image: url('/hero.png');"
    aria-hidden="true"
  ></div>
  <div class="absolute inset-0 -z-10 bg-black/55 backdrop-blur-[2px]" aria-hidden="true"></div>

  <div class="w-full max-w-sm space-y-6 rounded-xl border border-white/10 bg-surface/85 p-8 shadow-2xl backdrop-blur-md">
    <div class="text-center space-y-2">
      <BrandMark class="mx-auto h-16 w-16" />
      <h1 class="text-2xl font-semibold tracking-tight">{t('verify.title')}</h1>
    </div>

    {#if working}
      <p class="text-center text-sm text-fg-muted">{t('common.loading')}</p>
    {:else if success}
      <Alert tone="success">
        <p class="font-semibold">{t('verify.success_heading')}</p>
        <p class="mt-1 text-xs">{t('verify.success_body', { username: verifiedUsername })}</p>
      </Alert>
      <a
        href="/login"
        class="block w-full rounded-md bg-accent px-3 py-2 text-center text-sm font-medium text-on-accent hover:bg-accent/90"
      >{t('verify.sign_in_now')}</a>
    {:else}
      <Alert tone="error">
        <p class="font-semibold">{t('verify.failure_heading')}</p>
        <p class="mt-1 text-xs">{failure}</p>
      </Alert>
      <form class="space-y-3" onsubmit={resend}>
        <p class="text-xs text-fg-muted">{t('verify.resend_help')}</p>
        <TextField
          label={t('verify.resend_email_label')}
          name="resend_email"
          type="email"
          autocomplete="email"
          required
          bind:value={resendEmail}
          disabled={resending}
          testId="verify-resend-email"
        />
        <Button type="submit" variant="secondary" fullWidth loading={resending}>
          {resending ? t('common.loading') : t('verify.resend_button')}
        </Button>
        {#if resendError}<Alert tone="error">{resendError}</Alert>{/if}
        {#if resentMessage}<Alert tone="info">{resentMessage}</Alert>{/if}
      </form>
      <p class="text-center text-sm">
        <a href="/login" class="text-accent hover:underline">{t('verify.back_to_login')}</a>
      </p>
    {/if}
  </div>
</div>
