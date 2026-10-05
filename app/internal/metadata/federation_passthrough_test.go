// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.9.B — federation pass-through guard.
//
// Collections are local-instance only at MVP per ADR 0043, so a
// collection_field_value write must NOT emit a federation activity.
// If a future commit accidentally wires emission into the metadata
// upsert path, this test fails before the wire ships.
//
// The check is structural: count activity rows pre/post a value
// write. The activities table is the single source of truth for
// federation outbox candidates (see ADR 0044 + the outbox dispatch
// SELECT in app/internal/federation/outbox). Zero new rows = no
// new federation surface.
package metadata_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
)

// TestCollectionField_Upsert_DoesNotEmitFederationActivity asserts
// the federation table count is unchanged across a collection
// field value write. If/when collections start federating, this
// test moves to the federated-emission test suite and the
// assertion flips.
func TestCollectionField_Upsert_DoesNotEmitFederationActivity(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() { cleanCollectionTestRows(t, pool) })

	router, userRef := makeRouter(t, pool, true)
	fieldID := mustCreateCollectionField(t, router, "mcoltest_fedguard", "Fed Guard", "text")
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest col fedguard")

	// Snapshot the activities count for THIS user before the write.
	// Filtering by actor scopes the assertion so a parallel test
	// package emitting unrelated activity doesn't false-positive.
	var before, after int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM activities WHERE actor_user_ref = $1`,
		userRef,
	).Scan(&before); err != nil {
		t.Fatalf("count activities before: %v", err)
	}

	rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID), map[string]any{
		"value_text": "fed-guard-value",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT value: %d body=%s", rr.Code, rr.Body.String())
	}

	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM activities WHERE actor_user_ref = $1`,
		userRef,
	).Scan(&after); err != nil {
		t.Fatalf("count activities after: %v", err)
	}

	if after != before {
		t.Fatalf("activities row count changed: before=%d after=%d. "+
			"Collection metadata is local-only per ADR 0043 — writing a "+
			"collection field value must not emit a federation activity. "+
			"If you intentionally added federation support, move this test "+
			"to the federated-emission suite and flip the assertion.",
			before, after)
	}
}
