<script lang="ts">
  // /admin/federation/key-health — Phase 1.22.I-h admin dashboard.
  //
  // Five aggregate counts on the I-arc encryption surface:
  //
  //   * Users without a federation keypair (I-b backfill gap).
  //   * Remote actors whose encryption pubkey cache is empty
  //     (I-c miss).
  //   * Peers without negotiated capabilities (I-d miss).
  //   * Retained keys near expiry (sweeper preview).
  //   * The total approved-user denominator.
  //
  // Drill-down tables ride along: which users are missing keypairs
  // (so the admin can trigger a per-user backfill), and recent
  // rotations (operator + self).

  import { onMount } from 'svelte';
  import { api } from '$api/client';

  type RotationEvent = {
    user_ref: number;
    version: number;
    rotated_at: string;
    rotated_by_user_ref?: number | null;
  };

  type UserMissing = {
    ref: number;
    username?: string | null;
    created?: string | null;
  };

  type Health = {
    users_total: number;
    users_missing_keypair: number;
    remote_actors_missing_enc_key: number;
    peers_missing_capabilities: number;
    retained_keys_near_expiry: number;
    recent_rotations?: RotationEvent[];
    users_missing_keypair_sample?: UserMissing[];
  };

  let loading = $state(true);
  let error = $state<string | null>(null);
  let health = $state<Health | null>(null);

  let rotating = $state<number | null>(null);
  let rotateError = $state<string | null>(null);
  let rotateSuccess = $state<string | null>(null);

  async function load() {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/admin/federation/key-health', {});
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to load.';
        return;
      }
      health = r.data as Health;
    } finally {
      loading = false;
    }
  }

  onMount(load);

  function fmtDate(iso: string | null | undefined): string {
    if (!iso) return '—';
    try {
      return new Date(iso).toLocaleString();
    } catch {
      return iso;
    }
  }

  function actorLabel(ev: RotationEvent): string {
    if (ev.rotated_by_user_ref == null) return 'system';
    if (ev.rotated_by_user_ref === ev.user_ref) return 'self';
    return `admin (#${ev.rotated_by_user_ref})`;
  }

  async function adminRotate(userRef: number) {
    if (rotating != null) return;
    rotating = userRef;
    rotateError = null;
    rotateSuccess = null;
    try {
      const r = await api.POST('/admin/federation/users/{ref}/rotate-keys', {
        params: { path: { ref: userRef } },
      });
      if (r.error || !r.data) {
        rotateError = (r.error as { error?: string } | undefined)?.error
          ?? `Rotation failed for user #${userRef}.`;
        return;
      }
      const result = r.data as { new_version: number };
      rotateSuccess = `Rotated user #${userRef} to v${result.new_version}.`;
      await load();
    } finally {
      rotating = null;
    }
  }
</script>

<svelte:head><title>Federation key health — artist-alley</title></svelte:head>

<h1 class="mb-1 text-2xl font-semibold">Federation key health</h1>
<p class="mb-4 max-w-3xl text-sm text-fg-muted">
  Aggregate counts on the federation encryption surface. Non-zero values
  in the gap tiles surface dogfood-class issues operators can act on
  directly — the I-b backfill safety net should converge to zero, the
  I-c cache fills naturally as inbound activities land, and the I-d
  re-pair queue clears as operators re-pair stale peers.
</p>

{#if loading}
  <p class="text-sm text-fg-muted">Loading…</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
{:else if health}
  <div class="mb-6 grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-5">
    <div class="rounded-lg border border-border bg-surface-elevated p-3">
      <p class="text-xs uppercase text-fg-muted">Users (approved)</p>
      <p class="text-2xl font-semibold">{health.users_total}</p>
    </div>
    <div class="rounded-lg border p-3 {health.users_missing_keypair > 0 ? 'border-danger/40 bg-danger-container' : 'border-border bg-surface-elevated'}">
      <p class="text-xs uppercase text-fg-muted">Without keypair</p>
      <p class="text-2xl font-semibold {health.users_missing_keypair > 0 ? 'text-danger' : ''}">{health.users_missing_keypair}</p>
    </div>
    <div class="rounded-lg border p-3 {health.remote_actors_missing_enc_key > 0 ? 'border-warning/40 bg-warning/10' : 'border-border bg-surface-elevated'}">
      <p class="text-xs uppercase text-fg-muted">Remote actors w/o enc key</p>
      <p class="text-2xl font-semibold">{health.remote_actors_missing_enc_key}</p>
    </div>
    <div class="rounded-lg border p-3 {health.peers_missing_capabilities > 0 ? 'border-warning/40 bg-warning/10' : 'border-border bg-surface-elevated'}">
      <p class="text-xs uppercase text-fg-muted">Peers w/o caps</p>
      <p class="text-2xl font-semibold">{health.peers_missing_capabilities}</p>
    </div>
    <div class="rounded-lg border border-border bg-surface-elevated p-3">
      <p class="text-xs uppercase text-fg-muted">Retained keys near expiry (7d)</p>
      <p class="text-2xl font-semibold">{health.retained_keys_near_expiry}</p>
    </div>
  </div>

  {#if rotateError}
    <p role="alert" class="mb-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{rotateError}</p>
  {/if}
  {#if rotateSuccess}
    <p class="mb-3 rounded border border-success/40 bg-success/10 px-3 py-2 text-sm text-success">{rotateSuccess}</p>
  {/if}

  {#if health.users_missing_keypair_sample && health.users_missing_keypair_sample.length > 0}
    <section class="mb-6">
      <h2 class="mb-2 text-lg font-semibold">Users missing keypair</h2>
      <p class="mb-2 text-xs text-fg-muted">
        Each rotation here mints a fresh keypair as the admin (recorded
        as compromised-key recovery; subject ≠ rotated_by).
      </p>
      <table class="w-full max-w-3xl text-sm">
        <thead class="text-xs uppercase text-fg-muted">
          <tr><th class="text-left">Ref</th><th class="text-left">Username</th><th class="text-left">Created</th><th></th></tr>
        </thead>
        <tbody>
          {#each health.users_missing_keypair_sample as u (u.ref)}
            <tr class="border-t border-border">
              <td class="py-1.5">{u.ref}</td>
              <td class="py-1.5">{u.username ?? '—'}</td>
              <td class="py-1.5 text-xs text-fg-muted">{fmtDate(u.created)}</td>
              <td class="py-1.5">
                <button
                  type="button"
                  onclick={() => adminRotate(u.ref)}
                  disabled={rotating === u.ref}
                  class="rounded border border-border px-2 py-0.5 text-xs hover:bg-surface disabled:cursor-not-allowed disabled:opacity-50"
                >
                  {rotating === u.ref ? 'Rotating…' : 'Mint keypair'}
                </button>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </section>
  {/if}

  {#if health.recent_rotations && health.recent_rotations.length > 0}
    <section>
      <h2 class="mb-2 text-lg font-semibold">Recent rotations</h2>
      <table class="w-full max-w-3xl text-sm">
        <thead class="text-xs uppercase text-fg-muted">
          <tr><th class="text-left">User</th><th class="text-left">Version</th><th class="text-left">When</th><th class="text-left">By</th></tr>
        </thead>
        <tbody>
          {#each health.recent_rotations as ev (`${ev.user_ref}-${ev.version}`)}
            <tr class="border-t border-border">
              <td class="py-1.5">#{ev.user_ref}</td>
              <td class="py-1.5">v{ev.version}</td>
              <td class="py-1.5 text-xs text-fg-muted">{fmtDate(ev.rotated_at)}</td>
              <td class="py-1.5 text-xs">{actorLabel(ev)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </section>
  {/if}
{/if}
