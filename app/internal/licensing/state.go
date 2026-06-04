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
	"fmt"
	"log/slog"
	"os"
	"sync"
)

// State holds the cached snapshot. Implements Source.
type State struct {
	path   string
	logger *slog.Logger

	mu     sync.RWMutex
	cached Status
	// Raw verified claims kept around so handlers that need details
	// beyond what Status exposes (e.g. the cap-enforcement path
	// counting active seats) can read them without re-parsing.
	rawClaims *LicenseClaims
}

// NewState reads the .lic at `path`, verifies it, and returns a
// ready-to-use State. When `path` is empty or the file doesn't
// exist, State falls into community mode — that's NOT an error.
// Verification failures (bad signature, expired, etc.) are surfaced
// via Status.LastError so the admin UI can show them.
func NewState(path string, logger *slog.Logger) *State {
	s := &State{path: path, logger: logger}
	s.loadInitial()
	return s
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
		st.LastError = "read: " + err.Error()
		s.swap(st, nil)
		return st, err
	}
	claims, err := Verify(string(text))
	if err != nil {
		st := communityStatus()
		st.Path = s.path
		st.LastError = "verify: " + err.Error()
		s.swap(st, nil)
		return st, err
	}
	st := statusFromClaims(claims, s.path)
	s.swap(st, &claims)
	return st, nil
}

func (s *State) swap(st Status, raw *LicenseClaims) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached = st
	s.rawClaims = raw
}

// statusFromClaims maps verified claims onto the wire-level Status
// the admin UI consumes.
func statusFromClaims(c LicenseClaims, path string) Status {
	return Status{
		Loaded:          true,
		Tier:            c.Tier,
		Features:        append([]string(nil), c.Features...),
		Owner:           c.Owner,
		Org:             c.Org,
		LID:             c.LID,
		Seats:           copyInt64Ptr(c.Seats),
		SeatWindowDays:  c.SeatWindowDays,
		AssetCap:        copyInt64Ptr(c.AssetCap),
		NotBefore:       epochToISO(c.NotBefore),
		Expires:         epochToISO(c.Expires),
		IssuedAt:        epochToISO(c.IssuedAt),
		DaysUntilExpiry: daysUntil(c.Expires),
		Issuer:          c.Issuer,
		Path:            path,
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
