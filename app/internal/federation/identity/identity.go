// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package identity holds the per-instance Ed25519 keypair the
// federation transport layer uses to sign + verify peer-to-peer
// messages. Distinct from per-actor keys (those live on the
// "user" table via migration 00001) — this is the INSTANCE-level
// identity, the equivalent of an SSH host key.
//
// # Storage
//
// One row in system_config under the key
// federation.instance_identity. Public key is plain PEM; private
// key is wrapped by app/internal/atrest (AES-256-GCM with the
// host master key) so a database dump never yields the signing
// material.
//
// # Lifecycle
//
// GenerateIfMissing is called at boot. If no row exists, we
// generate a fresh Ed25519 keypair, wrap the private key, write
// the row. Idempotent — if a row already exists we just load.
// Rotation is deferred to a later phase (same reason per-user
// keys defer per docs/spec/federation/v1.md §14).
//
// # Caching
//
// The loaded keypair lives in a package-level Identity struct
// held by the singleton Manager. Every Sign call uses the
// already-decrypted private key from memory — no per-request
// DB hit, no per-request atrest decryption. The federation hot
// path can sign at memory speed.

package identity

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// sysconfigKey is the row key under which the instance identity
// is persisted in system_config. Stable string — published as a
// const so future tooling (operator scripts, key-rotation
// tooling) can find it.
const sysconfigKey = "federation.instance_identity"

// stored is the wire shape inside the system_config JSONB cell.
// Public PEM is human-readable; private key is base64'd
// ciphertext from atrest.Encrypt.
type stored struct {
	PublicKeyPEM     string    `json:"public_key_pem"`
	PrivateKeyEncB64 string    `json:"private_key_enc_b64"`
	GeneratedAt      time.Time `json:"generated_at"`
}

// Identity is the in-memory view of the loaded instance keypair.
// Held by Manager so Sign + PublicKey are memory-fast.
type Identity struct {
	publicKey   ed25519.PublicKey
	privateKey  ed25519.PrivateKey
	publicPEM   []byte
	generatedAt time.Time
}

// PublicKey returns the raw 32-byte Ed25519 public key. Useful
// for fingerprinting + the federation envelope's publicKey URL
// hint.
func (id *Identity) PublicKey() ed25519.PublicKey { return id.publicKey }

// PublicKeyPEM returns the PEM-wrapped public key — the form
// the actor doc + handshake payloads carry.
func (id *Identity) PublicKeyPEM() []byte { return id.publicPEM }

// Fingerprint returns lowercase hex SHA-256 of the raw public
// key — what the admin UI shows for out-of-band verification.
func (id *Identity) Fingerprint() string {
	return federation.PublicKeyFingerprint(id.publicKey)
}

// GeneratedAt returns when the keypair was first persisted —
// useful for the admin UI's "this instance's identity" panel.
func (id *Identity) GeneratedAt() time.Time { return id.generatedAt }

// Sign produces a 64-byte Ed25519 signature over msg using the
// instance private key. Memory-fast — no DB, no decrypt.
func (id *Identity) Sign(msg []byte) []byte {
	return federation.Sign(id.privateKey, msg)
}

// PrivateKey returns the in-memory ed25519 private key. Exposed
// for callers that need to integrate with libraries expecting
// the raw key type (e.g. httpsig.SignAndAttach). Internal
// package — boot wiring is the only call site outside this
// package; everything else should prefer Sign() above.
func (id *Identity) PrivateKey() ed25519.PrivateKey { return id.privateKey }

// Verify is a convenience for callers verifying signatures
// against THIS instance's public key (e.g. self-test paths).
// Production paths verifying PEER signatures should call
// federation.Verify directly with the peer's published key.
func (id *Identity) Verify(msg, sig []byte) error {
	return federation.Verify(id.publicKey, msg, sig)
}

// Manager is the per-process owner of the instance identity.
// Constructed once at boot via Load (which calls
// GenerateIfMissing). Safe for concurrent use — the loaded
// Identity is immutable once cached.
type Manager struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	mu       sync.RWMutex
	identity *Identity
}

// NewManager wires the package. Call Load to populate.
func NewManager(pool *pgxpool.Pool, logger *slog.Logger) *Manager {
	return &Manager{pool: pool, logger: logger}
}

// Errors callers may distinguish on.
var (
	// ErrNotLoaded indicates Identity was called before Load.
	// Boot must call Load before any federation HTTP handler
	// reaches the sign path; this guards against ordering bugs.
	ErrNotLoaded = errors.New("identity: not loaded; call Manager.Load at boot")

	// ErrAtrestUnavailable indicates Load was called before the
	// atrest master key was initialised. Federation can't sign
	// without it; surface a clear error rather than panicking.
	ErrAtrestUnavailable = errors.New("identity: atrest master key not initialised (AA_MASTER_KEY required)")
)

// Load fetches the persisted identity, generating + persisting
// a fresh keypair if no row exists. Idempotent — safe to call
// multiple times at boot. Returns the loaded Identity.
//
// Requires atrest.Initialised() == true. If the master key
// isn't available we fail fast: federation can't sign without
// a private key + we won't ship plaintext private keys to disk
// to work around it.
func (m *Manager) Load(ctx context.Context) (*Identity, error) {
	if !atrest.Initialised() {
		return nil, ErrAtrestUnavailable
	}
	m.mu.RLock()
	if m.identity != nil {
		id := m.identity
		m.mu.RUnlock()
		return id, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	// Double-check after acquiring write lock — another
	// goroutine may have loaded while we waited.
	if m.identity != nil {
		return m.identity, nil
	}

	row, err := m.fetch(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// First-boot path: generate + persist.
		row, err = m.generateAndStore(ctx)
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}

	id, err := decodeIdentity(row)
	if err != nil {
		// Auto-regenerate on at-rest auth failure. This happens
		// when AA_MASTER_KEY has rotated since the row was
		// written — the row is now permanently unrecoverable, so
		// continuing to surface it as "broken" leaves the app in
		// a half-working state where /federation/instance silently
		// 503s but the healthcheck passes (the misleading-log bug
		// that ate a dogfood-pairing session). Regenerate, log
		// LOUD that peers must re-pair, continue.
		//
		// Other decode errors (PEM malformed, key-pair mismatch,
		// etc.) propagate normally — those signal a corrupted row
		// that bisected investigation is needed for.
		if errors.Is(err, atrest.ErrBadCiphertext) {
			if m.logger != nil {
				m.logger.LogAttrs(ctx, slog.LevelWarn,
					"federation.identity.regenerated.after.key.drift",
					slog.String("reason", "stored identity could not be decrypted with current AA_MASTER_KEY — regenerating"),
					slog.String("impact", "all paired peers must re-pair: this instance's federation fingerprint will change"),
				)
			}
			row, err = m.generateAndStore(ctx)
			if err != nil {
				return nil, fmt.Errorf("regenerate after key drift: %w", err)
			}
			id, err = decodeIdentity(row)
			if err != nil {
				return nil, fmt.Errorf("decode after regenerate: %w", err)
			}
		} else {
			return nil, err
		}
	}
	m.identity = id
	if m.logger != nil {
		m.logger.LogAttrs(ctx, slog.LevelInfo, "federation.identity.loaded",
			slog.String("fingerprint", id.Fingerprint()),
			slog.Time("generated_at", id.GeneratedAt()),
		)
	}
	return id, nil
}

// Get returns the cached identity. Returns ErrNotLoaded if Load
// hasn't been called — the federation HTTP layer treats this as
// "instance identity unavailable, return 503 on inbound
// handshake".
func (m *Manager) Get() (*Identity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.identity == nil {
		return nil, ErrNotLoaded
	}
	return m.identity, nil
}

// --- internal helpers ----------------------------------------------------

// fetch reads the system_config row. Returns pgx.ErrNoRows when
// no identity has been persisted yet.
func (m *Manager) fetch(ctx context.Context) (stored, error) {
	var raw []byte
	err := m.pool.QueryRow(ctx,
		`SELECT value FROM system_config WHERE key = $1`, sysconfigKey,
	).Scan(&raw)
	if err != nil {
		return stored{}, err
	}
	var s stored
	if err := json.Unmarshal(raw, &s); err != nil {
		return stored{}, fmt.Errorf("identity: decode stored value: %w", err)
	}
	return s, nil
}

// generateAndStore mints a fresh Ed25519 keypair, wraps the
// private key via atrest, writes the system_config row, returns
// the stored shape.
func (m *Manager) generateAndStore(ctx context.Context) (stored, error) {
	pub, priv, err := federation.GenerateActorKeyPair()
	if err != nil {
		return stored{}, fmt.Errorf("identity: generate keypair: %w", err)
	}
	pubPEM, err := federation.PublicKeyToPEM(pub)
	if err != nil {
		return stored{}, err
	}
	privPEM, err := federation.PrivateKeyToPEM(priv)
	if err != nil {
		return stored{}, err
	}
	privEnc, err := atrest.Encrypt(privPEM)
	if err != nil {
		return stored{}, fmt.Errorf("identity: wrap private key: %w", err)
	}
	// Best-effort zero the plaintext PEM in memory.
	for i := range privPEM {
		privPEM[i] = 0
	}

	s := stored{
		PublicKeyPEM:     string(pubPEM),
		PrivateKeyEncB64: base64.StdEncoding.EncodeToString(privEnc),
		GeneratedAt:      time.Now().UTC(),
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return stored{}, fmt.Errorf("identity: encode stored value: %w", err)
	}
	if _, err := m.pool.Exec(ctx,
		`INSERT INTO system_config (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
		sysconfigKey, payload,
	); err != nil {
		return stored{}, fmt.Errorf("identity: persist: %w", err)
	}
	if m.logger != nil {
		m.logger.LogAttrs(ctx, slog.LevelInfo, "federation.identity.generated",
			slog.String("fingerprint", federation.PublicKeyFingerprint(pub)),
		)
	}
	return s, nil
}

// decodeIdentity turns a persisted row into an in-memory
// Identity. Unwraps the private key via atrest, parses both
// keys, sanity-checks they're a matching pair.
func decodeIdentity(s stored) (*Identity, error) {
	pub, err := federation.PublicKeyFromPEM([]byte(s.PublicKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("identity: decode public PEM: %w", err)
	}
	privEnc, err := base64.StdEncoding.DecodeString(s.PrivateKeyEncB64)
	if err != nil {
		return nil, fmt.Errorf("identity: decode private ciphertext: %w", err)
	}
	privPEM, err := atrest.Decrypt(privEnc)
	if err != nil {
		return nil, fmt.Errorf("identity: unwrap private key: %w", err)
	}
	priv, err := federation.PrivateKeyFromPEM(privPEM)
	if err != nil {
		// Zero the plaintext on any error before bubbling up.
		for i := range privPEM {
			privPEM[i] = 0
		}
		return nil, fmt.Errorf("identity: decode private PEM: %w", err)
	}
	// Sanity: derived public must equal stored public. Catches
	// the rare case of a tampered cell where pub + priv mismatch.
	derivedPub, ok := priv.Public().(ed25519.PublicKey)
	if !ok || string(derivedPub) != string(pub) {
		for i := range privPEM {
			privPEM[i] = 0
		}
		return nil, errors.New("identity: stored public key does not match private key")
	}
	id := &Identity{
		publicKey:   pub,
		privateKey:  priv,
		publicPEM:   []byte(s.PublicKeyPEM),
		generatedAt: s.GeneratedAt,
	}
	// We keep the plaintext private PEM out of memory — only the
	// parsed ed25519.PrivateKey is retained.
	for i := range privPEM {
		privPEM[i] = 0
	}
	return id, nil
}
