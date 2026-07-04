// Package lockout is the Phase 1.19.D per-username account-lockout
// layer. Composes with (does not replace) the in-process LoginLimiter
// at app/internal/auth/ratelimit.go:
//
//   - LoginLimiter is memory-only, short-term, per-instance. Blunts
//     high-frequency burst brute-force from a single IP OR against a
//     single username within a ~minute window.
//
//   - Lockout is DB-backed, durable across process restarts + IP
//     rotation, per-user. Blunts slow-drip distributed brute-force
//     AND username enumeration across many IPs.
//
// Both gates run per login attempt; both must clear for login to
// proceed. Neither knows about the other. Composition happens in the
// login handler.
//
// Anti-enumeration is load-bearing: locked-out users receive the
// SAME 401 response shape as wrong-password users. Attackers rotating
// IPs against a probed username set cannot distinguish "account
// exists + is locked" from "wrong credentials." The Manager surfaces
// lockout state; the login handler enforces the identical response
// shape (including bcrypt work-factor timing on the locked path).
//
// Auto-clear at read-time: IsLockedOut checks lockout_until > NOW()
// on every call. Stale lockout_until values stay in the row until
// overwritten by the next failed login OR admin unlock. Cheaper than
// running a sweeper AND avoids the sweeper-vs-auth race.
//
// Race safety: IncrementFailedLogin uses a single UPDATE...RETURNING
// with the threshold check in a CASE expression. Postgres row-level
// locking serialises concurrent updates so exactly the threshold-th
// attempt writes the lockout deadline; no over-count, no under-count.
//
// Federation-in-mind: failed_login_count and lockout_until are
// per-instance state. Never federated. A federated remote user
// authenticates on their home instance; their local shadow row has
// no lockout state on this instance.
package lockout
