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
    info: 'border-border bg-surface-elevated text-fg-muted',
    success: 'border-success/40 bg-success-container text-on-success-container',
  };
</script>

<div role="alert" class="rounded-md border px-3 py-2 text-sm {tones[tone]}">
  {@render children?.()}
</div>
