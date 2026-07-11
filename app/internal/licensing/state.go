// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// In-memory cached license state + file loader.
//
// State is the concrete implementation of the Source interface.
// Construct one at server startup via NewState(path, logger). It
// reads the .lic file once at boot, verifies, and caches the
// resulting Status. The cached Status is what every dependent
// package reads via the Source interface.
//
// Reloads:
//   - Reload(ctx) is a manual trigger — admin handler calls it after
//     a successful POST /admin/license/upload (next PR).
//   - No filesystem-watch loop in v1 — explicit reloads are simpler
//     to reason about + audit. A future enhancement could watch
//     mtime on the .lic path.
//
// Concurrency:
//   - sync.RWMutex around the cached Status field.
//   - Reads (Tier / HasFeature / Features / Status) take RLock.
//   - Reloads take Lock for the brief swap.

package licensing

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// State holds the cached snapshot. Implements Source.
type State struct {
	path       string
	orgKeyPath string
	logger     *slog.Logger

	mu     sync.RWMutex
	cached Status
	// Raw verified claims kept around so handlers that need details
	// beyond what Status exposes (e.g. the cap-enforcement path
	// counting active seats) can read them without re-parsing.
	rawClaims *LicenseClaims

	// onReload is fired after every successful Status swap (boot
	// load, manual upload, 24h re-verify). Used by http/server.go to
	// rebuild the auth.Registry so enterprise providers (LDAP, SAML)
	// register / de-register based on the new feature list without
	// requiring a process restart. Multiple callbacks supported; they
	// run in registration order, synchronously, while holding only
	// the read lock. Callbacks MUST be fast and side-effect-only —
	// no DB calls, no HTTP work.
	onReload []func(Status)
}

// NewState reads the .lic at `path`, verifies it, and returns a
// ready-to-use State. When `path` is empty or the file doesn't
// exist, State falls into community mode — that's NOT an error.
// Verification failures (bad signature, expired, etc.) are surfaced
// via Status.LastError so the admin UI can show them.
//
// orgKeyPath points at the customer-side org.key seed file used for
// Layer-1 cross-binding when the loaded license declares an
// `org_pubkey` claim. Empty disables cross-binding entirely — bound
// licenses installed on a state with no orgKeyPath will fail closed
// with ErrOrgKeyMissing.
func NewState(path, orgKeyPath string, logger *slog.Logger) *State {
	s := &State{path: path, orgKeyPath: orgKeyPath, logger: logger}
	s.loadInitial()
	return s
}

// OrgKeyPath returns the configured customer-side org.key path.
// Surfaced so handlers can echo it back to admins ("we looked at X")
// when a cross-binding fails.
func (s *State) OrgKeyPath() string { return s.orgKeyPath }

// OnReload registers a callback that fires after every successful
// Status swap — boot load, /admin/license/upload, 24h re-verify
// ticker. Used by the boot wiring in http/server.go to rebuild
// dependent state (identity-provider registry, multi-tenant manager,
// etc.) when the install's feature list changes without requiring a
// process restart.
//
// Callbacks run synchronously inside swap() under the State's write
// lock — keep them FAST + non-blocking. The typical body is "ask
// LicenseSource what features are present, rebuild some structure,
// hand it back to a setter". DB / HTTP / disk I/O does not belong
// here.
//
// The callback receives the newly-cached Status. Use s.HasFeature
// against the State if you need the canonical answer; the supplied
// Status is a snapshot copy and stays valid after the lock releases.
func (s *State) OnReload(fn func(Status)) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	s.onReload = append(s.onReload, fn)
	s.mu.Unlock()
}

// StartReverify launches a background ticker that re-runs the load +
// verify pipeline on `interval` and atomically swaps the cached
// Status when the outcome changes. Returns immediately; the goroutine
// is bound to `ctx` and exits when ctx is cancelled (the http.Server
// shutdown context).
//
// Why this exists, even though the cap-feature bridge cache is hot
// and never expires:
//
//   - **Defense-in-depth against runtime patches**: a patcher who
//     freezes the cache (e.g. by stubbing capLicenseAllows to return
//     true) has to also kill this goroutine, or the next tick will
//     re-read the .lic from disk and overwrite the cached Status.
//     The two checks live in different packages, different idioms,
//     and the goroutine is one of several boot-registered tasks —
//     finding + neutralising it is meaningfully more work than a
//     constant-fold.
//
//   - **Catches mid-runtime expiry**: a license that's valid at boot
//     but expires while the process is up surfaces in the admin UI
//     within `interval` rather than at the next restart.
//
//   - **Picks up org.key swaps**: if an admin rotates the customer-
//     activation key + uploads a re-issued .lic, both files come
//     into effect without a restart.
//
// interval ≤ 0 disables the ticker entirely (returns without
// launching). Production wiring uses 24 hours; tests use much
// shorter intervals via the same entry point.
func (s *State) StartReverify(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	if s.path == "" {
		// Community mode: nothing to re-verify. Don't burn a goroutine.
		return
	}
	go s.reverifyLoop(ctx, interval)
}

func (s *State) reverifyLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if s.logger != nil {
				s.logger.Info("licensing: re-verify loop stopping",
					slog.String("path", s.path))
			}
			return
		case <-t.C:
			_, err := s.loadAndSwap()
			if err != nil && s.logger != nil {
				// Don't promote to ERROR — the cached Status now carries
				// the new LastError, and the admin UI will render it.
				s.logger.Warn("licensing: re-verify failed",
					slog.String("path", s.path),
					slog.String("err", err.Error()),
				)
			}
		}
	}
}

// Tier implements Source.
func (s *State) Tier() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cached.Tier
}

// HasFeature implements Source. Always answers from the cached
// snapshot — never re-parses or hits disk.
func (s *State) HasFeature(feature string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return hasFeatureIn(feature, s.cached.Features)
}

// Features implements Source. Returns a copy so callers can mutate
// without affecting cached state.
func (s *State) Features() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.cached.Features))
	copy(out, s.cached.Features)
	return out
}

// Status implements Source.
func (s *State) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return a copy so the receiver can't mutate cached state.
	c := s.cached
	if c.Features != nil {
		c.Features = append([]string(nil), c.Features...)
	}
	return c
}

// Claims returns the verified raw claims for callers that need
// detail beyond Status (e.g. asset-counter computing % of cap).
// Returns nil when no license is loaded (community mode).
func (s *State) Claims() *LicenseClaims {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.rawClaims == nil {
		return nil
	}
	c := *s.rawClaims
	return &c
}

// Path returns the configured file path; empty when no license
// path was configured.
func (s *State) Path() string {
	return s.path
}

// Reload re-reads the .lic file, re-verifies it, and atomically
// swaps the cached Status. Returns the new Status + any error.
// Idempotent — calling on community mode is a no-op.
func (s *State) Reload() (Status, error) {
	if s.path == "" {
		// Stay community.
		return s.Status(), nil
	}
	return s.loadAndSwap()
}

// SaveAndReload writes the supplied license text to the configured
// path, atomically swaps the cached Status, and returns the new
// status. The text is verified BEFORE being persisted — an invalid
// envelope never overwrites a valid file on disk. This is what the
// POST /admin/license/upload handler calls after the admin pastes a
// new .lic body.
//
// Errors:
//   - ErrStateNil — no LicensePath configured (state was constructed
//     with path == ""). Tests can hit this; production wires a path.
//   - verifier errors (ErrBadEnvelope, ErrBadSignature, ErrExpired,
//     ErrNotYetValid, etc.) — wrapped via Verify and returned as-is.
//   - os.WriteFile errors — surfaced unchanged.
//
// Permissions: writes the file with 0600. The .lic carries an Ed25519
// signature so leakage isn't a forgery risk, but the owner/org/seats
// inside it are licensee-private; 0600 keeps it out of casual reach
// on a shared host.
func (s *State) SaveAndReload(text string) (Status, error) {
	if s.path == "" {
		return s.Status(), ErrStateNil
	}
	// Verify FIRST. A bad upload must not clobber a working file.
	claims, err := Verify(text)
	if err != nil {
		return s.Status(), err
	}
	// Also reject envelopes that pass signature but can't activate
	// against the current org.key — saves the admin a useless restart
	// loop ("upload, see it broken, upload again"). Same shape as the
	// signature check above: a license that won't activate must never
	// overwrite a working file on disk.
	if claims.OrgPubkey != nil && *claims.OrgPubkey != "" {
		if err := VerifyOrgCrossBinding(*claims.OrgPubkey, s.orgKeyPath); err != nil {
			return s.Status(), err
		}
	}
	if err := os.WriteFile(s.path, []byte(text), 0o600); err != nil {
		return s.Status(), err
	}
	return s.loadAndSwap()
}

// --- internals -------------------------------------------------------------

func (s *State) loadInitial() {
	if s.path == "" {
		s.swap(communityStatus(), nil)
		if s.logger != nil {
			s.logger.Info("licensing: no license path configured; running community mode")
		}
		return
	}
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		s.swap(communityStatus(), nil)
		if s.logger != nil {
			s.logger.Info("licensing: no license file; running community mode",
				slog.String("path", s.path))
		}
		return
	}
	if _, err := s.loadAndSwap(); err != nil && s.logger != nil {
		// Non-fatal — Status.LastError carries the reason; community
		// fallback is what runs.
		s.logger.Warn("licensing: initial load failed; running community mode",
			slog.String("path", s.path),
			slog.String("err", err.Error()),
		)
	}
}

func (s *State) loadAndSwap() (Status, error) {
	text, err := os.ReadFile(s.path)
	if err != nil {
		st := communityStatus()
		st.Path = s.path
		st.OrgKeyPath = s.orgKeyPath
		st.LastError = "read: " + err.Error()
		s.swap(st, nil)
		return st, err
	}
	claims, err := Verify(string(text))
	if err != nil {
		st := communityStatus()
		st.Path = s.path
		st.OrgKeyPath = s.orgKeyPath
		st.LastError = "verify: " + err.Error()
		s.swap(st, nil)
		return st, err
	}
	// Layer-1 cross-binding. A license that declares org_pubkey only
	// activates when org.key on disk derives to that same public key.
	// On failure we fall to community mode + surface LastError, same
	// shape as the signature/expiry failure path so the admin UI
	// renders consistently.
	if claims.OrgPubkey != nil && *claims.OrgPubkey != "" {
		if err := VerifyOrgCrossBinding(*claims.OrgPubkey, s.orgKeyPath); err != nil {
			st := communityStatus()
			st.Path = s.path
			st.OrgKeyPath = s.orgKeyPath
			st.OrgBindingRequired = true
			st.OrgBound = false
			st.OrgBindingError = err.Error()
			st.LastError = "org cross-binding: " + err.Error()
			s.swap(st, nil)
			return st, err
		}
	}
	st := statusFromClaims(claims, s.path, s.orgKeyPath)
	s.swap(st, &claims)
	return st, nil
}

func (s *State) swap(st Status, raw *LicenseClaims) {
	s.mu.Lock()
	s.cached = st
	s.rawClaims = raw
	// Snapshot the callback slice so we can release the lock before
	// firing — callbacks may want to call back into State methods
	// (HasFeature, Status) which would deadlock against a write lock.
	cbs := make([]func(Status), len(s.onReload))
	copy(cbs, s.onReload)
	s.mu.Unlock()
	for _, fn := range cbs {
		fn(st)
	}
}

// statusFromClaims maps verified claims onto the wire-level Status
// the admin UI consumes. By the time this is called the verifier has
// accepted the envelope AND (when applicable) the cross-binding has
// passed — so OrgBound is unconditionally true here.
func statusFromClaims(c LicenseClaims, path, orgKeyPath string) Status {
	required := c.OrgPubkey != nil && *c.OrgPubkey != ""
	return Status{
		Loaded:             true,
		Tier:               c.Tier,
		Features:           append([]string(nil), c.Features...),
		Owner:              c.Owner,
		Org:                c.Org,
		LID:                c.LID,
		Seats:              copyInt64Ptr(c.Seats),
		SeatWindowDays:     c.SeatWindowDays,
		AssetCap:           copyInt64Ptr(c.AssetCap),
		NotBefore:          epochToISO(c.NotBefore),
		Expires:            epochToISO(c.Expires),
		IssuedAt:           epochToISO(c.IssuedAt),
		DaysUntilExpiry:    daysUntil(c.Expires),
		Issuer:             c.Issuer,
		Path:               path,
		OrgBindingRequired: required,
		OrgBound:           true,
		OrgKeyPath:         orgKeyPath,
	}
}

func copyInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// ensure interface satisfaction at compile time.
var _ Source = (*State)(nil)

// SourceFunc is a convenience adapter for tests + edge cases where a
// caller wants to plug a custom Status snapshot in without the file
// loader. Wraps a single function returning a Status.
type SourceFunc func() Status

// Tier implements Source.
func (f SourceFunc) Tier() string { return f().Tier }

// HasFeature implements Source.
func (f SourceFunc) HasFeature(feature string) bool {
	return hasFeatureIn(feature, f().Features)
}

// Features implements Source.
func (f SourceFunc) Features() []string {
	st := f()
	out := make([]string, len(st.Features))
	copy(out, st.Features)
	return out
}

// Status implements Source.
func (f SourceFunc) Status() Status { return f() }

// ErrStateNil is returned by helpers that need a non-nil Source but
// were handed nil. Treat as a programming error — wire Source at
// construction.
var ErrStateNil = fmt.Errorf("licensing: source is nil")
