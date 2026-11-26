// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: migration 00104 records each received access copy's outcome on
// its existing file row. Pinned here against a real, fresh database:
//
//   - rows written before it read as unattempted; originals and
//     preservation entries keep their states;
//   - an access row records a bounded reason or becomes verified only
//     while its record is the current complete version, with its declared
//     facts unchanged; verified is assigned once;
//   - a verified access row commits only with its record's pin at the
//     stored size, also when nothing but that row changed (another owner
//     pinning the hash does not count; the record's own pin of the same
//     hash through an original does); the deferred check on the row is
//     queued only by its move into verified;
//   - supersession keeps a verified copy held; withdrawal keeps its
//     verified_at; a new access row is always an unattempted claim;
//   - Down refuses while a received copy exists, otherwise restores the
//     00103 shape keeping recorded reasons as data; Up again reuses them.
//
// Skips without AA_DB_PASSWORD, like the other migration tests.

package db

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	accessOutcomesBeforeVersion = 103 // 00103_federation_inbound_grant_allow_access_copies
	accessOutcomesAtVersion     = 104 // 00104_federation_received_access_copy_outcomes
)

func TestMigration00104_ReceivedAccessCopyOutcomes_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, accessOutcomesBeforeVersion); err != nil {
		t.Fatalf("up to %d: %v", accessOutcomesBeforeVersion, err)
	}

	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm104', 1) RETURNING ref`,
		"m104-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	newPeer := func() string {
		t.Helper()
		var id string
		if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
			VALUES ($1, 'm104', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m104-"+uuid.NewString()[:8]+".example", ref).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	hash := func(c string) string { return strings.Repeat(c, 64) }
	object := func(h string, size int) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, `INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, $2, 'fs')`, h, size); err != nil {
			t.Fatal(err)
		}
	}
	// inTx runs stmts in one transaction and commits: the first error,
	// including a deferred check's at commit.
	type stmt struct {
		q    string
		args []any
	}
	inTx := func(stmts ...stmt) error {
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		for _, s := range stmts {
			if _, err := tx.ExecContext(ctx, s.q, s.args...); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	type frow struct {
		pos        int
		role, hash string
		size       int
	}
	// record inserts version n of asset from peer, following pred, with
	// files, and (complete) verifies and pins its originals in the same
	// transaction, superseding predID if given.
	record := func(peer, asset string, n int, pred, predID any, files []frow, complete bool) (string, error) {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id, predecessor_version_uri)
			VALUES ($1, $2, 'https://m104.example/users/a', $3, gen_random_uuid(), $4, $5, $6, '{}'::jsonb, 'public', 'k', $7) RETURNING id`,
			peer, "https://m104.example/activities/"+uuid.NewString(), asset, asset+"/versions/"+strconv.Itoa(n), n,
			strings.Repeat("0", 64), pred).Scan(&id); err != nil {
			return "", err
		}
		for _, f := range files {
			state := "pending"
			if f.role != "original" {
				state = "not_transferred"
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO federation_received_asset_files
				(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type, state)
				VALUES ($1, $2, $3, $4, $5, jsonb_build_array(jsonb_build_object('algorithm', 'sha256', 'value', $4::text)), 'image/png', $6)`,
				id, f.pos, f.role, f.hash, f.size, state); err != nil {
				return "", err
			}
		}
		if complete {
			for _, f := range files {
				if f.role != "original" {
					continue
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id)
					VALUES ($1, 'federation_received_asset', $2) ON CONFLICT DO NOTHING`, f.hash, id); err != nil {
					return "", err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE federation_received_asset_files
				SET state = 'verified', verified_at = now(), detected_media_type = 'image/png', detected_by = 'm104'
				WHERE received_asset_id = $1 AND role = 'original'`, id); err != nil {
				return "", err
			}
			if predID != nil {
				if _, err := tx.ExecContext(ctx, `UPDATE federation_received_assets SET state = 'superseded', superseded_at = now()
					WHERE id = $1`, predID); err != nil {
					return "", err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE federation_received_assets
				SET state = 'complete', completed_at = now(), authorizing_grant_id = gen_random_uuid(), predecessor_id = $2
				WHERE id = $1`, id, predID); err != nil {
				return "", err
			}
		}
		return id, tx.Commit()
	}
	fileState := func(id string, pos int) (state string, reason sql.NullString, verified sql.NullTime) {
		t.Helper()
		if err := sqlDB.QueryRowContext(ctx, `SELECT state, access_failure_reason, verified_at FROM federation_received_asset_files
			WHERE received_asset_id = $1 AND position = $2`, id, pos).Scan(&state, &reason, &verified); err != nil {
			t.Fatal(err)
		}
		return state, reason, verified
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The deferred access-row check is queued only by an access row
	// moving into verified: not by inserting a claim, recording a reason
	// or a withdrawal (those are covered by the write check and the
	// record and pin checks).
	wantTriggerScope := func(when string) {
		t.Helper()
		var def string
		if err := sqlDB.QueryRowContext(ctx, `SELECT pg_get_triggerdef(oid) FROM pg_trigger
			WHERE tgname = 'federation_received_asset_files_commit_check_trg'`).Scan(&def); err != nil {
			t.Fatalf("%s: the access-row check trigger: %v", when, err)
		}
		for _, part := range []string{"CREATE CONSTRAINT TRIGGER federation_received_asset_files_commit_check_trg AFTER UPDATE ON public.federation_received_asset_files ",
			"DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (((new.role = 'access'::text) AND (old.state <> 'verified'::text) AND (new.state = 'verified'::text))) ",
			"EXECUTE FUNCTION federation_received_asset_files_commit_check()"} {
			if !strings.Contains(def, part) {
				t.Fatalf("%s: trigger %q, want it to contain %q", when, def, part)
			}
		}
		if strings.Contains(def, "INSERT") || strings.Contains(def, "DELETE") {
			t.Fatalf("%s: trigger %q fires on more than updates", when, def)
		}
	}
	wantRefused := func(what, contains string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: accepted", what)
		}
		if contains != "" && !strings.Contains(err.Error(), contains) {
			t.Fatalf("%s: %v, want an error naming %q", what, err, contains)
		}
	}

	// Objects: A the original of v1 (also declared as access position 4),
	// B and C access copies of v1, D an access copy stored at another size,
	// E and F v2's original and access copy. B is pinned by another owner.
	hA, hB, hC, hD, hE, hF, hP := hash("a"), hash("b"), hash("c"), hash("d"), hash("e"), hash("f"), hash("9")
	for i, h := range []string{hA, hB, hC, hD, hE, hF} {
		object(h, i+1)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($1, 'user', 'other-owner')`, hB); err != nil {
		t.Fatal(err)
	}
	peer := newPeer()
	asset := "https://m104.example/assets/" + uuid.NewString()
	v1Files := []frow{{0, "original", hA, 1}, {1, "access", hB, 2}, {2, "access", hC, 3}, {3, "preservation", hP, 7},
		{4, "access", hA, 1}, {5, "access", hD, 9}}
	v1, err := record(peer, asset, 1, nil, nil, v1Files, true)
	if err != nil {
		t.Fatalf("v1 at 00103: %v", err)
	}

	// Up: everything recorded before reads as before, access rows unattempted.
	if _, err := p.UpTo(ctx, accessOutcomesAtVersion); err != nil {
		t.Fatalf("up to %d: %v", accessOutcomesAtVersion, err)
	}
	wantTriggerScope("after Up")
	for pos, want := range map[int]string{0: "verified", 1: "not_transferred", 2: "not_transferred", 3: "not_transferred", 4: "not_transferred", 5: "not_transferred"} {
		if st, reason, _ := fileState(v1, pos); st != want || reason.Valid {
			t.Fatalf("position %d after Up: %s %v, want %s and no reason", pos, st, reason, want)
		}
	}

	exec := func(q string, args ...any) stmt { return stmt{q, args} }
	setReason := func(id string, pos int, reason string) stmt {
		return exec(`UPDATE federation_received_asset_files SET access_failure_reason = $3 WHERE received_asset_id = $1 AND position = $2`, id, pos, reason)
	}
	verify := func(id string, pos int) stmt {
		return exec(`UPDATE federation_received_asset_files SET state = 'verified', verified_at = now(), access_failure_reason = NULL
			WHERE received_asset_id = $1 AND position = $2`, id, pos)
	}
	recordPin := func(h, id string) stmt {
		return exec(`INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($1, 'federation_received_asset', $2)`, h, id)
	}

	// A bounded reason on an unverified access row of the current version;
	// a later failure replaces it.
	if err := inTx(setReason(v1, 2, "timeout")); err != nil {
		t.Fatalf("record a failure: %v", err)
	}
	if err := inTx(setReason(v1, 2, "fixity_mismatch")); err != nil {
		t.Fatalf("record a later failure: %v", err)
	}
	wantRefused("an unknown reason", "access_reason_check", inTx(setReason(v1, 2, "nope")))
	wantRefused("a reason on a preservation entry", "only a pending original is updated", inTx(setReason(v1, 3, "timeout")))
	wantRefused("a reason on an original", "only a pending original is updated", inTx(setReason(v1, 0, "timeout")))
	wantRefused("an access row's checksums rewritten", "not rewritten",
		inTx(exec(`UPDATE federation_received_asset_files SET checksums = '[]'::jsonb WHERE received_asset_id = $1 AND position = 1`, v1)))
	wantRefused("an access row's media type rewritten", "not rewritten",
		inTx(exec(`UPDATE federation_received_asset_files SET declared_media_type = 'image/gif' WHERE received_asset_id = $1 AND position = 1`, v1)))
	wantRefused("a preservation entry verified", "", inTx(verify(v1, 3)))

	// Verified only with the record's own pin at the stored size.
	wantRefused("verified with only another owner's pin", "verified access copy that is not pinned", inTx(verify(v1, 1)))
	wantRefused("verified at another size", "verified access copy that is not pinned", inTx(recordPin(hD, v1), verify(v1, 5)))
	if err := inTx(recordPin(hB, v1), verify(v1, 1)); err != nil {
		t.Fatalf("verify with the record's pin: %v", err)
	}
	// The record's own pin of the same hash, through its original, is that pin.
	if err := inTx(verify(v1, 4)); err != nil {
		t.Fatalf("verify an access copy whose hash the record's original already pins: %v", err)
	}
	st, _, at := fileState(v1, 1)
	if st != "verified" || !at.Valid {
		t.Fatalf("position 1: %s %v", st, at)
	}
	// Single assignment.
	wantRefused("verified_at rewritten", "", inTx(exec(`UPDATE federation_received_asset_files SET verified_at = now() + interval '1 hour'
		WHERE received_asset_id = $1 AND position = 1`, v1)))
	wantRefused("verified back to not_transferred", "", inTx(exec(`UPDATE federation_received_asset_files SET state = 'not_transferred', verified_at = NULL
		WHERE received_asset_id = $1 AND position = 1`, v1)))
	wantRefused("a reason on a verified copy", "", inTx(setReason(v1, 1, "timeout")))
	wantRefused("a verified copy withdrawn without its record", "not withdrawn",
		inTx(exec(`UPDATE federation_received_asset_files SET state = 'withdrawn' WHERE received_asset_id = $1 AND position = 1`, v1)))
	wantRefused("the record's pin of a verified copy removed", "verified access copy that is not pinned",
		inTx(exec(`DELETE FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_received_asset' AND pin_subject_id = $2`, hB, v1)))
	if _, _, again := fileState(v1, 1); !again.Valid || !again.Time.Equal(at.Time) {
		t.Fatalf("verified_at changed: %v then %v", at, again)
	}

	// A new access row is an unattempted claim.
	for name, q := range map[string]string{
		"verified": `INSERT INTO federation_received_asset_files (received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type, state, verified_at)
			VALUES ($1, 1, 'access', $2, 6, '[]'::jsonb, 'image/png', 'verified', now())`,
		"with a reason": `INSERT INTO federation_received_asset_files (received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type, state, access_failure_reason)
			VALUES ($1, 1, 'access', $2, 6, '[]'::jsonb, 'image/png', 'not_transferred', 'timeout')`,
	} {
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number, file_set_sha256, record, sensitivity, signer_key_id)
			VALUES ($1, $2, 'https://m104.example/users/a', $3, gen_random_uuid(), $3 || '/versions/1', 1, $4, '{}'::jsonb, 'public', 'k') RETURNING id`,
			newPeer(), "https://m104.example/activities/"+uuid.NewString(), "https://m104.example/assets/"+uuid.NewString(), strings.Repeat("0", 64)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		_, err = tx.ExecContext(ctx, q, id, hF)
		_ = tx.Rollback()
		wantRefused("an access row inserted "+name, "", err)
	}

	// A pending successor's access rows record nothing.
	v2Files := []frow{{0, "original", hE, 5}, {1, "access", hF, 6}}
	v2, err := record(peer, asset, 2, asset+"/versions/1", nil, v2Files, false)
	if err != nil {
		t.Fatalf("v2 pending: %v", err)
	}
	wantRefused("a reason on a pending version's access copy", "current complete version", inTx(setReason(v2, 1, "timeout")))
	// v2 completes and supersedes v1 (in one transaction, as complete does).
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []stmt{
		recordPin(hE, v2),
		exec(`UPDATE federation_received_asset_files SET state = 'verified', verified_at = now(), detected_media_type = 'image/png', detected_by = 'm104'
			WHERE received_asset_id = $1 AND role = 'original'`, v2),
		exec(`UPDATE federation_received_assets SET state = 'superseded', superseded_at = now() WHERE id = $1`, v1),
		exec(`UPDATE federation_received_assets SET state = 'complete', completed_at = now(), authorizing_grant_id = gen_random_uuid(), predecessor_id = $2 WHERE id = $1`, v2, v1),
	} {
		if _, err := tx.ExecContext(ctx, s.q, s.args...); err != nil {
			t.Fatalf("activate v2: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("activate v2: %v", err)
	}
	// The superseded version keeps its verified copy, pinned.
	if st, _, _ := fileState(v1, 1); st != "verified" ||
		count(`SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'federation_received_asset' AND pin_subject_id = $2`, hB, v1) != 1 {
		t.Fatalf("superseded v1 position 1: %s", st)
	}
	wantRefused("a reason on a superseded version's access copy", "current complete version", inTx(setReason(v1, 2, "timeout")))
	wantRefused("a superseded version's access copy verified", "current complete version", inTx(recordPin(hC, v1), verify(v1, 2)))
	if err := inTx(setReason(v2, 1, "timeout")); err != nil {
		t.Fatalf("a reason on the new current version: %v", err)
	}

	// Withdrawal, as Withdraw does it: pins released, records withdrawn,
	// originals and verified copies withdrawn, keeping verified_at.
	if err := inTx(
		exec(`SELECT federation_received_asset_release_pins(ARRAY[$1::text, $2::text])`, v1, v2),
		exec(`UPDATE federation_received_assets SET state = 'withdrawn', reason = 'withdrawn', withdrawn_at = now() WHERE id IN ($1, $2)`, v1, v2),
		exec(`UPDATE federation_received_asset_files SET state = 'withdrawn' WHERE received_asset_id IN ($1, $2)
			AND ((role = 'original' AND state IN ('pending', 'verified')) OR (role = 'access' AND state = 'verified'))`, v1, v2),
	); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if st, reason, w := fileState(v1, 1); st != "withdrawn" || !w.Valid || !w.Time.Equal(at.Time) || reason.Valid {
		t.Fatalf("withdrawn copy: %s %v %v", st, w, reason)
	}
	if st, reason, _ := fileState(v1, 2); st != "not_transferred" || reason.String != "fixity_mismatch" {
		t.Fatalf("a failed claim after withdrawal: %s %v", st, reason)
	}
	if n := count(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id IN ($1, $2)`, v1, v2); n != 0 {
		t.Fatalf("%d record pins left after withdrawal", n)
	}
	if count(`SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'user'`, hB) != 1 ||
		count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NULL`, hB) != 1 ||
		count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NOT NULL`, hA) != 1 {
		t.Fatal("withdrawal touched another owner's pin, or left an unpinned object unscheduled")
	}
	wantRefused("an unverified access row withdrawn", "", inTx(exec(`UPDATE federation_received_asset_files SET state = 'withdrawn'
		WHERE received_asset_id = $1 AND position = 2`, v1)))

	// Down refuses while a received copy (here: withdrawn, with its history) exists.
	other := newPeer()
	otherAsset := "https://m104.example/assets/" + uuid.NewString()
	object(hash("7"), 11)
	w1, err := record(other, otherAsset, 1, nil, nil, []frow{{0, "original", hash("7"), 11}, {1, "access", hash("8"), 12}}, true)
	if err != nil {
		t.Fatalf("another asset: %v", err)
	}
	if err := inTx(setReason(w1, 1, "timeout")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, accessOutcomesBeforeVersion); err == nil || !strings.Contains(err.Error(), "00104 down") {
		t.Fatalf("down with a received copy: %v", err)
	}
	if st, _, _ := fileState(v1, 1); st != "withdrawn" {
		t.Fatalf("the refused Down changed a copy: %s", st)
	}
	// Without one, Down restores 00103 and keeps recorded reasons as data.
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM federation_peers WHERE id = $1`, peer); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, accessOutcomesBeforeVersion); err != nil {
		t.Fatalf("down to %d: %v", accessOutcomesBeforeVersion, err)
	}
	if constraintExists(t, sqlDB, "federation_received_asset_files_access_withdrawn_check") ||
		count(`SELECT count(*) FROM pg_trigger WHERE tgname = 'federation_received_asset_files_commit_check_trg'`) != 0 ||
		count(`SELECT count(*) FROM pg_proc WHERE proname = 'federation_received_asset_files_commit_check'`) != 0 {
		t.Fatal("Down left 00104's access shape")
	}
	if !columnExists(t, sqlDB, "federation_received_asset_files", "access_failure_reason") {
		t.Fatal("Down dropped the recorded reasons")
	}
	if _, reason, _ := fileState(w1, 1); reason.String != "timeout" {
		t.Fatalf("Down lost a recorded reason: %v", reason)
	}
	wantRefused("an access row recorded under 00103", "", inTx(setReason(w1, 1, "fixity_mismatch")))
	wantRefused("an access row verified under 00103", "only a pending original is updated", inTx(verify(w1, 1)))

	// Up again reuses the column and its reasons.
	if _, err := p.UpTo(ctx, accessOutcomesAtVersion); err != nil {
		t.Fatalf("up again: %v", err)
	}
	wantTriggerScope("after Up again")
	if _, reason, _ := fileState(w1, 1); reason.String != "timeout" {
		t.Fatalf("Up again lost a recorded reason: %v", reason)
	}
	if err := inTx(setReason(w1, 1, "fixity_mismatch")); err != nil {
		t.Fatalf("record a failure after Up again: %v", err)
	}
}
