package testdb

import "testing"

// The guard's whole value is that it says NO to the dev database. These
// cases are the ones that decide whether it does.
func TestIsDisposable(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
		why  string
	}{
		// Refused — the databases that actually exist on the shared
		// stacks and would lose real data.
		{"artist_alley", false, "the dev database — the whole point"},
		{"postgres", false, "the maintenance database"},
		{"artist_alley_pkgtest", false,
			"ends in 'test' but is a real database on the coding stack; " +
				"a bare 'test' substring check would wrongly accept it"},
		{"artist_alley_kfscratch", false, "a real scratch database"},
		{"testing_grounds", false, "starts with 'test'; not a _test suffix"},
		{"", false, "empty is never a database name"},

		// Accepted — exactly what scripts/test.sh produces.
		{"artist_alley_test", true, "test.sh:47, primary checkout"},
		{"artist_alley_test_1a2b3c4d", true, "test.sh:72, linked worktree"},
		{SafeDefault, true, "the safe default must pass its own check"},
	} {
		if got := IsDisposable(tc.name); got != tc.want {
			t.Errorf("IsDisposable(%q) = %v, want %v — %s", tc.name, got, tc.want, tc.why)
		}
	}
}

// An unset AA_DB_NAME is the direct-`go test` case: it must land on a
// disposable database, not on the dev one.
func TestResolve_UnsetDefaultsToSafeDatabase(t *testing.T) {
	got, err := resolve("", false)
	if err != nil {
		t.Fatalf("unset must resolve, got error: %v", err)
	}
	if got != SafeDefault {
		t.Errorf("unset resolved to %q, want %q", got, SafeDefault)
	}
	if got == "artist_alley" {
		t.Error("unset resolved to the dev database — this is #1125")
	}
}

// Set-but-empty behaves as unset rather than refusing: an exported
// AA_DB_NAME= is a wrapper that computed nothing, not an operator
// naming a dangerous database.
func TestResolve_EmptyDefaultsToSafeDatabase(t *testing.T) {
	got, err := resolve("", true)
	if err != nil {
		t.Fatalf("empty must resolve, got error: %v", err)
	}
	if got != SafeDefault {
		t.Errorf("empty resolved to %q, want %q", got, SafeDefault)
	}
}

// An explicit dangerous name must REFUSE, not be silently redirected to
// the safe default. Redirection would hide the misconfiguration.
func TestResolve_ExplicitDevDatabaseRefuses(t *testing.T) {
	got, err := resolve("artist_alley", true)
	if err == nil {
		t.Fatalf("resolve(%q) returned %q with no error; it must refuse", "artist_alley", got)
	}
	if got != "" {
		t.Errorf("a refusal must not also hand back a name, got %q", got)
	}
	// The message has to name the offending value, or the operator
	// cannot tell which of several exports is at fault.
	if msg := err.Error(); !contains(msg, "artist_alley") || !contains(msg, EnvVar) {
		t.Errorf("refusal must name both %s and the rejected value; got: %s", EnvVar, msg)
	}
}

func TestResolve_ExplicitDisposableIsHonoured(t *testing.T) {
	const want = "artist_alley_test_deadbeef"
	got, err := resolve(want, true)
	if err != nil {
		t.Fatalf("resolve(%q): %v", want, err)
	}
	if got != want {
		t.Errorf("resolve(%q) = %q; an explicit disposable name must be used verbatim", want, got)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
