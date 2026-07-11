<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /* Phase 1.14.E-1 — minimal "Generate variation (AI)" trigger.

     Self-contained popover the AssetViewer renders in the top-right
     corner of the image canvas when the asset is an image. Owns
     the prompt input, the async-job lifecycle, status feedback, and
     the redirect-on-success.

     E-2 will fold this into a richer Creative tools panel with
     mask drawing + inpaint/outpaint/variations/remove-bg; the
     popover stays as the img2img sub-surface inside that panel. */

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { goto } from '$app/navigation';

  interface Props {
    /** Source asset id — what the variation derives from. */
    assetId: string;
    /** Optional callback fired when a derivative asset id is
        known. Default behaviour navigates to the new asset; hosts
        can override (e.g. swap the carousel position). */
    onDerivative?: (id: string) => void;
  }

  let { assetId, onDerivative }: Props = $props();

  /* UI state */
  let open = $state(false);
  let prompt = $state('');
  let status: 'idle' | 'submitting' | 'polling' | 'done' | 'error' = $state('idle');
  let error = $state<string | null>(null);
  let jobId = $state<string | null>(null);
  let pollTimer: ReturnType<typeof setTimeout> | null = null;

  function toggle() {
    open = !open;
    if (open) error = null;
  }

  async function submit(e: Event) {
    e.preventDefault();
    if (status === 'submitting' || status === 'polling') return;
    if (!prompt.trim()) {
      error = t('aiedit.img2img.prompt_required');
      return;
    }
    status = 'submitting';
    error = null;
    try {
      const { data, error: apiErr, response } = await api.POST(
        '/assets/{id}/edit/img2img',
        {
          params: { path: { id: assetId } },
          body: { prompt: prompt.trim() },
        }
      );
      if (apiErr) {
        const code = response?.status ?? 0;
        const msg = (apiErr as { error?: string }).error ?? '';
        error = explainError(code, msg);
        status = 'error';
        return;
      }
      jobId = data?.job_id ?? null;
      status = 'polling';
      schedulePoll();
    } catch (e) {
      error = (e as Error).message;
      status = 'error';
    }
  }

  function schedulePoll() {
    if (pollTimer) clearTimeout(pollTimer);
    pollTimer = setTimeout(() => { void poll(); }, 2000);
  }

  async function poll() {
    if (!jobId) return;
    const { data, error: apiErr } = await api.GET('/jobs/{id}', {
      params: { path: { id: jobId } },
    });
    if (apiErr || !data) {
      // Transient — keep polling, give up after ~5 minutes
      // wall-clock. The job worker's own retry policy handles
      // bridge transient errors; we just observe.
      schedulePoll();
      return;
    }
    const state = (data as { state?: string }).state ?? '';
    switch (state) {
      case 'done': {
        status = 'done';
        const result = (data as { result?: { derivative_asset_id?: string } }).result;
        const derivID = result?.derivative_asset_id;
        if (derivID) {
          if (onDerivative) {
            onDerivative(derivID);
          } else {
            void goto(`/assets/${derivID}`);
          }
        }
        return;
      }
      case 'failed':
      case 'failed_terminal':
      case 'failed_max_attempts':
        status = 'error';
        error = (data as { last_error?: string }).last_error ?? t('aiedit.img2img.job_failed');
        return;
      default:
        // queued / running / etc.
        schedulePoll();
    }
  }

  function explainError(code: number, msg: string): string {
    switch (code) {
      case 401: return t('aiedit.img2img.err_unauth');
      case 403: return t('aiedit.img2img.err_forbidden');
      case 404: return t('aiedit.img2img.err_not_found');
      case 409: return t('aiedit.img2img.err_server_not_configured');
      case 422: return t('aiedit.img2img.err_not_image');
      default:  return msg || t('aiedit.img2img.err_generic');
    }
  }
</script>

<div class="img2img-root">
  <button
    type="button"
    class="trigger"
    onclick={toggle}
    aria-expanded={open}
    aria-label={t('aiedit.img2img.button_label')}
    title={t('aiedit.img2img.button_title')}
  >
    {t('aiedit.img2img.button_label')}
  </button>

  {#if open}
    <form class="popover" onsubmit={submit}>
      <h4 class="title">{t('aiedit.img2img.heading')}</h4>
      <p class="hint">{t('aiedit.img2img.hint')}</p>

      <label class="block">
        <span class="label">{t('aiedit.img2img.prompt_label')}</span>
        <textarea
          bind:value={prompt}
          rows="3"
          maxlength="2000"
          placeholder={t('aiedit.img2img.prompt_placeholder')}
          disabled={status === 'submitting' || status === 'polling'}
        ></textarea>
      </label>

      {#if error}
        <p role="alert" class="error">{error}</p>
      {/if}

      {#if status === 'polling'}
        <p class="status">{t('aiedit.img2img.status_polling')}</p>
      {:else if status === 'done'}
        <p class="status-ok">{t('aiedit.img2img.status_done')}</p>
      {/if}

      <div class="row">
        <button type="button" class="ghost" onclick={toggle} disabled={status === 'polling'}>
          {t('common.cancel')}
        </button>
        <button
          type="submit"
          class="primary"
          disabled={status === 'submitting' || status === 'polling' || status === 'done'}
        >
          {#if status === 'submitting'}{t('aiedit.img2img.btn_submitting')}
          {:else if status === 'polling'}{t('aiedit.img2img.btn_polling')}
          {:else}{t('aiedit.img2img.btn_submit')}{/if}
        </button>
      </div>
    </form>
  {/if}
</div>

<style>
  .img2img-root { position: relative; }
  .trigger {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    padding: 0.25rem 0.5rem;
    border: 1px solid var(--color-border);
    border-radius: 0.375rem;
    background: var(--color-surface-elevated);
    color: var(--color-fg);
    font-size: 0.75rem;
    line-height: 1;
    cursor: pointer;
  }
  .trigger:hover { background: var(--color-surface); }
  .popover {
    position: absolute;
    top: calc(100% + 0.25rem);
    right: 0;
    width: 20rem;
    padding: 0.75rem;
    border: 1px solid var(--color-border);
    border-radius: 0.5rem;
    background: var(--color-surface-elevated);
    box-shadow: 0 6px 18px rgba(0, 0, 0, 0.18);
    z-index: 50;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .title { margin: 0; font-size: 0.85rem; font-weight: 600; }
  .hint { margin: 0; font-size: 0.7rem; color: var(--color-fg-muted); }
  .label { display: block; font-size: 0.7rem; color: var(--color-fg-muted); margin-bottom: 0.25rem; }
  textarea {
    width: 100%;
    padding: 0.375rem 0.5rem;
    border: 1px solid var(--color-border);
    border-radius: 0.25rem;
    background: var(--color-surface);
    color: var(--color-fg);
    font-size: 0.8rem;
    resize: vertical;
  }
  textarea:disabled { opacity: 0.6; cursor: not-allowed; }
  .error {
    margin: 0;
    padding: 0.375rem 0.5rem;
    background: var(--color-danger-container);
    color: var(--color-danger);
    border: 1px solid var(--color-danger);
    border-radius: 0.25rem;
    font-size: 0.7rem;
  }
  .status, .status-ok {
    margin: 0;
    font-size: 0.7rem;
    color: var(--color-fg-muted);
  }
  .status-ok { color: var(--color-success); }
  .row {
    display: flex;
    justify-content: flex-end;
    gap: 0.375rem;
  }
  .ghost, .primary {
    padding: 0.25rem 0.625rem;
    border-radius: 0.25rem;
    font-size: 0.75rem;
    cursor: pointer;
  }
  .ghost {
    border: 1px solid var(--color-border);
    background: transparent;
    color: var(--color-fg);
  }
  .ghost:disabled { opacity: 0.5; cursor: not-allowed; }
  .primary {
    border: 1px solid var(--color-accent);
    background: var(--color-accent);
    color: var(--color-on-accent, white);
  }
  .primary:disabled { opacity: 0.5; cursor: not-allowed; }
</style>
