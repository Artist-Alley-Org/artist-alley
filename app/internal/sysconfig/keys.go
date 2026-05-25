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
type SMTP struct {
	Host       string         `json:"host"`
	Port       int            `json:"port"`
	Encryption SMTPEncryption `json:"encryption"`
	Username   string         `json:"username,omitempty"`
	Password   string         `json:"password,omitempty"` // plaintext for now; see migration 00009 comment
	FromAddr   string         `json:"from_address"`        // "ArtSite <noreply@example.com>"
}

// Store wraps the sqlc-generated Queries with typed Get/Set methods.
// Construct one at server boot; safe for concurrent use.
type Store struct {
	Pool *pgxpool.Pool
}

// NewStore builds a Store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool}
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
func (s *Store) GetSMTP(ctx context.Context) (SMTP, error) {
	var out SMTP
	if err := s.getKey(ctx, KeySMTP, &out); err != nil {
		return SMTP{}, err
	}
	return out, nil
}

// SetSMTP validates and writes the SMTP config.
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
	return s.setKey(ctx, KeySMTP, v)
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
	smtpJSON, err := json.Marshal(smtp)
	if err != nil {
		return fmt.Errorf("sysconfig: marshal smtp: %w", err)
	}
	if err := q.UpsertSystemConfig(ctx, UpsertSystemConfigParams{Key: KeySMTP, Value: smtpJSON}); err != nil {
		return fmt.Errorf("sysconfig: write smtp: %w", err)
	}
	return nil
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
