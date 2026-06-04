// .lic text envelope parser.
//
// The envelope is the user-facing transport — what customers paste
// into /etc/artist-alley/license.lic. Cryptographically it's just a
// wrapper around the claims dict + signature; verification operates
// on the canonical JSON of the claims (see canonical.go), not on the
// envelope text. Re-formatting the envelope (whitespace, line
// ordering inside the BEGIN/END block) can NEVER break verification
// because the verifier rebuilds the canonical JSON from the parsed
// fields. Mutating a field VALUE, however, breaks verification — the
// new canonical bytes won't match the signature.

package licensing

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	licBeginMarker = "-----BEGIN ARTIST-ALLEY LICENSE-----"
	licEndMarker   = "-----END ARTIST-ALLEY LICENSE-----"

	pluginBeginMarker = "-----BEGIN ARTIST-ALLEY PLUGIN LICENSE-----"
	pluginEndMarker   = "-----END ARTIST-ALLEY PLUGIN LICENSE-----"

	certBeginMarker = "-----BEGIN ARTIST-ALLEY PUBLISHER CERT-----"
	certEndMarker   = "-----END ARTIST-ALLEY PUBLISHER CERT-----"
)

// SignedLicense holds a parsed core license envelope.
type SignedLicense struct {
	Claims        LicenseClaims
	SignatureB64u string
}

// SignedPublisherCert holds a parsed publisher certificate envelope
// (also used embedded inside plugin license envelopes).
type SignedPublisherCert struct {
	Claims        PublisherCertClaims
	SignatureB64u string
}

// SignedPluginLicense holds a parsed plugin license envelope, which
// embeds a publisher cert verbatim so the verifier can walk the
// chain offline.
type SignedPluginLicense struct {
	Claims        PluginLicenseClaims
	SignatureB64u string
	PublisherCert SignedPublisherCert
}

// DecodeLicense parses a core license envelope.
func DecodeLicense(text string) (SignedLicense, error) {
	raw := strings.TrimSpace(text)
	if !strings.HasPrefix(raw, licBeginMarker) {
		return SignedLicense{}, fmt.Errorf("envelope: missing BEGIN marker")
	}
	if !strings.HasSuffix(raw, licEndMarker) {
		return SignedLicense{}, fmt.Errorf("envelope: missing END marker")
	}
	inner := strings.TrimSpace(raw[len(licBeginMarker) : len(raw)-len(licEndMarker)])
	fields, err := parseEnvelopeFields(inner)
	if err != nil {
		return SignedLicense{}, fmt.Errorf("envelope: %w", err)
	}

	v, err := requireInt(fields, "v")
	if err != nil {
		return SignedLicense{}, err
	}
	if v != 1 {
		return SignedLicense{}, fmt.Errorf("envelope: unsupported version %d", v)
	}

	seatsRaw, _ := mustField(fields, "seats")
	assetCapRaw, _ := mustField(fields, "asset_cap")
	hostFpRaw, _ := mustField(fields, "host_fp")
	featuresRaw, _ := mustField(fields, "features")
	orgPubkeyRaw := fields["org_pubkey"]
	boundDomainsRaw := fields["bound_domains"]

	nbf, err := isoToEpoch(must(fields, "nbf"))
	if err != nil {
		return SignedLicense{}, fmt.Errorf("envelope: nbf: %w", err)
	}
	exp, err := isoToEpoch(must(fields, "exp"))
	if err != nil {
		return SignedLicense{}, fmt.Errorf("envelope: exp: %w", err)
	}
	iat, err := isoToEpoch(must(fields, "iat"))
	if err != nil {
		return SignedLicense{}, fmt.Errorf("envelope: iat: %w", err)
	}
	swd, err := requireInt(fields, "seat_window_days")
	if err != nil {
		return SignedLicense{}, err
	}

	claims := LicenseClaims{
		V:               1,
		KID:             must(fields, "kid"),
		LID:             must(fields, "lid"),
		Product:         must(fields, "prod"),
		Tier:            must(fields, "tier"),
		Seats:           parseNullableInt64(seatsRaw),
		SeatWindowDays:  swd,
		AssetCap:        parseNullableInt64(assetCapRaw),
		Owner:           must(fields, "owner"),
		Org:             must(fields, "org"),
		NotBefore:       nbf,
		Expires:         exp,
		IssuedAt:        iat,
		Features:        parseFeatures(featuresRaw),
		HostFingerprint: parseNullableString(hostFpRaw),
		OrgPubkey:       parseNullableString(orgPubkeyRaw),
		BoundDomains:    parseNullableStringSlice(boundDomainsRaw),
		Issuer:          must(fields, "iss"),
	}

	sig, ok := fields["sig"]
	if !ok || sig == "" {
		return SignedLicense{}, fmt.Errorf("envelope: missing signature")
	}
	if len(sig) < 80 {
		return SignedLicense{}, fmt.Errorf("envelope: signature truncated")
	}

	return SignedLicense{Claims: claims, SignatureB64u: sig}, nil
}

// DecodePublisherCert parses a publisher-cert envelope.
func DecodePublisherCert(text string) (SignedPublisherCert, error) {
	raw := strings.TrimSpace(text)
	if !strings.HasPrefix(raw, certBeginMarker) {
		return SignedPublisherCert{}, fmt.Errorf("publisher cert: missing BEGIN marker")
	}
	if !strings.HasSuffix(raw, certEndMarker) {
		return SignedPublisherCert{}, fmt.Errorf("publisher cert: missing END marker")
	}
	inner := strings.TrimSpace(raw[len(certBeginMarker) : len(raw)-len(certEndMarker)])
	fields, err := parseEnvelopeFields(inner)
	if err != nil {
		return SignedPublisherCert{}, fmt.Errorf("publisher cert: %w", err)
	}

	v, err := requireInt(fields, "v")
	if err != nil {
		return SignedPublisherCert{}, err
	}
	if v != 1 {
		return SignedPublisherCert{}, fmt.Errorf("publisher cert: unsupported version %d", v)
	}

	nbf, err := isoToEpoch(must(fields, "nbf"))
	if err != nil {
		return SignedPublisherCert{}, err
	}
	exp, err := isoToEpoch(must(fields, "exp"))
	if err != nil {
		return SignedPublisherCert{}, err
	}
	iat, err := isoToEpoch(must(fields, "iat"))
	if err != nil {
		return SignedPublisherCert{}, err
	}

	claims := PublisherCertClaims{
		V:                  1,
		CertID:             must(fields, "cert_id"),
		SignedByKID:        must(fields, "signed_by_kid"),
		PublisherSlug:      must(fields, "publisher_slug"),
		PublisherKID:       must(fields, "publisher_kid"),
		PublisherPubkeyB64: must(fields, "publisher_pubkey_b64"),
		DisplayName:        must(fields, "display_name"),
		AllowedProducts:    parseFeatures(must(fields, "allowed_products")),
		NotBefore:          nbf,
		Expires:            exp,
		IssuedAt:           iat,
		Issuer:             must(fields, "iss"),
	}

	sig, ok := fields["sig"]
	if !ok || sig == "" {
		return SignedPublisherCert{}, fmt.Errorf("publisher cert: missing signature")
	}
	return SignedPublisherCert{Claims: claims, SignatureB64u: sig}, nil
}

// parseEnvelopeFields walks the envelope body line by line, returning
// a map of trimmed key → value. Multi-line continuations (anything
// starting with whitespace) are joined onto the previous field —
// matches the line-wrap behaviour of long sig lines and the TS
// encoder's wrap behaviour.
func parseEnvelopeFields(body string) (map[string]string, error) {
	fields := make(map[string]string)
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			return nil, fmt.Errorf("line without ':' — %.60q", line)
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		fields[key] = value
	}
	return fields, nil
}

func mustField(fields map[string]string, key string) (string, bool) {
	v, ok := fields[key]
	return v, ok
}

func must(fields map[string]string, key string) string {
	return fields[key]
}

func requireInt(fields map[string]string, key string) (int, error) {
	raw, ok := fields[key]
	if !ok {
		return 0, fmt.Errorf("missing %q", key)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid int %q: %w", key, err)
	}
	return n, nil
}

func parseNullableInt64(raw string) *int64 {
	if raw == "" || raw == "unlimited" {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func parseNullableString(raw string) *string {
	if raw == "" || raw == "none" {
		return nil
	}
	v := raw
	return &v
}

func parseNullableStringSlice(raw string) []string {
	if raw == "" || raw == "none" {
		return nil
	}
	return parseFeatures(raw)
}

func parseFeatures(raw string) []string {
	if raw == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isoToEpoch(iso string) (int64, error) {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return 0, fmt.Errorf("invalid RFC3339 timestamp %q: %w", iso, err)
	}
	return t.Unix(), nil
}
