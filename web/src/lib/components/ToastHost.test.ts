// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #991 — the delete toast never reached the screen, and every existing
// test stayed green through it.
//
// That is the part worth fixing first. #981 built the queue and #989
// wired a second surface into it, and both were covered by assertions
// on `toasts.items` — a list the store owns and updates whether or not
// anything is rendered. The store was never the broken half. So these
// tests only ever ask the document: is there a node with
// `data-testid="toast"` in it, and is it still there afterwards.
//
// The failure was not "nothing rendered". A toast raised while the
// asset viewer's <dialog> owned the top layer was parented INTO that
// dialog — correct, and the only placement that renders above it — and
// the navigation that follows a delete then DESTROYED the component
// that declared the dialog. Removing an element fires no `close`, so
// nothing told the toast its host had gone; it left the page inside a
// detached subtree, still parented and still in the store. See
// $lib/portal.

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import ToastHost from './ToastHost.svelte';
import { toasts } from '$stores/toasts.svelte';

// happy-dom implements <dialog>.showModal() but not the `:modal`
// pseudo-class, and that selector is the ONE thing $lib/portal uses to
// find the top layer. Left alone, every toast in this file lands on the
// body, the dialog-hosted path is never taken, and the bug under test
// cannot happen — a suite that passes for the same reason the old one
// did. So the missing pseudo-class is filled in: showModal() marks the
// element, and `dialog:modal` is rewritten to that marker.
const MODAL_ATTR = 'data-test-modal';
let realShowModal: typeof HTMLDialogElement.prototype.showModal;
let realClose: typeof HTMLDialogElement.prototype.close;
let realQuerySelectorAll: typeof document.querySelectorAll;

function installModalShim() {
  realShowModal = HTMLDialogElement.prototype.showModal;
  realClose = HTMLDialogElement.prototype.close;
  realQuerySelectorAll = document.querySelectorAll;

  HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
    this.setAttribute(MODAL_ATTR, '');
    realShowModal.call(this);
  };
  HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement, returnValue?: string) {
    this.removeAttribute(MODAL_ATTR);
    realClose.call(this, returnValue as string);
  };
  document.querySelectorAll = function patched(this: Document, selector: string) {
    return realQuerySelectorAll.call(
      this,
      selector === 'dialog:modal' ? `dialog[${MODAL_ATTR}]` : selector,
    );
  } as typeof document.querySelectorAll;
}

function removeModalShim() {
  HTMLDialogElement.prototype.showModal = realShowModal;
  HTMLDialogElement.prototype.close = realClose;
  document.querySelectorAll = realQuerySelectorAll;
}

/** The shape AssetPlaylist / PostHost raise after a successful delete:
 *  a message, an inline Undo and a link to the trash. */
function pushDeleteToast() {
  return toasts.push({
    message: 'Asset deleted.',
    href: '/account/trash',
    linkLabel: 'View trash',
    action: { label: 'Undo', run: () => {} },
  });
}

function toastNodes() {
  return realQuerySelectorAll.call(document, '[data-testid="toast"]');
}

/** Open a modal dialog the way AssetPlaylist does when maximized. */
function openViewerDialog() {
  const dialog = document.createElement('dialog');
  dialog.setAttribute('data-testid', 'asset-playlist');
  document.body.appendChild(dialog);
  dialog.showModal();
  return dialog;
}

/** Let Svelte render, then let the MutationObserver batch land. */
async function settle() {
  await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

describe('ToastHost — a raised toast is on the screen', () => {
  beforeEach(() => {
    installModalShim();
    toasts.clear();
  });

  afterEach(() => {
    toasts.clear();
    document.querySelectorAll('dialog').forEach((d) => d.remove());
    removeModalShim();
  });

  it('puts a delete toast in the document, with Undo and View trash', async () => {
    render(ToastHost);
    pushDeleteToast();
    await settle();

    const nodes = toastNodes();
    expect(nodes.length).toBe(1);
    const toast = nodes[0] as HTMLElement;
    expect(toast.isConnected).toBe(true);
    expect(toast.textContent).toContain('Asset deleted.');
    expect(toast.querySelector('[data-testid="toast-action"]')?.textContent?.trim()).toBe('Undo');
    expect(toast.querySelector('[data-testid="toast-link"]')?.textContent?.trim()).toBe(
      'View trash',
    );
  });

  // #991. The delete that raised the toast also navigates away, and the
  // navigation destroys the component that declared the viewer dialog —
  // so the dialog is REMOVED rather than closed. Before the fix the
  // toast went with it and the user saw nothing.
  it('survives its host dialog being removed from the document', async () => {
    render(ToastHost);
    const dialog = openViewerDialog();

    pushDeleteToast();
    await settle();

    // Precondition: it really did land in the top layer, not the body.
    // Without this the test could pass for the wrong reason.
    expect(toastNodes().length).toBe(1);
    expect((toastNodes()[0] as HTMLElement).parentElement).toBe(dialog);

    dialog.remove();
    await settle();

    expect(toastNodes().length).toBe(1);
    const toast = toastNodes()[0] as HTMLElement;
    expect(toast.isConnected).toBe(true);
    expect(toast.textContent).toContain('Asset deleted.');
    expect(toast.querySelector('[data-testid="toast-action"]')?.textContent?.trim()).toBe('Undo');
  });

  // The sibling case, which #981 already handled via the `close` event.
  // Kept so the two exits stay covered by the same file.
  it('survives its host dialog being closed', async () => {
    render(ToastHost);
    const dialog = openViewerDialog();

    pushDeleteToast();
    await settle();
    expect((toastNodes()[0] as HTMLElement).parentElement).toBe(dialog);

    dialog.close();
    await settle();

    expect(toastNodes().length).toBe(1);
    expect((toastNodes()[0] as HTMLElement).isConnected).toBe(true);
  });
});
