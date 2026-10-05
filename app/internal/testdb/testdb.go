// Package testdb resolves the Postgres database that integration tests
// are allowed to touch, and refuses any database that is not disposable.
//
// WHY THIS EXISTS (#1125)
// -----------------------
// The Go suite is destructive by design: fixtures TRUNCATE and DELETE
// shared tables (`user`, `user_roles`, `jobs`, `goose_db_version`).
// `scripts/test.sh` therefore points the whole run at a disposable
// `<dev>_test` database and resets it each run.
//
// Nothing enforced that. Every test file resolved the database itself
// with its own copy of an `envOr` helper, and every copy defaulted to
// `"artist_alley"` — the DEV database. Under `scripts/test.sh` that
// default is never reached, because the script exports AA_DB_NAME. But
// a DIRECT `go test ./app/internal/auth/...` inherits no such export,
// falls through to the default, and runs the destructive fixtures
// against dev data. That is the whole hazard: the safe path was a
// property of the *wrapper*, not of the tests.
//
// There was no single seam to guard — the literal
// `envOr("AA_DB_NAME", "artist_alley")` appeared in 46 test files
// across 17 differently-named local helpers. This package is that
// seam, and the call sites were rewritten to go through it.
//
// TWO PROTECTIONS, NOT ONE
// ------------------------
//  1. The default is SAFE. With AA_DB_NAME unset the answer is
//     `artist_alley_test`, never the dev database. A default that is
//     wrong only when someone forgets is not a default worth having.
//  2. An explicit value is still CHECKED, and a bad one REFUSES —
//     loudly, with the name it objected to. A default can be overridden
//     by the same forgetfulness it protects against; a refusal cannot.
//     Silently redirecting a wrong target to a right one would swap a
//     visible failure for an invisible one, which is the shape of the
//     bug being fixed.
package testdb

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// EnvVar is the environment variable that names the test database.
const EnvVar = "AA_DB_NAME"

// SafeDefault is used when EnvVar is unset or empty. It is deliberately
// NOT the dev database name.
const SafeDefault = "artist_alley_test"

// Name returns the database integration tests may use, failing the test
// immediately if the configured name is not a disposable test database.
//
// It takes a testing.TB rather than reading a package-level value so the
// check happens when a test actually asks to connect — past the
// `AA_DB_PASSWORD` skip that unit-only runs rely on. A package-level
// init() check would fire in `go test ./...` with no database at all,
// turning skipped integration tests into hard failures.
func Name(tb testing.TB) string {
	tb.Helper()
	name, err := resolve(os.LookupEnv(EnvVar))
	if err != nil {
		// Fatalf, not Skipf: a run pointed at the wrong database must
		// stop, and must say so. A skip would read as "nothing to do
		// here", which is exactly the silence this guards against.
		tb.Fatalf("%v", err)
	}
	return name
}

// MustName is Name for callers that have no testing.TB — TestMain, which
// receives only a *testing.M. It panics rather than returning an error
// because there is no test to fail yet and no safe way to continue.
func MustName() string {
	name, err := resolve(os.LookupEnv(EnvVar))
	if err != nil {
		panic(err.Error())
	}
	return name
}

// IsDisposable reports whether name looks like a database the suite is
// allowed to destroy.
//
// scripts/test.sh produces two shapes and both must pass:
//
//	artist_alley_test            (primary checkout, test.sh:47)
//	artist_alley_test_1a2b3c4d   (linked worktree, test.sh:72)
//
// The marker is the underscore-delimited segment "_test", so a name
// merely *ending* in "test" — `artist_alley_pkgtest`, a real database
// on the shared coding stack — is correctly refused. Matching a bare
// "test" substring would accept it, and accepting a database because
// its name happens to contain four letters is not a check.
func IsDisposable(name string) bool {
	return strings.HasSuffix(name, "_test") || strings.Contains(name, "_test_")
}

func resolve(raw string, ok bool) (string, error) {
	name := raw
	if !ok || name == "" {
		name = SafeDefault
	}
	if !IsDisposable(name) {
		return "", fmt.Errorf(
			"refusing to run destructive integration tests against %s=%q: "+
				"the name must contain a %q segment (e.g. %q), because this suite "+
				"TRUNCATEs and DELETEs shared tables and would destroy real data.\n"+
				"       Run the suite with ./scripts/test.sh, which provisions and "+
				"resets a disposable database, or export %s=%s for a direct `go test`.",
			EnvVar, name, "_test", SafeDefault, EnvVar, SafeDefault)
	}
	return name, nil
}
