// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1163 — the reverse-image capability on the public boot payload.
//
// What this pins is the SHAPE the frontend depends on: the flag is
// present on `GET /appearance` and states the handler's resolved
// answer, in both directions. It is deliberately not a test of what
// sets that answer — the boot path decides that from whether the CLIP
// provider registered (api.go), which is a wiring fact, not a handler
// one. Publishing the config value instead would have been the bug:
// `search.visual.enabled` can be true on an install whose sidecar never
// came up, and there POST /search/by-image still answers 501.

package sysconfig_test

import (
	"context"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

func publicAppearance(t *testing.T, ctx context.Context, h *sysconfig.Handler) openapi.AppearanceConfig {
	t.Helper()
	got, err := h.GetPublicAppearance(ctx, openapi.GetPublicAppearanceRequestObject{})
	if err != nil {
		t.Fatalf("GetPublicAppearance: %v", err)
	}
	ok, is := got.(openapi.GetPublicAppearance200JSONResponse)
	if !is {
		t.Fatalf("GetPublicAppearance returned %T, want 200", got)
	}
	return openapi.AppearanceConfig(ok)
}

func TestPublicAppearancePublishesVisualSearchCapability(t *testing.T) {
	withLogoHandler(t, func(ctx context.Context, h *sysconfig.Handler, _ *storage.Service) {
		// Default install: no CLIP channel, and the client must be able
		// to tell without uploading an image to find out.
		cfg := publicAppearance(t, ctx, h)
		if cfg.VisualSearchEnabled == nil {
			t.Fatalf("visual_search_enabled absent from the public boot payload; " +
				"the frontend has nothing to gate the reverse-image arm on (#1163)")
		}
		if *cfg.VisualSearchEnabled {
			t.Errorf("visual_search_enabled = true on a handler with no provider wired, want false")
		}

		// An install whose provider registered at boot.
		h.VisualSearchEnabled = true
		cfg = publicAppearance(t, ctx, h)
		if cfg.VisualSearchEnabled == nil || !*cfg.VisualSearchEnabled {
			t.Errorf("visual_search_enabled = %v with the capability resolved true, want true",
				cfg.VisualSearchEnabled)
		}
	})
}
