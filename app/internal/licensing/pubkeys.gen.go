// Code generated for the embedded root pubkey catalog.
// Regenerate via the license server's script:
//
//   npm run emit:go-pubkeys -- --url https://lic.artist-alley.org \
//     > /path/to/artist-alley/app/internal/licensing/pubkeys.gen.go
//
// The "generated" comment makes this file recognisable to tooling
// (gofmt + linters skip semantic checks on generated files). Don't
// hand-edit unless you're also updating the upstream catalog.
//
// Bootstrapped values below are the prod + staging kids as of
// 2026-06-04. Both are kept so:
//   - retired keys keep verifying old licenses (Layer-3 anti-pirate)
//   - dev / staging deployments of artist-alley can verify
//     licenses issued by the staging license server
//
// In production customer installs, ExpectedIssuer below pins the
// canonical authority — staging-issued licenses are rejected by
// the iss check in verifier.go.

package licensing

// PublicKey is one entry in the embedded root catalog.
type PublicKey struct {
	KID          string
	PublicKeyB64 string
	Purpose      string
	CreatedAt    string
	RetiredAt    string // empty when active
}

// ExpectedIssuer is the canonical iss string baked into every
// license issued by the prod license server. Verifiers reject
// licenses with a different iss to guard against cross-environment
// confusion (e.g. a staging-issued license accidentally installed
// into a customer prod deployment).
//
// Declared as a var (not const) so:
//   - tests can save/restore + override it
//   - a future env-var override (AA_LICENSE_ISSUER) can swap at boot
//     for dev / staging deployments without rebuilding
//
// The default value below is the only one customer prod installs
// should ever see.
var ExpectedIssuer = "lic.artist-alley.org"

// PublicKeys is the catalog the verifier consults to resolve kids.
// Retired keys are kept so already-issued licenses keep verifying.
var PublicKeys = []PublicKey{
	{
		KID:          "aa-2026-core-prod-01",
		PublicKeyB64: "e/sP9xADk17aR5+cledvLs2iKhfl+xzOHKsk5/E+mt4=",
		Purpose:      "core",
		CreatedAt:    "2026-06-04T05:57:33.764Z",
		RetiredAt:    "",
	},
	{
		KID:          "aa-2026-core-01",
		PublicKeyB64: "MGp5A+DeVKNy2YAS3+zSuwvjC96TZ1q2R49iA92YIDE=",
		Purpose:      "core",
		CreatedAt:    "2026-06-04T05:51:37.883Z",
		RetiredAt:    "",
	},
}
