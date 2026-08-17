// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1195 — public mode on the public boot payload.
//
// The collection edit modal offers a "Public" visibility tier only on an
// install where anonymous visitors can actually read it. The switch that
// decides that lives behind `system.config.read`, and the person asking
// is a curator with no admin capability at all — so the answer rides
// `GET /appearance`, the unauthenticated fetch every client already
// makes at boot.
//
// What this pins is that the flag TRACKS the switch, in both directions,
// driven through the real write endpoint rather than by poking the
// store. A flag that reports one fixed value is worse than no flag: it
// would hide the option on every public install, or offer it on every
// private one, and either way the frontend would have no way to tell.

package sysconfig_test

import (
	"context"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

func TestPublicAppearancePublishesPublicMode(t *testing.T) {
	withLogoHandler(t, func(ctx context.Context, h *sysconfig.Handler, _ *storage.Service) {
		setMode := func(on bool) {
			t.Helper()
			got, err := h.UpdatePublicMode(ctx, openapi.UpdatePublicModeRequestObject{
				Body: &openapi.UpdatePublicModeJSONRequestBody{Enabled: on},
			})
			if err != nil {
				t.Fatalf("UpdatePublicMode(%v): %v", on, err)
			}
			ok, is := got.(openapi.UpdatePublicMode200JSONResponse)
			if !is {
				t.Fatalf("UpdatePublicMode(%v) returned %T, want 200", on, got)
			}
			if ok.Enabled != on {
				t.Fatalf("UpdatePublicMode(%v) persisted %v — the boot payload below "+
					"would be reporting the wrong thing correctly", on, ok.Enabled)
			}
		}

		// Restore whatever this database had, so a suite ordering change
		// cannot leave public mode flipped for another test.
		restore, err := h.GetPublicMode(ctx, openapi.GetPublicModeRequestObject{})
		if err != nil {
			t.Fatalf("GetPublicMode: %v", err)
		}
		if was, is := restore.(openapi.GetPublicMode200JSONResponse); is {
			t.Cleanup(func() { setMode(was.Enabled) })
		}

		setMode(false)
		cfg := publicAppearance(t, ctx, h)
		if cfg.PublicModeEnabled == nil {
			t.Fatalf("public_mode_enabled absent from the public boot payload; the " +
				"collection modal has nothing to gate the Public tier on (#1195)")
		}
		if *cfg.PublicModeEnabled {
			t.Errorf("public_mode_enabled = true with public mode OFF — the modal would " +
				"offer a tier no anonymous visitor could ever read")
		}

		setMode(true)
		cfg = publicAppearance(t, ctx, h)
		if cfg.PublicModeEnabled == nil || !*cfg.PublicModeEnabled {
			t.Errorf("public_mode_enabled = %v with public mode ON, want true — the "+
				"Public tier would be unreachable on an install that supports it",
				cfg.PublicModeEnabled)
		}
	})
}
