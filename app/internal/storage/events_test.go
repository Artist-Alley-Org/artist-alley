// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1598 (core): per-asset fixity-check events from the checksum sweep,
// pinned on a real database and the fs backend.
//
//   - every asset that references a swept object (its file, an
//     alternate, a companion) gets exactly one event per observed
//     variant, so two assets sharing deduplicated bytes both have the
//     history, and an asset referencing the same bytes three ways still
//     gets one;
//   - the outcome says what was observed: success (digest_match) only
//     for a recorded digest the bytes match on a verified row; a warning
//     (never success) for a legacy row with no recorded digest, original
//     or derived; checksum_mismatch, size_mismatch and missing_object
//     failures; with the observed and recorded digests, the generation,
//     the row's status and the software agent;
//   - a backend that cannot deliver the bytes gives every referencing
//     asset a read_error failure with no error text, and the run is
//     still marked failed; a retry adds nothing; a cancelled context is
//     not a read failure; a clean read later is a new event;
//   - a batch retried after its events are durable but before the
//     cursor checkpoint writes no second event or finding; a new run, or
//     a new generation of a variant, is a new event;
//   - the finding and the events are one transaction: a failed event
//     leaves no finding behind; a fabricated observation is refused;
//   - events are append-only and outlive their sweep run: no UPDATE
//     except the run's ON DELETE SET NULL of the live reference, the run
//     snapshot kept; no DELETE while the asset exists; the CHECKs refuse
//     an outcome, reason, digest, source or type outside the core.

package storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

const eventAgentVersion = "0.11.2-test"

type eventRig struct {
	*fixityRig
	sweeper *storage.SweepHandler
	q       *storage.Queries
}

func newEventRig(t *testing.T) *eventRig {
	t.Helper()
	r := newFixityRig(t)
	r.svc.AgentVersion = eventAgentVersion
	return &eventRig{
		fixityRig: r,
		sweeper:   storage.NewChecksumVerifyHandler(r.pool, r.svc, nil, nil),
		q:         storage.New(r.pool),
	}
}

// original uploads unique bytes as an original through the real upload
// path, so its row records its address as its digest.
func (r *eventRig) original(t *testing.T, label string) (string, []byte) {
	t.Helper()
	tail := make([]byte, 16)
	if _, err := rand.Read(tail); err != nil {
		t.Fatal(err)
	}
	body := append([]byte(label+": "), tail...)
	res, err := r.svc.UploadOriginal(context.Background(), bytes.NewReader(body), "image/png",
		storage.PinRef{SubjectType: "user", SubjectID: "9159801"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = r.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, res.Hash)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, res.Hash)
	})
	return res.Hash, body
}

// asset creates an asset whose primary file is fileHash ("" for none).
// Registered after the objects it references, so it is removed first.
func (r *eventRig) asset(t *testing.T, fileHash string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var fh any
	if fileHash != "" {
		fh = fileHash
	}
	if _, err := r.pool.Exec(context.Background(),
		`INSERT INTO assets (id, title, asset_type, file_hash) VALUES ($1, 'fixity events', (SELECT MIN(ref) FROM asset_types), $2)`,
		id, fh); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = r.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id) })
	return id
}

func (r *eventRig) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (r *eventRig) run(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := r.q.CreateSweepRun(context.Background(), storage.CreateSweepRunParams{
		ID: pgtype.UUID{Bytes: id, Valid: true}, Kind: "checksum_verify",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = r.pool.Exec(context.Background(), `DELETE FROM storage_sweep_runs WHERE id = $1`, id)
	})
	return id
}

// sweep walks every storage_variants row as run's checksum sweep, batch
// by batch, and returns the per-item finding count the batches report.
func (r *eventRig) sweep(t *testing.T, run uuid.UUID) int64 {
	t.Helper()
	cur, curV := "", ""
	var found int64
	for i := 0; i < 1000; i++ {
		next, nextV, scanned, f, err := r.sweeper.VerifyChecksumBatch(context.Background(), run, cur, curV)
		if err != nil {
			t.Fatalf("sweep batch: %v", err)
		}
		found += f
		if scanned == 0 {
			return found
		}
		cur, curV = next, nextV
	}
	t.Fatal("sweep did not finish")
	return 0
}

// events lists an asset's events, newest first, from run (uuid.Nil: all).
func (r *eventRig) events(t *testing.T, asset, run uuid.UUID) []storage.AssetPreservationEvent {
	t.Helper()
	all, err := r.q.ListAssetPreservationEvents(context.Background(), storage.ListAssetPreservationEventsParams{
		AssetID: pgtype.UUID{Bytes: asset, Valid: true}, PageLimit: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run == uuid.Nil {
		return all
	}
	var out []storage.AssetPreservationEvent
	for _, e := range all {
		if e.SweepRunID.Valid && uuid.UUID(e.SweepRunID.Bytes) == run {
			out = append(out, e)
		}
	}
	return out
}

func (r *eventRig) findings(t *testing.T, run uuid.UUID, hash string) map[string]string {
	t.Helper()
	rows, err := r.pool.Query(context.Background(),
		`SELECT variant_key, finding FROM storage_sweep_findings WHERE run_id = $1 AND object_hash = $2`, run, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, finding string
		if err := rows.Scan(&key, &finding); err != nil {
			t.Fatal(err)
		}
		if prev, dup := out[key]; dup {
			t.Fatalf("two findings for %s (%s, %s)", key, prev, finding)
		}
		out[key] = finding
	}
	return out
}

func deref[T any](p *T) (v T) {
	if p != nil {
		v = *p
	}
	return v
}

// wantEvent is what one event must say; observed "" means no bytes.
type wantEvent struct {
	outcome, reason    string
	generation         int64
	observed, recorded string
	size               int64
	status             string
}

func checkEvent(t *testing.T, who string, e storage.AssetPreservationEvent, hash string, run uuid.UUID, w wantEvent, since time.Time) {
	t.Helper()
	got := wantEvent{
		outcome: e.Outcome, reason: e.OutcomeReason, generation: e.ObservedGeneration,
		observed: deref(e.ObservedSha256), recorded: deref(e.RecordedSha256), size: deref(e.ObservedSizeBytes),
		status: e.FixityStatus,
	}
	if got != w {
		t.Errorf("%s %s: event\n got %+v\nwant %+v", who, e.VariantKey, got, w)
	}
	if e.Source != "local" || e.EventType != "fixity-check" || e.ObjectHash != hash {
		t.Errorf("%s %s: source %q type %q object %s", who, e.VariantKey, e.Source, e.EventType, e.ObjectHash)
	}
	if !e.SweepRunID.Valid || uuid.UUID(e.SweepRunID.Bytes) != run || e.SweepRunRef != e.SweepRunID {
		t.Errorf("%s %s: sweep run %v (live %v), want %s", who, e.VariantKey, e.SweepRunID, e.SweepRunRef, run)
	}
	if e.AgentName != storage.FixityAgentName || deref(e.AgentVersion) != eventAgentVersion {
		t.Errorf("%s %s: agent %q %q", who, e.VariantKey, e.AgentName, deref(e.AgentVersion))
	}
	// A digest travels with its algorithm and size; missing bytes have none.
	if (w.observed == "") != (e.Algorithm == nil) || (e.Algorithm != nil && *e.Algorithm != "sha256") || (w.observed == "") != (e.ObservedSizeBytes == nil) {
		t.Errorf("%s %s: algorithm %v size %v for observed %q", who, e.VariantKey, e.Algorithm, e.ObservedSizeBytes, w.observed)
	}
	if !e.OccurredAt.Valid || e.OccurredAt.Time.Before(since) || e.OccurredAt.Time.After(time.Now().Add(time.Minute)) {
		t.Errorf("%s %s: occurred_at %v, started %v", who, e.VariantKey, e.OccurredAt, since)
	}
}

func TestSweepEvents_EveryReferencingAssetGetsOneTruthfulEventPerObservation(t *testing.T) {
	r := newEventRig(t)
	ctx := context.Background()
	since := time.Now().Add(-time.Second)

	shared, sharedBody := r.original(t, "shared original")
	other, otherBody := r.original(t, "alternate and companion bytes")
	legacyOrig, legacyOrigBody := r.original(t, "legacy original")
	// A pre-00082 original: no recorded digest, only its address, which
	// can expose corruption but is not a recorded digest (a warning).
	r.exec(t, `UPDATE storage_variants SET content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL, fixity_status = 'unverifiable'
		WHERE object_hash = $1 AND variant_key = 'original'`, legacyOrig)

	put := func(key string, body []byte) {
		t.Helper()
		if _, err := r.svc.PutVariant(ctx, shared, key, bytes.NewReader(body), "image/webp"); err != nil {
			t.Fatal(err)
		}
	}
	preview := []byte("derived preview bytes")
	put("preview", preview)
	good := []byte("0123456789abcdef0123456789abcdef")
	evil := []byte("0123456789abcdef0123456789abcdeX")
	put("hires", good)
	if _, err := r.svc.Backend.Put(ctx, shared, "hires", bytes.NewReader(evil)); err != nil {
		t.Fatal(err)
	}
	poster := []byte("poster bytes that will go missing")
	put("poster", poster)
	if err := r.svc.Backend.Delete(ctx, shared, "poster"); err != nil {
		t.Fatal(err)
	}
	legacy := []byte("legacy derived bytes")
	r.legacyRow(t, shared, "legacy", legacy, len(legacy))
	short := []byte("short")
	r.legacyRow(t, shared, "col", short, 999)

	a1 := r.asset(t, shared) // file, alternate and companion: still one event per variant
	r.exec(t, `INSERT INTO asset_alternates (asset_id, label, object_hash, size_bytes) VALUES ($1, 'same bytes', $2, 1)`, a1, shared)
	r.exec(t, `INSERT INTO asset_companions (asset_id, companion_path, object_hash, size_bytes) VALUES ($1, 'sidecar/same.bin', $2, 1)`, a1, shared)
	a2 := r.asset(t, shared) // deduplicated: the same bytes as a1
	a3 := r.asset(t, "")
	r.exec(t, `INSERT INTO asset_alternates (asset_id, label, object_hash, size_bytes) VALUES ($1, 'alt', $2, 1)`, a3, other)
	a4 := r.asset(t, "")
	r.exec(t, `INSERT INTO asset_companions (asset_id, companion_path, object_hash, size_bytes) VALUES ($1, 'sidecar/other.bin', $2, 1)`, a4, other)
	a5 := r.asset(t, "") // references nothing
	a6 := r.asset(t, legacyOrig)

	run := r.run(t)
	if found := r.sweep(t, run); found < 3 {
		t.Fatalf("sweep reported %d findings, want at least the 3 planted", found)
	}

	n := func(b []byte) int64 { return int64(len(b)) }
	wantShared := map[string]wantEvent{
		storage.VariantOriginal: {"success", storage.ReasonDigestMatch, 1, shared, shared, n(sharedBody), storage.FixityVerified},
		"preview":               {"success", storage.ReasonDigestMatch, 1, sum(preview), sum(preview), n(preview), storage.FixityVerified},
		"hires":                 {"failure", storage.ReasonChecksumMismatch, 1, sum(evil), sum(good), n(evil), storage.FixityMismatch},
		"poster":                {"failure", storage.ReasonMissingObject, 1, "", sum(poster), 0, storage.FixityMissing},
		"legacy":                {"warning", storage.ReasonNoRecordedDigest, 0, sum(legacy), "", n(legacy), storage.FixityUnverifiable},
		"col":                   {"failure", storage.ReasonSizeMismatch, 0, sum(short), "", n(short), storage.FixityUnverifiable},
	}
	origFx := r.fixity(t, shared, storage.VariantOriginal)
	if origFx.Generation != 1 {
		t.Fatalf("uploaded original generation %d; the expectations assume 1", origFx.Generation)
	}
	byVariant := func(who string, asset uuid.UUID, want map[string]wantEvent, hash string) {
		t.Helper()
		evs := r.events(t, asset, uuid.Nil)
		if len(evs) != len(want) {
			t.Fatalf("%s: %d events, want %d (one per variant): %+v", who, len(evs), len(want), evs)
		}
		seen := map[string]bool{}
		for _, e := range evs {
			w, ok := want[e.VariantKey]
			if !ok || seen[e.VariantKey] {
				t.Fatalf("%s: unexpected or repeated event for %q", who, e.VariantKey)
			}
			seen[e.VariantKey] = true
			checkEvent(t, who, e, hash, run, w, since)
		}
	}
	byVariant("a1", a1, wantShared, shared)
	byVariant("a2", a2, wantShared, shared)
	wantOther := map[string]wantEvent{storage.VariantOriginal: {"success", storage.ReasonDigestMatch, 1, other, other, n(otherBody), storage.FixityVerified}}
	byVariant("a3 (alternate)", a3, wantOther, other)
	byVariant("a4 (companion)", a4, wantOther, other)
	byVariant("a5", a5, map[string]wantEvent{}, "")
	byVariant("a6 (legacy original)", a6, map[string]wantEvent{
		storage.VariantOriginal: {"warning", storage.ReasonNoRecordedDigest, 1, legacyOrig, "", n(legacyOrigBody), storage.FixityUnverifiable},
	}, legacyOrig)

	// The event never says more than the row: the legacy rows stay unverifiable.
	for _, key := range []string{"legacy", "col"} {
		if fx := r.fixity(t, shared, key); fx.Status != storage.FixityUnverifiable || fx.SHA256 != "" {
			t.Errorf("legacy %s after the sweep: %+v", key, fx)
		}
	}
	if fx := r.fixity(t, legacyOrig, storage.VariantOriginal); fx.Status != storage.FixityUnverifiable {
		t.Errorf("legacy original promoted by the sweep: %+v", fx)
	}
	got := r.findings(t, run, shared)
	want := map[string]string{"hires": storage.FindingChecksumMismatch, "poster": storage.FindingMissingObject, "col": storage.FindingSizeMismatch}
	if len(got) != len(want) {
		t.Fatalf("findings %v, want %v", got, want)
	}
	for k, f := range want {
		if got[k] != f {
			t.Errorf("finding for %s = %q, want %q", k, got[k], f)
		}
	}
}

func eventIDs(evs []storage.AssetPreservationEvent) map[[16]byte]bool {
	out := map[[16]byte]bool{}
	for _, e := range evs {
		out[e.ID.Bytes] = true
	}
	return out
}

func TestSweepEvents_RetriedBatchAddsNothing_NewRunAndNewGenerationDo(t *testing.T) {
	r := newEventRig(t)
	ctx := context.Background()
	hash, _ := r.original(t, "retry original")
	if _, err := r.svc.PutVariant(ctx, hash, "preview", bytes.NewReader([]byte("preview gen 1")), "image/webp"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PutVariant(ctx, hash, "hires", bytes.NewReader([]byte("hires good bytes")), "image/webp"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Backend.Put(ctx, hash, "hires", bytes.NewReader([]byte("hires BAD! bytes"))); err != nil {
		t.Fatal(err)
	}
	a := r.asset(t, hash)
	b := r.asset(t, hash)

	run1 := r.run(t)
	found1 := r.sweep(t, run1)
	first := r.events(t, a, run1)
	if len(first) != 3 || len(r.events(t, b, run1)) != 3 {
		t.Fatalf("first pass: a %d events, b %d, want 3 each", len(first), len(r.events(t, b, run1)))
	}
	// The process stops after the batch's events and finding are durable
	// but before Handle's cursor checkpoint; the job is retried with the
	// same payload, so the same batch runs again.
	found2 := r.sweep(t, run1)
	retried := r.events(t, a, run1)
	if len(retried) != 3 || len(r.events(t, b, run1)) != 3 {
		t.Fatalf("after the retry: a %d events, b %d, want still 3 each", len(retried), len(r.events(t, b, run1)))
	}
	ids := eventIDs(first)
	for id := range eventIDs(retried) {
		if !ids[id] {
			t.Fatal("the retry replaced an event instead of keeping the original")
		}
	}
	if f := r.findings(t, run1, hash); len(f) != 1 || f["hires"] != storage.FindingChecksumMismatch {
		t.Fatalf("findings after the retry: %v, want one checksum_mismatch", f)
	}
	if found2 != found1 {
		t.Fatalf("the retried batch reported %d findings, the first %d: the count is one per item", found2, found1)
	}

	// A later run is a new observation: new events, the old ones kept.
	run2 := r.run(t)
	r.sweep(t, run2)
	if got := len(r.events(t, a, run2)); got != 3 {
		t.Fatalf("run 2: %d events, want 3", got)
	}
	if got := len(r.events(t, a, uuid.Nil)); got != 6 {
		t.Fatalf("history after two runs: %d events, want 6", got)
	}
	if f := r.findings(t, run2, hash); len(f) != 1 {
		t.Fatalf("run 2 findings: %v", f)
	}

	// A regeneration inside the same run is a new generation of the
	// variant, so a new event, and the generation-1 event stays.
	regen := []byte("preview gen 2, regenerated")
	if _, err := r.svc.PutVariant(ctx, hash, "preview", bytes.NewReader(regen), "image/webp"); err != nil {
		t.Fatal(err)
	}
	r.sweep(t, run2)
	evs := r.events(t, a, run2)
	if len(evs) != 4 {
		t.Fatalf("run 2 after regeneration: %d events, want 4", len(evs))
	}
	gens := map[int64]string{}
	for _, e := range evs {
		if e.VariantKey == "preview" {
			gens[e.ObservedGeneration] = deref(e.RecordedSha256)
		}
	}
	if len(gens) != 2 || gens[2] != sum(regen) || gens[1] == "" || gens[1] == gens[2] {
		t.Fatalf("preview events by generation: %v", gens)
	}
}

func TestVariantObservationOutcome_SuccessNeedsAVerifiedRecordedDigest(t *testing.T) {
	d1, d2 := strings.Repeat("1", 64), strings.Repeat("2", 64)
	found := func(o storage.VariantObservation) storage.VariantObservation {
		o.ObjectHash, o.VariantKey, o.Found, o.ObservedSize = d1, "preview", true, 4
		return o
	}
	cases := []struct {
		why             string
		obs             storage.VariantObservation
		outcome, reason string
	}{
		{"verified recorded digest the bytes match", found(storage.VariantObservation{ObservedSHA256: d1, RecordedSHA256: d1, StatusAfter: storage.FixityVerified}), "success", storage.ReasonDigestMatch},
		{"no recorded digest, unverifiable", found(storage.VariantObservation{ObservedSHA256: d1, StatusAfter: storage.FixityUnverifiable}), "warning", storage.ReasonNoRecordedDigest},
		{"a legacy original matching its address", func() storage.VariantObservation {
			o := found(storage.VariantObservation{ObservedSHA256: d1, StatusAfter: storage.FixityUnverifiable})
			o.VariantKey = storage.VariantOriginal
			return o
		}(), "warning", storage.ReasonNoRecordedDigest},
		// Fabrications: none of these may claim success.
		{"no recorded digest but 'verified'", found(storage.VariantObservation{ObservedSHA256: d1, StatusAfter: storage.FixityVerified}), "", ""},
		{"recorded digest the bytes do not match, no finding", found(storage.VariantObservation{ObservedSHA256: d2, RecordedSHA256: d1, StatusAfter: storage.FixityVerified}), "", ""},
		{"matching digest on a row left unverifiable", found(storage.VariantObservation{ObservedSHA256: d1, RecordedSHA256: d1, StatusAfter: storage.FixityUnverifiable}), "", ""},
		{"matching digest on a row left mismatch", found(storage.VariantObservation{ObservedSHA256: d1, RecordedSHA256: d1, StatusAfter: storage.FixityMismatch}), "", ""},
		{"a finding outside the fixity vocabulary", found(storage.VariantObservation{ObservedSHA256: d1, RecordedSHA256: d1, StatusAfter: storage.FixityVerified, Finding: storage.FindingOrphanObject}), "", ""},
		// Failures.
		{"read failure", storage.VariantObservation{ObjectHash: d1, VariantKey: "preview", ReadFailed: true, RecordedSHA256: d1, StatusAfter: storage.FixityVerified}, "failure", storage.ReasonReadError},
		{"missing bytes", storage.VariantObservation{ObjectHash: d1, VariantKey: "preview", StatusAfter: storage.FixityMissing}, "failure", storage.ReasonMissingObject},
		{"checksum finding", found(storage.VariantObservation{ObservedSHA256: d2, RecordedSHA256: d1, StatusAfter: storage.FixityMismatch, Finding: storage.FindingChecksumMismatch}), "failure", storage.ReasonChecksumMismatch},
		{"size finding", found(storage.VariantObservation{ObservedSHA256: d2, StatusAfter: storage.FixityUnverifiable, Finding: storage.FindingSizeMismatch}), "failure", storage.ReasonSizeMismatch},
	}
	for _, c := range cases {
		if o, r := c.obs.Outcome(); o != c.outcome || r != c.reason {
			t.Errorf("%s: Outcome() = %q/%q, want %q/%q", c.why, o, r, c.outcome, c.reason)
		}
	}
}

func TestRecordSweepObservation_FindingAndEventsAreOneTransaction_AndFabricationsAreRefused(t *testing.T) {
	r := newEventRig(t)
	ctx := context.Background()
	hash, _ := r.original(t, "rollback original")
	a := r.asset(t, hash)
	run := r.run(t)
	// A finding, then an event the table refuses (a negative generation).
	_, err := r.svc.RecordSweepObservation(ctx, run, storage.VariantObservation{
		ObjectHash: hash, VariantKey: "preview", Found: true, Generation: -1,
		ObservedSHA256: strings.Repeat("e", 64), ObservedSize: 4, RecordedSHA256: hash, StatusAfter: storage.FixityMismatch,
		Finding: storage.FindingChecksumMismatch, Detail: "planted", ObservedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("an invalid event was accepted")
	}
	if f := r.findings(t, run, hash); len(f) != 0 {
		t.Fatalf("the finding survived its failed event: %v", f)
	}
	// Fabricated successes: absent or mismatched recorded digest.
	for _, obs := range []storage.VariantObservation{
		{ObjectHash: hash, VariantKey: "preview", Found: true, Generation: 1, ObservedSHA256: hash, ObservedSize: 4, StatusAfter: storage.FixityVerified},
		{ObjectHash: hash, VariantKey: "preview", Found: true, Generation: 1, ObservedSHA256: hash, ObservedSize: 4, RecordedSHA256: strings.Repeat("e", 64), StatusAfter: storage.FixityVerified},
	} {
		if _, err := r.svc.RecordSweepObservation(ctx, run, obs); err == nil {
			t.Errorf("a fabricated success was recorded: %+v", obs)
		}
	}
	if evs := r.events(t, a, uuid.Nil); len(evs) != 0 {
		t.Fatalf("events after refused observations: %d", len(evs))
	}
	// Nothing was checked: nothing is recorded.
	if n, err := r.svc.RecordSweepObservation(ctx, run, storage.VariantObservation{
		ObjectHash: hash, VariantKey: "preview", Gone: true,
	}); err != nil || n != 0 {
		t.Fatalf("a gone observation: %d %v", n, err)
	}
}

// flakyBackend fails reads of chosen (hash, variant)s: mid-stream (the
// open succeeds, the read fails) or at open. The error text carries a
// path, as a real backend's would. "cancel" instead ends the caller's
// context during the read, which is not a read failure.
type flakyBackend struct {
	storage.Backend
	mu     sync.Mutex
	fail   map[string]string // hash/variant -> "stream" | "open" | "cancel"
	cancel context.CancelFunc
}

func (f *flakyBackend) set(hash, variant, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if mode == "" {
		delete(f.fail, hash+"/"+variant)
		return
	}
	f.fail[hash+"/"+variant] = mode
}

const flakyErrText = "read /srv/aa-storage/private/blobs: input/output error"

func (f *flakyBackend) Get(ctx context.Context, hash, variant string) (io.ReadCloser, *storage.ObjectInfo, error) {
	f.mu.Lock()
	mode, cancel := f.fail[hash+"/"+variant], f.cancel
	f.mu.Unlock()
	if mode == "cancel" {
		cancel()
		return nil, nil, ctx.Err()
	}
	if mode == "open" {
		return nil, nil, errors.New(flakyErrText)
	}
	rc, info, err := f.Backend.Get(ctx, hash, variant)
	if err != nil || mode != "stream" {
		return rc, info, err
	}
	return &brokenReader{rc: rc}, info, nil
}

type brokenReader struct {
	rc   io.ReadCloser
	read bool
}

func (b *brokenReader) Read(p []byte) (int, error) {
	if !b.read && len(p) > 0 { // some bytes, then the device fails
		b.read = true
		n, _ := b.rc.Read(p[:1])
		return n, nil
	}
	return 0, errors.New(flakyErrText)
}

func (b *brokenReader) Close() error { return b.rc.Close() }

func TestSweepEvents_ReadErrorIsAFailureEventForEveryAsset_AndTheRunStillFails(t *testing.T) {
	r := newEventRig(t)
	ctx := context.Background()
	hash, _ := r.original(t, "read error original")
	good := []byte("hires bytes the device cannot read")
	if _, err := r.svc.PutVariant(ctx, hash, "hires", bytes.NewReader(good), "image/webp"); err != nil {
		t.Fatal(err)
	}
	flaky := &flakyBackend{Backend: r.svc.Backend, fail: map[string]string{}}
	r.svc.Backend = flaky
	r.sweeper = storage.NewChecksumVerifyHandler(r.pool, r.svc, nil, nil)
	a := r.asset(t, hash)
	b := r.asset(t, hash)
	run := r.run(t)
	since := time.Now().Add(-time.Second)

	// The job starts at this object, so its first row is "hires".
	payload, _ := json.Marshal(map[string]any{"run_id": run, "cursor": hash, "cursor_variant": ""})
	handle := func() error {
		t.Helper()
		_, err := r.sweeper.Handle(ctx, &jobs.Claim{ID: uuid.New(), Type: storage.JobChecksumVerify, Payload: payload, Attempts: 1, MaxAttempts: 3})
		return err
	}
	readErrors := func(who string, asset uuid.UUID) {
		t.Helper()
		evs := r.events(t, asset, run)
		if len(evs) != 1 {
			t.Fatalf("%s: %d events, want exactly the one read_error: %+v", who, len(evs), evs)
		}
		checkEvent(t, who, evs[0], hash, run, wantEvent{"failure", storage.ReasonReadError, 1, "", sum(good), 0, storage.FixityVerified}, since)
	}

	flaky.set(hash, "hires", "stream")
	err := handle()
	if err == nil || !strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("the sweep did not fail on the read error: %v", err)
	}
	readErrors("a", a)
	readErrors("b", b)
	var status, runErr string
	if err := r.pool.QueryRow(ctx, `SELECT status, coalesce(error, '') FROM storage_sweep_runs WHERE id = $1`, run).Scan(&status, &runErr); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !strings.Contains(runErr, "input/output error") {
		t.Fatalf("run after the read error: status %q error %q, want failed with the error for the operator", status, runErr)
	}
	// The error text and its path are the operator's, never the event's.
	var leaked int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM asset_preservation_events WHERE asset_id IN ($1, $2)
		AND (to_jsonb(asset_preservation_events)::text LIKE '%input/output%' OR to_jsonb(asset_preservation_events)::text LIKE '%/srv/%')`, a, b).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("%d events carry the backend error (%v)", leaked, err)
	}
	if f := r.findings(t, run, hash); len(f) != 0 {
		t.Fatalf("a read error is not a fixity finding: %v", f)
	}

	// The job is retried with the same payload: the open now fails too.
	// Same result, so nothing new.
	flaky.set(hash, "hires", "open")
	if err := handle(); err == nil {
		t.Fatal("the retried sweep did not fail")
	}
	readErrors("a after the retry", a)
	readErrors("b after the retry", b)

	// A cancelled context is not a read failure: nothing is recorded.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, _, err := r.sweeper.VerifyChecksumBatch(cancelled, run, hash, ""); err == nil {
		t.Fatal("a cancelled batch succeeded")
	}
	readErrors("a after a cancelled batch", a)
	// Neither a lock that cannot be taken nor a context that ends during
	// the read is a read failure: nothing about the file was learned.
	if obs, err := r.svc.VerifyVariant(cancelled, hash, "hires"); err == nil || obs.ReadFailed {
		t.Fatalf("cancelled before the lock: ReadFailed=%v err=%v", obs.ReadFailed, err)
	}
	mid, stop := context.WithCancel(ctx)
	defer stop()
	flaky.mu.Lock()
	flaky.cancel = stop
	flaky.mu.Unlock()
	flaky.set(hash, "hires", "cancel")
	if obs, err := r.svc.VerifyVariant(mid, hash, "hires"); err == nil || obs.ReadFailed {
		t.Fatalf("cancelled during the read: ReadFailed=%v err=%v", obs.ReadFailed, err)
	}
	readErrors("a after the cancellations", a)

	// The device recovers: the clean read is a new event, the failure stays.
	flaky.set(hash, "hires", "")
	if _, _, _, _, err := r.sweeper.VerifyChecksumBatch(ctx, run, hash, ""); err != nil {
		t.Fatal(err)
	}
	for _, asset := range []uuid.UUID{a, b} {
		reasons := map[string]int{}
		for _, e := range r.events(t, asset, run) {
			reasons[e.VariantKey+"/"+e.OutcomeReason]++
		}
		want := map[string]int{"hires/" + storage.ReasonReadError: 1, "hires/" + storage.ReasonDigestMatch: 1, "original/" + storage.ReasonDigestMatch: 1}
		if len(reasons) != len(want) {
			t.Fatalf("history after recovery: %v, want %v", reasons, want)
		}
		for k, n := range want {
			if reasons[k] != n {
				t.Fatalf("history after recovery: %v, want %v", reasons, want)
			}
		}
	}
	if err := r.pool.QueryRow(ctx, `SELECT status FROM storage_sweep_runs WHERE id = $1`, run).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("the run's failure is no longer visible: %q %v", status, err)
	}
}

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func TestPreservationEvents_AppendOnly_OutliveTheirRun_AndConstrained(t *testing.T) {
	r := newEventRig(t)
	ctx := context.Background()
	hash, _ := r.original(t, "append-only original")
	a := r.asset(t, hash)
	run := r.run(t)
	r.sweep(t, run)
	evs := r.events(t, a, run)
	if len(evs) != 1 {
		t.Fatalf("%d events, want 1", len(evs))
	}
	id := evs[0].ID

	// Every column the insert below sets, valid as given (run-less).
	cols := []string{"asset_id", "source", "event_type", "outcome", "outcome_reason", "algorithm", "object_hash", "variant_key",
		"observed_generation", "observed_sha256", "observed_size_bytes", "recorded_sha256", "fixity_status", "agent_name"}
	other := strings.Repeat("e", 64)
	base := map[string]any{
		"asset_id": a, "source": "local", "event_type": "fixity-check", "outcome": "success", "outcome_reason": "digest_match",
		"algorithm": "sha256", "object_hash": hash, "variant_key": "preview", "observed_generation": 1,
		"observed_sha256": hash, "observed_size_bytes": 4, "recorded_sha256": hash, "fixity_status": "verified", "agent_name": "artist-alley",
	}
	noBytes := map[string]any{"algorithm": nil, "observed_sha256": nil, "observed_size_bytes": nil}
	with := func(ms ...map[string]any) map[string]any {
		out := map[string]any{}
		for _, m := range ms {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	insert := func(extraCols []string, over map[string]any) error {
		cs := append(append([]string{}, cols...), extraCols...)
		vals := make([]any, len(cs))
		ph := make([]string, len(cs))
		for i, c := range cs {
			v := base[c]
			if o, ok := over[c]; ok {
				v = o
			}
			vals[i], ph[i] = v, fmt.Sprintf("$%d", i+1)
		}
		_, err := r.pool.Exec(ctx, `INSERT INTO asset_preservation_events (`+strings.Join(cs, ", ")+`) VALUES (`+strings.Join(ph, ", ")+`)`, vals...)
		return err
	}
	refused := []struct {
		why  string
		over map[string]any
	}{
		// success must be a verified, recorded digest the bytes match
		{"success with no recorded digest", map[string]any{"recorded_sha256": nil}},
		{"success with a mismatched recorded digest", map[string]any{"recorded_sha256": other}},
		{"success on an unverifiable row", map[string]any{"fixity_status": "unverifiable"}},
		{"success on a mismatch row", map[string]any{"fixity_status": "mismatch"}},
		{"success with no observed digest", noBytes},
		{"the retired address_match reason", map[string]any{"outcome_reason": "address_match", "recorded_sha256": nil, "fixity_status": "unverifiable"}},
		{"success for a missing object", with(noBytes, map[string]any{"outcome_reason": "missing_object", "fixity_status": "missing"})},
		// warning is only no_recorded_digest, on an unverifiable row
		{"warning that claims a match", map[string]any{"outcome": "warning"}},
		{"warning with a recorded digest", map[string]any{"outcome": "warning", "outcome_reason": "no_recorded_digest", "fixity_status": "unverifiable"}},
		{"warning on a verified row", map[string]any{"outcome": "warning", "outcome_reason": "no_recorded_digest", "recorded_sha256": nil}},
		{"warning with nothing read", with(noBytes, map[string]any{"outcome": "warning", "outcome_reason": "no_recorded_digest", "recorded_sha256": nil, "fixity_status": "unverifiable"})},
		// failures carry the evidence they claim
		{"failure that claims a match", map[string]any{"outcome": "failure"}},
		{"checksum mismatch whose digests agree", map[string]any{"outcome": "failure", "outcome_reason": "checksum_mismatch", "fixity_status": "mismatch"}},
		{"checksum mismatch on a derived row with nothing recorded", map[string]any{"outcome": "failure", "outcome_reason": "checksum_mismatch", "recorded_sha256": nil, "observed_sha256": other, "fixity_status": "unverifiable"}},
		{"checksum mismatch of an original that matches its address", map[string]any{"outcome": "failure", "outcome_reason": "checksum_mismatch", "recorded_sha256": nil, "variant_key": "original", "fixity_status": "unverifiable"}},
		{"checksum mismatch of an original matching both its address and its recorded digest", map[string]any{"outcome": "failure", "outcome_reason": "checksum_mismatch", "variant_key": "original", "fixity_status": "mismatch"}},
		{"missing object with a digest", map[string]any{"outcome": "failure", "outcome_reason": "missing_object", "fixity_status": "missing"}},
		{"missing object on a verified row", with(noBytes, map[string]any{"outcome": "failure", "outcome_reason": "missing_object"})},
		{"read error with a digest", map[string]any{"outcome": "failure", "outcome_reason": "read_error"}},
		{"an outcome outside the vocabulary", map[string]any{"outcome": "passed"}},
		{"free-text reason", map[string]any{"outcome_reason": "looked fine to me"}},
		// shape
		{"a digest without its algorithm", map[string]any{"algorithm": nil}},
		{"a digest without its size", map[string]any{"observed_size_bytes": nil}},
		{"an algorithm other than sha256", map[string]any{"algorithm": "md5"}},
		{"a storage path for the object", map[string]any{"object_hash": "/srv/storage/" + hash[:8]}},
		{"an uppercase digest", map[string]any{"observed_sha256": strings.ToUpper(hash), "recorded_sha256": strings.ToUpper(hash)}},
		{"a fixity status outside the vocabulary", map[string]any{"fixity_status": "trusted"}},
		{"a negative generation", map[string]any{"observed_generation": -1}},
		{"an empty agent", map[string]any{"agent_name": ""}},
		{"an empty variant", map[string]any{"variant_key": ""}},
		// only this instance's own fixity checks, for now
		{"a format-identification event", map[string]any{"event_type": "format-identification"}},
		{"a transfer event", map[string]any{"event_type": "transfer"}},
		{"an event type outside spec §17.4", map[string]any{"event_type": "checksum"}},
		{"a remote claim", map[string]any{"source": "remote_claim"}},
		{"a source outside the vocabulary", map[string]any{"source": "peer"}},
	}
	for _, c := range refused {
		if err := insert(nil, c.over); err == nil || sqlState(err) != "23514" {
			t.Errorf("%s: err %v (sqlstate %q), want a check violation", c.why, err, sqlState(err))
		}
	}
	// Honest rows of every reason are accepted (run-less).
	for _, ok := range []map[string]any{
		nil,
		{"outcome": "warning", "outcome_reason": "no_recorded_digest", "recorded_sha256": nil, "fixity_status": "unverifiable"},
		{"outcome": "failure", "outcome_reason": "checksum_mismatch", "observed_sha256": other, "fixity_status": "mismatch"},
		{"outcome": "failure", "outcome_reason": "checksum_mismatch", "recorded_sha256": nil, "variant_key": "original", "observed_sha256": other, "fixity_status": "unverifiable"},
		// an original whose recorded digest was rewritten to match its corrupted bytes
		{"outcome": "failure", "outcome_reason": "checksum_mismatch", "recorded_sha256": other, "variant_key": "original", "observed_sha256": other, "fixity_status": "mismatch"},
		{"outcome": "failure", "outcome_reason": "size_mismatch", "recorded_sha256": nil, "fixity_status": "unverifiable"},
		with(noBytes, map[string]any{"outcome": "failure", "outcome_reason": "missing_object", "fixity_status": "missing"}),
		with(noBytes, map[string]any{"outcome": "failure", "outcome_reason": "read_error"}),
	} {
		if err := insert(nil, ok); err != nil {
			t.Errorf("an honest event %v was refused: %v", ok, err)
		}
	}
	const honest = 1 + 8
	if got := len(r.events(t, a, uuid.Nil)); got != honest {
		t.Fatalf("%d events, want %d", got, honest)
	}
	// A run snapshot must come from a run that exists.
	for _, c := range []struct {
		why  string
		over map[string]any
	}{
		{"a snapshot with no live run", map[string]any{"sweep_run_id": uuid.New(), "sweep_run_ref": nil}},
		{"a snapshot of a run that does not exist", func() map[string]any { g := uuid.New(); return map[string]any{"sweep_run_id": g, "sweep_run_ref": g} }()},
		{"a snapshot that disagrees with its live run", map[string]any{"sweep_run_id": uuid.New(), "sweep_run_ref": run}},
	} {
		if err := insert([]string{"sweep_run_id", "sweep_run_ref"}, c.over); err == nil {
			t.Errorf("%s: accepted", c.why)
		}
	}

	appendOnly := func(what, q string, args ...any) {
		t.Helper()
		_, err := r.pool.Exec(ctx, q, args...)
		if err == nil || sqlState(err) != "23000" || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err %v (sqlstate %q), want the append-only refusal", what, err, sqlState(err))
		}
	}
	appendOnly("UPDATE", `UPDATE asset_preservation_events SET outcome = outcome WHERE id = $1`, id)
	appendOnly("UPDATE of every row", `UPDATE asset_preservation_events SET fixity_status = 'verified' WHERE asset_id = $1`, a)
	appendOnly("clearing the live run while it exists", `UPDATE asset_preservation_events SET sweep_run_ref = NULL WHERE id = $1`, id)
	appendOnly("rewriting the run snapshot", `UPDATE asset_preservation_events SET sweep_run_id = NULL, sweep_run_ref = NULL WHERE id = $1`, id)
	appendOnly("DELETE of a sweep event", `DELETE FROM asset_preservation_events WHERE id = $1`, id)
	appendOnly("DELETE of a run-less event", `DELETE FROM asset_preservation_events WHERE asset_id = $1 AND sweep_run_id IS NULL`, a)
	if got := len(r.events(t, a, uuid.Nil)); got != honest {
		t.Fatalf("after the refused writes: %d events, want %d", got, honest)
	}

	// Deleting the run (retention) keeps the history: the snapshot stays,
	// only the live reference clears, and the event is still append-only.
	before := evs[0]
	r.exec(t, `DELETE FROM storage_sweep_runs WHERE id = $1`, run)
	kept := r.events(t, a, run)
	if len(kept) != 1 {
		t.Fatalf("after deleting the run: %d of its events left, want 1", len(kept))
	}
	after := kept[0]
	if after.SweepRunRef.Valid || after.ID != before.ID || after.OutcomeReason != before.OutcomeReason || after.OccurredAt != before.OccurredAt {
		t.Fatalf("after deleting the run: %+v, was %+v", after, before)
	}
	appendOnly("UPDATE after the run is gone", `UPDATE asset_preservation_events SET outcome = outcome WHERE id = $1`, id)
	appendOnly("DELETE after the run is gone", `DELETE FROM asset_preservation_events WHERE id = $1`, id)
	if got := len(r.events(t, a, uuid.Nil)); got != honest {
		t.Fatalf("history after the run's deletion: %d events, want %d", got, honest)
	}
	// Erasing the asset erases its history.
	r.exec(t, `DELETE FROM assets WHERE id = $1`, a)
	var left int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM asset_preservation_events WHERE asset_id = $1`, a).Scan(&left); err != nil || left != 0 {
		t.Fatalf("after deleting the asset: %d events left (%v)", left, err)
	}
}

// handleToCompletion drives run's checksum sweep through Handle, from
// the (cursor, "") resume point, taking each batch's re-enqueued
// successor off the jobs table, until the run completes.
func (r *eventRig) handleToCompletion(t *testing.T, h *storage.SweepHandler, run uuid.UUID, cursor string) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = r.pool.Exec(context.Background(), `DELETE FROM jobs WHERE type = $1 AND payload->>'run_id' = $2`, string(storage.JobChecksumVerify), run.String())
	})
	payload, _ := json.Marshal(map[string]any{"run_id": run, "cursor": cursor, "cursor_variant": ""})
	for i := 0; i < 200; i++ {
		out, err := h.Handle(ctx, &jobs.Claim{ID: uuid.New(), Type: storage.JobChecksumVerify, Payload: payload, Attempts: 1, MaxAttempts: 3})
		if err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
		var res struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatal(err)
		}
		if res.Status == "completed" {
			return
		}
		if err := r.pool.QueryRow(ctx, `DELETE FROM jobs WHERE id = (SELECT id FROM jobs WHERE type = $1 AND payload->>'run_id' = $2 ORDER BY enqueued_at DESC LIMIT 1) RETURNING payload`,
			string(storage.JobChecksumVerify), run.String()).Scan(&payload); err != nil {
			t.Fatalf("batch %d: next job: %v", i, err)
		}
	}
	t.Fatal("the sweep did not complete")
}

func TestSweepEvents_OriginalWhoseDigestWasRewrittenToItsCorruptBytesIsAChecksumMismatch_AndTheRunCompletes(t *testing.T) {
	r := newEventRig(t)
	ctx := context.Background()
	hash, body := r.original(t, "original corrupted with its digest")
	a := r.asset(t, hash)
	b := r.asset(t, hash)
	// The bytes change AND the row's digest and size are rewritten to
	// describe them, consistently, while object_hash still names the
	// original content. Only the address can tell.
	evil := append([]byte("CORRUPT "), body...)
	if _, err := r.svc.Backend.Put(ctx, hash, storage.VariantOriginal, bytes.NewReader(evil)); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE storage_variants SET content_sha256 = $2, content_size_bytes = $3, size_bytes = $3, digest_source = 'recheck'
		WHERE object_hash = $1 AND variant_key = 'original'`, hash, sum(evil), len(evil))
	if fx := r.fixity(t, hash, storage.VariantOriginal); fx.Status != storage.FixityVerified || fx.SHA256 != sum(evil) {
		t.Fatalf("seeded row: %+v", fx)
	}

	jobsSvc := jobs.NewService(r.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	h := storage.NewChecksumVerifyHandler(r.pool, r.svc, jobsSvc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run := r.run(t)
	since := time.Now().Add(-time.Second)
	r.handleToCompletion(t, h, run, hash)

	for who, asset := range map[string]uuid.UUID{"a": a, "b": b} {
		evs := r.events(t, asset, run)
		if len(evs) != 1 {
			t.Fatalf("%s: %d events, want 1: %+v", who, len(evs), evs)
		}
		checkEvent(t, who, evs[0], hash, run, wantEvent{"failure", storage.ReasonChecksumMismatch, 1, sum(evil), sum(evil), int64(len(evil)), storage.FixityMismatch}, since)
	}
	if f := r.findings(t, run, hash); len(f) != 1 || f[storage.VariantOriginal] != storage.FindingChecksumMismatch {
		t.Fatalf("findings: %v, want the original's checksum_mismatch", f)
	}
	if fx := r.fixity(t, hash, storage.VariantOriginal); fx.Status != storage.FixityMismatch {
		t.Fatalf("row after the sweep: %+v, want mismatch", fx)
	}
	var status string
	var found int64
	if err := r.pool.QueryRow(ctx, `SELECT status, findings_count FROM storage_sweep_runs WHERE id = $1`, run).Scan(&status, &found); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || found < 1 {
		t.Fatalf("run: status %q findings_count %d, want completed with the finding counted", status, found)
	}
}

func TestRecordSweepObservation_AnAssetDeletedMidObservationGetsNoEvent_AndTheSweepGoesOn(t *testing.T) {
	for _, mode := range []string{"committed", "in flight"} {
		t.Run(mode, func(t *testing.T) {
			r := newEventRig(t)
			ctx := context.Background()
			hash, _ := r.original(t, "shared bytes, one owner leaving "+mode)
			gone := r.asset(t, hash)
			kept := r.asset(t, hash)
			run := r.run(t)

			committed := make(chan error, 1)
			fired := false
			// Between the reference lookup (both assets listed) and the
			// inserts, the asset is hard-deleted: committed outright, or
			// deleted and committed only while the insert waits on its row.
			r.svc.SetEventAfterRefsHook(func(h string) {
				if h != hash || fired {
					return
				}
				fired = true
				if mode == "committed" {
					_, err := r.pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, gone)
					committed <- err
					return
				}
				tx, err := r.pool.Begin(ctx)
				if err != nil {
					committed <- err
					return
				}
				if _, err := tx.Exec(ctx, `DELETE FROM assets WHERE id = $1`, gone); err != nil {
					_ = tx.Rollback(ctx)
					committed <- err
					return
				}
				go func() {
					time.Sleep(300 * time.Millisecond)
					committed <- tx.Commit(ctx)
				}()
			})
			t.Cleanup(func() { r.svc.SetEventAfterRefsHook(nil) })

			if _, _, _, _, err := r.sweeper.VerifyChecksumBatch(ctx, run, hash, ""); err != nil {
				t.Fatalf("the sweep failed on a deleted asset: %v", err)
			}
			if !fired {
				t.Fatal("the seam never ran")
			}
			if err := <-committed; err != nil {
				t.Fatalf("delete: %v", err)
			}
			var left int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM asset_preservation_events WHERE asset_id = $1`, gone).Scan(&left); err != nil || left != 0 {
				t.Fatalf("the deleted asset has %d events (%v)", left, err)
			}
			evs := r.events(t, kept, run)
			if len(evs) != 1 || evs[0].OutcomeReason != storage.ReasonDigestMatch {
				t.Fatalf("surviving asset: %+v, want one digest_match", evs)
			}
			// The retry is still idempotent for the survivor.
			if _, _, _, _, err := r.sweeper.VerifyChecksumBatch(ctx, run, hash, ""); err != nil {
				t.Fatal(err)
			}
			if got := len(r.events(t, kept, run)); got != 1 {
				t.Fatalf("surviving asset after the retry: %d events, want 1", got)
			}
		})
	}
}
