// Phase 1.17.D — changeset helper unit tests.
//
// Pure-Go (no Postgres) so the diff + sensitive-field stripping
// contract is pinned independently of the audit_events write
// path. Integration coverage (RecordChange end-to-end against
// a real DB + audit row) lives in the retrofit packages'
// tests (commit 2 + 3).

package audit

import (
	"reflect"
	"strings"
	"testing"
)

type sampleConfig struct {
	SiteName string
	BaseURL  string
	Enabled  bool
	internal string // unexported — must be skipped
}

type sampleWithSensitive struct {
	Name            string
	PasswordHash    string // matches "password" — pattern-stripped
	APIKey          string // matches "apikey" — pattern-stripped
	HiddenByTag     string `audit:"-"`
	NormalField     int
}

type otherType struct {
	OtherField string
}

func TestDiffStructs_AllFieldsEqual_ReturnsEmpty(t *testing.T) {
	a := sampleConfig{SiteName: "x", BaseURL: "https://a/", Enabled: true}
	b := sampleConfig{SiteName: "x", BaseURL: "https://a/", Enabled: true}
	got := diffStructs(a, b)
	if len(got) != 0 {
		t.Errorf("diff = %v, want empty", got)
	}
}

func TestDiffStructs_OneFieldChanged_ReturnsOneEntry(t *testing.T) {
	before := sampleConfig{SiteName: "Old", BaseURL: "https://a/", Enabled: true}
	after := sampleConfig{SiteName: "New", BaseURL: "https://a/", Enabled: true}
	got := diffStructs(before, after)
	if len(got) != 1 {
		t.Fatalf("diff size = %d, want 1: %v", len(got), got)
	}
	entry, ok := got["SiteName"]
	if !ok {
		t.Fatalf("missing SiteName key; got %v", got)
	}
	if entry["before"] != "Old" || entry["after"] != "New" {
		t.Errorf("SiteName entry = %+v", entry)
	}
}

func TestDiffStructs_MultipleFields_AllChangedReported(t *testing.T) {
	before := sampleConfig{SiteName: "Old", BaseURL: "https://a/", Enabled: false}
	after := sampleConfig{SiteName: "New", BaseURL: "https://b/", Enabled: true}
	got := diffStructs(before, after)
	for _, want := range []string{"SiteName", "BaseURL", "Enabled"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s in diff: %+v", want, got)
		}
	}
}

func TestDiffStructs_UnexportedFields_Skipped(t *testing.T) {
	a := sampleConfig{SiteName: "x", internal: "secret-a"}
	b := sampleConfig{SiteName: "x", internal: "secret-b"}
	got := diffStructs(a, b)
	if _, leaked := got["internal"]; leaked {
		t.Errorf("unexported field appeared in diff: %v", got)
	}
}

func TestDiffStructs_AuditTagStripped(t *testing.T) {
	a := sampleWithSensitive{Name: "x", HiddenByTag: "a", NormalField: 1}
	b := sampleWithSensitive{Name: "x", HiddenByTag: "b", NormalField: 1}
	got := diffStructs(a, b)
	if _, leaked := got["HiddenByTag"]; leaked {
		t.Errorf("audit:\"-\" tagged field leaked: %v", got)
	}
}

func TestDiffStructs_SensitivePatternStripped(t *testing.T) {
	// PasswordHash matches "password" substring. APIKey matches
	// "apikey". Both must be stripped even though neither has the
	// audit:"-" tag — defense-in-depth.
	a := sampleWithSensitive{Name: "x", PasswordHash: "old", APIKey: "AAA"}
	b := sampleWithSensitive{Name: "x", PasswordHash: "new", APIKey: "BBB"}
	got := diffStructs(a, b)
	if _, leaked := got["PasswordHash"]; leaked {
		t.Errorf("PasswordHash leaked: %v", got)
	}
	if _, leaked := got["APIKey"]; leaked {
		t.Errorf("APIKey leaked: %v", got)
	}
}

func TestDiffStructs_TypeMismatch_ReturnsNil(t *testing.T) {
	a := sampleConfig{SiteName: "x"}
	b := otherType{OtherField: "y"}
	got := diffStructs(a, b)
	if got != nil {
		t.Errorf("type mismatch should return nil; got %v", got)
	}
}

func TestDiffStructs_NonStructInput_ReturnsNil(t *testing.T) {
	got := diffStructs("string-not-struct", "another-string")
	if got != nil {
		t.Errorf("non-struct input should return nil; got %v", got)
	}
}

func TestDiffStructs_PointerInput_Dereferenced(t *testing.T) {
	a := &sampleConfig{SiteName: "x", BaseURL: "https://a/"}
	b := &sampleConfig{SiteName: "y", BaseURL: "https://a/"}
	got := diffStructs(a, b)
	if _, ok := got["SiteName"]; !ok {
		t.Errorf("pointer-input diff didn't capture SiteName change: %v", got)
	}
}

func TestDiffStructs_NilBefore_DiffsAgainstZeroValue(t *testing.T) {
	// Common "row was just created" pattern: before == nil
	// pointer, after == populated. Every non-zero field on
	// `after` should appear in the diff with `before` = zero.
	var before *sampleConfig
	after := &sampleConfig{SiteName: "New Site", BaseURL: "https://b/", Enabled: true}
	got := diffStructs(before, after)
	if _, ok := got["SiteName"]; !ok {
		t.Errorf("nil-before should diff against zero; missing SiteName: %v", got)
	}
	if entry, ok := got["SiteName"]; ok {
		if entry["before"] != "" || entry["after"] != "New Site" {
			t.Errorf("SiteName entry = %+v, want before=\"\" after=\"New Site\"", entry)
		}
	}
}

func TestDiffStructs_NestedStruct_TreatedAsValue(t *testing.T) {
	// Nested structs compare via reflect.DeepEqual at the
	// outer-struct field level. The diff reports the WHOLE
	// nested value as before/after — operators see the full
	// nested change, not a flattened path.
	type inner struct{ X, Y int }
	type outer struct {
		Name string
		Sub  inner
	}
	a := outer{Name: "x", Sub: inner{X: 1, Y: 2}}
	b := outer{Name: "x", Sub: inner{X: 1, Y: 3}}
	got := diffStructs(a, b)
	if _, ok := got["Sub"]; !ok {
		t.Errorf("nested-struct change should appear at outer field: %v", got)
	}
	if got["Sub"]["after"].(inner).Y != 3 {
		t.Errorf("nested value wasn't preserved in diff: %+v", got)
	}
}

func TestIsSensitiveFieldName_CaseInsensitive(t *testing.T) {
	// All these should be stripped regardless of case.
	for _, name := range []string{
		"Password",
		"password",
		"PasswordHash",
		"password_hash",
		"APIKey",
		"ApiKey",
		"apikey",
		"PrivateKeyEnc",
		"SigningPublicKeyPem", // matches "signing"
		"EncryptionPrivateKey",
		"MasterKeyWrapper",
		"AuthToken",
	} {
		if !isSensitiveFieldName(name) {
			t.Errorf("isSensitiveFieldName(%q) = false, want true", name)
		}
	}
}

func TestIsSensitiveFieldName_BenignAccepted(t *testing.T) {
	// Field names that DON'T match any pattern should pass through.
	for _, name := range []string{
		"SiteName",
		"BaseURL",
		"Enabled",
		"FullName",
		"DisplayName",
		"Bio",
	} {
		if isSensitiveFieldName(name) {
			t.Errorf("isSensitiveFieldName(%q) = true, want false (benign)", name)
		}
	}
}

// Compile-time sanity check that changesetKey + sensitivePatterns
// are still the values the convention documentation references.
// If a future refactor renames them, this test catches the
// downstream doc drift at build time.
func TestConventionsPinned(t *testing.T) {
	if changesetKey != "changeset" {
		t.Errorf("changesetKey = %q; docs/observability/audit-events.md references \"changeset\"", changesetKey)
	}
	// Spot-check 3 critical patterns survive any future trim.
	for _, p := range []string{"password", "privatekey", "token"} {
		found := false
		for _, sp := range sensitiveFieldPatterns {
			if sp == p {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("critical pattern %q dropped from sensitiveFieldPatterns; review the strip surface", p)
		}
	}
	// reflect import sanity — silences linter on the slim test file.
	_ = reflect.TypeOf(struct{}{})
	_ = strings.Contains
}
