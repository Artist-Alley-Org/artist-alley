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
import { lang, t } from '$stores/lang.svelte';
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
 * The account's browse-feed content preferences (#891, default inverted
 * by #921), joined onto the session response from
 * `user_preferences.feed_filters`.
 *
 * The FILTERING is the server's — `GET /posts` reads the stored
 * preference and applies it, so nothing here decides what the feed
 * contains. What the client needs is the FACT of the setting, on the
 * same paint as the grid, so the feed can explain its own shape instead
 * of popping an explanation in a frame later.
 *
 * Absent for every account on the build's defaults: `/auth/me` omits the
 * object when every key is at its zero value, and since #921 the zero
 * value is "hide the placeholders" rather than "show everything". Read
 * an absent object as the DEFAULT feed, never as "unfiltered".
 */
export interface AccountFeedFilters {
  show_restricted?: boolean | null;
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
  /** Account-level browse-feed content preferences (#891/#921). Absent —
   *  not an object of falses — for every account on the defaults. */
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

/**
 * Mirrors `CurrentUser.capabilities_status` (#956). `resolved` means
 * `caps` is authoritative — an empty array then means the account
 * genuinely holds nothing. `unavailable` means the server could not
 * determine the set at all, and `caps` says nothing about the account.
 *
 * Those are not two ways of saying "no rights". Collapsing them is what
 * let a resolver blip render an administrator a permission refusal.
 */
export type CapsStatus = 'resolved' | 'unavailable';

class AuthState {
  user = $state<AuthUser | null>(null);
  ready = $state(false);
  /** Capability codes the caller holds globally. Loaded by refresh().
   *  Only meaningful when `capsStatus === 'resolved'`. */
  caps = $state<string[]>([]);
  /**
   * Whether `caps` above could be determined. Assigned by the same
   * setter, from the same response, as `caps` itself — see adopt().
   *
   * Defaults to `resolved` because the pre-boot state is "signed out,
   * holding nothing", which is a determination, not a failure. Nothing
   * gated reads this before `ready` anyway.
   */
  capsStatus = $state<CapsStatus>('resolved');

  /**
   * True when the server told us it could not work out what this
   * account may do. Surfaces MUST still grant nothing (`can()` returns
   * false throughout), but they must describe THIS rather than a
   * permission decision: the honest render is an error with a retry,
   * not "you don't have permission to view this page".
   */
  get capsUnavailable(): boolean {
    return this.capsStatus === 'unavailable';
  }

  /**
   * `system.admin` short-circuits every check, matching the backend's
   * Identity.Can. Used by the admin menu visibility gate and any
   * capability-aware UI bits.
   *
   * Unknown rights are NO rights (#956). The explicit guard is belt to
   * `adopt()`'s braces: the server omits `capabilities` whenever it
   * reports `unavailable`, so `caps` is already empty on that path —
   * but "the gate cannot open on a set we do not trust" is the rule,
   * and a rule enforced only by a coincidence of another layer is one
   * refactor from being untrue.
   */
  can(code: string): boolean {
    if (this.capsUnavailable) return false;
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
    // Unknown rights are no rights (#956) — same rule as can().
    if (this.capsUnavailable) return false;
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
   * The ONE place `user`, `caps` and `capsStatus` are assigned, and
   * they are assigned together, from the same response, synchronously
   * (#871). That is the whole invariant: `ready` is what the
   * capability-gated surfaces wait on, so any path that can publish a
   * user without their capabilities is a path that can tell an
   * administrator they have no permission. Fetching the caps separately
   * is exactly such a path — it is how the /admin gate came to flash a
   * red panel at real admins — so there is no setter for one without
   * the other.
   *
   * `capsStatus` joins the set for the same reason and travels with it
   * (#956): a capability list and the question "is this list real?" are
   * one fact, and reading them a frame apart would recreate #871 with
   * an extra step.
   */
  private adopt(u: Record<string, unknown>): void {
    this.user = mapUser(u);
    this.caps = mapCaps(u);
    this.capsStatus = mapCapsStatus(u);
    // The account's language arrives on this same body and has to be
    // APPLIED, not merely stored (#869). It hangs here for the reason
    // the caps do: this is the one place a user is published, so every
    // path that can produce a signed-in identity — login(), refresh(),
    // and hydrateFrom() on boot — applies it, and a fourth path cannot
    // be added that quietly does not.
    //
    // Measured before the fix, because "which path was broken" was not
    // what the issue assumed: a COLD NAVIGATE was already correct, since
    // lang.init() reads auth.user in +layout.svelte's onMount and
    // +layout.ts has awaited hydration by then. SIGNING IN was not — the
    // root layout mounts once, so a visitor who lands on /login runs
    // init() against a null user and keeps English until a full reload.
    // Same mount-once gap theme.syncFromAccount() was written for. The
    // apply sits on adopt() rather than on login() so the two paths
    // cannot answer differently again.
    lang.syncFromAccount();
  }

  /** Re-fetch the current session from the server. */
  async refresh(): Promise<void> {
    const { data, error, response } = await api.GET('/auth/me');
    if (error || !data) {
      this.user = null;
      this.caps = [];
      // Not a degraded capability lookup: there is no session to
      // resolve capabilities FOR. Anonymous genuinely holds nothing,
      // and +layout.ts sends a user-less visitor to /login anyway.
      this.capsStatus = 'resolved';
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
    this.capsStatus = 'resolved';
    // The account's language leaves with the account (#967). adopt()
    // applies it AND writes the device cookie so the next cold load
    // paints it without a flash; without this, that cookie would outlive
    // the session and the next visitor at a shared machine would get the
    // previous account's language on their first paint.
    //
    // The pairing is the point: syncFromAccount() only earns the right
    // to write device state because logout() takes it back. Deliberately
    // NOT in clear() below — that is the 401 path, and a session that
    // aged out is not somebody leaving the machine.
    lang.reset();
  }

  /** Drop in-memory state without a network call. Used on 401.
   *  Signed out is a determination, not a failed one — see refresh(). */
  clear(): void {
    this.user = null;
    this.caps = [];
    this.capsStatus = 'resolved';
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
 *  malformed all yield an empty set — the safe fallback. WHY it is
 *  empty is `capabilities_status`'s job, not this function's; read
 *  mapCapsStatus() alongside it and never treat an empty array on its
 *  own as "the account has no rights" (#956). */
function mapCaps(u: Record<string, unknown>): string[] {
  const c = u.capabilities;
  if (!Array.isArray(c)) return [];
  return c.filter((v): v is string => typeof v === 'string');
}

/**
 * Read `CurrentUser.capabilities_status` off a session body (#956).
 *
 * Strict allowlist, and the direction is deliberate: ONLY the literal
 * `'resolved'` is taken as "this list is real". Anything else — absent,
 * null, a typo, a value from a schema version this build does not know
 * — reads as `unavailable`.
 *
 * That is the safe direction on both axes at once. `unavailable` grants
 * nothing (can() returns false throughout), so an unrecognised body can
 * never widen access; and it renders as "we could not determine your
 * rights, retry" rather than "you have no permission", so an
 * unrecognised body can never accuse the user of something untrue
 * either. Defaulting the other way would restore the exact conflation
 * this field exists to end, and would do it silently.
 */
function mapCapsStatus(u: Record<string, unknown>): CapsStatus {
  return u.capabilities_status === 'resolved' ? 'resolved' : 'unavailable';
}

function extractError(err: unknown): string | undefined {
  if (err && typeof err === 'object' && 'error' in err) {
    const v = (err as { error: unknown }).error;
    if (typeof v === 'string') return v;
  }
  return undefined;
}
