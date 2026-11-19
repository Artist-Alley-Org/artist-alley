// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.18.A-2 follow-up A — dedup race test.
//
// Proves the load-bearing concurrency invariant: N concurrent
// uploads of the same file by the same user produce EXACTLY ONE
// asset row, with the losers getting a dedup-warning response
// (default behavior). The partial unique index on
// (owner_user_ref, file_hash) from migration 00001 + the
// 23505-classification race-loser path in CreateAsset together
// provide the guarantee.
//
// TestUpload_RaceLoser_ReleasesItsConnectionBeforeRefetch pins the
// race-loser path itself, deterministically and on a one-connection
// pool: the loser must give its failed transaction's connection back
// before it reads the winner's row.

package assets_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

func TestUpload_ConcurrentSameFile_ExactlyOneAssetRowCreated(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(func() { pool.Close() })

	// Unique per-run hash so concurrent runs of this test against
	// the same shared dev DB don't collide on each other's
	// fixtures. Storage's CHECK constraint requires 64 hex chars
	// (sha256 shape), so strip the UUID dashes + pad with hex
	// nibbles to length.
	rawA := uuid.New().String() // 36 chars incl. dashes
	rawB := uuid.New().String()
	cleaned := ""
	for _, r := range rawA + rawB {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			cleaned += string(r)
		}
	}
	for len(cleaned) < 64 {
		cleaned += "0"
	}
	hash := cleaned[:64]
	const ownerRef int64 = 9_142_999

	// Seed a parent storage_objects row so the asset's file_hash
	// FK resolves. Hard-delete any prior assets for this (owner,
	// hash) so we have a clean slate.
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`DELETE FROM assets WHERE owner_user_ref = $1 AND file_hash = $2`,
		ownerRef, hash,
	); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1, 'image/png', 'fs')
		ON CONFLICT (hash) DO NOTHING
	`, hash); err != nil {
		t.Fatalf("seed storage_object: %v", err)
	}

	// Real storage Service for the AddPin call path inside
	// CreateAsset. fs backend over a t.TempDir — no real bytes
	// involved, just pin bookkeeping in the DB.
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	h := assets.NewHandler(pool, svc, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)

	const N = 5
	type result struct {
		resp openapi.CreateAssetResponseObject
		err  error
	}
	var wg sync.WaitGroup
	results := make([]result, N)
	start := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // release all goroutines at once
			ctx := auth.WithIdentity(context.Background(),
				&auth.Identity{UserRef: ownerRef, Capabilities: []string{}})
			ext := "png"
			req := openapi.CreateAssetRequestObject{
				Body: &openapi.AssetCreate{
					Title:         "race-test",
					AssetType:     1,
					FileHash:      &hash,
					FileExtension: &ext,
				},
			}
			resp, err := h.CreateAsset(ctx, req)
			results[idx] = result{resp: resp, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	// Tally outcomes. The CONCURRENT-RACE INVARIANT is:
	//   - exactly one goroutine creates an asset row
	//   - the other N-1 see a dedup response (either 200 + warn
	//     or 201 — both shapes mean "you uploaded the same file
	//     someone else just did")
	//   - DB ends with exactly one live row for (owner, hash)
	var creates201, warns200, errsHard int
	var createdID, dedupID openapi_types.UUID
	for _, r := range results {
		if r.err != nil {
			errsHard++
			t.Errorf("hard error: %v", r.err)
			continue
		}
		switch v := r.resp.(type) {
		case openapi.CreateAsset201JSONResponse:
			creates201++
			createdID = openapi.Asset(v).Id
		case openapi.CreateAsset200JSONResponse:
			warns200++
			dedupID = v.DuplicateWarning.ExistingAssetId
		default:
			t.Errorf("unexpected response type %T", r.resp)
		}
	}
	if errsHard > 0 {
		t.Fatalf("got %d hard errors", errsHard)
	}
	if creates201 != 1 {
		t.Errorf("creates201 = %d, want exactly 1 (race winner)", creates201)
	}
	if warns200 != N-1 {
		t.Errorf("warns200 = %d, want exactly %d (race losers)", warns200, N-1)
	}
	// Dedup ID should match the created ID.
	if dedupID != createdID {
		t.Errorf("dedup ID %v doesn't match created ID %v", dedupID, createdID)
	}

	// DB-level invariant: exactly one live row for (owner, hash).
	var liveCount int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM assets WHERE owner_user_ref = $1 AND file_hash = $2 AND deleted_at IS NULL`,
		ownerRef, hash,
	).Scan(&liveCount)
	if liveCount != 1 {
		t.Errorf("live row count = %d, want 1 (the partial unique index from migration 00016 is the guarantee)", liveCount)
	}

	// Cleanup so the next test run starts clean.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM assets WHERE owner_user_ref = $1 AND file_hash = $2`,
			ownerRef, hash,
		)
	})
}

// The race loser must release its failed transaction's connection
// before it takes another from the pool for the re-fetch.
//
// CI run 44425649344 hung here for ten minutes: on a 4-CPU runner the
// pool's default MaxConns is 4, the four losers of the five-way race
// each held their aborted transaction's connection while waiting for a
// second one to read the winner's row, and the winner waited behind
// them for the connection its AddPin needed. Nothing could move.
//
// This test makes that path certain instead of timing-dependent. The
// handler gets a pool of exactly one connection, so a loser that keeps
// its transaction while asking for another can never be served. A
// separate transaction (the "winner") holds an uncommitted row for the
// same (owner, hash): the handler's pre-check cannot see it, so the
// handler's INSERT waits on the unique index. Only once Postgres shows
// that wait is the winner committed, which turns the INSERT into a
// 23505 and sends the handler down the race-loser path. The handler's
// context is bounded, so without the release it fails within seconds
// rather than hanging the package.
func TestUpload_RaceLoser_ReleasesItsConnectionBeforeRefetch(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := context.Background()
	pool := openPool(t, pwd)
	t.Cleanup(func() { pool.Close() })

	cfg := pool.Config()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("one-connection pool: %v", err)
	}
	t.Cleanup(func() { small.Close() })
	if got := small.Config().MaxConns; got != 1 {
		t.Fatalf("handler pool MaxConns = %d, want 1", got)
	}

	// Two dashless UUIDs are exactly 64 hex characters, the sha256
	// shape storage_objects requires.
	hash := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	const ownerRef int64 = 9_143_001
	ext := "png"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM assets WHERE owner_user_ref = $1 AND file_hash = $2`, ownerRef, hash)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1, 'image/png', 'fs')
		ON CONFLICT (hash) DO NOTHING
	`, hash); err != nil {
		t.Fatalf("seed storage_object: %v", err)
	}

	// The winner: an uncommitted row for the same (owner, hash), on the
	// test's own pool, never the handler's.
	winner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("winner begin: %v", err)
	}
	defer func() { _ = winner.Rollback(context.Background()) }()
	var winnerPID int32
	if err := winner.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&winnerPID); err != nil {
		t.Fatalf("winner pid: %v", err)
	}
	owner := ownerRef
	won, err := assets.New(winner).CreateAsset(ctx, assets.CreateAssetParams{
		Title:            "race-winner",
		AssetType:        1,
		OwnerUserRef:     &owner,
		Status:           "active",
		FileHash:         &hash,
		FileExtension:    &ext,
		Metadata:         []byte("{}"),
		ProcessingStatus: "pending",
		// #1595: every CreateAsset caller states the source (the CHECK
		// refuses an empty one); a raw insert has no upload receipt.
		OriginalBasenameSource: "unknown",
	})
	if err != nil {
		t.Fatalf("winner insert: %v", err)
	}

	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	h := assets.NewHandler(small, storage.NewService(backend, small),
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)

	type result struct {
		resp openapi.CreateAssetResponseObject
		err  error
	}
	done := make(chan result, 1)
	hctx, cancel := context.WithTimeout(
		auth.WithIdentity(ctx, &auth.Identity{UserRef: ownerRef, Capabilities: []string{}}),
		15*time.Second)
	defer cancel()
	go func() {
		resp, err := h.CreateAsset(hctx, openapi.CreateAssetRequestObject{
			Body: &openapi.AssetCreate{
				Title:         "race-loser",
				AssetType:     1,
				FileHash:      &hash,
				FileExtension: &ext,
			},
		})
		done <- result{resp: resp, err: err}
	}()

	// Wait until the handler's INSERT is blocked on the winner's row.
	// Committing earlier would let the pre-check see the row and answer
	// from there, never reaching the path under test.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`,
			winnerPID,
		).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("handler answered before its INSERT waited on the winner (resp %T, err %v)", r.resp, r.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the handler's INSERT never waited on the winner's row")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := winner.Commit(ctx); err != nil {
		t.Fatalf("winner commit: %v", err)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("handler did not return; its context should have ended it")
	}
	if r.err != nil {
		t.Fatalf("race loser failed instead of answering with the duplicate: %v", r.err)
	}
	dup, ok := r.resp.(openapi.CreateAsset200JSONResponse)
	if !ok {
		t.Fatalf("race loser answered %T, want the 200 duplicate warning", r.resp)
	}
	if want := openapi_types.UUID(won.ID.Bytes); dup.DuplicateWarning.ExistingAssetId != want {
		t.Errorf("duplicate names %v, want the winner %v", dup.DuplicateWarning.ExistingAssetId, want)
	}
	if dup.Id != openapi_types.UUID(won.ID.Bytes) {
		t.Errorf("duplicate body is asset %v, want the winner %v", dup.Id, won.ID.Bytes)
	}

	var live int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM assets WHERE owner_user_ref = $1 AND file_hash = $2 AND deleted_at IS NULL`,
		ownerRef, hash,
	).Scan(&live); err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 1 {
		t.Errorf("live rows = %d, want exactly the winner's", live)
	}
	if n := small.Stat().AcquiredConns(); n != 0 {
		t.Errorf("handler pool still has %d acquired connection(s) after the request", n)
	}
}
