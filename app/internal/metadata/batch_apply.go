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
	case 422:
		return openapi.ApplyBatchAssetFieldEdit422JSONResponse(body)
	default:
		// EVERY status this operation can produce is named above.
		// The default is a last resort, not a bucket: a 404 fell
		// into it once and came back as a 422 wearing a reason
		// that described a different situation entirely. A new
		// status needs an arm here, not a shrug.
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
//  3. LOCK AND RE-RESOLVE THE CALLER'S AUTHORITY. The shared half of
//     the authority advisory lock FIRST, then the read — reading inside
//     the transaction is necessary and NOT sufficient, because at READ
//     COMMITTED an authority change can still commit between the read
//     and the writes it authorizes.
//  4. LOCK THE WHOLE ASSET TIER in ONE ascending FOR UPDATE pass:
//     every would-change subject AND any proposed reference target,
//     one statement, before a single value is written. Reference
//     liveness is decided here, batch-wide, from that one locked read.
//  5. Per target: read the owner and team off the row this transaction
//     already HOLDS, then write guarded on the preview's set_at.
//  6. Rebuild each DISTINCT containing post's search document exactly
//     once, ascending, after every write.
//  7. Mint any term at least one successful write actually stored.
//  8. Record EXACTLY ONE audit envelope.
//
// # Steps 4 and 6 are ONE correction, and it is a lock-order one
//
// Every field-value write drags a lock on `posts` behind it. The
// asset_field_value trigger rebuilds the subject's search_text, that
// UPDATE fires assets_member_post_search_text, and that rebuilds every
// containing post. So a batch that locked its subjects one at a time,
// interleaved with its writes, was holding a POST row from an earlier
// target while asking for the next ASSET row. An ordinary
// single-target write takes those two in the opposite order, and the
// pair deadlocked (SQLSTATE 40P01).
//
// The asset tier is therefore taken WHOLE and FIRST, and the post tier
// is SUPPRESSED during the writes and paid off once per post
// afterwards, ascending. The batch never asks for an assets row while
// holding a posts row, and no two acquirers walk the post tier in
// opposite directions. See migration 00067 for the database half.
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

	// ── 3. CURRENT EFFECTIVE AUTHORITY, LOCKED THEN RE-READ ────────
	//
	// EFFECTIVE, never raw grant-set equality. A caller who lost one
	// direct grant while a role still confers the capability has not
	// lost anything, and refusing them would be asserting about their
	// grant rows rather than about their authority.
	//
	// ⛔ SERIALIZED, because being inside the transaction is NOT enough.
	//
	// At READ COMMITTED every statement takes a fresh snapshot, so a
	// grant, a revoke, a role assignment or a closure change can commit
	// AFTER this read and BEFORE the writes it authorizes, and the
	// stale verdict would still let them through. This transaction locks
	// the field definition, the reference target and each subject — and
	// none of those is where authority lives. Authority lives in
	// `user_roles`, `roles`, `role_capabilities`,
	// `user_capability_grants`, `user_capability_revokes` and
	// `team_closure`.
	//
	// A row lock cannot close it either: the dangerous mutation is
	// frequently an INSERT — a revoke ADDS a row — and there is nothing
	// to lock before it exists. So the reader takes the shared half of
	// the authority advisory lock, held to COMMIT, and every production
	// path that mutates authority takes the exclusive half. See
	// auth.LockAuthorityShared for the full argument and the registry of
	// participating writers.
	//
	// BEFORE the read, not after: a lock taken afterwards would
	// serialize the writes while still letting the verdict be drawn from
	// a world that had already moved, which is the same shape of mistake
	// as locking after a graph walk.
	if err := auth.LockAuthorityShared(ctx, tx, id.UserRef); err != nil {
		return zero, err
	}
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

	// ── 4. THE ASSET TIER, LOCKED WHOLE AND IN ONE DIRECTION ───────
	live, err := h.lockBatchAssetTier(ctx, qTx, field, value, payload)
	if err != nil {
		return zero, err
	}

	// THE REFERENCE-LIVENESS SEAM, decided from that one locked read
	// and BEFORE any per-target outcome exists.
	//
	// The precedence is the point. A single id can be both a subject
	// and the proposed reference target, and the two roles refuse
	// differently: as a subject an absent or soft-deleted row is that
	// target's `gone`, as the reference target it is a batch-wide
	// refusal that writes nothing at all. reference_invalidated WINS,
	// and it wins HERE, before the write pass runs, so a response
	// carrying both a reference_invalidated and a per-target `gone` is
	// a shape that cannot occur.
	if field.Type == "reference" && value.Ref.Valid {
		if _, ok := live[uuid.UUID(value.Ref.Bytes)]; !ok {
			// DELIBERATELY NOT dangling_reference. That code says
			// the target never resolved; this one says it resolved
			// when the operator looked and has since stopped, and
			// the remedy is to re-preview rather than to correct
			// the value.
			return zero, refuse(409, openapi.BatchReferenceInvalidated,
				"%s: referenced asset %s no longer exists; nothing was written",
				field.Code, uuid.UUID(value.Ref.Bytes)).withField(field.Code)
		}
	}

	// ── 5. THE PER-TARGET WRITES ───────────────────────────────────
	outcomes, committed, err := h.writeBatchTargets(ctx, tx, qTx, current, field, mode, value, payload, live)
	if err != nil {
		return zero, err
	}

	// ── 6. THE COALESCED POST REBUILD ──────────────────────────────
	//
	// On EVERY success path, because a success that skips it commits
	// stale post documents.
	if err := h.rebuildBatchPostSearch(ctx, qTx, outcomes); err != nil {
		return zero, err
	}

	// ── 7. THE COUPLED MINT ────────────────────────────────────────
	//
	// A new term commits ONLY IF at least one successful write actually
	// stored it. "The preview predicted would_change > 0" is not
	// enough: if every one of those targets ended conflict, gone,
	// unauthorized_at_apply or error, the operator's word never reached
	// a single record and the catalogue must not have grown a term
	// because of it.
	mintedTerms, err := h.mintCommittedTerms(ctx, qTx, field, payload.Mintable, payload.MintableTerms, committed)
	if err != nil {
		return zero, err
	}

	// ── 8. EXACTLY ONE AUDIT ENVELOPE ──────────────────────────────
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

// lockBatchAssetTier takes the batch's ENTIRE asset tier in one
// ascending FOR UPDATE statement, and returns the LIVE rows by id.
//
// The set is the union of the would-change subjects and, on a
// reference field, the proposed reference target. One statement, so
// there is exactly one acquisition order and it is ascending id; one
// read, so the per-target gates below re-check G1, G2 and G5 against
// rows this transaction HOLDS rather than re-reading them.
//
// # Why FOR UPDATE rather than the FOR SHARE this replaced
//
// Every write in this batch ends in an UPDATE of the subject's
// `assets` row, which needs FOR NO KEY UPDATE. A FOR SHARE holder is
// therefore a holder that must UPGRADE, and two batches sharing one
// target would each hold FOR SHARE and each block on the upgrade,
// which is the deadlock the pre-lock exists to remove. FOR UPDATE is
// taken once, at the strength the transaction will ultimately need.
//
// FOR UPDATE is also the weakest mode that makes a membership INSERT
// queue behind the batch: a foreign key takes FOR KEY SHARE, which
// conflicts with FOR UPDATE and not with FOR NO KEY UPDATE. A
// membership DELETE takes no parent lock at all in either mode, which
// is why rebuild_post_search_text locks its post at entry as well.
//
// # Absent and soft-deleted are the SAME answer, and the caller maps it
//
// The statement does not filter `deleted_at`: it locks and returns
// whatever exists, and the classification happens here, because the
// two roles this set serves refuse differently. Filtering to the live
// rows is that classification, and NOTHING is filtered on `status`. An
// archived asset is a live subject and a valid reference target.
func (h *Handler) lockBatchAssetTier(
	ctx context.Context,
	qTx *Queries,
	field FieldDefinition,
	value batchValue,
	payload batchTokenPayload,
) (map[uuid.UUID]LockBatchAssetTierRow, error) {
	seen := make(map[uuid.UUID]struct{}, len(payload.Targets)+1)
	ids := make([]pgtype.UUID, 0, len(payload.Targets)+1)
	add := func(id uuid.UUID) {
		if _, dup := seen[id]; dup {
			// An id serving BOTH roles is locked and read ONCE. It is
			// still classified twice, under the precedence commitBatch
			// states.
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, pgtype.UUID{Bytes: id, Valid: true})
	}
	for _, t := range payload.Targets {
		if t.Partition != string(openapi.BatchPartitionWouldChange) {
			continue
		}
		assetID, err := uuid.Parse(t.AssetID)
		if err != nil {
			continue
		}
		add(assetID)
	}
	if field.Type == "reference" && value.Ref.Valid {
		// The reference target joins the SAME set even when there is
		// nothing to write. A zero-would-change apply is a real
		// operation, and it must still refuse batch-wide if the
		// reference it names has gone.
		add(uuid.UUID(value.Ref.Bytes))
	}

	live := make(map[uuid.UUID]LockBatchAssetTierRow, len(ids))
	if len(ids) == 0 {
		return live, nil
	}
	rows, err := qTx.LockBatchAssetTier(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("metadata: lock batch asset tier: %w", err)
	}
	for _, r := range rows {
		if r.DeletedAt.Valid {
			continue
		}
		live[uuid.UUID(r.ID.Bytes)] = r
	}
	return live, nil
}

// rebuildBatchPostSearch pays off what the suppression flag deferred:
// one rebuild per DISTINCT containing post, ascending post id, after
// every field-value write in the batch has landed.
//
// The set is derived from the targets that actually CHANGED. A target
// that ended conflict, gone, unauthorized_at_apply or error wrote
// nothing, so nothing about its posts moved and rebuilding them would
// be taking a post row lock for no reason.
//
// Ascending, and one call per post rather than one per written row:
// a thousand targets across four posts is four rebuilds. Each call
// takes its post FOR NO KEY UPDATE at entry, before it reads the
// member documents it bakes, so the document it writes is computed
// from the world it holds rather than from one it merely saw.
func (h *Handler) rebuildBatchPostSearch(
	ctx context.Context,
	qTx *Queries,
	outcomes []batchOutcome,
) error {
	written := make([]pgtype.UUID, 0, len(outcomes))
	for _, o := range outcomes {
		if o.Outcome != openapi.BatchOutcomeChanged {
			continue
		}
		written = append(written, pgtype.UUID{Bytes: o.AssetID, Valid: true})
	}
	if len(written) == 0 {
		return nil
	}
	posts, err := qTx.ListPostsContainingAssets(ctx, written)
	if err != nil {
		return fmt.Errorf("metadata: containing posts: %w", err)
	}
	for _, postID := range posts {
		if err := qTx.RebuildPostSearchText(ctx, postID); err != nil {
			return fmt.Errorf("metadata: rebuild post search text: %w", err)
		}
	}
	return nil
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
	live map[uuid.UUID]LockBatchAssetTierRow,
) ([]batchOutcome, map[string]struct{}, error) {
	// SUPPRESS the per-row asset-to-post search propagation for the
	// rest of THIS transaction, and only this one: set_config's third
	// argument is SET LOCAL, so the flag dies with the transaction and
	// cannot reach the next caller that borrows this connection.
	//
	// Setting it is taking on a debt. rebuildBatchPostSearch pays it,
	// on every success path, once per distinct containing post and in
	// ascending post id order. Suppressing without paying would commit
	// stale post documents; paying per row instead of per post is what
	// produced the inverted lock order in the first place.
	if payload.Counts.WouldChange > 0 {
		if err := qTx.SuppressAssetPostSearchPropagation(ctx); err != nil {
			return nil, nil, fmt.Errorf("metadata: suppress post propagation: %w", err)
		}
	}

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

		// THE SUBJECT SEAM, already held. lockBatchAssetTier took this
		// row FOR UPDATE before any write in this batch, so the owner
		// and team below are read off a row this transaction HOLDS: a
		// competing ownership transfer, team move or soft delete either
		// committed before that lock (and is seen here) or is blocked
		// until this batch commits (and is ordered after it). Stronger
		// than the FOR SHARE it replaced, which conflicted with the
		// same three writers but not with a membership insert.
		subjectRow, held := live[assetID]
		if !held {
			// ABSENT OR SOFT-DELETED since the preview. An ARCHIVED
			// asset is NOT gone: the tier filters deleted_at and never
			// status, and is written below like any other.
			res.Outcome = openapi.BatchOutcomeGone
			out = append(out, res)
			continue
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
// The contract is per TERM — "a new term may commit only if at least
// one successful mutation stores THAT canonical term" — so the test is
// per term, against the set the outcomes report, and not a single
// boolean asking whether anything at all committed.
//
// # Whether the two can currently differ
//
// Honestly: no. Of the three modes that can mint, every successful
// write stores the whole proposed set, so any commit at all stores
// every mintable term. `remove` was the case that pulled them apart —
// it could name a term the field does not have, mark it mintable, and
// then store nothing — and that is now fixed further upstream, where it
// belongs: a removal MATCHES and never mints (see
// resolveBatchVocabulary). With that closed, the intersection below
// currently reduces to the boolean.
//
// It is kept anyway, and not because two forms of the same test are
// better than one. It is kept because the boolean is only equivalent by
// a property of the CURRENT MODE SET, which is exactly the kind of
// coincidence a fifth mode quietly breaks — and this way the code says
// the rule rather than a consequence of it.
//
// If nothing stored a given term the options document is left
// BYTE-IDENTICAL and no cache is invalidated, because nothing changed.
func (h *Handler) mintCommittedTerms(
	ctx context.Context,
	qTx *Queries,
	field FieldDefinition,
	mintable []string,
	terms map[string]string,
	stored map[string]struct{},
) ([]string, error) {
	// The RAW TERMS, not the slugs. A created option is labelled with
	// the term it was created from, so handing the slug over would
	// label `character-design` as "character-design" where the
	// single-target writer labels it "Character Design". They resolve
	// to the same slug either way; only the label differs, and only
	// one of the two is what the operator typed.
	commit := make([]string, 0, len(mintable))
	for _, slug := range mintable {
		if _, wrote := stored[slug]; !wrote {
			continue
		}
		if raw, ok := terms[slug]; ok && raw != "" {
			commit = append(commit, raw)
			continue
		}
		commit = append(commit, slug)
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
