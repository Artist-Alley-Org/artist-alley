// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"fmt"
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
	return s.setKey(ctx, KeyAppearance, v)
}
