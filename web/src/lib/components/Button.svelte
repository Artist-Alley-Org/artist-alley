<script lang="ts">
  // Hand-rolled button primitive. Three variants cover the surfaces
  // we need in 1.13.C: primary actions (sign in, complete setup),
  // secondary actions (cancel, sign out), and link-style affordances
  // ("don't have an account?" type links).

  type Variant = 'primary' | 'secondary' | 'ghost';

  interface Props {
    variant?: Variant;
    type?: 'button' | 'submit' | 'reset';
    disabled?: boolean;
    loading?: boolean;
    onclick?: (e: MouseEvent) => void;
    children?: import('svelte').Snippet;
    class?: string;
    fullWidth?: boolean;
    /** `data-testid` for the underlying <button>. Use a value from
        helpers/testids.ts when the button is a Playwright target. */
    testId?: string;
  }

  let {
    variant = 'primary',
    type = 'button',
    disabled = false,
    loading = false,
    onclick,
    children,
    class: extra = '',
    fullWidth = false,
    testId,
  }: Props = $props();

  const base =
    'inline-flex items-center justify-center gap-2 rounded-md px-4 py-2 text-sm font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:ring-offset-surface focus-visible:ring-accent';

  const variants: Record<Variant, string> = {
    primary: 'bg-accent text-accent-fg hover:opacity-90',
    secondary: 'border border-border bg-surface-elevated text-fg hover:bg-surface',
    ghost: 'text-fg-muted hover:text-fg',
  };
</script>

<button
  {type}
  disabled={disabled || loading}
  {onclick}
  data-testid={testId}
  class="{base} {variants[variant]} {fullWidth ? 'w-full' : ''} {extra}"
>
  {#if loading}
    <span
      class="inline-block h-4 w-4 animate-spin rounded-full border-2 border-current border-t-transparent"
      aria-hidden="true"
    ></span>
  {/if}
  {@render children?.()}
</button>
