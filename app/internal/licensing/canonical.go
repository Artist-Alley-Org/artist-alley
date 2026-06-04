// Canonical JSON encoding for signature verification.
//
// The TypeScript license server canonicalises claims as:
//   - object keys sorted lexicographically (UTF-16 code unit order;
//     ASCII bytes compare identically)
//   - no insignificant whitespace
//   - UTF-8 output
//   - integers as decimal (no .0)
//   - explicit `null` for nullable fields (not omitted)
//   - arrays preserve insertion order
//
// Go's `encoding/json` matches all of those when we marshal a
// `map[string]any` (map keys are sorted; ints render decimal; nil
// renders as null; arrays preserve order). Two gotchas:
//
//  1. HTML-escaping is on by default in `json.Marshal` — `<`, `>`,
//     `&` become `\u003c` etc. Disable it via `json.Encoder` so the
//     TS canonical output matches byte-for-byte.
//  2. `json.Encoder.Encode` appends a trailing newline. Strip it.
//
// We deliberately go via `map[string]any` rather than marshalling
// the struct directly because struct field order is source-order in
// Go, not lexicographic — relying on it would silently break the
// contract the first time someone reorders fields for readability.

package licensing

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// canonicalLicenseMap converts LicenseClaims to the map form the
// canonical JSON encoder operates on. Pointer-typed fields become
// explicit `nil` entries in the map, which json.Marshal renders as
// `null` — matching the TypeScript canonical's preservation of
// explicit null.
func canonicalLicenseMap(c LicenseClaims) map[string]any {
	return map[string]any{
		"v":                c.V,
		"kid":              c.KID,
		"lid":              c.LID,
		"prod":             c.Product,
		"tier":             c.Tier,
		"seats":            nullableInt64(c.Seats),
		"seat_window_days": c.SeatWindowDays,
		"asset_cap":        nullableInt64(c.AssetCap),
		"owner":            c.Owner,
		"org":              c.Org,
		"nbf":              c.NotBefore,
		"exp":              c.Expires,
		"iat":              c.IssuedAt,
		"features":         stringSliceOrEmpty(c.Features),
		"host_fp":          nullableString(c.HostFingerprint),
		"org_pubkey":       nullableString(c.OrgPubkey),
		"bound_domains":    nullableStringSlice(c.BoundDomains),
		"iss":              c.Issuer,
	}
}

func canonicalPluginLicenseMap(c PluginLicenseClaims) map[string]any {
	return map[string]any{
		"v":              c.V,
		"publisher_kid":  c.PublisherKID,
		"publisher_slug": c.PublisherSlug,
		"lid":            c.LID,
		"prod":           c.Product,
		"tier":           c.Tier,
		"owner":          c.Owner,
		"org":            c.Org,
		"nbf":            c.NotBefore,
		"exp":            c.Expires,
		"iat":            c.IssuedAt,
		"features":       stringSliceOrEmpty(c.Features),
		"org_pubkey":     nullableString(c.OrgPubkey),
	}
}

func canonicalPublisherCertMap(c PublisherCertClaims) map[string]any {
	return map[string]any{
		"v":                    c.V,
		"cert_id":              c.CertID,
		"signed_by_kid":        c.SignedByKID,
		"publisher_slug":       c.PublisherSlug,
		"publisher_kid":        c.PublisherKID,
		"publisher_pubkey_b64": c.PublisherPubkeyB64,
		"display_name":         c.DisplayName,
		"allowed_products":     stringSliceOrEmpty(c.AllowedProducts),
		"nbf":                  c.NotBefore,
		"exp":                  c.Expires,
		"iat":                  c.IssuedAt,
		"iss":                  c.Issuer,
	}
}

// canonicalBytes marshals an any-map (or any JSON-serialisable value)
// to the canonical byte form used for signature verification.
func canonicalBytes(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // match TS canonical (no \u003c for '<')
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("canonical: encode: %w", err)
	}
	// json.Encoder.Encode appends a trailing newline; strip it so the
	// output is byte-identical to TS JSON.stringify(canonical(...)).
	out := bytes.TrimRight(buf.Bytes(), "\n")
	return out, nil
}

// --- helpers for the explicit-null treatment of pointers --------------------

// nullableInt64 returns the dereferenced int64 or nil for json.Marshal.
// Returning `any` rather than an int64 with a magic sentinel lets the
// map carry an honest `nil` that becomes `null` on the wire.
func nullableInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullableStringSlice returns `nil` when the slice is nil, the slice
// itself otherwise. A nil slice renders as `null`; an empty []string{}
// renders as `[]`. The TS canonical distinguishes the two — a license
// with `bound_domains: null` is structurally different from one with
// `bound_domains: []`. We preserve the distinction.
func nullableStringSlice(s []string) any {
	if s == nil {
		return nil
	}
	return s
}

// stringSliceOrEmpty defaults a nil slice to []. Used for the
// `features` field which is never null in the wire schema (only ever
// `[]` or a populated list).
func stringSliceOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
