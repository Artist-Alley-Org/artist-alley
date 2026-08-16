// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1106 — `GET /assets?liked_by=` and the difference between a CORPUS
// listing and a DERIVED one.
//
// # Two correct answers to "you cannot read this row"
//
// Unfiltered browse returns an unreadable asset as a PLACEHOLDER: id,
// `restricted: true`, the owner's display name, nothing else (#883,
// #899). That is ADR 0064's answer for a corpus listing, and it is what
// #881's request-access flow attaches to — "there is work here, by this
// artist, that you may ask for".
//
// A likes listing is not a corpus. Its rows are one person's ACTIONS,
// and a placeholder on it says "this person liked something you cannot
// see" — a fact about a third party's behaviour attached to a row they
// do not own, disclosed to a viewer entitled to neither. So on THIS page
// the unreadable row is ABSENT.
//
// # What this file has to prove, and why each arm exists
//
//  1. The readable asset the same user liked IS listed. Without it,
//     "absent" is satisfied by an endpoint that returns nothing, which
//     is a different bug with the same test output.
//  2. The unreadable one is absent — not present-and-redacted. Asserted
//     against the SERIALIZED page, because a placeholder IS a row and a
//     struct-level "is it readable" check would pass on one.
//  3. Browse still placeholders the very same asset for the very same
//     caller. This is the arm that makes 2 a DERIVED-LISTING rule rather
//     than an accidental tightening of the field plane: two pages, one
//     caller, one asset, deliberately different answers.
//
// Skips without AA_DB_PASSWORD.

package assets_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

const (
	albOwner    int64 = 11061001 // owns both assets
	albLiker    int64 = 11061002 // likes both; is not the owner
	albStranger int64 = 11061003 // the viewer; entitled to neither owner nor liker
)

func albSeedAsset(t *testing.T, pool *pgxpool.Pool, title, sensitivity string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	sum := sha256.Sum256(id[:])
	hash := hex.EncodeToString(sum[:])
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 4242, 'fs')
		 ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                    processing_status, file_hash, file_extension, file_size_bytes)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active',$4,'ready',$5,'png',4242)`,
		id, title, albOwner, sensitivity, hash); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO likes (target_kind, target_id, user_ref) VALUES ('asset', $1, $2)
		 ON CONFLICT DO NOTHING`, id, albLiker); err != nil {
		t.Fatalf("seed like: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM likes WHERE target_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

// albPage issues one real request and returns the items keyed by id.
func albPage(t *testing.T, router chi.Router, url string) map[string]json.RawMessage {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", url, rr.Code, rr.Body.String())
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

func TestLikedByAssets_UnreadableIsAbsentNotPlaceholdered(t *testing.T) {
	pool := byTagPool(t)

	readable := albSeedAsset(t, pool, "alb public splash", "public")
	withheld := albSeedAsset(t, pool, "alb SECRET boss concept", "restricted")

	router := simRouter(t, pool, &auth.Identity{UserRef: albStranger, AuthMethod: "session"})

	likes := albPage(t, router, "/assets?limit=200&liked_by=11061002")

	// Arm 1 — the positive control. Without it every assertion below is
	// satisfied by an endpoint that returns nothing at all.
	if _, present := likes[readable.String()]; !present {
		t.Fatalf("the READABLE asset this user liked is missing from the Likes page — "+
			"the filter is refusing everything, which passes the withholding arm below "+
			"for the wrong reason. Page: %v", keysOf(likes))
	}

	// Arm 2 — the rule.
	if raw, present := likes[withheld.String()]; present {
		t.Errorf("a liked asset the viewer CANNOT READ appeared on the Likes page as %s. "+
			"A derived listing must not disclose that somebody liked something this "+
			"viewer may not see — absent, not redacted (#1106)", raw)
	}

	// Arm 3 — the same caller, the same asset, on BROWSE. This is what
	// makes arm 2 a property of the derived listing rather than a
	// tightening of the field plane that would have broken #881's
	// request-access affordance everywhere.
	browse := albPage(t, router, "/assets?limit=200")
	raw, present := browse[withheld.String()]
	if !present {
		t.Fatalf("the withheld asset is missing from BROWSE too — #883/#899 require a " +
			"placeholder there, so this change tightened the corpus listing as well and " +
			"took #881's request-access affordance with it")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal browse row: %v", err)
	}
	var restricted bool
	if err := json.Unmarshal(m["restricted"], &restricted); err != nil || !restricted {
		t.Errorf("browse row for the withheld asset is not a placeholder (raw=%s) — the "+
			"fixture is not testing an unreadable asset", raw)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
