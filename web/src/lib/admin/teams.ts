// Pure helpers for the /admin/teams surface (Phase 1.17.E).
// Sit-side from the page so vitest can pin them without booting
// svelte-kit. Same pattern as $lib/admin/users.

export interface Team {
  id: string;
  slug: string;
  name: string;
  description: string;
  created_at: string;
  updated_at: string;
  parent_ids?: string[];
  origin_server_id?: string | null;
}

export interface TeamMember {
  user_ref: number;
  username?: string | null;
  display_name?: string | null;
  added_at: string;
}

/** Slugs must be URL-safe + lowercase + bounded. Mirrors the
 *  backend's UNIQUE NULLS NOT DISTINCT (origin_server_id, slug)
 *  constraint — a malformed slug would 409 server-side; we catch
 *  it client-side so the UI can refuse-with-explanation. */
const SLUG_RE = /^[a-z0-9](?:[a-z0-9-]{0,79})$/;

export function isValidSlug(s: string): boolean {
  return SLUG_RE.test(s);
}

/** Slugify a free-text name into a candidate slug. Drops everything
 *  outside [a-z0-9-], collapses repeats, trims leading/trailing
 *  dashes, caps at 80 chars. Used by the create form's "auto-slug
 *  from name" suggestion — the admin can override. */
export function slugify(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 80);
}

/** UUID v4 shape check for the "add parent" + "add member" inputs.
 *  We accept any RFC-4122 UUID (not just v4) since the server may
 *  emit v7 in the future. */
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function isValidUUID(s: string): boolean {
  return UUID_RE.test(s);
}
