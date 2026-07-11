<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/security — federation keypair management (Phase 1.22.I-h).
  //
  // One action surface today: rotate my federation encryption keypair.
  // Generates a new X25519 keypair, retires the previous key into the
  // grace window (default 30 days), and surfaces the new version +
  // base64 public bytes so the user can verify the change took effect.
  //
  // Future I-h-followup will add: list-my-keys table (current +
  // retained-grace + expiry preview), revoke-this-session, and
  // hardware-token enrolment if/when that lands.

  import { api } from '$api/client';

  type RotationResult = {
    user_ref: number;
    new_version: number;
    previous_version?: number;
    new_public_key_b64: string;
    algorithm: string;
    retained_until_days?: number;
  };

  let submitting = $state(false);
  let error = $state<string | null>(null);
  let result = $state<RotationResult | null>(null);
  let confirming = $state(false);

  async function rotate() {
    if (submitting) return;
    submitting = true;
    error = null;
    try {
      const r = await api.POST('/account/security/rotate-federation-keys', {});
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Rotation failed.';
        return;
      }
      result = r.data as RotationResult;
      confirming = false;
    } finally {
      submitting = false;
    }
  }
</script>

<svelte:head><title>Security — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">Federation encryption keys</h2>
<p class="mb-4 max-w-2xl text-sm text-fg-muted">
  Your account's X25519 keypair seals federated activities sent to peers
  who advertise nacl-box encryption. Rotating mints a fresh keypair and
  retires the previous one into a 30-day grace window — long enough for
  in-flight envelopes to land but short enough to limit the blast radius
  of a compromised key. Recommended after suspected credential exposure.
</p>

{#if result}
  <div class="mb-4 max-w-2xl space-y-2 rounded-lg border border-success/40 bg-success/10 p-4">
    <p class="text-sm font-semibold text-success">Rotation complete.</p>
    <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-xs text-fg-muted">
      <dt>New version</dt><dd class="text-fg">v{result.new_version}</dd>
      {#if result.previous_version && result.previous_version > 0}
        <dt>Previous version</dt><dd class="text-fg">v{result.previous_version} (retained {result.retained_until_days ?? 30} days)</dd>
      {/if}
      <dt>Algorithm</dt><dd class="text-fg">{result.algorithm}</dd>
      <dt>New public key</dt>
      <dd class="break-all font-mono text-fg">{result.new_public_key_b64}</dd>
    </dl>
  </div>
{/if}

<div class="max-w-2xl rounded-lg border border-border bg-surface-elevated p-4">
  {#if !confirming}
    <button
      type="button"
      onclick={() => (confirming = true)}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white hover:bg-accent/90"
    >
      Rotate federation keys
    </button>
  {:else}
    <p class="mb-3 text-sm text-fg">
      Are you sure? The new keypair takes effect immediately. Peers
      will pick up the new public key on their next inbound activity
      from you.
    </p>
    <div class="flex gap-2">
      <button
        type="button"
        onclick={rotate}
        disabled={submitting}
        class="rounded-md bg-danger px-4 py-1.5 text-sm font-medium text-white hover:bg-danger/90 disabled:cursor-not-allowed disabled:bg-danger/40"
      >
        {submitting ? 'Rotating…' : 'Yes, rotate now'}
      </button>
      <button
        type="button"
        onclick={() => (confirming = false)}
        disabled={submitting}
        class="rounded-md border border-border px-4 py-1.5 text-sm hover:bg-surface"
      >
        Cancel
      </button>
    </div>
  {/if}

  {#if error}
    <p role="alert" class="mt-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {/if}
</div>
