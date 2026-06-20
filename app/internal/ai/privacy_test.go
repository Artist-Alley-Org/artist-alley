package ai

import (
	"reflect"
	"sort"
	"testing"
)

func TestClassifyPrivacy_LockOn_PublicAllowsAny(t *testing.T) {
	got := ClassifyPrivacy(SensitivityPublic, PrivacyPolicy{LockSensitiveToLocal: true})
	if got != PrivacyClassAny {
		t.Errorf("public + lock-on = %q, want %q", got, PrivacyClassAny)
	}
}

func TestClassifyPrivacy_LockOn_TeamAllowsAny(t *testing.T) {
	got := ClassifyPrivacy(SensitivityTeam, PrivacyPolicy{LockSensitiveToLocal: true})
	if got != PrivacyClassAny {
		t.Errorf("team + lock-on = %q, want %q", got, PrivacyClassAny)
	}
}

func TestClassifyPrivacy_LockOn_RestrictedLocksLocal(t *testing.T) {
	got := ClassifyPrivacy(SensitivityRestricted, PrivacyPolicy{LockSensitiveToLocal: true})
	if got != PrivacyClassLocalOnly {
		t.Errorf("restricted + lock-on = %q, want %q", got, PrivacyClassLocalOnly)
	}
}

func TestClassifyPrivacy_LockOn_EmbargoLocksLocal(t *testing.T) {
	got := ClassifyPrivacy(SensitivityEmbargo, PrivacyPolicy{LockSensitiveToLocal: true})
	if got != PrivacyClassLocalOnly {
		t.Errorf("embargo + lock-on = %q, want %q", got, PrivacyClassLocalOnly)
	}
}

func TestClassifyPrivacy_LockOff_RestrictedAllowsAny(t *testing.T) {
	// Operator opted out via the admin UI; the gate stops filtering.
	got := ClassifyPrivacy(SensitivityRestricted, PrivacyPolicy{LockSensitiveToLocal: false})
	if got != PrivacyClassAny {
		t.Errorf("restricted + lock-off = %q, want %q", got, PrivacyClassAny)
	}
}

func TestFilterLocalOnly_NarrowsToAllowedSet(t *testing.T) {
	policy := PrivacyPolicy{LocalProviders: []string{"ollama", "clip_local"}}
	got := FilterLocalOnly([]string{"openai", "ollama", "claude", "clip_local"}, policy)
	want := []string{"ollama", "clip_local"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filtered = %v, want %v", got, want)
	}
}

func TestFilterLocalOnly_EmptyInputReturnsNil(t *testing.T) {
	if got := FilterLocalOnly(nil, PrivacyPolicy{LocalProviders: []string{"ollama"}}); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestFilterLocalOnly_NoLocalConfigured_FiltersAllOut(t *testing.T) {
	// Operator wiped the local_providers list; the filter empties.
	// The router will translate this empty result into the
	// "privacy gate blocked everything" signal.
	got := FilterLocalOnly([]string{"openai", "claude"}, PrivacyPolicy{LocalProviders: nil})
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestFilterLocalOnly_PreservesCandidateOrder(t *testing.T) {
	// The router uses the result for fallback ordering, so this is a
	// stability contract — we don't accidentally re-sort the list.
	policy := PrivacyPolicy{LocalProviders: []string{"ollama", "vllm", "clip_local"}}
	got := FilterLocalOnly([]string{"vllm", "ollama", "clip_local"}, policy)
	want := []string{"vllm", "ollama", "clip_local"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order changed: got %v, want %v", got, want)
	}
}
