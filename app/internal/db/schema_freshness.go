package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaStatus classifies the delta between the DB's applied
// migration head (goose_db_version.MAX(version_id)) and the max
// numeric prefix found in the embedded migrations FS.
type SchemaStatus int

const (
	// SchemaOK — DB head == embedded max. The running binary was
	// built from the same migration set the DB has applied.
	SchemaOK SchemaStatus = iota

	// SchemaUnappliedMigrations — embedded max > DB head. The
	// binary knows about migrations the DB hasn't run yet. Under
	// normal boot this shouldn't happen (Migrate ran and returned
	// nil), so this is a defensive check against goose bugs,
	// partial-apply crashes, or someone running the binary against
	// a DB that isn't caught up.
	SchemaUnappliedMigrations

	// SchemaUnknownNewerSchema — DB head > embedded max. The DB
	// has migrations applied that this binary doesn't know about,
	// which happens when running an OLD binary against a NEWER DB
	// (e.g., during a bad rollback). The binary can't guarantee it
	// understands the schema shape and should surface a warning.
	SchemaUnknownNewerSchema
)

// String makes SchemaStatus greppable in logs.
func (s SchemaStatus) String() string {
	switch s {
	case SchemaOK:
		return "ok"
	case SchemaUnappliedMigrations:
		return "unapplied_migrations"
	case SchemaUnknownNewerSchema:
		return "unknown_newer_schema"
	default:
		return "unknown"
	}
}

// SchemaFreshness is the structured result of a freshness check.
// Callers surface Warning (may be empty) on /admin/system/health;
// Status drives the boot-time refuse-to-start decision.
type SchemaFreshness struct {
	Status         SchemaStatus
	EmbeddedMaxVer int64
	DBMaxVer       int64
	// Warning is a human-readable one-liner suitable for admin
	// dashboards. Empty when Status == SchemaOK.
	Warning string
}

// CheckSchemaFreshness compares the DB's current max applied
// migration version_id against the highest numeric prefix in the
// embedded migrations FS. Returns SchemaFreshness with the classified
// Status + a Warning string ready for /admin/system/health.
//
// Boot behaviour is the caller's decision: for SchemaOK proceed
// normally; for SchemaUnappliedMigrations log ERROR + refuse to
// start; for SchemaUnknownNewerSchema log WARN + continue. This
// function does NOT return a Go error for either mismatch — the
// caller reads Status and picks the action.
//
// Errors here are strictly transport / query / FS failures (DB
// unreachable, embed corrupt, etc.). Those bubble up as regular
// errors.
func CheckSchemaFreshness(ctx context.Context, pool *pgxpool.Pool) (SchemaFreshness, error) {
	embeddedMax, err := embeddedMaxVersion()
	if err != nil {
		return SchemaFreshness{}, fmt.Errorf("schema freshness: embedded max: %w", err)
	}
	dbMax, err := dbMaxVersion(ctx, pool)
	if err != nil {
		return SchemaFreshness{}, fmt.Errorf("schema freshness: db max: %w", err)
	}

	out := SchemaFreshness{
		EmbeddedMaxVer: embeddedMax,
		DBMaxVer:       dbMax,
	}
	switch {
	case dbMax == embeddedMax:
		out.Status = SchemaOK
	case embeddedMax > dbMax:
		out.Status = SchemaUnappliedMigrations
		out.Warning = fmt.Sprintf(
			"binary embeds %d migrations but DB has applied %d — %d unapplied",
			embeddedMax, dbMax, embeddedMax-dbMax,
		)
	default: // dbMax > embeddedMax
		out.Status = SchemaUnknownNewerSchema
		out.Warning = fmt.Sprintf(
			"DB has applied %d migrations but binary only embeds %d — running old code against newer schema",
			dbMax, embeddedMax,
		)
	}
	return out, nil
}

// versionPrefix matches the NNNNN_… prefix on a migration filename.
// The 5-digit convention was locked with the baseline squash per
// ADR 0046; the parser refuses shorter/longer prefixes to catch
// naming drift early.
var versionPrefix = regexp.MustCompile(`^(\d{5})_.*\.sql$`)

// embeddedMaxVersion walks migrationsFS and returns the highest
// numeric prefix present. Prefixes that don't parse are silently
// skipped so goose helper files don't confuse the count; only
// canonical N-digit migration filenames contribute.
func embeddedMaxVersion() (int64, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0, fmt.Errorf("read migrations dir: %w", err)
	}
	var versions []int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := versionPrefix.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		versions = append(versions, v)
	}
	if len(versions) == 0 {
		return 0, errors.New("no canonical migration filenames found in embedded FS")
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions[len(versions)-1], nil
}

// dbMaxVersion returns the MAX(version_id) from goose_db_version.
// Returns 0 if the table is absent (very first boot on a fresh DB —
// though callers should never reach this branch because Migrate
// runs first and creates the table).
func dbMaxVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var v int64
	err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version`).Scan(&v)
	if err == nil {
		return v, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return 0, err
}
