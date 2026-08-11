// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/** Move an overlay out of whatever box it was declared in.
 *
 *  `position: fixed` is relative to the VIEWPORT only while no ancestor
 *  establishes a containing block — and `contain`, `container-type`,
 *  `transform` and `filter` all do. Driven in a browser from a
 *  restricted grid tile (#881), Modal rendered inside the tile and was
 *  clipped by it: CardRestricted's plate is `container-type: size`,
 *  which is exactly one of those. Every caller that ever mounts an
 *  overlay from inside a card, a viewer pane or a transformed surface
 *  hits the same thing, so the fix belongs here rather than at each
 *  call site.
 *
 *  The target is an OPEN native `<dialog>` if there is one, and
 *  `document.body` otherwise. A `<dialog>` opened with showModal()
 *  lives in the browser's top layer, and anything appended to the body
 *  renders beneath it and swallows every click — which is the other
 *  half of the same bug, seen from the maximized asset viewer
 *  (AssetPlaylist calls showModal() when maximized and show() when
 *  windowed).
 *
 *  ## Which dialog — `scope`, and why the default is not enough
 *
 *  `scope: 'ancestor'` (the default) resolves `closest('dialog[open]')`
 *  from where the node was DECLARED. That is right for a modal, which
 *  is declared inside the surface that raised it.
 *
 *  It is wrong for anything declared at the root layout, and silently
 *  so: `closest()` walks ANCESTORS, and a layout-level singleton has no
 *  dialog above it, so it always resolves to the body. Driven in a
 *  browser (#981) the delete toast was in the DOM, correct in every
 *  respect, and invisible — the maximized viewer's top layer covered
 *  it. The toast's host is not on its ancestor chain; it is whatever
 *  modal happens to be open at the moment the toast is raised.
 *
 *  So `scope: 'document'` asks the document instead, via the `:modal`
 *  pseudo-class — the one selector that distinguishes a dialog in the
 *  top layer from a `show()`n one that is merely open. A non-modal
 *  dialog is deliberately NOT matched: it obeys z-index like anything
 *  else, so a body-level toast already sits above it and re-parenting
 *  would tie the toast's life to a surface that does not own it.
 *
 *  Resolved before the move, while the node is still where it was
 *  declared.
 *
 *  WHY IT IS A MODULE (#981). It began inside Modal.svelte, whose own
 *  header says a second surface needing the same shape is the trigger
 *  to share rather than copy. #981's toast is that surface — it is
 *  raised BY the asset viewer, so a body-level toast is exactly the
 *  node the top layer hides, and a second copy of this reasoning is a
 *  second place for it to rot.
 *
 *  ## Re-homing when the host dialog goes away
 *
 *  A node parented to a `<dialog>` is removed from the page when that
 *  dialog closes. For a modal that is correct — the modal belongs to
 *  the surface that raised it. For a toast it is not: deleting the last
 *  asset in a playlist closes the viewer, and the confirmation of that
 *  very delete would vanish with it. So when the host is a dialog we
 *  fall back to the body, keeping the node alive exactly as long as its
 *  owner intends. Callers that want the modal behaviour pass
 *  `rehome: false`.
 *
 *  A dialog goes away in TWO ways and only one of them is an event.
 *  `close()` fires `close`. Being REMOVED from the document — which is
 *  what happens when the component that declared the dialog is
 *  destroyed by a navigation — fires nothing at all, and the portalled
 *  node is carried out of the page inside the detached subtree. It is
 *  still parented, still styled, still in the store that raised it, and
 *  on no screen.
 *
 *  #991 is that second way. The delete toast on /assets/{id} was raised
 *  after `await onClose()` precisely so no dialog would be left to adopt
 *  it — but the standalone close policy is `history.back()` (see
 *  $lib/util/closeToOrigin), and a history entry cannot be awaited. The
 *  await resolved on the spot, the viewer's dialog was still open and
 *  still owned the top layer, the toast was parented into it, and the
 *  popstate that landed a frame later took the dialog and the
 *  acknowledgement with it. Every delete worked; not one said so.
 *
 *  Ordering cannot fix that, which is why it belongs here rather than at
 *  the call site: a caller cannot promise the dialog is gone when it has
 *  no way to wait for the navigation that removes it. So the node's
 *  survival is made independent of how its host ends. Detachment is
 *  watched for with a MutationObserver because there is no event for it;
 *  it runs only while a rehoming node actually lives inside a dialog
 *  (toasts — a handful per session, seconds each), and its callback is
 *  one `isConnected` read.
 */
/** The topmost dialog in the browser's top layer, or null.
 *
 *  `:modal` is what makes this answerable at all — there is no public
 *  property saying whether a dialog was opened with showModal(), and
 *  `[open]` is true for both. Wrapped because an engine that does not
 *  know the pseudo-class THROWS on the whole selector rather than
 *  matching nothing, and a missing toast is a better failure than a
 *  page that stops rendering.
 *
 *  Last match wins: the top layer is a stack, and the most recently
 *  opened modal is the one covering everything else. Document order is
 *  only an approximation of open order, but every surface in this app
 *  opens at most one modal dialog at a time, so the approximation has
 *  no case to be wrong in. */
function topLayerHost(): HTMLElement | null {
  try {
    const modals = document.querySelectorAll<HTMLDialogElement>('dialog:modal');
    return modals.length > 0 ? modals[modals.length - 1] : null;
  } catch {
    return null;
  }
}

export function portal(
  node: HTMLElement,
  opts: { rehome?: boolean; scope?: 'ancestor' | 'document' } = {},
) {
  const rehome = opts.rehome ?? true;
  const host =
    (opts.scope === 'document' ? topLayerHost() : node.closest('dialog[open]')) ?? document.body;
  host.appendChild(node);

  let onClose: (() => void) | null = null;
  let detachWatch: MutationObserver | null = null;

  if (rehome && host instanceof HTMLDialogElement) {
    const toBody = () => {
      // Only if we are still where we were put — a caller may have
      // moved on, and stealing the node back would be worse.
      if (node.parentNode === host) document.body.appendChild(node);
    };

    onClose = toBody;
    host.addEventListener('close', onClose);

    // The other way a dialog ends: someone removes it. No event, so the
    // document is watched instead. `isConnected` is the question that
    // matters — not "was this particular removal ours", because the
    // dialog can leave inside any ancestor's removal.
    if (typeof MutationObserver !== 'undefined') {
      detachWatch = new MutationObserver(() => {
        if (host.isConnected) return;
        toBody();
        detachWatch?.disconnect();
        detachWatch = null;
      });
      detachWatch.observe(document.documentElement, { childList: true, subtree: true });
    }
  }

  return {
    destroy() {
      detachWatch?.disconnect();
      detachWatch = null;
      if (onClose && host instanceof HTMLDialogElement) {
        host.removeEventListener('close', onClose);
      }
      node.remove();
    },
  };
}
