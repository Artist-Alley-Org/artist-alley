<script lang="ts">
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import Button from '$components/Button.svelte';
  import TextField from '$components/TextField.svelte';
  import Alert from '$components/Alert.svelte';
  import { onMount } from 'svelte';

  // Prefilled defaults come from AA_SETUP_DEFAULT_* env vars on the
  // server side (see internal/setup/handler.go). We pull them so a
  // packaged deployment lands at /setup with most fields ready.
  interface Defaults {
    admin_username: string;
    admin_email: string;
    admin_fullname: string;
    site_name: string;
    site_base_url: string;
    smtp_host: string;
    smtp_port: number;
    smtp_encryption: 'none' | 'starttls' | 'tls';
    smtp_username: string;
    smtp_from_address: string;
  }

  let defaults = $state<Defaults | null>(null);

  // Admin
  let adminUsername = $state('');
  let adminEmail = $state('');
  let adminFullname = $state('');
  let adminPassword = $state('');
  let adminPasswordConfirm = $state('');

  // Site
  let siteName = $state('');
  let siteBaseUrl = $state('');

  // SMTP (optional)
  let configureSMTP = $state(false);
  let smtpHost = $state('');
  let smtpPort = $state('587');
  let smtpEncryption = $state<'none' | 'starttls' | 'tls'>('starttls');
  let smtpUsername = $state('');
  let smtpPassword = $state('');
  let smtpFromAddress = $state('');

  let submitting = $state(false);
  let error = $state<string | null>(null);

  onMount(async () => {
    const { data } = await api.GET('/setup/status');
    if (data?.defaults) {
      defaults = data.defaults as Defaults;
      adminUsername = defaults.admin_username || '';
      adminEmail = defaults.admin_email || '';
      adminFullname = defaults.admin_fullname || '';
      siteName = defaults.site_name || '';
      siteBaseUrl = defaults.site_base_url || '';
      smtpHost = defaults.smtp_host || '';
      smtpPort = String(defaults.smtp_port || 587);
      smtpEncryption = defaults.smtp_encryption || 'starttls';
      smtpUsername = defaults.smtp_username || '';
      smtpFromAddress = defaults.smtp_from_address || '';
      if (smtpHost) configureSMTP = true;
    }
  });

  async function onSubmit(e: SubmitEvent) {
    e.preventDefault();
    if (submitting) return;
    error = null;

    if (adminPassword !== adminPasswordConfirm) {
      error = 'Passwords do not match.';
      return;
    }
    if (adminPassword.length < 8) {
      error = 'Password must be at least 8 characters.';
      return;
    }

    submitting = true;
    try {
      const body: Record<string, unknown> = {
        admin: {
          username: adminUsername,
          password: adminPassword,
          email: adminEmail,
          fullname: adminFullname || null,
        },
        site: {
          name: siteName,
          base_url: siteBaseUrl || undefined,
        },
        smtp: configureSMTP
          ? {
              host: smtpHost,
              port: Number(smtpPort) || 587,
              encryption: smtpEncryption,
              username: smtpUsername || undefined,
              password: smtpPassword || undefined,
              from_address: smtpFromAddress,
            }
          : null,
      };
      const { data, error: apiErr } = await api.POST('/setup/complete', {
        // openapi-fetch types this strictly via the schema; the cast
        // keeps the call ergonomic without re-typing every field.
        body: body as never,
      });
      if (apiErr || !data) {
        const msg =
          (apiErr as { error?: string } | undefined)?.error ?? 'Setup failed';
        throw new Error(msg);
      }
      // /setup/complete logs the new admin in as a side effect, so
      // we already have a session cookie. Refresh the auth store and
      // land on the home page.
      await auth.refresh();
      await goto('/');
    } catch (err) {
      error = err instanceof Error ? err.message : 'Setup failed';
    } finally {
      submitting = false;
    }
  }
</script>

<svelte:head>
  <title>First-run setup — artist-alley</title>
</svelte:head>

<div class="flex-1 flex items-center justify-center px-6 py-10">
  <div class="w-full max-w-2xl space-y-8">
    <div class="text-center space-y-2">
      <img src="/logo.svg" alt="" class="mx-auto h-16 w-16" aria-hidden="true" />
      <h1 class="text-2xl font-semibold tracking-tight">First-run setup</h1>
      <p class="text-sm text-fg-muted">
        Create the first administrator and configure your site. You can change any of this later.
      </p>
    </div>

    <form class="space-y-8" onsubmit={onSubmit}>
      {#if error}
        <Alert tone="error">{error}</Alert>
      {/if}

      <!-- Admin -->
      <section class="space-y-4">
        <header>
          <h2 class="text-sm font-semibold uppercase tracking-wide text-fg-muted">
            Administrator
          </h2>
        </header>
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <TextField
            label="Username"
            name="admin_username"
            autocomplete="username"
            required
            bind:value={adminUsername}
            disabled={submitting}
          />
          <TextField
            label="Email"
            name="admin_email"
            type="email"
            autocomplete="email"
            required
            bind:value={adminEmail}
            disabled={submitting}
          />
          <TextField
            label="Full name"
            name="admin_fullname"
            autocomplete="name"
            bind:value={adminFullname}
            disabled={submitting}
          />
          <div class="hidden sm:block"></div>
          <TextField
            label="Password"
            name="admin_password"
            type="password"
            autocomplete="new-password"
            required
            minlength={8}
            helper="At least 8 characters."
            bind:value={adminPassword}
            disabled={submitting}
          />
          <TextField
            label="Confirm password"
            name="admin_password_confirm"
            type="password"
            autocomplete="new-password"
            required
            minlength={8}
            bind:value={adminPasswordConfirm}
            disabled={submitting}
          />
        </div>
      </section>

      <!-- Site -->
      <section class="space-y-4">
        <header>
          <h2 class="text-sm font-semibold uppercase tracking-wide text-fg-muted">Site</h2>
        </header>
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <TextField
            label="Site name"
            name="site_name"
            required
            bind:value={siteName}
            disabled={submitting}
          />
          <TextField
            label="Base URL"
            name="site_base_url"
            type="url"
            placeholder="https://art.example.com"
            helper="Used in outgoing links (e.g. password reset). May be left blank now."
            bind:value={siteBaseUrl}
            disabled={submitting}
          />
        </div>
      </section>

      <!-- SMTP -->
      <section class="space-y-4">
        <header class="flex items-center justify-between gap-4">
          <h2 class="text-sm font-semibold uppercase tracking-wide text-fg-muted">SMTP</h2>
          <label class="inline-flex items-center gap-2 text-sm text-fg-muted">
            <input
              type="checkbox"
              bind:checked={configureSMTP}
              disabled={submitting}
              class="rounded border-border bg-surface-elevated"
            />
            Configure now
          </label>
        </header>
        {#if configureSMTP}
          <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <TextField
              label="Host"
              name="smtp_host"
              required
              bind:value={smtpHost}
              disabled={submitting}
            />
            <TextField
              label="Port"
              name="smtp_port"
              required
              bind:value={smtpPort}
              disabled={submitting}
            />
            <label class="space-y-1.5">
              <span class="block text-sm font-medium text-fg">Encryption</span>
              <select
                bind:value={smtpEncryption}
                disabled={submitting}
                class="block w-full rounded-md border border-border bg-surface-elevated px-3 py-2 text-sm text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                <option value="none">None</option>
                <option value="starttls">STARTTLS</option>
                <option value="tls">TLS</option>
              </select>
            </label>
            <TextField
              label="From address"
              name="smtp_from"
              required
              placeholder="Site <noreply@example.com>"
              bind:value={smtpFromAddress}
              disabled={submitting}
            />
            <TextField
              label="Username"
              name="smtp_user"
              bind:value={smtpUsername}
              disabled={submitting}
            />
            <TextField
              label="Password"
              name="smtp_pass"
              type="password"
              bind:value={smtpPassword}
              disabled={submitting}
            />
          </div>
        {:else}
          <p class="text-xs text-fg-muted">
            Email features (password reset, notifications) stay disabled until SMTP is configured. You can fill this in later from the admin settings.
          </p>
        {/if}
      </section>

      <div class="flex justify-end pt-2">
        <Button type="submit" variant="primary" loading={submitting}>
          Create admin & finish setup
        </Button>
      </div>
    </form>
  </div>
</div>
