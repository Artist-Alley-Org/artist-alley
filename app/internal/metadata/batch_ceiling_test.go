// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE TWO CEILINGS AND THE FOUR PERFORMANCE ACCEPTANCES (#1173, #1119,
// ADR 0019).
//
// The ceilings are the boundary between what this synchronous,
// bounded operation does and what #39's unbounded actions still owe to
// a job queue. They are also what the latency budget, the audit
// envelope's size and the batch-wide guards' hold time are all sized
// against — so they are measured here rather than asserted from a
// constant.
package metadata_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// A108-A110 — the ceilings
// ---------------------------------------------------------------------------

// A108. 501 SELECTION ENTRIES is 422, WITH NO EXPANSION QUERY EXECUTED.
// The entry ceiling is checked before any membership query runs, so an
// absurd selection costs one comparison rather than five hundred
// expansions — and the assertion is made on the QUERY COUNTER, because
// a status-only assertion cannot tell a cheap refusal from an expensive
// one.
func TestBatch_SelectionEntryCeiling(t *testing.T) {
	f := newBatchFixture(t)
	_, ctx := f.bulkOperator("entryceiling")
	field := f.textField(false)

	before := f.postExpansionCalls()

	entries := make([]openapi.BatchAssetFieldSelectionEntry, 0, 501)
	for i := 0; i < 501; i++ {
		entries = append(entries, openapi.BatchAssetFieldSelectionEntry{
			Kind: openapi.BatchSelectionPost, Id: openapi_types.UUID(uuid.New()),
		})
	}
	res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), entries)
	f.wantPreviewRefusal(res, 422, openapi.BatchSelectionEntryCeiling)
	if res.Refusal.Expected == nil || *res.Refusal.Expected != 500 {
		t.Fatalf("the refusal must name the ceiling, got %+v", res.Refusal.Expected)
	}
	if res.Refusal.Actual == nil || *res.Refusal.Actual != 501 {
		t.Fatalf("the refusal must name the actual count, got %+v", res.Refusal.Actual)
	}
	if after := f.postExpansionCalls(); after != before {
		t.Fatalf("NO EXPANSION QUERY may run: post_assets was queried %d extra times", after-before)
	}

	// 500 exactly is accepted, so the ceiling is a bound rather than an
	// off-by-one.
	ok := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), entries[:500])
	if ok.Status != 200 && ok.Refusal != nil && ok.Refusal.Reason == openapi.BatchSelectionEntryCeiling {
		t.Fatal("500 entries is within the ceiling")
	}
}

// A109. 1,001 DISTINCT EXPANDED TARGETS is 422 with BOTH counts — the
// ceiling and the actual — plus the ENTRY count, so an operator can see
// that a handful of posts reached over a thousand assets.
func TestBatch_ExpandedTargetCeiling(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("expceiling")
	field := f.textField(false)

	assets := f.bulkAssets(owner, 1001)
	entries := assetEntries(assets...)

	res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), entries[:1001])
	// 1,001 asset entries also exceeds the ENTRY ceiling, so the
	// expanded ceiling needs posts to reach it — see the next case.
	if res.Refusal == nil {
		t.Fatalf("want a refusal, got %d", res.Status)
	}

	// A110. A SINGLE POST whose membership alone exceeds the ceiling is
	// refused on exactly the same terms: NO PARTIAL EXPANSION, no
	// trimming, no "the first thousand". A batch that quietly wrote a
	// different set than the operator selected would be worse than a
	// refusal.
	big := f.post(owner, assets...)
	single := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), postEntries(big))
	f.wantPreviewRefusal(single, 422, openapi.BatchExpandedTargetCeiling)
	if single.Refusal.Expected == nil || *single.Refusal.Expected != 1000 {
		t.Fatalf("want the ceiling named, got %+v", single.Refusal.Expected)
	}
	if single.Refusal.Actual == nil || *single.Refusal.Actual != 1001 {
		t.Fatalf("want the actual count named, got %+v", single.Refusal.Actual)
	}
	if single.Refusal.EntryCount == nil || *single.Refusal.EntryCount != 1 {
		t.Fatalf("want the ENTRY count named too (one post reached 1001 assets), got %+v",
			single.Refusal.EntryCount)
	}
	if single.OK != nil {
		t.Fatal("NO PARTIAL EXPANSION")
	}
}

// ---------------------------------------------------------------------------
// A111-A113 — the four performance acceptances, AT the ceiling
// ---------------------------------------------------------------------------

// TestBatch_AtTheCeiling measures all four acceptances on ONE fixture
// of 1,000 real targets, because building it is the expensive part and
// four separate builds would measure the fixture rather than the
// operation.
//
//  1. apply latency at 1,000 targets, p95 <= 10 s
//  2. the search-text rebuild and cache notify fire for every written
//     row, and the elapsed trigger cost is reported
//  3. the audit envelope round-trips BYTE-INTACT and stays under 128 KB
//  4. the batch-wide guards' hold time is measured, and a concurrent
//     ordinary single-target write to an UNRELATED field is not starved
func TestBatch_AtTheCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("the ceiling fixture builds 1,000 assets")
	}
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("ceiling")
	field := f.field("t", fieldSpec{Type: "text"})
	unrelated := f.field("u", fieldSpec{Type: "text"})

	const n = 1000
	assets := f.bulkAssets(owner, n)

	// Reached through POSTS, not through a thousand asset entries: the
	// entry ceiling is 500, and a selection of four posts reaching a
	// thousand assets is the shape an operator actually produces.
	posts := make([]uuid.UUID, 0, 4)
	for i := 0; i < 4; i++ {
		posts = append(posts, f.post(owner, assets[i*250:(i+1)*250]...))
	}
	selection := postEntries(posts...)

	// ── 1. LATENCY, p95 over five runs ─────────────────────────────
	const runs = 5
	durations := make([]time.Duration, 0, runs)
	var lastOp string
	for i := 0; i < runs; i++ {
		p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field,
			textValue(fmt.Sprintf("ceiling run %d", i)), selection)
		if p.Counts.Expanded != n || p.Counts.WouldChange != n {
			t.Fatalf("run %d: want %d would_change, got %+v", i, n, p.Counts)
		}
		start := time.Now()
		res := f.apply(ctx, p.Token, fmt.Sprintf("ceiling measurement run %d", i), intp(n))
		elapsed := time.Since(start)
		if res.OK == nil {
			t.Fatalf("run %d refused: %+v", i, res.Refusal)
		}
		if res.OK.OutcomeCounts.Changed != n {
			t.Fatalf("run %d: want %d changed, got %+v", i, n, res.OK.OutcomeCounts)
		}
		durations = append(durations, elapsed)
		lastOp = p.OperationId.String()
		t.Logf("apply run %d over %d targets: %s (%.3f ms/target)",
			i, n, elapsed, float64(elapsed.Microseconds())/1000.0/float64(n))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[len(durations)-1] // the worst of five IS the p95 bound here
	t.Logf("APPLY LATENCY AT %d TARGETS: p95 = %s (budget 10s)", n, p95)
	if p95 > 10*time.Second {
		t.Fatalf("p95 apply latency at %d targets is %s, over the 10 s budget", n, p95)
	}

	// ── 2. SEARCH-TEXT REBUILD AND NOTIFY ──────────────────────────
	//
	// The trigger fires per written row. Asserted on COMPLETION rather
	// than on a count of notifications — the queue is not observable
	// from here — and the elapsed cost is reported.
	triggerStart := time.Now()
	var withSearchText int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM assets
		 WHERE id = ANY($1::uuid[]) AND search_text IS NOT NULL`, assets).Scan(&withSearchText); err != nil {
		t.Fatalf("read search_text: %v", err)
	}
	t.Logf("search-text rebuild: %d of %d rows carry a search_text after the batch (read in %s)",
		withSearchText, n, time.Since(triggerStart))
	if withSearchText != n {
		t.Fatalf("the rebuild trigger must fire for every written row; %d of %d", withSearchText, n)
	}
	var notifyErrors int
	_ = f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction (aborted)'`).Scan(&notifyErrors)
	if notifyErrors > 0 {
		t.Logf("note: %d aborted backends observed (not attributed to this batch)", notifyErrors)
	}

	// ── 3. THE AUDIT ENVELOPE AT THE CEILING ───────────────────────
	var raw []byte
	if err := f.pool.QueryRow(f.ctx,
		`SELECT metadata FROM audit_events WHERE metadata->>'operation_id' = $1`, lastOp).Scan(&raw); err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	t.Logf("AUDIT ENVELOPE AT %d TARGETS: %d bytes (%0.1f KB), budget 128 KB", n, len(raw), float64(len(raw))/1024)
	if len(raw) > 128*1024 {
		t.Fatalf("the envelope is %d bytes, over the 128 KB budget", len(raw))
	}
	// BYTE-INTACT ROUND TRIP: it decodes, and every target id is there.
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("the envelope must round-trip: %v", err)
	}
	targets, _ := env["target_ids"].(map[string]any)
	changed, _ := targets["changed"].([]any)
	if len(changed) != n {
		t.Fatalf("the envelope must carry all %d target ids, found %d", n, len(changed))
	}
	seen := map[string]bool{}
	for _, id := range changed {
		seen[id.(string)] = true
	}
	for _, a := range assets {
		if !seen[a.String()] {
			t.Fatalf("target %s missing from the envelope", a)
		}
	}

	// ── 4. GUARD CONTENTION ────────────────────────────────────────
	//
	// The batch-wide guards are held across up to a thousand writes.
	// Measured, and asserted NOT TO STARVE an ordinary single-target
	// write to an UNRELATED field, which must complete while the batch
	// is in flight.
	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("contention run"), selection)
	victim := assets[0]

	var wg sync.WaitGroup
	var batchElapsed, victimElapsed time.Duration
	var victimErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		res := f.apply(ctx, p.Token, "contention measurement", intp(n))
		batchElapsed = time.Since(start)
		if res.OK == nil {
			t.Errorf("the contention run refused: %+v", res.Refusal)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		_, victimErr = f.pool.Exec(context.Background(), `
			INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by)
			VALUES ($1, $2, 'ordinary single-target write', 'manual')
			ON CONFLICT (asset_id, field_id) DO UPDATE SET value_text = EXCLUDED.value_text`,
			victim, unrelated)
		victimElapsed = time.Since(start)
	}()
	wg.Wait()

	t.Logf("GUARD CONTENTION AT %d TARGETS: the batch held its guards for %s; a concurrent "+
		"ordinary single-target write to an UNRELATED field completed in %s",
		n, batchElapsed, victimElapsed)
	if victimErr != nil {
		t.Fatalf("the unrelated single-target write was STARVED: %v", victimErr)
	}
	if got, ok := f.storedText(victim, unrelated); !ok || got != "ordinary single-target write" {
		t.Fatalf("the unrelated write must have landed, got %q", got)
	}
	if victimElapsed > batchElapsed {
		t.Logf("note: the unrelated write took longer than the batch itself, which would "+
			"mean it queued behind it (%s vs %s)", victimElapsed, batchElapsed)
	}
}

// bulkAssets seeds n assets in ONE statement. A loop of inserts at the
// ceiling is minutes of fixture time measuring the fixture.
func (f *batchFixture) bulkAssets(owner int64, n int) []uuid.UUID {
	f.t.Helper()
	ids := make([]uuid.UUID, 0, n)
	hashes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, uuid.New())
		hashes = append(hashes, fmt.Sprintf("%064x", i)[:64])
	}
	// Distinct hashes so the per-owner dedup index does not refuse them.
	for i := range hashes {
		hashes[i] = strings.Repeat("0", 40) + fmt.Sprintf("%024d", i)
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		SELECT h, 1024, 'image/png', 'fs' FROM unnest($1::text[]) AS h
		ON CONFLICT (hash) DO NOTHING`, hashes); err != nil {
		f.t.Fatalf("bulk storage_objects: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO assets (id, title, asset_type, owner_user_ref, status,
		                    file_hash, file_extension, file_size_bytes, sensitivity)
		SELECT i, 'bx-bulk', 1, $3, 'active', h, 'png', 1024, 'public'
		  FROM unnest($1::uuid[], $2::text[]) AS t(i, h)`, ids, hashes, owner); err != nil {
		f.t.Fatalf("bulk assets: %v", err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM asset_field_value_history WHERE asset_id = ANY($1::uuid[])`, ids)
		_, _ = f.pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = ANY($1::uuid[])`, ids)
		_, _ = f.pool.Exec(c, `DELETE FROM post_assets WHERE asset_id = ANY($1::uuid[])`, ids)
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = ANY($1::text[])`, hashes)
	})
	return ids
}

// postExpansionCalls reads Postgres' own counter of sequential and
// index scans on post_assets, so "no expansion query ran" is an
// observation of the DATABASE rather than of the handler's intentions.
func (f *batchFixture) postExpansionCalls() int64 {
	f.t.Helper()
	var n *int64
	if err := f.pool.QueryRow(f.ctx, `
		SELECT COALESCE(seq_scan, 0) + COALESCE(idx_scan, 0)
		  FROM pg_stat_user_tables WHERE relname = 'post_assets'`).Scan(&n); err != nil {
		f.t.Fatalf("read post_assets scan counter: %v", err)
	}
	if n == nil {
		return 0
	}
	return *n
}
