// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1596: the metadata-extraction field writer refuses a value extracted
// from an original the asset no longer shows. The compare is under the
// asset row lock in the write's own transaction, so it holds against a
// replacement that commits first and against one that holds the lock
// while the write waits.
//
// Real Postgres; skips without AA_DB_PASSWORD.

package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	assetmetadata "github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/assets"
)

func TestMetaValueWriter_RefusesAReplacedOriginal(t *testing.T) {
	pool := openPoolForSensitivity(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	hash := func() string {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		h := hex.EncodeToString(b)
		if _, err := pool.Exec(ctx, `INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, 64, 'image/jpeg', 'fs')`, h); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, h)
			_, _ = pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, h)
		})
		return h
	}
	h1, h2, h3 := hash(), hash(), hash()

	a := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO assets (id, title, asset_type, status, file_hash, file_extension, sensitivity)
		VALUES ($1, 'supersede probe', 1, 'active', $2, 'jpg', 'public')`, a, h1); err != nil {
		t.Fatal(err)
	}
	var field uuid.UUID
	code := "x1596_" + a.String()[:8]
	if err := pool.QueryRow(ctx, `INSERT INTO field_definition (code, label, type, subject_kind) VALUES ($1, 'supersede probe', 'text', 'asset') RETURNING id`, code).Scan(&field); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM extraction_failure WHERE asset_id = $1`, a)
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, a)
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = $1`, a)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, a)
		_, _ = pool.Exec(c, `DELETE FROM field_definition WHERE id = $1`, field)
	})

	w := metaValueWriterAdapter{pool: pool}
	write := func(c context.Context, text, from string) error {
		return w.WriteAssetFieldValue(c, assetmetadata.WriteAssetFieldValueParams{
			AssetID: a, FieldID: field, FieldType: "text", SetBy: "exif", FileHash: from,
			Value: assetmetadata.Value{Kind: assetmetadata.ValueKindText, Text: text},
		})
	}
	value := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT value_text FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`, a, field).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// moveTo makes the asset's next version on `to`, as the replacement
	// does (the same two statements), on tx.
	moveTo := func(tx pgx.Tx, to string) {
		t.Helper()
		var cur pgtype.UUID
		var n int64
		if err := tx.QueryRow(ctx, `SELECT a.current_version_id, v.number FROM assets a JOIN asset_versions v ON v.id = a.current_version_id WHERE a.id = $1 FOR UPDATE OF a`, a).Scan(&cur, &n); err != nil {
			t.Fatal(err)
		}
		q := assets.New(tx)
		next := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		ext := "jpg"
		if err := q.CreateReplacementVersion(ctx, assets.CreateReplacementVersionParams{
			AssetID: pgtype.UUID{Bytes: a, Valid: true}, VersionID: next, Number: n + 1, PredecessorID: cur,
			FileHash: to, FileExtension: &ext, OriginalBasenameSource: "unknown",
		}); err != nil {
			t.Fatal(err)
		}
		if err := q.MoveAssetToVersion(ctx, assets.MoveAssetToVersionParams{
			FileHash: to, FileExtension: &ext, ProcessingStatus: "pending", VersionID: next, ID: pgtype.UUID{Bytes: a, Valid: true},
			PreviewNamespaces: assets.PreviewMetadataNamespaces,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// While the asset shows h1, an extraction of h1 lands.
	if err := write(ctx, "from v1", h1); err != nil {
		t.Fatalf("write for the current bytes: %v", err)
	}

	// Replaced first: the late write of h1 is refused and changes nothing.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A failed check must not leave the row locked: the cleanup deletes it.
	defer func() { _ = tx.Rollback(context.Background()) }()
	moveTo(tx, h2)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := write(ctx, "late v1", h1); !errors.Is(err, assetmetadata.ErrAssetSuperseded) {
		t.Fatalf("late write of replaced bytes: %v, want ErrAssetSuperseded", err)
	}
	if got := value(); got != "from v1" {
		t.Fatalf("value %q after a refused write, want the value from before the replacement", got)
	}
	// So is a late failure report for the old file; one for the current
	// file is recorded.
	fw := metaFailureAdapter{pool: pool}
	failures := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM extraction_failure WHERE asset_id = $1`, a).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	report := func(from string) {
		t.Helper()
		if err := fw.RecordExtractionFailure(ctx, assetmetadata.RecordExtractionFailureParams{
			AssetID: a, Format: "image/jpeg", ErrorKind: "validation", Message: "probe", FileHash: from,
		}); err != nil {
			t.Fatal(err)
		}
	}
	report(h1)
	if n := failures(); n != 0 {
		t.Fatalf("%d failures recorded for the replaced file, want none", n)
	}
	report(h2)
	if n := failures(); n != 1 {
		t.Fatalf("%d failures recorded for the current file, want 1", n)
	}

	// Racing: a replacement holds the row lock while a write of the bytes
	// it is replacing waits on it. The write must come back refused once
	// the replacement commits, not land on the new version.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(context.Background()) }()
	tx = tx2
	moveTo(tx, h3)
	done := make(chan error, 1)
	go func() { done <- write(context.Background(), "racing v2", h2) }()
	waitForLockWaiter(t, pool)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, assetmetadata.ErrAssetSuperseded) {
			t.Fatalf("racing write: %v, want ErrAssetSuperseded", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the racing write never returned")
	}
	if got := value(); got != "from v1" {
		t.Fatalf("value %q after the race, want it unchanged", got)
	}

	// The current bytes' own extraction still lands.
	if err := write(ctx, "from v3", h3); err != nil {
		t.Fatalf("write for the current bytes: %v", err)
	}
	if got := value(); got != "from v3" {
		t.Fatalf("value %q, want the current bytes' value", got)
	}
}

// waitForLockWaiter returns once some backend on this database is
// waiting on a row lock: the racing write has reached the replacement's
// lock. Bounded; a write that never blocks fails the test here.
func waitForLockWaiter(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the racing write never waited on the replacement's row lock")
}
