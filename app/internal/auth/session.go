package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
)

// SessionCookieName matches the legacy PHP-side cookie. The legacy code
// reads/writes a cookie named exactly "user" (see include/authenticate.php and
// include/login_functions.php::set_login_cookies). Keeping the name
// identical is what makes login interoperable between Go and PHP
// during the transition; after PHP is retired we may rename to
// something less generic but every PHP path key on "user" so it
// stays as-is for now.
const SessionCookieName = "user"

// TokenPrefix is the visible marker stamped on every artist-alley
// Personal Access Token. It lets users (and leak-scanners) recognise
// the token type at a glance.
const TokenPrefix = "aa_pat_"

// sessionTokenBytes is the number of random bytes baked into a
// session-cookie value. 18 raw bytes -> 24 base64url chars (no
// padding), comfortably under the user.session column's 50-char
// width and well above any practical entropy floor (144 bits).
const sessionTokenBytes = 18

// apiTokenBytes is the random length for Personal Access Tokens.
// 24 raw bytes -> 32 base64url chars; with the "aa_pat_" prefix the
// token is 39 chars total.
const apiTokenBytes = 24

// NewSessionToken returns a fresh random string suitable for storing
// in user.session and sending in the rs_session cookie.
func NewSessionToken() (string, error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewAPIToken returns a fresh Personal Access Token in the form
// "aa_pat_<32 random base64url chars>".
func NewAPIToken() (string, error) {
	b := make([]byte, apiTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashAPIToken is the function we store in api_tokens.token_hash. A
// stolen DB snapshot cannot be replayed because only the digest is
// kept; clients always send the plaintext token over TLS.
func HashAPIToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// LooksLikeAPIToken returns true if s plausibly matches our PAT format.
// Cheap pre-filter to avoid pointlessly hashing every Authorization
// value that flies past the middleware.
func LooksLikeAPIToken(s string) bool {
	return strings.HasPrefix(s, TokenPrefix) && len(s) >= len(TokenPrefix)+16
}

// WriteSessionCookie sets the rs_session cookie on w with the same
// flags rs_setcookie() uses on the PHP side: HttpOnly, SameSite=Strict,
// Path=/, Secure when the request scheme is https.
func WriteSessionCookie(w http.ResponseWriter, r *http.Request, token string, daysExpire int) {
	expires := time.Time{}
	if daysExpire > 0 {
		expires = time.Now().Add(time.Duration(daysExpire) * 24 * time.Hour)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie expires the rs_session cookie on w. Matches
// rs_setcookie(.., -1) on the PHP side.
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// ExtractBearerToken pulls "<scheme> <value>" out of the Authorization
// header. Returns the token (with prefix preserved) and a bool
// indicating whether it was a Bearer header.
func ExtractBearerToken(h http.Header) (string, bool) {
	raw := h.Get("Authorization")
	if raw == "" {
		return "", false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(raw[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// SessionCookieValue returns the rs_session value from r, or "" if not
// present. Wraps r.Cookie so callers don't pull in net/http just for
// this.
func SessionCookieValue(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c == nil {
		return ""
	}
	return c.Value
}

// ErrNoCredentials indicates the caller presented neither a session
// cookie nor a Bearer token.
var ErrNoCredentials = errors.New("auth: no credentials presented")

// isHTTPS returns true when the request appears to have arrived over
// TLS. nginx may terminate TLS upstream and pass the actual scheme via
// X-Forwarded-Proto; we honour either signal.
func isHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		return true
	}
	return false
}
