<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Password input with a reveal toggle (#692).
  //
  // ONE implementation for every password field in the app. Nine
  // hand-rolled toggles drift apart — this is the only one, and
  // TextField delegates to it so the auth forms and the admin forms
  // get identical behaviour.
  //
  // Three things this file is deliberately careful about:
  //
  // 1. `revealed` is component-local $state. It is never exported,
  //    never written to a store, never logged, and dies with the
  //    component instance. Reveal is transient per field per mount —
  //    navigating away and back re-hides. That is the point: a
  //    "remember I revealed this" feature is a credential-leak
  //    feature.
  //
  // 2. One <input> node, `type` swapped in place. Two nodes (one
  //    password, one text) would break password managers — the
  //    manager binds to the node it recognised at fill time — and
  //    would drop caret position on every toggle. Swapping `type`
  //    is what the managers expect and what they follow.
  //
  //    Svelte forbids `bind:value` when `type` is dynamic, so this
  //    syncs by hand off `oninput` + `onchange`. `onchange` is not
  //    redundant: it is the backstop for password managers that fill
  //    the field without firing a trusted `input` event.
  //
  // 3. The button is a real <button type="button">. Bare — without
  //    the explicit type — a button inside a <form> defaults to
  //    `submit`, so revealing your password would post the form.

  import { t } from '$stores/lang.svelte';

  interface Props {
    value: string;
    id?: string;
    name?: string;
    placeholder?: string;
    autocomplete?: HTMLInputElement['autocomplete'];
    required?: boolean;
    disabled?: boolean;
    minlength?: number;
    maxlength?: number;
    autofocus?: boolean;
    testId?: string;
    /** Styling for the <input>. Callers pass their surface's own
        classes so the compact admin forms stay compact; the reveal
        button's gutter (`pr-11`) is appended here, not by callers,
        so it can't be forgotten. */
    inputClass?: string;
    ariaInvalid?: boolean;
    ariaDescribedby?: string;
  }

  let {
    value = $bindable(''),
    id,
    name,
    placeholder,
    autocomplete,
    required = false,
    disabled = false,
    minlength,
    maxlength,
    autofocus = false,
    testId,
    inputClass = '',
    ariaInvalid,
    ariaDescribedby,
  }: Props = $props();

  let el: HTMLInputElement | undefined = $state();
  let revealed = $state(false);

  // Starts empty so the live region says nothing on mount; it only
  // ever speaks in response to a toggle the user performed.
  let announcement = $state('');

  function sync() {
    if (el) value = el.value;
  }

  function toggle() {
    revealed = !revealed;
    announcement = revealed ? t('common.password_shown') : t('common.password_hidden');
    // Keep the caret where the user left it — flipping `type` moves
    // it to the end in some engines, which is jarring mid-edit.
    const pos = el?.selectionStart ?? null;
    if (el && pos !== null) {
      queueMicrotask(() => {
        try {
          el?.setSelectionRange(pos, pos);
        } catch {
          // Some engines refuse setSelectionRange on type=password.
        }
      });
    }
  }
</script>

<div class="relative">
  <!-- svelte-ignore a11y_autofocus -->
  <input
    bind:this={el}
    type={revealed ? 'text' : 'password'}
    {id}
    {name}
    {value}
    {placeholder}
    {autocomplete}
    {required}
    {disabled}
    {minlength}
    {maxlength}
    {autofocus}
    data-testid={testId}
    aria-invalid={ariaInvalid}
    aria-describedby={ariaDescribedby}
    oninput={sync}
    onchange={sync}
    class="{inputClass} pr-11"
  />
  <button
    type="button"
    onclick={toggle}
    {disabled}
    tabindex={disabled ? -1 : 0}
    aria-label={revealed ? t('common.hide_password') : t('common.show_password')}
    aria-pressed={revealed}
    aria-controls={id}
    data-testid={testId ? `${testId}-reveal` : undefined}
    class="absolute inset-y-0 right-0 grid w-11 min-h-6 min-w-6 place-items-center rounded-r-md
           text-fg-muted hover:text-fg
           focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring
           disabled:opacity-50 disabled:cursor-not-allowed"
  >
    {#if revealed}
      <svg
        xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24"
        fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"
        stroke-linejoin="round" aria-hidden="true"
      >
        <path d="M10.733 5.076a10.744 10.744 0 0 1 11.205 6.575 1 1 0 0 1 0 .696 10.747 10.747 0 0 1-1.444 2.49" />
        <path d="M14.084 14.158a3 3 0 0 1-4.242-4.242" />
        <path d="M17.479 17.499a10.75 10.75 0 0 1-15.417-5.151 1 1 0 0 1 0-.696 10.75 10.75 0 0 1 4.446-5.143" />
        <path d="m2 2 20 20" />
      </svg>
    {:else}
      <svg
        xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24"
        fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"
        stroke-linejoin="round" aria-hidden="true"
      >
        <path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0" />
        <circle cx="12" cy="12" r="3" />
      </svg>
    {/if}
  </button>
</div>
<span class="sr-only" aria-live="polite">{announcement}</span>
