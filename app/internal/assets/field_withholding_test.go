// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #899 — an asset you cannot open must not hand you its metadata.
//
// # Why this asserts on the SERIALIZED response
//
// The frontend already rendered a placeholder for these assets:
// CardThumb branches to CardRestricted off `preview_available`, so a
// component test passed the whole time the API was shipping the title,
// the description, the complete SHA-256, the byte size, the original
// filename and the whole free-form metadata blob. The card is not the
// contract. The JSON is, because the JSON is what a determined caller
// reads with curl.
//
// So every assertion here marshals the payload the handler produced and
// ENUMERATES ITS KEYS. An ALLOW-LIST, not a deny-list: the test asserts
// the key set is a SUBSET of what is permitted, so a field added to
// openapi.Asset later fails this test instead of silently shipping.
// `required` in openapi.yaml can no longer pin the shape — it had to
// shrink for absence to be expressible at all — and this is what
// replaces it, the same trade collections/member_allowlist_test.go
// documents for CollectionResource.
//
// # The cross-surface invariant
//
// TestAssetWithholding_NoWiderThanAPostMember is the one this sprint
// exists to establish. #883 fixed the container surfaces and left the
// direct ones, so for the same asset and the same caller the narrow
// path was the one through a POST. That is backwards, and it is how
// three surfaces drift apart. The test pins the direction: the direct
// payload must never carry a key the post-member placeholder does not.
//
// Skips without AA_DB_PASSWORD, same convention as the sibling suites.

package assets_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	fwOwner    int64 = 8991001
	fwStranger int64 = 8991002
)

// fwAllowList is the COMPLETE set of JSON keys a WITHHELD asset may
// carry: an id the caller already holds, the marker, and the one
// asset-derived value the owner permitted.
//
//	"The placeholder should never leak info. Not even title. Only the
//	 owner's name." — 2026-08-03
var fwAllowList = map[string]bool{
	"id":                 true,
	"restricted":         true,
	"owner_display_name": true,
}

// fwLeakyFields are the keys the pre-#899 payload carried and that a
// withheld one must not. Named individually so a failure says WHICH
// leak came back rather than "the key set grew".
//
// `file_hash` is the sharpest of them: a content identifier confirms
// whether a file you already hold is the same one, and it survives any
// later tightening of the others. `thumbhash` is second — it is a
// blurred picture of the content, not a neutral hint.
var fwLeakyFields = []string{
	"title", "description", "file_hash", "file_size_bytes", "file_extension",
	"metadata", "tags", "tag_details", "thumbhash", "asset_type", "status",
	"processing_status", "created_at", "updated_at", "owner_user_ref",
	"pixel_width", "pixel_height", "state_id",
	"preview_available", "ladder_available", "scrub_available",
}

func fwSeedOwner(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	const display = "Lucas Lefebvre"
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username) VALUES ($1, $2)
		 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
		fwOwner, "fw-owner-899"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_profiles (user_ref, display_name) VALUES ($1, $2)
		 ON CONFLICT (user_ref) DO UPDATE SET display_name = EXCLUDED.display_name`,
		fwOwner, display); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_profiles WHERE user_ref = $1`, fwOwner)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, fwOwner)
	})
	return display
}

// fwSeedAsset plants an asset with EVERY field a leak could travel
// through populated — including a `col` variant so preview_available
// has something to be true about, and a metadata blob with a value no
// stranger should ever read.
func fwSeedAsset(t *testing.T, pool *pgxpool.Pool, title, sensitivity string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	sum := sha256.Sum256(id[:])
	hash := hex.EncodeToString(sum[:])
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 7313, 'fs')
		 ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1, 'col', 1)
		 ON CONFLICT (object_hash, variant_key) DO NOTHING`, hash); err != nil {
		t.Fatalf("seed variant: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status, file_hash, file_extension,
		                    file_size_bytes, thumbhash, metadata)
		VALUES ($1, $2, $3, $4, (SELECT MIN(ref) FROM asset_types), 'active', $5, 'ready',
		        $6, 'ogg', 7313, $7, $8::jsonb)`,
		id, title, "UNRELEASED — -14 LUFS, do not distribute", fwOwner, sensitivity, hash,
		[]byte{0xde, 0xad, 0xbe, 0xef},
		`{"filename":"unreleased-boss-theme.ogg","acquisition_source":"internal","license":"Internal"}`,
	); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO asset_tag (asset_id, tag, source) VALUES ($1, 'unreleased', 'import')
		 ON CONFLICT DO NOTHING`, id); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_tag WHERE asset_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

// fwKeys marshals a value and returns its sorted top-level JSON keys.
// The whole point: read what went on the WIRE, not what the struct
// holds.
func fwKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fwGetAsset(t *testing.T, router chi.Router, id uuid.UUID) (int, json.RawMessage) {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/assets/"+id.String(), nil))
	return rr.Code, rr.Body.Bytes()
}

func fwListAssets(t *testing.T, router chi.Router) map[string]json.RawMessage {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/assets?limit=200", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list assets: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Items []json.RawMessage `json:"items"`
	}
	mustDecode(t, rr.Body.Bytes(), &envelope)
	out := make(map[string]json.RawMessage, len(envelope.Items))
	for _, raw := range envelope.Items {
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("list item: %v", err)
		}
		out[probe.ID] = raw
	}
	return out
}

// fwAssertWithheld is the shared assertion: the payload's key set is a
// SUBSET of the allow-list, the marker is true, and none of the named
// leaks came back.
func fwAssertWithheld(t *testing.T, label string, raw json.RawMessage, wantOwnerName string) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: unmarshal: %v", label, err)
	}
	for k := range m {
		if !fwAllowList[k] {
			t.Errorf("%s: withheld asset carried key %q, which is not on the allow-list "+
				"(permitted: id, restricted, owner_display_name). Payload: %s", label, k, raw)
		}
	}
	for _, leak := range fwLeakyFields {
		if _, present := m[leak]; present {
			t.Errorf("%s: withheld asset still ships %q — this is the #899 leak", label, leak)
		}
	}
	var restricted bool
	if err := json.Unmarshal(m["restricted"], &restricted); err != nil || !restricted {
		t.Errorf("%s: restricted marker is not true (raw=%s)", label, m["restricted"])
	}
	var name string
	if _, ok := m["owner_display_name"]; !ok {
		t.Errorf("%s: owner_display_name absent — the placeholder must say whose work it is, "+
			"or #881 request-access has nothing to address", label)
	} else if err := json.Unmarshal(m["owner_display_name"], &name); err != nil || name != wantOwnerName {
		t.Errorf("%s: owner_display_name = %q, want %q", label, name, wantOwnerName)
	}
}

// TestAssetWithholding_DetailAndList is the core assertion, on both
// direct surfaces, for both caller classes that could reach them.
//
// Every one of these fails on pre-#899 dev, where GET /assets/{id}
// returned 200 with the complete record.
func TestAssetWithholding_DetailAndList(t *testing.T) {
	pool := byTagPool(t)
	display := fwSeedOwner(t, pool)
	restricted := fwSeedAsset(t, pool, "Unreleased boss theme", "restricted")
	public := fwSeedAsset(t, pool, "Public splash art", "public")

	strangerRef := fwStranger
	for _, c := range []struct {
		name string
		id   *auth.Identity
	}{
		// Anonymous cannot reach the ROW at all for a restricted asset
		// (the predicate's anonymous branch demands sensitivity='public'),
		// so this case asserts 404 rather than a placeholder — included
		// so a change that starts LISTING restricted rows anonymously
		// gets caught here too.
		{"anonymous", nil},
		{"authenticated stranger", &auth.Identity{UserRef: strangerRef, AuthMethod: "session", Capabilities: []string{}}},
	} {
		router := simRouter(t, pool, c.id)

		status, body := fwGetAsset(t, router, restricted)
		switch {
		case c.id == nil:
			if status != http.StatusNotFound {
				t.Errorf("%s: GET restricted asset status=%d, want 404 (row plane)", c.name, status)
			}
		default:
			if status != http.StatusOK {
				t.Fatalf("%s: GET restricted asset status=%d, want 200 — a withheld asset is a "+
					"PLACEHOLDER, not a 404, or #881 request-access has nothing to point at", c.name, status)
			}
			fwAssertWithheld(t, c.name+" / GET /assets/{id}", body, display)
		}

		// The public asset must be untouched on the same call — a
		// withhold-everything implementation would pass every assertion
		// above.
		status, body = fwGetAsset(t, router, public)
		if status != http.StatusOK {
			t.Fatalf("%s: GET public asset status=%d, want 200", c.name, status)
		}
		var pub map[string]json.RawMessage
		if err := json.Unmarshal(body, &pub); err != nil {
			t.Fatalf("%s: unmarshal public: %v", c.name, err)
		}
		for _, want := range []string{"title", "file_hash", "file_extension", "tags", "created_at"} {
			if _, ok := pub[want]; !ok {
				t.Errorf("%s: a PUBLIC asset lost %q — the withholding is too wide", c.name, want)
			}
		}

		// And the same rule on the browse list, which does NOT come
		// through enrichAssetDerived and so gets it wrong independently.
		if c.id == nil {
			continue
		}
		items := fwListAssets(t, router)
		row, ok := items[restricted.String()]
		if !ok {
			t.Fatalf("%s: the restricted asset is missing from the browse list — ADR 0064 keeps "+
				"the ROW visible; only its columns are withheld", c.name)
		}
		fwAssertWithheld(t, c.name+" / GET /assets (list)", row, display)
		if _, ok := items[public.String()]; !ok {
			t.Errorf("%s: the public asset fell out of the browse list", c.name)
		}
	}
}

// TestAssetWithholding_NoWiderThanAPostMember is the cross-surface
// invariant. For one asset and one caller, the direct payload's key set
// must be a subset of the key set #883's post-member placeholder
// carries for the same asset (its `asset_id` reads as `id` here; the
// post-join columns `sort_order` and the container's own keys are not
// asset fields and are excluded).
//
// The PostMember placeholder is built here as the REAL literal rather
// than a hardcoded list, so if that placeholder ever widens, this test
// widens with it instead of going stale.
func TestAssetWithholding_NoWiderThanAPostMember(t *testing.T) {
	pool := byTagPool(t)
	display := fwSeedOwner(t, pool)
	restricted := fwSeedAsset(t, pool, "Unreleased boss theme", "restricted")

	strangerRef := fwStranger
	router := simRouter(t, pool,
		&auth.Identity{UserRef: strangerRef, AuthMethod: "session", Capabilities: []string{}})
	status, body := fwGetAsset(t, router, restricted)
	if status != http.StatusOK {
		t.Fatalf("GET restricted asset status=%d, want 200", status)
	}

	// The #883 placeholder, exactly as posts/handler.go writes it.
	name := display
	member := openapi.PostMember{
		AssetId:          openapi_types.UUID(restricted),
		SortOrder:        0,
		Restricted:       true,
		OwnerDisplayName: &name,
	}
	permitted := map[string]bool{}
	for _, k := range fwKeys(t, member) {
		switch k {
		case "asset_id":
			// The member's identity key IS the asset id.
			permitted["id"] = true
		case "sort_order":
			// A container column, not an asset field.
		default:
			permitted[k] = true
		}
	}

	for _, k := range fwKeys(t, json.RawMessage(body)) {
		if !permitted[k] {
			t.Errorf("GET /assets/{id} carries %q, which the post-member placeholder for the SAME "+
				"asset and the SAME caller does not. The direct surface must never be wider than "+
				"the container surface (#883/#899). Permitted: %v", k, permitted)
		}
	}
}

// TestAssetWithholding_FeatureStillWorks is the other half. A
// withhold-everything implementation passes every assertion above, so
// the counterweight lives in the same file: the owner reaches their own
// restricted asset in full, and so does a caller holding
// content.read.all — the capability that exists so the demo-viewer role
// can render a mostly-restricted catalogue, which a catalogue of
// placeholders is not.
func TestAssetWithholding_FeatureStillWorks(t *testing.T) {
	pool := byTagPool(t)
	fwSeedOwner(t, pool)
	restricted := fwSeedAsset(t, pool, "Unreleased boss theme", "restricted")

	ownerRef := fwOwner
	strangerRef := fwStranger
	for _, c := range []struct {
		name string
		id   *auth.Identity
	}{
		{"the owner", &auth.Identity{UserRef: ownerRef, AuthMethod: "session", Capabilities: []string{}}},
		{"content.read.all holder", &auth.Identity{
			UserRef: strangerRef, AuthMethod: "session",
			Capabilities: []string{visibility.ContentReadAll},
		}},
		{"system.admin", &auth.Identity{
			UserRef: strangerRef, AuthMethod: "session",
			Capabilities: []string{visibility.SystemAdmin},
		}},
	} {
		router := simRouter(t, pool, c.id)
		status, body := fwGetAsset(t, router, restricted)
		if status != http.StatusOK {
			t.Fatalf("%s: GET restricted asset status=%d, want 200", c.name, status)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		if _, withheld := m["owner_display_name"]; withheld {
			t.Errorf("%s: got the placeholder for an asset they are entitled to read in full", c.name)
		}
		for _, want := range []string{"title", "description", "file_hash", "file_size_bytes",
			"file_extension", "metadata", "tags", "thumbhash", "created_at"} {
			if _, ok := m[want]; !ok {
				t.Errorf("%s: lost %q on an asset they may read — the withholding is too wide", c.name, want)
			}
		}
		var title string
		if err := json.Unmarshal(m["title"], &title); err != nil || title != "Unreleased boss theme" {
			t.Errorf("%s: title = %q, want the real one", c.name, title)
		}
	}
}
