<script lang="ts">
  // /register — Phase 1.19.C self-service signup.
  //
  // Two terminal states:
  //   1. 202 → server queued a verification email. We render a
  //      "check your inbox" panel + a resend button.
  //   2. 200 → server signed the user in directly (admin has
  //      verification turned off). We bounce to the home page.
  //
  // Server-side authoritative validation; the regexes here are
  // UX only so the form gives early feedback. The 403 branch
  // (registration disabled) is rendered as a static panel so
  // a deep link doesn't dead-end on a closed install.

  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import Button from '$components/Button.svelte';
  import TextField from '$components/TextField.svelte';
  import Alert from '$components/Alert.svelte';

  let username = $state('');
  let email = $state('');
  let password = $state('');
  let fullname = $state('');
  let submitting = $state(false);
  let error = $state<string | null>(null);
  let disabledByOperator = $state(false);

  // Post-submit state: server returned 202 → show check-your-
  // inbox panel + resend button.
  let pending = $state<{ user_ref: number; email: string } | null>(null);
  let resending = $state(false);
  let resendError = $state<string | null>(null);
  let resentMessage = $state<string | null>(null);

  async function onSubmit(e: SubmitEvent) {
    e.preventDefault();
    if (submitting) return;
    error = null;
    submitting = true;
    try {
      const r = await api.POST('/auth/register', {
        body: {
          username: username.trim(),
          email: email.trim(),
          password,
          fullname: fullname.trim() || undefined,
        } as never,
      });
      if (r.error) {
        const code = (r.error as { error?: string }).error;
        if (r.response.status === 403) {
          disabledByOperator = true;
          return;
        }
        if (r.response.status === 409) {
          error = t('register.error_duplicate');
          return;
        }
        if (r.response.status === 429) {
          error = t('register.error_rate_limit');
          return;
        }
        error = code ?? t('register.error_generic');
        return;
      }
      // 200 path — verification disabled. The cookie has been
      // set; refresh the auth store + go home.
      if (r.response.status === 200) {
        await auth.refresh();
        await goto('/');
        return;
      }
      // 202 path — pending verification.
      const d = r.data as { user_ref: number; email: string; message?: string };
      pending = { user_ref: d.user_ref, email: d.email };
    } finally {
      submitting = false;
    }
  }

  async function resend() {
    if (!pending || resending) return;
    resending = true;
    resendError = null;
    resentMessage = null;
    try {
      const r = await api.POST('/auth/resend-verification', {
        body: { email: pending.email } as never,
      });
      if (r.error) {
        resendError = (r.error as { error?: string }).error ?? t('register.error_generic');
        return;
      }
      resentMessage = t('register.resend_sent');
    } finally {
      resending = false;
    }
  }
</script>

<svelte:head><title>{t('register.title')} — artist-alley</title></svelte:head>

<div class="relative flex-1 flex items-center justify-center px-6 py-12 isolate">
  <div
    class="absolute inset-0 -z-10 bg-cover bg-center bg-no-repeat"
    style="background-image: url('/hero.png');"
    aria-hidden="true"
  ></div>
  <div class="absolute inset-0 -z-10 bg-black/55 backdrop-blur-[2px]" aria-hidden="true"></div>

  <div class="w-full max-w-sm space-y-6 rounded-xl border border-white/10 bg-surface/85 p-8 shadow-2xl backdrop-blur-md">
    <div class="text-center space-y-2">
      <img src="/logo.svg" alt="" class="mx-auto h-16 w-16" aria-hidden="true" />
      <h1 class="text-2xl font-semibold tracking-tight">{t('register.title')}</h1>
      <p class="text-sm text-fg-muted">artist-alley</p>
    </div>

    {#if disabledByOperator}
      <Alert tone="info">{t('register.disabled_message')}</Alert>
      <p class="text-center text-sm">
        <a href="/login" class="text-accent hover:underline">{t('register.back_to_login')}</a>
      </p>
    {:else if pending}
      <Alert tone="success">
        <p class="font-semibold">{t('register.pending_heading')}</p>
        <p class="mt-1 text-xs">{t('register.pending_body', { email: pending.email })}</p>
      </Alert>
      <div class="space-y-2">
        <Button type="button" variant="secondary" fullWidth loading={resending} onclick={resend}>
          {resending ? t('common.loading') : t('register.resend_button')}
        </Button>
        {#if resendError}
          <Alert tone="error">{resendError}</Alert>
        {/if}
        {#if resentMessage}
          <Alert tone="info">{resentMessage}</Alert>
        {/if}
      </div>
      <p class="text-center text-sm">
        <a href="/login" class="text-accent hover:underline">{t('register.back_to_login')}</a>
      </p>
    {:else}
      <form class="space-y-4" onsubmit={onSubmit}>
        {#if error}
          <Alert tone="error">{error}</Alert>
        {/if}

        <TextField
          label={t('register.username_label')}
          name="username"
          autocomplete="username"
          required
          autofocus
          bind:value={username}
          disabled={submitting}
          testId="register-username"
        />
        <TextField
          label={t('register.email_label')}
          name="email"
          type="email"
          autocomplete="email"
          required
          bind:value={email}
          disabled={submitting}
          testId="register-email"
        />
        <TextField
          label={t('register.fullname_label')}
          name="fullname"
          autocomplete="name"
          bind:value={fullname}
          disabled={submitting}
          testId="register-fullname"
        />
        <TextField
          label={t('register.password_label')}
          name="password"
          type="password"
          autocomplete="new-password"
          required
          bind:value={password}
          disabled={submitting}
          testId="register-password"
        />

        <Button type="submit" variant="primary" fullWidth loading={submitting} testId="register-submit">
          {t('register.submit')}
        </Button>
      </form>
      <p class="text-center text-sm">
        {t('register.have_account')}
        <a href="/login" class="text-accent hover:underline">{t('register.sign_in_link')}</a>
      </p>
    {/if}
  </div>
</div>
