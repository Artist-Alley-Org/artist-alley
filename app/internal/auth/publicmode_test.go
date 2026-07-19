// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestIsPublicSurface(t *testing.T) {
	cases := []struct {
		path string
		want bool
		why  string
	}{
		// Governed — both mounts of the middleware.
		{"/api/v1/assets", true, "asset list"},
		{"/api/v1/assets/1234", true, "asset item"},
		{"/api/v1/assets/1234/file", true, "asset bytes"},
		{"/api/v1/assets/1234/variants/hls/master.m3u8", true, "multi-segment variant"},
		{"/api/v1/assets/1234/archive/bundle.zip", true, "archive bundle"},
		{"/api/v1/collections", true, "collection list"},
		{"/api/v1/collections/1234/resources", true, "collection contents"},
		{"/api/v1/search", true, "search"},
		{"/api/v1/search/facets", true, "facets"},
		{"/api/v1/search/suggest", true, "suggest"},
		{"/api/v1/search/by-image", true, "reverse image search"},
		{"/api/v1/iiif/3/abc/info.json", true, "IIIF under /api/v1"},
		{"/iiif/3/abc/full/max/0/default.jpg", true, "IIIF at the root mount"},
		{"/iiif/2/abc/manifest", true, "IIIF 2.x redirect"},

		// NOT governed — must serve anonymous callers in both states.
		// A regression here does not degrade a feature; it locks the
		// operator out of the install.
		{"/api/v1/auth/login", false, "login must work with the toggle off"},
		{"/api/v1/auth/providers", false, "the login page renders SSO buttons from this"},
		{"/api/v1/auth/register", false, "signup"},
		{"/api/v1/auth/verify-email", false, "signup"},
		{"/api/v1/auth/saml/metadata", false, "fetched by the IdP, which has no session"},
		{"/api/v1/setup/status", false, "first boot"},
		{"/api/v1/setup/complete", false, "first boot"},
		{"/api/v1/appearance", false, "public boot payload; the login page needs it"},
		{"/api/v1/build-info", false, "version banner"},
		{"/api/v1/unsubscribe", false, "token-authed; gating 401s every link already mailed"},
		{"/api/v1/openapi.json", false, "API reference, no data"},

		// Already 401 for anonymous via their own handler gate. Naming
		// them would change nothing; not naming them must also change
		// nothing.
		{"/api/v1/admin/search/health", false, "admin"},
		{"/api/v1/account/messages", false, "account"},

		// Segment-boundary: a prefix entry must not match a longer
		// sibling name.
		{"/api/v1/assetsomething", false, "must not be swept in by the /assets prefix"},
		{"/api/v1/searchable", false, "must not be swept in by the /search prefix"},
		{"/api/v1/collections-export", false, "must not be swept in by the /collections prefix"},
	}
	for _, c := range cases {
		if got := IsPublicSurface(c.path); got != c.want {
			t.Errorf("IsPublicSurface(%q) = %v, want %v — %s", c.path, got, c.want, c.why)
		}
	}
}

// notGovernedAnonymousOps are the operations that openapi.yaml marks
// anonymous (`security: []`) and that public mode deliberately does NOT
// govern, because they must serve anonymous callers in BOTH states.
//
// This is the list the test below checks against, and it is the reason
// the list exists in a test rather than in a comment: adding
// `security: []` to a new operation now forces an explicit decision
// about which side of the toggle it falls on.
var notGovernedAnonymousOps = map[string]string{
	"GET /setup/status":    "first boot — the setup wizard must reach this before any admin exists",
	"POST /setup/complete": "first boot — this is what creates the admin",
	"GET /build-info":      "version banner; no data",
	"GET /appearance":      "public boot payload — the login page cannot render without it",
}

// TestPublicSurfaceCoversAnonymousOperations is the build-time
// guarantee that replaces the runtime "new routes are gated by default"
// property an allowlist would have given (see the header comment in
// publicmode.go).
//
// Every operation the spec marks anonymous must be either governed by
// the toggle or explicitly excused above. An operation that is neither
// is a route somebody opened to anonymous callers without deciding
// whether a private install should serve it — which is exactly the
// mistake this test exists to catch, and it catches it in CI in front
// of the author rather than in production.
func TestPublicSurfaceCoversAnonymousOperations(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}

	var (
		pathRe = regexp.MustCompile(`^  (/\S*):\s*$`)
		opRe   = regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)
		secRe  = regexp.MustCompile(`^      security:\s*\[\]\s*$`)
	)

	var path, op string
	found := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if m := pathRe.FindStringSubmatch(line); m != nil {
			path = m[1]
			continue
		}
		if m := opRe.FindStringSubmatch(line); m != nil {
			op = strings.ToUpper(m[1])
			continue
		}
		if !secRe.MatchString(line) {
			continue
		}
		found++
		key := op + " " + path
		if _, excused := notGovernedAnonymousOps[key]; excused {
			continue
		}
		if !IsPublicSurface(path) {
			t.Errorf("%s is declared anonymous (security: []) in openapi.yaml but is neither "+
				"governed by public mode nor listed in notGovernedAnonymousOps. Decide which: "+
				"add it to PublicSurfaceRoutes so a private install refuses it, or excuse it "+
				"with a reason.", key)
		}
	}

	// Guard the guard. If the scanner stops matching (an indentation
	// change in the spec, a reformat), every assertion above silently
	// passes over zero operations and this test becomes decorative.
	if found == 0 {
		t.Fatal("scanned openapi.yaml and found no `security: []` operations at all; " +
			"the scanner has stopped matching and this test is no longer checking anything")
	}
}
