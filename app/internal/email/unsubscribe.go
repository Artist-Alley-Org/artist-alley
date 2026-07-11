package email

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// RFC 8058 one-click unsubscribe (Phase 1.55.Y).
//
// The token is a stateless, signed opaque string — no DB row — so a
// recipient can one-click-unsubscribe a topic without logging in. The
// signature is HMAC-SHA256 over "<user_ref>:<topic>:<exp_unix>" keyed
// on the instance scramble key (the same primitive auth.HashPassword
// uses). The endpoint verifies the MAC + expiry, then sets that topic's
// email cadence off (drops "email" from the channel list).
//
// Token wire form: "v1.<payload_b64url>.<mac_b64url>" where payload is
// "<user_ref>:<topic>:<exp_unix>". base64url (no padding) keeps it
// header- and URL-safe.

// UnsubscribeTTL is how long a minted token stays valid. Long enough
// that a digest email sitting in an inbox for weeks still unsubscribes,
// short enough to bound replay of a leaked token.
const UnsubscribeTTL = 90 * 24 * time.Hour

// ErrBadUnsubscribeToken is returned for any malformed, tampered, or
// expired token — deliberately opaque so the endpoint can't be used as
// an oracle.
var ErrBadUnsubscribeToken = errors.New("email: invalid or expired unsubscribe token")

// SignUnsubscribe mints a token for (userRef, topic) valid until now+TTL.
func SignUnsubscribe(scrambleKey string, userRef int64, topic string, now time.Time) string {
	exp := now.Add(UnsubscribeTTL).Unix()
	payload := fmt.Sprintf("%d:%s:%d", userRef, topic, exp)
	mac := unsubscribeMAC(scrambleKey, payload)
	return "v1." + b64(payload) + "." + b64(mac)
}

// VerifyUnsubscribe checks a token's signature + expiry and returns the
// (userRef, topic) it authorizes. Any failure returns
// ErrBadUnsubscribeToken.
func VerifyUnsubscribe(scrambleKey, token string, now time.Time) (int64, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, "", ErrBadUnsubscribeToken
	}
	payload, err := unb64(parts[1])
	if err != nil {
		return 0, "", ErrBadUnsubscribeToken
	}
	gotMAC, err := unb64(parts[2])
	if err != nil {
		return 0, "", ErrBadUnsubscribeToken
	}
	wantMAC := unsubscribeMAC(scrambleKey, payload)
	if !hmac.Equal([]byte(gotMAC), []byte(wantMAC)) {
		return 0, "", ErrBadUnsubscribeToken
	}
	// payload = "<user_ref>:<topic>:<exp>". topic may contain no ':'
	// (verbs are [a-z_]), so split on the first + last colon.
	first := strings.IndexByte(payload, ':')
	last := strings.LastIndexByte(payload, ':')
	if first < 0 || last <= first {
		return 0, "", ErrBadUnsubscribeToken
	}
	userRef, err := strconv.ParseInt(payload[:first], 10, 64)
	if err != nil {
		return 0, "", ErrBadUnsubscribeToken
	}
	topic := payload[first+1 : last]
	exp, err := strconv.ParseInt(payload[last+1:], 10, 64)
	if err != nil {
		return 0, "", ErrBadUnsubscribeToken
	}
	if now.Unix() > exp {
		return 0, "", ErrBadUnsubscribeToken
	}
	return userRef, topic, nil
}

// UnsubscribeHeaders builds the RFC 8058 List-Unsubscribe +
// List-Unsubscribe-Post header pair. siteURL is the instance base
// (e.g. "https://art.example.com"); token is a signed token from
// SignUnsubscribe. The mailto target is a courtesy fallback for
// clients that don't support the HTTPS one-click.
func UnsubscribeHeaders(siteURL, token string) map[string]string {
	url := UnsubscribeURL(siteURL, token)
	return map[string]string{
		"List-Unsubscribe":      "<" + url + ">",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}
}

// UnsubscribeURL is the one-click endpoint the List-Unsubscribe header
// points at. It lives under /api/v1 so mail clients POST directly to
// the backend (RFC 8058 one-click); humans who GET it see a
// confirmation page from the same handler.
func UnsubscribeURL(siteURL, token string) string {
	return strings.TrimRight(siteURL, "/") + "/api/v1/unsubscribe?token=" + token
}

func unsubscribeMAC(key, payload string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(payload))
	return string(m.Sum(nil))
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func unb64(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return string(b), err
}
