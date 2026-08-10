// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Turning an audit row into a sentence (#600).
//
// The API sends the raw dotted `event_type` and a `role`, never a
// pre-rendered phrase — a server-side sentence would be one language
// baked into a JSON contract. So the wording lives here, and this
// module is the one place that decides what an event says.
//
// ## Why the copy is keyed by (type, role) and not by type alone
//
// One event type means two different things depending on which side of
// it you are on. `admin.users.disabled` read by the administrator who
// did it is "You disabled a user account"; read by the person it
// happened to it is "Your account was disabled". A single string would
// have to be vague enough to cover both, and vague is the failure mode
// this surface cannot afford — the whole value of an account activity
// log is telling those two apart.
//
// ## Why `role` is never rendered as a label
//
// It picks the voice; it is not itself shown. The discriminator is
// mechanical (`actor = caller`), and for a handful of types the
// mechanism disagrees with the plain reading: `login.succeeded`,
// `logout`, `user.registered` and `user.email_verified` are all
// recorded with a NULL actor — the auth handlers have no actor to name
// at the moment they fire — so a user's own sign-in arrives as
// `on_my_account`. A badge reading "on your account" next to "You
// signed in" would be visibly wrong. Authoring the sentence per pair
// makes that a non-issue: the `on_my_account` voice for
// `login.succeeded` is simply "You signed in", because that is what
// happened.
//
// ## Unknown types fall back to a sentence, never to a payload
//
// New event types land continuously and this catalogue will always
// trail them. `t()` falls back to the KEY STRING when a lookup misses,
// so a naive dynamic lookup would print
// `account.activity.event.some.new.type.by_me` into the page. Hence
// KNOWN_ACTIVITY_EVENTS: the lookup only happens for a type this module
// declares it has copy for, and everything else gets a plain sentence
// naming the type. Never a JSON dump — an account page has no audience
// for one.
//
// activityEvents.test.ts asserts this list and the en.json catalogue
// agree in both directions, so adding a type here without copy (or copy
// without a type) fails there rather than in front of a user.

/** Which side of the event the caller was on. Mirrors the API enum. */
export type ActivityRole = 'by_me' | 'on_my_account';

/** Every event type this module has authored copy for, in both voices.
 *
 *  Scoped to the types that can actually name a user: the recorder
 *  writes plenty of others (federation delivery, retention sweeps, seed
 *  bookkeeping) with neither an actor nor a subject, and those can
 *  never reach this endpoint. Types that CAN reach it but are purely
 *  operational are left to the fallback on purpose — an operator who
 *  needs to read them has /admin/audit. */
export const KNOWN_ACTIVITY_EVENTS = [
  // Sign-in and sessions
  'login.succeeded',
  'login.failed',
  'logout',
  'session.revoked',
  'auth.lockout.triggered',
  'auth.lockout.cleared',
  // The account itself
  'user.registered',
  'user.email_verified',
  'user.password_changed',
  'user.password_reset',
  'user.profile_updated',
  'user.status_changed',
  // Administrative lifecycle
  'admin.users.approved',
  'admin.users.disabled',
  'admin.users.archived',
  'admin.users.restored',
  'admin.users.refused_last_admin',
  'admin.user.hard_deleted_by_gc',
  'admin.impersonation.started',
  'admin.impersonation.ended',
  'admin.search.feedback.audit_viewed',
  // Permissions
  'user.capability_granted',
  'user.capability_revoked',
  'user.capability_grant_removed',
  'user.capability_revoke_removed',
  'user.capability_grant_expired_swept',
  'user.capability_revoke_expired_swept',
  // Access requests
  'request.created',
  'request.granted',
  'request.denied',
  'request.expired',
  // Content the caller deleted or put back
  'admin.asset.soft_deleted',
  'admin.asset.restored',
  'admin.post.soft_deleted',
  'admin.post.restored',
  'admin.collection.soft_deleted',
  'admin.collection.restored',
  // Federation identity
  'federation.user.key_generated',
  'federation.user.key_rotated',
] as const;

const KNOWN = new Set<string>(KNOWN_ACTIVITY_EVENTS);

/** The i18n key for one (type, role) pair, or null when this module has
 *  no copy for the type and the caller should use the fallback. */
export function activityEventKey(eventType: string, role: ActivityRole): string | null {
  if (!KNOWN.has(eventType)) return null;
  return `account.activity.event.${eventType}.${role}`;
}
