// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// APPLY — spending the token (#1173, #1119, ADR 0019).
//
// # The invariant this file exists to hold
//
//	TOKEN CONSUMPTION, THE DURABLE FIELD AND VOCABULARY MUTATIONS, AND
//	THE OPERATION'S SINGLE AUDIT ENVELOPE ARE ONE ATOMIC COMMITTED
//	OUTCOME.
//
// There are EXACTLY TWO durable results and no third:
//
//  1. A PRE-WRITE REFUSAL commits no field value, no term and no
//     envelope, and LEAVES THE TOKEN USABLE.
//  2. A COMMITTED APPLY — including a partial one, and including one
//     where would_change was zero — commits its result, exactly one
//     envelope and the consumption TOGETHER.
//
// A lost HTTP response therefore never makes a spent token spendable,
// and a 200 is not the consumption boundary — it is the normal HTTP
// representation of a committed result that would have been committed
// anyway. Consumption is an UPDATE inside the same transaction as the
// writes, so the transaction IS the boundary, and a rollback un-spends
// the token as surely as it un-writes the rows.
//
// # The validation precedence, and why it is exactly this
//
//	NO TOKEN-BOUND SEMANTIC INFORMATION — the mode, the would_change
//	count, the field, the target set, the expiry, the consumption
//	state, or the expected confirmation count — MAY INFLUENCE ANY
//	EXTERNALLY VISIBLE RESPONSE UNTIL INTEGRITY AND CALLER BINDING
//	HAVE BOTH SUCCEEDED.
//
// The attack this closes is specific. Apply does NOT send the mode. So
// a server that validated `confirm_count` against the token's mode
// before checking who the token belongs to would answer a question
// about SOMEBODY ELSE'S TOKEN: present another user's token with the
// count omitted, and `confirm_count_required` versus
// `confirm_count_not_applicable` says whether their preview was an
// overwrite or a fill. Do it twice and the expiry and consumption
// states fall out the same way. That is an enumeration oracle over
// every preview on the instance, built entirely out of refusals, and it
// defeats the whole point of collapsing the invalid cases into one 403.
//
// So: token-independent request-shape checks may run first — they leak
// nothing, being facts about the schema — and then, in EXACTLY this
// order, integrity, caller binding, consumption, expiry, mode-specific
// confirmation, current authority and configuration, and the committed
// apply.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// tokenInvalidMessage is THE ONE MESSAGE. Malformed, unknown, tampered
// and another caller's token all answer with these exact bytes and this
// exact status, and they stay identical however else the token differs
// — expired, consumed, overwrite-mode, fill-mode.
//
// A constant rather than a formatted string, deliberately: a message
// built per branch is a message that will eventually differ per branch,
// and the difference is the oracle.
const tokenInvalidMessage = "the preview token is not valid for this caller"

func tokenInvalid() *batchRefusal {
	return &batchRefusal{
		Status:  403,
		Reason:  openapi.BatchPreviewTokenInvalid,
		Message: tokenInvalidMessage,
	}
}

// ApplyBatchAssetFieldEdit spends a preview token.
func (h *Handler) ApplyBatchAssetFieldEdit(
	ctx context.Context,
	req openapi.ApplyBatchAssetFieldEditRequestObject,
) (openapi.ApplyBatchAssetFieldEditResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil || id.IsAnonymous() {
		return openapi.ApplyBatchAssetFieldEdit401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return applyRefusal(refuse(400, openapi.BatchTokenRequired, "missing body")), nil
	}

	out, err := h.applyBatch(ctx, id, *req.Body)
	if err != nil {
		if r, ok := asBatchRefusal(err); ok {
			return applyRefusal(r), nil
		}
		return nil, err
	}
	return openapi.ApplyBatchAssetFieldEdit200JSONResponse(out), nil
}

func applyRefusal(r *batchRefusal) openapi.ApplyBatchAssetFieldEditResponseObject {
	body := r.body()
	switch r.Status {
	case 400:
		return openapi.ApplyBatchAssetFieldEdit400JSONResponse(body)
	case 403:
		return openapi.ApplyBatchAssetFieldEdit403JSONResponse(body)
	case 409:
		return openapi.ApplyBatchAssetFieldEdit409JSONResponse(body)
	default:
		return openapi.ApplyBatchAssetFieldEdit422JSONResponse(body)
	}
}

func (h *Handler) applyBatch(
	ctx context.Context,
	id *auth.Identity,
	body openapi.BatchAssetFieldApplyRequest,
) (openapi.BatchAssetFieldApplyResult, error) {
	var zero openapi.BatchAssetFieldApplyResult

	// ── PHASE 0 — TOKEN-INDEPENDENT REQUEST SHAPE ──────────────────
	//
	// May run first because every one of these is a fact about the
	// SCHEMA rather than about anybody's preview. The bounds below are
	// CONSTANTS; nothing here reads the token.
	if body.Token == "" {
		return zero, refuse(400, openapi.BatchTokenRequired, "a preview token is required")
	}
	reason, err := validateBatchReason(body.Reason)
	if err != nil {
		return zero, err
	}
	if body.ConfirmCount != nil {
		n := *body.ConfirmCount
		if n < 0 || n > batchExpandedTargetCeiling {
			// The CONSTANT bounds only. Whether a count is required at
			// all, forbidden, or equal to the expected value are all
			// token-bound and are checked at step 5, after binding.
			return zero, refuse(400, openapi.BatchConfirmCountInvalid,
				"the confirmation count must be an integer between 0 and %d", batchExpandedTargetCeiling)
		}
	}

	// ── STEP 1 — INTEGRITY ─────────────────────────────────────────
	hash, ok := batchTokenHash(body.Token)
	if !ok {
		return zero, tokenInvalid()
	}
	q := New(h.Pool)
	row, err := q.GetBatchPreviewByTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// UNKNOWN. Byte-identical to malformed above and to
			// wrong-caller below.
			return zero, tokenInvalid()
		}
		return zero, fmt.Errorf("metadata: load preview: %w", err)
	}

	// ── STEP 2 — CALLER BINDING ────────────────────────────────────
	//
	// Everything below this line may speak about the token. Nothing
	// above it did.
	if !batchTokenBoundTo(row.CallerUserRef, id.UserRef) {
		return zero, tokenInvalid()
	}

	// ── STEP 3 — CONSUMED ──────────────────────────────────────────
	//
	// BEFORE EXPIRY, and the order is a decision rather than an
	// accident. A token that is BOTH consumed and expired answers
	// preview_consumed, because that tells the operator THEIR OPERATION
	// ALREADY RAN. preview_expired would tell them it never happened
	// and invite them to run it a second time.
	if row.ConsumedAt.Valid {
		return zero, refuse(409, openapi.BatchPreviewConsumed,
			"this preview has already been applied; re-preview to make another change")
	}

	// ── STEP 4 — EXPIRY ────────────────────────────────────────────
	if row.ExpiresAt.Valid && time.Now().After(row.ExpiresAt.Time) {
		return zero, refuse(409, openapi.BatchPreviewExpired,
			"this preview has expired; re-preview to see the current state")
	}

	payload, err := decodeBatchPayload(row.Payload)
	if err != nil {
		return zero, err
	}
	mode := batchMode(row.Mode)

	// ── STEP 5 — MODE-SPECIFIC CONFIRMATION ────────────────────────
	//
	// Reachable ONLY by the token's own caller, which is the whole
	// reason it sits here and not in Phase 0.
	if err := validateConfirmCount(mode, body.ConfirmCount, payload.Counts.WouldChange); err != nil {
		return zero, err
	}

	// ── STEPS 6 AND 7 — one transaction, one committed outcome ─────
	return h.commitBatch(ctx, id, row, payload, mode, reason, body.ConfirmCount)
}

// validateConfirmCount is step 5.
//
// The denominator is WOULD_CHANGE and not `eligible`, and that is the
// point of a typed confirmation: the number an operator types is the
// number of records that will actually change. Confirming `eligible`
// would have them type a number that includes every target the
// operation leaves alone, which on a `remove` over a mixed selection is
// routinely several times larger.
func validateConfirmCount(mode batchMode, supplied *int, wouldChange int) error {
	needs := mode == modeOverwrite || mode == modeRemove
	switch {
	case needs && supplied == nil:
		return refuse(400, openapi.BatchConfirmCountRequired,
			"%s requires a confirmation count naming how many records will change", string(mode))
	case !needs && supplied != nil:
		// REFUSED rather than ignored. A count supplied where none
		// applies means the client and the server disagree about what
		// the operation is, and silently discarding it would let that
		// disagreement reach the records.
		return refuse(400, openapi.BatchConfirmCountNotApplicable,
			"%s does not take a confirmation count", string(mode))
	case needs && *supplied != wouldChange:
		r := refuse(400, openapi.BatchConfirmCountMismatch,
			"the confirmation count does not match this preview: %d records will change", wouldChange)
		expected, actual := wouldChange, *supplied
		r.Expected, r.Actual = &expected, &actual
		return r
	}
	return nil
}

// commitBatch is steps 6 and 7: ONE transaction whose commit is the
// operation's whole durable outcome.
//
// The order inside it is load-bearing at every step:
//
//  1. CONSUME the token, under its row lock. First, so two concurrent
//     replays cannot both see it unconsumed — the second blocks on
//     the lock and then matches zero rows.
//  2. LOCK THE FIELD DEFINITION with FOR UPDATE, BEFORE reading it.
//     Lock-then-read, never read-then-lock: a lock taken after the
//     read would serialise the writes while still letting the batch
//     validate against a definition that had already changed.
//  3. RE-RESOLVE THE CALLER'S AUTHORITY from this transaction, not
//     from the request-time cache.
//  4. LOCK THE REFERENCE TARGET with FOR SHARE, so its liveness and
//     every write using it are atomic.
//  5. Per target: LOCK THE SUBJECT with FOR SHARE before reading its
//     owner and team, then write guarded on the preview's set_at.
//  6. Mint any term at least one successful write actually stored.
//  7. Record EXACTLY ONE audit envelope.
//
// Any refusal before the commit rolls all of it back, including the
// consumption — which is what makes "a pre-write refusal leaves the
// token usable" a property of the database rather than a promise.
func (h *Handler) commitBatch(
	ctx context.Context,
	id *auth.Identity,
	row GetBatchPreviewByTokenHashRow,
	payload batchTokenPayload,
	mode batchMode,
	reason string,
	confirmCount *int,
) (openapi.BatchAssetFieldApplyResult, error) {
	var zero openapi.BatchAssetFieldApplyResult

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return zero, fmt.Errorf("metadata: begin batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qTx := New(tx)

	// ── 1. THE SINGLE-USE LATCH ────────────────────────────────────
	if _, err := qTx.ConsumeBatchPreview(ctx, row.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent replay won the row. Zero additional writes,
			// zero mints, zero envelopes — this transaction has done
			// nothing else yet and is about to roll back.
			return zero, refuse(409, openapi.BatchPreviewConsumed,
				"this preview has already been applied; re-preview to make another change")
		}
		return zero, fmt.Errorf("metadata: consume preview: %w", err)
	}

	// ── 2. THE BATCH-WIDE DEFINITION AND VOCABULARY SEAM ───────────
	locked, err := qTx.LockFieldDefinitionForBatch(ctx, row.FieldID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, refuse(409, openapi.BatchDefinitionDrift,
				"the field has been removed since this preview was taken")
		}
		return zero, fmt.Errorf("metadata: lock field definition: %w", err)
	}
	field := lockedFieldDefinition(locked)

	// TWO fingerprints and two refusals, not one of each: a
	// configuration change and a curation change call for different
	// corrections, and one combined hash could only say "something
	// moved".
	if definitionFingerprint(field) != payload.DefinitionFingerprint {
		return zero, refuse(409, openapi.BatchDefinitionDrift,
			"%s has been reconfigured since this preview was taken; re-preview to see the current rules",
			field.Code).withField(field.Code)
	}
	if vocabularyFingerprint(field.Options) != payload.VocabularyFingerprint {
		return zero, refuse(409, openapi.BatchVocabularyDrift,
			"%s's vocabulary has changed since this preview was taken; re-preview to see the current terms",
			field.Code).withField(field.Code)
	}

	// ── 3. CURRENT EFFECTIVE AUTHORITY, RE-READ IN THIS TX ─────────
	//
	// EFFECTIVE, never raw grant-set equality. A caller who lost one
	// direct grant while a role still confers the capability has not
	// lost anything, and refusing them would be asserting about their
	// grant rows rather than about their authority.
	//
	// Ordered AFTER the field lock, which is what makes the mint-
	// authority seam observable: a contender blocked on that lock has
	// not yet read authority, so a revocation that commits while it
	// waits is SEEN when it proceeds.
	current, err := auth.ResolveEffectiveIdentity(ctx, tx, id)
	if err != nil {
		return zero, err
	}

	if !bulkAdmitted(current) {
		return zero, refuse(403, openapi.BatchBulkCapabilityRequired,
			"batch metadata editing requires %s, globally or scoped to a team", CapBulkEdit)
	}
	if !effectiveWritePermission(current, field) {
		return zero, refuse(403, openapi.BatchFieldWriteCapabilityRequired,
			"writing %s requires %s", field.Code, *field.WriteCapability).withField(field.Code)
	}
	if len(payload.Mintable) > 0 && !canExtendVocabulary(current) {
		// ⚠️ `fields.vocabulary.extend` is GLOBAL-ONLY (canExtendVocabulary
		// is `id.Can(code)` with no InTeam), reproduced exactly. A caller
		// holding it only team-scoped cannot mint here, because they
		// cannot mint on the single-target path either.
		return zero, refuse(403, openapi.BatchVocabularyExtendRequired,
			"creating a term in %s requires %s", field.Code, CapVocabularyExtend).withField(field.Code)
	}

	value := payload.Value.batchValue()

	// ── 4. THE REFERENCE-LIVENESS SEAM ─────────────────────────────
	if field.Type == "reference" && value.Ref.Valid {
		if _, err := qTx.LockBatchReferenceTarget(ctx, value.Ref); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// DELIBERATELY NOT dangling_reference. That code says
				// the target never resolved; this one says it resolved
				// when the operator looked and has since stopped, and
				// the remedy is to re-preview rather than to correct
				// the value.
				return zero, refuse(409, openapi.BatchReferenceInvalidated,
					"%s: referenced asset %s no longer exists; nothing was written",
					field.Code, uuid.UUID(value.Ref.Bytes)).withField(field.Code)
			}
			return zero, fmt.Errorf("metadata: lock reference target: %w", err)
		}
	}

	// ── 5. THE PER-TARGET WRITES ───────────────────────────────────
	outcomes, committed, err := h.writeBatchTargets(ctx, tx, qTx, current, field, mode, value, payload)
	if err != nil {
		return zero, err
	}

	// ── 6. THE COUPLED MINT ────────────────────────────────────────
	//
	// A new term commits ONLY IF at least one successful write actually
	// stored it. "The preview predicted would_change > 0" is not
	// enough: if every one of those targets ended conflict, gone,
	// unauthorized_at_apply or error, the operator's word never reached
	// a single record and the catalogue must not have grown a term
	// because of it.
	mintedTerms, err := h.mintCommittedTerms(ctx, qTx, field, payload.Mintable, committed)
	if err != nil {
		return zero, err
	}

	// ── 7. EXACTLY ONE AUDIT ENVELOPE ──────────────────────────────
	//
	// In this transaction, and its failure FAILS THE APPLY. See
	// RecordBatchAssetFieldEditInTx for why this one writer is not
	// best-effort: the envelope is a member of the atomic outcome, and
	// an optional member of an atomic outcome is not a member of it.
	if h.Audit != nil {
		env := buildBatchEnvelope(row, payload, field, mode, reason, confirmCount, outcomes, mintedTerms)
		if err := h.Audit.RecordBatchAssetFieldEditInTx(
			ctx, audit.New(tx), batchRequest(ctx), id.UserRef, env); err != nil {
			return zero, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return zero, fmt.Errorf("metadata: commit batch: %w", err)
	}

	// AFTER the commit, and only when a term actually committed. A
	// cache dropped before the write lands is a cache that repopulates
	// with the pre-write document.
	if len(mintedTerms) > 0 {
		h.InvalidateFieldVocabulary(ctx, row.FieldID)
	}

	return batchResultBody(row, payload, field, mode, outcomes, mintedTerms), nil
}

// batchOutcome is one would_change target's fate.
type batchOutcome struct {
	AssetID uuid.UUID
	Outcome openapi.BatchAssetFieldOutcome
	Reason  *openapi.BatchAssetFieldUnauthorizedReason
	// Terms are the canonical slugs this target actually STORED. Only
	// a target whose write succeeded contributes any, which is what
	// couples the mint to a real write.
	Terms []string
}

// writeBatchTargets performs the per-target work: the subject lock, the
// three per-target gate re-checks, and the guarded write.
//
// Apply re-checks G1, G2 and G5 PER TARGET — G3 and G4 are batch-wide
// and were settled above. It re-checks EFFECTIVE permission, so a
// caller whose GLOBAL bulk grant was revoked while a SCOPED grant for
// one of the selection's teams remains is NOT failed wholesale: the
// covered team proceeds and the uncovered one becomes
// unauthorized_at_apply. Whole-batch failure there would refuse work
// the caller is still entitled to do.
func (h *Handler) writeBatchTargets(
	ctx context.Context,
	tx pgx.Tx,
	qTx *Queries,
	current *auth.Identity,
	field FieldDefinition,
	mode batchMode,
	value batchValue,
	payload batchTokenPayload,
) ([]batchOutcome, map[string]struct{}, error) {
	out := make([]batchOutcome, 0, payload.Counts.WouldChange)
	// The union of canonical terms that SUCCESSFUL writes actually
	// stored. Not a boolean "did anything commit": a batch can succeed
	// on targets that stored none of the new terms, which is exactly
	// what `remove` does — its residual is a subset of what the target
	// already held, so a brand-new term named in a removal is stored by
	// nobody and must not be created.
	stored := map[string]struct{}{}

	for _, t := range payload.Targets {
		if t.Partition != string(openapi.BatchPartitionWouldChange) {
			// Apply writes ONLY the would_change subset and NEVER
			// re-expands. A post that gained a member after the
			// preview does not enlarge the operation the operator
			// confirmed with a typed number.
			continue
		}
		assetID, err := uuid.Parse(t.AssetID)
		if err != nil {
			continue
		}
		pgAsset := pgtype.UUID{Bytes: assetID, Valid: true}
		res := batchOutcome{AssetID: assetID}

		// THE SUBJECT SEAM. FOR SHARE, taken BEFORE the owner and team
		// are read, so a competing ownership transfer, team move or
		// soft delete either committed before this read (and is seen)
		// or is blocked until this batch commits (and is ordered after
		// it). See LockBatchTargetSubject for why "same transaction"
		// is not sufficient on its own.
		subjectRow, err := qTx.LockBatchTargetSubject(ctx, pgAsset)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// SOFT-DELETED since the preview. An ARCHIVED asset is
				// NOT gone — the probe filters deleted_at and never
				// status — and is written below like any other.
				res.Outcome = openapi.BatchOutcomeGone
				out = append(out, res)
				continue
			}
			return nil, nil, fmt.Errorf("metadata: lock batch subject: %w", err)
		}
		subject := batchSubject{
			ID: assetID, OwnerRef: subjectRow.OwnerUserRef,
			AssetType: subjectRow.AssetType, Live: true,
		}
		if subjectRow.TeamID.Valid {
			team := uuid.UUID(subjectRow.TeamID.Bytes)
			subject.TeamID = &team
		}

		if !bulkScopeCovers(current, subject.TeamID) {
			reason := openapi.BatchUnauthorizedBulkScope
			res.Outcome, res.Reason = openapi.BatchOutcomeUnauthorizedAtApply, &reason
			out = append(out, res)
			continue
		}
		if !subjectAuthorised(current, subject) {
			reason := openapi.BatchUnauthorizedSubjectAuthority
			res.Outcome, res.Reason = openapi.BatchOutcomeUnauthorizedAtApply, &reason
			out = append(out, res)
			continue
		}
		if !fieldReadableForBatch(current, field, subject.TeamID) {
			reason := openapi.BatchUnauthorizedUnreadable
			res.Outcome, res.Reason = openapi.BatchOutcomeUnauthorizedAtApply, &reason
			out = append(out, res)
			continue
		}

		next := value
		if len(t.NextOptions) > 0 {
			// The set modes' per-target result, computed at preview
			// against the value the guard below proves has not moved.
			next.Options = t.NextOptions
		}

		changed, err := h.writeOneBatchTarget(ctx, qTx, field, pgAsset, t, next, current.UserRef)
		if err != nil {
			return nil, nil, err
		}
		if !changed {
			res.Outcome = openapi.BatchOutcomeConflict
			out = append(out, res)
			continue
		}
		res.Outcome = openapi.BatchOutcomeChanged
		if field.Type == "multi_select" && !t.Delete {
			res.Terms = next.Options
		} else if !t.Delete {
			res.Terms = vocabularySlugs(field.Type, next.Text, next.Options)
		}
		for _, term := range res.Terms {
			stored[term] = struct{}{}
		}
		out = append(out, res)
	}
	return out, stored, nil
}

// writeOneBatchTarget performs the guarded write for one target and
// reports whether it landed.
//
// GUARDED ON THE PREVIEW'S set_at, using 20a's own statements. The
// precondition and the mutation are ONE statement, so a competing
// writer cannot fit between them; a zero-row result IS the conflict.
// Which of the two arms applies is decided by whether the PREVIEW saw
// a row, not by whether one is there now — "the value was absent and
// still is" and "the value was absent and somebody wrote one" are
// different worlds and only the first may proceed.
func (h *Handler) writeOneBatchTarget(
	ctx context.Context,
	qTx *Queries,
	field FieldDefinition,
	pgAsset pgtype.UUID,
	t batchTokenTarget,
	next batchValue,
	callerRef int64,
) (bool, error) {
	prev, err := qTx.GetAssetFieldValue(ctx, GetAssetFieldValueParams{
		AssetID: pgAsset, FieldID: field.ID,
	})
	hadOld := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("metadata: load previous: %w", err)
	}

	var oldJSON []byte
	if hadOld {
		oldJSON, _ = valueRowToJSON(prev.ValueText, prev.ValueNum, prev.ValueDate, prev.ValueOptions, prev.ValueRef, field.Type)
	}

	// THE REMOVAL ARM — `remove` emptying an OPTIONAL multi_select.
	// The row is DELETED rather than written as `[]`, because a
	// multi_select row holding an empty array is a shape the
	// single-target writer refuses and the batch has no reason to
	// invent it.
	if t.Delete {
		if !t.Present || t.SetAt == nil {
			return false, nil
		}
		if _, err := qTx.DeleteAssetFieldValueIfUnchanged(ctx, DeleteAssetFieldValueIfUnchangedParams{
			AssetID:          pgAsset,
			FieldID:          field.ID,
			IfUnchangedSince: pgtype.Timestamptz{Time: *t.SetAt, Valid: true},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, fmt.Errorf("metadata: batch delete: %w", err)
		}
		if err := qTx.AppendAssetFieldValueHistory(ctx, AppendAssetFieldValueHistoryParams{
			AssetID: pgAsset, FieldID: field.ID, OldValue: oldJSON, NewValue: nil,
			SetBy: "manual", ChangedByUserRef: &callerRef,
		}); err != nil {
			return false, fmt.Errorf("metadata: append history: %w", err)
		}
		return true, nil
	}

	params := UpsertAssetFieldValueParams{
		AssetID: pgAsset, FieldID: field.ID,
		ValueText: next.Text, ValueNum: next.Num, ValueDate: next.Date,
		ValueOptions: next.Options, ValueRef: next.Ref,
		SetBy: "manual", SetByUserRef: &callerRef,
	}

	var written AssetFieldValue
	if t.Present && t.SetAt != nil {
		written, err = qTx.UpdateAssetFieldValueIfUnchanged(ctx, UpdateAssetFieldValueIfUnchangedParams{
			ValueText: params.ValueText, ValueNum: params.ValueNum, ValueDate: params.ValueDate,
			ValueOptions: params.ValueOptions, ValueRef: params.ValueRef,
			SetBy: params.SetBy, SetByUserRef: params.SetByUserRef,
			AssetID: pgAsset, FieldID: field.ID,
			IfUnchangedSince: pgtype.Timestamptz{Time: *t.SetAt, Valid: true},
		})
	} else {
		written, err = qTx.InsertAssetFieldValueWhenAbsent(ctx, InsertAssetFieldValueWhenAbsentParams{
			AssetID: pgAsset, FieldID: field.ID,
			ValueText: params.ValueText, ValueNum: params.ValueNum, ValueDate: params.ValueDate,
			ValueOptions: params.ValueOptions, ValueRef: params.ValueRef,
			SetBy: params.SetBy, SetByUserRef: params.SetByUserRef,
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("metadata: batch upsert: %w", err)
	}

	newJSON, _ := valueRowToJSON(written.ValueText, written.ValueNum, written.ValueDate,
		written.ValueOptions, written.ValueRef, field.Type)
	if err := qTx.AppendAssetFieldValueHistory(ctx, AppendAssetFieldValueHistoryParams{
		AssetID: pgAsset, FieldID: field.ID, OldValue: oldJSON, NewValue: newJSON,
		SetBy: "manual", ChangedByUserRef: &callerRef,
	}); err != nil {
		return false, fmt.Errorf("metadata: append history: %w", err)
	}
	return true, nil
}

// mintCommittedTerms grows the vocabulary, and ONLY for terms a
// successful write ACTUALLY STORED.
//
// The coupling is the contract, and "did anything commit at all" is NOT
// a sufficient test of it. A batch can succeed on targets that stored
// none of the new terms:
//
//   - `remove` naming a term the field does not have. The term is
//     mintable, every residual is a SUBSET of what its target already
//     held, so no row ever carries it — and minting it would grow the
//     catalogue with a term whose only appearance in the operation was
//     an instruction to take it away.
//   - a partial apply where the only targets that would have carried a
//     new term all conflicted, went away, or lost authority, while
//     other targets succeeded.
//
// So the terms come from the OUTCOMES — what each successful write
// stored — intersected with what the preview said was mintable. If
// nothing stored a given term the options document is left
// BYTE-IDENTICAL and no cache is invalidated, because nothing changed.
func (h *Handler) mintCommittedTerms(
	ctx context.Context,
	qTx *Queries,
	field FieldDefinition,
	mintable []string,
	stored map[string]struct{},
) ([]string, error) {
	commit := make([]string, 0, len(mintable))
	for _, term := range mintable {
		if _, wrote := stored[term]; wrote {
			commit = append(commit, term)
		}
	}
	if len(commit) == 0 {
		return nil, nil
	}
	// EnsureOpenVocabularyTerms takes its own FOR UPDATE on the row
	// this transaction already holds under LockFieldDefinitionForBatch,
	// which is a re-entrant no-op, and then performs the read-modify-
	// write against the LIVE document. Reused rather than reimplemented
	// so a term the batch creates is normalised by exactly the rule
	// that normalises one the admin editor creates.
	res, err := EnsureOpenVocabularyTerms(ctx, qTx, field.ID, commit, true)
	if err != nil {
		var rej *slugRejection
		if errors.As(err, &rej) {
			return nil, refuse(409, openapi.BatchVocabularyDrift,
				"%s: %q can no longer be created; re-preview to see the current terms",
				field.Code, rej.Slug).withField(field.Code)
		}
		return nil, err
	}
	sort.Strings(res.Created)
	return res.Created, nil
}

// lockedFieldDefinition adapts the locked row to the shared shape every
// gate in this package already takes, so none of them needs a second
// signature for the batch.
func lockedFieldDefinition(r LockFieldDefinitionForBatchRow) FieldDefinition {
	return FieldDefinition{
		ID: r.ID, Code: r.Code, Label: r.Label, Type: r.Type,
		SubjectKind: r.SubjectKind, AppliesTo: r.AppliesTo, Required: r.Required,
		Status: r.Status, Options: r.Options, OpenVocabulary: r.OpenVocabulary,
		MirrorsColumn: r.MirrorsColumn, ReadOnly: r.ReadOnly, RegexpFilter: r.RegexpFilter,
		ReadCapability: r.ReadCapability, WriteCapability: r.WriteCapability,
		DisplayCondition: r.DisplayCondition,
	}
}

// buildBatchEnvelope assembles the one audit record.
//
// NO FIELD VALUE, old or new. Unreadable and refused targets contribute
// their id and a non-value-sensitive partition label only. See
// audit.BatchAssetFieldEditEnvelope for the whole argument.
func buildBatchEnvelope(
	row GetBatchPreviewByTokenHashRow,
	payload batchTokenPayload,
	field FieldDefinition,
	mode batchMode,
	reason string,
	confirmCount *int,
	outcomes []batchOutcome,
	minted []string,
) audit.BatchAssetFieldEditEnvelope {
	env := audit.BatchAssetFieldEditEnvelope{
		OperationID:  uuid.UUID(row.ID.Bytes).String(),
		Mode:         string(mode),
		FieldID:      uuid.UUID(field.ID.Bytes).String(),
		FieldCode:    field.Code,
		Reason:       reason,
		ConfirmCount: confirmCount,

		Expanded:     payload.Counts.Expanded,
		Eligible:     payload.Counts.Eligible,
		WouldChange:  payload.Counts.WouldChange,
		NoOp:         payload.Counts.NoOp,
		Refused:      payload.Counts.Refused,
		Inapplicable: payload.Counts.Inapplicable,
		Unreadable:   payload.Counts.Unreadable,
		Unauthorized: payload.Counts.Unauthorized,

		SelectionEntryCount: payload.SelectionEntryCount,
		CommittedTerms:      minted,
		TargetIDs:           map[string][]string{},
	}

	// Every expanded target's id under its PREVIEW partition label, so
	// the envelope accounts for the whole selection and not only the
	// part that was written.
	for _, t := range payload.Targets {
		if t.Partition == string(openapi.BatchPartitionWouldChange) {
			continue
		}
		env.TargetIDs[t.Partition] = append(env.TargetIDs[t.Partition], t.AssetID)
	}
	reasons := map[string]int{}
	for _, o := range outcomes {
		key := string(o.Outcome)
		env.TargetIDs[key] = append(env.TargetIDs[key], o.AssetID.String())
		switch o.Outcome {
		case openapi.BatchOutcomeChanged:
			env.Changed++
		case openapi.BatchOutcomeConflict:
			env.Conflict++
		case openapi.BatchOutcomeGone:
			env.Gone++
		case openapi.BatchOutcomeUnauthorizedAtApply:
			env.UnauthorizedAtApply++
			if o.Reason != nil {
				reasons[string(*o.Reason)]++
			}
		default:
			env.Errored++
		}
	}
	if len(reasons) > 0 {
		env.UnauthorizedAtApplyReasons = reasons
	}
	return env
}

func batchResultBody(
	row GetBatchPreviewByTokenHashRow,
	payload batchTokenPayload,
	field FieldDefinition,
	mode batchMode,
	outcomes []batchOutcome,
	minted []string,
) openapi.BatchAssetFieldApplyResult {
	out := openapi.BatchAssetFieldApplyResult{
		OperationId: openapi_types.UUID(uuid.UUID(row.ID.Bytes)),
		Mode:        mode,
		FieldId:     openapi_types.UUID(uuid.UUID(field.ID.Bytes)),
		FieldCode:   field.Code,
		Counts:      payload.Counts.wire(),
		Targets:     make([]openapi.BatchAssetFieldApplyTarget, 0, len(outcomes)),
	}
	for _, o := range outcomes {
		out.Targets = append(out.Targets, openapi.BatchAssetFieldApplyTarget{
			AssetId:            openapi_types.UUID(o.AssetID),
			Outcome:            o.Outcome,
			UnauthorizedReason: o.Reason,
		})
		switch o.Outcome {
		case openapi.BatchOutcomeChanged:
			out.OutcomeCounts.Changed++
		case openapi.BatchOutcomeConflict:
			out.OutcomeCounts.Conflict++
		case openapi.BatchOutcomeGone:
			out.OutcomeCounts.Gone++
		case openapi.BatchOutcomeUnauthorizedAtApply:
			out.OutcomeCounts.UnauthorizedAtApply++
		default:
			out.OutcomeCounts.Error++
		}
	}
	if len(minted) > 0 {
		terms := append([]string(nil), minted...)
		out.CommittedTerms = &terms
	}
	return out
}
