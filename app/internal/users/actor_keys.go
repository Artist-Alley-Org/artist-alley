// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.A — federation actor keypair plumbing.
//
// Per docs/spec/federation/v1.md §5 + §6 + §13, every federated
// user holds two keypairs:
//
//   - Ed25519 signing keypair (envelope signatures)
//   - X25519 encryption keypair (NaCl-box recipient key)
//
// Both private keys are stored encrypted-at-rest via the
// app/internal/atrest AES-256-GCM wrapper. Public keys are stored
// in the clear (they're meant to be public). The actor_uri is
// the stable cross-instance handle for this user.
//
// Lazy generation: existing users (created before migration
// 00048) and any user not yet involved in federation has NULL key
// columns. EnsureActorKeyMaterial generates a fresh keypair set,
// encrypts the private bits, and persists. It is idempotent —
// callers that don't know whether keys exist yet can call it
// blindly; only the first call generates.
//
// Key rotation is deferred to Phase 1.22.K per spec §14. Until
// then, the only way to "rotate" a compromised user key is to
// delete-and-recreate the user account.

package users

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/nacl"
)

// ActorKeyMaterial is the typed view of the five federation
// columns on the user row. Returned by GetActorKeyMaterial; nil
// when the user has no keys yet.
//
// Private-key fields are the AES-GCM ciphertext bytes — call
// DecryptSigningPrivateKey / DecryptEncryptionPrivateKey on this
// package (or use the convenience getters on Handler) to obtain
// plaintext keys for in-memory cryptographic operations.
type ActorKeyMaterial struct {
	ActorURI                string // e.g. "https://studio-a.example/users/alice"
	SigningPublicKeyPEM     []byte // RFC 8410 PEM of the Ed25519 public key
	SigningPrivateKeyEnc    []byte // atrest-encrypted PKCS#8 PEM of Ed25519 private key
	EncryptionPublicKey     []byte // raw 32-byte X25519 public key
	EncryptionPrivateKeyEnc []byte // atrest-encrypted raw 32-byte X25519 private scalar
}

// ErrNoActorKeyMaterial indicates the user exists but has no
// federation keys generated yet. Callers either generate them via
// EnsureActorKeyMaterial or surface this as "user is not
// federated" to the caller.
var ErrNoActorKeyMaterial = errors.New("users: actor key material not generated for this user")

// actorKeyCacheKey is the canonical LRU key for the per-user
// actor-key cache. Stringified userRef keeps the format identical
// to other per-user caches in this package.
func actorKeyCacheKey(userRef int64) string { return strconv.FormatInt(userRef, 10) }

// GetActorKeyMaterial returns the federation key columns for the
// user. ErrNoActorKeyMaterial when all key columns are NULL. The
// public-key bytes are returned in the clear; private-key bytes
// remain encrypted — use the Decrypt* helpers to unwrap.
//
// Federation hot-path: cache-backed. The cached value holds the
// at-rest (encrypted) form of the private keys; decryption
// happens lazily inside the Decrypt* helpers so plaintext keys
// never sit in the LRU.
func (h *Handler) GetActorKeyMaterial(ctx context.Context, userRef int64) (*ActorKeyMaterial, error) {
	if h.actorKeys != nil {
		if hit, ok := h.actorKeys.Get(actorKeyCacheKey(userRef)); ok {
			cp := hit
			return &cp, nil
		}
	}
	row, err := New(h.Pool).GetActorKeyMaterial(ctx, userRef)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("users: user %d not found", userRef)
		}
		return nil, err
	}
	if row.ActorUri == nil ||
		row.SigningPublicKeyPem == nil ||
		row.SigningPrivateKeyEnc == nil ||
		row.EncryptionPublicKey == nil ||
		row.EncryptionPrivateKeyEnc == nil {
		// NOTE: deliberately do NOT cache the "no keys yet" state
		// — the next call may be EnsureActorKeyMaterial, and
		// caching a sentinel here would force every code path to
		// invalidate before write.
		return nil, ErrNoActorKeyMaterial
	}
	out := &ActorKeyMaterial{
		ActorURI:                *row.ActorUri,
		SigningPublicKeyPEM:     []byte(*row.SigningPublicKeyPem),
		SigningPrivateKeyEnc:    row.SigningPrivateKeyEnc,
		EncryptionPublicKey:     row.EncryptionPublicKey,
		EncryptionPrivateKeyEnc: row.EncryptionPrivateKeyEnc,
	}
	if h.actorKeys != nil {
		h.actorKeys.Add(actorKeyCacheKey(userRef), *out)
	}
	return out, nil
}

// invalidateActorKeys evicts the cached entry for userRef + (via
// the registry's NOTIFY broadcast) every other process in the
// federation cluster. Called after EnsureActorKeyMaterial writes.
func (h *Handler) invalidateActorKeys(ctx context.Context, userRef int64) {
	if h.actorKeys == nil {
		return
	}
	if err := h.actorKeys.Invalidate(ctx, actorKeyCacheKey(userRef)); err != nil && h.Logger != nil {
		h.Logger.Warn("users.actor_keys.cache.invalidate.error",
			"user_ref", userRef,
			"err", err.Error(),
		)
	}
}

// EnsureActorKeyMaterial generates + persists a fresh keypair set
// for the user if they don't have one yet. Idempotent: if keys
// already exist, returns them unchanged.
//
// The actor URI is computed from baseURL + the user's username.
// baseURL is the operator's site base URL (sysconfig.Site.BaseURL);
// the caller passes it in rather than this package reaching for
// it directly so wiring stays explicit.
//
// Requires atrest.Initialised() == true. If the master key isn't
// loaded, returns an error and does not generate keys (we will
// not ship private keys to disk unwrapped).
func (h *Handler) EnsureActorKeyMaterial(ctx context.Context, userRef int64, baseURL, username string) (*ActorKeyMaterial, error) {
	// Fast path: already generated.
	if existing, err := h.GetActorKeyMaterial(ctx, userRef); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNoActorKeyMaterial) {
		return nil, err
	}
	if !atrest.Initialised() {
		return nil, errors.New("users: cannot generate actor keys without atrest master key (AA_MASTER_KEY)")
	}
	if baseURL == "" || username == "" {
		return nil, errors.New("users: baseURL and username are required to compose actor URI")
	}

	// 1. Generate the Ed25519 signing keypair.
	signPub, signPriv, err := federation.GenerateActorKeyPair()
	if err != nil {
		return nil, fmt.Errorf("users: generate signing keypair: %w", err)
	}
	signPubPEM, err := federation.PublicKeyToPEM(signPub)
	if err != nil {
		return nil, err
	}
	signPrivPEM, err := federation.PrivateKeyToPEM(signPriv)
	if err != nil {
		return nil, err
	}
	signPrivEnc, err := atrest.Encrypt(signPrivPEM)
	if err != nil {
		return nil, fmt.Errorf("users: encrypt signing private key: %w", err)
	}
	// Best-effort zero of the unencrypted PEM in memory after wrap.
	for i := range signPrivPEM {
		signPrivPEM[i] = 0
	}

	// 2. Generate the X25519 encryption keypair (distinct from
	// the signing keypair per spec §6.2).
	encPub, encPriv, err := nacl.GenerateActorEncryptionKeypair()
	if err != nil {
		return nil, fmt.Errorf("users: generate encryption keypair: %w", err)
	}
	encPrivEnc, err := atrest.Encrypt(encPriv)
	if err != nil {
		return nil, fmt.Errorf("users: encrypt encryption private key: %w", err)
	}
	for i := range encPriv {
		encPriv[i] = 0
	}

	// 3. Compose the actor URI.
	actorURI := baseURL + "/users/" + username

	// 4. Persist.
	signPubPEMStr := string(signPubPEM)
	if err := New(h.Pool).SetActorKeyMaterial(ctx, SetActorKeyMaterialParams{
		Ref:                     userRef,
		ActorUri:                &actorURI,
		SigningPublicKeyPem:     &signPubPEMStr,
		SigningPrivateKeyEnc:    signPrivEnc,
		EncryptionPublicKey:     encPub,
		EncryptionPrivateKeyEnc: encPrivEnc,
	}); err != nil {
		return nil, fmt.Errorf("users: persist actor keys: %w", err)
	}
	// Bust any stale cache entry — at v1 keys are written exactly
	// once per user lifetime (rotation deferred to 1.22.K) but
	// invalidating is cheap and defends against future re-issue
	// paths landing without re-thinking this code.
	h.invalidateActorKeys(ctx, userRef)
	return &ActorKeyMaterial{
		ActorURI:                actorURI,
		SigningPublicKeyPEM:     signPubPEM,
		SigningPrivateKeyEnc:    signPrivEnc,
		EncryptionPublicKey:     encPub,
		EncryptionPrivateKeyEnc: encPrivEnc,
	}, nil
}

// GetActorSigningPrivateKey decrypts + returns the Ed25519
// private-key PEM bytes for in-memory signing operations. The
// caller is responsible for zeroing the returned slice when done;
// the package does NOT keep a reference.
//
// SECURITY: do not log, print, persist, or pass to anything
// outside the signing operation that needs it.
func (m *ActorKeyMaterial) DecryptSigningPrivateKey() ([]byte, error) {
	return atrest.Decrypt(m.SigningPrivateKeyEnc)
}

// DecryptEncryptionPrivateKey decrypts + returns the raw 32-byte
// X25519 private scalar for in-memory NaCl-box decryption. Same
// caveats as DecryptSigningPrivateKey.
func (m *ActorKeyMaterial) DecryptEncryptionPrivateKey() ([]byte, error) {
	return atrest.Decrypt(m.EncryptionPrivateKeyEnc)
}
