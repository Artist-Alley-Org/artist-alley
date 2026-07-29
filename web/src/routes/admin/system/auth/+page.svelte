<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import PasswordInput from '$components/PasswordInput.svelte';

  // Provider config is a TYPED per-kind block, not a free-form map
  // (#718). It used to be the latter, and the read path returned it
  // verbatim to any `system.config.read` holder — so every OAuth client
  // secret, LDAP bind password and SAML SP private key was readable by
  // a capability that cannot set one.
  //
  // The three secrets below are write-only. The API returns
  // `<field>_set` booleans and never the values, so each input starts
  // empty on every load and only ever holds what this admin just typed
  // — the same argument that makes the reveal toggle safe on the AI and
  // SMTP surfaces. Empty on save means "keep what's stored"; the server
  // merges the on-file value in per provider id. Sending '' would wipe
  // every credential on an unrelated edit (#708's shape).
  type ProviderKind = 'ldap' | 'saml' | 'google' | 'github' | 'x';

  interface SSOConfig {
    // OAuth 2.0 / OIDC — google | github | x
    client_id?: string;
    /** Write-only. Blank = unchanged. Never populated by load(). */
    client_secret: string;
    client_secret_set?: boolean;
    redirect_uri?: string;
    scopes?: string[];
    // LDAP
    server_url?: string;
    start_tls?: boolean;
    base_dn?: string;
    bind_dn?: string;
    /** Write-only. Blank = unchanged. */
    bind_password: string;
    bind_password_set?: boolean;
    user_search_filter?: string;
    // SAML 2.0
    idp_metadata_url?: string;
    idp_entity_id?: string;
    idp_certificate?: string;
    sp_entity_id?: string;
    sp_acs_url?: string;
    /** Write-only. Blank = unchanged. */
    sp_private_key: string;
    sp_private_key_set?: boolean;
  }

  interface SSOProvider {
    id?: string;
    kind: ProviderKind;
    enabled: boolean;
    display_name: string;
    config: SSOConfig;
  }

  const OAUTH_KINDS: ProviderKind[] = ['google', 'github', 'x'];

  function blankConfig(): SSOConfig {
    return { client_secret: '', bind_password: '', sp_private_key: '' };
  }

  // Whatever the server sent, minus any secret it did not (and must
  // not) send. The three write-only fields are forced blank so a value
  // left over from a previous save is never re-posted.
  function toEditable(cfg: Partial<SSOConfig> | undefined): SSOConfig {
    return { ...blankConfig(), ...(cfg ?? {}), client_secret: '', bind_password: '', sp_private_key: '' };
  }

  let policy = $state({
    min_length: 0,
    require_upper: false,
    require_number: false,
    require_symbol: false,
    disallow_common: false,
    max_age_days: 0,
  });
  let providers = $state<SSOProvider[]>([]);
  // Phase 1.19.C — self-registration knobs.
  let selfRegistration = $state({
    enabled: false,
    require_email_verification: true,
    default_role: 'Base',
  });
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/admin/system/auth');
      if (data) {
        const d = data as {
          password_policy?: typeof policy;
          sso_providers?: SSOProvider[];
          self_registration?: typeof selfRegistration;
        };
        if (d.password_policy) policy = { ...policy, ...d.password_policy };
        providers = (d.sso_providers ?? []).map((p) => ({ ...p, config: toEditable(p.config) }));
        if (d.self_registration) selfRegistration = { ...selfRegistration, ...d.self_registration };
      }
    } finally {
      loading = false;
    }
  }

  function addProvider() {
    providers = [...providers, { kind: 'ldap', enabled: false, display_name: 'New provider', config: blankConfig() }];
  }

  function removeProvider(idx: number) {
    providers = providers.filter((_, i) => i !== idx);
  }

  // Scopes are an array on the wire and a comma-separated line in the
  // form. Kept as a pair of pure converters rather than a second piece
  // of state, so there is nothing to keep in sync.
  function scopesText(cfg: SSOConfig): string {
    return (cfg.scopes ?? []).join(', ');
  }

  function setScopes(cfg: SSOConfig, raw: string) {
    const list = raw.split(',').map((s) => s.trim()).filter(Boolean);
    cfg.scopes = list.length > 0 ? list : undefined;
  }

  async function save() {
    if (saving) return;
    saving = true;
    saved = false;
    error = null;
    try {
      // Drop the read-only `*_set` markers, and omit each secret
      // entirely unless the admin typed one — the body must say "no
      // opinion about this credential", not "set it to empty".
      const payload = providers.map((p) => {
        const {
          client_secret, client_secret_set,
          bind_password, bind_password_set,
          sp_private_key, sp_private_key_set,
          ...rest
        } = p.config;
        void client_secret_set; void bind_password_set; void sp_private_key_set;
        return {
          id: p.id,
          kind: p.kind,
          enabled: p.enabled,
          display_name: p.display_name,
          config: {
            ...rest,
            ...(client_secret ? { client_secret } : {}),
            ...(bind_password ? { bind_password } : {}),
            ...(sp_private_key ? { sp_private_key } : {}),
          },
        };
      });
      const { error: apiErr } = await api.PATCH('/admin/system/auth', {
        body: {
          password_policy: policy,
          sso_providers: payload,
          self_registration: selfRegistration,
        } as never,
      });
      if (apiErr) {
        error = (apiErr as { error?: string }).error ?? t('errors.save_failed');
        return;
      }
      saved = true;
      await load();
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>{t('admin.system.auth.title')} — {site.name}</title></svelte:head>

<h2 class="mb-4 text-xl font-semibold">{t('admin.system.auth.title')}</h2>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else}
  <form onsubmit={(e) => { e.preventDefault(); void save(); }} class="max-w-3xl space-y-6">
    <section class="space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
      <h3 class="text-sm font-medium text-fg">{t('admin.system.auth.password_policy')}</h3>
      <div class="grid grid-cols-2 gap-3">
        <label class="text-sm">
          <span class="block text-xs text-fg-muted">{t('admin.system.auth.min_length')}</span>
          <input type="number" min="0" max="256" bind:value={policy.min_length} class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
        </label>
        <label class="text-sm">
          <span class="block text-xs text-fg-muted">{t('admin.system.auth.max_age_days')}</span>
          <input type="number" min="0" max="36500" bind:value={policy.max_age_days} class="mt-1 w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
        </label>
      </div>
      <div class="grid grid-cols-2 gap-2">
        <label class="inline-flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={policy.require_upper} class="h-4 w-4 accent-accent" />{t('admin.system.auth.require_upper')}</label>
        <label class="inline-flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={policy.require_number} class="h-4 w-4 accent-accent" />{t('admin.system.auth.require_number')}</label>
        <label class="inline-flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={policy.require_symbol} class="h-4 w-4 accent-accent" />{t('admin.system.auth.require_symbol')}</label>
        <label class="inline-flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={policy.disallow_common} class="h-4 w-4 accent-accent" />{t('admin.system.auth.disallow_common')}</label>
      </div>
    </section>

    <section class="space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
      <h3 class="text-sm font-medium text-fg">{t('admin.system.auth.self_registration_heading')}</h3>
      <p class="text-xs text-fg-muted">{t('admin.system.auth.self_registration_help')}</p>
      <label class="inline-flex items-center gap-2 text-sm">
        <input type="checkbox" bind:checked={selfRegistration.enabled} class="h-4 w-4 accent-accent" data-testid="auth-selfreg-enabled" />
        {t('admin.system.auth.self_registration_enabled')}
      </label>
      <label class="inline-flex items-center gap-2 text-sm">
        <input type="checkbox" bind:checked={selfRegistration.require_email_verification} class="h-4 w-4 accent-accent" data-testid="auth-selfreg-verify" />
        {t('admin.system.auth.self_registration_require_verify')}
      </label>
      <label class="block">
        <span class="block text-xs text-fg-muted">{t('admin.system.auth.self_registration_default_role')}</span>
        <input type="text" bind:value={selfRegistration.default_role} placeholder="Base" class="mt-1 w-full max-w-xs rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
      </label>
    </section>

    <section class="space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
      <header class="flex items-center justify-between">
        <h3 class="text-sm font-medium text-fg">{t('admin.system.auth.sso_providers')}</h3>
        <button type="button" onclick={addProvider} class="rounded-md border border-border px-2.5 py-1 text-xs text-fg-muted hover:text-fg">
          {t('admin.system.auth.add_provider')}
        </button>
      </header>
      {#if providers.length === 0}
        <p class="rounded-md bg-surface-elevated px-3 py-2 text-xs text-fg-muted">{t('admin.system.auth.no_providers')}</p>
      {:else}
        <div class="space-y-2">
          {#each providers as p, idx (idx)}
            <article class="rounded border border-border bg-surface-elevated p-3">
              <div class="grid grid-cols-1 gap-2 md:grid-cols-[10rem_1fr_auto_auto]">
                <label class="text-sm">
                  <span class="block text-xs text-fg-muted">{t('admin.system.auth.provider_kind')}</span>
                  <select bind:value={providers[idx].kind} class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none">
                    <option value="ldap">LDAP</option>
                    <option value="saml">SAML</option>
                    <option value="google">Google</option>
                    <option value="github">GitHub</option>
                    <option value="x">X</option>
                  </select>
                </label>
                <label class="text-sm">
                  <span class="block text-xs text-fg-muted">{t('admin.system.auth.provider_display_name')}</span>
                  <input type="text" bind:value={providers[idx].display_name} class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                </label>
                <label class="inline-flex items-end gap-1 text-sm">
                  <input type="checkbox" bind:checked={providers[idx].enabled} class="h-4 w-4 accent-accent" />
                  <span class="pb-1 text-xs text-fg-muted">{t('admin.system.auth.provider_enabled')}</span>
                </label>
                <button type="button" onclick={() => removeProvider(idx)} class="self-end rounded-md border border-danger/40 px-2 py-1 text-xs text-danger hover:bg-danger-container">
                  {t('admin.system.auth.remove_provider')}
                </button>
              </div>

              <!--
                Per-kind config (#718). Only the block matching this
                provider's kind renders; the fields the API defines for
                the other two stay in state untouched, so switching kind
                to look and switching back is not destructive.
              -->
              <div class="mt-3 border-t border-border pt-3">
                <p class="mb-2 text-xs font-medium text-fg-muted">{t('admin.system.auth.provider_config')}</p>

                {#if OAUTH_KINDS.includes(p.kind)}
                  <div class="grid grid-cols-1 gap-2 md:grid-cols-2">
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.client_id')}</span>
                      <input type="text" bind:value={providers[idx].config.client_id} data-testid="sso-client-id-{idx}" class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                    </label>
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.client_secret')}</span>
                      <PasswordInput
                        bind:value={providers[idx].config.client_secret}
                        placeholder={providers[idx].config.client_secret_set ? t('admin.system.auth.secret_on_file') : t('admin.system.auth.secret_unset')}
                        autocomplete="new-password"
                        testId="sso-client-secret-{idx}"
                        inputClass="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                      />
                      <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.auth.secret_help')}</span>
                    </label>
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.redirect_uri')}</span>
                      <input type="url" bind:value={providers[idx].config.redirect_uri} class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                    </label>
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.scopes')}</span>
                      <input
                        type="text"
                        value={scopesText(providers[idx].config)}
                        oninput={(e) => setScopes(providers[idx].config, e.currentTarget.value)}
                        placeholder="openid, email"
                        class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                      />
                      <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.auth.scopes_help')}</span>
                    </label>
                  </div>
                {:else if p.kind === 'ldap'}
                  <div class="grid grid-cols-1 gap-2 md:grid-cols-2">
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.server_url')}</span>
                      <input type="text" bind:value={providers[idx].config.server_url} placeholder="ldaps://ldap.example.org:636" data-testid="sso-server-url-{idx}" class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                    </label>
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.base_dn')}</span>
                      <input type="text" bind:value={providers[idx].config.base_dn} placeholder="ou=people,dc=example,dc=org" class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                    </label>
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.bind_dn')}</span>
                      <input type="text" bind:value={providers[idx].config.bind_dn} placeholder="cn=svc,dc=example,dc=org" class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                    </label>
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.bind_password')}</span>
                      <PasswordInput
                        bind:value={providers[idx].config.bind_password}
                        placeholder={providers[idx].config.bind_password_set ? t('admin.system.auth.secret_on_file') : t('admin.system.auth.secret_unset')}
                        autocomplete="new-password"
                        testId="sso-bind-password-{idx}"
                        inputClass="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                      />
                      <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.auth.secret_help')}</span>
                    </label>
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.user_search_filter')}</span>
                      <input type="text" bind:value={providers[idx].config.user_search_filter} placeholder="(uid=%s)" class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                    </label>
                    <label class="inline-flex items-end gap-2 text-sm">
                      <input type="checkbox" bind:checked={providers[idx].config.start_tls} class="h-4 w-4 accent-accent" />
                      <span class="pb-1 text-xs text-fg-muted">{t('admin.system.auth.start_tls')}</span>
                    </label>
                  </div>
                {:else}
                  <div class="grid grid-cols-1 gap-2 md:grid-cols-2">
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.idp_metadata_url')}</span>
                      <input type="url" bind:value={providers[idx].config.idp_metadata_url} data-testid="sso-idp-metadata-{idx}" class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                    </label>
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.idp_entity_id')}</span>
                      <input type="text" bind:value={providers[idx].config.idp_entity_id} class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                    </label>
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.sp_entity_id')}</span>
                      <input type="text" bind:value={providers[idx].config.sp_entity_id} class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                    </label>
                    <label class="text-sm">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.sp_acs_url')}</span>
                      <input type="url" bind:value={providers[idx].config.sp_acs_url} class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                    </label>
                    <label class="text-sm md:col-span-2">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.idp_certificate')}</span>
                      <textarea rows="3" bind:value={providers[idx].config.idp_certificate} class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 font-mono text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"></textarea>
                      <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.auth.idp_certificate_help')}</span>
                    </label>
                    <label class="text-sm md:col-span-2">
                      <span class="block text-xs text-fg-muted">{t('admin.system.auth.sp_private_key')}</span>
                      <PasswordInput
                        bind:value={providers[idx].config.sp_private_key}
                        placeholder={providers[idx].config.sp_private_key_set ? t('admin.system.auth.secret_on_file') : t('admin.system.auth.secret_unset')}
                        autocomplete="new-password"
                        testId="sso-sp-private-key-{idx}"
                        inputClass="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                      />
                      <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.auth.secret_help')}</span>
                    </label>
                  </div>
                {/if}
              </div>
            </article>
          {/each}
        </div>
      {/if}
    </section>

    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
    {/if}
    {#if saved}
      <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success">{t('admin.system.auth.saved')}</p>
    {/if}

    <button type="submit" disabled={saving} class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40">
      {saving ? t('common.loading') : t('admin.system.auth.save')}
    </button>
  </form>
{/if}
