// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Auth state — runes-backed, single instance.
//
// The browser holds a `user` cookie (set by the Go binary on successful
// /api/v1/auth/login); our state tracks the resolved user object so
// component code doesn't have to refetch it on every navigation.
//
// Loading semantics:
//   - On first import, `ready` is false and `user` is null. Call
//     `auth.refresh()` once early (the root +layout does this) to
//     resolve from /api/v1/auth/me.
//   - Login / logout update state in place; no manual refetch needed.
//   - 401 responses from anywhere in the app should call auth.clear()
//     so the chrome reflects the logged-out state immediately.
//   - Capabilities ride the session response and are adopted with the
//     user, never fetched separately. See adopt() (#871).

import { api } from '$api/client';
import { t } from '$stores/lang.svelte';
import { ADMIN_TILE_CAPS } from '$lib/admin/sections';

/** The account's default-view selections, as `/auth/me` reports them
 *  (`CurrentUser.default_views`). Snake_case because it is the wire
 *  shape, unwrapped; the store that consumes it is
 *  browseView.svelte.ts. Absent keys mean "no account preference",
 *  which is a different thing from a stored empty string. */
export interface AccountViewDefaults {
  home_tab?: string | null;
  browse_layout?: string | null;
  browse_sort?: string | null;
}

/**
 * The account's browse-feed content filters (#891), joined onto the
 * session response from `user_preferences.feed_filters`.
 *
 * The FILTERING is the server's — `GET /posts` reads the stored
 * preference and applies it, so nothing here decides what the feed
 * contains. What the client needs is the FACT that it is on, on the
 * same paint as the grid, so a shorter feed can say why it is shorter
 * instead of popping an explanation in a frame later.
 */
export interface AccountFeedFilters {
  hide_restricted?: boolean | null;
}

export interface AuthUser {
  ref: number;
  username: string;
  fullname?: string | null;
  email?: string | null;
  usergroup?: number | null;
  authMethod?: string;
  /** User's persisted UI prefs (joined into the response by the API). */
  language?: string | null;
  /** `''` = the account has no stored preference (each device falls
   *  back to the app default); `'system'` = follow the OS, everywhere.
   *  The two are not the same value — see #677 / migration 00033. */
  theme?: 'light' | 'dark' | 'system' | '' | null;
  /** Account-level browse defaults, joined from user_preferences.
   *  A SEED for devices with no local choice, never an override of one
   *  — the precedence rule lives in browseView.init() (#706). */
  defaultViews?: AccountViewDefaults | null;
  /** Account-level browse-feed content filters (#891). Absent — not an
   *  object of falses — for every account that has not opted in. */
  feedFilters?: AccountFeedFilters | null;
  /**
   * Non-null when the session was minted via
   * POST /admin/users/{ref}/impersonate. Drives the persistent
   * "you are acting as @target" banner. Phase 1.19.A-2.
   */
  impersonatedBy?: { ref: number; username: string } | null;
}

// Capability the backend wildcards over all other capability checks.
// Mirrors `Identity.SuperAdminCapability` in app/internal/auth.
const SYSTEM_ADMIN = 'system.admin';

class AuthState {
  user = $state<AuthUser | null>(null);
  ready = $state(false);
  /** Capability codes the caller holds globally. Loaded by refresh(). */
  caps = $state<string[]>([]);

  /**
   * `system.admin` short-circuits every check, matching the backend's
   * Identity.Can. Used by the admin menu visibility gate and any
   * capability-aware UI bits.
   */
  can(code: string): boolean {
    if (this.caps.includes(SYSTEM_ADMIN)) return true;
    return this.caps.includes(code);
  }

  /**
   * Whether to show the admin entry point at all (#385). True for
   * `system.admin`, and also for any read-cap holder who can open at
   * least one admin surface — so the backend read caps (#356) actually
   * surface in the UI instead of the old binary `system.admin` gate
   * hiding admin entirely from read-only roles.
   *
   * A getter, not a $derived: it reads `this.caps` ($state), so callers
   * that reference it inside their own $derived/effect stay reactive.
   */
  get canSeeAdmin(): boolean {
    if (this.caps.includes(SYSTEM_ADMIN)) return true;
    return ADMIN_TILE_CAPS.some((c) => this.caps.includes(c));
  }

  /**
   * Whether the current user can open one admin tile. Three cases:
   *   - `public` tile (help/docs/about) → visible to anyone in the
   *     admin shell; its page guards nothing sensitive (#399). This is
   *     what un-hid the help section from read-cap operators.
   *   - tile with a `cap` → visible only to holders of that capability
   *     (and `system.admin`, which short-circuits every check).
   *   - cap-less, non-public tile → superuser-only (#385). Most admin
   *     tiles are unmigrated and fall here; they must NOT leak to a
   *     read-cap holder, which is why `public` is explicit rather than
   *     inferred from a missing `cap`.
   * Invariant: a visible tile never 403s on click.
   */
  canSeeTile(tile: { cap?: string; public?: boolean }): boolean {
    if (tile.public) return true;
    return this.can(tile.cap ?? SYSTEM_ADMIN);
  }

  /**
   * Adopt a session payload (the `CurrentUser` schema, as returned by
   * /auth/me, /auth/login and /auth/register) as the signed-in
   * identity.
   *
   * The ONE place `user` and `caps` are assigned, and they are assigned
   * together, from the same response, synchronously (#871). That is the
   * whole invariant: `ready` is what the capability-gated surfaces wait
   * on, so any path that can publish a user without their capabilities
   * is a path that can tell an administrator they have no permission.
   * Fetching the caps separately is exactly such a path — it is how
   * the /admin gate came to flash a red panel at real admins — so
   * there is no setter for one without the other.
   */
  private adopt(u: Record<string, unknown>): void {
    this.user = mapUser(u);
    this.caps = mapCaps(u);
  }

  /** Re-fetch the current session from the server. */
  async refresh(): Promise<void> {
    const { data, error, response } = await api.GET('/auth/me');
    if (error || !data) {
      this.user = null;
      this.caps = [];
    } else {
      this.adopt(data);
    }
    // 401 just means anonymous — not an error condition for refresh.
    void response;
    this.ready = true;
  }

  /**
   * Throws on failure with a user-presentable message. Throws a
   * dedicated [LoginNeedsTOTPError] when the server returns
   * `2fa_required` so the login page can re-prompt with a code
   * input rather than show a generic error.
   */
  async login(username: string, password: string, provider?: string, totpCode?: string): Promise<void> {
    const body: { username: string; password: string; provider?: string; totp_code?: string } = {
      username, password,
    };
    if (provider && provider !== 'password') body.provider = provider;
    if (totpCode) body.totp_code = totpCode;
    const { data, error } = await api.POST('/auth/login', { body });
    if (error || !data) {
      const code = extractError(error) ?? t('auth.err_invalid_credentials');
      if (code === '2fa_required') throw new LoginNeedsTOTPError();
      if (code === 'invalid_2fa_code') throw new LoginNeedsTOTPError('invalid_2fa_code');
      throw new Error(code);
    }
    this.adopt(data);
    this.ready = true;
  }

  async logout(): Promise<void> {
    await api.POST('/auth/logout');
    this.user = null;
    this.caps = [];
  }

  /** Drop in-memory state without a network call. Used on 401. */
  clear(): void {
    this.user = null;
    this.caps = [];
  }

  /**
   * Populate user state from a raw response body (e.g. the result
   * of a SvelteKit load-context fetch). Used by +layout.ts so the
   * load function doesn't have to go through the api client wrapper
   * (which uses the global fetch and would trip SvelteKit's hydration
   * warning).
   *
   * This is the BOOT path — the one that decides what a cold navigate
   * to /admin/* renders on its first frame — so it takes capabilities
   * off the same body, via the same setter, as every other path.
   */
  hydrateFrom(u: Record<string, unknown>): void {
    this.adopt(u);
  }

  /** Mark the store as initialised even when no user was loaded. */
  markReady(): void {
    this.ready = true;
  }
}

export const auth = new AuthState();

/**
 * Thrown by [AuthState.login] when the server responded
 * `error: "2fa_required"` (kind="required") or
 * `error: "invalid_2fa_code"` (kind="invalid"). The login page
 * pivots on this to render the TOTP code input.
 */
export class LoginNeedsTOTPError extends Error {
  kind: 'required' | 'invalid';
  constructor(code: '2fa_required' | 'invalid_2fa_code' = '2fa_required') {
    super(code);
    this.name = 'LoginNeedsTOTPError';
    this.kind = code === 'invalid_2fa_code' ? 'invalid' : 'required';
  }
}

function mapUser(u: Record<string, unknown>): AuthUser {
  const ib = u.impersonated_by as { ref?: number; username?: string } | null | undefined;
  return {
    ref: Number(u.ref),
    username: String(u.username),
    fullname: (u.fullname ?? null) as string | null,
    email: (u.email ?? null) as string | null,
    usergroup: (u.usergroup ?? null) as number | null,
    authMethod: u.auth_method as string | undefined,
    language: (u.language ?? null) as string | null,
    theme: (u.theme ?? null) as 'light' | 'dark' | 'system' | '' | null,
    defaultViews: (u.default_views ?? null) as AccountViewDefaults | null,
    feedFilters: (u.feed_filters ?? null) as AccountFeedFilters | null,
    impersonatedBy: ib && ib.ref != null && ib.username != null
      ? { ref: ib.ref, username: ib.username }
      : null,
  };
}

/** Read `CurrentUser.capabilities` off a session body. Absent, null or
 *  malformed all mean "holds nothing" — the safe fallback, and the one
 *  the server documents for a failed capability lookup. */
function mapCaps(u: Record<string, unknown>): string[] {
  const c = u.capabilities;
  if (!Array.isArray(c)) return [];
  return c.filter((v): v is string => typeof v === 'string');
}

function extractError(err: unknown): string | undefined {
  if (err && typeof err === 'object' && 'error' in err) {
    const v = (err as { error: unknown }).error;
    if (typeof v === 'string') return v;
  }
  return undefined;
}
