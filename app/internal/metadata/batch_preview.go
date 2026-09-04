// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// PREVIEW — the dry run (#1173, #1119, ADR 0019).
//
// Writes NOTHING. No field value, no vocabulary term, no audit event.
// It resolves a typed selection to an ordered distinct target set,
// partitions every target, and mints the single-use token the apply
// spends.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// PreviewBatchAssetFieldEdit computes a batch's partition and mints its
// token.
func (h *Handler) PreviewBatchAssetFieldEdit(
	ctx context.Context,
	req openapi.PreviewBatchAssetFieldEditRequestObject,
) (openapi.PreviewBatchAssetFieldEditResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil || id.IsAnonymous() {
		return openapi.PreviewBatchAssetFieldEdit401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return previewRefusal(refuse(400, openapi.BatchEmptySelection, "missing body")), nil
	}

	out, err := h.previewBatch(ctx, id, *req.Body)
	if err != nil {
		if r, ok := asBatchRefusal(err); ok {
			return previewRefusal(r), nil
		}
		return nil, err
	}
	return openapi.PreviewBatchAssetFieldEdit200JSONResponse(out), nil
}

func previewRefusal(r *batchRefusal) openapi.PreviewBatchAssetFieldEditResponseObject {
	body := r.body()
	switch r.Status {
	case 400:
		return openapi.PreviewBatchAssetFieldEdit400JSONResponse(body)
	case 403:
		return openapi.PreviewBatchAssetFieldEdit403JSONResponse(body)
	case 404:
		// The ordinary NotFound shape, not the batch refusal body: a
		// field that does not exist has no batch-specific reason to
		// give, and inventing one would put a code in the enum that no
		// client could act on differently from a 404.
		return openapi.PreviewBatchAssetFieldEdit404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: r.Message},
		}
	case 422:
		return openapi.PreviewBatchAssetFieldEdit422JSONResponse(body)
	default:
		// EVERY status this operation can produce is named above.
		// The default is a last resort, not a bucket: a 404 fell
		// into it once and came back as a 422 wearing a reason
		// that described a different situation entirely. A new
		// status needs an arm here, not a shrug.
		return openapi.PreviewBatchAssetFieldEdit422JSONResponse(body)
	}
}

// previewBatch is the whole dry run.
//
// THE ORDER IS THE CONTRACT. Batch-wide refusals that depend on nothing
// but the request and the field come first, so a caller who could never
// have succeeded is told so without a membership query being run on
// their behalf; then the selection is expanded; then each target is
// gated; and only below all five gates is any stored value looked at.
func (h *Handler) previewBatch(
	ctx context.Context,
	id *auth.Identity,
	body openapi.BatchAssetFieldPreviewRequest,
) (openapi.BatchAssetFieldPreview, error) {
	var zero openapi.BatchAssetFieldPreview
	q := New(h.Pool)

	// ── G1, batch-wide half — BEFORE EXPANSION ─────────────────────
	//
	// A caller with no holding of the instrument anywhere is refused
	// before a single membership query runs. Not an optimisation:
	// expanding a selection for somebody who cannot act on any of it
	// would answer questions about post membership on their behalf.
	if !bulkAdmitted(id) {
		return zero, refuse(403, openapi.BatchBulkCapabilityRequired,
			"batch metadata editing requires %s, globally or scoped to a team", CapBulkEdit)
	}

	pgField := pgtype.UUID{Bytes: uuid.UUID(body.FieldId), Valid: true}
	field, err := h.getFieldByIDCached(ctx, pgField)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A 404 with the ordinary NotFound body. Deliberately NOT a
			// batch refusal reason: `field_archived` says the field
			// exists and refuses values, which is a different fact and
			// a different fix.
			return zero, &batchRefusal{Status: 404, Message: "field not found"}
		}
		return zero, err
	}

	// ── G4 — the field's OWN write_capability, BATCH-WIDE ──────────
	//
	// Reproduced at its SHIPPED GLOBAL-ONLY scope. See
	// effectiveWritePermission for why the asymmetry with the field's
	// team-scope-aware READ gate is preserved here rather than fixed.
	//
	// Above the value inspection and above expansion's per-target work,
	// so a caller who lacks it inspects ZERO targets: a batch that
	// partitioned a thousand assets before refusing on a field-level
	// gate would have done a thousand reads it had no business doing.
	if !effectiveWritePermission(id, field) {
		return zero, refuse(403, openapi.BatchFieldWriteCapabilityRequired,
			"writing %s requires %s", field.Code, *field.WriteCapability).withField(field.Code)
	}

	if err := validateBatchMode(body.Mode); err != nil {
		return zero, err
	}

	// Configuration and mode first, and the VALUE only after: see
	// batchConfigurationRefusal for why "append on a text field" is
	// mode_not_supported_for_type rather than value_type_mismatch.
	if err := batchConfigurationRefusal(field, body.Mode); err != nil {
		return zero, err
	}
	value, err := buildBatchValue(field.Type, body.Value)
	if err != nil {
		return zero, err
	}
	if err := batchFieldRefusal(field, body.Mode, value); err != nil {
		return zero, err
	}

	// ── VOCABULARY, batch-wide half — NON-MUTATING ─────────────────
	vocab, err := resolveBatchVocabulary(field, body.Mode,
		vocabularySlugs(field.Type, value.Text, value.Options), canExtendVocabulary(id))
	if err != nil {
		return zero, err
	}
	value = applyCanonicalSlugs(field.Type, value, vocab)

	// The required check runs a SECOND time against the canonicalised
	// value, and it has to: `select` given "   " is refused as
	// required_value_empty ABOVE the vocabulary pass (R1 sits above the
	// write transaction on the shipped path), while an alias that
	// canonicalises to a real slug must be judged on what will be
	// STORED. Running it only once, on either side, gets one of the two
	// wrong.
	if body.Mode == modeOverwrite && field.Required && valueIsEmpty(field.Type, value.required()) {
		return zero, refuse(422, openapi.BatchRequiredValueEmpty,
			"%s is required, so it cannot be given an empty value. Write a value, or change the field's configuration if it should be optional",
			field.Code).withField(field.Code)
	}

	// ── REFERENCE liveness at preview ──────────────────────────────
	//
	// A target that does not resolve NOW is 422 dangling_reference and
	// the batch produces ZERO targets. Deliberately a DIFFERENT code
	// from the apply's reference_invalidated: this one says the
	// proposed target never resolved, that one says it resolved when
	// the operator looked and has since stopped, and they call for
	// different corrections.
	if field.Type == "reference" {
		live, err := referenceTargetLive(ctx, q, value.Ref)
		if err != nil {
			return zero, err
		}
		if !live {
			return zero, refuse(422, openapi.BatchDanglingReference,
				"%s: referenced asset %s does not exist", field.Code,
				uuid.UUID(value.Ref.Bytes)).withField(field.Code)
		}
	}

	// ── EXPANSION ──────────────────────────────────────────────────
	expansion, err := h.expandSelection(ctx, q, id, body.Selection)
	if err != nil {
		return zero, err
	}

	// ── PARTITION ──────────────────────────────────────────────────
	targets, counts, err := h.partitionTargets(ctx, q, id, field, body.Mode, value, vocab, expansion)
	if err != nil {
		return zero, err
	}
	if !counts.reconciles() {
		// Not a defensive nicety. The partition arithmetic is the
		// contract the operator's confirmation count is derived from,
		// and a server that has mis-partitioned a target must not hand
		// out a token binding the wrong set.
		return zero, fmt.Errorf(
			"metadata: batch partition does not reconcile: %+v", counts)
	}

	payload := batchTokenPayload{
		Mode:                  string(body.Mode),
		FieldID:               uuid.UUID(pgField.Bytes).String(),
		FieldCode:             field.Code,
		FieldType:             field.Type,
		DefinitionFingerprint: definitionFingerprint(field),
		VocabularyFingerprint: vocabularyFingerprint(field.Options),
		Value:                 tokenValueOf(value),
		Mintable:              vocab.Mintable,
		MintableTerms:         vocab.Terms,
		Targets:               targets,
		Counts:                counts,
		SelectionEntryCount:   expansion.EntryCount,
	}
	for _, p := range expansion.EmptyPosts {
		payload.EmptyPosts = append(payload.EmptyPosts, p.String())
	}

	now := time.Now()
	token, row, err := h.storeBatchPreview(ctx, q, id.UserRef, pgField, body.Mode, payload, now)
	if err != nil {
		return zero, err
	}

	// Opportunistic sweep, well past expiry, so the table stays bounded
	// without a scheduler. After the mint, and its failure is ignored:
	// housekeeping must never fail an operation that succeeded.
	if err := q.PurgeExpiredBatchPreviews(ctx); err != nil && h.Logger != nil {
		h.Logger.Warn("metadata.batch.purge_previews", "err", err)
	}

	return h.previewBody(token, row, field, body.Mode, value, vocab, payload), nil
}

// applyCanonicalSlugs folds the canonical slug set back into the value.
//
// The row stores the CANONICAL slug, never the text a client sent —
// skipping this is how "Sunset" ends up in value_options beside the
// `sunset` term it was supposed to become, and how a closed field's
// alias gets approved as `gb` and stored as `uk`.
func applyCanonicalSlugs(fieldType string, v batchValue, vocab batchVocabulary) batchValue {
	switch fieldType {
	case "multi_select":
		if len(vocab.Slugs) > 0 {
			v.Options = vocab.Slugs
		}
	case "select", "tree":
		// The single-slug types move too, now that an alias or a merge
		// tombstone redirects on a CLOSED vocabulary.
		if len(vocab.Slugs) == 1 {
			s := vocab.Slugs[0]
			v.Text = &s
		}
	}
	v.Mintable = vocab.Mintable
	return v
}

// partitionTargets runs the five gates and then the value inspection
// over every expanded target, in the deterministic order.
//
// TWO round trips for the whole batch, not two per target: one subject
// probe and one value read, both keyed on the ordered id list. At the
// thousand-target ceiling the difference between that and a loop of
// per-target queries is the entire latency budget.
func (h *Handler) partitionTargets(
	ctx context.Context,
	q *Queries,
	id *auth.Identity,
	field FieldDefinition,
	mode batchMode,
	value batchValue,
	vocab batchVocabulary,
	expansion batchExpansion,
) ([]batchTokenTarget, batchCounts, error) {
	var counts batchCounts
	out := make([]batchTokenTarget, 0, len(expansion.TargetIDs))

	if len(expansion.TargetIDs) == 0 {
		return out, counts, nil
	}

	ids := make([]pgtype.UUID, 0, len(expansion.TargetIDs))
	for _, t := range expansion.TargetIDs {
		ids = append(ids, pgtype.UUID{Bytes: t, Valid: true})
	}

	rows, err := q.ListBatchTargetSubjects(ctx, ids)
	if err != nil {
		return nil, counts, fmt.Errorf("metadata: load batch subjects: %w", err)
	}
	subjects := make(map[uuid.UUID]batchSubject, len(rows))
	for _, r := range rows {
		s := batchSubject{ID: uuid.UUID(r.ID.Bytes), AssetType: r.AssetType, Live: true, OwnerRef: r.OwnerUserRef}
		if r.TeamID.Valid {
			t := uuid.UUID(r.TeamID.Bytes)
			s.TeamID = &t
		}
		subjects[s.ID] = s
	}

	// The value read covers every target. It is filtered down to the
	// entitled ones in Go rather than in SQL because the entitlement
	// depends on the caller's closure-expanded scoped grants, which
	// live in the identity and not in the database's reach from here.
	// Nothing read for an unreadable target ever leaves this function.
	valueRows, err := q.ListBatchTargetValues(ctx, ListBatchTargetValuesParams{
		FieldID:  field.ID,
		AssetIds: ids,
	})
	if err != nil {
		return nil, counts, fmt.Errorf("metadata: load batch values: %w", err)
	}
	held := make(map[uuid.UUID]storedValue, len(valueRows))
	for _, r := range valueRows {
		held[uuid.UUID(r.AssetID.Bytes)] = storedValue{
			Present: true, Text: r.ValueText, Num: r.ValueNum, Date: r.ValueDate,
			Options: r.ValueOptions, Ref: r.ValueRef, SetAt: r.SetAt,
		}
	}

	for _, target := range expansion.TargetIDs {
		entry := batchTokenTarget{AssetID: target.String()}
		counts.Expanded++

		subject, live := subjects[target]
		if !live {
			// Soft-deleted or absent between the selection and this
			// read. Not a target the caller can act on and not a
			// target they should learn anything about, so it takes the
			// same answer an out-of-scope one does.
			counts.Unauthorized++
			entry.Partition = string(openapi.BatchPartitionUnauthorized)
			out = append(out, entry)
			continue
		}

		// G1 — bulk instrument IN THIS TARGET'S SCOPE.
		if !bulkScopeCovers(id, subject.TeamID) {
			counts.Unauthorized++
			entry.Partition = string(openapi.BatchPartitionUnauthorized)
			out = append(out, entry)
			continue
		}
		// G2 — the ORDINARY subject authority rule. The bulk
		// instrument does not replace it: holding a batch editor is
		// not authority over assets you could not edit one at a time.
		if !subjectAuthorised(id, subject) {
			counts.Unauthorized++
			entry.Partition = string(openapi.BatchPartitionUnauthorized)
			out = append(out, entry)
			continue
		}
		// G3 — applicability. Not an error: selecting a mixed bag and
		// editing a field that covers only some of it is ordinary.
		if !fieldApplies(field, subject.AssetType) {
			counts.Inapplicable++
			entry.Partition = string(openapi.BatchPartitionInapplicable)
			out = append(out, entry)
			continue
		}
		// G5 — the field's read_capability ON THIS SUBJECT, and THE
		// ANTI-ORACLE BOUNDARY. Below this line the stored value is
		// looked at; above it, nothing about the stored value —
		// emptiness, membership, equality, `set_at` — is observable.
		if !fieldReadableForBatch(id, field, subject.TeamID) {
			counts.Unreadable++
			entry.Partition = string(openapi.BatchPartitionUnreadable)
			out = append(out, entry)
			continue
		}

		// ── THE STORED-VALUE LINE ──────────────────────────────────
		stored := held[target]
		res := resolveTargetValue(field, mode, value, stored, vocab.Status)
		entry.Partition = string(res.Partition)
		switch res.Partition {
		case openapi.BatchPartitionWouldChange:
			counts.WouldChange++
			counts.Eligible++
			entry.Present = stored.Present
			if stored.Present && stored.SetAt.Valid {
				t := stored.SetAt.Time
				entry.SetAt = &t
			}
			entry.Delete = res.Delete
			if field.Type == "multi_select" && !res.Delete {
				entry.NextOptions = res.Next.Options
			}
		case openapi.BatchPartitionNoOp:
			counts.NoOp++
			counts.Eligible++
		case openapi.BatchPartitionRefused:
			counts.Refused++
			if res.Reason != nil {
				entry.Reason = string(*res.Reason)
			}
		}
		out = append(out, entry)
	}
	return out, counts, nil
}

// previewBody renders the wire response.
func (h *Handler) previewBody(
	token string,
	row InsertBatchPreviewRow,
	field FieldDefinition,
	mode batchMode,
	value batchValue,
	vocab batchVocabulary,
	payload batchTokenPayload,
) openapi.BatchAssetFieldPreview {
	out := openapi.BatchAssetFieldPreview{
		Token:               token,
		ExpiresAt:           row.ExpiresAt.Time,
		OperationId:         openapi_types.UUID(uuid.UUID(row.ID.Bytes)),
		Mode:                mode,
		FieldId:             openapi_types.UUID(uuid.UUID(field.ID.Bytes)),
		FieldCode:           field.Code,
		FieldType:           field.Type,
		ResolvedValue:       value.wire(),
		SelectionEntryCount: payload.SelectionEntryCount,
		Counts:              payload.Counts.wire(),
		Targets:             make([]openapi.BatchAssetFieldPreviewTarget, 0, len(payload.Targets)),
	}
	if len(vocab.Mintable) > 0 {
		terms := append([]string(nil), vocab.Mintable...)
		out.MintableTerms = &terms
	}
	if len(payload.EmptyPosts) > 0 {
		posts := make([]openapi_types.UUID, 0, len(payload.EmptyPosts))
		for _, p := range payload.EmptyPosts {
			if id, err := uuid.Parse(p); err == nil {
				posts = append(posts, openapi_types.UUID(id))
			}
		}
		out.EmptyPosts = &posts
	}
	for _, t := range payload.Targets {
		id, err := uuid.Parse(t.AssetID)
		if err != nil {
			continue
		}
		wire := openapi.BatchAssetFieldPreviewTarget{
			AssetId:   openapi_types.UUID(id),
			Partition: openapi.BatchAssetFieldPartition(t.Partition),
		}
		if t.Reason != "" {
			reason := openapi.BatchAssetFieldTargetRefusalReason(t.Reason)
			wire.RefusalReason = &reason
		}
		// `if_unchanged_since` rides ONLY on a would_change target, and
		// never on an unreadable one: a timestamp saying when a value
		// was last written is a fact about the value.
		if t.SetAt != nil && t.Partition == string(openapi.BatchPartitionWouldChange) {
			at := *t.SetAt
			wire.IfUnchangedSince = &at
		}
		out.Targets = append(out.Targets, wire)
	}
	return out
}

// batchRequest is the *http.Request behind a strict-server call, for
// the audit envelope's ip and user-agent. Absent on a non-HTTP caller,
// which the recorder handles by leaving both columns null.
func batchRequest(ctx context.Context) *http.Request {
	return auth.RequestFromContext(ctx)
}
