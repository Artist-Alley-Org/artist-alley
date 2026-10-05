// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// Alias-then-merge (ADR 0092 §4, #789)
// ---------------------------------------------------------------------------
//
// A vocabulary anyone can extend drifts: "concept art", "concept-art",
// "ConceptArt". #789's research pass over the highest-volume production
// normalisation systems found them converging on one three-layer answer,
// and this file is the third layer. The other two are elsewhere and this
// only works because they are:
//
//  1. ALIAS — a write-time redirect. An operator adds `conceptart` as an
//     alias of `concept-art`; new writes land on the canonical term and
//     nothing stored is touched. Cheap, reversible, and where a
//     normalisation should start. Lives in options.go (FieldOption.Aliases)
//     and open_vocabulary.go (indexVocabulary's second pass).
//
//  2. TOMBSTONE — a merged-away slug is never deleted and never reused.
//     It keeps a permanent `replaced_by`, and it keeps RESOLVING, so a
//     value naming it arrives at the successor rather than being refused.
//     indexVocabulary's third pass.
//
//  3. MERGE — this file. Rewrites the stored rows.
//
// # Why the tombstone is the load-bearing part
//
// ADR 0092 rejects deleting on merge, and the reason is federation: a
// peer that saw `uk` before the merge holds a value naming it. If the
// row is gone, that peer's value resolves to nothing and there is no
// way — ever — to tell "this term was merged into another" from "this
// term never existed". A tombstone is what turns a destructive edit into
// a recoverable one.
//
// # Why the friction
//
// Every surveyed system guards the same failure: rewriting rows before
// the redirect has proven correct. Ours are three, and none of them is
// decoration:
//
//   - `fields.vocabulary.merge`, which `fields.admin` does NOT imply.
//     Curating an option list writes one row; this writes thousands
//     across two tables, on records belonging to other people.
//   - a required `reason`, recorded on the audit event.
//   - `dry_run`, which runs the real statements and rolls them back, so
//     the preview cannot disagree with the thing it previews.

// mergeReasonMinLen mirrors the spec's minLength. Enforced here as well
// because the generated server does not validate string lengths, and a
// requirement only the documentation enforces is not one.
const mergeReasonMinLen = 3

// MergeFieldValues folds one vocabulary term into another.
func (h *Handler) MergeFieldValues(
	ctx context.Context,
	req openapi.MergeFieldValuesRequestObject,
) (openapi.MergeFieldValuesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.MergeFieldValues401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapVocabularyMerge) {
		return openapi.MergeFieldValues403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "missing capability: " + CapVocabularyMerge,
			},
		}, nil
	}
	if req.Body == nil {
		return openapi.MergeFieldValues400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "body required"},
		}, nil
	}

	source := strings.TrimSpace(req.Body.Source)
	target := strings.TrimSpace(req.Body.Target)
	reason := strings.TrimSpace(req.Body.Reason)
	dryRun := req.Body.DryRun != nil && *req.Body.DryRun

	switch {
	case source == "" || target == "":
		return openapi.MergeFieldValues400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "source and target are required"},
		}, nil
	case source == target:
		return openapi.MergeFieldValues400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "source and target must differ"},
		}, nil
	case len(reason) < mergeReasonMinLen:
		return openapi.MergeFieldValues400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: fmt.Sprintf("reason must be at least %d characters — a merge without a recorded reason is not auditable", mergeReasonMinLen),
			},
		}, nil
	}

	pgField := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}

	// Read once, outside the transaction, for the field's CODE — the
	// only thing a refusal body needs that the locked row does not
	// carry, and the thing a human reads first ("keywords: …"). Also
	// answers 404 before any lock is taken.
	field, err := h.getFieldByIDCached(ctx, pgField)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.MergeFieldValues404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, fmt.Errorf("metadata: merge: load field: %w", err)
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("metadata: merge: begin tx: %w", err)
	}
	// Rollback on every path that does not explicitly commit — which
	// includes the dry run, deliberately. The preview runs the SAME
	// statements the real merge does and then throws the work away, so
	// the counts it reports are measured rather than estimated.
	defer func() { _ = tx.Rollback(ctx) }()
	qTx := New(tx)

	// The row lock is the same one minting takes, for the same reason:
	// the merge rewrites the whole options document, and a term created
	// by a concurrent value save between our read and our write would be
	// discarded. Taking it first also serialises two merges on one field.
	locked, err := qTx.LockFieldDefinitionVocabulary(ctx, pgField)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.MergeFieldValues404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "field not found"},
			}, nil
		}
		return nil, fmt.Errorf("metadata: merge: lock vocabulary: %w", err)
	}

	values, rest, err := decodeOptionValues(locked.Options)
	if err != nil && !errors.Is(err, errNoValues) {
		return nil, fmt.Errorf("metadata: merge: read vocabulary: %w", err)
	}
	if rest == nil {
		rest = map[string]json.RawMessage{}
	}

	if rej := applyMergeToOptions(values, source, target); rej != nil {
		return openapi.MergeFieldValues422JSONResponse(rejectionBody(field.Code, rej)), nil
	}

	var actor *int64
	if id.UserRef != 0 {
		ref := id.UserRef
		actor = &ref
	}

	assetRows, err := qTx.RewriteAssetValuesForMergedOption(ctx, RewriteAssetValuesForMergedOptionParams{
		FieldID:      pgField,
		Source:       source,
		Target:       target,
		FieldType:    locked.Type,
		ActorUserRef: actor,
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: merge: rewrite asset values: %w", err)
	}
	collectionRows, err := qTx.RewriteCollectionValuesForMergedOption(ctx, RewriteCollectionValuesForMergedOptionParams{
		FieldID:      pgField,
		Source:       source,
		Target:       target,
		FieldType:    locked.Type,
		ActorUserRef: actor,
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: merge: rewrite collection values: %w", err)
	}

	// The options document is rewritten LAST among the writes, so a
	// failure rewriting values leaves the vocabulary untouched rather
	// than tombstoning a term whose values still name it. Inside one
	// transaction either order is atomic; the ordering is for the
	// person reading a partial log after an error.
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("metadata: merge: encode vocabulary: %w", err)
	}
	rest["values"] = encoded
	doc, err := json.Marshal(rest)
	if err != nil {
		return nil, fmt.Errorf("metadata: merge: encode vocabulary: %w", err)
	}
	// Through the admin editor's own normaliser, so a document this path
	// produced is valid by the rule that validates one an operator
	// edited — tree-wide slug uniqueness, replaced_by pointing at a real
	// term, alias keys unambiguous.
	doc, err = NormalizeOptionsDoc(doc)
	if err != nil {
		return nil, fmt.Errorf("metadata: merge: normalise vocabulary: %w", err)
	}
	if err := qTx.SetFieldDefinitionOptions(ctx, SetFieldDefinitionOptionsParams{
		ID:      pgField,
		Options: doc,
	}); err != nil {
		return nil, fmt.Errorf("metadata: merge: write vocabulary: %w", err)
	}

	result := openapi.VocabularyMergeResult{
		Source:                    source,
		Target:                    target,
		DryRun:                    dryRun,
		AssetValuesRewritten:      int(assetRows),
		CollectionValuesRewritten: int(collectionRows),
		Tombstoned:                !dryRun,
	}
	if dryRun {
		// Nothing committed. The deferred Rollback does the work; saying
		// so here is for the reader, since "the function returns without
		// committing" is exactly the shape a bug takes.
		return openapi.MergeFieldValues200JSONResponse(result), nil
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("metadata: merge: commit: %w", err)
	}

	// After the commit: a cache dropped before the write lands
	// repopulates with the pre-write document.
	h.InvalidateFieldVocabulary(ctx, pgField)

	if h.Audit != nil {
		h.Audit.VocabularyMerged(ctx, actor, audit.VocabularyMerge{
			FieldID:                   uuid.UUID(req.Id).String(),
			FieldCode:                 field.Code,
			Source:                    source,
			Target:                    target,
			Reason:                    reason,
			AssetValuesRewritten:      int(assetRows),
			CollectionValuesRewritten: int(collectionRows),
		})
	}
	if h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelInfo, "metadata.vocabulary.merged",
			slog.String("field", field.Code),
			slog.String("source", source),
			slog.String("target", target),
			slog.Int64("asset_values", assetRows),
			slog.Int64("collection_values", collectionRows),
		)
	}
	return openapi.MergeFieldValues200JSONResponse(result), nil
}

// applyMergeToOptions turns `source` into a tombstone pointing at
// `target`, in place, and reports why it may not.
//
// Both terms must exist: a merge is a statement about two things the
// field HAS, and inventing either would let a typo silently create the
// term it was supposed to fix.
//
// The target may not be archived. Merging into a retired term moves
// every value onto something no picker offers and no new value may
// choose — the opposite of what a merge is for — and it would also
// build the tombstone chain that resolveTombstone has to walk.
//
// A source that is ALREADY a tombstone is refused rather than
// re-pointed. Re-aiming an existing forwarding address rewrites history
// a peer may already have resolved through; the operator merges the
// current target onward instead, which composes and leaves both hops
// visible.
func applyMergeToOptions(values []FieldOption, source, target string) *slugRejection {
	var src, dst *FieldOption
	walkOptionsPtr(values, func(o *FieldOption) {
		switch strings.TrimSpace(o.Value) {
		case source:
			src = o
		case target:
			dst = o
		}
	})
	if src == nil {
		return &slugRejection{Slug: source}
	}
	if dst == nil {
		return &slugRejection{Slug: target}
	}
	if dst.Status == OptionArchived {
		return &slugRejection{Slug: target, Status: OptionArchived}
	}
	if src.Status == OptionArchived && src.ReplacedBy != "" {
		return &slugRejection{Slug: source, Status: OptionArchived}
	}

	src.Status = OptionArchived
	src.ReplacedBy = strings.TrimSpace(dst.Value)
	// The source's own aliases go with it. An operator who taught the
	// vocabulary that "u.k." means `uk` meant the SPELLING, not the row,
	// and dropping them here would quietly un-normalise every future
	// write that used one. They keep working because the tombstone
	// forwards them (indexVocabulary's third pass) — moving them onto
	// the target instead would be the same behaviour with the provenance
	// lost.
	return nil
}

// walkOptionsPtr is walkOptions for a caller that needs to MUTATE the
// options it visits. Separate rather than a flag on walkOptions because
// the read-only walk hands out copies deliberately — half a dozen
// callers rely on being able to fill in a default label without
// touching the document — and a version that silently aliased the
// backing array would be a very quiet way to corrupt one.
func walkOptionsPtr(opts []FieldOption, visit func(*FieldOption)) {
	for i := range opts {
		visit(&opts[i])
		walkOptionsPtr(opts[i].Children, visit)
	}
}
