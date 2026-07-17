// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #374 — PATCH /admin/system/site must MERGE against the stored config,
// not replace it. The bug this pins: SiteConfig is one schema serving
// GET-response, PATCH-body, and PATCH-response, so an omitted `name`
// deserializes to "" and a body of {base_url} used to overwrite the
// name with "" — "update my base_url" silently un-named the site.
//
// This is the invariant a comment can't hold: a partial update
// preserves the fields it omits.

package sysconfig_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

func TestUpdateSiteConfig_PartialUpdate_PreservesOmittedFields(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		if err := h.Store.SetSite(ctx, sysconfig.Site{
			Name:    "Artist Alley",
			BaseURL: "https://tx.example.com",
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		// PATCH only base_url. Name is omitted → deserializes to "" →
		// must NOT clobber the stored name. This is the exact shape that
		// was blanking the site name in production.
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.SiteConfig{BaseUrl: strPtr("https://new.example.com")}
			if _, err := h.UpdateSiteConfig(ctx, openapi.UpdateSiteConfigRequestObject{Body: &body}); err != nil {
				t.Fatalf("UpdateSiteConfig (base_url only): %v", err)
			}
		})
		got, err := h.Store.GetSite(ctx)
		if err != nil {
			t.Fatalf("GetSite: %v", err)
		}
		if got.Name != "Artist Alley" {
			t.Errorf("base_url-only PATCH blanked the name: got %q, want %q", got.Name, "Artist Alley")
		}
		if got.BaseURL != "https://new.example.com" {
			t.Errorf("base_url not applied: got %q", got.BaseURL)
		}

		// PATCH only name. base_url is a pointer, omitted → nil → keep.
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.SiteConfig{Name: "Renamed"}
			if _, err := h.UpdateSiteConfig(ctx, openapi.UpdateSiteConfigRequestObject{Body: &body}); err != nil {
				t.Fatalf("UpdateSiteConfig (name only): %v", err)
			}
		})
		got, err = h.Store.GetSite(ctx)
		if err != nil {
			t.Fatalf("GetSite: %v", err)
		}
		if got.Name != "Renamed" {
			t.Errorf("name not applied: got %q", got.Name)
		}
		if got.BaseURL != "https://new.example.com" {
			t.Errorf("name-only PATCH cleared base_url: got %q, want it preserved", got.BaseURL)
		}
	})
}

// A non-empty name in the body still updates, and a present-but-empty
// base_url pointer still clears — merge must not become "ignore
// everything".
func TestUpdateSiteConfig_ExplicitValuesStillApply(t *testing.T) {
	withAuditStore(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		if err := h.Store.SetSite(ctx, sysconfig.Site{
			Name:    "Before",
			BaseURL: "https://before.example.com",
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			// New name + an explicit empty base_url (pointer present) =
			// deliberately clear the URL.
			body := openapi.SiteConfig{Name: "After", BaseUrl: strPtr("")}
			if _, err := h.UpdateSiteConfig(ctx, openapi.UpdateSiteConfigRequestObject{Body: &body}); err != nil {
				t.Fatalf("UpdateSiteConfig: %v", err)
			}
		})
		got, err := h.Store.GetSite(ctx)
		if err != nil {
			t.Fatalf("GetSite: %v", err)
		}
		if got.Name != "After" {
			t.Errorf("name not updated: got %q, want %q", got.Name, "After")
		}
		if got.BaseURL != "" {
			t.Errorf("explicit empty base_url should clear it: got %q", got.BaseURL)
		}
	})
}
