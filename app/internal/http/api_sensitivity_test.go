// Phase 1.22.I-i — boot-wiring helper test for the receiver-side
// encryption policy gate's SensitivityLookup callback. Verifies
// the lookup correctly maps "asset" objectKinds to their
// sensitivity tier and falls through to SensitivityNotFound for
// unknown / missing rows.
//
// Real Postgres; skips without AA_DB_PASSWORD (same convention
// as the other federation integration suites).

package http

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

func openPoolForSensitivity(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOrSens("AA_DB_HOST", "postgres")
	port := envOrSens("AA_DB_PORT", "5432")
	user := envOrSens("AA_DB_USER", "artist_alley")
	name := envOrSens("AA_DB_NAME", "artist_alley")
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

func envOrSens(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// insertAssetAtTier creates a throwaway asset at the named
// sensitivity and registers cleanup.
func insertAssetAtTier(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tier string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, title, asset_type, status, sensitivity)
		 VALUES ($1, 'sensitivity-test', 1, 'active', $2)`,
		id, tier,
	); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

func TestInboxSensitivityLookup_AssetExists_ReturnsTier(t *testing.T) {
	pool := openPoolForSensitivity(t)
	defer pool.Close()
	ctx := context.Background()

	for _, tier := range []string{"public", "team", "restricted", "embargo"} {
		t.Run(tier, func(t *testing.T) {
			id := insertAssetAtTier(t, ctx, pool, tier)
			lookup := inboxSensitivityLookup(pool)
			got, err := lookup(ctx, "asset", id)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if string(got) != tier {
				t.Errorf("tier = %q, want %q", got, tier)
			}
		})
	}
}

func TestInboxSensitivityLookup_AssetMissing_ReturnsNotFound(t *testing.T) {
	pool := openPoolForSensitivity(t)
	defer pool.Close()
	ctx := context.Background()

	lookup := inboxSensitivityLookup(pool)
	got, err := lookup(ctx, "asset", uuid.New())
	if err != nil {
		t.Fatalf("missing-asset lookup should not error; got %v", err)
	}
	if got != inbox.SensitivityNotFound {
		t.Errorf("missing asset tier = %q, want SensitivityNotFound", got)
	}
}

func TestInboxSensitivityLookup_UnknownKind_PassesThrough(t *testing.T) {
	pool := openPoolForSensitivity(t)
	defer pool.Close()
	ctx := context.Background()

	lookup := inboxSensitivityLookup(pool)
	for _, kind := range []string{"post", "collection", "comment", "user", "futurekind"} {
		got, err := lookup(ctx, kind, uuid.New())
		if err != nil {
			t.Errorf("kind=%q errored: %v", kind, err)
		}
		if got != inbox.SensitivityNotFound {
			t.Errorf("kind=%q tier=%q, want SensitivityNotFound (no domain lookup wired)", kind, got)
		}
	}
}
