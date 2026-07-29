// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"fmt"

	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// KeyAppearance — system_config key for installation-wide brand and
// typography choices. The actual font assets ship with the frontend
// bundle; this key just records which IDs the admin picked.
const KeyAppearance = "appearance"

// FontSlot enumerates the four typography slots a brand can be
// re-skinned through. Each slot resolves to one font ID from the
// curated catalogue (see web/src/lib/fonts/catalogue.ts).
//
//	brand   — logo / hero only           (default: Limelight)
//	display — H1–H3 headings              (default: Inter Display, falls back to sans)
//	sans    — body / UI                   (default: Inter, system stack as last resort)
//	mono    — code / tabular numerics     (default: JetBrains Mono, system mono)
//
// Splitting brand from display means an admin can run a stylized
// wordmark (Limelight, Bebas Neue) without subjecting body headings
// to the same display face.
type FontSlot string

const (
	FontSlotBrand   FontSlot = "brand"
	FontSlotDisplay FontSlot = "display"
	FontSlotSans    FontSlot = "sans"
	FontSlotMono    FontSlot = "mono"
)

// AppearanceConfig is the full per-install appearance payload stored
// under KeyAppearance.
//
// The font fields hold font catalogue IDs (e.g. "limelight",
// "inter-variable", "system-sans"), not raw CSS family names. The
// frontend resolves each ID to its @font-face declarations at boot.
//
// Empty string in any field means "use the slot's default". Letting
// admins write through-empty preserves the contract that the in-tree
// defaults are always the safest pick.
type AppearanceConfig struct {
	BrandFont   string `json:"brand_font"`
	DisplayFont string `json:"display_font"`
	BodyFont    string `json:"body_font"`
	MonoFont    string `json:"mono_font"`

	// ActiveLogo is the hash of the selected entry in LogoHistory, or
	// "" when the install shows the shipped default mark
	// (web/static/logo.svg). Empty means "use the default", the same
	// contract the font slots express with an empty string — and the
	// shipped default is in-tree, so it is always available no matter
	// what has happened to storage.
	//
	// The active logo is stored as a POINTER INTO LogoHistory rather
	// than as its own copy of the metadata, so the two can never drift
	// into disagreeing about the same hash.
	ActiveLogo string `json:"active_logo,omitempty"`

	// LogoHistory is the operator's recent logos, most-recently-used
	// first, capped at [MaxLogoHistory]. It exists so an operator can
	// jump back to a previous mark — the point is recovery ("in case
	// an image was lost"), which is also why every listed entry is
	// pinned in storage: see [MaxLogoHistory].
	LogoHistory []LogoConfig `json:"logo_history,omitempty"`
}

// MaxLogoHistory caps the recent-logo list.
//
// Every hash in LogoHistory is pinned in the storage layer under
// `appearance:logo`, and that pinning is load-bearing, not incidental:
// the storage layer marks an object GC-eligible as soon as its LAST
// pin is dropped, so content-addressing on its own would NOT keep a
// superseded logo alive. Without a pin per listed entry, the history
// would rot into exactly the broken-thumbnail state it exists to
// prevent.
//
// The cap is therefore also a storage-retention policy: at most five
// logo blobs are held alive per install. Eviction from this list drops
// the pin, which hands the bytes to the normal GC lifecycle (with its
// grace window) rather than deleting them out from under anything.
const MaxLogoHistory = 5

// ActiveLogoEntry returns the metadata for the selected logo, or nil
// when the install is on the shipped default.
//
// Returns nil rather than an error when ActiveLogo names a hash that
// is not in the history: chrome renders on every page, so a config
// that has somehow lost its referent falls back to the shipped mark
// instead of failing the request.
func (c AppearanceConfig) ActiveLogoEntry() *LogoConfig {
	if c.ActiveLogo == "" {
		return nil
	}
	return c.FindLogo(c.ActiveLogo)
}

// FindLogo returns the history entry with the given hash, or nil.
//
// This is the membership check that makes hash-addressed logo reads
// safe: callers resolve a caller-supplied hash THROUGH this, so the
// only objects any logo endpoint can ever serve are ones this package
// validated and put in the list itself.
func (c AppearanceConfig) FindLogo(hash string) *LogoConfig {
	for i := range c.LogoHistory {
		if c.LogoHistory[i].Hash == hash {
			return &c.LogoHistory[i]
		}
	}
	return nil
}

// promoteLogo returns history with entry moved (or inserted) at the
// front, capped at MaxLogoHistory, plus whatever fell off the end.
//
// MRU, not an append log: re-selecting something already listed moves
// it rather than duplicating it, so the list stays a set of five
// distinct logos and the operator's most recent choices are the ones
// that survive.
func promoteLogo(history []LogoConfig, entry LogoConfig) (next, evicted []LogoConfig) {
	next = append(next, entry)
	for _, h := range history {
		if h.Hash == entry.Hash {
			continue // the moved entry, already at the front
		}
		if len(next) < MaxLogoHistory {
			next = append(next, h)
			continue
		}
		evicted = append(evicted, h)
	}
	return next, evicted
}

// LogoConfig is a REFERENCE to an uploaded logo, never the image
// itself. The bytes live in the content-addressed storage layer under
// the pin `appearance:logo`; this records which object they are and
// what we proved about them at upload time.
//
// Keeping only a reference here is what lets the existing
// system_config row stay small and cheap to read — it is fetched on
// the public boot path of every cold page load, and the storage layer
// already solves dedup, GC and multi-backend placement for blobs.
//
// ContentType is derived from decoding the bytes, NOT from anything
// the uploading client claimed. The public serve path echoes it into
// the Content-Type header, so it is a security-relevant field: see
// [ValidateLogo].
type LogoConfig struct {
	// Hash is the lowercase hex sha256 the storage layer computed —
	// the object key, and the cache-busting version token.
	Hash string `json:"hash"`
	// ContentType is the decoded, allowlisted MIME type.
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	SizeBytes   int64  `json:"size_bytes"`
}

// GetAppearance returns the appearance config or, if unset, an empty
// AppearanceConfig (all slots blank — frontend applies defaults).
func (s *Store) GetAppearance(ctx context.Context) (AppearanceConfig, error) {
	var out AppearanceConfig
	if err := s.getKey(ctx, KeyAppearance, &out); err != nil {
		return AppearanceConfig{}, err
	}
	return out, nil
}

// SetAppearance writes the appearance config. Validation here is
// minimal — we accept any non-empty string as a font ID because the
// catalogue lives in the frontend and may grow without a backend
// redeploy. An unrecognized ID just falls back to the slot default
// at render time, which is the safest failure mode.
func (s *Store) SetAppearance(ctx context.Context, v AppearanceConfig) error {
	// Soft length cap so a malicious caller can't stuff the system_config
	// row with multi-MB strings. Real font IDs are short slugs.
	for slot, val := range map[FontSlot]string{
		FontSlotBrand:   v.BrandFont,
		FontSlotDisplay: v.DisplayFont,
		FontSlotSans:    v.BodyFont,
		FontSlotMono:    v.MonoFont,
	} {
		if len(val) > 64 {
			return fmt.Errorf("sysconfig: appearance.%s_font too long (%d > 64)", slot, len(val))
		}
	}
	if len(v.LogoHistory) > MaxLogoHistory {
		return fmt.Errorf("sysconfig: appearance.logo_history holds %d entries (max %d)",
			len(v.LogoHistory), MaxLogoHistory)
	}
	seen := make(map[string]struct{}, len(v.LogoHistory))
	for i, l := range v.LogoHistory {
		// The hash is interpolated into a storage path and echoed as a
		// cache key, and the content type is echoed into a response
		// header. Neither may reach either place unchecked, even
		// though today's only writer already validated them — this is
		// the last gate before persistence, so it re-checks.
		if err := storage.ValidateHash(l.Hash); err != nil {
			return fmt.Errorf("sysconfig: appearance.logo_history[%d]: %w", i, err)
		}
		if _, ok := allowedLogoMIME[l.ContentType]; !ok {
			return fmt.Errorf("sysconfig: appearance.logo_history[%d]: content type %q is not allowed",
				i, l.ContentType)
		}
		if _, dup := seen[l.Hash]; dup {
			return fmt.Errorf("sysconfig: appearance.logo_history has duplicate hash %s", l.Hash)
		}
		seen[l.Hash] = struct{}{}
	}
	if v.ActiveLogo != "" {
		// The active logo must be a member of the history. This is the
		// invariant that keeps the pin set and the reachable set equal:
		// everything servable is listed, and everything listed is
		// pinned.
		if _, ok := seen[v.ActiveLogo]; !ok {
			return fmt.Errorf("sysconfig: appearance.active_logo %s is not in logo_history", v.ActiveLogo)
		}
	}
	return s.setKey(ctx, KeyAppearance, v)
}

// AddLogo records a newly uploaded logo, makes it the active one, and
// returns the entries evicted from the tail of the history.
//
// The caller MUST drop the storage pin for every evicted entry — that
// is what hands their bytes back to the GC lifecycle. Returning them
// rather than unpinning here keeps this package's Store free of a
// storage dependency it otherwise doesn't need, and lets the caller
// order the unpin AFTER the config write succeeds.
//
// Read-modify-write rather than a partial update, because the whole
// config is one JSON document in system_config — there is no field to
// address on its own. The window between the read and the write is the
// same one SetAppearance already has; the loser of a race between two
// admins saving appearance at once is existing behaviour, not
// something this introduces.
func (s *Store) AddLogo(ctx context.Context, entry LogoConfig) ([]LogoConfig, error) {
	cur, err := s.GetAppearance(ctx)
	if err != nil {
		return nil, err
	}
	next, evicted := promoteLogo(cur.LogoHistory, entry)
	cur.LogoHistory = next
	cur.ActiveLogo = entry.Hash
	if err := s.SetAppearance(ctx, cur); err != nil {
		return nil, err
	}
	return evicted, nil
}

// SelectLogo activates an entry that is ALREADY in the history and
// moves it to the front. Pass "" to go back to the shipped default.
//
// It refuses a hash that is not already listed. That refusal is the
// security boundary for this endpoint: without it, "select a logo by
// hash" would let an admin aim the public, unauthenticated logo route
// at any object on the install, including another user's private
// asset. Membership in the history is proof that this package decoded
// and approved those exact bytes itself.
//
// No pin changes: selecting reorders the list but does not change who
// is on it, and the shipped default needs no pin because it ships in
// the frontend bundle.
func (s *Store) SelectLogo(ctx context.Context, hash string) error {
	cur, err := s.GetAppearance(ctx)
	if err != nil {
		return err
	}
	if hash == "" {
		cur.ActiveLogo = ""
		return s.SetAppearance(ctx, cur)
	}
	entry := cur.FindLogo(hash)
	if entry == nil {
		return fmt.Errorf("sysconfig: logo %s is not in the recent list", hash)
	}
	next, evicted := promoteLogo(cur.LogoHistory, *entry)
	if len(evicted) > 0 {
		// Unreachable: promoting a member cannot grow the list past
		// the cap. Guarded anyway because silently dropping a pinned
		// entry here would leak the blob.
		return fmt.Errorf("sysconfig: selecting %s unexpectedly evicted %d entries", hash, len(evicted))
	}
	cur.LogoHistory = next
	cur.ActiveLogo = entry.Hash
	return s.SetAppearance(ctx, cur)
}
