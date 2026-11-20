// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

// Replacing an asset's original file (#1596): POST /assets/{id}/versions.
//
// The new bytes arrive first through POST /storage/objects, which records
// the object (verified, #1597), pins it to the uploader (`user:<ref>`) and
// returns an upload receipt (#1595). This endpoint then makes them the
// asset's next version, in ONE transaction:
//
//  1. the asset row is locked (two replacements of one asset serialise);
//  2. the caller must still be allowed to edit it, the asset must be
//     local, `expected_version` must be its current version's number, and
//     the bytes must differ from the current ones and not belong to
//     another live asset of the same owner (the per-owner unique index);
//  3. the receipt is used up (issued to the caller, for exactly these
//     bytes, unexpired): a replacement that fails afterwards rolls back
//     and leaves it usable;
//  4. version n+1 is created, origin `replacement`, predecessor the
//     current version, with the receipt's base name as its provenance;
//     its insert trigger pins the new original to it (migration 00084);
//  5. the `asset:` pin moves from the old bytes to the new ones, object
//     row locked first (the order every pin writer uses). The old bytes
//     stay pinned by their own version, for the life of the asset;
//  6. the asset moves to the version: file, extension, size, thumbhash,
//     processing state;
//  7. the preview processing the new bytes need is queued in the same
//     transaction, so a committed version always has its work queued. A
//     file type with nothing to process is set ready, which requests the
//     version reconcile in the same statement (migration 00085), so it
//     too is sealed without anything left to chance after the commit;
//  8. so is the rest of the per-bytes work CreateAsset fans out: metadata
//     extraction, transcription and the visual embedding, each carrying
//     the new hash (the old picture's embedding is cleared here). The
//     text embedding (ai.embed) reads title and tags, not the file, so a
//     replacement leaves it alone;
//  9. the facts the old bytes' workers already wrote are retired with
//     them, so the asset never shows the old file's facts beside the new
//     file (see retireReplacedBytesFacts): page count and the preview
//     workers' metadata namespaces, extracted and measured field values
//     (each with a history entry, old value to null), the old file's
//     pending extraction failures (dismissed, kept as history) and its
//     AI subtitle tracks (pins released). A person's values and tracks
//     stay.
//
// THE FILE TYPE STAYS IN ITS CLASS. The caller names the new file's
// extension every time, normalized like the dispatch sets spell them,
// and it must classify the same as the current one (viewer kind and the
// asset type CreateAsset would promote to). Everything keyed on the class
// at create time (the asset_type promotion, the viewer, which pipelines
// exist for it) then stays true. The bytes themselves are not sniffed:
// the extension is the caller's claim here exactly as on POST /assets.
//
// A JOB FOR THE OLD BYTES CANNOT TOUCH THE NEW VERSION. Every write a
// preview, extraction, transcription or embedding job makes to the asset
// (status, thumbhash, dimensions, page count, metadata, field values,
// subtitle tracks, the visual vector) compares the hash the job was
// dispatched for in the same statement, under the asset row lock this
// transaction also takes. Renditions keyed by the old hash are still
// written, because those bytes are still a version of the asset.
//
// After the commit, best effort only: the uploader's `user:` pin is
// dropped (the version and asset pins hold the bytes now) and the caches
// other packages keep about this asset are invalidated, as UpdateAsset
// does. A failure there leaves a correct, fully queued version.
//
// Old bytes and their history are never touched: their fixity rows
// (#1597) and preservation events (#1598) are keyed by object hash, so
// the new bytes get their own and the old ones stay.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/provenance"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualstore"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/subtitles"
	"github.com/mscrnt/artist-alley/app/internal/viewkind"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// replaceHooks are #1596 test seams; zero in production.
type replaceHooks struct {
	beforeCommit func(ctx context.Context, tx pgx.Tx) error // after every write, before the commit
}

// CreateAssetVersion implements POST /assets/{id}/versions.
func (h *Handler) CreateAssetVersion(ctx context.Context, req openapi.CreateAssetVersionRequestObject) (openapi.CreateAssetVersionResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil || caller.IsAnonymous() {
		return openapi.CreateAssetVersion401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	assetID := uuid.UUID(req.Id)
	notFound := openapi.CreateAssetVersion404JSONResponse{NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"}}
	badRequest := func(msg string) openapi.CreateAssetVersionResponseObject {
		return openapi.CreateAssetVersion400JSONResponse{BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: msg}}
	}
	conflict := func(msg string) openapi.CreateAssetVersionResponseObject {
		return openapi.CreateAssetVersion409JSONResponse{ConflictJSONResponse: openapi.ConflictJSONResponse{Error: msg}}
	}

	// An asset the caller cannot see (the GetAsset row rule: a draft, a
	// private asset), or whose content it cannot read (the DownloadAssetFile
	// rule: a file is content), is 404, so this does not confirm a hidden
	// asset exists. A reader who may not edit gets 403 below.
	if seen, err := visibility.CanSee(ctx, h.Pool, visibility.EntityAsset, callerFromContext(ctx), assetID); err != nil || !seen {
		return notFound, nil
	}
	readCaller, caps := contentCaller(ctx)
	if ok, err := visibility.CanReadContent(ctx, h.Pool, readCaller, caps, assetID, visibility.MatureFromContext(ctx)); err != nil || !ok {
		return notFound, nil
	}
	if req.Body == nil {
		return badRequest("missing body"), nil
	}
	in := req.Body
	if err := storage.ValidateHash(in.FileHash); err != nil {
		return badRequest("file_hash must be a lowercase hex sha256"), nil
	}
	rid, perr := uuid.Parse(in.UploadReceipt)
	if perr != nil {
		return badRequest(errUploadReceiptInvalid.Error()), nil
	}
	if in.ExpectedVersion < 1 {
		return badRequest("expected_version must be at least 1"), nil
	}
	newExt, ok := normalizeReplacementExtension(in.FileExtension)
	if !ok {
		return badRequest("file_extension must be the new file's extension: 1 to 32 letters or digits"), nil
	}
	ext := &newExt

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("assets: replace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := New(tx)
	pgID := pgtype.UUID{Bytes: assetID, Valid: true}

	cur, err := q.LockAssetForReplace(ctx, pgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound, nil
	}
	if err != nil {
		return nil, fmt.Errorf("assets: replace: lock asset: %w", err)
	}
	if !canMutateAsset(caller, cur.OwnerUserRef, cur.TeamID) {
		return openapi.CreateAssetVersion403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "you may not edit this asset"},
		}, nil
	}
	if cur.OriginServerID.Valid {
		return conflict("a received asset is versioned by its origin"), nil
	}
	if !cur.CurrentVersionID.Valid || cur.FileHash == nil {
		return conflict("this asset has no file to replace"), nil
	}
	number, err := q.GetAssetVersionNumber(ctx, cur.CurrentVersionID)
	if err != nil {
		return nil, fmt.Errorf("assets: replace: current version: %w", err)
	}
	if in.ExpectedVersion != number {
		return conflict("the asset is at version " + strconv.FormatInt(number, 10) + ", not " + strconv.FormatInt(in.ExpectedVersion, 10)), nil
	}
	if in.FileHash == *cur.FileHash {
		return conflict("these are already the asset's current bytes"), nil
	}
	if cur.OwnerUserRef != nil {
		taken, err := q.OwnerHasLiveAssetWithHash(ctx, OwnerHasLiveAssetWithHashParams{OwnerUserRef: cur.OwnerUserRef, FileHash: in.FileHash, ID: pgID})
		if err != nil {
			return nil, fmt.Errorf("assets: replace: duplicate check: %w", err)
		}
		if taken {
			return conflict("the owner already has another asset with these bytes"), nil
		}
	}
	if !sameFileClass(cur.FileExtension, newExt) {
		return conflict("a replacement must be the same kind of file as the current one (" +
			fileClassLabel(cur.FileExtension) + "); upload a new asset for a " + fileClassLabel(ext) + " file"), nil
	}
	exists, err := storage.New(tx).ObjectRowExists(ctx, in.FileHash)
	if err != nil {
		return nil, fmt.Errorf("assets: replace: object lookup: %w", err)
	}
	if !exists {
		return badRequest("file_hash is not an uploaded object"), nil
	}
	rc, err := q.ConsumeUploadReceipt(ctx, ConsumeUploadReceiptParams{
		ID: pgtype.UUID{Bytes: rid, Valid: true}, UserRef: caller.UserRef, FileHash: in.FileHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return badRequest(errUploadReceiptInvalid.Error()), nil
	}
	if err != nil {
		return nil, fmt.Errorf("assets: replace: consume upload receipt: %w", err)
	}

	version := uuid.New()
	source := rc.OriginalBasenameSource
	if source == "" {
		source = string(provenance.SourceUnknown)
	}
	by := caller.UserRef
	if err := q.CreateReplacementVersion(ctx, CreateReplacementVersionParams{
		AssetID: pgID, VersionID: pgtype.UUID{Bytes: version, Valid: true}, Number: number + 1,
		PredecessorID: cur.CurrentVersionID, FileHash: in.FileHash, FileExtension: ext,
		OriginalBasename: rc.OriginalBasename, OriginalBasenameSource: source, CreatedBy: &by,
	}); err != nil {
		return nil, fmt.Errorf("assets: replace: create version: %w", err)
	}

	// The asset pin follows the current bytes: added to the new ones,
	// removed from the old (each object row locked before its pin row).
	sq := storage.New(tx)
	assetPin := assetID.String()
	if err := sq.ClearGCEligible(ctx, in.FileHash); err != nil {
		return nil, fmt.Errorf("assets: replace: pin new bytes: %w", err)
	}
	if err := sq.AddPin(ctx, storage.AddPinParams{ObjectHash: in.FileHash, PinSubjectType: PinSubjectTypeAsset, PinSubjectID: assetPin}); err != nil {
		return nil, fmt.Errorf("assets: replace: pin new bytes: %w", err)
	}
	if err := sq.LockObjectForPinChange(ctx, *cur.FileHash); err != nil {
		return nil, fmt.Errorf("assets: replace: unpin old bytes: %w", err)
	}
	if err := sq.RemovePin(ctx, storage.RemovePinParams{ObjectHash: *cur.FileHash, PinSubjectType: PinSubjectTypeAsset, PinSubjectID: assetPin}); err != nil {
		return nil, fmt.Errorf("assets: replace: unpin old bytes: %w", err)
	}
	if err := sq.MarkGCEligibleIfOrphaned(ctx, storage.MarkGCEligibleIfOrphanedParams{
		Hash: *cur.FileHash, Column2: pgtype.Interval{Microseconds: h.gcGrace().Microseconds(), Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("assets: replace: unpin old bytes: %w", err)
	}

	var thumbhash []byte
	if isImageExt(ext) {
		hCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		thumbhash = computeThumbhash(hCtx, h.Storage, in.FileHash, h.Logger)
		cancel()
	}
	steps := []dispatch.Step(nil)
	if needsProcessing(ext) && dispatch.CanPreview(ext) {
		steps = dispatch.PlanForExt(ext, jobs.PriorityHigh)
	}
	status := "ready"
	if len(steps) > 0 {
		status = "pending"
	}
	if err := q.MoveAssetToVersion(ctx, MoveAssetToVersionParams{
		FileHash: in.FileHash, FileExtension: ext, Thumbhash: thumbhash, ProcessingStatus: status,
		PreviewNamespaces: PreviewMetadataNamespaces,
		VersionID:         pgtype.UUID{Bytes: version, Valid: true}, ID: pgID,
	}); err != nil {
		if isPgUniqueViolation(err) {
			return conflict("the owner already has another asset with these bytes"), nil
		}
		return nil, fmt.Errorf("assets: replace: move asset: %w", err)
	}
	payload, err := json.Marshal(dispatch.NewPayload(assetID, in.FileHash, ext, false))
	if err != nil {
		return nil, err
	}
	jq := jobs.New(tx)
	for _, step := range steps {
		p := int32(step.Priority)
		if _, err := jq.EnqueueJob(ctx, jobs.EnqueueJobParams{Type: string(step.Type), Payload: payload, Priority: &p}); err != nil {
			return nil, fmt.Errorf("assets: replace: queue %s: %w", step.Type, err)
		}
	}
	if err := h.retireReplacedBytesFacts(ctx, tx, assetID, caller.UserRef); err != nil {
		return nil, err
	}
	if err := h.queueReplacementFollowUps(ctx, tx, assetID, in.FileHash, newExt, number+1); err != nil {
		return nil, err
	}
	if h.replaceHooks.beforeCommit != nil {
		if err := h.replaceHooks.beforeCommit(ctx, tx); err != nil {
			return nil, err
		}
	}
	view, err := q.GetAssetVersionView(ctx, pgtype.UUID{Bytes: version, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("assets: replace: read version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		if isPgUniqueViolation(err) {
			return conflict("the asset changed concurrently"), nil
		}
		return nil, fmt.Errorf("assets: replace: commit: %w", err)
	}

	// Best effort, after the commit: the version and asset pins hold the
	// bytes now, so the uploader's pin can go; and every other domain's
	// cached view of this asset is stale.
	if h.Storage != nil {
		if err := h.Storage.RemovePin(ctx, storage.PinRef{SubjectType: storage.PinSubjectTypeUser, SubjectID: strconv.FormatInt(caller.UserRef, 10)}, in.FileHash); err != nil && h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelDebug, "assets.replace.remove_user_pin.skipped", slog.String("err", err.Error()))
		}
	}
	h.invalidateDerivedCaches(ctx, assetID, "replace")
	// The retired AI tracks: this process at once, every other one by
	// NOTIFY, so neither GET /assets/{id} nor the track list serves the
	// old recording's words.
	if err := subtitles.InvalidateTracksOn(ctx, h.registry, assetID); err != nil && h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.replace.subtitle_cache.invalidate.error", slog.String("err", err.Error()))
	}
	return openapi.CreateAssetVersion201JSONResponse(versionToAPI(view)), nil
}

// The metadata namespaces the preview workers own on assets.metadata. Each
// worker writes only its own key (MergeAssetMetadata), and a replacement
// removes all of them with the bytes they describe; every other key stays.
const (
	MetadataNamespaceAudio   = "audio"
	MetadataNamespacePDF     = "pdf"
	MetadataNamespaceFont    = "font"
	MetadataNamespaceEbook   = "ebook"
	MetadataNamespaceComic   = "comic"
	MetadataNamespaceArchive = "archive"
	MetadataNamespaceText    = "text"
)

// PreviewMetadataNamespaces lists every key above.
var PreviewMetadataNamespaces = []string{
	MetadataNamespaceAudio, MetadataNamespacePDF, MetadataNamespaceFont, MetadataNamespaceEbook,
	MetadataNamespaceComic, MetadataNamespaceArchive, MetadataNamespaceText,
}

// replacedBytesExtractorProvenance is every extractor provenance that can
// be stored on asset_field_value.set_by: the CHECK admits exif, iptc and
// xmp, so the pdf and raw extractors' names, and the generic "extract",
// never reach a stored row.
var replacedBytesExtractorProvenance = []string{"exif", "iptc", "xmp"}

// replacementHistorySetBy is the asset_field_value_history.set_by of a
// value retired because the file it came from was replaced. The history
// column is free text; the caller who replaced the file is recorded as
// changed_by_user_ref.
const replacementHistorySetBy = "replacement"

// retireReplacedBytesFacts removes, on the replacement's transaction, the
// facts the old bytes' workers wrote that are not already reset by
// MoveAssetToVersion (page count and the preview metadata namespaces are):
//
//   - extracted field values (exif, iptc, xmp) and the two measured pixel
//     dimensions, each with a history entry old value -> null, so an
//     audit keeps what was there and why it went. A person's values, API
//     and import values, upload defaults and other computed values stay.
//     skip_if_set would otherwise treat an old extracted value as
//     occupied, and a field the new file lacks would never clear;
//   - the asset's pending extraction failures, soft-dismissed as the
//     re-extract path does: they describe the old file;
//   - its AI subtitle tracks, with their pins released (a person's tracks
//     stay). The track cache is dropped after the commit.
//
// The new bytes' jobs, queued next, write the facts again for the new
// file. The old version's own record (its fixity, provenance and
// predecessor link) is not touched.
func (h *Handler) retireReplacedBytesFacts(ctx context.Context, tx pgx.Tx, assetID uuid.UUID, by int64) error {
	pgID := pgtype.UUID{Bytes: assetID, Valid: true}
	rows, err := New(tx).RetireReplacedBytesFieldValues(ctx, RetireReplacedBytesFieldValuesParams{
		AssetID: pgID, ExtractedBy: replacedBytesExtractorProvenance,
		DimensionCodes: []string{pixeldims.Width, pixeldims.Height},
	})
	if err != nil {
		return fmt.Errorf("assets: replace: retire field values: %w", err)
	}
	mq := metadata.New(tx)
	for _, r := range rows {
		old, err := metadata.ValueRowJSON(r.ValueText, r.ValueNum, r.ValueDate, r.ValueOptions, r.ValueRef, r.FieldType)
		if err != nil {
			return fmt.Errorf("assets: replace: retired value history: %w", err)
		}
		if err := mq.AppendAssetFieldValueHistory(ctx, metadata.AppendAssetFieldValueHistoryParams{
			AssetID: pgID, FieldID: r.FieldID, OldValue: old, NewValue: nil,
			SetBy: replacementHistorySetBy, ChangedByUserRef: &by,
		}); err != nil {
			return fmt.Errorf("assets: replace: retired value history: %w", err)
		}
	}
	if _, err := New(tx).DismissExtractionFailuresForReplacedBytes(ctx, pgID); err != nil {
		return fmt.Errorf("assets: replace: dismiss old extraction failures: %w", err)
	}
	if _, err := subtitles.RetireAITracks(ctx, tx, assetID, h.gcGrace()); err != nil {
		return fmt.Errorf("assets: replace: %w", err)
	}
	return nil
}

// queueReplacementFollowUps queues, on the replacement's transaction, the
// per-bytes work CreateAsset fans out after an upload, for the new
// bytes (#1596). Durable, unlike CreateAsset's post-commit best effort:
// a version that commits has every one of these queued.
//
// Each payload names the new hash, so a job that runs after a further
// replacement finishes as a no-op, and no idempotency key can coalesce
// it with a job still running for older bytes: metadata.extract has
// none (as at create), and the other two are keyed per version.
func (h *Handler) queueReplacementFollowUps(ctx context.Context, tx pgx.Tx, assetID uuid.UUID, hash, ext string, version int64) error {
	jq := jobs.New(tx)
	enqueue := func(t jobs.JobType, priority *int32, key *string) error {
		b, err := json.Marshal(map[string]string{"asset_id": assetID.String(), "file_hash": hash})
		if err != nil {
			return err
		}
		if _, err := jq.EnqueueJob(ctx, jobs.EnqueueJobParams{Type: string(t), Payload: b, Priority: priority, IdempotencyKey: key}); err != nil {
			return fmt.Errorf("assets: replace: queue %s: %w", t, err)
		}
		return nil
	}

	// The old picture's vector would keep the new one findable by the
	// old one until the new embedding lands (or forever, with the
	// sidecar off). Cleared under the asset lock this transaction holds;
	// an old-bytes job then writes nothing (see UpsertAssetVisualEmbedding).
	if err := visualstore.New(tx).DeleteAssetVisualEmbedding(ctx, pgtype.UUID{Bytes: assetID, Valid: true}); err != nil {
		return fmt.Errorf("assets: replace: clear visual embedding: %w", err)
	}
	if h.visualEmbedDispatcher != nil {
		if err := h.visualEmbedDispatcher.Dispatch(ctx, VisualEmbedInput{
			AssetID: assetID, FileExtension: ext, FileHash: hash, Version: version, DB: tx,
		}); err != nil {
			return fmt.Errorf("assets: replace: queue visual embedding: %w", err)
		}
	}
	if isExifExtractableImageExt(&ext) {
		if err := enqueue(metadataExtractJobType, nil, nil); err != nil {
			return err
		}
	}
	_, isVideo := dispatch.VideoExts[ext]
	_, isAudio := dispatch.AudioExts[ext]
	if isVideo || isAudio {
		low := int32(jobs.PriorityLow)
		key := aiTranscribeIdempotencyKey(assetID.String()+"|v"+strconv.FormatInt(version, 10), "")
		if err := enqueue(aiTranscribeJobType, &low, &key); err != nil {
			return err
		}
	}
	return nil
}

// normalizeReplacementExtension spells a replacement's extension the
// way the dispatch sets and viewkind do (lowercase, no leading dot) and
// accepts only 1 to 32 ASCII letters or digits, the shape every
// extension those sets route has.
func normalizeReplacementExtension(raw string) (string, bool) {
	e := viewkind.Normalize(raw)
	if e == "" || len(e) > 32 {
		return "", false
	}
	for _, r := range e {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return "", false
		}
	}
	return e, true
}

// sameFileClass reports whether a replacement extension classifies like
// the current one: the same viewer kind and the same asset type
// CreateAsset would promote the upload to. An asset with no extension
// is the unrecognised class, and stays in it.
func sameFileClass(current *string, next string) bool {
	cur := ""
	if current != nil {
		cur = *current
	}
	return viewkind.KindForExtension(cur) == viewkind.KindForExtension(next) &&
		assetTypeFor(cur) == assetTypeFor(next)
}

// fileClassLabel names an extension's class for a refusal message.
func fileClassLabel(ext *string) string {
	if ext == nil || *ext == "" {
		return "an unrecognised type"
	}
	return string(viewkind.KindForExtension(*ext)) + " (." + viewkind.Normalize(*ext) + ")"
}

func versionToAPI(v GetAssetVersionViewRow) openapi.AssetVersion {
	out := openapi.AssetVersion{
		Id: openapi_types.UUID(v.ID.Bytes), Number: v.Number, Origin: openapi.AssetVersionOrigin(v.Origin),
		CreatedAt: v.CreatedAt.Time, OriginalHash: v.OriginalHash, OriginalSizeBytes: v.OriginalSizeBytes,
		OriginalMediaType: v.OriginalMediaType, OriginalExtension: v.OriginalExtension,
	}
	if v.PredecessorID.Valid {
		p := openapi_types.UUID(v.PredecessorID.Bytes)
		out.PredecessorId = &p
	}
	return out
}

func (h *Handler) gcGrace() time.Duration {
	if h.Storage != nil && h.Storage.GCGracePeriod > 0 {
		return h.Storage.GCGracePeriod
	}
	return storage.DefaultGCGracePeriod
}
