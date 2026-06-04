package samlauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

func TestProvider_InterfaceContract(t *testing.T) {
	p := New("saml-okta", "Okta")
	if p.Name() != "saml-okta" {
		t.Errorf("Name = %q, want saml-okta", p.Name())
	}
	if p.Kind() != auth.KindSAML {
		t.Errorf("Kind = %q, want %q", p.Kind(), auth.KindSAML)
	}
	if p.RequiredLicenseFeature() != LicenseFeature {
		t.Errorf("RequiredLicenseFeature = %q, want %q", p.RequiredLicenseFeature(), LicenseFeature)
	}
	// SAML uses redirect flows — must NOT advertise password support.
	if p.SupportsPassword() {
		t.Error("SupportsPassword must be false for SAML")
	}
}

// SAML's Authenticate is a forbidden door — clients hitting password
// POST with provider="saml-*" get ErrProviderUnsupportedMethod, NOT
// ErrProviderUnimplemented. Important because the login handler maps
// those errors to different responses.
func TestProvider_AuthenticateRejectsPassword(t *testing.T) {
	p := New("saml", "SAML")
	_, err := p.Authenticate(context.Background(), "alice", "secret")
	if !errors.Is(err, auth.ErrProviderUnsupportedMethod) {
		t.Errorf("Authenticate err = %v, want ErrProviderUnsupportedMethod", err)
	}
}

// All three redirect-flow stubs return 501 with the same JSON body
// shape, so the admin UI can render a "build pending" badge without
// parsing prose. Each op is named in the response so support tickets
// pinpoint which one fired.
func TestProvider_RedirectStubsReturn501(t *testing.T) {
	p := New("saml", "SAML")
	cases := []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		op   string
	}{
		{"BeginLogin", p.BeginLogin, "saml_begin_login"},
		{"ConsumeAssertion", p.ConsumeAssertion, "saml_consume_assertion"},
		{"Metadata", p.Metadata, "saml_metadata"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.fn(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != http.StatusNotImplemented {
				t.Errorf("code = %d, want 501", rec.Code)
			}
			body := rec.Body.String()
			if !contains(body, c.op) {
				t.Errorf("body = %q, must mention op %q", body, c.op)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
