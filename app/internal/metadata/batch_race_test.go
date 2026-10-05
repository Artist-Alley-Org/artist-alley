// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE SEVEN RACE SEAMS — the batch's four atomicity invariant families,
// proven (#1173, #1119, ADR 0019).
//
// # What the sequential suite cannot prove
//
// batch_authz_e2e_test.go proves the RULES: an ownership transfer
// between preview and apply produces unauthorized_at_apply, a
// definition change produces 409 definition_drift, a deleted reference
// target produces 409 reference_invalidated. Every one of those
// assertions PASSES against an implementation that reads the world,
// thinks about it, and then writes — because nothing else is running.
//
// The window they leave open is exactly the case the invariants exist
// for. An ownership transfer that commits BETWEEN the gate's read and
// the write it authorises produces a write the gate would have refused,
// and no sequential test can enter that window.
//
// # "Same transaction" is not sufficient, which is why these exist
//
// At READ COMMITTED, a transaction that reads `assets` and then writes
// `asset_field_value` sees a snapshot per STATEMENT. 20a's guarded-write
// pattern does not transfer: its precondition and its mutation are the
// same row and therefore one statement, while here the precondition is
// on `assets` and the mutation lands on a DIFFERENT TABLE. Nor does the
// foreign key help — its implicit lock is FOR KEY SHARE, which
// conflicts only with FOR UPDATE, while ownership transfer, team move
// and soft delete all take FOR NO KEY UPDATE and slip straight through.
//
// # THE SEAM, and why it is not a sleep
//
// This is display_condition_race_test.go's mechanism, itself derived
// from 20a's field_value_race_test.go, applied to seven different
// locks. A test that fires two operations and hopes they collide proves
// nothing: on a quiet machine the first finishes before the second's
// connection is checked out, and the test then reports green against
// the very implementation it was written to catch. A sleep is the same
// failure with a longer runtime.
//
// A HELD LOCK plus an OBSERVED WAIT:
//
//  1. A gate transaction performs the COMPETING MUTATION and holds it
//     uncommitted. Its own write is the lock — an UPDATE of an `assets`
//     row takes FOR NO KEY UPDATE, which is precisely what the batch's
//     FOR SHARE conflicts with.
//  2. The contender — a real apply, over HTTP-shaped handler calls on
//     its OWN POOL with a distinctive application_name — is launched.
//     It runs its whole handler and BLOCKS at the locked read, BEFORE
//     reading the state its invariant depends on. That is the property
//     that matters: a lock taken AFTER the read would serialise the
//     writes while still letting the batch authorise against a world
//     that had already moved.
//  3. The test WAITS UNTIL pg_stat_activity reports the contender's
//     backend waiting on a lock. AN OBSERVATION OF STATE, NOT AN
//     ELAPSED DURATION — and it FAILS OUTRIGHT if the overlap never
//     happens, rather than quietly proving nothing.
//  4. The gate COMMITS. The contender proceeds, sees the committed
//     change, and must answer for the world as it now is.
package metadata_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/posts"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// batchRaceEnv gives the CONTENDER its own pool with a distinctive
// application_name, so the wait observation cannot be confused by
// another package's tests sharing the database.
type batchRaceEnv struct {
	*batchFixture
	contender *metadata.Handler
	pool      *pgxpool.Pool
	appName   string
}

func newBatchRaceEnv(t *testing.T) *batchRaceEnv {
	t.Helper()
	base := newBatchFixture(t)

	appName := fmt.Sprintf("aa-batchrace-%d", time.Now().UnixNano())
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s dbname=%s sslmode=disable password=%s application_name=%s pool_max_conns=8",
		envOr("AA_DB_HOST", "postgres"), envOr("AA_DB_PORT", "5432"),
		envOr("AA_DB_USER", "artist_alley"), testdb.Name(t),
		os.Getenv("AA_DB_PASSWORD"), appName,
	)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("contender pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("contender pool ping: %v", err)
	}
	t.Cleanup(pool.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := metadata.NewHandler(pool, logger, nil)
	h.Audit = audit.NewRecorder(pool, logger)
	return &batchRaceEnv{batchFixture: base, contender: h, pool: pool, appName: appName}
}

// gate performs the competing mutation and HOLDS IT UNCOMMITTED. The
// gate's own write is the lock: an UPDATE of a row takes FOR NO KEY
// UPDATE, which is exactly what the batch's FOR SHARE read conflicts
// with, so the contender piles up behind it.
type raceGate struct {
	tx       pgx.Tx
	t        *testing.T
	released bool
}

// autoRelease rolls the gate back if the test aborts before releasing it.
//
// Without this a FAILING race test hangs instead of failing: the gate's
// uncommitted mutation holds row locks that the fixture's cleanup then
// waits on forever. A test that hangs on failure is a test whose failure
// nobody reads.
func (g *raceGate) autoRelease() {
	g.t.Cleanup(func() {
		if !g.released {
			_ = g.tx.Rollback(context.Background())
		}
	})
}

func (e *batchRaceEnv) openGate(sql string, args ...any) *raceGate {
	e.t.Helper()
	tx, err := e.batchFixture.pool.Begin(context.Background())
	if err != nil {
		e.t.Fatalf("gate begin: %v", err)
	}
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		_ = tx.Rollback(context.Background())
		e.t.Fatalf("gate mutation: %v", err)
	}
	g := &raceGate{tx: tx, t: e.t}
	g.autoRelease()
	return g
}

// openStaleVerdictGate holds the exclusive authority lock AND a subject
// asset row, then performs a real authority mutation inside both.
//
// Two locks because the two trees park in different places. The
// corrected batch takes the shared authority lock BEFORE its authority
// read and parks there. The uncorrected batch takes no authority lock at
// all, sails through the read with a stale "allowed", and parks on the
// subject row it takes afterwards. Holding both means the overlap is
// real and OBSERVED in either tree — so the test measures what the batch
// DOES with the window rather than whether it has one.
func (e *batchRaceEnv) openStaleVerdictGate(userRef int64, subject uuid.UUID, sql string, args ...any) *raceGate {
	e.t.Helper()
	tx, err := e.batchFixture.pool.Begin(context.Background())
	if err != nil {
		e.t.Fatalf("stale-verdict gate begin: %v", err)
	}
	g := &raceGate{tx: tx, t: e.t}
	g.autoRelease()
	if err := auth.LockAuthorityForUpdate(context.Background(), tx, userRef); err != nil {
		e.t.Fatalf("stale-verdict gate authority lock: %v", err)
	}
	if _, err := tx.Exec(context.Background(),
		`SELECT id FROM assets WHERE id = $1 FOR UPDATE`, subject); err != nil {
		e.t.Fatalf("stale-verdict gate subject lock: %v", err)
	}
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("stale-verdict gate mutation: %v", err)
	}
	return g
}

// openAuthorityGate holds the EXCLUSIVE half of the production
// authority lock and performs a real authority mutation inside it,
// uncommitted.
//
// ⛔ It takes the lock through auth.LockAuthorityForUpdate — the SAME
// exported call the admin grant and revoke handlers, the role
// assignment path, the expiry sweeper and the team-closure paths make.
// That is the whole point: the previous version of this seam wrapped its
// revoke in a `field_definition ... FOR UPDATE` that no production path
// takes, which manufactured the ordering it claimed to observe. A gate
// that reaches for an unrelated artifact proves nothing about
// production.
func (e *batchRaceEnv) openAuthorityGate(userRef int64, sql string, args ...any) *raceGate {
	e.t.Helper()
	tx, err := e.batchFixture.pool.Begin(context.Background())
	if err != nil {
		e.t.Fatalf("authority gate begin: %v", err)
	}
	if err := auth.LockAuthorityForUpdate(context.Background(), tx, userRef); err != nil {
		_ = tx.Rollback(context.Background())
		e.t.Fatalf("authority gate lock: %v", err)
	}
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		_ = tx.Rollback(context.Background())
		e.t.Fatalf("authority gate mutation: %v", err)
	}
	g := &raceGate{tx: tx, t: e.t}
	g.autoRelease()
	return g
}

// commit releases the gate by COMMITTING the competing change, so the
// contender resumes into a world that has genuinely moved.
func (g *raceGate) commit() {
	g.t.Helper()
	g.released = true
	if err := g.tx.Commit(context.Background()); err != nil {
		g.t.Fatalf("gate commit: %v", err)
	}
}

// waitForBlockedContender IS THE HAPPENS-BEFORE WITNESS.
//
// It fails the test rather than continuing if the overlap never
// materialises, because a race test that quietly ran its contender
// after the gate had finished is a test that reports green for the bug
// it exists to catch.
func (e *batchRaceEnv) waitForBlockedContender(t *testing.T, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := e.batchFixture.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND application_name = $1
			   AND wait_event_type = 'Lock'`, e.appName).Scan(&n); err != nil {
			t.Fatalf("observe contender: %v", err)
		}
		if n >= 1 {
			t.Logf("synchronisation seam: the apply is observed BLOCKED on the %s lock, "+
				"before it could read the state its invariant depends on", what)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the apply never blocked on the %s lock — the overlap this test asserts never "+
		"happened, so it would have proved nothing", what)
}

// applyOnContender runs a real apply on the contender's pool.
func (e *batchRaceEnv) applyOnContender(ctx context.Context, token, reason string, confirm *int) applyResult {
	body := openapi.BatchAssetFieldApplyRequest{Token: token, Reason: reason, ConfirmCount: confirm}
	resp, err := e.contender.ApplyBatchAssetFieldEdit(ctx,
		openapi.ApplyBatchAssetFieldEditRequestObject{Body: &body})
	if err != nil {
		return applyResult{Status: 500}
	}
	out := applyResult{}
	switch v := resp.(type) {
	case openapi.ApplyBatchAssetFieldEdit200JSONResponse:
		r := openapi.BatchAssetFieldApplyResult(v)
		out.OK, out.Status = &r, 200
	case openapi.ApplyBatchAssetFieldEdit400JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 400
	case openapi.ApplyBatchAssetFieldEdit403JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 403
	case openapi.ApplyBatchAssetFieldEdit409JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 409
	case openapi.ApplyBatchAssetFieldEdit422JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 422
	}
	return out
}

// race launches the apply, waits until it is OBSERVED BLOCKED, releases
// the gate, and returns the outcome.
func (e *batchRaceEnv) race(
	t *testing.T, what string, gate *raceGate,
	ctx context.Context, token, reason string, confirm *int,
) applyResult {
	t.Helper()
	done := make(chan applyResult, 1)
	go func() { done <- e.applyOnContender(ctx, token, reason, confirm) }()
	e.waitForBlockedContender(t, what)
	gate.commit()
	select {
	case res := <-done:
		return res
	case <-time.After(30 * time.Second):
		t.Fatal("the apply never completed after the gate released")
		return applyResult{}
	}
}

// ---------------------------------------------------------------------------
// A70 — SEAM 1: OWNERSHIP
// ---------------------------------------------------------------------------

// The target's owner changes while the apply is blocked BEFORE reading
// it. Without the FOR SHARE the apply would authorise against the old
// owner and write a field value on an asset its caller no longer has
// any authority over.
func TestBatchRace_OwnershipTransfer(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("raceown")
	stranger := e.user("newowner")
	field := e.field("t", fieldSpec{Type: "text"})
	asset := e.asset(&owner, nil)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("batch"), assetEntries(asset))

	gate := e.openGate(`UPDATE assets SET owner_user_ref = $1 WHERE id = $2`, stranger, asset)
	res := e.race(t, "asset row", gate, ctx, p.Token, "ownership moved under us", intp(1))

	if res.OK == nil {
		t.Fatalf("the batch commits its result: %+v", res.Refusal)
	}
	got, _ := outcomeOf(res.OK, asset)
	if got != openapi.BatchOutcomeUnauthorizedAtApply {
		t.Fatalf("the transfer committed BEFORE the gate read: want unauthorized_at_apply, got %s", got)
	}
	for _, tgt := range res.OK.Targets {
		if tgt.UnauthorizedReason == nil || *tgt.UnauthorizedReason != openapi.BatchUnauthorizedSubjectAuthority {
			t.Fatalf("want the subject_authority sub-reason, got %v", tgt.UnauthorizedReason)
		}
	}
	if e.rowExists(asset, field) {
		t.Fatal("NOTHING may be written on an asset the caller no longer has authority over")
	}
}

// ---------------------------------------------------------------------------
// A71 — SEAM 2: TEAM MOVE
// ---------------------------------------------------------------------------

// The target moves to a team the caller's SCOPED bulk grant does not
// cover, while the apply is blocked before reading its team.
func TestBatchRace_TeamMove(t *testing.T) {
	e := newBatchRaceEnv(t)
	teamA := e.team("raceA")
	teamB := e.team("raceB")
	owner := e.user("raceteam")
	e.grant(owner, capBulkEdit, &teamA)
	e.grant(owner, "assets.admin", &teamA)
	e.grant(owner, "assets.admin", &teamB)
	ctx := e.identity(owner)

	field := e.field("t", fieldSpec{Type: "text"})
	asset := e.asset(nil, &teamA)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("batch"), assetEntries(asset))
	if p.Counts.WouldChange != 1 {
		t.Fatalf("the target is in scope at preview; got %+v", p.Counts)
	}

	gate := e.openGate(`UPDATE assets SET team_id = $1 WHERE id = $2`, teamB, asset)
	res := e.race(t, "asset row", gate, ctx, p.Token, "team moved under us", intp(1))

	if res.OK == nil {
		t.Fatalf("apply refused wholesale: %+v", res.Refusal)
	}
	got, _ := outcomeOf(res.OK, asset)
	if got != openapi.BatchOutcomeUnauthorizedAtApply {
		t.Fatalf("want unauthorized_at_apply, got %s", got)
	}
	if e.rowExists(asset, field) {
		t.Fatal("the moved asset must not be written")
	}
}

// ---------------------------------------------------------------------------
// A72 — SEAM 3: SUBJECT SOFT-DELETE
// ---------------------------------------------------------------------------

func TestBatchRace_SubjectSoftDelete(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("racedel")
	field := e.field("t", fieldSpec{Type: "text"})
	doomed := e.asset(&owner, nil)
	survivor := e.asset(&owner, nil)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("batch"),
		assetEntries(doomed, survivor))

	gate := e.openGate(`UPDATE assets SET deleted_at = NOW() WHERE id = $1`, doomed)
	res := e.race(t, "asset row", gate, ctx, p.Token, "target deleted under us", intp(2))

	if res.OK == nil {
		t.Fatalf("the rest of the batch proceeds: %+v", res.Refusal)
	}
	if got, _ := outcomeOf(res.OK, doomed); got != openapi.BatchOutcomeGone {
		t.Fatalf("want gone, got %s", got)
	}
	if got, _ := outcomeOf(res.OK, survivor); got != openapi.BatchOutcomeChanged {
		t.Fatalf("the survivor must still be written, got %s", got)
	}
	if e.rowExists(doomed, field) {
		t.Fatal("a soft-deleted target must not be written")
	}
}

// ---------------------------------------------------------------------------
// A73 — SEAM 4: DEFINITION / CONFIGURATION DRIFT
// ---------------------------------------------------------------------------

// The field is reconfigured while the apply is blocked BEFORE reading
// it. EXACTLY TWO SERIAL OUTCOMES are permitted, and this asserts the
// first: the external change wins, the batch refuses, ZERO WRITES.
//
// PARTIAL WRITES ARE ASSERTED IMPOSSIBLE — the forbidden third outcome
// is the first N targets written under the old rules and the rest under
// the new ones, so the assertion is made on EVERY target and not on the
// status alone.
func TestBatchRace_DefinitionDrift(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("racedef")
	field := e.field("t", fieldSpec{Type: "text"})
	targets := []uuid.UUID{e.asset(&owner, nil), e.asset(&owner, nil), e.asset(&owner, nil)}

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("batch"), assetEntries(targets...))

	// read_only is a definition property the batch's fingerprint covers.
	gate := e.openGate(`UPDATE field_definition SET read_only = true WHERE id = $1`, field)
	res := e.race(t, "field_definition row", gate, ctx, p.Token, "definition moved under us", intp(3))

	e.wantRefusal(res, 409, openapi.BatchDefinitionDrift)
	for _, a := range targets {
		if e.rowExists(a, field) {
			t.Fatalf("PARTIAL WRITE: asset %s was written under the pre-change rules", a)
		}
		if e.historyCount(a, field) != 0 {
			t.Fatalf("PARTIAL WRITE: asset %s gained a history row", a)
		}
	}
	if e.tokenConsumed(p.OperationId.String()) {
		t.Fatal("a batch-wide refusal leaves the token spendable")
	}
	if e.envelopes(p.OperationId.String()) != 0 {
		t.Fatal("a batch-wide refusal commits no envelope")
	}
}

// ---------------------------------------------------------------------------
// A74 — SEAM 5: VOCABULARY DRIFT
// ---------------------------------------------------------------------------

// ⚠️ A POST-PREVIEW-PRE-APPLY DRIFT TEST DOES NOT SATISFY THIS ROW. The
// curation must commit while the apply is OBSERVED BLOCKED, before it
// has read the options document it validates against — otherwise the
// test proves a comparison rather than an atomicity.
func TestBatchRace_VocabularyDrift(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("racevocab")
	field := e.field("ms", fieldSpec{Type: "multi_select", Options: []map[string]any{
		vocabOption("a", "A", "active"), vocabOption("b", "B", "active"),
	}})
	a1 := e.asset(&owner, nil)
	a2 := e.asset(&owner, nil)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, optionsValue("a"), assetEntries(a1, a2))

	gate := e.openGate(
		`UPDATE field_definition SET options = $1 WHERE id = $2`,
		[]byte(`{"values":[{"value":"a","label":"A","status":"deprecated"},{"value":"b","label":"B"}]}`),
		field)
	res := e.race(t, "field_definition row", gate, ctx, p.Token, "vocabulary curated under us", intp(2))

	e.wantRefusal(res, 409, openapi.BatchVocabularyDrift)
	for _, a := range []uuid.UUID{a1, a2} {
		if e.rowExists(a, field) {
			t.Fatalf("ZERO WRITES: asset %s was written", a)
		}
	}
	if e.tokenConsumed(p.OperationId.String()) {
		t.Fatal("the token stays spendable")
	}
}

// ---------------------------------------------------------------------------
// A75 — SEAM 6: REFERENCE LIVENESS
// ---------------------------------------------------------------------------

// THE CONTENTION IS OBSERVED. There is NO FOREIGN KEY on `value_ref` —
// asset_field_value has exactly two, on asset_id and field_id — so
// nothing in the schema stops the target being soft-deleted midway
// through a thousand writes pointing at it, and a pre-batch re-check
// establishes a fact that can stop being true before the last write
// lands.
func TestBatchRace_ReferenceLiveness(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("raceref")
	field := e.field("ref", fieldSpec{Type: "reference"})
	subject := e.asset(&owner, nil)
	target := e.asset(&owner, nil)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, refValue(target), assetEntries(subject))

	gate := e.openGate(`UPDATE assets SET deleted_at = NOW() WHERE id = $1`, target)
	res := e.race(t, "reference target row", gate, ctx, p.Token, "reference target deleted under us", intp(1))

	e.wantRefusal(res, 409, openapi.BatchReferenceInvalidated)
	if e.rowExists(subject, field) {
		t.Fatal("ZERO field writes may point at a target that stopped resolving")
	}
	if e.tokenConsumed(p.OperationId.String()) {
		t.Fatal("the token stays spendable")
	}
}

// ---------------------------------------------------------------------------
// A76 — SEAM 7: EFFECTIVE AUTHORITY
// ---------------------------------------------------------------------------

// The batch reads the caller's EFFECTIVE AUTHORITY and then writes and
// mints under that verdict. Those two must be serialized, or an
// authority change can commit in between and the stale verdict still
// authorizes the mutation.
//
// # Why the previous version of this test proved nothing
//
// It made its competing revoke run inside
// `WITH held AS (SELECT id FROM field_definition ... FOR UPDATE)`, so
// the revoke waited on the field lock the apply already takes. ⛔ A
// PRODUCTION REVOKE NEVER TOUCHES `field_definition`. It writes
// `user_capability_revokes`, or deletes from `user_capability_grants`,
// or changes `user_roles` — tables the batch locked nothing in. The
// test manufactured the very ordering it claimed to prove and was green
// over an unproven invariant.
//
// # What this version does instead
//
// The gate is the REAL production mutation, taking the REAL production
// lock: `auth.LockAuthorityForUpdate`, the same call the admin
// grant/revoke handlers, the role-assignment path, the expiry sweeper
// and the team-closure paths make. Nothing here reaches for an
// unrelated artifact to create safety with.
//
// The dangerous ordering is then constructed exactly: the apply is held
// so that its authority READ has not happened, the revoke commits, and
// the apply proceeds into the read and the write. If the batch did not
// hold the shared half of that lock, it would read the pre-revoke
// verdict and mint under it.
func TestBatchRace_EffectiveAuthorityRevocation(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, _ := e.bulkOperator("raceauth")
	e.grant(owner, capVocabExtend, nil)
	ctx := e.identity(owner)

	field := e.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	// N >= 2, so a partial outcome would be visible: the batch must not
	// write the first target under the stale verdict and refuse the rest.
	a1 := e.asset(&owner, nil)
	a2 := e.asset(&owner, nil)
	a3 := e.asset(&owner, nil)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
		optionsValue("race-term"), assetEntries(a1, a2, a3))
	if p.Counts.WouldChange != 3 {
		t.Fatalf("the fixture needs three would_change targets, got %+v", p.Counts)
	}
	if p.MintableTerms == nil || len(*p.MintableTerms) != 1 {
		t.Fatalf("the fixture needs a mintable term, got %+v", p.MintableTerms)
	}
	optionsBefore := string(e.optionsDoc(field))

	// THE GATE IS A PRODUCTION AUTHORITY MUTATION. It takes the exclusive
	// half of the authority lock — via the same exported call the admin
	// handlers use — and then performs the revoke as a plain DELETE, with
	// no reference to any other table.
	gate := e.openAuthorityGate(owner, `
		DELETE FROM user_capability_grants
		 WHERE user_ref = $1 AND capability_code = $2 AND team_id IS NULL`,
		owner, capVocabExtend)

	res := e.race(t, "authority lock", gate, ctx, p.Token, "authority revoked under us", intp(3))

	e.wantRefusal(res, 403, openapi.BatchVocabularyExtendRequired)

	// ZERO UNAUTHORIZED WRITES, on every target — no partial batch.
	for i, a := range []uuid.UUID{a1, a2, a3} {
		if e.rowExists(a, field) {
			t.Fatalf("target %d was written under a REVOKED verdict", i)
		}
		if e.historyCount(a, field) != 0 {
			t.Fatalf("target %d gained a history row under a REVOKED verdict", i)
		}
	}
	// ZERO UNAUTHORIZED MINTS.
	if after := string(e.optionsDoc(field)); after != optionsBefore {
		t.Fatalf("a term was minted under a REVOKED verdict\nbefore=%s\nafter=%s",
			optionsBefore, after)
	}
	if e.tokenConsumed(p.OperationId.String()) {
		t.Fatal("a batch-wide refusal leaves the token spendable")
	}
	if e.envelopes(p.OperationId.String()) != 0 {
		t.Fatal("and commits no audit envelope")
	}
}

// TestBatchRace_StaleVerdictCannotAuthorizeTheWrite is THE HARM PROOF,
// and it is the one that matters most.
//
// The two seams above fail on the uncorrected code because the apply
// never blocks on a lock it does not take — a true and useful signal,
// but it says "the mechanism is absent" rather than "the absence lets a
// forbidden write through". This test says the second thing.
//
// # The dangerous ordering, constructed exactly
//
// The gate holds TWO things: the exclusive authority lock, and the
// SUBJECT ASSET ROW. The subject row is a lock the uncorrected batch
// genuinely takes, and it takes it AFTER it has read authority. So on
// the uncorrected code the apply gets all the way past its authority
// read with a verdict of "allowed", parks on the subject row, watches
// the revoke commit, and then proceeds to write under a verdict that is
// no longer true.
//
// On the corrected code it never gets that far: the shared authority
// lock is taken BEFORE the read, so the apply parks there instead, and
// when it resumes it reads the revoked state and refuses.
//
// Either way the overlap is real and observed. What differs is what the
// batch does with it.
func TestBatchRace_StaleVerdictCannotAuthorizeTheWrite(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, _ := e.bulkOperator("racestale")
	e.grant(owner, capVocabExtend, nil)
	ctx := e.identity(owner)

	field := e.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	// N >= 2: a partial outcome must be visible if one occurs.
	a1 := e.asset(&owner, nil)
	a2 := e.asset(&owner, nil)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
		optionsValue("stale-term"), assetEntries(a1, a2))
	if p.Counts.WouldChange != 2 {
		t.Fatalf("want 2 would_change, got %+v", p.Counts)
	}
	optionsBefore := string(e.optionsDoc(field))

	// Hold the authority lock AND the first subject row, then revoke.
	gate := e.openStaleVerdictGate(owner, a1, `
		DELETE FROM user_capability_grants
		 WHERE user_ref = $1 AND capability_code = $2 AND team_id IS NULL`,
		owner, capVocabExtend)

	res := e.race(t, "authority lock or subject row", gate, ctx, p.Token,
		"authority revoked while the batch was in flight", intp(2))

	// THE ASSERTION. However the batch got here, a revoked caller may not
	// have written or minted anything.
	for i, a := range []uuid.UUID{a1, a2} {
		if e.rowExists(a, field) {
			t.Fatalf("STALE VERDICT AUTHORIZED A WRITE: target %d was written after the "+
				"caller's authority was revoked", i)
		}
		if e.historyCount(a, field) != 0 {
			t.Fatalf("STALE VERDICT AUTHORIZED A WRITE: target %d gained a history row", i)
		}
	}
	if after := string(e.optionsDoc(field)); after != optionsBefore {
		t.Fatalf("STALE VERDICT AUTHORIZED A MINT\nbefore=%s\nafter=%s", optionsBefore, after)
	}
	if res.OK != nil && res.OK.OutcomeCounts.Changed != 0 {
		t.Fatalf("STALE VERDICT AUTHORIZED %d WRITES", res.OK.OutcomeCounts.Changed)
	}
	// And no partial batch: the operation refuses whole.
	e.wantRefusal(res, 403, openapi.BatchVocabularyExtendRequired)
	if e.tokenConsumed(p.OperationId.String()) {
		t.Fatal("a batch-wide refusal leaves the token spendable")
	}
}

// TestBatchRace_StructuralAuthorityLockExcludesTheBatch drives the REAL batch
// against the STRUCTURAL half of the authority lock — the half `aa seed`
// takes.
//
// The two seams above use the per-user key, which the admin grant and
// revoke endpoints take. The structural key is what a mutation with an
// unnameable blast radius takes: the expiry sweeper, a team re-parenting,
// and `aa seed --reset`, whose TRUNCATE ... CASCADE empties
// `user_roles`, `user_capability_grants` and `user_capability_revokes`
// wholesale.
//
// ⛔ WHAT THIS PROVES, AND WHAT IT DOES NOT. The gate takes the lock
// through auth.AcquireStructuralAuthorityLock — the same exported call
// the seed spans make — and performs a seed-equivalent authority
// mutation. So it proves the PRIMITIVE excludes a batch correctly, with
// N = 3 and no partial write.
//
// It does NOT prove that any production caller takes it. That is a
// separate claim and it is proven separately, by driving the real
// callers: TestSeedReset_WaitsForAnInFlightAuthorityReader over
// `resetContent`, and the two phase tests in internal/seed over
// applyTeams and applyFixturePrincipals. Reading this test as caller
// evidence is the mistake that let a missing writer through twice.
//
// N = 3, so a partial outcome would be visible.
func TestBatchRace_StructuralAuthorityLockExcludesTheBatch(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, _ := e.bulkOperator("raceseed")
	ctx := e.identity(owner)

	field := e.field("t", fieldSpec{Type: "text"})
	a1 := e.asset(&owner, nil)
	a2 := e.asset(&owner, nil)
	a3 := e.asset(&owner, nil)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("batch"),
		assetEntries(a1, a2, a3))
	if p.Counts.WouldChange != 3 {
		t.Fatalf("want 3 would_change, got %+v", p.Counts)
	}

	// The SESSION-scoped structural lock, held exactly as a seed holds
	// it across its multi-statement span.
	release, err := auth.AcquireStructuralAuthorityLock(context.Background(), e.batchFixture.pool, nil)
	if err != nil {
		t.Fatalf("structural lock: %v", err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	// The seed-equivalent mutation, committed inside the held lock.
	if _, err := e.batchFixture.pool.Exec(e.batchFixture.ctx,
		`DELETE FROM user_capability_grants WHERE user_ref = $1`, owner); err != nil {
		t.Fatalf("seed-equivalent authority wipe: %v", err)
	}

	done := make(chan applyResult, 1)
	go func() { done <- e.applyOnContender(ctx, p.Token, "seed wiped authority under us", intp(3)) }()
	e.waitForBlockedContender(t, "structural authority lock")
	released = true
	release()

	var res applyResult
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the apply never completed after the structural lock released")
	}

	e.wantRefusal(res, 403, openapi.BatchBulkCapabilityRequired)
	for i, a := range []uuid.UUID{a1, a2, a3} {
		if e.rowExists(a, field) {
			t.Fatalf("target %d was written after a seed-equivalent authority wipe", i)
		}
		if e.historyCount(a, field) != 0 {
			t.Fatalf("target %d gained a history row", i)
		}
	}
	if e.tokenConsumed(p.OperationId.String()) {
		t.Fatal("a batch-wide refusal leaves the token spendable")
	}
}

// TestBatchRace_BulkAuthorityRevocation is the same seam over a
// DIFFERENT authority kind, because A76 pointed only at
// `fields.vocabulary.extend` and that is not the whole surface. The
// batch consumes four: bulk-edit admission and per-target scope, subject
// authority, the field's own write capability, and mint authority — and
// every one of them is drawn from the SAME effective-authority read.
//
// Here the bulk instrument itself is revoked mid-flight, on a field with
// no vocabulary at all, so nothing about minting is involved.
func TestBatchRace_BulkAuthorityRevocation(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, _ := e.bulkOperator("racebulk")
	ctx := e.identity(owner)

	field := e.field("t", fieldSpec{Type: "text"})
	a1 := e.asset(&owner, nil)
	a2 := e.asset(&owner, nil)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("batch"),
		assetEntries(a1, a2))
	if p.Counts.WouldChange != 2 {
		t.Fatalf("want 2 would_change, got %+v", p.Counts)
	}

	gate := e.openAuthorityGate(owner, `
		DELETE FROM user_capability_grants
		 WHERE user_ref = $1 AND capability_code = $2 AND team_id IS NULL`,
		owner, capBulkEdit)

	res := e.race(t, "authority lock", gate, ctx, p.Token, "bulk grant revoked under us", intp(2))

	e.wantRefusal(res, 403, openapi.BatchBulkCapabilityRequired)
	for i, a := range []uuid.UUID{a1, a2} {
		if e.rowExists(a, field) {
			t.Fatalf("target %d was written after the bulk instrument was revoked", i)
		}
	}
	if e.tokenConsumed(p.OperationId.String()) {
		t.Fatal("the token stays spendable")
	}
}

// ===========================================================================
// SPRINT 20c-i: THE LOCK-ORDER CORRECTION (#1173, #1119, ADR 0019)
// ===========================================================================
//
// Post-merge Integration CI went intermittently red with SQLSTATE 40P01
// on TestBatch_AtTheCeiling. It was a real deadlock, and the cycle ran
// through two levels of trigger:
//
//	batch    holds posts[P]  -> waits assets[V]
//	ordinary holds assets[V] -> waits posts[P]
//
// An ordinary asset_field_value write rebuilds the asset's search_text,
// which UPDATEs `assets`, which fires assets_member_post_search_text,
// which rebuilds every containing post. So EVERY metadata write takes
// assets THEN posts. The batch, locking and writing one target at a
// time, ended up holding a post row from an earlier target while asking
// for the next asset row: the same two tiers, the other way round.
//
// The tests below are in TWO CLASSES and the distinction is not
// cosmetic.
//
// CLASS 1 (Test..._Regression) are deterministic real-path regressions.
// Each drives the real apply and the real ordinary writer or the real
// membership endpoints, and each FAILS against the pre-correction tree.
//
// CLASS 2 (Test..._Structural) are STRUCTURAL coverage of the corrected
// lock hierarchy. They are not red-before and are not claimed to be.
// They exist because the hierarchy is a property of the implementation
// that no amount of reading proves and a refactor can quietly break.
//
// NOTHING HERE IS TIMED. Every ordering comes from observed database
// wait state, and a run in which the intended overlap did not happen
// fails rather than passing quietly.

// writerEnv is a THIRD pool, distinct from the fixture's and from the
// batch contender's, carrying its own application_name.
//
// Three pools because these tests ask a three-way question: what the
// GATE holds, what the BATCH is parked on, and what an ORDINARY writer
// or a membership change can still do meanwhile. Sharing a pool between
// the last two would make "is the ordinary writer blocked" unanswerable
// from pg_stat_activity, which is the only place these tests get their
// ordering from.
type writerEnv struct {
	pool    *pgxpool.Pool
	meta    *metadata.Handler
	posts   *posts.Handler
	appName string
}

func (e *batchRaceEnv) writer(t *testing.T) *writerEnv {
	t.Helper()
	appName := fmt.Sprintf("aa-batchwriter-%d", time.Now().UnixNano())
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s dbname=%s sslmode=disable password=%s application_name=%s pool_max_conns=6",
		envOr("AA_DB_HOST", "postgres"), envOr("AA_DB_PORT", "5432"),
		envOr("AA_DB_USER", "artist_alley"), testdb.Name(t),
		os.Getenv("AA_DB_PASSWORD"), appName,
	)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("writer pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("writer pool ping: %v", err)
	}
	t.Cleanup(pool.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	meta := metadata.NewHandler(pool, logger, nil)
	// The recorder is not optional. A second batch driven on this pool
	// must write its envelope like any other, or "exactly one envelope
	// each" would be asserting against a handler that writes none.
	meta.Audit = audit.NewRecorder(pool, logger)
	return &writerEnv{
		pool:    pool,
		meta:    meta,
		posts:   posts.NewHandler(pool, logger, nil),
		appName: appName,
	}
}

// setFieldValue drives the REAL ordinary single-target writer, the same
// handler the metadata editor calls. Not a hand-rolled upsert: the
// deadlock lives in the trigger chain that handler's write sets off,
// and a test that issued the INSERT itself would still reproduce it,
// but would stop being evidence about the endpoint the moment the
// endpoint stopped writing that row that way.
func (w *writerEnv) setFieldValue(ctx context.Context, asset, field uuid.UUID, text string) error {
	v := text
	body := openapi.AssetFieldValueWrite{ValueText: &v}
	resp, err := w.meta.SetAssetFieldValue(ctx, openapi.SetAssetFieldValueRequestObject{
		Id: openapi_types.UUID(asset), FieldId: openapi_types.UUID(field), Body: &body,
	})
	if err != nil {
		return err
	}
	if _, ok := resp.(openapi.SetAssetFieldValue200JSONResponse); !ok {
		return fmt.Errorf("the ordinary write was refused: %T", resp)
	}
	return nil
}

// addMember drives the REAL membership endpoint.
func (w *writerEnv) addMember(ctx context.Context, post, asset uuid.UUID) error {
	body := openapi.AddPostAssetJSONRequestBody{AssetId: openapi_types.UUID(asset)}
	resp, err := w.posts.AddPostAsset(ctx, openapi.AddPostAssetRequestObject{
		Id: openapi_types.UUID(post), Body: &body,
	})
	if err != nil {
		return err
	}
	if _, ok := resp.(openapi.AddPostAsset204Response); !ok {
		return fmt.Errorf("the membership addition was refused: %T", resp)
	}
	return nil
}

// removeMember drives the REAL membership endpoint.
func (w *writerEnv) removeMember(ctx context.Context, post, asset uuid.UUID) error {
	resp, err := w.posts.RemovePostAsset(ctx, openapi.RemovePostAssetRequestObject{
		Id: openapi_types.UUID(post), AssetId: openapi_types.UUID(asset),
	})
	if err != nil {
		return err
	}
	if _, ok := resp.(openapi.RemovePostAsset204Response); !ok {
		return fmt.Errorf("the membership removal was refused: %T", resp)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Gates that hold a LOCK and nothing else
// ---------------------------------------------------------------------------

// openAssetLockGate holds ONE assets row and performs no mutation.
//
// Deliberately not openGate's `UPDATE assets ...`: an UPDATE of an
// assets row fires assets_member_post_search_text and would leave the
// gate holding every containing POST as well. A gate that holds more
// than the seam under test cannot tell you which lock the contender
// stopped at.
func (e *batchRaceEnv) openAssetLockGate(id uuid.UUID) *raceGate {
	e.t.Helper()
	tx, err := e.batchFixture.pool.Begin(context.Background())
	if err != nil {
		e.t.Fatalf("asset gate begin: %v", err)
	}
	g := &raceGate{tx: tx, t: e.t}
	g.autoRelease()
	if _, err := tx.Exec(context.Background(),
		`SELECT id FROM assets WHERE id = $1 FOR UPDATE`, id); err != nil {
		e.t.Fatalf("asset gate lock: %v", err)
	}
	return g
}

// openPostLockGate holds the given posts in ASCENDING id order, in the
// same FOR NO KEY UPDATE mode rebuild_post_search_text takes, and
// performs no mutation.
func (e *batchRaceEnv) openPostLockGate(ids ...uuid.UUID) *raceGate {
	e.t.Helper()
	sorted := append([]uuid.UUID(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	tx, err := e.batchFixture.pool.Begin(context.Background())
	if err != nil {
		e.t.Fatalf("post gate begin: %v", err)
	}
	g := &raceGate{tx: tx, t: e.t}
	g.autoRelease()
	for _, id := range sorted {
		if _, err := tx.Exec(context.Background(),
			`SELECT id FROM posts WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
			e.t.Fatalf("post gate lock: %v", err)
		}
	}
	return g
}

// openValueRowGate holds ONE asset_field_value row, which is where a
// batch parks MID-WRITE-PHASE: it has locked its whole asset tier and
// written the targets before this one, and is now blocked on the write
// itself rather than on any lock it takes deliberately.
func (e *batchRaceEnv) openValueRowGate(asset, field uuid.UUID) *raceGate {
	e.t.Helper()
	tx, err := e.batchFixture.pool.Begin(context.Background())
	if err != nil {
		e.t.Fatalf("value gate begin: %v", err)
	}
	g := &raceGate{tx: tx, t: e.t}
	g.autoRelease()
	var got string
	if err := tx.QueryRow(context.Background(),
		`SELECT asset_id::text FROM asset_field_value
		  WHERE asset_id = $1 AND field_id = $2 FOR UPDATE`, asset, field).Scan(&got); err != nil {
		e.t.Fatalf("value gate lock: %v", err)
	}
	return g
}

// ---------------------------------------------------------------------------
// Observing the database's own wait state
// ---------------------------------------------------------------------------

// waitForBlockedApp is waitForBlockedContender for any pool, and it
// fails rather than continuing when the overlap never materialises.
func (e *batchRaceEnv) waitForBlockedApp(t *testing.T, appName, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := e.batchFixture.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND application_name = $1
			   AND wait_event_type = 'Lock'`, appName).Scan(&n); err != nil {
			t.Fatalf("observe %s: %v", appName, err)
		}
		if n >= 1 {
			t.Logf("synchronisation seam: %s is observed BLOCKED on the %s lock", appName, what)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never blocked on the %s lock: the overlap this test asserts never happened, "+
		"so it would have proved nothing", appName, what)
}

// blockedByCount reports how many backends of `waiter` are currently
// blocked by a backend of `blocker`, straight out of pg_blocking_pids.
func (e *batchRaceEnv) blockedByCount(t *testing.T, waiterApp, blockerApp string) int {
	t.Helper()
	var n int
	if err := e.batchFixture.pool.QueryRow(context.Background(), `
		SELECT count(*)
		  FROM pg_stat_activity w
		  CROSS JOIN LATERAL unnest(pg_blocking_pids(w.pid)) AS b(pid)
		  JOIN pg_stat_activity holder ON holder.pid = b.pid
		 WHERE w.datname = current_database()
		   AND w.application_name = $1
		   AND holder.application_name = $2`, waiterApp, blockerApp).Scan(&n); err != nil {
		t.Fatalf("read pg_blocking_pids: %v", err)
	}
	return n
}

// waitingTupleLocks reports the relation and the row identity of every
// tuple lock a backend of `appName` currently carries.
//
// A tuple lock is what PostgreSQL takes on the row a transaction must
// WAIT for before it queues on the holder's transaction id, so this is
// the precise answer to "which row is it stopped at" rather than an
// inference from which query it last reported.
func (e *batchRaceEnv) waitingTupleLocks(t *testing.T, appName string) map[string][]string {
	t.Helper()
	rows, err := e.batchFixture.pool.Query(context.Background(), `
		SELECT l.relation::regclass::text, l.page, l.tuple
		  FROM pg_stat_activity a
		  JOIN pg_locks l ON l.pid = a.pid
		 WHERE a.datname = current_database()
		   AND a.application_name = $1
		   AND l.locktype = 'tuple'`, appName)
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var rel string
		var page, tuple int64
		if err := rows.Scan(&rel, &page, &tuple); err != nil {
			t.Fatalf("scan pg_locks: %v", err)
		}
		out[rel] = append(out[rel], fmt.Sprintf("(%d,%d)", page, tuple))
	}
	return out
}

// assetAtCtid resolves a heap tuple identifier back to the asset id, so
// "blocked on an assets ROW" can name WHICH row rather than only the
// table.
func (e *batchRaceEnv) assetAtCtid(t *testing.T, ctid string) (uuid.UUID, bool) {
	t.Helper()
	var id uuid.UUID
	err := e.batchFixture.pool.QueryRow(context.Background(),
		`SELECT id FROM assets WHERE ctid = $1::tid`, ctid).Scan(&id)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// ---------------------------------------------------------------------------
// Fixture helpers these seams need
// ---------------------------------------------------------------------------

// sortedAssets seeds n identical assets and returns their ids ASCENDING.
//
// The batch walks its targets by asset id, so "the earlier target" and
// "the later target" are facts about the ids rather than about the
// order they were created in. Sorting is how a test names them.
func (f *batchFixture) sortedAssets(owner int64, n int) []uuid.UUID {
	f.t.Helper()
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, f.asset(&owner, nil))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// sortedPosts seeds n EMPTY posts and returns their ids ASCENDING, so
// the caller can attach members in a chosen physical order afterwards.
func (f *batchFixture) sortedPosts(owner int64, n int) []uuid.UUID {
	f.t.Helper()
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, f.post(owner))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// addMember attaches an asset to a post by direct insert.
//
// The ORDER of these calls is load-bearing in the pre-correction world:
// asset_member_post_search_text_trigger's loop had no ORDER BY, so the
// order it walked a member's posts in was the order their rows happened
// to sit in. A test that wants to reproduce the inversion deterministically
// has to choose that order, and this is how.
func (f *batchFixture) addMember(post, asset uuid.UUID, order int) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, $3)`,
		post, asset, order); err != nil {
		f.t.Fatalf("seed post member: %v", err)
	}
}

func (f *batchFixture) isMember(post, asset uuid.UUID) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM post_assets WHERE post_id = $1 AND asset_id = $2`,
		post, asset).Scan(&n); err != nil {
		f.t.Fatalf("read membership: %v", err)
	}
	return n > 0
}

// postSearchText reads a post's STORED search document, STRIPPED.
//
// strip() drops lexeme positions and weights, and it has to: the member
// half of the document is a string_agg over post_assets with no ORDER
// BY, so which member's words land at which position is an accident of
// the join plan and moves between two runs of the same rebuild. The
// question these tests ask is which WORDS the document carries, and
// comparing raw tsvectors would answer a different one and fail on it.
func (f *batchFixture) postSearchText(post uuid.UUID) string {
	f.t.Helper()
	var doc *string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT strip(search_text)::text FROM posts WHERE id = $1`, post).Scan(&doc); err != nil {
		f.t.Fatalf("read post search_text: %v", err)
	}
	if doc == nil {
		return ""
	}
	return *doc
}

// assertPostDocumentIsFresh is the CORRECTNESS assertion the whole post
// tier exists to protect: the document that is STORED must be the
// document a rebuild from the committed world produces.
//
// It reads the stored value, forces a rebuild, reads it again, and
// compares. Equal means the stored document already agreed with the
// world; different means something committed a document computed from a
// world that had moved, which is exactly the staleness a lock taken
// after the reads would allow.
func (f *batchFixture) assertPostDocumentIsFresh(t *testing.T, post uuid.UUID, what string) {
	t.Helper()
	before := f.postSearchText(post)
	if _, err := f.pool.Exec(f.ctx, `SELECT rebuild_post_search_text($1)`, post); err != nil {
		t.Fatalf("force rebuild: %v", err)
	}
	after := f.postSearchText(post)
	if before != after {
		t.Fatalf("%s: the committed post document is STALE.\n  stored:  %s\n  correct: %s",
			what, before, after)
	}
}

// postDocumentContains reports whether a post's stored document carries
// a word, which is how "R still carries A's old text" is asserted
// against the index rather than against a narrative.
func (f *batchFixture) postDocumentContains(post uuid.UUID, word string) bool {
	f.t.Helper()
	var hit bool
	if err := f.pool.QueryRow(f.ctx,
		`SELECT COALESCE(search_text @@ plainto_tsquery('english', $2), false)
		   FROM posts WHERE id = $1`, post, word).Scan(&hit); err != nil {
		f.t.Fatalf("query post document: %v", err)
	}
	return hit
}

// assertNoPartialOutcome is the batch's all-or-nothing shape, asserted
// per run: every would_change target reports an outcome, and the
// outcome arithmetic reconciles.
func assertEveryTargetChanged(t *testing.T, r *openapi.BatchAssetFieldApplyResult, want int) {
	t.Helper()
	assertApplyReconciles(t, r)
	if r.OutcomeCounts.Changed != want {
		t.Fatalf("want %d changed, got %+v", want, r.OutcomeCounts)
	}
	if r.OutcomeCounts.Conflict+r.OutcomeCounts.Gone+r.OutcomeCounts.UnauthorizedAtApply+r.OutcomeCounts.Error != 0 {
		t.Fatalf("no partial outcome is permitted here, got %+v", r.OutcomeCounts)
	}
}

// envelopeUnder128K is the audit acceptance, restated where these tests
// can assert it alongside the rest of the committed outcome.
func (f *batchFixture) envelopeUnder128K(t *testing.T, operationID string) {
	t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(f.ctx,
		`SELECT metadata FROM audit_events
		  WHERE event_type = $1 AND metadata->>'operation_id' = $2`,
		audit.EventAssetFieldBatchEditApplied, operationID).Scan(&raw); err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	if len(raw) > 128*1024 {
		t.Fatalf("the envelope is %d bytes, over the 128 KB budget", len(raw))
	}
}

// isDeadlock reports whether an error is PostgreSQL's own deadlock
// verdict. 40P01 is the bug this sprint exists to remove, so it is
// named rather than folded into "some error happened".
func isDeadlock(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40P01"
	}
	return strings.Contains(err.Error(), "40P01") || strings.Contains(err.Error(), "deadlock detected")
}

// applyOnContenderErr is applyOnContender with the transport error kept
// rather than flattened to a 500.
//
// 40P01 is the whole subject of this section, and a helper that turned
// a deadlock into "status 500" would hide the one failure these tests
// exist to name.
func (e *batchRaceEnv) applyOnContenderErr(ctx context.Context, token, reason string, confirm *int) (applyResult, error) {
	body := openapi.BatchAssetFieldApplyRequest{Token: token, Reason: reason, ConfirmCount: confirm}
	resp, err := e.contender.ApplyBatchAssetFieldEdit(ctx,
		openapi.ApplyBatchAssetFieldEditRequestObject{Body: &body})
	if err != nil {
		return applyResult{Status: 500}, err
	}
	out := applyResult{}
	switch v := resp.(type) {
	case openapi.ApplyBatchAssetFieldEdit200JSONResponse:
		r := openapi.BatchAssetFieldApplyResult(v)
		out.OK, out.Status = &r, 200
	case openapi.ApplyBatchAssetFieldEdit400JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 400
	case openapi.ApplyBatchAssetFieldEdit403JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 403
	case openapi.ApplyBatchAssetFieldEdit409JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 409
	case openapi.ApplyBatchAssetFieldEdit422JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 422
	}
	return out, nil
}

// batchRun carries a backgrounded apply and its transport error.
type batchRun struct {
	res applyResult
	err error
}

func (e *batchRaceEnv) launchApply(ctx context.Context, token, reason string, confirm *int) chan batchRun {
	done := make(chan batchRun, 1)
	go func() {
		r, err := e.applyOnContenderErr(ctx, token, reason, confirm)
		done <- batchRun{res: r, err: err}
	}()
	return done
}

// awaitApply collects a backgrounded apply, with a bounded deadline as
// a LOUD FAILURE GUARD and never as an ordering device.
func awaitApply(t *testing.T, done chan batchRun) batchRun {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(60 * time.Second):
		t.Fatal("the apply never completed after the gate released")
		return batchRun{}
	}
}

// awaitErr collects a backgrounded single operation the same way.
func awaitErr(t *testing.T, done chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(60 * time.Second):
		t.Fatalf("%s never completed after the gate released", what)
		return nil
	}
}

// ---------------------------------------------------------------------------
// REGRESSION 1: SAME POST, BATCH VERSUS THE ORDINARY WRITER
// ---------------------------------------------------------------------------

// The reported failure, reduced to three assets in one post.
//
// A < B < V all belong to post P. The gate holds assets[B], so the
// batch parks there in BOTH the corrected and the uncorrected tree and
// the seam itself is behaviour-neutral. The difference is what the
// batch is HOLDING while it waits.
//
// Uncorrected, it has already written A, and writing A rebuilt P: the
// batch holds posts[P] and is asking for assets[B]. An ordinary write
// to V on a DIFFERENT field then walks assets[V] to posts[P] and stops
// dead behind the batch, and when the gate releases the two want each
// other's rows. That is the 40P01.
//
// Corrected, the batch holds assets[A] and NOTHING on the post tier,
// because the whole asset tier is taken before a single value is
// written and the post rebuild is deferred to the end.
//
// THE PERMANENT ASSERTION IS STATE-BASED, not a duration: the ordinary
// write completes while the batch is parked, and the batch is NEVER
// observed among pg_blocking_pids of the writer's backend.
func TestBatchDeadlock_SamePostOrdinaryWrite_Regression(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("dlsamepost")
	field := e.field("bt", fieldSpec{Type: "text"})
	unrelated := e.field("ut", fieldSpec{Type: "text"})

	ids := e.sortedAssets(owner, 3)
	a, b, v := ids[0], ids[1], ids[2]
	post := e.post(owner, a, b, v)

	w := e.writer(t)
	wctx := e.identity(owner)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
		textValue("samepostbatchword"), assetEntries(a, b, v))
	if p.Counts.WouldChange != 3 {
		t.Fatalf("want 3 would_change, got %+v", p.Counts)
	}

	gate := e.openAssetLockGate(b)
	done := e.launchApply(ctx, p.Token, "same post, ordinary writer alongside", intp(3))
	e.waitForBlockedContender(t, "middle target's asset row")

	werr := make(chan error, 1)
	go func() { werr <- w.setFieldValue(wctx, v, unrelated, "ordinarysamepostword") }()

	// Poll the DATABASE, not the clock. The loop ends either because
	// the writer finished (the corrected world) or because the batch
	// turned up among its blockers (the uncorrected one).
	var (
		writerDone   bool
		writerErr    error
		blockedByBat bool
	)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !writerDone && !blockedByBat {
		if e.blockedByCount(t, w.appName, e.appName) > 0 {
			blockedByBat = true
			break
		}
		select {
		case writerErr = <-werr:
			writerDone = true
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	// The overlap was real by construction: the batch was OBSERVED
	// parked before the writer started, and the gate that parked it is
	// still held.
	gate.commit()
	if !writerDone {
		writerErr = awaitErr(t, werr, "the ordinary single-target write")
	}
	run := awaitApply(t, done)

	if blockedByBat {
		t.Fatalf("THE ORDINARY SINGLE-TARGET WRITE WAS BLOCKED BY THE BATCH. " +
			"The batch is holding the containing post while it waits for an asset row, " +
			"which is the lock-order inversion this sprint removes.")
	}
	if !writerDone {
		t.Fatal("the ordinary write never completed while the batch was parked, so the " +
			"seam this test asserts never happened")
	}
	if writerErr != nil {
		if isDeadlock(writerErr) {
			t.Fatalf("the ordinary write DEADLOCKED against the batch: %v", writerErr)
		}
		t.Fatalf("the ordinary write failed: %v", writerErr)
	}
	if run.err != nil {
		if isDeadlock(run.err) {
			t.Fatalf("the apply DEADLOCKED against the ordinary write: %v", run.err)
		}
		t.Fatalf("the apply failed: %v", run.err)
	}
	if run.res.OK == nil {
		t.Fatalf("the batch commits its result: %d %+v", run.res.Status, run.res.Refusal)
	}
	assertEveryTargetChanged(t, run.res.OK, 3)
	for _, id := range []uuid.UUID{a, b, v} {
		if got, ok := e.storedText(id, field); !ok || got != "samepostbatchword" {
			t.Fatalf("target %s did not keep the batch value, got %q", id, got)
		}
	}
	if got, ok := e.storedText(v, unrelated); !ok || got != "ordinarysamepostword" {
		t.Fatalf("the ordinary value must have landed, got %q", got)
	}
	if !e.tokenConsumed(p.OperationId.String()) {
		t.Fatal("the token is consumed exactly once by a committed apply")
	}
	if n := e.envelopes(p.OperationId.String()); n != 1 {
		t.Fatalf("want exactly one audit envelope, got %d", n)
	}
	e.envelopeUnder128K(t, p.OperationId.String())
	e.assertPostDocumentIsFresh(t, post, "the shared post after both writers committed")
}

// ---------------------------------------------------------------------------
// REGRESSION 2: THE INVERSION IS TRANSACTION-WIDE, NOT PER ASSET
// ---------------------------------------------------------------------------

// Sorting each asset's own post loop is not enough, and this is the
// case that says so.
//
// Two posts, P < Q. The batch's targets are A in Q and B in P, with
// A < B by asset id, so an uncorrected batch reaches Q FIRST and P
// SECOND: descending post order, arrived at one target at a time.
// Meanwhile an ordinary write to a NON-TARGET X that belongs to both
// walks P then Q. Neither transaction did anything wrong per statement;
// the order is a property of the WHOLE transaction, and only a
// transaction-wide post phase can fix it.
func TestBatchDeadlock_TransactionWidePostOrder_Regression(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("dlwide")
	field := e.field("bt", fieldSpec{Type: "text"})
	unrelated := e.field("ut", fieldSpec{Type: "text"})

	ids := e.sortedAssets(owner, 3)
	a, b, x := ids[0], ids[1], ids[2]
	ps := e.sortedPosts(owner, 2)
	pLow, qHigh := ps[0], ps[1]

	// A lives in the HIGH post and B in the LOW one, so walking targets
	// by ascending asset id walks the posts DESCENDING.
	e.addMember(qHigh, a, 0)
	e.addMember(pLow, b, 0)
	// The non-target belongs to both. Its row in the LOW post is
	// inserted first, which is the order the uncorrected trigger walks
	// them in and the order the corrected one enforces.
	e.addMember(pLow, x, 1)
	e.addMember(qHigh, x, 1)

	w := e.writer(t)
	wctx := e.identity(owner)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
		textValue("widebatchword"), assetEntries(a, b))
	if p.Counts.WouldChange != 2 {
		t.Fatalf("want 2 would_change, got %+v", p.Counts)
	}

	gate := e.openAssetLockGate(b)
	done := e.launchApply(ctx, p.Token, "two posts, opposite directions", intp(2))
	e.waitForBlockedContender(t, "later target's asset row")

	werr := make(chan error, 1)
	go func() { werr <- w.setFieldValue(wctx, x, unrelated, "widordinaryword") }()

	// The writer either finishes while the batch is parked or piles up
	// behind it. Both are observations; neither is a sleep.
	var writerDone bool
	var writerErr error
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case writerErr = <-werr:
			writerDone = true
		default:
		}
		if writerDone {
			break
		}
		if e.blockedByCount(t, w.appName, e.appName) > 0 {
			t.Log("the ordinary write is queued behind the batch's post lock")
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	gate.commit()
	if !writerDone {
		writerErr = awaitErr(t, werr, "the ordinary single-target write")
	}
	run := awaitApply(t, done)

	if isDeadlock(writerErr) {
		t.Fatalf("THE ORDINARY WRITE DEADLOCKED: the batch reached the two posts in the "+
			"opposite order: %v", writerErr)
	}
	if isDeadlock(run.err) {
		t.Fatalf("THE APPLY DEADLOCKED: the batch reached the two posts in the opposite "+
			"order to the ordinary writer: %v", run.err)
	}
	if writerErr != nil {
		t.Fatalf("the ordinary write failed: %v", writerErr)
	}
	if run.err != nil {
		t.Fatalf("the apply failed: %v", run.err)
	}
	if run.res.OK == nil {
		t.Fatalf("the batch commits its result: %d %+v", run.res.Status, run.res.Refusal)
	}
	assertEveryTargetChanged(t, run.res.OK, 2)
	if got, ok := e.storedText(x, unrelated); !ok || got != "widordinaryword" {
		t.Fatalf("the ordinary value must have landed, got %q", got)
	}
	e.assertPostDocumentIsFresh(t, pLow, "the low post")
	e.assertPostDocumentIsFresh(t, qHigh, "the high post")
}

// ---------------------------------------------------------------------------
// REGRESSION 4: MEMBERSHIP REMOVAL, AND WHY THE REBUILD LOCKS FIRST
// ---------------------------------------------------------------------------

// rebuild_post_search_text bakes a post's document out of THREE reads
// that all precede its UPDATE: the member assets' own documents, the
// post's tags, and the post row itself. At READ COMMITTED each of those
// takes its own snapshot, and blocking on the UPDATE's row lock
// re-evaluates the target row and nothing else. The local variables
// keep whatever they were computed from.
//
// So a transaction that computes a post document, waits, and then
// applies it commits a document describing a world that has since
// moved. Here the removal of A from R computes R out of its REMAINING
// member M while the batch is still holding M's new value uncommitted,
// then applies it on top of the batch's own correct rebuild, and R ends
// up carrying M's OLD text.
//
// The fix is that the function takes its post FOR NO KEY UPDATE as its
// FIRST statement, so the removal is stopped BEFORE it can read
// anything and computes only once it owns the row.
func TestBatchDeadlock_MembershipRemovalFreshness_Regression(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("dlremove")
	field := e.field("bt", fieldSpec{Type: "text"})

	ids := e.sortedAssets(owner, 2)
	a, m := ids[0], ids[1]
	ps := e.sortedPosts(owner, 2)
	rLow, rHigh := ps[0], ps[1]

	// A is in BOTH posts, and its row in the LOW post is inserted
	// first: that is the order the uncorrected trigger walks them in,
	// so the batch parks on the HIGH post while holding the LOW one in
	// either tree. M is in the low post only, and it is the member
	// whose freshness the removal has to respect.
	e.addMember(rLow, a, 0)
	e.addMember(rHigh, a, 0)
	e.addMember(rLow, m, 1)

	// Distinctive OLD words, so a stale document is visible in the
	// index rather than inferred.
	e.setValue(a, field, map[string]any{"text": "removedmemberword"})
	e.setValue(m, field, map[string]any{"text": "staleremainingword"})

	w := e.writer(t)
	wctx := e.identity(owner)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
		textValue("freshbatchword"), assetEntries(a, m))
	if p.Counts.WouldChange != 2 {
		t.Fatalf("want 2 would_change, got %+v", p.Counts)
	}

	gate := e.openPostLockGate(rHigh)
	done := e.launchApply(ctx, p.Token, "membership removal alongside", intp(2))
	e.waitForBlockedContender(t, "the high post row")

	rerr := make(chan error, 1)
	start := time.Now()
	go func() { rerr <- w.removeMember(wctx, rLow, a) }()

	// OBSERVED BLOCKED. Where it is blocked is the whole point: with
	// the entry lock in place it has not read a single member document
	// yet, and the committed result below is what proves it.
	e.waitForBlockedApp(t, w.appName, "low post row, at the rebuild's entry")

	gate.commit()
	removeErr := awaitErr(t, rerr, "the membership removal")
	membershipLatency := time.Since(start)
	run := awaitApply(t, done)

	t.Logf("MEMBERSHIP LATENCY against a parked batch: RemovePostAsset completed in %s",
		membershipLatency)

	if isDeadlock(removeErr) || isDeadlock(run.err) {
		t.Fatalf("DEADLOCK between the apply and the membership removal: %v / %v", removeErr, run.err)
	}
	if removeErr != nil {
		t.Fatalf("the membership removal failed: %v", removeErr)
	}
	if run.err != nil {
		t.Fatalf("the apply failed: %v", run.err)
	}
	if run.res.OK == nil {
		t.Fatalf("the batch commits its result: %d %+v", run.res.Status, run.res.Refusal)
	}
	assertEveryTargetChanged(t, run.res.OK, 2)

	if e.isMember(rLow, a) {
		t.Fatal("the membership removal must have landed")
	}
	if e.postDocumentContains(rLow, "removedmemberword") {
		t.Fatal("the post still carries the text of the member that was REMOVED from it")
	}
	if e.postDocumentContains(rLow, "staleremainingword") {
		t.Fatal("THE POST DOCUMENT IS STALE: it carries the remaining member's OLD text, " +
			"which means the removal computed the document before the batch's write " +
			"committed and applied it afterwards")
	}
	if !e.postDocumentContains(rLow, "freshbatchword") {
		t.Fatal("THE POST DOCUMENT IS STALE: it does not carry the remaining member's " +
			"committed value")
	}
	e.assertPostDocumentIsFresh(t, rLow, "the post the member was removed from")
	e.assertPostDocumentIsFresh(t, rHigh, "the post the target still belongs to")
}

// ---------------------------------------------------------------------------
// REGRESSION 5: A NON-TARGET JOINING A POST THE BATCH IS EDITING
// ---------------------------------------------------------------------------

// The same freshness rule, reached from the other direction and in both
// orderings.
//
// A post R holds a batch target A. An asset X that is NOT a target
// joins R while the batch is in flight. Whichever of the two owns R
// first, the document that finally commits must describe BOTH: X is a
// member, and A carries its NEW value.
func TestBatchDeadlock_NonTargetMembershipAdditionFreshness_Regression(t *testing.T) {
	t.Run("the batch owns the post row first", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dladd1")
		field := e.field("bt", fieldSpec{Type: "text"})
		other := e.field("ot", fieldSpec{Type: "text"})

		ids := e.sortedAssets(owner, 2)
		a, x := ids[0], ids[1]
		ps := e.sortedPosts(owner, 2)
		rLow, rHigh := ps[0], ps[1]
		e.addMember(rLow, a, 0)
		e.addMember(rHigh, a, 0)
		e.setValue(x, other, map[string]any{"text": "joiningmemberword"})

		w := e.writer(t)
		wctx := e.identity(owner)

		p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
			textValue("addfreshbatchword"), assetEntries(a))
		if p.Counts.WouldChange != 1 {
			t.Fatalf("want 1 would_change, got %+v", p.Counts)
		}

		gate := e.openPostLockGate(rHigh)
		done := e.launchApply(ctx, p.Token, "membership addition alongside", intp(1))
		e.waitForBlockedContender(t, "the high post row")

		aerr := make(chan error, 1)
		start := time.Now()
		go func() { aerr <- w.addMember(wctx, rLow, x) }()
		e.waitForBlockedApp(t, w.appName, "low post row, at the rebuild's entry")

		gate.commit()
		addErr := awaitErr(t, aerr, "the membership addition")
		membershipLatency := time.Since(start)
		run := awaitApply(t, done)

		t.Logf("MEMBERSHIP LATENCY against a parked batch: AddPostAsset completed in %s",
			membershipLatency)

		if isDeadlock(addErr) || isDeadlock(run.err) {
			t.Fatalf("DEADLOCK between the apply and the membership addition: %v / %v", addErr, run.err)
		}
		if addErr != nil {
			t.Fatalf("the membership addition failed: %v", addErr)
		}
		if run.err != nil || run.res.OK == nil {
			t.Fatalf("the apply failed: %v %+v", run.err, run.res.Refusal)
		}
		assertEveryTargetChanged(t, run.res.OK, 1)

		if !e.isMember(rLow, x) {
			t.Fatal("the membership addition must have landed")
		}
		if !e.postDocumentContains(rLow, "joiningmemberword") {
			t.Fatal("the post document must reflect the member that joined it")
		}
		if !e.postDocumentContains(rLow, "addfreshbatchword") {
			t.Fatal("THE POST DOCUMENT IS STALE: it carries the batch target's OLD document " +
				"rather than the value the batch committed")
		}
		e.assertPostDocumentIsFresh(t, rLow, "the post the non-target joined")
		e.assertPostDocumentIsFresh(t, rHigh, "the other containing post")
	})

	t.Run("the membership addition owns the post row first", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dladd2")
		field := e.field("bt", fieldSpec{Type: "text"})
		other := e.field("ot", fieldSpec{Type: "text"})

		ids := e.sortedAssets(owner, 3)
		a, b, x := ids[0], ids[1], ids[2]
		post := e.post(owner, a, b)
		e.setValue(x, other, map[string]any{"text": "secondjoinerword"})

		w := e.writer(t)
		wctx := e.identity(owner)

		p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
			textValue("reverseorderbatchword"), assetEntries(a, b))
		if p.Counts.WouldChange != 2 {
			t.Fatalf("want 2 would_change, got %+v", p.Counts)
		}

		// The batch parks on the ASSET tier, before it has written
		// anything, so the membership change reaches the post row first
		// and the batch's own rebuild has to run after it.
		gate := e.openAssetLockGate(b)
		done := e.launchApply(ctx, p.Token, "membership addition wins the post row", intp(2))
		e.waitForBlockedContender(t, "later target's asset row")

		aerr := make(chan error, 1)
		start := time.Now()
		go func() { aerr <- w.addMember(wctx, post, x) }()

		// Either it finishes while the batch is parked, which is the
		// corrected world, or it turns up behind the batch's post lock,
		// which is the uncorrected one. Observed, never timed.
		var addDone bool
		var addErr error
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case addErr = <-aerr:
				addDone = true
			default:
			}
			if addDone {
				break
			}
			if e.blockedByCount(t, w.appName, e.appName) > 0 {
				t.Log("the membership addition is queued behind the batch's post lock")
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		membershipLatency := time.Since(start)
		t.Logf("MEMBERSHIP LATENCY against a parked batch: AddPostAsset settled in %s (completed: %v)",
			membershipLatency, addDone)

		gate.commit()
		if !addDone {
			addErr = awaitErr(t, aerr, "the membership addition")
		}
		run := awaitApply(t, done)

		if isDeadlock(addErr) || isDeadlock(run.err) {
			t.Fatalf("DEADLOCK between the apply and the membership addition: %v / %v", addErr, run.err)
		}
		if addErr != nil {
			t.Fatalf("the membership addition failed: %v", addErr)
		}
		if run.err != nil || run.res.OK == nil {
			t.Fatalf("the apply failed: %v %+v", run.err, run.res.Refusal)
		}
		assertEveryTargetChanged(t, run.res.OK, 2)

		if !e.isMember(post, x) {
			t.Fatal("the membership addition must have landed")
		}
		if !e.postDocumentContains(post, "secondjoinerword") {
			t.Fatal("THE POST DOCUMENT IS STALE: the batch's rebuild ran after the member " +
				"joined and must describe it")
		}
		if !e.postDocumentContains(post, "reverseorderbatchword") {
			t.Fatal("the post document must carry the batch's committed value")
		}
		e.assertPostDocumentIsFresh(t, post, "the post both operations touched")
	})
}

// ---------------------------------------------------------------------------
// REGRESSION 6: THE REBUILD PHASE RUNS ON EVERY SUCCESS PATH
// ---------------------------------------------------------------------------

// Suppressing the per-row propagation is only correct if the batch
// pays the debt back. A success that skipped the rebuild would commit
// a set of post documents that no longer describe their members, and
// nothing else in the system would notice until somebody searched.
//
// So: a real apply over several posts with overlapping membership, no
// gate and no contender, and then EVERY affected post is compared
// against a fresh rebuild.
func TestBatchDeadlock_PostDocumentsAfterCommit_Regression(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("dlrebuild")
	field := e.field("bt", fieldSpec{Type: "text"})

	ids := e.sortedAssets(owner, 5)
	ps := e.sortedPosts(owner, 3)
	// Deliberately overlapping: one asset in two posts, one post
	// holding two targets, one post holding a target and a stranger.
	e.addMember(ps[0], ids[0], 0)
	e.addMember(ps[0], ids[1], 1)
	e.addMember(ps[1], ids[1], 0)
	e.addMember(ps[1], ids[2], 1)
	e.addMember(ps[2], ids[3], 0)
	e.addMember(ps[2], ids[4], 1)
	for _, id := range ids {
		e.setValue(id, field, map[string]any{"text": "beforecommitword"})
	}

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
		textValue("aftercommitword"), assetEntries(ids...))
	if p.Counts.WouldChange != len(ids) {
		t.Fatalf("want %d would_change, got %+v", len(ids), p.Counts)
	}
	res := e.apply(ctx, p.Token, "post documents after commit", intp(len(ids)))
	if res.OK == nil {
		t.Fatalf("the batch commits: %d %+v", res.Status, res.Refusal)
	}
	assertEveryTargetChanged(t, res.OK, len(ids))

	for i, post := range ps {
		if !e.postDocumentContains(post, "aftercommitword") {
			t.Fatalf("post %d does not carry the committed value", i)
		}
		if e.postDocumentContains(post, "beforecommitword") {
			t.Fatalf("post %d still carries the pre-batch value", i)
		}
		e.assertPostDocumentIsFresh(t, post, fmt.Sprintf("post %d after a plain batch commit", i))
	}
}

// ---------------------------------------------------------------------------
// REGRESSION 7: THE REFUSALS THE SHARED LOCK PASS MUST NOT COLLAPSE
// ---------------------------------------------------------------------------

// Folding the reference target into the subjects' lock statement puts
// one id in two roles, and the two roles refuse DIFFERENTLY. Losing
// that distinction would be the cheap way to write this pass and would
// silently change what an operator is told.
//
//	SUBJECT missing or soft-deleted           -> that target is `gone`
//	REFERENCE TARGET missing or soft-deleted  -> the WHOLE batch is
//	                                             reference_invalidated,
//	                                             writes nothing, and
//	                                             leaves the token usable
//
// And when ONE id carries both roles, reference_invalidated WINS,
// batch-wide, before any per-target outcome exists. A response carrying
// both a reference_invalidated and a per-target `gone` is a shape that
// does not exist.
func TestBatchDeadlock_ReferenceAndSubjectRoles_Regression(t *testing.T) {
	t.Run("a soft-deleted SUBJECT is that target's gone", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dlrole1")
		field := e.field("bt", fieldSpec{Type: "text"})
		ids := e.sortedAssets(owner, 2)
		alive, doomed := ids[0], ids[1]

		p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
			textValue("roleword"), assetEntries(alive, doomed))
		e.softDeleteAsset(doomed)

		res := e.apply(ctx, p.Token, "one subject went away", intp(2))
		if res.OK == nil {
			t.Fatalf("the batch commits its result: %d %+v", res.Status, res.Refusal)
		}
		assertApplyReconciles(t, res.OK)
		if got, _ := outcomeOf(res.OK, doomed); got != openapi.BatchOutcomeGone {
			t.Fatalf("the soft-deleted subject is `gone`, got %q", got)
		}
		if got, _ := outcomeOf(res.OK, alive); got != openapi.BatchOutcomeChanged {
			t.Fatalf("the live subject is written, got %q", got)
		}
	})

	t.Run("a soft-deleted REFERENCE TARGET refuses batch-wide", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dlrole2")
		field := e.field("br", fieldSpec{Type: "reference"})
		subjects := e.sortedAssets(owner, 2)
		target := e.asset(&owner, nil)

		p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
			refValue(target), assetEntries(subjects...))
		e.softDeleteAsset(target)

		res := e.apply(ctx, p.Token, "the reference target went away", intp(2))
		e.wantRefusal(res, 409, openapi.BatchReferenceInvalidated)
		for _, s := range subjects {
			if e.rowExists(s, field) {
				t.Fatalf("subject %s was written by a refused batch", s)
			}
		}
		if e.tokenConsumed(p.OperationId.String()) {
			t.Fatal("a pre-write refusal leaves the token usable")
		}
		if n := e.envelopes(p.OperationId.String()); n != 0 {
			t.Fatalf("a pre-write refusal writes no envelope, got %d", n)
		}
	})

	t.Run("ONE id in BOTH roles refuses batch-wide and nothing else", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dlrole3")
		field := e.field("br", fieldSpec{Type: "reference"})
		ids := e.sortedAssets(owner, 2)
		dual, other := ids[0], ids[1]

		// The reference value points at an asset that is ALSO one of
		// the subjects, so the single locking pass sees one id wearing
		// both hats.
		p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
			refValue(dual), assetEntries(dual, other))
		if p.Counts.WouldChange != 2 {
			t.Fatalf("want 2 would_change, got %+v", p.Counts)
		}
		auditBefore := e.auditEventCount()
		e.softDeleteAsset(dual)

		res := e.apply(ctx, p.Token, "the dual-role asset went away", intp(2))

		// reference_invalidated WINS, batch-wide.
		e.wantRefusal(res, 409, openapi.BatchReferenceInvalidated)
		if res.OK != nil {
			t.Fatal("a batch-wide refusal carries no per-target outcomes at all")
		}
		// ZERO field writes.
		for _, s := range []uuid.UUID{dual, other} {
			if e.rowExists(s, field) {
				t.Fatalf("subject %s was written by a batch-wide refusal", s)
			}
		}
		// NO envelope, and no audit event of any kind from this apply.
		if n := e.envelopes(p.OperationId.String()); n != 0 {
			t.Fatalf("a pre-write refusal writes no envelope, got %d", n)
		}
		if after := e.auditEventCount(); after != auditBefore {
			t.Fatalf("a pre-write refusal writes no audit event, %d became %d", auditBefore, after)
		}
		// THE TOKEN REMAINS USABLE.
		if e.tokenConsumed(p.OperationId.String()) {
			t.Fatal("a pre-write refusal leaves the token usable")
		}
	})
}

// ---------------------------------------------------------------------------
// REGRESSION 8: THE BOUNDARIES, INCLUDING THE ONE THAT LOOKS LIKE NONE
// ---------------------------------------------------------------------------

// N is the number of WOULD-CHANGE SUBJECTS the asset tier locks. One,
// several, several across several posts, and none.
//
// N = 0 IS NOT A BYPASS. ADR 0019: "a `would_change == 0` apply is a
// REAL operation: it completes, consumes its token, and writes one
// envelope recording the zero-change operation with its reason and
// counts." Everything above the asset tier still runs, and the
// reference target is still revalidated. What N = 0 removes is only the
// work there is none of: no subject rows to lock, and no post document
// to rebuild.
func TestBatchDeadlock_TargetSetBoundaries_Regression(t *testing.T) {
	t.Run("N=1", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dln1")
		field := e.field("bt", fieldSpec{Type: "text"})
		a := e.asset(&owner, nil)
		post := e.post(owner, a)

		p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("oneword"), assetEntries(a))
		res := e.apply(ctx, p.Token, "one target", intp(1))
		if res.OK == nil {
			t.Fatalf("the batch commits: %d %+v", res.Status, res.Refusal)
		}
		assertEveryTargetChanged(t, res.OK, 1)
		e.assertPostDocumentIsFresh(t, post, "the single target's post")
	})

	t.Run("N>=2 across one post", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dln2")
		field := e.field("bt", fieldSpec{Type: "text"})
		ids := e.sortedAssets(owner, 4)
		post := e.post(owner, ids...)

		p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("fourword"), assetEntries(ids...))
		res := e.apply(ctx, p.Token, "four targets", intp(4))
		if res.OK == nil {
			t.Fatalf("the batch commits: %d %+v", res.Status, res.Refusal)
		}
		assertEveryTargetChanged(t, res.OK, 4)
		e.assertPostDocumentIsFresh(t, post, "the shared post")
	})

	t.Run("multi-post membership", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dlnmulti")
		field := e.field("bt", fieldSpec{Type: "text"})
		ids := e.sortedAssets(owner, 3)
		ps := e.sortedPosts(owner, 3)
		// Every target belongs to MORE THAN ONE post, which is the real
		// corpus shape the coalesced rebuild has to handle.
		for i, id := range ids {
			e.addMember(ps[i], id, 0)
			e.addMember(ps[(i+1)%len(ps)], id, 1)
		}

		p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("multipostword"), assetEntries(ids...))
		res := e.apply(ctx, p.Token, "targets in several posts each", intp(3))
		if res.OK == nil {
			t.Fatalf("the batch commits: %d %+v", res.Status, res.Refusal)
		}
		assertEveryTargetChanged(t, res.OK, 3)
		for i, post := range ps {
			if !e.postDocumentContains(post, "multipostword") {
				t.Fatalf("post %d must carry the committed value", i)
			}
			e.assertPostDocumentIsFresh(t, post, fmt.Sprintf("multi-membership post %d", i))
		}
	})

	t.Run("N=0 still runs the field-definition seam", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dln0def")
		field := e.field("bt", fieldSpec{Type: "text"})
		a := e.asset(&owner, nil)
		e.setValue(a, field, map[string]any{"text": "alreadythere"})

		// FILL_EMPTIES against a target that is not empty. Overwrite
		// can never reach would_change 0: a set advances set_at and
		// writes a history row, so it changes the record even against
		// identical bytes (A80).
		p := e.mustPreview(ctx, openapi.BatchModeFillEmpties, field,
			textValue("wouldfill"), assetEntries(a))
		if p.Counts.WouldChange != 0 || p.Counts.NoOp != 1 {
			t.Fatalf("want a zero-would-change preview, got %+v", p.Counts)
		}

		// TIER 1 still runs: a held field_definition row parks the
		// zero-change apply exactly as it would park any other.
		gate := e.openGate(`UPDATE field_definition SET label = label WHERE id = $1`, field)
		done := e.launchApply(ctx, p.Token, "zero change, definition held", nil)
		e.waitForBlockedContender(t, "field definition row")
		gate.commit()
		run := awaitApply(t, done)
		if run.err != nil || run.res.OK == nil {
			t.Fatalf("the zero-change apply commits: %v %+v", run.err, run.res.Refusal)
		}
		if run.res.OK.OutcomeCounts.Changed != 0 {
			t.Fatalf("nothing changes, got %+v", run.res.OK.OutcomeCounts)
		}
	})

	t.Run("N=0 still serializes against effective authority", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dln0auth")
		field := e.field("bt", fieldSpec{Type: "text"})
		a := e.asset(&owner, nil)
		e.setValue(a, field, map[string]any{"text": "alreadythere"})

		p := e.mustPreview(ctx, openapi.BatchModeFillEmpties, field,
			textValue("wouldfill"), assetEntries(a))
		if p.Counts.WouldChange != 0 {
			t.Fatalf("want a zero-would-change preview, got %+v", p.Counts)
		}

		// #1406's seam, unchanged by this sprint and asserted here
		// because a zero-change apply is exactly the shape a bypass
		// would take.
		gate := e.openAuthorityGate(owner, `
			DELETE FROM user_capability_grants
			 WHERE user_ref = $1 AND capability_code = $2 AND team_id IS NULL`,
			owner, capBulkEdit)
		done := e.launchApply(ctx, p.Token, "zero change, authority held", nil)
		e.waitForBlockedContender(t, "authority lock")
		gate.commit()
		run := awaitApply(t, done)
		if run.err != nil {
			t.Fatalf("the zero-change apply failed: %v", run.err)
		}
		if run.res.Refusal == nil || run.res.Refusal.Reason != openapi.BatchBulkCapabilityRequired {
			t.Fatalf("the revoked caller is refused, got %d %+v", run.res.Status, run.res.Refusal)
		}
	})

	t.Run("N=0 consumes its token and writes one envelope, and rebuilds no post", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dln0env")
		field := e.field("bt", fieldSpec{Type: "text"})
		a := e.asset(&owner, nil)
		post := e.post(owner, a)
		e.setValue(a, field, map[string]any{"text": "alreadythere"})

		p := e.mustPreview(ctx, openapi.BatchModeFillEmpties, field,
			textValue("wouldfill"), assetEntries(a))
		if p.Counts.WouldChange != 0 {
			t.Fatalf("want a zero-would-change preview, got %+v", p.Counts)
		}

		// NO POST IS REBUILT, asserted behaviourally: an independent
		// transaction holds the containing post for the whole apply,
		// and the apply completes anyway. A rebuild would have queued
		// behind this and the run would time out.
		gate := e.openPostLockGate(post)
		done := e.launchApply(ctx, p.Token, "zero change, post held", nil)
		run := awaitApply(t, done)
		gate.commit()

		if run.err != nil || run.res.OK == nil {
			t.Fatalf("the zero-change apply commits: %v %+v", run.err, run.res.Refusal)
		}
		if run.res.OK.OutcomeCounts.Changed != 0 {
			t.Fatalf("nothing changes, got %+v", run.res.OK.OutcomeCounts)
		}
		if !e.tokenConsumed(p.OperationId.String()) {
			t.Fatal("a zero-change apply is a REAL operation: it consumes its token")
		}
		if n := e.envelopes(p.OperationId.String()); n != 1 {
			t.Fatalf("a zero-change apply writes EXACTLY ONE envelope, got %d", n)
		}
	})

	t.Run("N=0 does not bypass reference invalidation", func(t *testing.T) {
		e := newBatchRaceEnv(t)
		owner, ctx := e.bulkOperator("dln0ref")
		field := e.field("br", fieldSpec{Type: "reference"})
		a := e.asset(&owner, nil)
		target := e.asset(&owner, nil)
		e.setValue(a, field, map[string]any{"ref": a})

		// The subject already holds a reference, so fill_empties has
		// nothing to write. The PROPOSED target is a different asset
		// and must still be locked and revalidated.
		p := e.mustPreview(ctx, openapi.BatchModeFillEmpties, field, refValue(target), assetEntries(a))
		if p.Counts.WouldChange != 0 {
			t.Fatalf("want a zero-would-change preview, got %+v", p.Counts)
		}
		e.softDeleteAsset(target)

		res := e.apply(ctx, p.Token, "zero change, dead reference", nil)
		e.wantRefusal(res, 409, openapi.BatchReferenceInvalidated)
		if e.tokenConsumed(p.OperationId.String()) {
			t.Fatal("a pre-write refusal leaves the token usable")
		}
		if n := e.envelopes(p.OperationId.String()); n != 0 {
			t.Fatalf("a pre-write refusal writes no envelope, got %d", n)
		}
	})
}

// applyResultOf classifies an apply response once, so the three pools
// that drive an apply in this file cannot drift in how they read it.
func applyResultOf(resp openapi.ApplyBatchAssetFieldEditResponseObject) applyResult {
	out := applyResult{}
	switch v := resp.(type) {
	case openapi.ApplyBatchAssetFieldEdit200JSONResponse:
		r := openapi.BatchAssetFieldApplyResult(v)
		out.OK, out.Status = &r, 200
	case openapi.ApplyBatchAssetFieldEdit400JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 400
	case openapi.ApplyBatchAssetFieldEdit403JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 403
	case openapi.ApplyBatchAssetFieldEdit409JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 409
	case openapi.ApplyBatchAssetFieldEdit422JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 422
	}
	return out
}

// launchApply on the writer's pool, so a SECOND real batch can be
// observed against the first.
func (w *writerEnv) launchApply(ctx context.Context, token, reason string, confirm *int) chan batchRun {
	done := make(chan batchRun, 1)
	go func() {
		body := openapi.BatchAssetFieldApplyRequest{Token: token, Reason: reason, ConfirmCount: confirm}
		resp, err := w.meta.ApplyBatchAssetFieldEdit(ctx,
			openapi.ApplyBatchAssetFieldEditRequestObject{Body: &body})
		if err != nil {
			done <- batchRun{res: applyResult{Status: 500}, err: err}
			return
		}
		done <- batchRun{res: applyResultOf(resp)}
	}()
	return done
}

// awaitTupleWait polls until a backend of appName carries a tuple lock,
// which is the row PostgreSQL has parked it on, and fails rather than
// guessing when none appears.
func (e *batchRaceEnv) awaitTupleWait(t *testing.T, appName string) map[string][]string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		locks := e.waitingTupleLocks(t, appName)
		if len(locks) > 0 {
			return locks
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never parked on a row lock, so this test observed nothing", appName)
	return nil
}

// ---------------------------------------------------------------------------
// STRUCTURAL 10: TWO REAL BATCHES, DISTINCT FIELDS, IDENTICAL TARGETS
// ---------------------------------------------------------------------------
//
// ⚠️ STRUCTURAL COVERAGE. This is not a red-before regression and is not
// claimed as one. It exists because the corrected hierarchy is a
// property of the implementation that reading cannot verify and a
// refactor can quietly break.
//
// THE TWO FIELDS MUST BE DISTINCT DEFINITIONS. LockFieldDefinitionForBatch
// is FOR UPDATE on the field_definition ROW, so two batches over the
// SAME field serialise at tier 1 and never reach the asset tier
// together: such a test would prove nothing about tier 2 while looking
// exactly like one that did. Distinct fields let both batches through
// tier 1 and put them nose to nose on the assets they share.
//
// The target sets are IDENTICAL, deliberately: maximum overlap is the
// case an unordered acquisition would deadlock on.
func TestBatchDeadlock_TwoConcurrentBatchesDistinctFields_Structural(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("dltwobatch")
	f1 := e.field("f1", fieldSpec{Type: "text"})
	f2 := e.field("f2", fieldSpec{Type: "text"})

	ids := e.sortedAssets(owner, 2)
	a, b := ids[0], ids[1]
	post := e.post(owner, a, b)

	second := e.writer(t)

	p1 := e.mustPreview(ctx, openapi.BatchModeOverwrite, f1, textValue("firstbatchword"), assetEntries(a, b))
	p2 := e.mustPreview(ctx, openapi.BatchModeOverwrite, f2, textValue("secondbatchword"), assetEntries(a, b))
	if p1.Counts.WouldChange != 2 || p2.Counts.WouldChange != 2 {
		t.Fatalf("want 2 would_change each, got %+v / %+v", p1.Counts, p2.Counts)
	}

	// The ordered-asset gate: B1 takes the earlier target and stops on
	// the later one, so it is provably HOLDING a tier-2 lock when B2
	// starts.
	gate := e.openAssetLockGate(b)
	done1 := e.launchApply(ctx, p1.Token, "first batch", intp(2))
	e.waitForBlockedContender(t, "later target's asset row")

	// ONLY NOW. Starting B2 before B1 was observed holding a tier-2
	// lock would leave the interleaving to chance.
	done2 := second.launchApply(ctx, p2.Token, "second batch", intp(2))
	e.waitForBlockedApp(t, second.appName, "an asset row held by the first batch")

	if n := e.blockedByCount(t, second.appName, e.appName); n == 0 {
		t.Fatal("the second batch must be blocked BY THE FIRST; it is waiting on something else")
	}
	locks := e.awaitTupleWait(t, second.appName)
	if ctids, onFieldDef := locks["field_definition"]; onFieldDef {
		t.Fatalf("the second batch is parked on a FIELD_DEFINITION row %v, so this run says "+
			"nothing about the asset tier; the two batches must use DISTINCT fields", ctids)
	}
	ctids, onAssets := locks["assets"]
	if !onAssets {
		t.Fatalf("the second batch must be parked on an ASSETS row, it is parked on %v", locks)
	}
	parkedOnEarlier := false
	for _, ctid := range ctids {
		if id, ok := e.assetAtCtid(t, ctid); ok && id == a {
			parkedOnEarlier = true
		}
	}
	if !parkedOnEarlier {
		t.Fatalf("the second batch must be parked on the EARLIER target %s, which the first "+
			"batch already holds; its tuple locks are %v", a, ctids)
	}
	t.Logf("STRUCTURAL: the second batch is parked on assets row %s, already held by the first, "+
		"and on no field_definition row", a)

	gate.commit()
	run1 := awaitApply(t, done1)
	run2 := awaitApply(t, done2)

	if isDeadlock(run1.err) || isDeadlock(run2.err) {
		t.Fatalf("the two batches DEADLOCKED: %v / %v", run1.err, run2.err)
	}
	if run1.err != nil || run1.res.OK == nil {
		t.Fatalf("the first batch commits: %v %+v", run1.err, run1.res.Refusal)
	}
	if run2.err != nil || run2.res.OK == nil {
		t.Fatalf("the second batch commits: %v %+v", run2.err, run2.res.Refusal)
	}
	assertEveryTargetChanged(t, run1.res.OK, 2)
	assertEveryTargetChanged(t, run2.res.OK, 2)

	for _, id := range []uuid.UUID{a, b} {
		if got, ok := e.storedText(id, f1); !ok || got != "firstbatchword" {
			t.Fatalf("%s lost the first batch's value, got %q", id, got)
		}
		if got, ok := e.storedText(id, f2); !ok || got != "secondbatchword" {
			t.Fatalf("%s lost the second batch's value, got %q", id, got)
		}
	}
	for _, p := range []*openapi.BatchAssetFieldPreview{p1, p2} {
		op := p.OperationId.String()
		if !e.tokenConsumed(op) {
			t.Fatalf("token %s is consumed exactly once", op)
		}
		if n := e.envelopes(op); n != 1 {
			t.Fatalf("want exactly one envelope for %s, got %d", op, n)
		}
	}
	e.assertPostDocumentIsFresh(t, post, "the post both batches wrote into")
}

// ---------------------------------------------------------------------------
// STRUCTURAL 11: TIER 3 TAKES NO POST LOCK
// ---------------------------------------------------------------------------
//
// ⚠️ STRUCTURAL COVERAGE. Not a red-before regression.
//
// The whole hierarchy rests on one claim about the write phase: while
// the batch is writing field values it holds NOTHING on the post tier.
// That is true only because `assets` carries two other AFTER UPDATE
// triggers, assets_mature_sync and assets_ai_provenance_sync, which
// both loop post_assets and touch posts, and both early-return unless
// `mature`, `ai_provenance` or the nullness of `deleted_at` actually
// changed. A field-value write changes none of them, so neither fires.
//
// That is an assumption about somebody else's trigger, and a comment
// asserting it would go stale the first time one of them gained a
// branch. So it is asserted BEHAVIOURALLY: with a real batch parked
// mid-write-phase, an independent transaction takes FOR NO KEY UPDATE
// on EVERY post containing a target with NOWAIT. NOWAIT is the
// assertion: "immediately, or fail".
func TestBatchDeadlock_NoPostLockDuringWrites_Structural(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, ctx := e.bulkOperator("dltier3")
	field := e.field("bt", fieldSpec{Type: "text"})

	ids := e.sortedAssets(owner, 3)
	a, b, c := ids[0], ids[1], ids[2]
	ps := e.sortedPosts(owner, 2)
	e.addMember(ps[0], a, 0)
	e.addMember(ps[0], b, 1)
	e.addMember(ps[1], c, 0)
	e.addMember(ps[1], a, 1)
	for _, id := range ids {
		e.setValue(id, field, map[string]any{"text": "tier3beforeword"})
	}

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field,
		textValue("tier3afterword"), assetEntries(a, b, c))
	if p.Counts.WouldChange != 3 {
		t.Fatalf("want 3 would_change, got %+v", p.Counts)
	}

	// MID-WRITE-PHASE, and it has to be: the batch has taken its whole
	// asset tier and written the first target, and is stopped on the
	// second target's VALUE ROW rather than on any lock it takes
	// deliberately. Whatever it is holding on the post tier at this
	// moment, it acquired by writing.
	gate := e.openValueRowGate(b, field)
	done := e.launchApply(ctx, p.Token, "tier 3 holds no post lock", intp(3))
	e.waitForBlockedContender(t, "the middle target's value row")

	probe, err := e.batchFixture.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("probe begin: %v", err)
	}
	probeFailure := ""
	for _, post := range ps {
		if _, err := probe.Exec(context.Background(),
			`SELECT id FROM posts WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, post); err != nil {
			probeFailure = fmt.Sprintf("post %s could NOT be locked while the batch was "+
				"mid-write-phase: %v", post, err)
			break
		}
	}
	_ = probe.Rollback(context.Background())

	gate.commit()
	run := awaitApply(t, done)

	if probeFailure != "" {
		t.Fatalf("TIER 3 TOOK A POST LOCK DURING ITS WRITES. %s", probeFailure)
	}
	t.Logf("STRUCTURAL: every containing post was lockable IMMEDIATELY while the batch was "+
		"parked mid-write-phase, over %d posts", len(ps))

	if run.err != nil || run.res.OK == nil {
		t.Fatalf("the batch commits: %v %+v", run.err, run.res.Refusal)
	}
	assertEveryTargetChanged(t, run.res.OK, 3)
	for i, post := range ps {
		if !e.postDocumentContains(post, "tier3afterword") {
			t.Fatalf("post %d must carry the committed value", i)
		}
		e.assertPostDocumentIsFresh(t, post, fmt.Sprintf("post %d after the batch committed", i))
	}
}
