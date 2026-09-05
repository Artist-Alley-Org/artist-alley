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
	tx pgx.Tx
	t  *testing.T
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
	return &raceGate{tx: tx, t: e.t}
}

// commit releases the gate by COMMITTING the competing change, so the
// contender resumes into a world that has genuinely moved.
func (g *raceGate) commit() {
	g.t.Helper()
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
// A76 — SEAM 7: MINT AUTHORITY
// ---------------------------------------------------------------------------

// The caller's `fields.vocabulary.extend` is revoked while the apply is
// blocked BEFORE it re-reads their authority.
//
// The gate holds the FIELD DEFINITION row, which the apply must take
// FOR UPDATE before it re-resolves the caller — so the revocation
// commits while the apply provably has not yet asked the question. NO
// STALE-AUTHORITY MINT: the apply refuses, and the options document is
// byte-identical.
func TestBatchRace_MintAuthorityRevocation(t *testing.T) {
	e := newBatchRaceEnv(t)
	owner, _ := e.bulkOperator("racemint")
	e.grant(owner, capVocabExtend, nil)
	ctx := e.identity(owner)

	field := e.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	asset := e.asset(&owner, nil)

	p := e.mustPreview(ctx, openapi.BatchModeOverwrite, field, optionsValue("race-term"), assetEntries(asset))
	if p.MintableTerms == nil || len(*p.MintableTerms) != 1 {
		t.Fatalf("the fixture needs a mintable term, got %+v", p.MintableTerms)
	}
	before := string(e.optionsDoc(field))

	// The gate takes the SAME field_definition lock the apply must hold
	// before it reads authority, and revokes inside that window.
	gate := e.openGate(`
		WITH held AS (SELECT id FROM field_definition WHERE id = $1 FOR UPDATE)
		DELETE FROM user_capability_grants
		 WHERE user_ref = $2 AND capability_code = $3 AND team_id IS NULL
		   AND EXISTS (SELECT 1 FROM held)`,
		field, owner, capVocabExtend)

	res := e.race(t, "field_definition row", gate, ctx, p.Token, "extend revoked under us", intp(1))

	e.wantRefusal(res, 403, openapi.BatchVocabularyExtendRequired)
	if e.rowExists(asset, field) {
		t.Fatal("ZERO writes")
	}
	if after := string(e.optionsDoc(field)); after != before {
		t.Fatalf("NO STALE-AUTHORITY MINT: the options document must be byte-identical\nbefore=%s\nafter=%s",
			before, after)
	}
	if e.tokenConsumed(p.OperationId.String()) {
		t.Fatal("the token stays spendable")
	}
}
