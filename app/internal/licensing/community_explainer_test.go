// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package licensing

import (
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/ldapauth"
	"github.com/mscrnt/artist-alley/app/internal/samlauth"
	"github.com/mscrnt/artist-alley/app/internal/sitetext"
	"github.com/mscrnt/artist-alley/app/internal/tenancy"
)

// communityExplainerKey is the sentence the admin License page shows
// whenever no license is loaded (web/src/routes/admin/system/license,
// `{#if !status.loaded}`), which is every install that never added a
// .lic. FR and ES carry no license block, so they fall back to this
// English string too.
const communityExplainerKey = "admin.system.license.community_explainer"

// TestCommunityExplainer_MatchesCommunityMode holds that sentence to what
// community mode actually does, read from the same State the status
// endpoint serves. It used to promise a 15-seat / 20,000-asset cap and
// "the full feature set" while the community status set no cap and
// lacked every enterprise gate, and it offered a paid .lic that is not
// sold (ADR 0017 status note, 2026-10-07).
//
// The checks follow the facts rather than the wording: if community
// mode ever gains a cap, or the enterprise gates join the community
// feature set, the expected sentence changes with it.
func TestCommunityExplainer_MatchesCommunityMode(t *testing.T) {
	text, ok := sitetext.ShippedValue(communityExplainerKey)
	if !ok {
		t.Fatalf("shipped catalogue has no %s", communityExplainerKey)
	}
	lower := strings.ToLower(text)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := NewState(filepath.Join(t.TempDir(), "absent.lic"), "", logger).Status()
	if st.Loaded {
		t.Fatal("no license file should mean community mode")
	}

	// Caps: a cap the explainer names must be the cap community mode
	// applies, and an absent cap must not be named at all.
	caps := []struct {
		noun string
		val  *int64
	}{
		{"seat", st.Seats},
		{"asset", st.AssetCap},
	}
	for _, c := range caps {
		claim := regexp.MustCompile(`\d[\d,.]*\s+(?:[a-z]+\s+)?` + c.noun + `s?\b`)
		if c.val == nil {
			if m := claim.FindString(lower); m != "" {
				t.Errorf("community mode applies no %s cap, but the explainer claims %q", c.noun, m)
			}
			continue
		}
		if !strings.Contains(text, strconv.FormatInt(*c.val, 10)) {
			t.Errorf("community mode caps %ss at %d, but the explainer does not say so", c.noun, *c.val)
		}
	}
	if st.Seats == nil && st.AssetCap == nil && !strings.Contains(lower, "no seat or asset cap") {
		t.Errorf("community mode applies no seat or asset cap; the explainer must say so: %q", text)
	}

	// Features: the page lists these three enterprise gates under the
	// explainer. Any of them missing from community mode makes "the full
	// feature set" false and needs the explainer to say they stay off.
	var gated []string
	for _, f := range []string{ldapauth.LicenseFeature, samlauth.LicenseFeature, tenancy.LicenseFeature} {
		if !hasFeatureIn(f, st.Features) {
			gated = append(gated, f)
		}
	}
	if len(gated) > 0 {
		for _, phrase := range []string{"full feature set", "every feature", "all features"} {
			if strings.Contains(lower, phrase) {
				t.Errorf("community mode lacks %v, but the explainer claims %q", gated, phrase)
			}
		}
		if !strings.Contains(lower, "unavailable without a license") {
			t.Errorf("community mode lacks %v; the explainer must say license-gated features stay unavailable: %q", gated, text)
		}
	}

	// Offering: no license tier is sold today, so the explainer must not
	// point the operator at buying or upgrading to one.
	offer := regexp.MustCompile(`\b(paid|purchase|buy|upgrade)\b`)
	if m := offer.FindString(lower); m != "" {
		t.Errorf("no license is offered today, but the explainer says %q", m)
	}
}
