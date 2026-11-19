// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1598: the operator's read of an asset's preservation history
// (AdminHandler.ListPreservationEvents), pinned on a real database.
//
//   - pages are newest first by (occurred_at, id): events sharing one
//     timestamp keep a single fixed order, every event appears exactly
//     once across the pages, the last page has no cursor, and the
//     total counts the whole history;
//   - a newer event written while an operator pages back does not
//     shift, repeat or skip the older pages (keyset, not offset);
//   - a cursor belongs to one asset: replayed against another, or
//     malformed, it is refused, never silently restarted;
//   - two assets sharing deduplicated bytes each list the sweep's
//     events; an asset with no events is an empty page; a missing asset
//     is not found; a soft-deleted asset keeps its history;
//   - deleting the sweep run keeps the event and its run snapshot.

package storage_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// plantEvents writes n honest success events for asset in run, the
// first `same` of them at one shared timestamp and the rest one second
// apart, each on its own variant so they are distinct events.
func (r *eventRig) plantEvents(t *testing.T, asset, run uuid.UUID, hash string, base time.Time, n, same int) {
	t.Helper()
	for i := 0; i < n; i++ {
		at := base
		if i >= same {
			at = base.Add(time.Duration(i) * time.Second)
		}
		alg, size := "sha256", int64(10+i)
		if _, err := r.q.InsertFixityEvent(context.Background(), storage.InsertFixityEventParams{
			AssetID:            pgtype.UUID{Bytes: asset, Valid: true},
			OccurredAt:         pgtype.Timestamptz{Time: at, Valid: true},
			Outcome:            "success",
			OutcomeReason:      storage.ReasonDigestMatch,
			Algorithm:          &alg,
			ObjectHash:         hash,
			VariantKey:         fmt.Sprintf("preview/%02d", i),
			ObservedGeneration: 1,
			ObservedSha256:     &hash,
			ObservedSizeBytes:  &size,
			RecordedSha256:     &hash,
			FixityStatus:       storage.FixityVerified,
			SweepRunID:         pgtype.UUID{Bytes: run, Valid: true},
			AgentName:          storage.FixityAgentName,
		}); err != nil {
			t.Fatalf("plant event %d: %v", i, err)
		}
	}
}

// wantOrder is the history order computed independently of the query:
// every event of the asset, sorted by (occurred_at, id) descending.
func (r *eventRig) wantOrder(t *testing.T, asset uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT id, occurred_at FROM asset_preservation_events WHERE asset_id = $1`, asset)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type ev struct {
		id uuid.UUID
		at time.Time
	}
	var evs []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.id, &e.at); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	sort.Slice(evs, func(i, j int) bool {
		if !evs[i].at.Equal(evs[j].at) {
			return evs[i].at.After(evs[j].at)
		}
		return strings.Compare(evs[i].id.String(), evs[j].id.String()) > 0
	})
	out := make([]uuid.UUID, len(evs))
	for i, e := range evs {
		out[i] = e.id
	}
	return out
}

// pageAll walks the whole history `limit` events at a time.
func pageAll(t *testing.T, a *storage.AdminHandler, asset uuid.UUID, limit int32) ([]uuid.UUID, int, int64) {
	t.Helper()
	var ids []uuid.UUID
	cursor, pages := "", 0
	var total int64 = -1
	for pages < 100 {
		page, err := a.ListPreservationEvents(context.Background(), asset, cursor, limit)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if total >= 0 && page.Total != total {
			t.Fatalf("total moved between pages: %d then %d", total, page.Total)
		}
		total = page.Total
		if len(page.Items) > int(limit) {
			t.Fatalf("page %d has %d items, limit %d", pages, len(page.Items), limit)
		}
		for _, e := range page.Items {
			ids = append(ids, e.ID)
		}
		if page.NextCursor == "" {
			return ids, pages, total
		}
		cursor = page.NextCursor
	}
	t.Fatal("history did not end")
	return nil, 0, 0
}

func TestPreservationHistory_PagesNewestFirstAcrossEqualTimestamps(t *testing.T) {
	r := newEventRig(t)
	admin := storage.NewAdminHandler(r.pool)
	hash, _ := r.original(t, "history paging")
	asset := r.asset(t, hash)
	run := r.run(t)
	base := time.Date(2026, 11, 1, 12, 0, 0, 123456000, time.UTC) // microsecond precision, as stored
	r.plantEvents(t, asset, run, hash, base, 7, 4)                // 4 share one timestamp

	want := r.wantOrder(t, asset)
	if len(want) != 7 {
		t.Fatalf("planted %d events, want 7", len(want))
	}
	for _, limit := range []int32{1, 2, 3, 7, 50} {
		got, pages, total := pageAll(t, admin, asset, limit)
		if total != 7 {
			t.Fatalf("limit %d: total %d, want 7", limit, total)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("limit %d: order\n got %v\nwant %v", limit, got, want)
		}
		if wantPages := (7 + int(limit) - 1) / int(limit); pages != wantPages {
			t.Fatalf("limit %d: %d pages, want %d", limit, pages, wantPages)
		}
	}
	// limit is clamped: 0 and too-large ask for the default page.
	for _, limit := range []int32{0, -1, 500} {
		page, err := admin.ListPreservationEvents(context.Background(), asset, "", limit)
		if err != nil || len(page.Items) != 7 || page.NextCursor != "" {
			t.Fatalf("limit %d: %d items, cursor %q, %v", limit, len(page.Items), page.NextCursor, err)
		}
	}
	// The fields are the event's own, safe file reference included.
	page, err := admin.ListPreservationEvents(context.Background(), asset, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	e := page.Items[0]
	if e.Source != "local" || e.EventType != "fixity-check" || e.Outcome != "success" || e.OutcomeReason != storage.ReasonDigestMatch ||
		e.ObjectHash != hash || !strings.HasPrefix(e.VariantKey, "preview/") || e.Generation != 1 || deref(e.ObservedSHA256) != hash ||
		deref(e.RecordedSHA256) != hash || deref(e.Algorithm) != "sha256" || e.FixityStatus != storage.FixityVerified ||
		e.SweepRunID == nil || *e.SweepRunID != run || !e.SweepRunPresent || e.AgentName != storage.FixityAgentName {
		t.Fatalf("event fields: %+v", e)
	}
}

func TestPreservationHistory_ANewerEventDoesNotShiftOlderPages(t *testing.T) {
	r := newEventRig(t)
	admin := storage.NewAdminHandler(r.pool)
	ctx := context.Background()
	hash, _ := r.original(t, "history drift")
	asset := r.asset(t, hash)
	run := r.run(t)
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	r.plantEvents(t, asset, run, hash, base, 6, 3)
	before := r.wantOrder(t, asset)

	first, err := admin.ListPreservationEvents(ctx, asset, "", 2)
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	// A sweep writes a newer event while the operator reads page one.
	run2 := r.run(t)
	r.plantEvents(t, asset, run2, hash, base.Add(time.Hour), 1, 1)

	var rest []uuid.UUID
	cursor := first.NextCursor
	for cursor != "" {
		page, err := admin.ListPreservationEvents(ctx, asset, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Items {
			rest = append(rest, e.ID)
		}
		cursor = page.NextCursor
	}
	got := append([]uuid.UUID{first.Items[0].ID, first.Items[1].ID}, rest...)
	if fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("pages drifted after a newer event:\n got %v\nwant %v", got, before)
	}
	// The newer event is at the head of a fresh read.
	head, err := admin.ListPreservationEvents(ctx, asset, "", 1)
	if err != nil || head.Total != 7 || head.Items[0].OccurredAt.Before(base.Add(time.Hour)) {
		t.Fatalf("fresh read: %+v %v", head, err)
	}
}

func TestPreservationHistory_SharedBytesEmptyMissingDeletedAndCursorBinding(t *testing.T) {
	r := newEventRig(t)
	admin := storage.NewAdminHandler(r.pool)
	ctx := context.Background()
	hash, _ := r.original(t, "history shared bytes")
	a := r.asset(t, hash)
	b := r.asset(t, hash)
	empty := r.asset(t, "")
	run := r.run(t)
	r.sweep(t, run)

	// Deduplicated bytes: both assets list the sweep's event.
	for who, asset := range map[string]uuid.UUID{"a": a, "b": b} {
		page, err := admin.ListPreservationEvents(ctx, asset, "", 50)
		if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ObjectHash != hash || page.Items[0].VariantKey != storage.VariantOriginal {
			t.Fatalf("%s: %+v %v", who, page, err)
		}
	}
	// An asset with no events is an empty page, not an error.
	page, err := admin.ListPreservationEvents(ctx, empty, "", 50)
	if err != nil || page.Total != 0 || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("empty asset: %+v %v", page, err)
	}
	// A missing asset is not found.
	if _, err := admin.ListPreservationEvents(ctx, uuid.New(), "", 50); !errors.Is(err, storage.ErrHistoryAssetNotFound) {
		t.Fatalf("missing asset: %v", err)
	}

	// A cursor belongs to one asset, and a malformed one is refused.
	base := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	r.plantEvents(t, a, run, hash, base, 3, 1)
	pa, err := admin.ListPreservationEvents(ctx, a, "", 1)
	if err != nil || pa.NextCursor == "" {
		t.Fatalf("page of a: %+v %v", pa, err)
	}
	if _, err := admin.ListPreservationEvents(ctx, b, pa.NextCursor, 1); !errors.Is(err, storage.ErrHistoryCursor) {
		t.Fatalf("a's cursor replayed on b: %v", err)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{
		"not base64!",
		enc("h1:" + a.String()),
		enc("h0:" + a.String() + ":1:" + uuid.New().String()),
		enc("h1:" + a.String() + ":soon:" + uuid.New().String()),
		enc("h1:" + a.String() + ":1:not-a-uuid"),
		enc("h1:not-a-uuid:1:" + uuid.New().String()),
	} {
		if _, err := admin.ListPreservationEvents(ctx, a, bad, 1); !errors.Is(err, storage.ErrHistoryCursor) {
			t.Errorf("cursor %q: %v, want refused", bad, err)
		}
	}

	// A soft-deleted asset keeps its history.
	r.exec(t, `UPDATE assets SET deleted_at = now() WHERE id = $1`, b)
	if page, err := admin.ListPreservationEvents(ctx, b, "", 50); err != nil || page.Total != 1 {
		t.Fatalf("soft-deleted asset: %+v %v", page, err)
	}

	// Deleting the run keeps the event and its run snapshot.
	r.exec(t, `DELETE FROM storage_sweep_runs WHERE id = $1`, run)
	page, err = admin.ListPreservationEvents(ctx, b, "", 50)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("after run deletion: %+v %v", page, err)
	}
	if e := page.Items[0]; e.SweepRunID == nil || *e.SweepRunID != run || e.SweepRunPresent {
		t.Fatalf("after run deletion: run %v present %v, want the snapshot and not present", e.SweepRunID, e.SweepRunPresent)
	}
}
