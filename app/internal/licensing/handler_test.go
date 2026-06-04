package licensing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Tests for the admin licensing HTTP surface (Phase 1.17.O-1/O-2/O-3).
// Auth gating is the most important property to pin — these endpoints
// expose license features + accept .lic uploads, so any regression
// that lets an anonymous or non-SuperAdmin caller through would be a
// security incident.
//
// The handler accepts a *State at construction. We hand it an
// in-memory community State (path == "") for the GET + validate paths
// (no disk involved), and a temp-file-backed State for the upload
// path so SaveAndReload's write step actually runs.

// withSuperAdmin wraps ctx with a SuperAdmin Identity. Mirrors the
// resolver-middleware behavior the strict handler relies on.
func withSuperAdmin(ctx context.Context) context.Context {
	return auth.WithIdentity(ctx, &auth.Identity{
		UserRef:      1,
		Username:     "admin",
		Capabilities: []string{auth.SuperAdminCapability},
		AuthMethod:   "session",
	})
}

// withNonAdmin wraps ctx with an authenticated-but-unprivileged
// identity. Used to verify the 403 branch.
func withNonAdmin(ctx context.Context) context.Context {
	return auth.WithIdentity(ctx, &auth.Identity{
		UserRef:    7,
		Username:   "alice",
		AuthMethod: "session",
	})
}

// GET /admin/license/status — anonymous → 401.
func TestGetAdminLicenseStatus_AnonymousReturns401(t *testing.T) {
	h := NewHandler(NewState("", "", nil), nil)
	resp, err := h.GetAdminLicenseStatus(context.Background(), openapi.GetAdminLicenseStatusRequestObject{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := resp.(openapi.GetAdminLicenseStatus401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

// GET /admin/license/status — non-admin → 403.
func TestGetAdminLicenseStatus_NonAdminReturns403(t *testing.T) {
	h := NewHandler(NewState("", "", nil), nil)
	resp, _ := h.GetAdminLicenseStatus(withNonAdmin(context.Background()),
		openapi.GetAdminLicenseStatusRequestObject{})
	if _, ok := resp.(openapi.GetAdminLicenseStatus403JSONResponse); !ok {
		t.Errorf("expected 403, got %T", resp)
	}
}

// GET /admin/license/status — SuperAdmin on a community install gets
// the synthetic community Status back. Loaded=false + tier=community
// is the contract every consumer relies on.
func TestGetAdminLicenseStatus_SuperAdminCommunity(t *testing.T) {
	h := NewHandler(NewState("", "", nil), nil)
	resp, _ := h.GetAdminLicenseStatus(withSuperAdmin(context.Background()),
		openapi.GetAdminLicenseStatusRequestObject{})
	r, ok := resp.(openapi.GetAdminLicenseStatus200JSONResponse)
	if !ok {
		t.Fatalf("expected 200, got %T", resp)
	}
	if r.Loaded {
		t.Error("Loaded must be false in community mode")
	}
	if r.Tier != "community" {
		t.Errorf("Tier = %q, want community", r.Tier)
	}
}

// POST /admin/license/validate — anonymous → 401.
func TestValidateAdminLicense_AnonymousReturns401(t *testing.T) {
	h := NewHandler(NewState("", "", nil), nil)
	resp, _ := h.ValidateAdminLicense(context.Background(),
		openapi.ValidateAdminLicenseRequestObject{Body: &openapi.LicenseUploadRequest{LicenseText: "x"}})
	if _, ok := resp.(openapi.ValidateAdminLicense401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

// POST /admin/license/validate — non-admin → 403.
func TestValidateAdminLicense_NonAdminReturns403(t *testing.T) {
	h := NewHandler(NewState("", "", nil), nil)
	resp, _ := h.ValidateAdminLicense(withNonAdmin(context.Background()),
		openapi.ValidateAdminLicenseRequestObject{Body: &openapi.LicenseUploadRequest{LicenseText: "x"}})
	if _, ok := resp.(openapi.ValidateAdminLicense403JSONResponse); !ok {
		t.Errorf("expected 403, got %T", resp)
	}
}

// POST /admin/license/validate with empty body returns 400 with
// code="bad_envelope" so the UI knows it's not a transient error.
func TestValidateAdminLicense_NilBodyReturnsBadEnvelope(t *testing.T) {
	h := NewHandler(NewState("", "", nil), nil)
	resp, _ := h.ValidateAdminLicense(withSuperAdmin(context.Background()),
		openapi.ValidateAdminLicenseRequestObject{Body: nil})
	r400, ok := resp.(openapi.ValidateAdminLicense400JSONResponse)
	if !ok {
		t.Fatalf("expected 400, got %T", resp)
	}
	if r400.Code != "bad_envelope" {
		t.Errorf("Code = %q, want bad_envelope", r400.Code)
	}
}

// POST /admin/license/validate with garbage text returns 400 +
// bad_envelope — verifier rejects before any other check.
func TestValidateAdminLicense_BadEnvelopeReturns400(t *testing.T) {
	h := NewHandler(NewState("", "", nil), nil)
	resp, _ := h.ValidateAdminLicense(withSuperAdmin(context.Background()),
		openapi.ValidateAdminLicenseRequestObject{Body: &openapi.LicenseUploadRequest{LicenseText: "not a license"}})
	r400, ok := resp.(openapi.ValidateAdminLicense400JSONResponse)
	if !ok {
		t.Fatalf("expected 400, got %T", resp)
	}
	if !strings.Contains(r400.Message, "envelope") && r400.Code != "bad_envelope" {
		t.Errorf("expected bad_envelope code, got %+v", r400)
	}
}

// POST /admin/license/validate with a real, freshly-minted envelope
// succeeds + returns the Status that would result from installing
// it — without writing anything to disk.
func TestValidateAdminLicense_HappyPath(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kid := "validate-test-kid"
	withTestKey(t, kid, pub, "lic-test.artist-alley.org", func() {
		now := time.Now().Unix()
		seats := int64(50)
		claims := LicenseClaims{
			V: 1, KID: kid, LID: "01HZVL", Product: "artist-alley:core",
			Tier: "pro", Seats: &seats, SeatWindowDays: 30,
			Owner: "vl@test.com", Org: "vl",
			NotBefore: now - 60, Expires: now + 3600, IssuedAt: now,
			Features: []string{"core"},
			Issuer:   "lic-test.artist-alley.org",
		}
		text := mustSignEnvelope(t, priv, claims)

		h := NewHandler(NewState("", "", nil), nil)
		resp, _ := h.ValidateAdminLicense(withSuperAdmin(context.Background()),
			openapi.ValidateAdminLicenseRequestObject{Body: &openapi.LicenseUploadRequest{LicenseText: text}})
		r, ok := resp.(openapi.ValidateAdminLicense200JSONResponse)
		if !ok {
			t.Fatalf("expected 200, got %T", resp)
		}
		if !r.Loaded || r.Tier != "pro" {
			t.Errorf("preview Status = %+v, want loaded=true tier=pro", r)
		}
		// The cached state must NOT have been swapped — validate is
		// dry-run only. Calling GetAdminLicenseStatus still returns
		// community.
		status, _ := h.GetAdminLicenseStatus(withSuperAdmin(context.Background()),
			openapi.GetAdminLicenseStatusRequestObject{})
		s, _ := status.(openapi.GetAdminLicenseStatus200JSONResponse)
		if s.Loaded {
			t.Error("validate must not mutate cached State")
		}
	})
}

// POST /admin/license/upload — anonymous → 401.
func TestUploadAdminLicense_AnonymousReturns401(t *testing.T) {
	h := NewHandler(NewState("", "", nil), nil)
	resp, _ := h.UploadAdminLicense(context.Background(),
		openapi.UploadAdminLicenseRequestObject{Body: &openapi.LicenseUploadRequest{LicenseText: "x"}})
	if _, ok := resp.(openapi.UploadAdminLicense401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

// POST /admin/license/upload — non-admin → 403.
func TestUploadAdminLicense_NonAdminReturns403(t *testing.T) {
	h := NewHandler(NewState("", "", nil), nil)
	resp, _ := h.UploadAdminLicense(withNonAdmin(context.Background()),
		openapi.UploadAdminLicenseRequestObject{Body: &openapi.LicenseUploadRequest{LicenseText: "x"}})
	if _, ok := resp.(openapi.UploadAdminLicense403JSONResponse); !ok {
		t.Errorf("expected 403, got %T", resp)
	}
}

// POST /admin/license/upload with no LicensePath configured returns
// 409 with a clear "configure AA_LICENSE_PATH" message. The State's
// path field gates SaveAndReload via ErrStateNil; we exercise that
// branch here so a refactor that maps it to 500 instead doesn't slip.
func TestUploadAdminLicense_NoPathReturns409(t *testing.T) {
	h := NewHandler(NewState("", "", nil), nil)
	resp, _ := h.UploadAdminLicense(withSuperAdmin(context.Background()),
		openapi.UploadAdminLicenseRequestObject{Body: &openapi.LicenseUploadRequest{LicenseText: "x"}})
	r409, ok := resp.(openapi.UploadAdminLicense409JSONResponse)
	if !ok {
		t.Fatalf("expected 409, got %T", resp)
	}
	if !strings.Contains(r409.Error, "AA_LICENSE_PATH") {
		t.Errorf("error message must reference AA_LICENSE_PATH, got %q", r409.Error)
	}
}

// POST /admin/license/upload with bad envelope text returns 400 +
// bad_envelope — and crucially the file on disk is NOT created
// (verify-before-write contract). Same property the validate test
// covers, but exercised through the upload code path which has its
// own error mapping.
func TestUploadAdminLicense_BadEnvelopeReturns400AndDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "license.lic")
	h := NewHandler(NewState(path, "", nil), nil)
	resp, _ := h.UploadAdminLicense(withSuperAdmin(context.Background()),
		openapi.UploadAdminLicenseRequestObject{Body: &openapi.LicenseUploadRequest{LicenseText: "garbage"}})
	if _, ok := resp.(openapi.UploadAdminLicense400JSONResponse); !ok {
		t.Errorf("expected 400, got %T", resp)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("upload must not create the .lic file when verification fails")
	}
}

// POST /admin/license/upload — happy path: signed envelope, verified,
// persisted, cached State swapped.
func TestUploadAdminLicense_HappyPath(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kid := "upload-test-kid"
	withTestKey(t, kid, pub, "lic-test.artist-alley.org", func() {
		now := time.Now().Unix()
		claims := LicenseClaims{
			V: 1, KID: kid, LID: "01HZUL", Product: "artist-alley:core",
			Tier: "enterprise", SeatWindowDays: 30,
			Owner: "ul@test.com", Org: "ul",
			NotBefore: now - 60, Expires: now + 3600, IssuedAt: now,
			Features: []string{"core", "sso_ldap"},
			Issuer:   "lic-test.artist-alley.org",
		}
		text := mustSignEnvelope(t, priv, claims)

		dir := t.TempDir()
		path := filepath.Join(dir, "license.lic")
		state := NewState(path, "", nil)
		h := NewHandler(state, nil)

		resp, _ := h.UploadAdminLicense(withSuperAdmin(context.Background()),
			openapi.UploadAdminLicenseRequestObject{Body: &openapi.LicenseUploadRequest{LicenseText: text}})
		r, ok := resp.(openapi.UploadAdminLicense200JSONResponse)
		if !ok {
			t.Fatalf("expected 200, got %T", resp)
		}
		if r.Tier != "enterprise" {
			t.Errorf("Status.Tier = %q, want enterprise", r.Tier)
		}
		// File on disk must exist + match what we sent.
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != text {
			t.Error("file on disk does not match uploaded text")
		}
		// Cached State must reflect the install.
		if !state.HasFeature("sso_ldap") {
			t.Error("cached State must include uploaded features")
		}
	})
}
