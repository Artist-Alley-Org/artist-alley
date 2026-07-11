<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin license-status page (Phase 1.17.O).
  //
  // Read-only for now — shows what the artist-alley app sees from
  // its embedded verifier + the .lic file on disk (if any). The
  // upload + validate flows ship in Phase 1.17.O-2.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface LicenseStatus {
    loaded: boolean;
    tier: string;
    features: string[];
    owner?: string;
    org?: string;
    lid?: string;
    seats?: number | null;
    seat_window_days?: number;
    asset_cap?: number | null;
    nbf?: string;
    exp?: string;
    iat?: string;
    days_until_expiry: number;
    last_error?: string;
    iss?: string;
    path?: string;
    org_binding_required: boolean;
    org_bound: boolean;
    org_binding_error?: string;
    org_key_path?: string;
  }

  interface ValidateError {
    error: string;
    code: string;
    message: string;
  }

  interface ProviderSummary {
    name: string;
    display_name: string;
    kind: 'password' | 'ldap' | 'saml';
    supports_password: boolean;
  }

  let status = $state<LicenseStatus | null>(null);
  let providers = $state<ProviderSummary[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Upload flow state. `previewStatus` mirrors the candidate license
  // that the verifier accepted; `previewError` carries a structured
  // verifier rejection so we can show a code-keyed i18n message.
  // `installing` covers both validate AND install — the button toggles
  // between them and we want the spinner consistent.
  let licenseText = $state('');
  let validating = $state(false);
  let installing = $state(false);
  let previewStatus = $state<LicenseStatus | null>(null);
  let previewError = $state<ValidateError | null>(null);
  let installError = $state<string | null>(null);
  let installSuccess = $state(false);

  onMount(() => {
    void load();
  });

  async function load(): Promise<void> {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/admin/license/status');
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.system.license.load_error');
        return;
      }
      status = r.data as unknown as LicenseStatus;
      // Also pull the live provider list so the admin can see exactly
      // which enterprise gates were activated at boot — separate
      // signal from "feature is in the license" (the feature list
      // above) vs. "the binary has the impl wired up" (the registry).
      const rp = await api.GET('/auth/providers');
      if (rp.data?.providers) {
        providers = rp.data.providers as ProviderSummary[];
      }
    } finally {
      loading = false;
    }
  }

  // Which enterprise gates are active right now? Cross-references the
  // license feature list with the live provider registry so the admin
  // sees whether each enterprise feature is BOTH licensed AND wired
  // into the running binary. A feature licensed but not registered
  // means the binary is older than the license — restart needed.
  const enterpriseGates = $derived(() => {
    const features = new Set(status?.features ?? []);
    const providerKinds = new Set(providers.map((p) => p.kind));
    return [
      {
        label: t('admin.system.license.gate_ldap'),
        feature: 'sso_ldap',
        licensed: features.has('sso_ldap'),
        active: providerKinds.has('ldap'),
      },
      {
        label: t('admin.system.license.gate_saml'),
        feature: 'sso_saml',
        licensed: features.has('sso_saml'),
        active: providerKinds.has('saml'),
      },
      {
        label: t('admin.system.license.gate_multi_tenant'),
        feature: 'multi_tenant',
        licensed: features.has('multi_tenant'),
        // Multi-tenant doesn't go through the provider registry —
        // it's an admin/middleware subsystem. Treat "licensed" as
        // "active" for display purposes until the future
        // /admin/tenancy/status endpoint surfaces a richer signal.
        active: features.has('multi_tenant'),
      },
    ];
  });

  async function validateLicense(): Promise<void> {
    if (!licenseText.trim()) return;
    validating = true;
    previewStatus = null;
    previewError = null;
    installError = null;
    installSuccess = false;
    try {
      const r = await api.POST('/admin/license/validate', {
        body: { license_text: licenseText },
      });
      if (r.data) {
        previewStatus = r.data as unknown as LicenseStatus;
        return;
      }
      // 400 carries LicenseValidateError; other failures fall through.
      const v = asValidateError(r.error);
      if (v) {
        previewError = v;
      } else {
        installError = readErrorText(r.error, t('admin.system.license.validate_error_unknown'));
      }
    } finally {
      validating = false;
    }
  }

  async function installLicense(): Promise<void> {
    if (!licenseText.trim()) return;
    installing = true;
    installError = null;
    installSuccess = false;
    try {
      const r = await api.POST('/admin/license/upload', {
        body: { license_text: licenseText },
      });
      if (r.data) {
        status = r.data as unknown as LicenseStatus;
        installSuccess = true;
        // Clear the editor so the operator doesn't accidentally
        // re-submit the same envelope; the new state is now in
        // `status` above.
        licenseText = '';
        previewStatus = null;
        previewError = null;
        return;
      }
      const v = asValidateError(r.error);
      if (v) {
        previewError = v;
      } else {
        installError = readErrorText(r.error, t('admin.system.license.install_error_unknown'));
      }
    } finally {
      installing = false;
    }
  }

  // The fetch client returns r.error as one of:
  //   - a structured object (LicenseValidateError, Error, Unauthorized…)
  //   - a plain string (server fell back to text — happens for 404/500
  //     before our handler runs)
  //   - undefined (network failure)
  // Narrow to ValidateError only when the shape really matches; the
  // 'in' operator throws on strings, so guard with typeof first.
  function asValidateError(err: unknown): ValidateError | null {
    if (err && typeof err === 'object' && 'code' in err && 'message' in err) {
      const v = err as { code: unknown; message: unknown };
      if (typeof v.code === 'string' && typeof v.message === 'string') {
        return err as ValidateError;
      }
    }
    return null;
  }

  function readErrorText(err: unknown, fallback: string): string {
    if (typeof err === 'string' && err.trim()) return err;
    if (err && typeof err === 'object' && 'error' in err) {
      const e = (err as { error?: unknown }).error;
      if (typeof e === 'string' && e.trim()) return e;
    }
    return fallback;
  }

  function validateErrorMessage(e: ValidateError): string {
    const k = `admin.system.license.validate_code_${e.code}`;
    const translated = t(k);
    // t() falls back to the key when missing — distinguish "we have a
    // translation" from "we don't" so we can show the raw verifier
    // message rather than a key.
    return translated === k ? e.message : translated;
  }

  function formatCap(n: number | null | undefined): string {
    if (n === null || n === undefined) return t('admin.system.license.cap_unlimited');
    if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n % 1_000_000 === 0 ? 0 : 1)}M`;
    if (n >= 1_000) return `${(n / 1_000).toFixed(n % 1_000 === 0 ? 0 : 1)}k`;
    return String(n);
  }

  function formatIso(s: string | null | undefined): string {
    if (!s) return '—';
    try {
      return new Date(s).toLocaleString();
    } catch {
      return s;
    }
  }

  function tierBadgeClass(tier: string): string {
    if (tier === 'enterprise') return 'border-danger/40 bg-danger/10 text-danger';
    if (tier === 'pro') return 'border-accent/40 bg-accent/10 text-accent';
    if (tier === 'complementary') return 'border-success/40 bg-success/10 text-success';
    if (tier === 'plugin') return 'border-warning/40 bg-warning/10 text-warning';
    if (tier === 'community') return 'border-fg-muted/40 bg-fg-muted/10 text-fg-muted';
    return 'border-border bg-surface text-fg-muted';
  }

  function expiryBadge(days: number): { cls: string; label: string } | null {
    if (days < 0) {
      return {
        cls: 'border-danger/40 bg-danger/10 text-danger',
        label: t('admin.system.license.badge_expired'),
      };
    }
    if (days <= 7) {
      return {
        cls: 'border-danger/40 bg-danger/10 text-danger',
        label: t('admin.system.license.badge_expires_soon', { days: String(days) }),
      };
    }
    if (days <= 30) {
      return {
        cls: 'border-warning/40 bg-warning/10 text-warning',
        label: t('admin.system.license.badge_expires_in', { days: String(days) }),
      };
    }
    return null;
  }
</script>

<svelte:head><title>{t('admin.system.license.title')} — artist-alley</title></svelte:head>

<header class="mb-4">
  <h1 class="text-xl font-semibold">{t('admin.system.license.title')}</h1>
  <p class="text-sm text-fg-muted">{t('admin.system.license.intro')}</p>
</header>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
{:else if status}
  {@const expBadge = expiryBadge(status.days_until_expiry)}

  <section class="mb-4 rounded-lg border border-border bg-surface-elevated p-4">
    <div class="flex flex-wrap items-center gap-3">
      <h2 class="text-lg font-medium">{t('admin.system.license.section_summary')}</h2>
      <span class={'badge ' + tierBadgeClass(status.tier)}>{status.tier}</span>
      {#if !status.loaded}
        <span class="badge border border-fg-muted/40 bg-fg-muted/10 text-fg-muted">
          {t('admin.system.license.badge_community_mode')}
        </span>
      {/if}
      {#if expBadge && status.loaded}
        <span class={'badge ' + expBadge.cls}>{expBadge.label}</span>
      {/if}
      {#if status.org_binding_required}
        {#if status.org_bound}
          <span class="badge border border-success/40 bg-success/10 text-success">
            {t('admin.system.license.badge_org_bound')}
          </span>
        {:else}
          <span class="badge border border-danger/40 bg-danger/10 text-danger">
            {t('admin.system.license.badge_org_unbound')}
          </span>
        {/if}
      {/if}
    </div>
    {#if !status.loaded}
      <p class="mt-3 text-sm text-fg-muted">
        {t('admin.system.license.community_explainer')}
      </p>
    {/if}
    {#if status.last_error}
      <p role="alert" class="mt-3 rounded border border-danger/40 bg-danger/5 p-2 text-xs text-danger">
        <strong>{t('admin.system.license.verify_error_label')}:</strong> {status.last_error}
      </p>
    {/if}
  </section>

  <section class="mb-4 rounded-lg border border-border bg-surface-elevated p-4">
    <h2 class="mb-3 text-sm font-medium">{t('admin.system.license.section_capacity')}</h2>
    <dl class="grid grid-cols-[10rem,1fr] gap-x-3 gap-y-1.5 text-xs">
      <dt class="text-fg-muted">{t('admin.system.license.field_tier')}</dt>
      <dd>{status.tier}</dd>
      <dt class="text-fg-muted">{t('admin.system.license.field_seats')}</dt>
      <dd>{formatCap(status.seats)}</dd>
      <dt class="text-fg-muted">{t('admin.system.license.field_seat_window')}</dt>
      <dd>{status.seat_window_days ? `${status.seat_window_days} ${t('admin.system.license.days')}` : '—'}</dd>
      <dt class="text-fg-muted">{t('admin.system.license.field_asset_cap')}</dt>
      <dd>{formatCap(status.asset_cap)}</dd>
    </dl>
  </section>

  <section class="mb-4 rounded-lg border border-border bg-surface-elevated p-4">
    <h2 class="mb-3 text-sm font-medium">{t('admin.system.license.section_features')}</h2>
    {#if status.features.length === 0}
      <p class="text-xs text-fg-muted">{t('admin.system.license.no_features')}</p>
    {:else}
      <div class="flex flex-wrap gap-1.5">
        {#each status.features as f (f)}
          <span class="badge border border-accent/40 bg-accent/10 font-mono text-accent">{f}</span>
        {/each}
      </div>
    {/if}
  </section>

  <section class="mb-4 rounded-lg border border-border bg-surface-elevated p-4">
    <h2 class="mb-1 text-sm font-medium">{t('admin.system.license.section_enterprise_gates')}</h2>
    <p class="mb-3 text-xs text-fg-muted">{t('admin.system.license.enterprise_gates_explainer')}</p>
    <ul class="space-y-1.5 text-xs">
      {#each enterpriseGates() as gate (gate.feature)}
        <li class="flex items-center gap-2">
          {#if gate.active}
            <span class="inline-block h-2 w-2 rounded-full bg-success" aria-hidden="true"></span>
            <span class="font-medium">{gate.label}</span>
            <span class="text-fg-muted">— {t('admin.system.license.gate_state_active')}</span>
          {:else if gate.licensed}
            <span class="inline-block h-2 w-2 rounded-full bg-warning" aria-hidden="true"></span>
            <span class="font-medium">{gate.label}</span>
            <span class="text-fg-muted">— {t('admin.system.license.gate_state_licensed_pending')}</span>
          {:else}
            <span class="inline-block h-2 w-2 rounded-full bg-fg-muted/40" aria-hidden="true"></span>
            <span class="text-fg-muted">{gate.label}</span>
            <span class="text-fg-muted">— {t('admin.system.license.gate_state_unlicensed')}</span>
          {/if}
          <span class="ml-auto font-mono text-[10px] text-fg-muted">{gate.feature}</span>
        </li>
      {/each}
    </ul>
  </section>

  {#if status.loaded}
    <section class="mb-4 rounded-lg border border-border bg-surface-elevated p-4">
      <h2 class="mb-3 text-sm font-medium">{t('admin.system.license.section_identity')}</h2>
      <dl class="grid grid-cols-[10rem,1fr] gap-x-3 gap-y-1.5 text-xs">
        <dt class="text-fg-muted">{t('admin.system.license.field_owner')}</dt>
        <dd class="font-mono">{status.owner ?? '—'}</dd>
        <dt class="text-fg-muted">{t('admin.system.license.field_org')}</dt>
        <dd class="font-mono">{status.org ?? '—'}</dd>
        <dt class="text-fg-muted">{t('admin.system.license.field_lid')}</dt>
        <dd class="break-all font-mono">{status.lid ?? '—'}</dd>
        <dt class="text-fg-muted">{t('admin.system.license.field_issuer')}</dt>
        <dd class="font-mono">{status.iss ?? '—'}</dd>
        <dt class="text-fg-muted">{t('admin.system.license.field_path')}</dt>
        <dd class="break-all font-mono">{status.path ?? '—'}</dd>
      </dl>
    </section>

    <section class="mb-4 rounded-lg border border-border bg-surface-elevated p-4">
      <h2 class="mb-3 text-sm font-medium">{t('admin.system.license.section_validity')}</h2>
      <dl class="grid grid-cols-[10rem,1fr] gap-x-3 gap-y-1.5 text-xs">
        <dt class="text-fg-muted">{t('admin.system.license.field_nbf')}</dt>
        <dd>{formatIso(status.nbf)}</dd>
        <dt class="text-fg-muted">{t('admin.system.license.field_exp')}</dt>
        <dd>{formatIso(status.exp)}</dd>
        <dt class="text-fg-muted">{t('admin.system.license.field_iat')}</dt>
        <dd>{formatIso(status.iat)}</dd>
        <dt class="text-fg-muted">{t('admin.system.license.field_days_until_expiry')}</dt>
        <dd>{status.days_until_expiry}</dd>
      </dl>
    </section>
  {/if}

  {#if status.org_binding_required}
    <section class="rounded-lg border border-border bg-surface-elevated p-4">
      <h2 class="mb-2 text-sm font-medium">{t('admin.system.license.section_org_binding')}</h2>
      <p class="mb-3 text-xs text-fg-muted">{t('admin.system.license.org_binding_explainer')}</p>
      <dl class="grid grid-cols-[10rem,1fr] gap-x-3 gap-y-1.5 text-xs">
        <dt class="text-fg-muted">{t('admin.system.license.field_org_status')}</dt>
        <dd>
          {#if status.org_bound}
            <span class="text-success">{t('admin.system.license.org_status_bound')}</span>
          {:else}
            <span class="text-danger">{t('admin.system.license.org_status_unbound')}</span>
          {/if}
        </dd>
        <dt class="text-fg-muted">{t('admin.system.license.field_org_key_path')}</dt>
        <dd class="break-all font-mono">{status.org_key_path ?? '—'}</dd>
      </dl>
      {#if !status.org_bound && status.org_binding_error}
        <p role="alert" class="mt-3 rounded border border-danger/40 bg-danger/10 p-2 text-xs text-danger">
          <strong>{t('admin.system.license.org_binding_error_label')}:</strong> {status.org_binding_error}
        </p>
        <p class="mt-2 text-xs text-fg-muted">
          {t('admin.system.license.org_binding_recovery_hint')}
        </p>
      {/if}
    </section>
  {/if}

  <section class="mt-4 rounded-lg border border-border bg-surface-elevated p-4">
    <h2 class="mb-2 text-sm font-medium">{t('admin.system.license.section_install')}</h2>
    <p class="mb-3 text-xs text-fg-muted">{t('admin.system.license.install_explainer')}</p>

    <label for="license-text" class="mb-1 block text-xs font-medium">
      {t('admin.system.license.upload_label')}
    </label>
    <textarea
      id="license-text"
      bind:value={licenseText}
      class="mb-3 block w-full rounded border border-border bg-bg p-2 font-mono text-xs"
      rows="10"
      placeholder={t('admin.system.license.upload_placeholder')}
      spellcheck="false"
      autocomplete="off"
    ></textarea>

    <div class="flex flex-wrap gap-2">
      <button
        type="button"
        class="btn-secondary text-sm"
        disabled={validating || installing || !licenseText.trim()}
        onclick={() => void validateLicense()}
      >
        {validating ? t('common.loading') : t('admin.system.license.validate_button')}
      </button>
      <button
        type="button"
        class="btn-primary text-sm"
        disabled={validating || installing || !licenseText.trim()}
        onclick={() => void installLicense()}
      >
        {installing ? t('common.loading') : t('admin.system.license.install_button')}
      </button>
    </div>

    {#if installSuccess}
      <p role="status" class="mt-3 rounded border border-success/40 bg-success/10 p-2 text-xs text-success">
        {t('admin.system.license.install_success')}
      </p>
    {/if}

    {#if previewError}
      <p role="alert" class="mt-3 rounded border border-danger/40 bg-danger/10 p-2 text-xs text-danger">
        <strong>{t('admin.system.license.validate_error_label')}:</strong>
        {validateErrorMessage(previewError)}
      </p>
    {:else if installError}
      <p role="alert" class="mt-3 rounded border border-danger/40 bg-danger/10 p-2 text-xs text-danger">
        {installError}
      </p>
    {:else if previewStatus}
      <div class="mt-3 rounded border border-accent/40 bg-accent/5 p-3 text-xs">
        <p class="mb-2 font-medium">{t('admin.system.license.validate_preview_label')}</p>
        <dl class="grid grid-cols-[10rem,1fr] gap-x-3 gap-y-1">
          <dt class="text-fg-muted">{t('admin.system.license.field_tier')}</dt>
          <dd>{previewStatus.tier}</dd>
          <dt class="text-fg-muted">{t('admin.system.license.field_owner')}</dt>
          <dd class="font-mono">{previewStatus.owner ?? '—'}</dd>
          <dt class="text-fg-muted">{t('admin.system.license.field_org')}</dt>
          <dd class="font-mono">{previewStatus.org ?? '—'}</dd>
          <dt class="text-fg-muted">{t('admin.system.license.field_lid')}</dt>
          <dd class="break-all font-mono">{previewStatus.lid ?? '—'}</dd>
          <dt class="text-fg-muted">{t('admin.system.license.field_exp')}</dt>
          <dd>{formatIso(previewStatus.exp)}</dd>
          <dt class="text-fg-muted">{t('admin.system.license.field_seats')}</dt>
          <dd>{formatCap(previewStatus.seats)}</dd>
          <dt class="text-fg-muted">{t('admin.system.license.field_asset_cap')}</dt>
          <dd>{formatCap(previewStatus.asset_cap)}</dd>
        </dl>
      </div>
    {/if}
  </section>
{/if}
