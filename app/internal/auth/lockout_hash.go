package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// lookupUserRefByUsername resolves a username to a user_ref for the
// pre-auth lockout gate. Case-insensitive per FindUserByUsername.
// Returns (0, false) on not-found OR error — the caller treats
// "unknown user" identically to "user found" for enumeration
// protection.
//
// The provider's Authenticate does its own lookup; this pre-check
// adds one DB roundtrip per login attempt but Postgres's statement
// cache keeps the parse+plan work amortised.
func (h *Handler) lookupUserRefByUsername(ctx context.Context, username string) (int64, bool) {
	trimmed := strings.TrimSpace(username)
	if trimmed == "" {
		return 0, false
	}
	q := New(h.Pool)
	row, err := q.FindUserByUsername(ctx, &trimmed)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.lockout.user_lookup_error",
				slog.String("err", err.Error()))
		}
		return 0, false
	}
	return row.Ref, true
}

// ipSubnetHash returns a base64 HMAC-SHA256 digest of the client's IP
// subnet, salted with the provided secret. Used by 1.19.D lockout
// audits to record threat class (subnet) without becoming a per-IP
// audit log.
//
// Subnet size:
//   - IPv4: /24 (first three octets)
//   - IPv6: /56 (first 56 bits)
//
// Rationale: /24 is the standard "same ISP customer" partition for
// IPv4; /56 matches the IPv6 SLAAC prefix delegation size RFC 6177
// recommends for end-sites. Attackers rotating within a single subnet
// still collapse to one audit row per lockout; attackers spanning
// multiple subnets produce one audit row per subnet — the useful
// signal for detecting distributed campaigns.
//
// Empty salt returns empty string (audit fires with no hash; caller
// can treat as "not configured"). Nil request returns empty. Never
// panics on malformed IPs — returns empty.
func ipSubnetHash(req *http.Request, salt string) string {
	return IPSubnetHashWithDomain(req, salt, "lockout.v1:")
}

// IPSubnetHash is the shared helper for /24 IPv4 + /56 IPv6 HMAC-
// SHA256 subnet hashing. Exported so other subsystems that record
// threat-class-per-request audits (Phase 1.16.B-5-followup's search
// feedback, and future audit surfaces) can reuse the exact same
// masking + salt pattern that 1.19.D lockout uses. The `domain`
// arg is prepended before the subnet bytes so different subsystems
// produce collision-independent hashes on rotated salts — pass
// something short-and-versioned like "search.feedback.v1:".
func IPSubnetHash(req *http.Request, salt, domain string) string {
	return IPSubnetHashWithDomain(req, salt, domain)
}

// IPSubnetHashWithDomain is the internal implementation that both
// exported callers and the package-local ipSubnetHash delegate to.
// Kept exported (via IPSubnetHash) + wrapped so the internal lockout
// path can pin its own domain string as a source-of-truth constant.
func IPSubnetHashWithDomain(req *http.Request, salt, domain string) string {
	if req == nil || salt == "" {
		return ""
	}
	ipStr := clientIPKey(req)
	if ipStr == "" {
		return ""
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	var mask net.IPMask
	if v4 := ip.To4(); v4 != nil {
		ip = v4
		mask = net.CIDRMask(24, 32)
	} else {
		mask = net.CIDRMask(56, 128)
	}
	subnet := ip.Mask(mask)
	mac := hmac.New(sha256.New, []byte(salt))
	if domain != "" {
		mac.Write([]byte(domain))
	}
	mac.Write(subnet)
	digest := mac.Sum(nil)[:16]
	return strings.TrimRight(base64.URLEncoding.EncodeToString(digest), "=")
}
