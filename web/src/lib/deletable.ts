// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The three soft-deletable kinds, in one place (#981).
//
// Deleting an asset, a post and a collection are three endpoints with
// three different server-side side effects (assets unpin storage and
// fan out cache invalidation; collections emit a federation Tombstone
// inside the delete transaction; posts do neither). From the CLIENT
// they are one shape: same body, same 204, same trash, same restore.
// This module is that shape — the same argument `app/internal/trash`
// makes for serving one listing across all three.
//
// Three surfaces call it: the viewer's "Delete asset", the post
// kebab's "Delete post", and the collection page's "Delete
// collection". Before this issue all three were stubs — the viewer and
// the kebab called `stubAction()`, and the collection item was
// hardcoded `disabled` behind a `delete_soon` tooltip — so the whole
// delete/restore arc (#930, #936, #920/#935, #937) was reachable only
// through the API.

import { api } from '$api/client';
import { auth } from '$stores/auth.svelte';

export type DeletableKind = 'asset' | 'post' | 'collection';

/** The GLOBAL capability that lets a non-owner mutate — and therefore
 *  delete — each kind. Mirrors canMutateAsset / canMutatePost /
 *  canMutateCollection in the Go handlers.
 *
 *  ⚠️ GLOBAL ONLY, AND THAT IS A KNOWN CEILING. `auth.caps` is the
 *  caller's global capability set; the server additionally accepts a
 *  TEAM-SCOPED grant (`user_capability_grants.team_id`, pre-expanded
 *  through team_closure — see canMutateAsset), and the client cannot
 *  see those. So a team lead holding a scoped `assets.admin` over a
 *  colleague's asset will NOT be offered the delete item here even
 *  though DELETE /assets/{id} would accept them.
 *
 *  This is the same limitation, for the same reason, that #549
 *  documented for the status field, and it is deliberately not worked
 *  around: resolving team scope client-side would mean shipping the
 *  membership closure to the browser and reimplementing the resolver
 *  against it — a second copy of a security rule, which is the drift
 *  that becomes the bug. Erring towards hiding a usable button is the
 *  safe direction; the opposite (#938's lesson) is a button that 403s.
 *  The server gates regardless of what we draw. */
const ADMIN_CAP: Record<DeletableKind, string> = {
  asset: 'assets.admin',
  post: 'posts.admin',
  collection: 'collections.admin',
};

/** Is the signed-in user the owner of a thing owned by `ownerRef`?
 *
 *  Ref 0 is the anonymous sentinel and is never a principal on either
 *  side — the same guard canMutateAsset states server-side. A null or
 *  undefined owner (assets.owner_user_ref is nullable) matches nobody. */
export function isOwnedByMe(ownerRef: number | null | undefined): boolean {
  const me = auth.user?.ref;
  if (!me || me === 0) return false;
  if (ownerRef === null || ownerRef === undefined || ownerRef === 0) return false;
  return ownerRef === me;
}

/** May the signed-in user delete this? Owner, or a global mutation
 *  capability (auth.can already answers true for system.admin).
 *
 *  Returns false when the caps could not be resolved at all
 *  (`capsUnavailable`), which auth.can already handles — a surface
 *  that cannot establish authority must not assume it. */
export function canDelete(
  kind: DeletableKind,
  ownerRef: number | null | undefined,
): boolean {
  if (!auth.user) return false;
  return isOwnedByMe(ownerRef) || auth.can(ADMIN_CAP[kind]);
}

/** Whether the delete dialog should offer the reason box.
 *
 *  Only when deleting something you do NOT own. Self-delete skips it:
 *  the reason exists so the OWNER can learn why their work was removed
 *  (#931's appeal flow reads `deleted_reason`), and asking someone to
 *  explain a deletion to themselves is friction bought for nothing. */
export function shouldAskReason(ownerRef: number | null | undefined): boolean {
  return !isOwnedByMe(ownerRef);
}

const DELETE_PATH = {
  asset: '/assets/{id}',
  post: '/posts/{id}',
  collection: '/collections/{id}',
} as const;

// The restore endpoints still sit under /admin — that path is
// historical (#936 opened them to the deleter); it is not a claim that
// restoring needs admin rights. /account/trash's page says the same.
const RESTORE_PATH = {
  asset: '/admin/assets/{id}/restore',
  post: '/admin/posts/{id}/restore',
  collection: '/admin/collections/{id}/restore',
} as const;

/** The server's cap on `deleted_reason` (softDeleteReasonMaxLen). The
 *  input enforces it too so a paste is truncated rather than 400'd. */
export const REASON_MAX_LEN = 500;

/** Soft-delete one thing. Resolves to null on success, or the server's
 *  error message.
 *
 *  `reason` is sent only when non-empty. An empty body is the shape
 *  every one of these endpoints already accepts, and posting
 *  `{"reason": ""}` would write an empty string into `deleted_reason`
 *  where NULL means "none given". */
export async function deleteEntity(
  kind: DeletableKind,
  id: string,
  reason?: string,
): Promise<string | null> {
  const trimmed = (reason ?? '').trim();
  const body = trimmed ? { reason: trimmed.slice(0, REASON_MAX_LEN) } : {};
  try {
    const { error } = await api.DELETE(DELETE_PATH[kind] as '/assets/{id}', {
      params: { path: { id } },
      body: body as never,
    });
    if (error) return (error as { error?: string }).error ?? 'delete failed';
    return null;
  } catch (e) {
    return e instanceof Error ? e.message : 'delete failed';
  }
}

/** Undo a soft delete. Resolves to null on success.
 *
 *  Safe to offer straight after `deleteEntity` succeeded: the deleter
 *  is by construction someone auth.CanRestoreDeleted says yes to, so
 *  an Undo raised by the delete we just performed can never be an
 *  affordance the server refuses. */
export async function restoreEntity(
  kind: DeletableKind,
  id: string,
): Promise<string | null> {
  try {
    const { error } = await api.POST(
      RESTORE_PATH[kind] as '/admin/assets/{id}/restore',
      { params: { path: { id } } },
    );
    if (error) return (error as { error?: string }).error ?? 'restore failed';
    return null;
  } catch (e) {
    return e instanceof Error ? e.message : 'restore failed';
  }
}
