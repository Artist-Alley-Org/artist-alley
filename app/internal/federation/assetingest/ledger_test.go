// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetingest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/audit"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/receivedassets"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// #1574 slice 2: the transfer and fixity event ledger
// (federation_transfer_events, migration 00089), written by the receiver
// core in the same transaction as each record state change.

// ledgerRow is what a test reads back of one ledger row.
type ledgerRow struct {
	eventType, outcome, stateAfter, peerURL, agent string
	reason, basis, observedSHA, mismatchAlg        *string
	mismatchExpected, mismatchObserved             *string
	pos                                            *int32
	expectedSHA                                    *string
	expectedSize, observedSize                     *int64
	truncated                                      bool
	attempt                                        int32
}

func (f *fx) ledger(recordID uuid.UUID) []ledgerRow {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT event_type, outcome, record_state_after, peer_instance_url, agent_name,
		    reason, observed_basis, observed_sha256, mismatch_algorithm, mismatch_expected, mismatch_observed,
		    file_position, expected_sha256, expected_size_bytes, observed_size_bytes, observed_truncated, attempt
		  FROM federation_transfer_events WHERE received_asset_id = $1 ORDER BY occurred_at, file_position NULLS LAST`, recordID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		if err := rows.Scan(&r.eventType, &r.outcome, &r.stateAfter, &r.peerURL, &r.agent,
			&r.reason, &r.basis, &r.observedSHA, &r.mismatchAlg, &r.mismatchExpected, &r.mismatchObserved,
			&r.pos, &r.expectedSHA, &r.expectedSize, &r.observedSize, &r.truncated, &r.attempt); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func i64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func hexSHA256(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// TestLedger_FixityFailuresRecordEvidence: tampered bytes, a short body, an
// overlong body, a wrong Content-Length and a mismatch on an additional
// listed checksum each give a rejected record, no pin and no stored
// object for what arrived, and exactly one ledger row saying what was
// listed and what arrived.
func TestLedger_FixityFailuresRecordEvidence(t *testing.T) {
	for _, mode := range []string{"tamper", "short", "long", "length", "sha512"} {
		t.Run(mode, func(t *testing.T) {
			f := newFx(t)
			a := f.newAsset(2)
			body := f.origin.files[a.hash[1]]
			size := int64(len(body))
			wrongSHA512 := strings.Repeat("1", 128)
			if mode == "sha512" {
				a.files[1]["checksums"] = []map[string]string{{"algorithm": "sha256", "value": a.hash[1]},
					{"algorithm": "sha512", "value": wrongSHA512}}
			} else {
				f.origin.setMode(a.hash[1], mode)
			}
			f.grant(f.peerID, a.uri, nil)
			out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
			wantReason := casfetch.ReasonFixityMismatch
			if mode == "length" {
				wantReason = casfetch.ReasonLengthMismatch
			}
			if assetingest.ReasonOf(err) != wantReason || out.State != assetingest.StateRejected {
				t.Fatalf("outcome %+v %v", out, err)
			}
			f.nothingUsable(a.hash...)

			rows := f.ledger(out.RecordID)
			if len(rows) != 1 {
				t.Fatalf("%d ledger rows, want 1: %+v", len(rows), rows)
			}
			r := rows[0]
			if r.eventType != assetingest.EventFixityCheck || r.outcome != assetingest.OutcomeFailure || str(r.reason) != wantReason ||
				r.stateAfter != assetingest.StateRejected || r.attempt != 1 || r.peerURL != f.origin.srv.URL || r.agent != assetingest.AgentName {
				t.Fatalf("row %+v", r)
			}
			if r.pos == nil || *r.pos != 1 || str(r.expectedSHA) != a.hash[1] || i64(r.expectedSize) != size {
				t.Fatalf("file %v %s %d", r.pos, str(r.expectedSHA), i64(r.expectedSize))
			}
			var observedHash string
			switch mode {
			case "tamper":
				tampered := bytes.Clone(body)
				tampered[0] ^= 0xff
				observedHash = hexSHA256(tampered)
				if str(r.basis) != casfetch.ObservedReceived || str(r.observedSHA) != observedHash || i64(r.observedSize) != size {
					t.Fatalf("tamper observed %s %s %d", str(r.basis), str(r.observedSHA), i64(r.observedSize))
				}
			case "short":
				observedHash = hexSHA256(body[:len(body)/2])
				if str(r.basis) != casfetch.ObservedReceived || str(r.observedSHA) != observedHash || i64(r.observedSize) != size/2 {
					t.Fatalf("short observed %s %s %d", str(r.basis), str(r.observedSHA), i64(r.observedSize))
				}
			case "long":
				if str(r.basis) != casfetch.ObservedReceived || !r.truncated || r.observedSHA != nil || i64(r.observedSize) != size+1 {
					t.Fatalf("long observed %s truncated=%v %s %d", str(r.basis), r.truncated, str(r.observedSHA), i64(r.observedSize))
				}
			case "length":
				if str(r.basis) != casfetch.ObservedHeader || r.observedSHA != nil || i64(r.observedSize) != size+1 {
					t.Fatalf("length observed %s %s %d", str(r.basis), str(r.observedSHA), i64(r.observedSize))
				}
			case "sha512":
				real := sha512.Sum512(body)
				if str(r.basis) != casfetch.ObservedReceived || str(r.observedSHA) != a.hash[1] || i64(r.observedSize) != size ||
					str(r.mismatchAlg) != "sha512" || str(r.mismatchExpected) != wrongSHA512 || str(r.mismatchObserved) != hex.EncodeToString(real[:]) {
					t.Fatalf("sha512 observed %+v", r)
				}
			}
			// What arrived was never stored, under either digest.
			for _, h := range []string{a.hash[1], observedHash} {
				if h == "" {
					continue
				}
				if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`, h); n != 0 {
					t.Fatalf("an object is stored for %s", h[:12])
				}
			}
		})
	}
}

// TestLedger_CompletionRecordsEachOriginal: a complete record has one
// fixity-check success per original (observed = listed) and one
// ingestion success, written with the completion.
func TestLedger_CompletionRecordsEachOriginal(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(2)
	f.grant(f.peerID, a.uri, nil)
	out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if err != nil || out.State != assetingest.StateComplete {
		t.Fatalf("receive: %+v %v", out, err)
	}
	rows := f.ledger(out.RecordID)
	if len(rows) != 3 {
		t.Fatalf("%d rows: %+v", len(rows), rows)
	}
	for i, r := range rows[:2] {
		if r.eventType != assetingest.EventFixityCheck || r.outcome != assetingest.OutcomeSuccess || r.reason != nil ||
			str(r.basis) != casfetch.ObservedReceived || str(r.observedSHA) != a.hash[i] || str(r.expectedSHA) != a.hash[i] ||
			r.stateAfter != assetingest.StateComplete {
			t.Fatalf("original %d: %+v", i, r)
		}
	}
	if r := rows[2]; r.eventType != assetingest.EventIngestion || r.outcome != assetingest.OutcomeSuccess || r.pos != nil {
		t.Fatalf("ingestion row %+v", r)
	}
}

// TestLedger_OutageThenRetry: an outage is a transfer warning (the record
// stays pending), and the retry adds the success rows.
func TestLedger_OutageThenRetry(t *testing.T) {
	f := newFx(t, assetingest.Config{MaxAttempts: 3})
	a := f.newAsset(2)
	f.grant(f.peerID, a.uri, nil)
	f.origin.setMode(a.hash[1], "unavailable")
	e := f.envelope(a, a.member(f.version(a), 1, "public"))
	out, err := f.receive(e)
	if !assetingest.Retryable(err) {
		t.Fatalf("outage: %v", err)
	}
	f.origin.setMode(a.hash[1], "")
	if again, err := f.receive(e); err != nil || again.State != assetingest.StateComplete {
		t.Fatalf("retry: %+v %v", again, err)
	}
	rows := f.ledger(out.RecordID)
	if len(rows) != 4 {
		t.Fatalf("%d rows: %+v", len(rows), rows)
	}
	w := rows[0]
	if w.eventType != assetingest.EventTransfer || w.outcome != assetingest.OutcomeWarning || str(w.reason) != casfetch.ReasonHTTPStatus ||
		w.stateAfter != assetingest.StatePending || w.attempt != 1 || w.pos == nil || *w.pos != 1 || w.basis != nil {
		t.Fatalf("warning row %+v", w)
	}
	for _, r := range rows[1:] {
		if r.outcome != assetingest.OutcomeSuccess || r.attempt != 2 {
			t.Fatalf("after the retry: %+v", r)
		}
	}
}

// TestLedger_BindTimeRefusal: a record refused as it is bound (here, a
// restricted tier) has its ingestion failure row, attempt 0.
func TestLedger_BindTimeRefusal(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "restricted")))
	if assetingest.ReasonOf(err) != casfetch.ReasonSensitivity {
		t.Fatalf("restricted: %+v %v", out, err)
	}
	rows := f.ledger(out.RecordID)
	if len(rows) != 1 || rows[0].eventType != assetingest.EventIngestion || rows[0].outcome != assetingest.OutcomeFailure ||
		str(rows[0].reason) != casfetch.ReasonSensitivity || rows[0].attempt != 0 || rows[0].pos != nil {
		t.Fatalf("rows %+v", rows)
	}
}

// TestLedger_SurvivesPeerDeletion: deleting the peer removes the record
// (cascade) and leaves its ledger rows, which the operator API still
// answers with the peer's address as it was.
func TestLedger_SurvivesPeerDeletion(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(1)
	f.origin.setMode(a.hash[0], "tamper")
	f.grant(f.peerID, a.uri, nil)
	out, _ := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE id = $1`, f.peerID); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM federation_received_assets WHERE id = $1`, out.RecordID); n != 0 {
		t.Fatal("the record outlived its peer")
	}
	if rows := f.ledger(out.RecordID); len(rows) != 1 || rows[0].peerURL != f.origin.srv.URL || str(rows[0].reason) != casfetch.ReasonFixityMismatch {
		t.Fatalf("after the peer went: %+v", rows)
	}
	h := &receivedassets.Handler{Pool: f.pool, Storage: f.svc}
	id := openapi.ListFederationTransferEventsParams{}
	rid := out.RecordID
	id.ReceivedAssetId = &rid
	res, err := h.ListFederationTransferEvents(auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: f.userRef, Capabilities: []string{"system.admin"}}),
		openapi.ListFederationTransferEventsRequestObject{Params: id})
	if err != nil {
		t.Fatal(err)
	}
	list, ok := res.(openapi.ListFederationTransferEvents200JSONResponse)
	if !ok || len(list.Items) != 1 {
		t.Fatalf("admin answer %T %+v", res, res)
	}
	e := list.Items[0]
	if e.RecordExists || e.PeerExists || e.PeerInstanceUrl != f.origin.srv.URL || str(e.ObservedSha256) == "" ||
		str(e.ObservedSha256) == a.hash[0] || str(e.ExpectedSha256) != a.hash[0] {
		t.Fatalf("admin row %+v", e)
	}
}

// TestLedger_RetentionSweep: the daily sweep removes rows older than the
// retention period and keeps the rest; a period under 30 days is raised to
// 30 (the database refuses younger deletes in any case).
func TestLedger_RetentionSweep(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	peer := uuid.New() // no FK: a snapshot of a peer that never existed here
	hash := strings.Repeat("ab", 32)
	insert := func(days int) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO federation_transfer_events
			(occurred_at, peer_id, peer_instance_url, received_asset_id, activity_uri, origin_asset_uri, version_uri,
			 version_number, sensitivity, file_position, file_role, object_hash, declared_media_type, event_type, outcome,
			 reason, expected_sha256, expected_size_bytes, observed_basis, observed_size_bytes, attempt, agent_name, record_state_after)
			VALUES (now() - make_interval(days => $1), $2, 'https://sweep.example', gen_random_uuid(), 'a', 'o', 'v', 1, 'public',
			        0, 'original', $3, 'image/png', 'fixity-check', 'failure', 'fixity_mismatch', $3, 10, 'received_bytes', 9, 1,
			        'test', 'rejected') RETURNING id`, days, peer, hash).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	left := func() map[uuid.UUID]bool {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT id FROM federation_transfer_events WHERE peer_id = $1`, peer)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[uuid.UUID]bool{}
		for rows.Next() {
			var id uuid.UUID
			_ = rows.Scan(&id)
			out[id] = true
		}
		return out
	}
	d400, d40, d10 := insert(400), insert(40), insert(10)
	if _, err := assetingest.SweepEventRetention(ctx, pool, assetingest.DefaultEventRetentionDays); err != nil {
		t.Fatal(err)
	}
	if l := left(); l[d400] || !l[d40] || !l[d10] {
		t.Fatalf("after a 365-day sweep: %v", l)
	}
	if _, err := assetingest.SweepEventRetention(ctx, pool, 1); err != nil { // raised to 30
		t.Fatal(err)
	}
	if l := left(); l[d40] || !l[d10] {
		t.Fatalf("after a sweep asked for 1 day (30 applied): %v", l)
	}
}

// stubFetcher answers every fetch with err.
type stubFetcher struct{ err error }

func (s stubFetcher) Fetch(context.Context, casfetch.Request) (*casfetch.Result, error) {
	return nil, s.err
}

func (f *fx) detailsOf(recordID uuid.UUID) []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT COALESCE(detail, '') FROM federation_transfer_events
		WHERE received_asset_id = $1 ORDER BY occurred_at`, recordID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		out = append(out, d)
	}
	return out
}

// TestLedger_DetailIsBoundedValidUTF8 (rev589): a failure whose error text
// has a multibyte rune straddling the 2000-byte bound, or bytes PostgreSQL
// text refuses, still records the state change and its event, with a
// valid UTF-8 detail of at most 2000 bytes that keeps a whole prefix.
func TestLedger_DetailIsBoundedValidUTF8(t *testing.T) {
	const bound = 2000
	prefix := "casfetch: " + casfetch.ReasonTransport + ": "
	for name, tc := range map[string]struct {
		msg  string
		want func(full string) string
	}{
		"a rune straddles the bound": {
			// The 3-byte rune starts at byte 1999 of the full error text.
			msg:  strings.Repeat("a", bound-1-len(prefix)) + strings.Repeat("€", 20),
			want: func(full string) string { return full[:bound-1] },
		},
		"invalid bytes and NUL": {
			msg:  "bad \xff\xfe bytes and a \x00 NUL",
			want: func(string) string { return prefix + "bad � bytes and a � NUL" },
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFx(t, assetingest.Config{MaxAttempts: 3})
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			cause := &casfetch.Error{Reason: casfetch.ReasonTransport, Err: errors.New(tc.msg)}
			svc := assetingest.New(assetingest.Deps{
				Pool: f.pool, Storage: f.svc, Peers: peers{pool: f.pool}, Grants: f.grants,
				Fetcher: stubFetcher{err: cause}, Audit: audit.NewRecorder(f.pool, logger), Logger: logger,
				Config: assetingest.Config{MaxAttempts: 3},
			})
			a := f.newAsset(1)
			f.grant(f.peerID, a.uri, nil)
			out, err := svc.Receive(context.Background(), assetingest.Announcement{PeerID: f.peerID,
				Envelope: f.envelope(a, a.member(f.version(a), 1, "public")), Signer: f.signer})
			if !assetingest.Retryable(err) || assetingest.ReasonOf(err) != casfetch.ReasonTransport {
				t.Fatalf("outcome %+v %v", out, err)
			}
			if st, r := f.state(out.RecordID); st != assetingest.StatePending || r != casfetch.ReasonTransport {
				t.Fatalf("stored %s/%s: the state change did not commit", st, r)
			}
			rows := f.ledger(out.RecordID)
			if len(rows) != 1 || rows[0].eventType != assetingest.EventTransfer || rows[0].outcome != assetingest.OutcomeWarning {
				t.Fatalf("ledger %+v", rows)
			}
			d := f.detailsOf(out.RecordID)[0]
			if !utf8.ValidString(d) || len(d) > bound || strings.ContainsRune(d, 0) || d != tc.want(cause.Error()) {
				t.Fatalf("detail (%d bytes, valid=%v): %q", len(d), utf8.ValidString(d), d)
			}
		})
	}
}

// TestLedger_InvalidStoredRecordIsRejected (rev589): a pending record whose
// stored announcement no longer validates, or whose stored file-set digest
// no longer matches, is rejected as archival_invalid by the real process
// path, with one ingestion failure row written in the same transaction.
// The record is bound by the real path (an outage leaves it pending); the
// stored state is then altered as storage corruption would, with triggers
// suspended for that one transaction only (session_replication_role).
func TestLedger_InvalidStoredRecordIsRejected(t *testing.T) {
	for name, corrupt := range map[string]string{
		"the stored announcement no longer validates": `UPDATE federation_received_assets SET record = record - 'files' WHERE id = $1`,
		"the stored file-set digest changed":          `UPDATE federation_received_assets SET file_set_sha256 = repeat('0', 64) WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFx(t, assetingest.Config{MaxAttempts: 3})
			ctx := context.Background()
			a := f.newAsset(1)
			f.grant(f.peerID, a.uri, nil)
			f.origin.setMode(a.hash[0], "unavailable")
			out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
			if !assetingest.Retryable(err) || out.State != assetingest.StatePending {
				t.Fatalf("outage: %+v %v", out, err)
			}
			f.origin.setMode(a.hash[0], "")
			if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
				// Triggers off for this one transaction only (SET LOCAL), as a
				// storage-level corruption would bypass them.
				for _, q := range []string{`SET LOCAL session_replication_role = replica`, corrupt} {
					var args []any
					if strings.Contains(q, "$1") {
						args = []any{out.RecordID}
					}
					if _, err := tx.Exec(ctx, q, args...); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			res, err := f.ingest.Process(ctx, out.RecordID, func(string, string) (casfetch.Signer, error) { return f.signer, nil })
			if assetingest.ReasonOf(err) != assetingest.ReasonArchivalInvalid || res.State != assetingest.StateRejected {
				t.Fatalf("process: %+v %v", res, err)
			}
			if st, r := f.state(out.RecordID); st != assetingest.StateRejected || r != assetingest.ReasonArchivalInvalid {
				t.Fatalf("stored %s/%s", st, r)
			}
			rows := f.ledger(out.RecordID)
			var failures []ledgerRow
			for _, r := range rows {
				if r.outcome == assetingest.OutcomeFailure {
					failures = append(failures, r)
				}
			}
			if len(failures) != 1 || failures[0].eventType != assetingest.EventIngestion ||
				str(failures[0].reason) != assetingest.ReasonArchivalInvalid || failures[0].stateAfter != assetingest.StateRejected ||
				failures[0].attempt != 2 || failures[0].pos != nil {
				t.Fatalf("ledger %+v", rows)
			}
			f.nothingUsable(a.hash...)
		})
	}
}
