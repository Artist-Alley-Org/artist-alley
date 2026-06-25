<script lang="ts">
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { auth, LoginNeedsTOTPError } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import Button from '$components/Button.svelte';
  import TextField from '$components/TextField.svelte';
  import Alert from '$components/Alert.svelte';

  // Provider list fetched from GET /auth/providers. Anonymous endpoint,
  // safe to call before sign-in. Determines which buttons + forms the
  // login screen renders. The built-in password row is always present;
  // enterprise providers (LDAP, SAML, ...) appear iff the install
  // licenses them — there's no separate "is SSO enabled?" knob.
  interface ProviderSummary {
    name: string;
    display_name: string;
    kind: 'password' | 'ldap' | 'saml';
    supports_password: boolean;
  }

  let providers = $state<ProviderSummary[]>([]);
  let selectedProvider = $state('password');
  let username = $state('');
  let password = $state('');
  let submitting = $state(false);
  let error = $state<string | null>(null);

  // Phase 1.19.B — 2FA second step. needTOTP flips to true after a
  // 2fa_required response; the form replaces username+password with
  // a TOTP-code input + retries the original credentials.
  let needTOTP = $state(false);
  let totpCode = $state('');

  onMount(() => {
    void loadProviders();
  });

  async function loadProviders(): Promise<void> {
    try {
      const r = await api.GET('/auth/providers');
      if (r.data?.providers) {
        providers = r.data.providers as ProviderSummary[];
        // Default selection: password if available, else the first
        // password-supporting provider (e.g. LDAP-only installs).
        const pw = providers.find((p) => p.name === 'password');
        const firstPw = providers.find((p) => p.supports_password);
        selectedProvider = pw?.name ?? firstPw?.name ?? providers[0]?.name ?? 'password';
      }
    } catch {
      // Fail open: at least show the password form. Empty providers
      // array → the {#if} render path falls back to password-only.
      providers = [{ name: 'password', display_name: 'Password', kind: 'password', supports_password: true }];
    }
  }

  // Providers that take username + password POST.
  const passwordProviders = $derived(providers.filter((p) => p.supports_password));
  // Redirect-flow providers (SAML / OIDC, eventually). Rendered as
  // "Sign in with X" buttons that trigger a redirect.
  const redirectProviders = $derived(providers.filter((p) => !p.supports_password));

  async function onSubmit(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    if (submitting) return;
    error = null;
    submitting = true;
    try {
      await auth.login(username, password, selectedProvider, needTOTP ? totpCode : undefined);
      const next = page.url.searchParams.get('next') ?? '/';
      await goto(next);
    } catch (err) {
      if (err instanceof LoginNeedsTOTPError) {
        needTOTP = true;
        if (err.kind === 'invalid') {
          error = t('login.twofa_invalid');
        } else {
          error = null;
        }
        return;
      }
      error = err instanceof Error ? err.message : t('login.error_generic');
    } finally {
      submitting = false;
    }
  }

  function cancelTOTP() {
    needTOTP = false;
    totpCode = '';
    error = null;
  }

  function beginRedirectFlow(provider: ProviderSummary): void {
    // SAML SP-initiated login. Server route is mounted only when the
    // provider is registered (license-gated), so a direct nav here on
    // a Community install would 404 — but the button isn't rendered
    // either, so this is the licensed-install happy path.
    if (provider.kind === 'saml') {
      window.location.href = '/api/v1/auth/saml/login';
    }
  }
</script>

<svelte:head>
  <title>{t('login.title')} — artist-alley</title>
</svelte:head>

<div class="relative flex-1 flex items-center justify-center px-6 py-12 isolate">
  <!-- Background hero. Layered: image → darkening overlay → form card.
       The image is decorative (no semantic content) so it's
       `aria-hidden`; readability is guaranteed by the overlay +
       backdrop-blur on the card behind the form. -->
  <div
    class="absolute inset-0 -z-10 bg-cover bg-center bg-no-repeat"
    style="background-image: url('/hero.png');"
    aria-hidden="true"
  ></div>
  <div
    class="absolute inset-0 -z-10 bg-black/55 backdrop-blur-[2px]"
    aria-hidden="true"
  ></div>

  <div class="w-full max-w-sm space-y-8 rounded-xl border border-white/10 bg-surface/85 p-8 shadow-2xl backdrop-blur-md">
    <div class="text-center space-y-2">
      <img src="/logo.svg" alt="" class="mx-auto h-16 w-16" aria-hidden="true" />
      <h1 class="text-2xl font-semibold tracking-tight">{t('login.title')}</h1>
      <p class="text-sm text-fg-muted">artist-alley</p>
    </div>

    {#if passwordProviders.length > 1}
      <fieldset class="space-y-2">
        <legend class="block text-xs font-medium text-fg-muted">
          {t('login.provider_label')}
        </legend>
        <div class="flex flex-wrap gap-1.5">
          {#each passwordProviders as p (p.name)}
            <button
              type="button"
              class="rounded border px-2.5 py-1 text-xs font-medium transition"
              class:border-accent={selectedProvider === p.name}
              class:bg-accent={selectedProvider === p.name}
              class:text-on-accent={selectedProvider === p.name}
              class:border-border={selectedProvider !== p.name}
              class:text-fg={selectedProvider !== p.name}
              onclick={() => (selectedProvider = p.name)}
              disabled={submitting}
            >
              {p.display_name}
            </button>
          {/each}
        </div>
      </fieldset>
    {/if}

    <form class="space-y-4" onsubmit={onSubmit}>
      {#if error}
        <Alert tone="error">{error}</Alert>
      {/if}

      {#if needTOTP}
        <p class="text-sm text-fg-muted">{t('login.twofa_prompt', { username })}</p>
        <TextField
          label={t('login.twofa_label')}
          name="totp_code"
          autocomplete="one-time-code"
          required
          autofocus
          bind:value={totpCode}
          disabled={submitting}
          testId="login-totp"
        />
        <Button type="submit" variant="primary" fullWidth loading={submitting} testId="login-submit">
          {t('login.twofa_verify')}
        </Button>
        <button
          type="button"
          onclick={cancelTOTP}
          class="block w-full text-center text-xs text-fg-muted hover:text-fg"
        >{t('login.twofa_cancel')}</button>
      {:else}
        <TextField
          label={t('login.username_label')}
          name="username"
          autocomplete="username"
          required
          autofocus
          bind:value={username}
          disabled={submitting}
          testId="login-username"
        />

        <TextField
          label={t('login.password_label')}
          name="password"
          type="password"
          autocomplete="current-password"
          required
          bind:value={password}
          disabled={submitting}
          testId="login-password"
        />

        <Button type="submit" variant="primary" fullWidth loading={submitting} testId="login-submit">
          {t('login.submit')}
        </Button>
      {/if}
    </form>

    {#if redirectProviders.length > 0}
      <div class="space-y-2">
        <p class="text-center text-xs text-fg-muted">{t('login.or_continue_with')}</p>
        <div class="space-y-2">
          {#each redirectProviders as p (p.name)}
            <button
              type="button"
              class="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm font-medium hover:bg-surface-elevated"
              onclick={() => beginRedirectFlow(p)}
              disabled={submitting}
            >
              {t('login.sign_in_with', { provider: p.display_name })}
            </button>
          {/each}
        </div>
      </div>
    {/if}

    <p class="text-center text-xs text-fg-muted">
      {t('login.self_hosted_note')}
    </p>
  </div>
</div>

<style>
  /* Lock the page to fill the viewport — the hero image looks weak if
     the form is small and the rest of the page bleeds the page bg
     (instead of the photo) below it. The parent <main> is
     flex-1 + overflow-y-auto so this height: 100% propagates from there. */
  :global(main):has(> .relative.isolate) {
    background: transparent;
  }
</style>
