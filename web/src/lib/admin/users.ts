// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Helpers for the /admin/users surface. Pure (no DOM) so the
// vitest pure-helper suite can pin them.

// Phase 1.17.A — `archived` is the fourth lifecycle state. Same
// auth-gate semantics as `disabled` (login rejected) but hidden
// from the default admin list and presented as terminal in the
// UI (operator action implies "this user has left the org" rather
// than "temporarily revoked"). Reachable from active or disabled;
// reversible via the Restore action.
export type AdminUserStatus = 'active' | 'pending' | 'disabled' | 'archived';

export interface AdminUser {
  ref: number;
  username: string;
  display_name: string;
  status: AdminUserStatus;
  primary_role?: string | null;
  fullname?: string | null;
  email?: string | null;
  avatar_url?: string | null;
  auth_origin?: string | null;
  created_at: string;
  last_active?: string | null;
  account_expires?: string | null;
  profile_origin_server_id?: string | null;
  // Phase 1.19.D — persistent per-username lockout state.
  lockout_until?: string | null;
  failed_login_count?: number;
}

/** Tailwind class chip for the per-status badge. Keep the green /
 *  yellow / red palette consistent with the workflow-state badges so
 *  operators reading both surfaces don't need to learn two scales. */
export function statusBadgeClass(s: AdminUserStatus): string {
  switch (s) {
    case 'active':
      return 'bg-success/15 text-success border border-success/40';
    case 'pending':
      return 'bg-warning/15 text-warning border border-warning/40';
    case 'disabled':
      return 'bg-danger/15 text-danger border border-danger/40';
    case 'archived':
      // Slightly muted vs disabled — archived reads as "neutral
      // terminal" rather than "active operator concern".
      return 'bg-muted/30 text-muted-foreground border border-muted/50';
  }
}

// Phase 1.17.A — typed transition matrix mirroring the backend
// (internal/users/userstate.go). The admin UI uses this to drive
// which action buttons render for the current row state. Keep in
// sync with ValidateTransition on the Go side; tests pin the
// bijection.
const TRANSITIONS: Record<AdminUserStatus, AdminUserStatus[]> = {
  pending: ['active'],
  active: ['disabled', 'archived'],
  disabled: ['active', 'archived'],
  archived: ['active'],
};

export function validTargetsFrom(current: AdminUserStatus): AdminUserStatus[] {
  return TRANSITIONS[current] ?? [];
}

// Verb form for each target — used for confirmation copy + button
// labels. Approve/Disable/Archive/Restore mirror the typed audit
// event family on the backend.
export function transitionVerb(from: AdminUserStatus, to: AdminUserStatus): string {
  if (from === 'pending' && to === 'active') return 'approve';
  if (to === 'active') return 'restore';
  if (to === 'disabled') return 'disable';
  if (to === 'archived') return 'archive';
  return 'set';
}

/** Human-friendly relative duration since `iso`. Returns empty string
 *  for null/undefined so callers can render a fallback like "Never".
 *
 *  Buckets: <1m / <1h / <1d / <30d / <1y / years. Not localised yet
 *  (Phase 1.17 i18n surface adds Intl.RelativeTimeFormat plumbing); the
 *  English form here is the fallback when `t()` doesn't have a key. */
export function relativeAgo(iso: string | null | undefined, now: Date = new Date()): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const deltaSec = Math.max(0, Math.floor((now.getTime() - t) / 1000));
  if (deltaSec < 60) return 'just now';
  const min = Math.floor(deltaSec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const d = Math.floor(hr / 24);
  if (d < 30) return `${d}d ago`;
  if (d < 365) return `${Math.floor(d / 30)}mo ago`;
  return `${Math.floor(d / 365)}y ago`;
}

/** Build the URLSearchParams the openapi-fetch client expects.
 *  Empty / nullish values are omitted entirely so the request URL
 *  stays clean (no `?q=&status=`). */
export interface ListAdminUsersQuery {
  q?: string;
  status?: AdminUserStatus | '';
  cursor?: string | null;
  limit?: number;
}

export function buildListQuery(q: ListAdminUsersQuery): Record<string, string | number> {
  const out: Record<string, string | number> = {};
  if (q.q && q.q.trim() !== '') out.q = q.q.trim();
  if (q.status) out.status = q.status;
  if (q.cursor) out.cursor = q.cursor;
  if (q.limit) out.limit = q.limit;
  return out;
}
