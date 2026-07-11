// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package store is the JSON-file-backed persistence layer for
// the reference directory server. Single-file, atomic-rename
// writes, mutex-guarded reads. Sized for "hundreds of listings,
// not millions" — anyone deploying for that scale should swap
// the backend for Postgres + reuse the same Store interface.
//
// Format on disk:
//
//   { "operator": { ... per /v1/operator response ... },
//     "listings": [ { ... per /v1/listing entry ... } ... ],
//     "challenges": { token: { instance_url, expires_at } } }
//
// All fields are stable + human-readable so operators can edit
// the file by hand for emergency moderation.

package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Errors callers may distinguish on.
var (
	ErrListingNotFound   = errors.New("store: listing not found")
	ErrChallengeNotFound = errors.New("store: challenge not found")
	ErrChallengeExpired  = errors.New("store: challenge expired")
)

// Operator is the directory's own identity record — returned by
// GET /v1/operator + the source-of-truth signing keypair (the
// private key lives on disk in a separate file; the store only
// holds the public half).
type Operator struct {
	Name           string `json:"name"`
	OperatorURL    string `json:"operator_url"`
	Contact        string `json:"contact"`
	SpecVersion    string `json:"spec_version"`
	PublicKeyPEM   string `json:"public_key_pem"`
	Fingerprint    string `json:"fingerprint"`
}

// Listing is one entry in the directory.
type Listing struct {
	InstanceURL          string    `json:"instance_url"`
	DisplayName          string    `json:"display_name"`
	InstancePublicKeyPEM string    `json:"instance_public_key_pem"`
	Fingerprint          string    `json:"fingerprint"`
	Region               string    `json:"region,omitempty"`
	Description          string    `json:"description,omitempty"`
	Tags                 []string  `json:"tags,omitempty"`
	VerifiedAt           time.Time `json:"verified_at"`
	VerifiedVia          string    `json:"verified_via"` // "dns-txt"
	ListingID            string    `json:"listing_id"`
	CreatedAt            time.Time `json:"created_at"`
}

// Challenge is an outstanding DNS-TXT challenge token.
type Challenge struct {
	Token       string    `json:"token"`
	InstanceURL string    `json:"instance_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// data is the on-disk root structure.
type data struct {
	Operator   Operator             `json:"operator"`
	Listings   []Listing            `json:"listings"`
	Challenges map[string]Challenge `json:"challenges"`
}

// Store guards in-memory state + atomic-rename writes back to
// the JSON file. Safe for concurrent use.
type Store struct {
	path string
	mu   sync.RWMutex
	d    data
}

// Open loads (or creates) a store at the given path. The parent
// directory MUST exist; we don't auto-create it.
func Open(path string) (*Store, error) {
	s := &Store{path: path, d: data{Challenges: map[string]Challenge{}}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s.d); err != nil {
		return nil, fmt.Errorf("store: parse %s: %w", path, err)
	}
	if s.d.Challenges == nil {
		s.d.Challenges = map[string]Challenge{}
	}
	return s, nil
}

// flush writes the current state to disk atomically. Caller
// holds the write lock.
func (s *Store) flush() error {
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "store-*.json.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s.d); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// --- operator -----------------------------------------------------------

// GetOperator returns the configured operator identity. Returns
// zero-value when SetOperator hasn't been called — the server
// fails the GET /v1/operator handler in that case.
func (s *Store) GetOperator() Operator {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.Operator
}

// SetOperator persists the operator identity. Called once on
// startup after the server has loaded its keypair from disk.
func (s *Store) SetOperator(op Operator) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Operator = op
	return s.flush()
}

// --- listings -----------------------------------------------------------

// ListListings returns up to limit entries sorted by VerifiedAt
// descending. cursor is ignored in this file backend (the dataset
// is small enough that pagination isn't load-bearing); for a
// Postgres backend the implementation would use it.
func (s *Store) ListListings(limit int) []Listing {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Listing(nil), s.d.Listings...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].VerifiedAt.After(out[j].VerifiedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// UpsertListing adds or replaces a listing by instance_url.
// Idempotent: re-registering the same URL with a different
// pubkey just updates the row.
func (s *Store) UpsertListing(l Listing) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.ListingID == "" {
		l.ListingID = newListingID()
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now().UTC()
	}
	for i, existing := range s.d.Listings {
		if existing.InstanceURL == l.InstanceURL {
			// Preserve the original CreatedAt + ListingID; everything
			// else may have changed.
			l.ListingID = existing.ListingID
			l.CreatedAt = existing.CreatedAt
			s.d.Listings[i] = l
			return s.flush()
		}
	}
	s.d.Listings = append(s.d.Listings, l)
	return s.flush()
}

// DeleteListingByURL removes a listing. Returns ErrListingNotFound
// when the URL isn't present.
func (s *Store) DeleteListingByURL(instanceURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.d.Listings {
		if existing.InstanceURL == instanceURL {
			s.d.Listings = append(s.d.Listings[:i], s.d.Listings[i+1:]...)
			return s.flush()
		}
	}
	return ErrListingNotFound
}

// --- challenges --------------------------------------------------------

// PutChallenge persists a fresh challenge. Token MUST be unique
// (the server generates from crypto/rand so collisions are
// astronomically unlikely).
func (s *Store) PutChallenge(c Challenge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Challenges[c.Token] = c
	return s.flush()
}

// ConsumeChallenge looks up + removes a challenge atomically.
// Returns ErrChallengeNotFound for unknown tokens,
// ErrChallengeExpired for past-deadline ones.
func (s *Store) ConsumeChallenge(token string) (Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.d.Challenges[token]
	if !ok {
		return Challenge{}, ErrChallengeNotFound
	}
	delete(s.d.Challenges, token)
	if err := s.flush(); err != nil {
		return Challenge{}, err
	}
	if time.Now().UTC().After(c.ExpiresAt) {
		return c, ErrChallengeExpired
	}
	return c, nil
}

// PruneExpiredChallenges drops any expired challenges. Called
// from a background goroutine in the server; safe to no-op if
// the directory is read-only-mostly.
func (s *Store) PruneExpiredChallenges() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	pruned := 0
	for token, c := range s.d.Challenges {
		if now.After(c.ExpiresAt) {
			delete(s.d.Challenges, token)
			pruned++
		}
	}
	if pruned == 0 {
		return 0, nil
	}
	if err := s.flush(); err != nil {
		return pruned, err
	}
	return pruned, nil
}

// --- helpers -----------------------------------------------------------

// newListingID generates a sortable timestamp-prefixed ID. Not
// cryptographically secure — listing IDs are opaque references,
// not auth tokens.
func newListingID() string {
	// 8-char ms timestamp + 8-char random suffix. Sortable in lex
	// order without needing ULID dep.
	now := time.Now().UTC().UnixNano() / int64(time.Millisecond)
	suffix, _ := randHex8()
	return fmt.Sprintf("L%016x%s", now, suffix)
}

func randHex8() (string, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(cryptoReader(), b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// cryptoReader is a package-level var so tests can substitute a
// deterministic reader if they need stable IDs. Defaults to
// crypto/rand.
var cryptoReader = func() io.Reader { return rand.Reader }
