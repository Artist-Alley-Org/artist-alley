// HTTP-Signatures tests — round-trip + each error path mapped
// to the §12.1 reject reason taxonomy. Phase 1.22.D-a-2.

package httpsig_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
)

// --- helpers --------------------------------------------------------------

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// staticResolver returns a KeyResolver that knows exactly one
// keyId → public key mapping. Anything else → ErrUnknownKey.
func staticResolver(keyID string, pub ed25519.PublicKey) httpsig.KeyResolver {
	return func(k string) (ed25519.PublicKey, error) {
		if k == keyID {
			return pub, nil
		}
		return nil, httpsig.ErrUnknownKey
	}
}

func newSignedReq(t *testing.T, key ed25519.PrivateKey, keyID, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://local.example/federation/inbox",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Host", "local.example")
	if err := httpsig.SignAndAttach(req, []byte(body), keyID, key); err != nil {
		t.Fatal(err)
	}
	// http.Request from NewRequest doesn't set req.Host on the
	// inbound side; the server gets it from the request line.
	// For test purposes, the BuildSigningString reads from
	// req.Header.Get("Host") which we set above.
	return req
}

// --- round-trip happy path -----------------------------------------------

func TestSignVerify_RoundTrip(t *testing.T) {
	pub, priv := newKey(t)
	const keyID = "https://peer.example/users/alice#main-key"

	body := `{"hello":"world"}`
	req := newSignedReq(t, priv, keyID, body)

	gotKeyID, err := httpsig.Verify(req, []byte(body), staticResolver(keyID, pub), time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if gotKeyID != keyID {
		t.Errorf("returned keyId: got %q want %q", gotKeyID, keyID)
	}
}

// --- per-error-path: each maps to a §12.1 reject reason -------------------

func TestVerify_UnsignedRequest(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://x.example/", strings.NewReader(""))
	_, err := httpsig.Verify(req, nil, staticResolver("", nil), time.Now())
	if !errors.Is(err, httpsig.ErrUnsignedRequest) {
		t.Errorf("err: got %v want ErrUnsignedRequest", err)
	}
}

func TestVerify_UnsupportedAlgorithm(t *testing.T) {
	pub, priv := newKey(t)
	body := `{}`
	req := newSignedReq(t, priv, "k1", body)
	// Tamper: replace hs2019 with rsa-sha256 (the legacy non-allowlisted name).
	sig := req.Header.Get("Signature")
	tampered := strings.Replace(sig, `algorithm="hs2019"`, `algorithm="rsa-sha256"`, 1)
	req.Header.Set("Signature", tampered)
	_, err := httpsig.Verify(req, []byte(body), staticResolver("k1", pub), time.Now())
	if !errors.Is(err, httpsig.ErrUnsupportedAlgorithm) {
		t.Errorf("err: got %v want ErrUnsupportedAlgorithm", err)
	}
}

func TestVerify_SigMalformed(t *testing.T) {
	cases := []struct {
		name      string
		signature string
	}{
		{"empty", ""},
		{"no_equals", "garbage"},
		{"missing_required_param", `keyId="x",algorithm="hs2019"`},
		{"bad_base64", `keyId="x",algorithm="hs2019",headers="(request-target) host date digest",signature="!@#$"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://x.example/", strings.NewReader(""))
			req.Header.Set("Signature", tc.signature)
			_, err := httpsig.Verify(req, nil, staticResolver("", nil), time.Now())
			if err == nil {
				t.Fatalf("expected error")
			}
			// "empty" maps to ErrUnsignedRequest (semantically more
			// useful — peer just forgot to sign); the rest are
			// ErrSigMalformed.
			if tc.name == "empty" {
				if !errors.Is(err, httpsig.ErrUnsignedRequest) {
					t.Errorf("err: got %v want ErrUnsignedRequest", err)
				}
				return
			}
			if !errors.Is(err, httpsig.ErrSigMalformed) {
				t.Errorf("err: got %v want ErrSigMalformed", err)
			}
		})
	}
}

func TestVerify_MissingRequiredSignedHeader(t *testing.T) {
	pub, priv := newKey(t)
	body := `{}`
	// Sign with only host+date — missing (request-target) + digest.
	req, _ := http.NewRequest(http.MethodPost, "https://x.example/inbox", strings.NewReader(body))
	req.Header.Set("Host", "x.example")
	if _, err := httpsig.Sign(req, []byte(body), "k1", priv, []string{"host", "date"}); err != nil {
		t.Fatal(err)
	}
	// Manually attach the partial signature.
	sig, _ := httpsig.Sign(req, []byte(body), "k1", priv, []string{"host", "date"})
	req.Header.Set("Signature", sig)
	_, err := httpsig.Verify(req, []byte(body), staticResolver("k1", pub), time.Now())
	if !errors.Is(err, httpsig.ErrMissingHeader) {
		t.Errorf("err: got %v want ErrMissingHeader", err)
	}
}

func TestVerify_UnknownKey(t *testing.T) {
	_, priv := newKey(t)
	body := `{}`
	req := newSignedReq(t, priv, "k1", body)
	// Resolver knows "k2" only.
	resolver := staticResolver("k2", ed25519.PublicKey(make([]byte, 32)))
	_, err := httpsig.Verify(req, []byte(body), resolver, time.Now())
	if !errors.Is(err, httpsig.ErrUnknownKey) {
		t.Errorf("err: got %v want ErrUnknownKey", err)
	}
}

func TestVerify_DigestMismatch(t *testing.T) {
	pub, priv := newKey(t)
	body := `{"a":1}`
	req := newSignedReq(t, priv, "k1", body)
	// Verify with a DIFFERENT body — digest header still matches the
	// original; the body bytes differ.
	_, err := httpsig.Verify(req, []byte(`{"a":2}`), staticResolver("k1", pub), time.Now())
	if !errors.Is(err, httpsig.ErrDigestMismatch) {
		t.Errorf("err: got %v want ErrDigestMismatch", err)
	}
}

func TestVerify_StaleRequest(t *testing.T) {
	pub, priv := newKey(t)
	body := `{}`
	req := newSignedReq(t, priv, "k1", body)
	// Verify with "now" pushed 10min into the future — Date is ~now.
	future := time.Now().Add(10 * time.Minute)
	_, err := httpsig.Verify(req, []byte(body), staticResolver("k1", pub), future)
	if !errors.Is(err, httpsig.ErrStaleRequest) {
		t.Errorf("err: got %v want ErrStaleRequest", err)
	}
}

func TestVerify_SigInvalid(t *testing.T) {
	pub1, priv1 := newKey(t)
	_, priv2 := newKey(t)
	body := `{}`
	// Sign with priv1 but resolver maps the keyId to pub from a
	// DIFFERENT keypair — signature won't verify.
	req := newSignedReq(t, priv1, "k1", body)
	resolver := func(k string) (ed25519.PublicKey, error) {
		if k == "k1" {
			// Wrong key.
			pub2 := ed25519.PublicKey(priv2.Public().(ed25519.PublicKey))
			_ = pub1 // keep import live
			return pub2, nil
		}
		return nil, httpsig.ErrUnknownKey
	}
	_, err := httpsig.Verify(req, []byte(body), resolver, time.Now())
	if !errors.Is(err, httpsig.ErrSigInvalid) {
		t.Errorf("err: got %v want ErrSigInvalid", err)
	}
}

// --- low-level pieces ----------------------------------------------------

func TestComputeDigest_Stable(t *testing.T) {
	body := []byte(`{"a":1,"b":2}`)
	if got, want := httpsig.ComputeDigest(body),
		"SHA-256=QyWM/3g/5wNtikMDP4MK38YOwDc4JHNUisdCuIgpJ3c="; got != want {
		t.Errorf("digest: got %q want %q", got, want)
	}
}

func TestBuildSigningString_OrderingPreserved(t *testing.T) {
	h := http.Header{}
	h.Set("Host", "x.example")
	h.Set("Date", "Tue, 07 Jun 2026 00:00:00 GMT")
	h.Set("Digest", "SHA-256=AAA=")
	got, err := httpsig.BuildSigningString(http.MethodPost, "/inbox",
		[]string{"(request-target)", "host", "date", "digest"}, h)
	if err != nil {
		t.Fatal(err)
	}
	want := "(request-target): post /inbox\nhost: x.example\ndate: Tue, 07 Jun 2026 00:00:00 GMT\ndigest: SHA-256=AAA="
	if got != want {
		t.Errorf("\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestSplitParams_HandlesCommasInsideQuotes(t *testing.T) {
	pub, priv := newKey(t)
	const keyID = "https://peer.example/users/alice?org=x,y#main-key"
	body := `{}`
	req := newSignedReq(t, priv, keyID, body)
	gotKeyID, err := httpsig.Verify(req, []byte(body), staticResolver(keyID, pub), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if gotKeyID != keyID {
		t.Errorf("keyId with comma in URL: got %q want %q", gotKeyID, keyID)
	}
}

func TestVerify_DigestHeaderRequired(t *testing.T) {
	// Sign normally (Digest IS in the signed-headers list), then
	// delete the Digest header from the request before Verify.
	// Verify must surface ErrMissingHeader for "digest" — the
	// gold-standard fix for the previously-silent "if got != ''"
	// branch.
	pub, priv := newKey(t)
	body := `{}`
	req := newSignedReq(t, priv, "k1", body)
	req.Header.Del("Digest")
	_, err := httpsig.Verify(req, []byte(body), staticResolver("k1", pub), time.Now())
	if !errors.Is(err, httpsig.ErrMissingHeader) {
		t.Errorf("err: got %v want ErrMissingHeader for missing Digest", err)
	}
}

func TestVerify_AfterDrainBody_PreservesPayloadForReVerify(t *testing.T) {
	// This is the actual inbox pipeline usage pattern: handler
	// receives request → drain body → verify signature → parse
	// envelope. The drained bytes must equal what Verify hashed
	// for Digest, otherwise the production pipeline silently
	// drops correct envelopes.
	pub, priv := newKey(t)
	body := `{"id":"https://peer.example/activities/abc","type":"Like"}`
	req := newSignedReq(t, priv, "k1", body)
	got, err := httpsig.DrainBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := httpsig.Verify(req, got, staticResolver("k1", pub), time.Now()); err != nil {
		t.Fatalf("verify after drain: %v", err)
	}
	// Body must still parse as JSON (the inbox handler does
	// this next).
	var envelope map[string]any
	body2, _ := httpsig.DrainBody(req)
	if err := json.Unmarshal(body2, &envelope); err != nil {
		t.Errorf("body unparseable after drain+verify: %v", err)
	}
	if envelope["type"] != "Like" {
		t.Errorf("envelope type lost in round-trip: %+v", envelope)
	}
}

func TestBuildSigningString_MatchesConformanceVector(t *testing.T) {
	// Pinned conformance check: BuildSigningString output must
	// match testdata/conformance/http-sig/inbox_post.canonical.txt
	// byte-for-byte. Drift here is a wire-format breaking change.
	dir := filepath.Join("..", "testdata", "conformance", "http-sig")
	input := loadConformanceInput(t, filepath.Join(dir, "inbox_post.input.json"))
	wantBytes, err := os.ReadFile(filepath.Join(dir, "inbox_post.canonical.txt"))
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	for k, v := range input.Headers {
		h.Set(k, v)
	}
	got, err := httpsig.BuildSigningString(input.Method, input.Path, input.SignedHeaders, h)
	if err != nil {
		t.Fatalf("BuildSigningString: %v", err)
	}
	if got != string(wantBytes) {
		t.Errorf("canonical signing string drift\nGOT:\n%s\nWANT:\n%s",
			got, string(wantBytes))
	}
}

type conformanceInput struct {
	Method        string            `json:"method"`
	Path          string            `json:"path"`
	Headers       map[string]string `json:"headers"`
	SignedHeaders []string          `json:"signedHeaders"`
}

func loadConformanceInput(t *testing.T, path string) conformanceInput {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c conformanceInput
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDrainBody_LeavesBodyReadable(t *testing.T) {
	const want = `{"hello":"world"}`
	req, _ := http.NewRequest(http.MethodPost, "https://x.example/", bytes.NewReader([]byte(want)))
	got, err := httpsig.DrainBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("drained bytes mismatch: %q != %q", string(got), want)
	}
	// Body should still be readable.
	body := make([]byte, len(want))
	n, _ := req.Body.Read(body)
	if string(body[:n]) != want {
		t.Errorf("re-read body: got %q want %q", string(body[:n]), want)
	}
}
