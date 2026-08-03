<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Labelled text input with optional helper / error text. Two-way
  // bound via $bindable so callers can do <TextField bind:value=... />.
  //
  // The resting border is `border-border-strong`, NOT `border-border`
  // (#594). On a form control the border is the whole affordance — it is
  // the only thing that says "you can type here" — so it is graphical
  // information under WCAG 1.4.11 and owes 3:1. `border-border` is the
  // divider role and measures 1.28:1 / 1.38:1; it is correct on a card
  // edge or a table rule and wrong here. Any new control follows this,
  // and app.css states the split.
  //
  // type="password" delegates the control to PasswordInput, which
  // adds the reveal toggle (#692). The input styling is passed down
  // rather than duplicated, so the revealed and plain variants are
  // the same box.

  import PasswordInput from './PasswordInput.svelte';

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
    /** `data-testid` for the underlying <input>. Pin one when the
        field is a stable target for Playwright tests. */
    testId?: string;
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
    testId,
  }: Props = $props();

  // Stable id for label/aria-describedby plumbing.
  const fieldId = $derived(id ?? `tf-${name ?? Math.random().toString(36).slice(2, 9)}`);
  const helperId = $derived(`${fieldId}-help`);
  const hasError = $derived(error !== null && error !== undefined && error !== '');
  const describedBy = $derived(helper || hasError ? helperId : undefined);
  const inputClass = $derived(
    `block w-full rounded-md border bg-surface-elevated px-3 py-2 text-sm text-fg
     placeholder:text-fg-muted/60
     focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-1 focus-visible:ring-offset-surface focus-visible:ring-ring
     disabled:opacity-50 disabled:cursor-not-allowed
     ${hasError ? 'border-danger' : 'border-border-strong'}`,
  );
</script>

<div class="space-y-1.5">
  <label for={fieldId} class="block text-sm font-medium text-fg">
    {label}
    {#if required}<span class="text-accent" aria-hidden="true">*</span>{/if}
  </label>
  {#if type === 'password'}
    <PasswordInput
      id={fieldId}
      {name}
      {placeholder}
      {autocomplete}
      {required}
      {disabled}
      {minlength}
      {maxlength}
      {autofocus}
      {testId}
      {inputClass}
      ariaInvalid={hasError}
      ariaDescribedby={describedBy}
      bind:value
    />
  {:else}
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
      data-testid={testId}
      aria-invalid={hasError}
      aria-describedby={describedBy}
      bind:value
      class={inputClass}
    />
  {/if}
  {#if hasError}
    <p id={helperId} class="text-xs text-danger" role="alert">{error}</p>
  {:else if helper}
    <p id={helperId} class="text-xs text-fg-muted">{helper}</p>
  {/if}
</div>
