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

  /** Throws on failure with a user-presentable message. */
  async login(username: string, password: string, provider?: string): Promise<void> {
    const body: { username: string; password: string; provider?: string } = { username, password };
    if (provider && provider !== 'password') body.provider = provider;
    const { data, error } = await api.POST('/auth/login', { body });
    if (error || !data) {
      throw new Error(extractError(error) ?? 'Invalid credentials');
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

function mapUser(u: Record<string, unknown>): AuthUser {
  return {
    ref: Number(u.ref),
    username: String(u.username),
    fullname: (u.fullname ?? null) as string | null,
    email: (u.email ?? null) as string | null,
    usergroup: (u.usergroup ?? null) as number | null,
    authMethod: u.auth_method as string | undefined,
    language: (u.language ?? null) as string | null,
    theme: (u.theme ?? null) as 'light' | 'dark' | '' | null,
  };
}

function extractError(err: unknown): string | undefined {
  if (err && typeof err === 'object' && 'error' in err) {
    const v = (err as { error: unknown }).error;
    if (typeof v === 'string') return v;
  }
  return undefined;
}
