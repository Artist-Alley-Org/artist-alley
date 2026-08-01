// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the two silent drops in applyAssetFields (#807).
//
// The seeder used to throw a field value away in two places with one
// bare `continue` each: no definition for the code, and a value the
// declared type could not coerce. Both produced no output at all, so a
// malformed value and a value that was never in the manifest were
// indistinguishable — the seed reported success and the field was
// simply absent. The defect this guards is therefore NOT "a value was
// dropped"; dropping is legitimate. It is "a value was dropped and
// nothing said so".
//
// So every assertion below is about the WARNING, not about the drop:
// a rejected value that logs nothing is the regression. The one
// exception is the bare-date case, which asserts the stored value_date
// in the database — asserting the absence of a warning would pass just
// as happily if the value had been dropped for some other reason.

package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/logging"
)

// captureLogger returns a logger writing JSON records into buf, so a
// test can assert on the message AND the attributes an operator would
// actually grep for.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type logRecord struct {
	Msg   string `json:"msg"`
	Code  string `json:"code"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// recordsWithMsg pulls every JSON log line carrying the given msg.
func recordsWithMsg(t *testing.T, buf *bytes.Buffer, msg string) []logRecord {
	t.Helper()
	var out []logRecord
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec logRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON (%v): %s", err, line)
		}
		if rec.Msg == msg {
			out = append(out, rec)
		}
	}
	return out
}

// badValues: one value per declared field type that the type genuinely
// cannot accept. All eleven types fieldValueParams knows are covered —
// several of them can only be rejected by a JSON null, which is exactly
// what a manifest carrying `"version": null` produces.
func badValues() []struct {
	fieldType string
	raw       any
	why       string
} {
	return []struct {
		fieldType string
		raw       any
		why       string
	}{
		// The five string-backed types coerce anything non-nil, so a
		// JSON null is the only value they can refuse.
		{"text", nil, "JSON null"},
		{"longtext", nil, "JSON null"},
		{"rich_text", nil, "JSON null"},
		{"select", nil, "JSON null"},
		{"tree", nil, "JSON null"},
		// A quoted number is the classic manifest slip.
		{"number", "42", "a number as a JSON string"},
		{"boolean", nil, "JSON null"},
		// Neither a non-date string nor a US-style date parses.
		{"date", "not-a-date", "unparseable date"},
		{"date", "03/14/2026", "US-style date"},
		// datetime stays strict: a bare calendar date is a defect.
		{"datetime", "2026-03-14", "a bare date on a datetime field"},
		{"multi_select", []any{}, "an empty array"},
		{"multi_select", []any{nil, nil}, "an array of nulls"},
		{"reference", "not-a-uuid", "an unparseable UUID"},
		// An unknown type name falls through to the text branch, which
		// still refuses null — so even a typo'd type reports its drop.
		{"nosuchtype", nil, "JSON null on an unknown type"},
	}
}

// TestFieldValueParams_RejectsBadValues pins the coercion half: every
// value below must be REFUSED, so applyAssetFields reaches its warn.
func TestFieldValueParams_RejectsBadValues(t *testing.T) {
	for _, c := range badValues() {
		t.Run(c.fieldType+"/"+c.why, func(t *testing.T) {
			if _, ok := fieldValueParams(c.fieldType, c.raw); ok {
				t.Fatalf("fieldValueParams(%q, %#v) ACCEPTED %s — "+
					"applyAssetFields would insert it instead of warning", c.fieldType, c.raw, c.why)
			}
		})
	}
}

// TestApplyAssetFields_WarnsOnRejectedValue is the regression this
// issue exists to prevent: one warning per dropped value, naming the
// code, the declared type and the offending value.
func TestApplyAssetFields_WarnsOnRejectedValue(t *testing.T) {
	for _, c := range badValues() {
		t.Run(c.fieldType+"/"+c.why, func(t *testing.T) {
			var buf bytes.Buffer
			r := NewRunner(nil, nil, Options{Logger: captureLogger(&buf)})
			// A definition EXISTS for the code — so a drop here can
			// only be the value, never the catalogue.
			code := "fld_" + c.fieldType
			r.fields[code] = fieldMeta{id: pgtype.UUID{Bytes: uuid.New(), Valid: true}, typ: c.fieldType}

			// nil pool: reaching the insert would panic, which is
			// itself the assertion that the value was refused.
			if err := r.applyAssetFields(context.Background(),
				pgtype.UUID{Bytes: uuid.New(), Valid: true},
				map[string]any{code: c.raw}); err != nil {
				t.Fatalf("applyAssetFields: %v", err)
			}

			recs := recordsWithMsg(t, &buf, "seed.field.value_rejected")
			if len(recs) != 1 {
				t.Fatalf("dropping %s on a %q field logged %d seed.field.value_rejected "+
					"records, want exactly 1. A drop nobody hears is the defect (#807). Log:\n%s",
					c.why, c.fieldType, len(recs), buf.String())
			}
			if recs[0].Code != code {
				t.Errorf("warning names code %q, want %q", recs[0].Code, code)
			}
			if recs[0].Type != c.fieldType {
				t.Errorf("warning names type %q, want %q", recs[0].Type, c.fieldType)
			}
			if recs[0].Value == "" {
				t.Errorf("warning carried no value attribute — the operator cannot tell " +
					"which manifest entry is wrong")
			}
			// A rejected value must NOT be reported as an unknown code:
			// the two failures have different fixes.
			if got := recordsWithMsg(t, &buf, "seed.field.unknown_code"); len(got) != 0 {
				t.Errorf("a bad VALUE was reported as an unknown CODE (%d records) — "+
					"the two drops must stay distinguishable", len(got))
			}
			if n := r.fieldDrops.total(dropValueRejected); n != 1 {
				t.Errorf("tally counted %d value_rejected drops, want 1", n)
			}
		})
	}
}

// TestApplyAssetFields_WarnsOnUnknownCode covers the other half — the
// branch every asset in both current manifests hits, six times over,
// until #808 lands the missing definitions.
func TestApplyAssetFields_WarnsOnUnknownCode(t *testing.T) {
	var buf bytes.Buffer
	r := NewRunner(nil, nil, Options{Logger: captureLogger(&buf)})
	assetID := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	if err := r.applyAssetFields(context.Background(), assetID, map[string]any{
		"capture_date": "2026-03-14",
	}); err != nil {
		t.Fatalf("applyAssetFields: %v", err)
	}

	recs := recordsWithMsg(t, &buf, "seed.field.unknown_code")
	if len(recs) != 1 {
		t.Fatalf("an undefined code logged %d seed.field.unknown_code records, want 1. Log:\n%s",
			len(recs), buf.String())
	}
	if recs[0].Code != "capture_date" {
		t.Errorf("warning names code %q, want capture_date", recs[0].Code)
	}
	if got := recordsWithMsg(t, &buf, "seed.field.value_rejected"); len(got) != 0 {
		t.Errorf("an unknown CODE was reported as a bad VALUE (%d records)", len(got))
	}
	if n := r.fieldDrops.total(dropUnknownCode); n != 1 {
		t.Errorf("tally counted %d unknown_code drops, want 1", n)
	}
}

// TestApplyAssetFields_UnknownCodeIsNotFatal is the constraint that
// keeps CI alive until #808: both manifests carry six codes with no
// definition, so warn-only is load-bearing, not a nicety.
func TestApplyAssetFields_UnknownCodeIsNotFatal(t *testing.T) {
	r := NewRunner(nil, nil, Options{Logger: logging.Setup("error", "text")})
	vals := map[string]any{}
	for _, c := range []string{
		"production_notes", "usage_rights", "capture_date",
		"ingested_at", "production_area", "derived_from",
	} {
		vals[c] = "whatever"
	}
	if err := r.applyAssetFields(context.Background(),
		pgtype.UUID{Bytes: uuid.New(), Valid: true}, vals); err != nil {
		t.Fatalf("six undefined codes returned an error (%v) — that breaks the seed, "+
			"and therefore CI, against today's dataset. Warn only (#807).", err)
	}
	if n := r.fieldDrops.total(dropUnknownCode); n != 6 {
		t.Errorf("tally counted %d unknown_code drops, want 6", n)
	}
}

// TestFieldDropTally_SuppressesRepeatsAndSummarises: a misconfigured
// field is ~1,900 identical lines against site_a, which buries every
// other warning the run produced. Detail warnings are capped; the
// summary carries the true totals, and it is the part an operator reads.
func TestFieldDropTally_SuppressesRepeatsAndSummarises(t *testing.T) {
	var buf bytes.Buffer
	r := NewRunner(nil, nil, Options{Logger: captureLogger(&buf)})
	assetID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	for i := 0; i < 50; i++ {
		if err := r.applyAssetFields(context.Background(), assetID, map[string]any{
			"production_area": "audio-sfx",
			"capture_date":    "2026-01-01T00:00:00Z",
		}); err != nil {
			t.Fatalf("applyAssetFields: %v", err)
		}
	}

	recs := recordsWithMsg(t, &buf, "seed.field.unknown_code")
	if want := 2 * fieldDropLogLimit; len(recs) != want {
		t.Errorf("emitted %d detail warnings for 100 drops across 2 codes, want %d "+
			"(fieldDropLogLimit=%d per code)", len(recs), want, fieldDropLogLimit)
	}
	if n := r.fieldDrops.total(dropUnknownCode); n != 100 {
		t.Fatalf("tally counted %d drops, want 100 — suppression must not lose the count", n)
	}
	if got := r.fieldDrops.offenders(dropUnknownCode, 10); got != "capture_date=50, production_area=50" {
		t.Errorf("offenders() = %q, want %q", got, "capture_date=50, production_area=50")
	}

	buf.Reset()
	r.logFieldDrops()
	sum := recordsWithMsg(t, &buf, "seed.field.drops")
	if len(sum) != 1 {
		t.Fatalf("logFieldDrops emitted %d summary records, want 1. Log:\n%s", len(sum), buf.String())
	}
	if !strings.Contains(buf.String(), `"unknown_code":100`) {
		t.Errorf("summary does not report the 100 drops: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "production_area=50") {
		t.Errorf("summary does not name the offending codes: %s", buf.String())
	}
}

// TestFieldDropTally_SummaryAlwaysLogged: zeros are the evidence the
// check ran at all.
func TestFieldDropTally_SummaryAlwaysLogged(t *testing.T) {
	var buf bytes.Buffer
	r := NewRunner(nil, nil, Options{Logger: captureLogger(&buf)})
	r.logFieldDrops()
	if len(recordsWithMsg(t, &buf, "seed.field.drops")) != 1 {
		t.Fatalf("a clean run logged no summary. Log:\n%s", buf.String())
	}
}

// ---------------------------------------------------------------------
// Round-trip: a bare date must reach the DATABASE
// ---------------------------------------------------------------------

// TestApplyAssetFields_BareDateRoundTrips drives the real insert and
// reads value_date back out. Asserting "no warning fired" would pass
// just as happily if the value had been dropped for some other reason,
// which is the whole failure mode #807 is about — so this asserts the
// stored value.
//
// Skips (does not fail) when AA_DB_PASSWORD is unset, matching the
// other seed integration tests.
func TestApplyAssetFields_BareDateRoundTrips(t *testing.T) {
	pool := openCompanionTestPool(t)
	ctx := context.Background()

	var buf bytes.Buffer
	r := NewRunner(pool, nil, Options{Logger: captureLogger(&buf)})
	assetID := newCompanionTestAsset(t, pool)

	cases := []struct {
		name, ftype, raw, want string
	}{
		{"date_bare", "date", "2026-03-14", "2026-03-14T00:00:00Z"},
		{"date_rfc3339", "date", "2026-03-14T09:30:00Z", "2026-03-14T09:30:00Z"},
		{"datetime_rfc3339", "datetime", "2026-03-14T09:30:00Z", "2026-03-14T09:30:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code := "aa807_" + c.name
			fieldID := newDropTestField(t, pool, code, c.ftype)
			r.fields[code] = fieldMeta{id: fieldID, typ: c.ftype}

			if err := r.applyAssetFields(ctx, assetID, map[string]any{code: c.raw}); err != nil {
				t.Fatalf("applyAssetFields: %v", err)
			}

			var got time.Time
			err := pool.QueryRow(ctx,
				`SELECT value_date FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
				assetID, fieldID).Scan(&got)
			if err != nil {
				t.Fatalf("no asset_field_value row for %s=%q — the value was DROPPED (%v). Log:\n%s",
					c.ftype, c.raw, err, buf.String())
			}
			if s := got.UTC().Format(time.RFC3339); s != c.want {
				t.Errorf("%s field seeded with %q stored %s, want %s", c.ftype, c.raw, s, c.want)
			}
		})
	}
}

// newDropTestField inserts a throwaway field definition and cleans up
// its rows (the dev DB is shared).
func newDropTestField(t *testing.T, pool *pgxpool.Pool, code, ftype string) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	var id pgtype.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO field_definition (code, label, type) VALUES ($1, $2, $3)
		 ON CONFLICT (code) DO UPDATE SET type = EXCLUDED.type RETURNING id`,
		code, code, ftype).Scan(&id)
	if err != nil {
		t.Fatalf("insert field_definition %s: %v", code, err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value_history WHERE field_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE field_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM field_definition WHERE id = $1`, id)
	})
	return id
}
