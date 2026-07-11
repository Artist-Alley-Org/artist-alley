// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package dnstxt implements the artist-alley federation
// directory DNS-TXT verification protocol per
// docs/spec/federation-directory/v1.md §"DNS-TXT verification".
//
// The protocol exists so a federation directory entry isn't
// just self-declared: the listed instance must publish a TXT
// record at `_artist-alley.<host>` containing a directory-issued
// token, proving control of the domain. Without that, the
// directory is a phishing surface (anyone can claim to be
// `blizzard.com`).
//
// # Record format
//
//   _artist-alley.<host>  TXT  "v=aa1; directory=<host>; token=<hex>"
//
// `v=` — version (must be `aa1`).
// `directory=` — bare hostname of the directory operator.
// `token=` — the per-challenge token issued by /v1/challenge.
//
// Multiple TXT records at the same name are allowed (one per
// directory the instance has registered with); the verifier scans
// all of them and returns true on the first matching one.
//
// # Caching
//
// DNS lookups themselves are cached by the resolver (system or
// cgo). The verifier doesn't add a second layer — DNS TTLs are
// the source of truth. Operators who want faster invalidation
// publish shorter TTLs.

package dnstxt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Errors callers may distinguish on.
var (
	// ErrNoMatchingRecord — no TXT record at _artist-alley.<host>
	// matched the expected (version, directory, token) triple.
	// Catch-all for the "domain control not proven" outcome.
	ErrNoMatchingRecord = errors.New("dnstxt: no matching TXT record")

	// ErrBadInstanceURL — the supplied instance URL didn't have
	// a host we could derive a TXT lookup name from.
	ErrBadInstanceURL = errors.New("dnstxt: instance URL missing host")

	// ErrLookupFailed — the DNS lookup itself failed (NXDOMAIN,
	// timeout, etc.). Wraps the underlying net error so callers
	// can introspect via errors.As.
	ErrLookupFailed = errors.New("dnstxt: lookup failed")
)

// Resolver is the DNS interface — std lib net.Resolver satisfies
// it. Defined as an interface so tests can inject a fake.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// DefaultResolver wraps the std library's default resolver. Use
// this in production; tests pass a fake.
var DefaultResolver Resolver = &net.Resolver{}

// VerifyInput captures the four values the verifier compares
// against TXT records under the host. All fields required.
type VerifyInput struct {
	// InstanceURL is the URL the listing claims; the verifier
	// extracts host and looks up `_artist-alley.<host>`.
	InstanceURL string
	// DirectoryHost is the bare hostname of the directory
	// operator the proof is for — must match `directory=` in the
	// TXT record. Defends against a malicious directory replaying
	// a token to a different directory's verifier.
	DirectoryHost string
	// Token is the challenge token issued by /v1/challenge.
	Token string
}

// Verify returns nil when at least one TXT record under
// `_artist-alley.<host>` matches the (version=aa1, directory,
// token) triple. Returns ErrNoMatchingRecord on a clean negative
// result, ErrLookupFailed on a DNS-layer error, or
// ErrBadInstanceURL on input shape problems.
//
// The CONTEXT controls the lookup deadline; callers should pass
// a context with a sensible timeout (DNS can stall for tens of
// seconds otherwise).
func Verify(ctx context.Context, r Resolver, in VerifyInput) error {
	if r == nil {
		r = DefaultResolver
	}
	host, err := hostFromURL(in.InstanceURL)
	if err != nil {
		return err
	}
	if strings.TrimSpace(in.DirectoryHost) == "" || strings.TrimSpace(in.Token) == "" {
		return errors.New("dnstxt: directory host + token both required")
	}
	name := "_artist-alley." + host
	records, err := r.LookupTXT(ctx, name)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrLookupFailed, err)
	}
	wantDirectory := strings.ToLower(strings.TrimSpace(in.DirectoryHost))
	wantToken := strings.TrimSpace(in.Token)
	for _, rec := range records {
		if matchesRecord(rec, wantDirectory, wantToken) {
			return nil
		}
	}
	return ErrNoMatchingRecord
}

// matchesRecord parses one TXT value + checks all required
// attributes. Exported via a test-only flavour as MatchesRecord
// below (tests want to exercise the parser in isolation).
func matchesRecord(rec, wantDirectory, wantToken string) bool {
	attrs := parseAttrs(rec)
	if attrs["v"] != "aa1" {
		return false
	}
	if strings.ToLower(strings.TrimSpace(attrs["directory"])) != wantDirectory {
		return false
	}
	if strings.TrimSpace(attrs["token"]) != wantToken {
		return false
	}
	return true
}

// MatchesRecord is the test-visible flavour of matchesRecord.
func MatchesRecord(rec, wantDirectory, wantToken string) bool {
	return matchesRecord(rec, wantDirectory, wantToken)
}

// parseAttrs splits a "key=value; key=value; ..." TXT string
// into a map. Lowercases keys; preserves value case (tokens are
// case-sensitive hex).
//
// Tolerant of inconsistent spacing + trailing semicolons —
// operators copy-paste records and we want forgiving parsing.
// Strict on unknown keys: ignored (forward-compatible).
func parseAttrs(rec string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(rec, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(part[:eq]))
		val := strings.TrimSpace(part[eq+1:])
		out[key] = val
	}
	return out
}

// RecordValue formats the canonical TXT record value the
// directory hands to instances at challenge issue time. Stable
// shape; subscribers + verifiers both parse this format.
func RecordValue(directoryHost, token string) string {
	return fmt.Sprintf("v=aa1; directory=%s; token=%s",
		strings.ToLower(strings.TrimSpace(directoryHost)),
		strings.TrimSpace(token),
	)
}

// RecordName returns the canonical record name for the given
// instance URL. Returns ErrBadInstanceURL on malformed input.
func RecordName(instanceURL string) (string, error) {
	host, err := hostFromURL(instanceURL)
	if err != nil {
		return "", err
	}
	return "_artist-alley." + host, nil
}

// hostFromURL extracts the bare host from an https URL. Strips
// port if present; lowercases (DNS is case-insensitive).
func hostFromURL(u string) (string, error) {
	s := strings.TrimSpace(u)
	if !strings.HasPrefix(s, "https://") {
		return "", fmt.Errorf("%w: must be https://", ErrBadInstanceURL)
	}
	parsed, err := url.Parse(s)
	if err != nil || parsed.Host == "" {
		return "", ErrBadInstanceURL
	}
	host := parsed.Hostname()
	if host == "" {
		return "", ErrBadInstanceURL
	}
	return strings.ToLower(host), nil
}
