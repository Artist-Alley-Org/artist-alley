package sysconfig_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
