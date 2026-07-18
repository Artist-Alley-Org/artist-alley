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

import { api } from '$api/client';
import { t } from '$stores/lang.svelte';
import { ADMIN_TILE_CAPS } from '$lib/admin/sections';

export interface AuthUser {
  ref: number;
  username: string;
  fullname?: string | null;
  email?: string | null;
  usergroup?: number | null;
  authMethod?: string;
  /** User's persisted UI prefs (joined into the response by the API). */
  language?: string | null;
  theme?: 'light' | 'dark' | '' | null;
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

  /** Re-fetch the current session from the server. */
  async refresh(): Promise<void> {
    const { data, error, response } = await api.GET('/auth/me');
    if (error || !data) {
      this.user = null;
      this.caps = [];
    } else {
      this.user = mapUser(data);
      // Caps load in parallel — soft fail (empty caps = no admin
      // UI). Anonymous callers shouldn't reach this branch.
      void this.refreshCaps();
    }
    // 401 just means anonymous — not an error condition for refresh.
    void response;
    this.ready = true;
  }

  /**
   * Pull the caller's resolved capability set from
   * GET /auth/me/capabilities. Called by refresh() and after login.
   */
  async refreshCaps(): Promise<void> {
    if (!this.user) {
      this.caps = [];
      return;
    }
    try {
      const { data } = await api.GET('/auth/me/capabilities');
      if (data && Array.isArray(data.capabilities)) {
        this.caps = data.capabilities;
      }
    } catch {
      this.caps = [];
    }
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
    this.user = mapUser(data);
    this.ready = true;
    void this.refreshCaps();
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
   */
  hydrateFrom(u: Record<string, unknown>): void {
    this.user = mapUser(u);
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
    theme: (u.theme ?? null) as 'light' | 'dark' | '' | null,
    impersonatedBy: ib && ib.ref != null && ib.username != null
      ? { ref: ib.ref, username: ib.username }
      : null,
  };
}

function extractError(err: unknown): string | undefined {
  if (err && typeof err === 'object' && 'error' in err) {
    const v = (err as { error: unknown }).error;
    if (typeof v === 'string') return v;
  }
  return undefined;
}
