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
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
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
