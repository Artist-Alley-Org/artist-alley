// Package sysconfig is the typed access layer over the system_config
// table — the per-install settings an admin tunes through the UI
// (site name, base URL, SMTP credentials, etc.).
//
// Each "section" of admin-tunable settings is a single top-level key
// in the table whose JSONB value matches the Go struct declared here.
// The shapes are versioned by convention: never repurpose a key's
// fields, add new ones (with JSON `omitempty`) and migrate readers
// gradually.
package sysconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Keys recognised by the system. Stable strings; downstream tooling
// (admin UI, ops dashboards) keys off them.
const (
	KeySite = "site"
	KeySMTP = "smtp"
)

// Site holds the front-of-house identity for the install.
type Site struct {
	Name    string `json:"name"`              // "Acme Art Reviews"
	BaseURL string `json:"base_url"`          // "https://art.example.com"
}

// SMTPEncryption is one of: "none", "starttls", "tls". Anything else
// rejected at write time.
type SMTPEncryption string

const (
	SMTPEncryptionNone     SMTPEncryption = "none"
	SMTPEncryptionStartTLS SMTPEncryption = "starttls"
	SMTPEncryptionTLS      SMTPEncryption = "tls"
)

// SMTP is the outgoing email config. An empty Host means "email is not
// configured" — the app proceeds, but every feature that wants to
// send mail (password reset, notifications) becomes a no-op and logs.
//
// The in-memory Password field is plaintext; the persisted JSONB
// column carries `password_enc` (at-rest encrypted blob) when the
// Store has an [Encrypter] wired, falling back to a `password`
// plaintext field when it doesn't (dev / test).
type SMTP struct {
	Host       string         `json:"host"`
	Port       int            `json:"port"`
	Encryption SMTPEncryption `json:"encryption"`
	Username   string         `json:"username,omitempty"`
	Password   string         `json:"password,omitempty"`
	FromAddr   string         `json:"from_address"` // "ArtSite <noreply@example.com>"
}

// smtpStored is the wire-on-disk shape. Private — callers use SMTP
// + the Store's typed Get/Set methods. Keeps the encrypted blob out
// of the in-memory type so callers can't accidentally marshal it.
type smtpStored struct {
	Host        string         `json:"host"`
	Port        int            `json:"port"`
	Encryption  SMTPEncryption `json:"encryption"`
	Username    string         `json:"username,omitempty"`
	Password    string         `json:"password,omitempty"`     // plaintext fallback (no Encrypter)
	PasswordEnc []byte         `json:"password_enc,omitempty"` // atrest blob (with Encrypter)
	FromAddr    string         `json:"from_address"`
}

// Encrypter is the at-rest wrapping interface the Store uses for
// secret fields. In production this is satisfied by the atrest
// package; tests can pass a fake or leave it nil (plaintext mode).
type Encrypter interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// Store wraps the sqlc-generated Queries with typed Get/Set methods.
// Construct one at server boot; safe for concurrent use.
type Store struct {
	Pool *pgxpool.Pool
	enc  Encrypter
}

// NewStore builds a Store. Call [Store.WithEncrypter] post-construction
// to wire at-rest encryption for secret fields (production boot
// always does this; tests may leave it for plaintext-fallback mode).
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool}
}

// WithEncrypter attaches an at-rest encrypter for secret-bearing
// keys (currently just SMTP.Password). When set, new writes
// encrypt; reads decrypt if the stored row carries password_enc
// and fall through to the plaintext `password` field if only that
// is present (pre-encrypter rows or test fixtures).
func (s *Store) WithEncrypter(e Encrypter) *Store {
	s.enc = e
	return s
}

// GetSite returns the Site config or, if unset, an empty Site.
func (s *Store) GetSite(ctx context.Context) (Site, error) {
	var out Site
	if err := s.getKey(ctx, KeySite, &out); err != nil {
		return Site{}, err
	}
	return out, nil
}

// SetSite writes the Site config.
func (s *Store) SetSite(ctx context.Context, v Site) error {
	return s.setKey(ctx, KeySite, v)
}

// GetSMTP returns the SMTP config or, if unset, an empty SMTP.
// When the stored row carries an at-rest encrypted password
// (`password_enc`) the Store decrypts it; rows from before the
// encrypter was wired (or test fixtures) still read their
// plaintext `password` field as a fallback.
func (s *Store) GetSMTP(ctx context.Context) (SMTP, error) {
	var stored smtpStored
	if err := s.getKey(ctx, KeySMTP, &stored); err != nil {
		return SMTP{}, err
	}
	out := SMTP{
		Host:       stored.Host,
		Port:       stored.Port,
		Encryption: stored.Encryption,
		Username:   stored.Username,
		FromAddr:   stored.FromAddr,
	}
	switch {
	case len(stored.PasswordEnc) > 0 && s.enc != nil:
		pt, err := s.enc.Decrypt(stored.PasswordEnc)
		if err != nil {
			return SMTP{}, fmt.Errorf("sysconfig: decrypt smtp password: %w", err)
		}
		out.Password = string(pt)
	case stored.Password != "":
		out.Password = stored.Password
	}
	return out, nil
}

// SetSMTP validates and writes the SMTP config. With an
// [Encrypter] wired the Password field is encrypted at rest;
// without one, it's stored plaintext (dev / test).
func (s *Store) SetSMTP(ctx context.Context, v SMTP) error {
	if v.Host != "" {
		switch v.Encryption {
		case SMTPEncryptionNone, SMTPEncryptionStartTLS, SMTPEncryptionTLS:
		default:
			return fmt.Errorf("sysconfig: smtp encryption must be none|starttls|tls, got %q", v.Encryption)
		}
		if v.Port <= 0 || v.Port > 65535 {
			return fmt.Errorf("sysconfig: smtp port must be 1..65535, got %d", v.Port)
		}
	}
	stored, err := s.toStored(v)
	if err != nil {
		return err
	}
	return s.setKey(ctx, KeySMTP, stored)
}

// SetSiteAndSMTPTx writes both keys inside an externally-managed
// transaction — used by the setup wizard so the admin user, site
// config, and SMTP config all commit atomically.
func (s *Store) SetSiteAndSMTPTx(ctx context.Context, tx pgx.Tx, site Site, smtp SMTP) error {
	q := New(tx)
	siteJSON, err := json.Marshal(site)
	if err != nil {
		return fmt.Errorf("sysconfig: marshal site: %w", err)
	}
	if err := q.UpsertSystemConfig(ctx, UpsertSystemConfigParams{Key: KeySite, Value: siteJSON}); err != nil {
		return fmt.Errorf("sysconfig: write site: %w", err)
	}
	stored, err := s.toStored(smtp)
	if err != nil {
		return err
	}
	smtpJSON, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("sysconfig: marshal smtp: %w", err)
	}
	if err := q.UpsertSystemConfig(ctx, UpsertSystemConfigParams{Key: KeySMTP, Value: smtpJSON}); err != nil {
		return fmt.Errorf("sysconfig: write smtp: %w", err)
	}
	return nil
}

// toStored translates the in-memory SMTP into the on-disk shape,
// encrypting the password when an [Encrypter] is wired.
func (s *Store) toStored(v SMTP) (smtpStored, error) {
	out := smtpStored{
		Host:       v.Host,
		Port:       v.Port,
		Encryption: v.Encryption,
		Username:   v.Username,
		FromAddr:   v.FromAddr,
	}
	if v.Password == "" {
		return out, nil
	}
	if s.enc == nil {
		out.Password = v.Password
		return out, nil
	}
	enc, err := s.enc.Encrypt([]byte(v.Password))
	if err != nil {
		return smtpStored{}, fmt.Errorf("sysconfig: encrypt smtp password: %w", err)
	}
	out.PasswordEnc = enc
	return out, nil
}

func (s *Store) getKey(ctx context.Context, key string, dst any) error {
	row, err := New(s.Pool).GetSystemConfig(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // leave dst zero-valued
		}
		return fmt.Errorf("sysconfig: get %q: %w", key, err)
	}
	if err := json.Unmarshal(row.Value, dst); err != nil {
		return fmt.Errorf("sysconfig: unmarshal %q: %w", key, err)
	}
	return nil
}

func (s *Store) setKey(ctx context.Context, key string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("sysconfig: marshal %q: %w", key, err)
	}
	if err := New(s.Pool).UpsertSystemConfig(ctx, UpsertSystemConfigParams{
		Key:   key,
		Value: payload,
	}); err != nil {
		return fmt.Errorf("sysconfig: upsert %q: %w", key, err)
	}
	return nil
}
