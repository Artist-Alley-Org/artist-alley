// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
		{"/api/v1/featured", true, "public featured rail"},
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
		{"/api/v1/appearance/logo", false, "instance logo is chrome on the login page, like /appearance"},
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
	"GET /appearance/logo": "instance logo — chrome on the login page, same reason as /appearance. " +
		"Discloses nothing: uploading an instance logo IS publishing it.",
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

// TestResolveIdentityPublicModeGate drives the gate through the actual
// middleware rather than calling IsPublicSurface directly, because the
// thing that can break is the wiring: a gate placed above the token and
// cookie branches would reject authenticated callers too, and a gate
// scoped to every anonymous request rather than to the public surface
// would 401 the login endpoint.
//
// No pool is needed — with neither an Authorization header nor a
// session cookie, ResolveIdentity reaches the gate without touching
// the database.
func TestResolveIdentityPublicModeGate(t *testing.T) {
	cases := []struct {
		name       string
		publicMode func(context.Context) bool
		path       string
		wantStatus int
		why        string
	}{
		{
			name:       "public surface refused when the toggle is off",
			publicMode: func(context.Context) bool { return false },
			path:       "/api/v1/assets",
			wantStatus: http.StatusUnauthorized,
			why:        "this is the feature",
		},
		{
			name:       "public surface served when the toggle is on",
			publicMode: func(context.Context) bool { return true },
			path:       "/api/v1/assets",
			wantStatus: http.StatusOK,
			why:        "handler decides from here; the middleware is done",
		},
		{
			name:       "login is never gated, toggle off",
			publicMode: func(context.Context) bool { return false },
			path:       "/api/v1/auth/login",
			wantStatus: http.StatusOK,
			why:        "gating login locks the operator out with no recovery short of a DB shell",
		},
		{
			name:       "setup is never gated, toggle off",
			publicMode: func(context.Context) bool { return false },
			path:       "/api/v1/setup/status",
			wantStatus: http.StatusOK,
			why:        "a fresh install must reach the setup wizard before any identity exists",
		},
		{
			name:       "public appearance is never gated, toggle off",
			publicMode: func(context.Context) bool { return false },
			path:       "/api/v1/appearance",
			wantStatus: http.StatusOK,
			why:        "the login page cannot render without it",
		},
		// The two /previews cases sit next to /appearance deliberately:
		// the pair IS the distinction, and collapsing it is a live
		// hazard rather than a nitpick. The endpoint shipped (#591)
		// with a spec description claiming it was open "for the same
		// reasoning as /appearance" and that "the boot path needs it
		// before sign-in" — both false, and both an argument for
		// deleting the gate below. A reviewer auditing anonymous
		// surface reads the description, not the route table, so the
		// claim has to be pinned somewhere that fails when it stops
		// being true (#611).
		{
			name:       "preview ladder IS gated, toggle off",
			publicMode: func(context.Context) bool { return false },
			path:       "/api/v1/previews",
			wantStatus: http.StatusUnauthorized,
			why: "a private install must not hand its image-pipeline config to " +
				"anonymous callers; nothing before sign-in needs image rungs, so " +
				"this endpoint follows the content it describes rather than being " +
				"excused as boot-critical the way /appearance genuinely is",
		},
		{
			name:       "preview ladder served when the toggle is on",
			publicMode: func(context.Context) bool { return true },
			path:       "/api/v1/previews",
			wantStatus: http.StatusOK,
			why: "guards the guard — without this the case above would still pass " +
				"if the route were gated by accident, or by something unrelated to " +
				"public mode",
		},
		{
			name:       "a nil reader denies rather than publishes",
			publicMode: nil,
			path:       "/api/v1/assets",
			wantStatus: http.StatusUnauthorized,
			why:        "a dropped boot wire must fail closed",
		},
		{
			name:       "IIIF at the root mount is gated too",
			publicMode: func(context.Context) bool { return false },
			path:       "/iiif/3/abc/full/max/0/default.jpg",
			wantStatus: http.StatusUnauthorized,
			why:        "IIIF 401s on its own today; this keeps it covered for when it is opened",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Resolver{Logger: slog.New(slog.DiscardHandler), PublicMode: c.publicMode}
			var reached bool
			h := r.ResolveIdentity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			if rec.Code != c.wantStatus {
				t.Errorf("GET %s = %d, want %d — %s", c.path, rec.Code, c.wantStatus, c.why)
			}
			if c.wantStatus == http.StatusUnauthorized && reached {
				t.Errorf("GET %s reached the handler despite a 401 — the gate wrote a status but did not stop the chain", c.path)
			}
		})
	}
}

// TestPublicModeGateIgnoresContextInjectedIdentity pins one specific
// thing and is named for it, because the tempting name —
// "...NeverAffectsAuthenticatedCallers" — would claim coverage this
// does not have. ResolveIdentity authenticates from HEADERS; a
// context-injected Identity is a test fixture, not a credential, and
// the middleware must not treat it as one.
//
// The real "authenticated callers are unchanged in both toggle states"
// guarantee is structural rather than asserted here: the token and
// cookie branches both return before the gate, so an authenticated
// request cannot reach it. Exercising that needs a live pool and a
// real session, which is what the curl verification against a running
// server covers.
func TestPublicModeGateIgnoresContextInjectedIdentity(t *testing.T) {
	r := &Resolver{Logger: slog.New(slog.DiscardHandler),
		PublicMode: func(context.Context) bool { return false }}

	// Simulate a resolved identity by pre-seeding the context, which is
	// what the token/cookie branches do before calling next.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := r.ResolveIdentity(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/assets", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(WithIdentity(req.Context(), &Identity{UserRef: 1})))

	// The middleware re-resolves from headers, so a context-injected
	// identity does NOT survive — this asserts the anonymous path, and
	// the authenticated path is covered by the two early returns in
	// ResolveIdentity, which are unreachable from here without a pool.
	// What matters is that the gate did not somehow allow it through on
	// identity grounds it never checked.
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d; a context-injected identity must not be trusted by the gate — "+
			"the middleware authenticates from headers, not from context", rec.Code)
	}
}
