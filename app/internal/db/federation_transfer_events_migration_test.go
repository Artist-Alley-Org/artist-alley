// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMigration00089_FederationTransferEvents_UpDownUp: the #1574 slice 2
// ledger. Rows need no peer or record to exist (no foreign key); each
// outcome and reason is bound to its evidence; UPDATE is refused; DELETE
// is refused inside the retention period (default 365 days, never under
// 30 whatever the setting says) and allowed past it; Down removes the
// table and its functions; Up restores them.
func TestMigration00089_FederationTransferEvents_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	table := `SELECT count(*) FROM information_schema.tables WHERE table_name = 'federation_transfer_events'`
	funcs := `SELECT count(*) FROM pg_proc WHERE proname IN ('federation_transfer_events_guard', 'federation_transfer_event_retention')`
	if _, err := p.UpTo(ctx, 89); err != nil {
		t.Fatalf("up to 89: %v", err)
	}
	if count(table) != 1 || count(funcs) != 2 {
		t.Fatalf("after Up: table %d, functions %d", count(table), count(funcs))
	}
	if n := count(`SELECT count(*) FROM information_schema.table_constraints
		WHERE table_name = 'federation_transfer_events' AND constraint_type = 'FOREIGN KEY'`); n != 0 {
		t.Fatalf("the ledger has %d foreign keys; it must have none", n)
	}

	hash := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	// insert writes one row; cols overrides the defaults (a fixity-check
	// failure with a received digest that differs).
	insert := func(age time.Duration, cols map[string]any) (string, error) {
		t.Helper()
		v := map[string]any{
			"occurred_at": time.Now().Add(-age), "peer_id": uuid.New(), "peer_instance_url": "https://gone.example",
			"received_asset_id": uuid.New(), "activity_uri": "https://gone.example/activities/1",
			"origin_asset_uri": "https://gone.example/assets/1", "version_uri": "https://gone.example/assets/1/versions/1",
			"version_number": 1, "sensitivity": "public",
			"file_position": 0, "file_role": "original", "object_hash": hash, "declared_media_type": "image/png",
			"event_type": "fixity-check", "outcome": "failure", "reason": "fixity_mismatch",
			"expected_sha256": hash, "expected_size_bytes": 10,
			"observed_basis": "received_bytes", "observed_sha256": other, "observed_size_bytes": 10,
			"attempt": 1, "agent_name": "test", "record_state_after": "rejected",
		}
		for k, x := range cols {
			if x == nil {
				delete(v, k)
				continue
			}
			v[k] = x
		}
		names := make([]string, 0, len(v))
		ph := make([]string, 0, len(v))
		args := make([]any, 0, len(v))
		for k, x := range v {
			names = append(names, k)
			args = append(args, x)
			ph = append(ph, "$"+strconv.Itoa(len(args)))
		}
		var id string
		err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_transfer_events (`+strings.Join(names, ", ")+`)
			VALUES (`+strings.Join(ph, ", ")+`) RETURNING id`, args...).Scan(&id)
		return id, err
	}
	must := func(age time.Duration, cols map[string]any) string {
		t.Helper()
		id, err := insert(age, cols)
		if err != nil {
			t.Fatalf("insert %v: %v", cols, err)
		}
		return id
	}
	refused := func(name string, cols map[string]any) {
		t.Helper()
		if _, err := insert(0, cols); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// Rows that name no existing peer or record are accepted: snapshots.
	fresh := must(0, nil)
	must(0, map[string]any{"observed_sha256": nil, "observed_size_bytes": 5}) // short body
	must(0, map[string]any{"observed_sha256": nil, "observed_size_bytes": 11, "observed_truncated": true})
	must(0, map[string]any{"reason": "length_mismatch", "observed_basis": "response_header", "observed_sha256": nil, "observed_size_bytes": 11})
	must(0, map[string]any{"observed_sha256": hash, "mismatch_algorithm": "sha512",
		"mismatch_expected": strings.Repeat("1", 128), "mismatch_observed": strings.Repeat("2", 128)})
	must(0, map[string]any{"observed_basis": "unavailable", "observed_sha256": nil, "observed_size_bytes": nil})
	must(0, map[string]any{"outcome": "success", "reason": nil, "observed_sha256": hash, "record_state_after": "complete"})
	must(0, map[string]any{"event_type": "ingestion", "outcome": "success", "reason": nil, "record_state_after": "complete",
		"file_position": nil, "file_role": nil, "object_hash": nil, "declared_media_type": nil, "expected_sha256": nil,
		"expected_size_bytes": nil, "observed_basis": nil, "observed_sha256": nil, "observed_size_bytes": nil})
	must(0, map[string]any{"event_type": "transfer", "outcome": "warning", "reason": "http_status", "record_state_after": "pending",
		"observed_basis": nil, "observed_sha256": nil, "observed_size_bytes": nil})

	// Evidence bound to its reason and outcome.
	refused("fixity_mismatch with no observation", map[string]any{"observed_basis": nil, "observed_sha256": nil, "observed_size_bytes": nil})
	refused("fixity_mismatch whose observation matches", map[string]any{"observed_sha256": hash})
	refused("success whose observation differs", map[string]any{"outcome": "success", "reason": nil, "record_state_after": "complete"})
	refused("success without a reason-free outcome", map[string]any{"outcome": "success", "record_state_after": "complete", "observed_sha256": hash})
	refused("length_mismatch of the listed size", map[string]any{"reason": "length_mismatch", "observed_basis": "response_header", "observed_sha256": nil})
	refused("truncated with a digest", map[string]any{"observed_size_bytes": 11, "observed_truncated": true})
	refused("a fixity reason on a transfer event", map[string]any{"event_type": "transfer"})
	refused("an ingestion event naming a file", map[string]any{"event_type": "ingestion", "observed_basis": nil, "observed_sha256": nil, "observed_size_bytes": nil})
	refused("a failure that leaves the record pending", map[string]any{"record_state_after": "pending"})
	refused("an expected digest other than the file's", map[string]any{"expected_sha256": other})
	refused("an unknown reason", map[string]any{"reason": "made_up"})
	// NULL paths: a CHECK that evaluates to NULL passes, so each missing
	// value must still be refused.
	refused("a fixity-check success with no observation", map[string]any{"outcome": "success", "reason": nil,
		"record_state_after": "complete", "observed_basis": nil, "observed_sha256": nil, "observed_size_bytes": nil})
	refused("an observed digest with no basis", map[string]any{"observed_basis": nil})
	refused("truncated with no size", map[string]any{"observed_sha256": nil, "observed_size_bytes": nil, "observed_truncated": true})
	refused("a mismatch algorithm without its values", map[string]any{"observed_sha256": hash, "mismatch_algorithm": "sha512"})
	refused("length_mismatch with no observed size", map[string]any{"reason": "length_mismatch", "observed_basis": "received_bytes",
		"observed_sha256": nil, "observed_size_bytes": nil})
	refused("a malformed digest", map[string]any{"observed_sha256": "xyz"})

	// Immutable; kept for the retention period.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE federation_transfer_events SET detail = 'x' WHERE id = $1`, fresh); err == nil {
		t.Error("UPDATE was accepted")
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM federation_transfer_events WHERE id = $1`, fresh); err == nil {
		t.Error("DELETE of a fresh row was accepted")
	}
	days40 := must(40*24*time.Hour, nil)
	days400 := must(400*24*time.Hour, nil)
	days10 := must(10*24*time.Hour, nil)
	deleteWith := func(setting, id string) error {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		if setting != "" {
			if _, err := tx.ExecContext(ctx, `SELECT set_config('aa.federation_event_retention', $1, true)`, setting); err != nil {
				t.Fatal(err)
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM federation_transfer_events WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("deleted %d rows", n)
		}
		return tx.Commit()
	}
	if err := deleteWith("", days40); err == nil {
		t.Error("a 40-day-old row was deleted under the default 365 days")
	}
	if err := deleteWith("1 day", days10); err == nil {
		t.Error("a 10-day-old row was deleted under a setting below the 30-day floor")
	}
	if err := deleteWith("30 days", days40); err != nil {
		t.Errorf("a 40-day-old row under 30 days: %v", err)
	}
	if err := deleteWith("", days400); err != nil {
		t.Errorf("a 400-day-old row under the default: %v", err)
	}

	// TRUNCATE is not row-level: an operator's reset is not blocked.
	if _, err := sqlDB.ExecContext(ctx, `TRUNCATE federation_transfer_events`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// The record's reason set gains archival_invalid (rev589): a pending
	// record can be rejected with it, and the ledger accepts it.
	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm89', 1) RETURNING ref`,
		"m89-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	var peer string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'm89', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m89-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	pending := func() string {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id)
			VALUES ($1, $2, 'https://m89.example/users/a', 'https://m89.example/assets/1', gen_random_uuid(),
			        'https://m89.example/assets/1/versions/1', 1, $3, '{}'::jsonb, 'public', 'k') RETURNING id`,
			peer, "https://m89.example/activities/"+uuid.NewString(), strings.Repeat("0", 64)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
			VALUES ($1, 0, 'original', $2, 1, '[]'::jsonb, 'image/png')`, id, hash); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return id
	}
	reject := func(id string) error {
		t.Helper()
		_, err := sqlDB.ExecContext(ctx, `UPDATE federation_received_assets SET state = 'rejected', reason = 'archival_invalid' WHERE id = $1`, id)
		return err
	}
	kept := pending()
	if err := reject(kept); err != nil {
		t.Fatalf("archival_invalid after Up: %v", err)
	}
	must(0, map[string]any{"event_type": "ingestion", "reason": "archival_invalid",
		"file_position": nil, "file_role": nil, "object_hash": nil, "declared_media_type": nil, "expected_sha256": nil,
		"expected_size_bytes": nil, "observed_basis": nil, "observed_sha256": nil, "observed_size_bytes": nil})
	later := pending()

	if _, err := p.DownTo(ctx, 88); err != nil {
		t.Fatalf("down to 88: %v", err)
	}
	if count(table) != 0 || count(funcs) != 0 {
		t.Fatalf("after Down: table %d, functions %d", count(table), count(funcs))
	}
	// Down holds new writes to the 00086 set and keeps the existing row.
	if n := count(`SELECT count(*) FROM federation_received_assets WHERE id = $1 AND reason = 'archival_invalid'`, kept); n != 1 {
		t.Fatal("Down rewrote or removed a record rejected as archival_invalid")
	}
	if err := reject(later); err == nil {
		t.Fatal("after Down a new archival_invalid rejection was accepted")
	}
	if _, err := p.UpTo(ctx, 89); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if count(table) != 1 || count(funcs) != 2 {
		t.Fatalf("after Up again: table %d, functions %d", count(table), count(funcs))
	}
	if err := reject(later); err != nil {
		t.Fatalf("archival_invalid after Up again: %v", err)
	}
}
