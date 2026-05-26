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
}

class AuthState {
  user = $state<AuthUser | null>(null);
  ready = $state(false);

  /** Re-fetch the current session from the server. */
  async refresh(): Promise<void> {
    const { data, error, response } = await api.GET('/auth/me');
    if (error || !data) {
      this.user = null;
    } else {
      this.user = mapUser(data);
    }
    // 401 just means anonymous — not an error condition for refresh.
    void response;
    this.ready = true;
  }

  /** Throws on failure with a user-presentable message. */
  async login(username: string, password: string): Promise<void> {
    const { data, error } = await api.POST('/auth/login', {
      body: { username, password },
    });
    if (error || !data) {
      throw new Error(extractError(error) ?? 'Invalid credentials');
    }
    this.user = mapUser(data);
    this.ready = true;
  }

  async logout(): Promise<void> {
    await api.POST('/auth/logout');
    this.user = null;
  }

  /** Drop in-memory state without a network call. Used on 401. */
  clear(): void {
    this.user = null;
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
  };
}

function extractError(err: unknown): string | undefined {
  if (err && typeof err === 'object' && 'error' in err) {
    const v = (err as { error: unknown }).error;
    if (typeof v === 'string') return v;
  }
  return undefined;
}
