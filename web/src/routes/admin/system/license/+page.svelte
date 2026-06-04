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
  }

  let status = $state<LicenseStatus | null>(null);
  let loading = $state(true);
  let error = $state<string | null>(null);

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
    } finally {
      loading = false;
    }
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

    <section class="rounded-lg border border-border bg-surface-elevated p-4">
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

  <p class="mt-4 text-xs text-fg-dim">
    {t('admin.system.license.upload_coming_soon')}
  </p>
{/if}
