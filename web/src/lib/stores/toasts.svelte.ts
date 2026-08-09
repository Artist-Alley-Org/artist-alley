// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The app's transient-feedback queue (#981).
//
// WHY IT DID NOT EXIST BEFORE. Every surface that needed to say "that
// worked" or "that failed" either used `alert()` (PostHost's recreate-
// previews still carries the comment "No toast utility yet; alert is
// the simplest visible feedback until a proper notifications system
// lands") or painted a one-off banner into its own page. Neither works
// for a DELETE: the thing the message is about has just left the view,
// and on the viewer surface the page that would host a banner is a
// dialog that may be closing. So #981 needed one, and one is all this
// is — a list, a push, a dismiss.
//
// It is deliberately NOT the notification system. Nothing here is
// persisted, none of it survives a reload, and it has no relationship
// to `/account/notifications`. It is the acknowledgement of an action
// the user just took, addressed to the user who took it.
//
// ## Why an action, and why a link
//
// A delete toast carries both because they answer the two questions a
// delete raises. "Undo" answers "that was a mistake" in one click —
// cheap, because restore already exists as an endpoint and the deleter
// always satisfies auth.CanRestoreDeleted, so the undo can never be
// offered to someone the server would refuse. "View trash" answers
// "where did it go", which is the claim the confirm dialog just made
// and which would otherwise go unevidenced.
//
// ## Timing
//
// DEFAULT_TTL_MS is long by toast standards on purpose: an Undo the
// user cannot reach in time is worse than no Undo, and the alternative
// (never auto-dismissing) leaves stale acknowledgements stacked over
// the page. Hovering pauses the clock — see ToastHost — so a toast
// being read is not a toast being taken away.

import { browser } from '$app/environment';

export interface ToastAction {
  label: string;
  /** Runs on click. The toast dismisses itself first, so a slow action
   *  does not leave a dead button on screen. */
  run: () => void | Promise<void>;
}

export interface Toast {
  id: number;
  message: string;
  /** Optional trailing link ("View trash"). */
  href?: string;
  linkLabel?: string;
  /** Optional inline action ("Undo"). */
  action?: ToastAction;
  tone: 'info' | 'error';
  ttlMs: number;
}

export type ToastInput = Omit<Toast, 'id' | 'tone' | 'ttlMs'> &
  Partial<Pick<Toast, 'tone' | 'ttlMs'>>;

const DEFAULT_TTL_MS = 9000;

/** At most this many at once — older ones are dropped from the head.
 *  A queue that can grow without bound is a queue that can cover the
 *  control the user is trying to reach. */
const MAX_VISIBLE = 3;

class ToastStore {
  items = $state<Toast[]>([]);

  #nextId = 1;

  /** Show a toast. Returns its id so a caller can dismiss it early. */
  push(input: ToastInput): number {
    const id = this.#nextId++;
    const toast: Toast = {
      id,
      message: input.message,
      href: input.href,
      linkLabel: input.linkLabel,
      action: input.action,
      tone: input.tone ?? 'info',
      ttlMs: input.ttlMs ?? DEFAULT_TTL_MS,
    };
    const next = [...this.items, toast];
    this.items = next.length > MAX_VISIBLE ? next.slice(next.length - MAX_VISIBLE) : next;
    return id;
  }

  dismiss(id: number): void {
    this.items = this.items.filter((t) => t.id !== id);
  }

  clear(): void {
    this.items = [];
  }
}

export const toasts = new ToastStore();

/** Server-side rendering has no user to acknowledge anything to, and a
 *  queue that accumulated across an SSR pass would leak between
 *  requests. Callers that might run in both places check this. */
export const toastsEnabled = browser;
