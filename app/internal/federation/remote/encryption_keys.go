// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-c — remote-actor encryption-key cache + helpers.
//
// The federation_remote_actors table grew three nullable columns
// in migration 00008 holding the X25519 public key a remote actor
// advertises in their envelope's aa:encryptionPublicKey block.
// This file is the in-process surface for reading + writing that
// data: a small Handler with an LRU cache layered over the sqlc
// queries.
//
// # Responsibilities
//
//   - Cache hit on the read hot-path (I-e outbox encryption).
//   - Distinguish "actor doesn't exist" (ErrNoActor) from "actor
//     exists but advertised no key" (ErrNoEncryptionKey) so the
//     sender-refusal flow (I-g) can fire the right reason.
//   - Detect "this inbound key is actually new" vs "this inbound
//     key is a refresh" so the audit event (I-c-3 wiring)
//     doesn't fire on every single envelope from a stable peer.
//
// # Not in this commit
//
//   - The outbox does not yet emit aa:encryptionPublicKey in
//     env.Extra (I-c-3).
//   - The inbox dispatcher does not yet call SetEncryptionKey
//     (I-c-3).
//   - The sender refusal flow does not yet consult this surface
//     (I-g).

package remote

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// cacheDomainRemoteEncryptionKey is the cache.Registry domain
// for the per-actor encryption-key LRU. Cross-process NOTIFY
// broadcasts arrive on this channel so federated replicas drop
// their copy in lockstep when one of them writes.
const cacheDomainRemoteEncryptionKey = "remote_actor.encryption_key.x25519"

// encryptionKeyCacheSize bounds the LRU. 5,000 actors is well
// above what a typical pre-MVP instance sees; the cap exists so
// a pathological inbound storm (every-activity-from-a-new-actor)
// can't grow memory unboundedly. Eviction just means the next
// read for the evicted actor refetches from Postgres + repopulates.
const encryptionKeyCacheSize = 5_000

// RemoteEncryptionKey is the cached snapshot of an actor's
// advertised X25519 public key. Held by-value in the LRU; the
// 32-byte key array means zero allocs on Get.
//
// All fields are pre-validated at write time (migration 00008's
// atomic CHECK + Handler.SetEncryptionKey's 32-byte assertion),
// so consumers (I-e outbox encryption) can treat them as canonical.
type RemoteEncryptionKey struct {
	Key       [32]byte
	Version   int32
	UpdatedAt time.Time
}

// Sentinel errors callers may distinguish on. The two no-key
// states are different — "actor doesn't exist" means we never
// federated with them, "no encryption key" means the peer is on
// a pre-I-c build (or hasn't generated keys yet) — and the
// sender refusal flow (I-g) fires different reasons accordingly.
var (
	// ErrNoActor — the actor URI has no row in federation_remote_actors.
	ErrNoActor = errors.New("remote: actor not known locally")

	// ErrNoEncryptionKey — the actor row exists but the
	// encryption_public_key column is NULL (peer is pre-I-c, or
	// hasn't shipped a key yet).
	ErrNoEncryptionKey = errors.New("remote: actor has no advertised encryption key")

	// ErrEncryptionKeyMalformed — the inbound key isn't 32 bytes.
	// The migration's CHECK constraint would reject this at the
	// DB boundary too; we surface it as a typed error here so the
	// inbox dispatcher can log it as "malformed inbound key"
	// rather than as a generic write failure.
	ErrEncryptionKeyMalformed = errors.New("remote: encryption public key must be exactly 32 bytes")
)

// Handler owns the read + write surface for remote-actor
// encryption-key state. Boot constructs one + wires it into the
// inbox dispatcher's RemoteActorUpserter (1.22.I-c-3) so every
// inbound activity with an aa:encryptionPublicKey block lands
// here, and into the outbox dispatcher's recipient lookup
// (1.22.I-e) so envelope encryption can resolve the recipient's
// key.
//
// Construct via NewHandler. cacheReg may be nil for tests; the
// Handler degrades to a "cache always misses" mode that still
// works against the DB.
type Handler struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	q      *Queries

	encryptionKeys *cache.Cache[RemoteEncryptionKey]
}

// NewHandler wires the package.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, cacheReg *cache.Registry) *Handler {
	h := &Handler{pool: pool, logger: logger, q: New(pool)}
	if cacheReg != nil {
		h.encryptionKeys = cache.Register[RemoteEncryptionKey](
			cacheReg, cacheDomainRemoteEncryptionKey, encryptionKeyCacheSize,
		)
	}
	return h
}

// GetEncryptionKey returns the actor's current X25519 public key.
// Cache hit path: zero DB calls. Cache miss path: one query (warm
// case) or two (cold case where we need to distinguish missing
// actor from missing key).
//
// Returns:
//
//   - (key, nil) when the actor has an advertised key
//   - (zero, ErrNoEncryptionKey) when the actor is known but has
//     no key (pre-I-c peer)
//   - (zero, ErrNoActor) when the actor URI isn't in the local
//     table at all
//   - (zero, err) on any other DB failure
//
// Neither no-key state is cached — the next inbound activity
// from this actor may populate the column, and a sentinel cache
// entry would mask the update. The atomic CHECK + the Set path's
// Invalidate together make positive entries safe to cache.
func (h *Handler) GetEncryptionKey(ctx context.Context, actorURI string) (RemoteEncryptionKey, error) {
	if h.encryptionKeys != nil {
		if v, ok := h.encryptionKeys.Get(actorURI); ok {
			return v, nil
		}
	}

	row, err := h.q.GetRemoteActorEncryptionKey(ctx, actorURI)
	if err == nil {
		// The query filters WHERE encryption_public_key IS NOT
		// NULL, so the returned bytes + version pointer are
		// guaranteed non-NULL — but the generated types still
		// expose them as nullable. Pin shape at the boundary.
		if len(row.EncryptionPublicKey) != 32 || row.EncryptionPublicKeyVersion == nil {
			return RemoteEncryptionKey{}, ErrEncryptionKeyMalformed
		}
		var out RemoteEncryptionKey
		copy(out.Key[:], row.EncryptionPublicKey)
		out.Version = *row.EncryptionPublicKeyVersion
		if row.EncryptionPublicKeyUpdatedAt.Valid {
			out.UpdatedAt = row.EncryptionPublicKeyUpdatedAt.Time
		}
		if h.encryptionKeys != nil {
			h.encryptionKeys.Add(actorURI, out)
		}
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RemoteEncryptionKey{}, err
	}

	// pgx.ErrNoRows on the key query could mean either "no actor"
	// or "actor exists but key is NULL". Disambiguate with a
	// follow-up. This second query only runs on the cold-miss
	// no-key path — the steady-state cache hit + warm-miss
	// fast-path both bypass it.
	if _, gErr := h.q.GetRemoteActor(ctx, actorURI); gErr != nil {
		if errors.Is(gErr, pgx.ErrNoRows) {
			return RemoteEncryptionKey{}, ErrNoActor
		}
		return RemoteEncryptionKey{}, gErr
	}
	return RemoteEncryptionKey{}, ErrNoEncryptionKey
}

// SetEncryptionKey records a fresh inbound key on an existing
// remote-actor row. The actor row must already exist — boot
// wires this Handler downstream of UpsertRemoteActor so the
// inbox dispatcher does the display-info upsert (which creates
// the row) before this runs.
//
// Returns:
//
//   - changed=true       when this was a real key change (different
//                        bytes OR different version OR first-time set).
//                        Caller emits federation.remote_actor.key_updated.
//   - changed=false      when the inbound (key, version) match the
//                        cached value exactly — refresh-only. The
//                        DB row's updated_at still moves forward
//                        per the column's docs.
//   - ErrEncryptionKeyMalformed when len(key) != 32 at the in-process
//                        boundary, even before reaching the DB.
//   - ErrNoActor         when no row matched actor_uri (UpsertRemoteActor
//                        wasn't called).
//
// On success the cache entry is Invalidated (broadcast across
// peers) so the next read picks up the fresh value.
func (h *Handler) SetEncryptionKey(ctx context.Context, actorURI string, key []byte, version int32) (changed bool, prevVersion int32, err error) {
	if len(key) != 32 {
		return false, 0, ErrEncryptionKeyMalformed
	}
	if version < 1 {
		return false, 0, ErrEncryptionKeyMalformed
	}

	// Read previous state for change detection. ErrNoActor +
	// ErrNoEncryptionKey both mean "no prior key" — that's a
	// first-time set, which is a change. Any other error bubbles.
	var (
		prevKey     [32]byte
		hadPrev     bool
	)
	prev, prevErr := h.GetEncryptionKey(ctx, actorURI)
	switch {
	case prevErr == nil:
		hadPrev = true
		prevKey = prev.Key
		prevVersion = prev.Version
	case errors.Is(prevErr, ErrNoActor):
		// Don't preflight a non-existent actor — the UPDATE will
		// return 0 rowcount and we surface ErrNoActor. Keep going.
	case errors.Is(prevErr, ErrNoEncryptionKey):
		// First-time key set.
	default:
		return false, 0, prevErr
	}

	rowCount, err := h.q.SetRemoteActorEncryptionKey(ctx, SetRemoteActorEncryptionKeyParams{
		ActorUri:                   actorURI,
		EncryptionPublicKey:        key,
		EncryptionPublicKeyVersion: &version,
	})
	if err != nil {
		return false, 0, err
	}
	if rowCount == 0 {
		return false, 0, ErrNoActor
	}

	if h.encryptionKeys != nil {
		// Invalidate broadcasts across peers; the next read on any
		// node sees the fresh value. We don't pre-populate the cache
		// with the new value here because (a) the read after a write
		// is rare, and (b) seeding the cache pre-commit would race
		// readers into stale snapshots if the caller's wrapping tx
		// rolls back (we don't take a tx here today but defensive).
		_ = h.encryptionKeys.Invalidate(ctx, actorURI)
	}

	// Change detection — constant-time on the 32-byte key bytes
	// (no information leak via timing on which byte differed).
	if hadPrev {
		sameKey := subtle.ConstantTimeCompare(prevKey[:], key) == 1
		sameVersion := prevVersion == version
		if sameKey && sameVersion {
			return false, prevVersion, nil
		}
	}
	return true, prevVersion, nil
}

// CountMissingEncryptionKey returns the number of remote-actor
// rows still on NULL encryption_public_key. Operator-facing
// observability for the admin federation surface; backed by the
// partial index so it's cheap regardless of total actor count.
func (h *Handler) CountMissingEncryptionKey(ctx context.Context) (int64, error) {
	return h.q.CountRemoteActorsMissingEncryptionKey(ctx)
}
