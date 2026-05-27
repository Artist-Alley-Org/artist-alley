<script lang="ts">
  // Labelled text input with optional helper / error text. Two-way
  // bound via $bindable so callers can do <TextField bind:value=... />.

  interface Props {
    label: string;
    value: string;
    type?: 'text' | 'email' | 'password' | 'url';
    name?: string;
    placeholder?: string;
    // Match the HTMLInputElement DOM type so callers can pass any
    // valid autofill token without a cast. (TS rejects bare `string`
    // because the underlying attribute type is the AutoFill union.)
    autocomplete?: HTMLInputElement['autocomplete'];
    required?: boolean;
    disabled?: boolean;
    error?: string | null;
    helper?: string;
    autofocus?: boolean;
    minlength?: number;
    maxlength?: number;
    id?: string;
  }

  let {
    label,
    value = $bindable(''),
    type = 'text',
    name,
    placeholder,
    autocomplete,
    required = false,
    disabled = false,
    error = null,
    helper,
    autofocus = false,
    minlength,
    maxlength,
    id,
  }: Props = $props();

  // Stable id for label/aria-describedby plumbing.
  const fieldId = $derived(id ?? `tf-${name ?? Math.random().toString(36).slice(2, 9)}`);
  const helperId = $derived(`${fieldId}-help`);
  const hasError = $derived(error !== null && error !== undefined && error !== '');
</script>

<div class="space-y-1.5">
  <label for={fieldId} class="block text-sm font-medium text-fg">
    {label}
    {#if required}<span class="text-accent" aria-hidden="true">*</span>{/if}
  </label>
  <!-- svelte-ignore a11y_autofocus -->
  <input
    id={fieldId}
    {type}
    {name}
    {placeholder}
    {autocomplete}
    {required}
    {disabled}
    {minlength}
    {maxlength}
    {autofocus}
    aria-invalid={hasError}
    aria-describedby={helper || hasError ? helperId : undefined}
    bind:value
    class="block w-full rounded-md border bg-surface-elevated px-3 py-2 text-sm text-fg
           placeholder:text-fg-muted/60
           focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-1 focus-visible:ring-offset-surface focus-visible:ring-ring
           disabled:opacity-50 disabled:cursor-not-allowed
           {hasError ? 'border-danger' : 'border-border'}"
  />
  {#if hasError}
    <p id={helperId} class="text-xs text-danger" role="alert">{error}</p>
  {:else if helper}
    <p id={helperId} class="text-xs text-fg-muted">{helper}</p>
  {/if}
</div>
