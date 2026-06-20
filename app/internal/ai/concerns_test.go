package ai

import "testing"

func TestConcern_KnownConstantsValid(t *testing.T) {
	for _, c := range AllConcerns {
		if !c.Valid() {
			t.Errorf("Concern(%q).Valid() = false, want true", c)
		}
	}
}

func TestConcern_UnknownIsInvalid(t *testing.T) {
	for _, s := range []Concern{"", "summarize", "translate", "COMPLETE", " complete "} {
		if s.Valid() {
			t.Errorf("Concern(%q).Valid() = true, want false", s)
		}
	}
}

func TestParseConcern_RoundTrips(t *testing.T) {
	for _, want := range AllConcerns {
		got, err := ParseConcern(string(want))
		if err != nil {
			t.Errorf("ParseConcern(%q): %v", want, err)
			continue
		}
		if got != want {
			t.Errorf("ParseConcern(%q) = %q, want %q", want, got, want)
		}
	}
}

func TestParseConcern_RejectsUnknown(t *testing.T) {
	if _, err := ParseConcern("classify"); err == nil {
		t.Fatal("ParseConcern(\"classify\"): want error, got nil")
	}
}

func TestPrivacyClass_KnownValid(t *testing.T) {
	for _, p := range []PrivacyClass{PrivacyClassAny, PrivacyClassLocalOnly} {
		if !p.Valid() {
			t.Errorf("PrivacyClass(%q).Valid() = false, want true", p)
		}
	}
}

func TestPrivacyClass_UnknownInvalid(t *testing.T) {
	for _, p := range []PrivacyClass{"", "cloud_only", "ANY"} {
		if p.Valid() {
			t.Errorf("PrivacyClass(%q).Valid() = true, want false", p)
		}
	}
}
