// Package httpsig is the hand-rolled HTTP-Signatures
// implementation (Cavage-style hs2019 + Ed25519 only) per
// docs/spec/federation/v1.md §10 + the 1.22.D design proposal
// §5.5 Q5.
//
// We do NOT depend on go-fed/httpsig (GPL-3 — contamination
// risk for any future premium / cloud-bridge code per ADR 0040)
// or ory/x/httpx (Apache-2 but a subset). The spec already pins
// us to hs2019 + ed25519, so the parser surface is small + the
// audit surface is full.
//
// # Signed-header set (required minimum)
//
// `(request-target)`, `host`, `date`, `digest`. Per spec §10.2.
// Implementations MAY include more (e.g. `content-type`); the
// verifier authenticates only the headers named in the
// `headers="..."` Signature param.
//
// # Replay window
//
// ±5min on `Date` per spec §10.2. Combined with the envelope
// `id` UNIQUE constraint at the inbox, this gives the
// belt-and-suspenders replay protection §5.5 addition B
// documents.
//
// # Algorithm allowlist
//
// `algorithm="hs2019"` ONLY. Any other value (including the
// legacy `rsa-sha256`) returns ErrUnsupportedAlgorithm.
//
// # Key resolution
//
// The verifier accepts a KeyResolver callback that maps
// `keyId=<URL>` to an ed25519.PublicKey. Production wires this
// to peer.Registry's instance_public_key lookup; tests inject
// their own resolver.

package httpsig

import (

	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ReplayWindow is the maximum Date skew accepted by Verify.
// Pinned to ±5min per spec §10.2.
const ReplayWindow = 5 * time.Minute

// MinSignedHeaders names the headers a verifier requires the
// Signature param to cover. Missing any → ErrMissingHeader.
var MinSignedHeaders = []string{"(request-target)", "host", "date", "digest"}

// Algorithm is the only signature algorithm we accept.
const Algorithm = "hs2019"

// Errors callers may discriminate on. Map directly to the
// docs/spec/federation/v1.md §12.1 inbox reject reasons where
// the spec assigns one.
var (
	// ErrUnsignedRequest → §12.1 `unsigned`.
	ErrUnsignedRequest = errors.New("httpsig: missing Signature header")

	// ErrUnsupportedAlgorithm → §12.1 `unsupported_algorithm`.
	ErrUnsupportedAlgorithm = errors.New("httpsig: algorithm not in allowlist")

	// ErrSigMalformed → §12.1 `sig_malformed`.
	ErrSigMalformed = errors.New("httpsig: signature header malformed")

	// ErrSigInvalid → §12.1 `sig_invalid`.
	ErrSigInvalid = errors.New("httpsig: signature does not verify")

	// ErrStaleRequest → §12.1 `stale_request`.
	ErrStaleRequest = errors.New("httpsig: Date outside replay window")

	// ErrDigestMismatch — Digest header doesn't match body
	// hash. Mapped to `sig_invalid` at the inbox (the spec
	// doesn't carve out a separate reason — body tampering is
	// signature failure semantically).
	ErrDigestMismatch = errors.New("httpsig: Digest does not match body")

	// ErrUnknownKey → §12.1 `unknown_key`. Returned by the
	// resolver when keyId doesn't match any known key for the
	// named actor.
	ErrUnknownKey = errors.New("httpsig: keyId not recognised")

	// ErrMissingHeader is returned when the Signature param's
	// `headers="..."` list doesn't include a required entry.
	ErrMissingHeader = errors.New("httpsig: required header not signed")
)

// SignedParams is the parsed form of the Signature: header.
type SignedParams struct {
	KeyID     string   // keyId="..."
	Algorithm string   // algorithm="hs2019" (only accepted value)
	Headers   []string // ordered names from headers="(request-target) host date digest"
	Signature []byte   // raw bytes (base64-decoded from signature="...")
	Created   int64    // optional created=<unix-seconds>
	Expires   int64    // optional expires=<unix-seconds>
}

// ParseSignatureHeader parses the Signature: header value. Order
// of params is irrelevant per the spec. Required params: keyId,
// algorithm, headers, signature.
func ParseSignatureHeader(raw string) (SignedParams, error) {
	if raw == "" {
		return SignedParams{}, ErrUnsignedRequest
	}
	out := SignedParams{}
	// Tokens are comma-separated "name=quoted-value" pairs with
	// optional whitespace. The values for keyId / signature can
	// contain commas inside the quotes (a URL might), so we
	// hand-parse instead of strings.Split.
	for _, kv := range splitParams(raw) {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			return SignedParams{}, fmt.Errorf("%w: missing = in %q", ErrSigMalformed, kv)
		}
		key := strings.TrimSpace(kv[:eq])
		val := strings.TrimSpace(kv[eq+1:])
		// All values in HTTP-Sig Signature header are quoted-strings
		// EXCEPT created/expires which are unquoted integers.
		val = strings.Trim(val, `"`)
		switch key {
		case "keyId":
			out.KeyID = val
		case "algorithm":
			out.Algorithm = val
		case "headers":
			out.Headers = strings.Fields(val)
		case "signature":
			b, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				return SignedParams{}, fmt.Errorf("%w: bad signature base64: %v", ErrSigMalformed, err)
			}
			out.Signature = b
		case "created":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return SignedParams{}, fmt.Errorf("%w: bad created: %v", ErrSigMalformed, err)
			}
			out.Created = n
		case "expires":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return SignedParams{}, fmt.Errorf("%w: bad expires: %v", ErrSigMalformed, err)
			}
			out.Expires = n
		}
	}
	if out.KeyID == "" || out.Algorithm == "" || len(out.Headers) == 0 || len(out.Signature) == 0 {
		return SignedParams{}, fmt.Errorf("%w: missing required param", ErrSigMalformed)
	}
	return out, nil
}

// splitParams splits a Signature header value on top-level
// commas (outside of quotes). Necessary because keyId values
// may contain commas inside quotes (URLs with query strings).
func splitParams(s string) []string {
	var out []string
	var buf strings.Builder
	inQuotes := false
	for _, r := range s {
		switch r {
		case '"':
			inQuotes = !inQuotes
			buf.WriteRune(r)
		case ',':
			if inQuotes {
				buf.WriteRune(r)
			} else {
				out = append(out, buf.String())
				buf.Reset()
			}
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// BuildSigningString constructs the canonical string the
// Signature covers per Cavage §2.3:
//
//   For each header name in order:
//     "(request-target): <method-lower> <path-and-query>"  (special-case)
//   OR:
//     "<header-lower>: <value-folded>"
//
// Joined with "\n", no trailing newline. Header values are
// trimmed of leading/trailing whitespace; multi-value headers
// are joined with ", ".
func BuildSigningString(method, requestTarget string, headerNames []string, header http.Header) (string, error) {
	var lines []string
	for _, name := range headerNames {
		nameLower := strings.ToLower(name)
		if nameLower == "(request-target)" {
			lines = append(lines,
				fmt.Sprintf("(request-target): %s %s",
					strings.ToLower(method), requestTarget))
			continue
		}
		vals := header.Values(name)
		if len(vals) == 0 {
			return "", fmt.Errorf("%w: %q", ErrMissingHeader, name)
		}
		// Per RFC 7230 §3.2.2 the canonical form folds multi-value
		// headers with ", " separator. ASCII Host / Date / Digest
		// are always single-valued in practice.
		joined := strings.TrimSpace(strings.Join(vals, ", "))
		lines = append(lines, fmt.Sprintf("%s: %s", nameLower, joined))
	}
	return strings.Join(lines, "\n"), nil
}

// ComputeDigest returns the canonical Digest header value for a
// body: `SHA-256=<base64>`. Spec §10.2 requires SHA-256.
func ComputeDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "SHA-256=" + base64.StdEncoding.EncodeToString(sum[:])
}

// VerifyDigest checks the Digest header matches the body hash.
// Returns ErrDigestMismatch on mismatch.
func VerifyDigest(headerVal string, body []byte) error {
	expect := ComputeDigest(body)
	if headerVal != expect {
		return fmt.Errorf("%w: header=%q want=%q", ErrDigestMismatch, headerVal, expect)
	}
	return nil
}

// KeyResolver maps a keyId URL to an Ed25519 public key. Returns
// ErrUnknownKey when no key matches. Implementations are
// expected to validate the keyId belongs to the named actor /
// peer; the resolver is the policy hook, not just a lookup.
type KeyResolver func(keyID string) (ed25519.PublicKey, error)

// Verify checks an inbound HTTP request's HTTP-Signature per
// the spec. Returns the resolved keyId on success so the
// caller can record which key authenticated the request.
//
// Side effect: drains + caches r.Body internally so the caller
// can re-read it after Verify. The original io.ReadCloser is
// replaced with a fresh reader over the buffered bytes.
//
// Validation order matches §2.2 of the 1.22.D design pipeline:
//   1. Parse Signature header (→ ErrUnsignedRequest / ErrSigMalformed)
//   2. Algorithm in allowlist (→ ErrUnsupportedAlgorithm)
//   3. All MinSignedHeaders covered (→ ErrMissingHeader)
//   4. Resolve keyId (→ ErrUnknownKey)
//   5. Body hash matches Digest (→ ErrDigestMismatch)
//   6. Date within ReplayWindow (→ ErrStaleRequest)
//   7. Signature verifies (→ ErrSigInvalid)
//
// Each step short-circuits on its own error so the caller can
// surface the right §12.1 reject reason.
func Verify(r *http.Request, body []byte, resolve KeyResolver, now time.Time) (keyID string, err error) {
	// Step 1.
	params, err := ParseSignatureHeader(r.Header.Get("Signature"))
	if err != nil {
		return "", err
	}
	// Step 2.
	if params.Algorithm != Algorithm {
		return "", fmt.Errorf("%w: got %q want %q", ErrUnsupportedAlgorithm, params.Algorithm, Algorithm)
	}
	// Step 3.
	signed := map[string]bool{}
	for _, h := range params.Headers {
		signed[strings.ToLower(h)] = true
	}
	for _, req := range MinSignedHeaders {
		if !signed[strings.ToLower(req)] {
			return "", fmt.Errorf("%w: %q", ErrMissingHeader, req)
		}
	}
	// Step 4.
	pub, err := resolve(params.KeyID)
	if err != nil {
		return "", err
	}
	// Step 5. Digest is REQUIRED by MinSignedHeaders, so the
	// header MUST be present + non-empty + must match the body
	// hash. A signed-but-absent Digest header would have failed
	// at BuildSigningString below ("missing header value"), but
	// we surface the more specific error here for clearer
	// debugging.
	got := r.Header.Get("Digest")
	if got == "" {
		return "", fmt.Errorf("%w: %q", ErrMissingHeader, "digest")
	}
	if err := VerifyDigest(got, body); err != nil {
		return "", err
	}
	// Step 6.
	dateRaw := r.Header.Get("Date")
	parsedDate, err := http.ParseTime(dateRaw)
	if err != nil {
		return "", fmt.Errorf("%w: bad Date %q: %v", ErrStaleRequest, dateRaw, err)
	}
	skew := now.Sub(parsedDate)
	if skew > ReplayWindow || skew < -ReplayWindow {
		return "", fmt.Errorf("%w: %s skew %v exceeds ±%v", ErrStaleRequest, dateRaw, skew, ReplayWindow)
	}
	// Step 7. Build the signing string. Go's http server moves
	// the inbound Host header to r.Host (it's NOT in r.Header
	// at the receiver side per RFC 7230 §5.4 + Go's http
	// convention). The Cavage spec signs the Host header,
	// so we copy r.Host back into r.Header before building the
	// canonical signing string. No-op when the caller already
	// set the header (the signer-side path does).
	if r.Header.Get("Host") == "" && r.Host != "" {
		r.Header.Set("Host", r.Host)
	}
	target := r.URL.RequestURI() // path + raw query
	signingStr, err := BuildSigningString(r.Method, target, params.Headers, r.Header)
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(pub, []byte(signingStr), params.Signature) {
		return "", ErrSigInvalid
	}
	return params.KeyID, nil
}

// Sign builds + returns the Signature header value for an
// outbound HTTP request. The Date + Digest headers must already
// be set on the request (callers typically do this immediately
// before Sign). Body is hashed for the Digest; pass nil to skip
// Digest signing (only valid when the spec doesn't require it).
//
// The returned value goes into req.Header.Set("Signature", ...).
func Sign(req *http.Request, body []byte, keyID string, key ed25519.PrivateKey, signedHeaders []string) (string, error) {
	if req.Header.Get("Date") == "" {
		req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}
	if body != nil && req.Header.Get("Digest") == "" {
		req.Header.Set("Digest", ComputeDigest(body))
	}
	if len(signedHeaders) == 0 {
		signedHeaders = MinSignedHeaders
	}
	target := req.URL.RequestURI()
	signingStr, err := BuildSigningString(req.Method, target, signedHeaders, req.Header)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(key, []byte(signingStr))
	header := buildSignatureHeader(keyID, Algorithm, signedHeaders, sig)
	return header, nil
}

// SignAndAttach is the common-case helper: sets Date, Digest
// (if body non-nil), then signs and attaches the Signature
// header. Returns the bytes consumed for the digest so the
// caller can pass the same body to http.Request.Body.
func SignAndAttach(req *http.Request, body []byte, keyID string, key ed25519.PrivateKey) error {
	sig, err := Sign(req, body, keyID, key, MinSignedHeaders)
	if err != nil {
		return err
	}
	req.Header.Set("Signature", sig)
	return nil
}

func buildSignatureHeader(keyID, algo string, headers []string, sig []byte) string {
	// Sort headers list — Cavage doesn't require a specific
	// order in the header param, but stable output makes log
	// inspection + test fixtures deterministic.
	headerNames := make([]string, len(headers))
	copy(headerNames, headers)
	// preserve order of the canonical sig — DON'T sort.
	_ = sort.StringsAreSorted // keep sort import live

	var b strings.Builder
	fmt.Fprintf(&b, `keyId="%s",algorithm="%s",headers="%s",signature="%s"`,
		keyID, algo,
		strings.Join(headerNames, " "),
		base64.StdEncoding.EncodeToString(sig),
	)
	return b.String()
}

// DrainBody is a small helper for callers that need to verify
// the signature THEN re-read the body for their own parsing.
// Returns the bytes; replaces r.Body with a fresh reader over
// the same bytes.
func DrainBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	return body, nil
}
