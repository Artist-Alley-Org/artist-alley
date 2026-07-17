<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Inline alert / status message — used for form-level errors and
  // success notices. role="alert" announces it to screen readers.

  type Tone = 'error' | 'info' | 'success';

  interface Props {
    tone?: Tone;
    children?: import('svelte').Snippet;
  }

  let { tone = 'error', children }: Props = $props();

  const tones: Record<Tone, string> = {
    error: 'border-danger/40 bg-danger-container text-on-danger-container',
    // Informational tone uses the steel secondary set (#295) so info
    // reads as a distinct semantic — a deliberate "note", not a plain
    // neutral box — and stays clearly apart from danger (red) and
    // success (green). Mirrors the error/success container pattern.
    info: 'border-secondary/40 bg-secondary-container text-on-secondary-container',
    success: 'border-success/40 bg-success-container text-on-success-container',
  };
</script>

<div role="alert" class="rounded-md border px-3 py-2 text-sm {tones[tone]}">
  {@render children?.()}
</div>
