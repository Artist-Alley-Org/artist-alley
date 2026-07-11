// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// TestSiteRoundTrip writes and reads a Site config, confirming the
// JSONB serialization preserves every field.
func TestSiteRoundTrip(t *testing.T) {
	withStore(t, func(ctx context.Context, store *sysconfig.Store) {
		want := sysconfig.Site{
			Name:    "Studio Alpha",
			BaseURL: "https://art.studio-alpha.example.com",
		}
		if err := store.SetSite(ctx, want); err != nil {
			t.Fatalf("SetSite: %v", err)
		}
		got, err := store.GetSite(ctx)
		if err != nil {
			t.Fatalf("GetSite: %v", err)
		}
		if got != want {
			t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", got, want)
		}
	})
}

// TestGetSiteUnset returns the zero value, not an error, when the
// site key has never been written. Setup wizard relies on this.
func TestGetSiteUnset(t *testing.T) {
	withStore(t, func(ctx context.Context, store *sysconfig.Store) {
		// withStore pre-cleans, so the row is absent.
		got, err := store.GetSite(ctx)
		if err != nil {
			t.Fatalf("GetSite on unset key: %v", err)
		}
		if got != (sysconfig.Site{}) {
			t.Errorf("expected zero Site, got %+v", got)
		}
	})
}

// TestSMTPRoundTrip exercises the full SMTP struct.
func TestSMTPRoundTrip(t *testing.T) {
	withStore(t, func(ctx context.Context, store *sysconfig.Store) {
		want := sysconfig.SMTP{
			Host:       "smtp.example.com",
			Port:       587,
			Encryption: sysconfig.SMTPEncryptionStartTLS,
			Username:   "noreply",
			Password:   "s3cret",
			FromAddr:   "ArtSite <noreply@example.com>",
		}
		if err := store.SetSMTP(ctx, want); err != nil {
			t.Fatalf("SetSMTP: %v", err)
		}
		got, err := store.GetSMTP(ctx)
		if err != nil {
			t.Fatalf("GetSMTP: %v", err)
		}
		if got != want {
			t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", got, want)
		}
	})
}

// TestSMTPPassword_EncryptedAtRest confirms that with an Encrypter
// wired the persisted JSONB row carries `password_enc` (an opaque
// blob) instead of plaintext `password`. Read-back transparently
// decrypts so the in-memory value the caller sees is the original.
func TestSMTPPassword_EncryptedAtRest(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; sysconfig integration test skipped")
	}
	// Stand up a master key for this test (Reset on exit so other
	// tests aren't affected — atrest is process-global).
	key := make([]byte, atrest.MasterKeyLen)
	for i := range key {
		key[i] = byte(i ^ 0x5a)
	}
	if err := atrest.InitWithKey(key); err != nil {
		t.Fatalf("InitWithKey: %v", err)
	}
	t.Cleanup(atrest.Reset)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()
	clean := func() { _, _ = pool.Exec(context.Background(), `DELETE FROM system_config WHERE key = 'smtp'`) }
	clean()
	t.Cleanup(clean)

	store := sysconfig.NewStore(pool).WithEncrypter(atrest.PackageEncrypter{})

	const plaintext = "unguessable-secret-abc123"
	in := sysconfig.SMTP{
		Host: "smtp.example.com", Port: 587,
		Encryption: sysconfig.SMTPEncryptionStartTLS,
		Username:   "noreply", Password: plaintext,
		FromAddr: "noreply@example.com",
	}
	if err := store.SetSMTP(ctx, in); err != nil {
		t.Fatalf("SetSMTP: %v", err)
	}

	// Round-trip via the Store gives plaintext back.
	got, err := store.GetSMTP(ctx)
	if err != nil {
		t.Fatalf("GetSMTP: %v", err)
	}
	if got.Password != plaintext {
		t.Errorf("round-trip password = %q, want %q", got.Password, plaintext)
	}

	// Raw JSONB column MUST NOT contain the plaintext (sanity check
	// — guards against accidentally regressing the marshal path).
	var raw string
	if err := pool.QueryRow(ctx,
		`SELECT value::text FROM system_config WHERE key = 'smtp'`).Scan(&raw); err != nil {
		t.Fatalf("read raw smtp row: %v", err)
	}
	if strings.Contains(raw, plaintext) {
		t.Errorf("plaintext password leaked into raw JSONB row: %s", raw)
	}
	if !strings.Contains(raw, "password_enc") {
		t.Errorf("expected password_enc field in raw JSONB row, got: %s", raw)
	}

	// Reading WITHOUT an encrypter just leaves Password blank — the
	// cipher is opaque to a Store that can't decrypt.
	bareStore := sysconfig.NewStore(pool)
	bare, err := bareStore.GetSMTP(ctx)
	if err != nil {
		t.Fatalf("bare GetSMTP: %v", err)
	}
	if bare.Password != "" {
		t.Errorf("bare-store Password = %q, want empty (cipher should be opaque)", bare.Password)
	}
	if bare.Host != "smtp.example.com" {
		t.Errorf("bare-store Host = %q, want smtp.example.com (other fields should still round-trip)", bare.Host)
	}
}

// TestSMTPValidation rejects bad encryption / port when host is set.
// An empty Host is allowed and skips validation (email unconfigured).
func TestSMTPValidation(t *testing.T) {
	cases := []struct {
		name    string
		in      sysconfig.SMTP
		wantErr string // substring; "" means accept
	}{
		{
			name:    "valid starttls",
			in:      sysconfig.SMTP{Host: "smtp.example.com", Port: 587, Encryption: sysconfig.SMTPEncryptionStartTLS, FromAddr: "x@x.com"},
			wantErr: "",
		},
		{
			name:    "empty host skips validation",
			in:      sysconfig.SMTP{},
			wantErr: "",
		},
		{
			name:    "unknown encryption rejected",
			in:      sysconfig.SMTP{Host: "smtp.example.com", Port: 587, Encryption: "weakssl", FromAddr: "x@x.com"},
			wantErr: "encryption",
		},
		{
			name:    "port too low rejected",
			in:      sysconfig.SMTP{Host: "smtp.example.com", Port: 0, Encryption: sysconfig.SMTPEncryptionTLS, FromAddr: "x@x.com"},
			wantErr: "port",
		},
		{
			name:    "port too high rejected",
			in:      sysconfig.SMTP{Host: "smtp.example.com", Port: 70000, Encryption: sysconfig.SMTPEncryptionTLS, FromAddr: "x@x.com"},
			wantErr: "port",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withStore(t, func(ctx context.Context, store *sysconfig.Store) {
				err := store.SetSMTP(ctx, tc.in)
				if tc.wantErr == "" {
					if err != nil {
						t.Errorf("unexpected error: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
			})
		})
	}
}

// TestSetSiteAndSMTPTx commits both keys atomically. Tested by
// staging two writes in a tx, committing, then verifying both rows
// landed.
func TestSetSiteAndSMTPTx(t *testing.T) {
	withStore(t, func(ctx context.Context, store *sysconfig.Store) {
		tx, err := store.Pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		site := sysconfig.Site{Name: "Tx Site", BaseURL: "https://tx.example.com"}
		smtp := sysconfig.SMTP{Host: "tx-smtp.example.com", Port: 25, Encryption: sysconfig.SMTPEncryptionNone, FromAddr: "tx@example.com"}
		if err := store.SetSiteAndSMTPTx(ctx, tx, site, smtp); err != nil {
			t.Fatalf("SetSiteAndSMTPTx: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		gotSite, err := store.GetSite(ctx)
		if err != nil || gotSite != site {
			t.Errorf("site after tx: got %+v err %v", gotSite, err)
		}
		gotSMTP, err := store.GetSMTP(ctx)
		if err != nil || gotSMTP != smtp {
			t.Errorf("smtp after tx: got %+v err %v", gotSMTP, err)
		}
	})
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

func withStore(t *testing.T, fn func(context.Context, *sysconfig.Store)) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; sysconfig integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()
	store := sysconfig.NewStore(pool)

	// Pre- and post-clean the keys this test set touches.
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM system_config WHERE key IN ('site','smtp')`)
	}
	clean()
	t.Cleanup(clean)

	fn(ctx, store)
}

func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
