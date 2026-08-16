// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// #1073 — a collection's TTL could be set and could not be removed.
//
// `expires_at` wore `COALESCE(narg, expires_at)` while the column one
// line below it had already moved to the CASE shape (#1027). A NULL narg
// means "keep", and `CollectionUpdate.ExpiresAt` is a *time.Time with
// `omitempty`, so a body that said `{"expires_at": null}` and a body that
// omitted the field entirely arrived at the query as the same nil. The
// caller got a 200 and a collection that still expired.
//
// Every assertion below reads the COLUMN BACK OUT OF POSTGRES. The
// handler echoes the row it just wrote, so an assertion on the response
// body passes on the bug: the echo is derived from the same RETURNING
// clause the broken CASE would have populated correctly-looking.

// readExpiry reads the PERSISTED expires_at straight off the row. A nil
// result is a cleared TTL. Deliberately not a read of the response body:
// see the note above.
func readExpiry(t *testing.T, pool *pgxpool.Pool, id string) *time.Time {
	t.Helper()
	var ts *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT expires_at FROM collections WHERE id = $1`, id).Scan(&ts); err != nil {
		t.Fatalf("read back expires_at: %v", err)
	}
	if ts != nil {
		utc := ts.UTC()
		return &utc
	}
	return nil
}

// TestCollectionExpiry_ClearFlagNullsTheColumn is the fix, asserted at
// the database.
func TestCollectionExpiry_ClearFlagNullsTheColumn(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)

	deadline := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	colID := mustCreate(t, router, map[string]any{
		"name":       "ct_ttl_clear",
		"visibility": "private",
		"expires_at": deadline.Format(time.RFC3339),
	})
	if got := readExpiry(t, pool, colID); got == nil {
		t.Fatalf("precondition: expires_at is already NULL after create — the "+
			"clear test would pass without the fix. wanted %s", deadline)
	}

	if rr := patchJSON(t, router, "/collections/"+colID,
		map[string]any{"clear_expires_at": true}); rr.Code != http.StatusOK {
		t.Fatalf("clear_expires_at: %d body=%s", rr.Code, rr.Body.String())
	}
	if got := readExpiry(t, pool, colID); got != nil {
		t.Errorf("expires_at persisted as %s after clear_expires_at, want NULL — "+
			"this is #1073: the COALESCE read the clear as \"keep\"", got)
	}
}

// TestCollectionExpiry_ValueAndClearTogetherIs400 — two intentions in
// one body, refused rather than resolved. Same rule, and the same
// sentence, as cover_asset_id + clear_cover: silently discarding one is
// exactly how a "clear" that never happened gets shipped, which is the
// defect this issue is.
func TestCollectionExpiry_ValueAndClearTogetherIs400(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)

	deadline := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	colID := mustCreate(t, router, map[string]any{
		"name": "ct_ttl_both", "visibility": "private",
		"expires_at": deadline.Format(time.RFC3339),
	})

	rr := patchJSON(t, router, "/collections/"+colID, map[string]any{
		"expires_at":       time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339),
		"clear_expires_at": true,
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expires_at + clear_expires_at together: status=%d, want 400", rr.Code)
	}
	// And the refusal is total — a 400 that had already written something
	// would be worse than either resolution.
	got := readExpiry(t, pool, colID)
	if got == nil || !got.Equal(deadline) {
		t.Errorf("expires_at = %v after the refused patch, want the untouched %s", got, deadline)
	}
}

// TestCollectionExpiry_OmittedFieldKeepsTTL is the regression control.
// The CASE must not turn "I did not mention expires_at" into a clear —
// a fix that nulled the column on every PATCH would pass the test above
// and silently expire nothing.
func TestCollectionExpiry_OmittedFieldKeepsTTL(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)

	deadline := time.Now().Add(96 * time.Hour).UTC().Truncate(time.Second)
	colID := mustCreate(t, router, map[string]any{
		"name": "ct_ttl_keep", "visibility": "private",
		"expires_at": deadline.Format(time.RFC3339),
	})

	if rr := patchJSON(t, router, "/collections/"+colID,
		map[string]any{"name": "ct_ttl_keep_renamed"}); rr.Code != http.StatusOK {
		t.Fatalf("rename: %d body=%s", rr.Code, rr.Body.String())
	}
	got := readExpiry(t, pool, colID)
	if got == nil || !got.Equal(deadline) {
		t.Errorf("expires_at = %v after a PATCH that never mentioned it, want %s", got, deadline)
	}

	// `expires_at: null` is NOT a clear, and the OpenAPI description now
	// says so. It decodes to the same nil pointer as "absent" — there is
	// no wire-level difference left for the server to act on — so this
	// asserts the contract that actually ships rather than the one the
	// spec used to promise.
	if rr := patchJSON(t, router, "/collections/"+colID,
		map[string]any{"expires_at": nil}); rr.Code != http.StatusOK {
		t.Fatalf("expires_at:null: %d body=%s", rr.Code, rr.Body.String())
	}
	if got := readExpiry(t, pool, colID); got == nil || !got.Equal(deadline) {
		t.Errorf("expires_at = %v after `expires_at: null`, want the unchanged %s — "+
			"if this ever starts clearing, the CollectionUpdate docs need to change with it",
			got, deadline)
	}
}

// TestCollectionExpiry_ClearIsIdempotentOnAnUnsetTTL — clearing a TTL
// that was never set is a no-op, not an error.
func TestCollectionExpiry_ClearIsIdempotentOnAnUnsetTTL(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)

	colID := mustCreate(t, router, map[string]any{"name": "ct_ttl_none", "visibility": "private"})
	if rr := patchJSON(t, router, "/collections/"+colID,
		map[string]any{"clear_expires_at": true}); rr.Code != http.StatusOK {
		t.Fatalf("clear on an unset TTL: %d body=%s", rr.Code, rr.Body.String())
	}
	if got := readExpiry(t, pool, colID); got != nil {
		t.Errorf("expires_at = %s, want NULL", got)
	}
}
