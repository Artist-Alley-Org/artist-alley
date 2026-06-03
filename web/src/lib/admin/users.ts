// Helpers for the /admin/users surface. Pure (no DOM) so the
// vitest pure-helper suite can pin them.

export type AdminUserStatus = 'active' | 'pending' | 'disabled';

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
  }
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
