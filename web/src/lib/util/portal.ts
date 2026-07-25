// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Svelte action that relocates a node to a target (default document.body)
// so it escapes any `overflow: hidden` / `transform` ancestor that would
// otherwise clip or re-anchor it.
//
// The card overflow menu (#578) needs this: a grid card is
// `overflow-hidden` (letterboxed thumb) AND gets `hover:scale` (a
// transform, which becomes the containing block for `position: fixed`),
// so a dropdown rendered inline would be clipped by the first and
// mis-positioned by the second. Portaled to <body> and positioned from
// the trigger's viewport rect, the panel is free of both.

export function portal(node: HTMLElement, target: HTMLElement | string = document.body) {
  function resolve(t: HTMLElement | string): HTMLElement {
    return typeof t === 'string' ? (document.querySelector(t) as HTMLElement) ?? document.body : t;
  }
  let host = resolve(target);
  host.appendChild(node);
  return {
    update(next: HTMLElement | string) {
      host = resolve(next);
      host.appendChild(node);
    },
    destroy() {
      node.parentNode?.removeChild(node);
    },
  };
}
