package mention

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// cacheDomainResolve is the cache.Registry domain for username→ref
// resolution. Key is the lower-cased username; value is the ref, with
// 0 as the "known-absent" sentinel (refs are positive bigints, so 0
// can never collide with a real user). Caching the absent result too
// shrugs off the common non-usernames (@here, @channel, @everyone)
// that would otherwise re-query on every post.
const cacheDomainResolve = "mention.resolve"

// absentSentinel is the cached value for "this username does not
// resolve to a local user." Distinct from a cache MISS (not looked up
// yet) so we don't re-hit the DB for a name we already know is absent.
const absentSentinel int64 = 0

// Resolver maps local @usernames to user refs, fronted by a 5-minute
// cache (the TTL is the LRU's; usernames are API-immutable in v0.1.0 —
// no rename endpoint — so there's no invalidator to wire).
type Resolver struct {
	pool  *pgxpool.Pool
	cache *cache.Cache[int64]
}

// NewResolver wires the resolver to the pool + cache registry. A nil
// registry degrades to no caching (every resolve hits the DB) — the
// same nil-safe shape every handler in this codebase uses, which keeps
// unit tests that don't stand up a registry simple.
func NewResolver(pool *pgxpool.Pool, registry *cache.Registry) *Resolver {
	r := &Resolver{pool: pool}
	if registry != nil {
		// 5k entries covers the active mentionable-user population at
		// typical install sizes; ~24 bytes per int64 entry keeps the
		// LRU well under 1 MB at capacity.
		r.cache = cache.Register[int64](registry, cacheDomainResolve, 5_000)
	}
	return r
}

// ResolveLocal maps the local mentions to user refs, de-duplicated.
// Federated mentions (InstanceHost != "") are skipped — v0.1.0 has no
// WebFinger resolution. Unknown usernames drop silently (no error).
// Self-mentions are NOT filtered here: the notifications.Writer already
// gates actor==recipient, so keeping them is correct and avoids
// threading the actor ref into the resolver.
//
// Returns a de-duplicated slice of refs in first-seen order.
func (r *Resolver) ResolveLocal(ctx context.Context, mentions []Mention) []int64 {
	if len(mentions) == 0 {
		return nil
	}
	var out []int64
	seenRef := make(map[int64]struct{})
	for _, m := range mentions {
		if m.InstanceHost != "" {
			continue // federated — not resolvable locally in v0.1.0
		}
		ref, ok := r.resolveOne(ctx, m.Username)
		if !ok {
			continue // unknown username — drop silently
		}
		if _, dup := seenRef[ref]; dup {
			continue
		}
		seenRef[ref] = struct{}{}
		out = append(out, ref)
	}
	return out
}

// resolveOne resolves a single username to a ref, consulting the cache
// first. Returns (ref, true) for a real user, (0, false) for a known-
// or newly-discovered absent username.
func (r *Resolver) resolveOne(ctx context.Context, username string) (int64, bool) {
	key := strings.ToLower(username)

	if r.cache != nil {
		if v, hit := r.cache.Get(key); hit {
			if v == absentSentinel {
				return 0, false
			}
			return v, true
		}
	}

	ref, found := r.queryRef(ctx, key)
	if r.cache != nil {
		if found {
			r.cache.Add(key, ref)
		} else {
			r.cache.Add(key, absentSentinel)
		}
	}
	return ref, found
}

// queryRef runs the username→ref lookup. Case-insensitive via the
// user_username_lower_idx. No approval/state filter — the
// notifications.Writer gates deliverability (prefs + blocks), so an
// unapproved user simply never sees the row rather than being excluded
// here. LIMIT 1 is defensive against the (non-unique) lower-index
// admitting case-variant usernames; ORDER BY ref makes the pick
// deterministic.
func (r *Resolver) queryRef(ctx context.Context, lowerUsername string) (int64, bool) {
	const q = `SELECT ref FROM "user" WHERE lower(username) = $1 ORDER BY ref LIMIT 1`
	var ref int64
	if err := r.pool.QueryRow(ctx, q, lowerUsername).Scan(&ref); err != nil {
		// pgx.ErrNoRows → genuinely absent; any other error we also
		// treat as "unresolved" (the mention just doesn't fire) rather
		// than failing the caller's post/comment write.
		return 0, false
	}
	if ref == absentSentinel {
		// Paranoia: a real ref is never 0, but if one somehow is,
		// don't confuse it with the absent sentinel.
		return 0, false
	}
	return ref, true
}
