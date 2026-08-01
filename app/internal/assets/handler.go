// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package assets implements the asset-entity slice of the
// artist-alley HTTP API. An asset is the user-facing record on top
// of the byte-plane managed by the storage package.
//
// See ADR 0011 for the entity model. The storage byte upload /
// download endpoints (/storage/objects/*) live in
// internal/storage/handler.go.
package assets

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	// Register image decoders for thumbhash. We use the std lib's
	// JPEG/PNG/GIF decoders plus golang.org/x/image/webp/bmp/tiff so
	// the same code path covers everything our isImageExt allows.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"go.n16f.net/thumbhash"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/mscrnt/artist-alley/app/internal/asset/imagefmt"
	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// softDeleteReasonMaxLen bounds the operator-supplied reason string
// so operators can't accidentally paste MB of prose into an audit
// row. The DB column is TEXT with no length constraint; the cap
// lives at the handler for a clean 400 rather than a silent bloat.
const softDeleteReasonMaxLen = 500

// extractSoftDeleteReason pulls the reason from an optional
// SoftDeleteRequest body. Empty body / empty reason both map to "".
func extractSoftDeleteReason(body *openapi.SoftDeleteRequest) string {
	if body == nil || body.Reason == nil {
		return ""
	}
	return strings.TrimSpace(*body.Reason)
}

// softDeleteReasonPtr returns nil for empty strings, else a pointer
// to the value. Matches the sqlc-generated *string param type on
// soft-delete UPDATE queries so an empty reason writes NULL rather
// than "".
func softDeleteReasonPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// PinSubjectTypeAsset is the storage-pin subject_type assets use to
// claim their underlying bytes. Replaces the `user:` pin set by the
// initial upload.
const PinSubjectTypeAsset = "asset"

// maxListLimit caps the per-page row count regardless of what the
// caller requests. Higher than the openapi spec's default but in
// line with its declared maximum (200).
const maxListLimit = 200

// CacheDomainAssetCompanions keys the per-asset companion-list cache.
// Exposed so cross-package writers (e.g. a future bulk-edit endpoint
// that mutates companions outside this handler) can broadcast the
// invalidation through cache.Registry.Emit.
const CacheDomainAssetCompanions = "asset.companions"

// CacheDomainAssetAlternates keys the per-asset alternate-list cache.
// Mirrors the companions domain — sprite-tool palette swaps, future
// painting-track variants, and the thumbnail pipeline all mutate the
// alternate list and need to broadcast invalidations through this key.
const CacheDomainAssetAlternates = "asset.alternates"

// CacheDomainEPUBSpine + CacheDomainEPUBChapter — EPUB reader caches.
// Spine is small (chapter index per asset); chapter HTML is bigger
// (post-rewrite XHTML body). Both are invalidated only on asset re-
// upload (which generates a new asset id, so cold-key churn handles
// itself naturally).
const CacheDomainEPUBSpine = "asset.epub.spine"
const CacheDomainEPUBChapter = "asset.epub.chapter"

// Handler implements the asset-entity slice of
// openapi.StrictServerInterface.
type Handler struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
	Logger  *slog.Logger
	// Jobs is the background-job service. Used to enqueue
	// preview-generation jobs (and, later, EXIF + AI + checksum work)
	// after a successful asset create. Nil-safe — tests may pass nil
	// to skip the enqueue.
	Jobs *jobs.Service

	// SysConfig powers operator-set knobs the handler reads on the
	// hot path (currently: upload.dedup_scope + .dedup_behavior
	// from Phase 1.18.A-2 follow-up A). Nil-safe — tests can
	// construct without; absent config falls back to documented
	// defaults.
	SysConfig *sysconfig.Store

	// previewLadder reports the operator's CONFIGURED preview variant
	// keys, cached (#591). nil-safe: nil yields no ladder, so
	// ladder_available is false and clients stay on the `col` rung.
	previewLadder sysconfig.PreviewLadderReader

	// Audit records admin lifecycle events (soft_deleted currently;
	// restored fires from the softdelete.Service directly). Nil-safe.
	Audit *audit.Recorder

	// SoftDelete handles restore (clear deleted_at + audit) via the
	// shared softdelete.Service. Nil at construction time in tests;
	// wired at boot in api.go alongside the gc coordinator.
	SoftDelete *softdelete.Service

	// companions caches the per-asset list of sidecar files (the
	// model viewer fetches this on every 3D mount; cache hit rate is
	// high once a session settles on a working set of assets).
	// nil-safe — tests can pass a nil registry.
	companions *cache.Cache[[]openapi.AssetCompanion]
	// alternates caches the per-asset list of sibling variants.
	// Mostly hit by the sprite-tool palette swap UI + the future
	// authored-variant track.
	alternates *cache.Cache[[]openapi.AssetAlternate]
	// EPUB reader caches — spine list per asset, rendered chapter
	// HTML per (assetId, idx). Sized assuming a typical browsing
	// session hits a handful of books deep but only reads through
	// a few hundred chapters total.
	epubSpine    *cache.Cache[[]openapi.EpubSpineEntry]
	epubChapters *cache.Cache[[]byte]

	// similarReader is the embeddings-side seam for the
	// /assets/{id}/similar endpoint. Injected post-construction via
	// SetSimilarReader to avoid pulling ai/embeddings into this
	// package's import graph. Nil-safe — the endpoint returns 503-
	// like "embedding subsystem not wired" when nil (only happens
	// in tests that don't bother wiring it up).
	similarReader SimilarReader

	// visualEmbedDispatcher is the CLIP-visual-embed fanout seam for
	// image uploads (Phase 1.16.B-3-followup-2). Same shape as
	// similarReader: consumer-defined narrow interface + setter
	// injection to keep the visualembed package out of this file's
	// import graph. Nil = feature disabled or sidecar unregistered;
	// silent skip in CreateAsset.
	visualEmbedDispatcher VisualEmbedDispatcher
}

// SimilarReader is the narrow surface this package needs from the
// embeddings reader. *embeddings.Reader satisfies it. The interface
// lives here (consumer-defined) so the assets package doesn't import
// the embeddings package.
type SimilarReader interface {
	HasEmbedding(ctx context.Context, anchorID uuid.UUID, provider, model, modality string) (bool, error)
	FindSimilarByAnchor(ctx context.Context, anchorID uuid.UUID, provider, model, modality string, limit int) ([]SimilarNeighbour, error)
}

// SimilarNeighbour mirrors embeddings.Neighbour as a local type so
// the SimilarReader interface doesn't drag the embeddings package
// into this one's import graph. The adapter at the boot wire
// converts between the two.
type SimilarNeighbour struct {
	AssetID  uuid.UUID
	Distance float64
}

// SetSimilarReader injects the embeddings-side reader for the
// /assets/{id}/similar endpoint. Boot wire is the only caller.
func (h *Handler) SetSimilarReader(r SimilarReader) { h.similarReader = r }

// VisualEmbedDispatcher is the narrow surface this package needs from
// the visualembed package. *visualembed.Dispatcher satisfies it. The
// interface lives here (consumer-defined) so the assets package
// doesn't import the visualembed package.
type VisualEmbedDispatcher interface {
	Dispatch(ctx context.Context, in VisualEmbedInput)
}

// VisualEmbedInput mirrors visualembed.DispatchInput as a local type
// so the interface surface doesn't drag visualembed's public types
// into this file. The boot-time adapter converts between the two.
type VisualEmbedInput struct {
	AssetID       uuid.UUID
	FileExtension string
}

// SetVisualEmbedDispatcher injects the visualembed dispatcher for the
// CreateAsset fanout. Boot wire is the only caller.
func (h *Handler) SetVisualEmbedDispatcher(d VisualEmbedDispatcher) { h.visualEmbedDispatcher = d }

// NewHandler binds an entity handler to the DB pool and the storage
// Service it shares with the storage byte handler.
// SetPreviewLadder installs the cached configured-ladder reader (#591).
func (h *Handler) SetPreviewLadder(r sysconfig.PreviewLadderReader) { h.previewLadder = r }

// ladder returns the configured preview variant keys, or nil when the
// reader is not wired. nil is the conservative answer: it makes
// ladder_available false, so a client falls back to the single `col`
// rung rather than requesting rungs whose existence nobody vouched for.
func (h *Handler) ladder(ctx context.Context) []string {
	if h.previewLadder == nil {
		return nil
	}
	return h.previewLadder(ctx)
}

func NewHandler(pool *pgxpool.Pool, storageSvc *storage.Service, logger *slog.Logger, jobSvc *jobs.Service, registry *cache.Registry, sysCfg *sysconfig.Store) *Handler {
	h := &Handler{Pool: pool, Storage: storageSvc, Logger: logger, Jobs: jobSvc, SysConfig: sysCfg}
	if registry != nil {
		// 5_000 keys × ~512 bytes/entry ≈ 2.5MB resident. Working set
		// is "active assets being reviewed" which is well under that
		// for a single host.
		h.companions = cache.Register[[]openapi.AssetCompanion](registry, CacheDomainAssetCompanions, 5_000)
		h.alternates = cache.Register[[]openapi.AssetAlternate](registry, CacheDomainAssetAlternates, 5_000)
		h.epubSpine = cache.Register[[]openapi.EpubSpineEntry](registry, CacheDomainEPUBSpine, 5_000)
		h.epubChapters = cache.Register[[]byte](registry, CacheDomainEPUBChapter, 2_000)
	}
	return h
}

// ---------------------------------------------------------------------------
// CreateAsset
// ---------------------------------------------------------------------------

func (h *Handler) CreateAsset(
	ctx context.Context,
	req openapi.CreateAssetRequestObject,
) (openapi.CreateAssetResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.CreateAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreateAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	in := req.Body

	title := strings.TrimSpace(in.Title)
	if title == "" {
		return openapi.CreateAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "title is required"},
		}, nil
	}

	status := "active"
	if in.Status != nil {
		status = string(*in.Status)
	}
	switch status {
	case "draft", "active", "archived":
	default:
		return openapi.CreateAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "status must be one of draft|active|archived"},
		}, nil
	}

	metadataJSON, err := encodeMetadata(in.Metadata)
	if err != nil {
		return nil, err
	}

	var fileHashPtr *string
	if in.FileHash != nil && *in.FileHash != "" {
		fh := strings.ToLower(strings.TrimSpace(*in.FileHash))
		if err := storage.ValidateHash(fh); err != nil {
			return openapi.CreateAsset400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "file_hash: " + err.Error()},
			}, nil
		}
		fileHashPtr = &fh
	}

	// Workflow state: caller-supplied UUID is taken verbatim; the DB
	// FK guards against unknown values. We don't validate that the
	// state belongs to the matching `asset:<asset_type>` domain
	// here — that check kicks in on Transition() later. Tradeoff:
	// permits a typo in the upload modal that would only surface on
	// the first transition attempt; not worth a domain-lookup
	// round-trip on every create.
	var stateID pgtype.UUID
	if in.StateId != nil {
		stateID = pgtype.UUID{Bytes: uuid.UUID(*in.StateId), Valid: true}
	}

	// processing_status: image / video uploads queue for async
	// post-processing (variant generation, EXIF, etc.); everything
	// else is `ready` on day one because there's nothing to process.
	processingStatus := "ready"
	if needsProcessing(in.FileExtension) {
		processingStatus = "pending"
	}

	// Auto-promote asset_type when the file extension implies a
	// stronger category than the caller chose. Frontends that send a
	// generic Photo (1) for a .glb get bumped to 3D Object (5); a
	// caller who explicitly picked Audio (4) for a model would stay
	// as-is and live with their choice. The override rule is "promote
	// only from the generic defaults (1, 2)" so explicit picks are
	// always respected.
	if in.FileExtension != nil {
		if want := assetTypeFor(*in.FileExtension); want > 0 {
			if in.AssetType == 1 || in.AssetType == 2 {
				in.AssetType = want
			}
		}
	}

	// Thumbhash — synchronously computed for image extensions. ~1-3
	// ms on a 4K image; we do it BEFORE the INSERT so the asset row
	// is born with the placeholder. Failure here is soft: log + keep
	// thumbhash=NULL. The feed card just won't have a blurred
	// placeholder; the original /file URL still works.
	var thumbhashBytes []byte
	if fileHashPtr != nil && isImageExt(in.FileExtension) {
		hCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		thumbhashBytes = computeThumbhash(hCtx, h.Storage, *fileHashPtr, h.Logger)
		cancel()
	}

	// Phase 1.18.A-2 follow-up A — per-user dedup pre-check. Per
	// the operator-configured dedup_scope + dedup_behavior:
	//
	//   - scope=per_user (default): lookup via partial unique
	//     index on (owner_user_ref, file_hash). On hit, the
	//     behavior decides what to do.
	//   - scope=off or fileHashPtr=nil: skip the check entirely.
	//   - scope=per_team / global: application-level check NOT
	//     yet implemented (TODO follow-up). Falls through to the
	//     per-user partial index for now (which still fires the
	//     constraint for the same user uploading twice).
	//
	// The DB-side partial unique index from migration 00016
	// provides the load-bearing concurrency guarantee — even if
	// the pre-check passes, two concurrent uploads of the same
	// file by the same user can still race; one wins the unique
	// constraint, the other catches 23505 + re-runs the
	// dedup-response path below.
	var uploadCfg sysconfig.UploadConfig
	if h.SysConfig != nil {
		uploadCfg, _ = h.SysConfig.GetUpload(ctx)
	} else {
		uploadCfg = sysconfig.UploadConfig{DedupScope: sysconfig.DedupScopePerUser, DedupBehavior: sysconfig.DedupBehaviorWarn}
	}
	if fileHashPtr != nil && uploadCfg.DedupScope != sysconfig.DedupScopeOff && uploadCfg.DedupBehavior != sysconfig.DedupBehaviorAllow {
		existing, err := New(h.Pool).GetAssetByOwnerHash(ctx, GetAssetByOwnerHashParams{
			OwnerUserRef: &id.UserRef,
			FileHash:     fileHashPtr,
		})
		if err == nil {
			// Pre-check hit. Behavior gates the response shape.
			return h.dedupResponse(ctx, uploadCfg.DedupBehavior, existing)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("assets: dedup lookup: %w", err)
		}
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("assets: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	row, err := q.CreateAsset(ctx, CreateAssetParams{
		Title:            title,
		Description:      strDefault(in.Description, ""),
		AssetType:        in.AssetType,
		OwnerUserRef:     &id.UserRef,
		Status:           status,
		FileHash:         fileHashPtr,
		FileExtension:    in.FileExtension,
		FileSizeBytes:    nil, // backfilled below from storage_objects if hash present
		Metadata:         metadataJSON,
		OriginServerID:   pgtype.UUID{Valid: false},
		StateID:          stateID,
		ProcessingStatus: processingStatus,
		Thumbhash:        thumbhashBytes,
	})
	if err != nil {
		// Race-loser: a concurrent upload won the unique
		// constraint between our pre-check + this INSERT. Re-
		// fetch + return the same dedup-response the pre-check
		// path would have. Classified via SQLSTATE 23505 (same
		// pattern as userkeys.backfill #155).
		if isPgUniqueViolation(err) && fileHashPtr != nil {
			existing, fetchErr := New(h.Pool).GetAssetByOwnerHash(ctx, GetAssetByOwnerHashParams{
				OwnerUserRef: &id.UserRef,
				FileHash:     fileHashPtr,
			})
			if fetchErr == nil {
				return h.dedupResponse(ctx, uploadCfg.DedupBehavior, existing)
			}
		}
		return nil, fmt.Errorf("assets: insert: %w", err)
	}
	newID := uuid.UUID(row.ID.Bytes)

	// Tags (optional). Replace, not merge — at creation time there's
	// nothing to merge with.
	tags := []string{}
	if in.Tags != nil {
		seen := map[string]struct{}{}
		for _, t := range *in.Tags {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			if err := q.AddAssetTag(ctx, AddAssetTagParams{
				AssetID: row.ID,
				Tag:     t,
			}); err != nil {
				return nil, fmt.Errorf("assets: add tag: %w", err)
			}
			tags = append(tags, t)
		}
	}

	// Upload defaults (#793, ADR 0081 §3). Inside the tx, so an asset
	// is never observable without the values its field definitions say
	// it should be born with.
	//
	// This is the artist-friction wedge: every default that lands here
	// is a decision nobody had to make at upload time. The values are
	// written with set_by='default', which is what lets the async
	// extraction job overwrite them later without also overwriting
	// anything a person chose.
	//
	// A failure is logged and swallowed rather than rolled back. A
	// default is a convenience; refusing an upload because one field
	// definition carries a document the resolver dislikes would trade a
	// missing convenience for a lost file.
	if applied, dErr := metadata.ApplyAssetDefaults(ctx, tx, metadata.ApplyDefaultsParams{
		AssetID:   uuid.UUID(row.ID.Bytes),
		AssetType: in.AssetType,
		UserRef:   id.UserRef,
		Now:       row.CreatedAt.Time,
	}); dErr != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.create.defaults.error",
			slog.String("asset_id", uuid.UUID(row.ID.Bytes).String()),
			slog.String("err", dErr.Error()),
		)
	} else if len(applied) > 0 {
		h.Logger.LogAttrs(ctx, slog.LevelDebug, "assets.create.defaults.applied",
			slog.String("asset_id", uuid.UUID(row.ID.Bytes).String()),
			slog.Int("count", len(applied)),
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("assets: commit: %w", err)
	}

	// Pin rebalancing happens OUTSIDE the asset-table tx so a
	// storage-pin failure doesn't roll back the asset itself —
	// the worst case is an extra `user:` pin we GC later.
	if fileHashPtr != nil {
		if err := h.Storage.AddPin(ctx, storage.PinRef{
			SubjectType: PinSubjectTypeAsset,
			SubjectID:   newID.String(),
		}, *fileHashPtr); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.create.add_pin.error",
				slog.String("asset_id", newID.String()),
				slog.String("file_hash", *fileHashPtr),
				slog.String("err", err.Error()),
			)
		}
		if err := h.Storage.RemovePin(ctx, storage.PinRef{
			SubjectType: storage.PinSubjectTypeUser,
			SubjectID:   strconv.FormatInt(id.UserRef, 10),
		}, *fileHashPtr); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelDebug, "assets.create.remove_user_pin.skipped",
				slog.String("err", err.Error()),
			)
		}
	}

	// Enqueue async variant generation. Worker picks this up within
	// seconds and writes col/preview/screen/hires under the asset's
	// hash. The frontend renders the thumbhash placeholder until the
	// first variant lands and then polls with backoff. Failure to
	// enqueue here is a soft error — the row is still 'pending' so a
	// future backfill catches it; we don't want a queue blip to fail
	// uploads.
	if h.Jobs != nil && fileHashPtr != nil && processingStatus == "pending" {
		// Photos / typical mixed uploads run at PriorityHigh so the
		// queue is FIFO-by-arrival for the user-facing path. Big
		// files can demote themselves once we have size at create
		// time (the create body doesn't carry it today).
		priority := jobs.PriorityHigh
		// force=false: a brand-new asset has no variants to overwrite,
		// and the skip check is what keeps a re-upload of identical
		// bytes (same hash, same variants) nearly free.
		payload := dispatch.NewPayload(newID, *fileHashPtr, in.FileExtension, false)
		// Only enqueue when something can actually render this ext.
		// Unroutable extensions fall through to preview.raster, which
		// terminal-rejects them — a guaranteed dead job (#366). Skip
		// instead; the asset still uploads fine, it just has no preview.
		if dispatch.CanPreview(in.FileExtension) {
			// PlanForExt, not jobTypeForExt: a video gets a cheap poster
			// job at this priority plus the full ladder behind it
			// (#818), everything else gets the single job it always got.
			for _, step := range dispatch.PlanForExt(in.FileExtension, priority) {
				stepPriority := step.Priority
				if _, err := h.Jobs.Enqueue(ctx, step.Type, payload, jobs.EnqueueOpts{
					Priority: &stepPriority,
				}); err != nil {
					h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.create.enqueue_preview_failed",
						slog.String("asset_id", newID.String()),
						slog.String("job_type", string(step.Type)),
						slog.String("err", err.Error()),
					)
				}
			}
		} else {
			h.Logger.LogAttrs(ctx, slog.LevelDebug, "assets.create.no_preview_for_ext",
				slog.String("asset_id", newID.String()),
				slog.String("ext", strDefault(in.FileExtension, "")),
			)
		}

		// Phase 1.14.B — fan out an ai.embed job alongside the
		// preview job so the asset becomes searchable via vector
		// similarity within seconds of upload. PriorityLow because
		// the search-side use case is asynchronous; the operator
		// wants previews first. Idempotency key dedups against
		// in-flight runs for the same (asset, model) — re-enqueue
		// from a fanout retry returns the existing job's id.
		embedPriority := jobs.PriorityLow
		embedPayload := map[string]string{
			"asset_id": newID.String(),
			// Empty model + modality → handler falls back to
			// system_config.ai.embedding.default_model + "text".
		}
		embedIdem := aiEmbedIdempotencyKey(newID.String(), "")
		if _, err := h.Jobs.Enqueue(ctx, aiEmbedJobType, embedPayload, jobs.EnqueueOpts{
			Priority:       &embedPriority,
			IdempotencyKey: embedIdem,
		}); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.create.enqueue_embed_failed",
				slog.String("asset_id", newID.String()),
				slog.String("err", err.Error()),
			)
		}

		// Phase 1.16.B-3-followup-2 — fan out a search.visual_embed job
		// alongside ai.embed so image uploads become searchable via
		// reverse-image within seconds. Sibling to the backfill trigger
		// (PR #205) which handles pre-existing assets. Silent skip
		// when the dispatcher is nil (sidecar not registered) or
		// when the asset isn't an image; guards live inside Dispatch
		// so this call site stays uniform with ai.embed.
		if h.visualEmbedDispatcher != nil {
			h.visualEmbedDispatcher.Dispatch(ctx, VisualEmbedInput{
				AssetID:       newID,
				FileExtension: strDefault(in.FileExtension, ""),
			})
		}

		// Phase 1.18.A-2 — fan out a metadata.extract job for
		// image uploads. Worker reads the source bytes, pulls
		// EXIF / ICC / orientation via the asset/metadata
		// subsystem, then applies the values into the
		// operator-configured field-definitions. The applier's
		// equal-value short-circuit means re-runs over assets
		// that already have the extracted values are silent
		// no-ops.
		if isExifExtractableImageExt(in.FileExtension) {
			metaPayload := map[string]string{
				"asset_id": newID.String(),
			}
			if _, err := h.Jobs.Enqueue(ctx, metadataExtractJobType, metaPayload, jobs.EnqueueOpts{}); err != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.create.enqueue_metadata_extract_failed",
					slog.String("asset_id", newID.String()),
					slog.String("err", err.Error()),
				)
			}
		}

		// Phase 1.14.C — fan out an ai.transcribe job for audio and
		// video uploads. Video without an audio track surfaces as
		// an empty transcript downstream (the orchestrator ProbeDuration
		// returns 0 and the chunker rejects); cheap to enqueue
		// either way + the dedup-on-idempotency-key gate prevents
		// duplicate work if the upload retries.
		ext := strings.ToLower(strings.TrimPrefix(strDefault(in.FileExtension, ""), "."))
		_, isVideo := dispatch.VideoExts[ext]
		_, isAudio := dispatch.AudioExts[ext]
		if isVideo || isAudio {
			transcribePriority := jobs.PriorityLow
			transcribePayload := map[string]string{
				"asset_id": newID.String(),
				// Empty lang_hint + force_model → orchestrator uses
				// system_config.ai.transcribe.* defaults.
			}
			transcribeIdem := aiTranscribeIdempotencyKey(newID.String(), "")
			if _, err := h.Jobs.Enqueue(ctx, aiTranscribeJobType, transcribePayload, jobs.EnqueueOpts{
				Priority:       &transcribePriority,
				IdempotencyKey: transcribeIdem,
			}); err != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.create.enqueue_transcribe_failed",
					slog.String("asset_id", newID.String()),
					slog.String("err", err.Error()),
				)
			}
		}
	}

	return openapi.CreateAsset201JSONResponse(rowToAsset(rowToAssetRow(row), tags)), nil
}

// aiEmbedJobType + aiEmbedIdempotencyKey duplicate the constants
// from app/internal/ai/jobs to avoid an import cycle (ai/jobs depends
// on jobs which depends on assets-side enqueueing). The string + key
// format are part of the bridge contract — a future test in
// app/internal/ai/jobs/handlers_test.go asserts the values match.
const aiEmbedJobType jobs.JobType = "ai.embed"

func aiEmbedIdempotencyKey(assetID, model string) string {
	// SHA-256("ai.embed|<asset_id>|<model>") hex; mirrors
	// aijobs.EmbedIdempotencyKey. Pure-string compute here avoids
	// the cycle.
	sum := sha256.Sum256([]byte("ai.embed|" + assetID + "|" + model))
	return hex.EncodeToString(sum[:])
}

// aiTranscribeJobType + aiTranscribeIdempotencyKey duplicate the
// constants from app/internal/ai/jobs for the same reason as the
// embed pair above. Cross-package pinning test lives in
// embed_fanout_test.go's sibling check.
const aiTranscribeJobType jobs.JobType = "ai.transcribe"

// metadataExtractJobType duplicates the constant from
// app/internal/asset/metadata.JobTypeExtract — same
// avoid-import-cycle rationale as the ai.embed + ai.transcribe
// pairs above. If you change one you MUST change the other; the
// next call to ./scripts/test.sh --go will catch a mismatch via
// the boot-wire build error.
const metadataExtractJobType jobs.JobType = "metadata.extract"

// isExifExtractableImageExt is narrower than [isImageExt] — it
// reports true only for formats the asset/metadata/exif extractor
// claims via Supports(). Used by the upload-fanout to decide
// whether to enqueue a metadata.extract job. HEIC/AVIF/SVG/etc.
// are images but we don't extract from them yet (HEIC needs the
// future libheif add-on).
//
// Delegates rather than restating the set (#579). The backfill needs
// the same answer and could not reach this unexported copy, so the list
// was promoted to imagefmt.ExifExtractableExtensions; two copies of
// "which formats can we extract from" is exactly the drift that leaves
// the upload path and the backfill disagreeing about the same asset.
func isExifExtractableImageExt(ext *string) bool {
	if ext == nil {
		return false
	}
	return imagefmt.IsExifExtractableExtension(*ext)
}

func aiTranscribeIdempotencyKey(assetID, model string) string {
	// SHA-256("ai.transcribe|<asset_id>|<model>") hex; mirrors
	// aijobs.TranscribeIdempotencyKey.
	sum := sha256.Sum256([]byte("ai.transcribe|" + assetID + "|" + model))
	return hex.EncodeToString(sum[:])
}

// dedupResponse renders the existing-asset row into one of the
// two dedup-aware HTTP response shapes per the operator's
// configured [sysconfig.DedupBehavior]:
//
//   - warn  → HTTP 200 with AssetWithDedup (existing asset row +
//     duplicate_warning sub-object).
//   - block → HTTP 409 with UploadConflict ({error,
//     existing_asset_id}).
//   - allow → never reaches here (dedup skip is decided upstream
//     before this helper is called).
//
// The "warn" response carries the FULL existing-asset projection
// so the UI doesn't need a second round-trip to render the
// "this file was already uploaded as X" dialog with thumbnail +
// title.
func (h *Handler) dedupResponse(ctx context.Context, behavior sysconfig.DedupBehavior, existing GetAssetByOwnerHashRow) (openapi.CreateAssetResponseObject, error) {
	existingID := openapi_types.UUID(uuid.UUID(existing.ID.Bytes))
	switch behavior {
	case sysconfig.DedupBehaviorBlock:
		return openapi.CreateAsset409JSONResponse{
			Error:           "an asset with this file already exists in your library",
			ExistingAssetId: existingID,
		}, nil
	default:
		// warn (also fall-through for any future behavior we
		// haven't shipped yet — defaulting to "non-destructive
		// + visible" is the conservative choice).
		full := assetFromGetByOwnerHashRow(existing)
		// This is the one CreateAsset branch that returns an asset which
		// ALREADY has bytes, so it is the one that needs the derived
		// fields (#655). The 201 branch is a row created this instant —
		// no `col` variant, no ladder, nothing measured — where false /
		// null is the honest answer rather than a missing lookup. Here
		// the whole point of the payload is "render the file you already
		// uploaded", and without preview_available the dialog draws a
		// placeholder for an asset with a perfectly good thumbnail.
		if err := h.enrichAssetDerived(ctx, &full); err != nil {
			return nil, err
		}
		out := openapi.AssetWithDedup{
			AssetType:        full.AssetType,
			CreatedAt:        full.CreatedAt,
			Description:      full.Description,
			FileExtension:    full.FileExtension,
			FileHash:         full.FileHash,
			FileSizeBytes:    full.FileSizeBytes,
			Id:               full.Id,
			LadderAvailable:  full.LadderAvailable,
			Metadata:         full.Metadata,
			OwnerUserRef:     full.OwnerUserRef,
			PixelHeight:      full.PixelHeight,
			PixelWidth:       full.PixelWidth,
			PreviewAvailable: full.PreviewAvailable,
			ProcessingStatus: openapi.AssetWithDedupProcessingStatus(full.ProcessingStatus),
			Status:           openapi.AssetWithDedupStatus(full.Status),
			Thumbhash:        full.Thumbhash,
			Title:            full.Title,
			UpdatedAt:        full.UpdatedAt,
		}
		out.DuplicateWarning.ExistingAssetId = existingID
		out.DuplicateWarning.Message = "this file was already uploaded — returning the existing asset"
		return openapi.CreateAsset200JSONResponse(out), nil
	}
}

// assetFromGetByOwnerHashRow narrows the sqlc row shape to the
// openapi.Asset surface so dedupResponse can splice the fields
// into AssetWithDedup. Keeps the splice in one tested place.
func assetFromGetByOwnerHashRow(r GetAssetByOwnerHashRow) openapi.Asset {
	out := openapi.Asset{
		Id:               openapi_types.UUID(uuid.UUID(r.ID.Bytes)),
		Title:            r.Title,
		AssetType:        r.AssetType,
		Status:           openapi.AssetStatus(r.Status),
		ProcessingStatus: openapi.AssetProcessingStatus(r.ProcessingStatus),
		FileHash:         r.FileHash,
		FileExtension:    r.FileExtension,
		FileSizeBytes:    r.FileSizeBytes,
		OwnerUserRef:     r.OwnerUserRef,
	}
	if r.Description != "" {
		s := r.Description
		out.Description = &s
	}
	if r.CreatedAt.Valid {
		out.CreatedAt = r.CreatedAt.Time
	}
	if r.UpdatedAt.Valid {
		out.UpdatedAt = r.UpdatedAt.Time
	}
	if len(r.Metadata) > 0 {
		var m map[string]any
		if err := json.Unmarshal(r.Metadata, &m); err == nil {
			out.Metadata = &m
		}
	}
	if len(r.Thumbhash) > 0 {
		// Base64 on the wire, same as rowToAssetWithDetails. The dedup
		// dialog renders a card, and a card without a thumbhash has no
		// blur-up (#648).
		v := base64.StdEncoding.EncodeToString(r.Thumbhash)
		out.Thumbhash = &v
	}
	return out
}

// isPgUniqueViolation reports whether err is a Postgres SQLSTATE
// 23505 (unique_violation). The dedup path uses it to recognize
// the race-loser case where another goroutine won the unique
// constraint between this caller's pre-check and INSERT.
//
// Match by SQLSTATE on *pgconn.PgError — never by message text
// (varies across Postgres versions).
func isPgUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "23505"
	}
	return false
}


// ---------------------------------------------------------------------------
// GetAsset
// ---------------------------------------------------------------------------

// callerFromContext builds the visibility caller for the request,
// anonymous when there is no identity (#415).
func callerFromContext(ctx context.Context) visibility.Caller {
	if id := auth.IdentityFromContext(ctx); id != nil {
		return visibility.NewCaller(&id.UserRef)
	}
	return visibility.NewCaller(nil)
}

func (h *Handler) GetAsset(
	ctx context.Context,
	req openapi.GetAssetRequestObject,
) (openapi.GetAssetResponseObject, error) {
	// #415 — anonymous callers are admitted, but every caller (including
	// authenticated ones) must now pass a real visibility check. Before
	// this, ANY authenticated caller could fetch ANY asset by id: the
	// only gate was "is there an identity". CanSee replaces that.
	caller := callerFromContext(ctx)
	visible, err := visibility.CanSee(ctx, h.Pool, visibility.EntityAsset, caller, uuid.UUID(req.Id))
	if err != nil || !visible {
		// Fail closed, and 404 rather than 403 so this does not confirm
		// that a hidden asset exists at that id.
		return openapi.GetAsset404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}, nil
	}
	q := New(h.Pool)
	row, err := q.GetAsset(ctx, pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, err
	}
	tags, err := q.ListAssetTags(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("assets: list tags: %w", err)
	}
	// Phase 1.14.B — also fetch per-tag source/confidence/provenance
	// so the response includes the typed tag_details projection.
	// Two queries instead of one for now; trading a round-trip for
	// clarity. A future sqlc query can combine into a single
	// json_agg if profiling shows the second hop matters.
	details, err := q.ListAssetTagsDetailed(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("assets: list tag details: %w", err)
	}
	out := rowToAssetWithDetails(row, tags, details)
	if err := h.enrichAssetDerived(ctx, &out); err != nil {
		return nil, err
	}
	return openapi.GetAsset200JSONResponse(out), nil
}

// enrichAssetDerived fills the four fields a single-asset projection
// cannot carry off its own row — preview_available (#471),
// ladder_available (#610) and the recorded pixel dimensions (#640) —
// for the request's caller.
//
// ONE place, because the class of bug this closes is a response shape
// that disagrees with itself depending on which verb produced it (#655).
// GetAsset had this inline and UpdateAsset had nothing, so a PATCH
// returned preview_available=false for an asset whose GET one
// millisecond later returned true. Any handler emitting an
// openapi.Asset calls this; adding a fifth derived field means editing
// this function, not auditing every caller.
//
// Reads `out.Id` and `out.FileHash`, so the caller must have populated
// the row first. No-op on the zero-value id.
func (h *Handler) enrichAssetDerived(ctx context.Context, out *openapi.Asset) error {
	assetID := uuid.UUID(out.Id)
	if assetID == uuid.Nil {
		return nil
	}
	// Source pixel dimensions (#640). The detail response carries them
	// for the same reason the list does — one asset shape, one meaning
	// per field. Its own round trip because sqlc cannot express the
	// nullability of the projection (see the note on GetAsset); detail is
	// not a hot loop, and this is the same trade the tag-details query
	// above already makes.
	//
	// NOT gated on the content plane: these are metadata about a row the
	// caller has already been handed, like file_size_bytes. Nothing about
	// a width confirms readable bytes.
	var detW, detH *int32
	if err := h.Pool.QueryRow(ctx,
		`SELECT `+pixeldims.SelectColumnsSQL("assets.id")+` FROM assets WHERE assets.id = $1::uuid`,
		assetID.String(),
	).Scan(&detW, &detH); err != nil {
		return fmt.Errorf("assets: pixel dimensions: %w", err)
	}
	if pixeldims.Sane(detW, detH) {
		out.PixelWidth, out.PixelHeight = detW, detH
	}
	// preview_available (#471): a servable `col` exists AND the caller
	// passes the content plane. Detail is not a hot loop, so an EXISTS +
	// CanReadContent here is fine (the list path joins both in one pass).
	detCaller, detCaps := contentCaller(ctx)
	readable, err := visibility.CanReadContent(ctx, h.Pool, detCaller, detCaps, assetID)
	if err != nil {
		return fmt.Errorf("assets: content check: %w", err)
	}
	if readable && out.FileHash != nil && *out.FileHash != "" {
		// Both flags in one round trip. ladder_available is computed
		// against the CONFIGURED ladder (#591), never a hardcoded rung
		// list — an operator who drops a rung must move this flag, not
		// silently invalidate it.
		var hasCol, hasLadder bool
		if err := h.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM storage_variants WHERE object_hash = $1 AND variant_key = 'col'),
			        `+sysconfig.LadderSatisfiedSQL("$1", "$2"),
			*out.FileHash, h.ladder(ctx),
		).Scan(&hasCol, &hasLadder); err != nil {
			return fmt.Errorf("assets: variant check: %w", err)
		}
		out.PreviewAvailable = hasCol
		out.LadderAvailable = hasLadder
	}
	return nil
}

// ---------------------------------------------------------------------------
// UpdateAsset
// ---------------------------------------------------------------------------

func (h *Handler) UpdateAsset(
	ctx context.Context,
	req openapi.UpdateAssetRequestObject,
) (openapi.UpdateAssetResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.UpdateAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdateAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	in := req.Body
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}

	var statusPtr *string
	if in.Status != nil {
		s := string(*in.Status)
		switch s {
		case "draft", "active", "archived":
		default:
			return openapi.UpdateAsset400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "status must be one of draft|active|archived"},
			}, nil
		}
		statusPtr = &s
	}
	var titlePtr *string
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" {
			return openapi.UpdateAsset400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "title cannot be empty"},
			}, nil
		}
		titlePtr = &t
	}
	var descPtr *string
	if in.Description != nil {
		descPtr = in.Description
	}
	var metaJSON []byte
	if in.Metadata != nil {
		raw, err := encodeMetadata(in.Metadata)
		if err != nil {
			return nil, err
		}
		metaJSON = raw
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("assets: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	// Phase 1.16 optimistic-concurrency check. Done inside the tx
	// so two simultaneous edits can't both pass the gate + both
	// commit (the tx isolation guarantees this row is locked by
	// the UPDATE that follows). Caller opts in by passing
	// if_unchanged_since; absent = legacy last-write-wins.
	if in.IfUnchangedSince != nil {
		var currentUpdatedAt time.Time
		err := tx.QueryRow(ctx,
			`SELECT updated_at FROM assets WHERE id = $1 AND deleted_at IS NULL`,
			pgID,
		).Scan(&currentUpdatedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return openapi.UpdateAsset404JSONResponse{
					NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
				}, nil
			}
			return nil, fmt.Errorf("assets: load updated_at: %w", err)
		}
		// Truncate both sides to microsecond precision — Postgres
		// stores timestamptz at µs while Go's JSON marshalling
		// round-trips at ns. A bare equality check would false-
		// positive on the trailing ns.
		stored := currentUpdatedAt.Truncate(time.Microsecond)
		sent := in.IfUnchangedSince.Truncate(time.Microsecond)
		if !stored.Equal(sent) {
			return openapi.UpdateAsset409JSONResponse{
				Error:     "asset was edited by someone else after your last load; reload and try again",
				UpdatedAt: currentUpdatedAt,
			}, nil
		}
	}

	row, err := q.UpdateAsset(ctx, UpdateAssetParams{
		ID:          pgID,
		Title:       titlePtr,
		Description: descPtr,
		Status:      statusPtr,
		Metadata:    metaJSON,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: update: %w", err)
	}

	// Tag replacement when the client sends a tags array.
	tags := []string{}
	if in.Tags != nil {
		clean := dedupeTags(*in.Tags)
		// Two-step replacement: DELETE all + INSERT new. Cheaper than
		// diffing for small sets, and tags don't carry per-row state
		// we'd care to preserve.
		if _, err := tx.Exec(ctx, `DELETE FROM asset_tag WHERE asset_id = $1`, row.ID); err != nil {
			return nil, fmt.Errorf("assets: clear tags: %w", err)
		}
		for _, t := range clean {
			if err := q.AddAssetTag(ctx, AddAssetTagParams{AssetID: row.ID, Tag: t}); err != nil {
				return nil, fmt.Errorf("assets: add tag: %w", err)
			}
		}
		tags = clean
	} else {
		existing, err := q.ListAssetTags(ctx, row.ID)
		if err != nil {
			return nil, fmt.Errorf("assets: list tags: %w", err)
		}
		tags = existing
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("assets: commit: %w", err)
	}

	// Same shape a GET returns (#655). The UPDATE's RETURNING row carries
	// no preview / ladder / dimension columns, so without this a PATCH
	// answered `preview_available: false` for an asset whose GET answers
	// true — and a client that renders straight from the PATCH response
	// loses the thumbnail it had a moment ago.
	updated := rowToAsset(updateRowToGetRow(row), tags)
	if err := h.enrichAssetDerived(ctx, &updated); err != nil {
		return nil, err
	}
	return openapi.UpdateAsset200JSONResponse(updated), nil
}

// ---------------------------------------------------------------------------
// DeleteAsset
// ---------------------------------------------------------------------------

func (h *Handler) DeleteAsset(
	ctx context.Context,
	req openapi.DeleteAssetRequestObject,
) (openapi.DeleteAssetResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.DeleteAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}

	// Fetch first so we can drop the storage pin even though the soft
	// delete doesn't return the row.
	q := New(h.Pool)
	row, err := q.GetAsset(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.DeleteAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, err
	}
	reason := extractSoftDeleteReason(req.Body)
	if len(reason) > softDeleteReasonMaxLen {
		return openapi.DeleteAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "reason exceeds 500 chars"},
		}, nil
	}
	if err := q.SoftDeleteAsset(ctx, SoftDeleteAssetParams{
		ID:            pgID,
		DeletedReason: softDeleteReasonPtr(reason),
	}); err != nil {
		return nil, fmt.Errorf("assets: soft-delete: %w", err)
	}
	if h.Audit != nil {
		if id := auth.IdentityFromContext(ctx); id != nil {
			h.Audit.AdminAssetSoftDeleted(ctx, nil, uuid.UUID(pgID.Bytes).String(), id.UserRef, reason)
		}
	}
	if row.FileHash != nil {
		if err := h.Storage.RemovePin(ctx, storage.PinRef{
			SubjectType: PinSubjectTypeAsset,
			SubjectID:   uuid.UUID(row.ID.Bytes).String(),
		}, *row.FileHash); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.delete.remove_pin.error",
				slog.String("asset_id", uuid.UUID(row.ID.Bytes).String()),
				slog.String("file_hash", *row.FileHash),
				slog.String("err", err.Error()),
			)
		}
	}
	return openapi.DeleteAsset204Response{}, nil
}

// ---------------------------------------------------------------------------
// RestoreAsset — Phase 1.55.C-1b
// ---------------------------------------------------------------------------

// RestoreAsset clears deleted_at + deleted_reason on a soft-deleted
// asset. Admin-only. 404 if the asset is already live (or doesn't
// exist); 403 for non-admin authenticated callers; 401 for anon.
//
// The audit event fires from softdelete.Service.RestoreAsset itself
// so the write + audit stay together.
func (h *Handler) RestoreAsset(
	ctx context.Context,
	req openapi.RestoreAssetRequestObject,
) (openapi.RestoreAssetResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil || id.IsAnonymous() {
		return openapi.RestoreAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(auth.SuperAdminCapability) {
		return openapi.RestoreAsset403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "admin capability required"},
		}, nil
	}
	if h.SoftDelete == nil {
		return nil, fmt.Errorf("assets: RestoreAsset: SoftDelete service unwired")
	}
	// Audit's ctxFromRequest accepts nil (empty reqContext); we
	// don't have the *http.Request on the strict-server code path,
	// but the audit row still fires with actor + subject + reason.
	if err := h.SoftDelete.RestoreAsset(ctx, nil, uuid.UUID(req.Id), id.UserRef); err != nil {
		if errors.Is(err, softdelete.ErrNotDeleted) || errors.Is(err, softdelete.ErrNotFound) {
			return openapi.RestoreAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not soft-deleted"},
			}, nil
		}
		return nil, fmt.Errorf("assets: restore: %w", err)
	}
	return openapi.RestoreAsset204Response{}, nil
}

// ---------------------------------------------------------------------------
// ListAssets
// ---------------------------------------------------------------------------

func (h *Handler) ListAssets(
	ctx context.Context,
	req openapi.ListAssetsRequestObject,
) (openapi.ListAssetsResponseObject, error) {
	// #415 — anonymous callers are admitted. Row visibility is already
	// decided by the predicate inside ListAssetsPageGated, which returns
	// only published-public rows for an anonymous caller.
	callerID := auth.IdentityFromContext(ctx)

	// Phase 1.55.C-1b: ?include_deleted=true is admin-only. Non-
	// admins silently see the default filtered list.
	var includeDeletedArg *bool
	if req.Params.IncludeDeleted != nil && *req.Params.IncludeDeleted && callerID != nil && callerID.Can(auth.SuperAdminCapability) {
		t := true
		includeDeletedArg = &t
	}

	limit := int32(50)
	if req.Params.Limit != nil {
		l := *req.Params.Limit
		if l < 1 {
			l = 1
		}
		if l > maxListLimit {
			l = maxListLimit
		}
		limit = int32(l)
	}

	var cursorTs pgtype.Timestamptz
	var cursorID pgtype.UUID
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		ts, id, err := decodeCursor(*req.Params.Cursor)
		if err != nil {
			return openapi.ListAssets500JSONResponse{
				InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorTs = pgtype.Timestamptz{Time: ts, Valid: true}
		cursorID = pgtype.UUID{Bytes: id, Valid: true}
	}

	var ownerRef *int64
	if req.Params.OwnerRef != nil {
		ownerRef = req.Params.OwnerRef
	}
	var resType *int64
	if req.Params.AssetType != nil {
		resType = req.Params.AssetType
	}
	var statusPtr *string
	if req.Params.Status != nil {
		s := string(*req.Params.Status)
		statusPtr = &s
	}
	var qText *string
	if req.Params.Q != nil {
		s := strings.TrimSpace(*req.Params.Q)
		if s != "" {
			qText = &s
		}
	}
	// `q` and `tag` now compose: both are conjuncts of the one gated
	// query. Before #657 the tag filter lived in a separate sqlc
	// statement that carried no search_text column, so supplying both
	// meant silently dropping `q`.
	var tagFilter *string
	if req.Params.Tag != nil && *req.Params.Tag != "" {
		tagFilter = req.Params.Tag
	}

	q := New(h.Pool)

	// One-shot paging: fetch limit+1 to know whether there's a next page.
	fetch := limit + 1

	var assetsList []openapi.Asset
	var lastCreatedAt time.Time
	var lastID uuid.UUID
	var rowCount int

	// ONE path, filtered or not (#657). Hand-built so the visibility
	// predicate can be spliced in (#429) — sqlc's static SQL cannot take
	// a runtime fragment. `tag` used to fork off to its own static sqlc
	// query, which is how it came to apply no predicate, no ladder and
	// no preview flags: a filter is not a different kind of read, and
	// giving it a second query gave it a second set of rules that then
	// drifted. Anything added here now applies to every browse.
	listCaller, listCaps := contentCaller(ctx)
	rows, err := ListAssetsPageGated(ctx, h.Pool, listCaller, listCaps, ListAssetsPageGatedParams{
		IncludeDeleted:  includeDeletedArg,
		OwnerUserRef:    ownerRef,
		AssetType:       resType,
		Status:          statusPtr,
		Q:               qText,
		Tag:             tagFilter,
		CursorCreatedAt: cursorTs,
		CursorID:        cursorID,
		RowLimit:        fetch,
		Ladder:          h.ladder(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("assets: list: %w", err)
	}
	for i, r := range rows {
		if i >= int(limit) {
			break
		}
		tags, err := q.ListAssetTags(ctx, r.ID)
		if err != nil {
			return nil, fmt.Errorf("assets: list tags: %w", err)
		}
		a := rowToAsset(listRowToGetRow(r.ListAssetsPageRow), tags)
		a.PreviewAvailable = r.PreviewAvailable
		a.LadderAvailable = r.LadderAvailable
		// #640 — the tile's aspect ratio, joined by the same pass.
		// The gated row already applied the pair-or-neither rule.
		a.PixelWidth = r.PixelWidth
		a.PixelHeight = r.PixelHeight
		// Surface soft-delete state so the admin trash view
		// (include_deleted=true) can identify + label deleted rows.
		if r.DeletedAt.Valid {
			dt := r.DeletedAt.Time
			a.DeletedAt = &dt
			a.DeletedReason = r.DeletedReason
		}
		assetsList = append(assetsList, a)
		lastCreatedAt = r.CreatedAt.Time
		lastID = uuid.UUID(r.ID.Bytes)
	}
	rowCount = len(rows)

	resp := openapi.AssetList{Items: assetsList}
	if rowCount > int(limit) {
		next := encodeCursor(lastCreatedAt, lastID)
		resp.NextCursor = &next
	}
	if resp.Items == nil {
		resp.Items = []openapi.Asset{}
	}
	return openapi.ListAssets200JSONResponse(resp), nil
}

// ---------------------------------------------------------------------------
// DownloadAssetFile / DownloadAssetVariant
// ---------------------------------------------------------------------------

// contentCaller resolves the (caller, capabilities) pair the content
// checker needs, admitting ANONYMOUS callers (#416).
//
// These two handlers used to return 401 above the checker, which made
// them the last byte-serving paths that could not serve a guest — the
// raw-http ones in internal/http/handlers moved to this shape in #415
// (see requireContentAccess, which this mirrors). CanReadContent has
// admitted anonymous callers for public-tier assets since #415 and is
// tested for it; these handlers simply never let it run, so a public
// collection rendered for a signed-out visitor with every thumbnail
// broken.
//
// Letting anonymous through here does NOT widen access. CanReadContent
// still decides, and it admits an anonymous caller to the public tier
// only — team, restricted and embargo all deny. And with public mode
// off, auth/middleware.go rejects /assets/* for anonymous before any
// of this runs, so on a private install these lines are unreachable.
//
// An anonymous caller carries no capabilities: nil CapabilityChecker,
// never admin.
func contentCaller(ctx context.Context) (visibility.Caller, visibility.CapabilityChecker) {
	if id := auth.IdentityFromContext(ctx); id != nil {
		return visibility.NewCaller(&id.UserRef), func(code string) bool { return id.Can(code) }
	}
	return visibility.NewCaller(nil), nil
}

func (h *Handler) DownloadAssetFile(
	ctx context.Context,
	req openapi.DownloadAssetFileRequestObject,
) (openapi.DownloadAssetFileResponseObject, error) {
	// #433 — sensitivity gates CONTENT. 404 rather than 403 so this
	// plane does not confirm that a restricted asset exists.
	caller, caps := contentCaller(ctx)
	allowed, err := visibility.CanReadContent(ctx, h.Pool, caller, caps, uuid.UUID(req.Id))
	if err != nil || !allowed {
		return openapi.DownloadAssetFile404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}, nil
	}
	hash, ok, err := h.resolveAssetFileHash(ctx, uuid.UUID(req.Id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return openapi.DownloadAssetFile404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset has no file attached"},
		}, nil
	}

	body, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return openapi.DownloadAssetFile404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "stored object missing for this asset"},
			}, nil
		}
		return nil, err
	}
	return openapi.DownloadAssetFile200ApplicationoctetStreamResponse{
		Body:          body,
		ContentLength: info.Size,
	}, nil
}

func (h *Handler) DownloadAssetVariant(
	ctx context.Context,
	req openapi.DownloadAssetVariantRequestObject,
) (openapi.DownloadAssetVariantResponseObject, error) {
	// #433 — sensitivity gates CONTENT. 404 rather than 403 so this
	// plane does not confirm that a restricted asset exists.
	caller, caps := contentCaller(ctx)
	allowed, err := visibility.CanReadContent(ctx, h.Pool, caller, caps, uuid.UUID(req.Id))
	if err != nil || !allowed {
		return openapi.DownloadAssetVariant404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}, nil
	}
	hash, ok, err := h.resolveAssetFileHash(ctx, uuid.UUID(req.Id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return openapi.DownloadAssetVariant404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset has no file attached"},
		}, nil
	}
	body, info, err := h.Storage.Download(ctx, hash, req.Variant)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return openapi.DownloadAssetVariant404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "variant not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.DownloadAssetVariant200ApplicationoctetStreamResponse{
		Body:          body,
		ContentLength: info.Size,
	}, nil
}

// ---------------------------------------------------------------------------
// Tag management
// ---------------------------------------------------------------------------

// RecreateAssetPreview re-enqueues this asset's preview job at
// PriorityHigh. Used by the AssetViewer's Edit-menu "Recreate
// previews" item, and by the preview-pipeline ops surface when a
// worker bug-fix lands and the user wants existing renders
// regenerated against the new code.
//
// It enqueues with force=true unless the caller passes force=false
// (#760). It previously did not, and the result was a control that
// could not do the only thing it is named for: the job it queued hit
// each handler's "this variant is already in storage" check, skipped
// everything, and completed successfully. The operator got a 202, a
// job id, a `done` job — and the same stale thumbnail. Three merged
// renderer fixes (#689, #750, #753) were invisible on every existing
// install for exactly this reason.
//
// force=false is still reachable and still useful: it is the cheap
// "fill in whatever is missing" pass, e.g. after attaching a companion
// to an asset whose ladder never rendered at all.
//
// Failure to enqueue is loud: the caller gets a 500 rather than a
// silent no-op.
func (h *Handler) RecreateAssetPreview(
	ctx context.Context,
	req openapi.RecreateAssetPreviewRequestObject,
) (openapi.RecreateAssetPreviewResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.RecreateAssetPreview401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if h.Jobs == nil {
		// Test contexts that don't wire a Jobs service get a clean
		// 500 rather than a nil-deref panic.
		return nil, fmt.Errorf("assets: jobs service not configured")
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	row, err := New(h.Pool).GetAsset(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RecreateAssetPreview404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: get for recreate: %w", err)
	}
	if row.FileHash == nil || *row.FileHash == "" {
		return openapi.RecreateAssetPreview400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "asset has no file_hash; nothing to preview"},
		}, nil
	}
	// A deliberate "regenerate preview" for a file no handler can render
	// would enqueue a job whose only outcome is a TerminalError (#366).
	// Tell the caller plainly instead of handing back a 202 for a job
	// doomed to fail.
	if !dispatch.CanPreview(row.FileExtension) {
		return openapi.RecreateAssetPreview400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "no preview is available for this file type"},
		}, nil
	}

	force := true
	if req.Params.Force != nil {
		force = *req.Params.Force
	}
	payload := dispatch.NewPayload(uuid.UUID(req.Id), *row.FileHash, row.FileExtension, force)
	priority := jobs.PriorityHigh

	// A video plans two jobs (#818): the poster refreshes in seconds so
	// the operator sees their "recreate" land, the ladder follows. The
	// response reports the LAST step — the full job — because that is
	// the one whose completion means the asset is actually rebuilt, and
	// it is what a caller polling the returned id is waiting to see.
	steps := dispatch.PlanForExt(row.FileExtension, priority)
	var jobID uuid.UUID
	var jobType jobs.JobType
	for _, step := range steps {
		stepPriority := step.Priority
		id, err := h.Jobs.Enqueue(ctx, step.Type, payload, jobs.EnqueueOpts{Priority: &stepPriority})
		if err != nil {
			return nil, fmt.Errorf("assets: enqueue preview re-render: %w", err)
		}
		jobID, jobType = id, step.Type
	}
	return openapi.RecreateAssetPreview202JSONResponse{
		JobId:   openapi_types.UUID(jobID),
		JobType: string(jobType),
		Force:   force,
	}, nil
}

func (h *Handler) AddAssetTags(
	ctx context.Context,
	req openapi.AddAssetTagsRequestObject,
) (openapi.AddAssetTagsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.AddAssetTags401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil || len(req.Body.Tags) == 0 {
		return openapi.AddAssetTags400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "tags array is required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	if _, err := q.GetAsset(ctx, pgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddAssetTags404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, err
	}
	for _, t := range dedupeTags(req.Body.Tags) {
		if err := q.AddAssetTag(ctx, AddAssetTagParams{AssetID: pgID, Tag: t}); err != nil {
			return nil, fmt.Errorf("assets: add tag: %w", err)
		}
	}
	return openapi.AddAssetTags204Response{}, nil
}

func (h *Handler) RemoveAssetTag(
	ctx context.Context,
	req openapi.RemoveAssetTagRequestObject,
) (openapi.RemoveAssetTagResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.RemoveAssetTag401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	q := New(h.Pool)
	if err := q.RemoveAssetTag(ctx, RemoveAssetTagParams{AssetID: pgID, Tag: req.Tag}); err != nil {
		return nil, fmt.Errorf("assets: remove tag: %w", err)
	}
	return openapi.RemoveAssetTag204Response{}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *Handler) resolveAssetFileHash(ctx context.Context, id uuid.UUID) (string, bool, error) {
	q := New(h.Pool)
	row, err := q.GetAsset(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	if row.FileHash == nil || *row.FileHash == "" {
		return "", false, nil
	}
	return *row.FileHash, true, nil
}

// encodeMetadata serialises the optional API-side metadata map into
// the JSONB column. nil/empty -> "{}" so the column never holds
// SQL NULL.
func encodeMetadata(m *map[string]interface{}) ([]byte, error) {
	if m == nil || len(*m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(*m)
	if err != nil {
		return nil, fmt.Errorf("assets: marshal metadata: %w", err)
	}
	return b, nil
}

// dedupeTags trims, lowercases, and dedups a tag list. Order is
// preserved (first occurrence wins).
func dedupeTags(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// strDefault returns *p or the default if p is nil.
func strDefault(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// encodeCursor builds the opaque pagination token. RFC3339-nano +
// "|" + uuid, base64-url encoded.
func encodeCursor(t time.Time, id uuid.UUID) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, errors.New("bad cursor shape")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return t, id, nil
}

// rowToAsset converts the sqlc-emitted row shape into the OpenAPI
// Asset response. Several sqlc-generated row types share the same
// columns; we normalise via rowToAssetRow/listRowToGetRow etc.
func rowToAsset(row GetAssetRow, tags []string) openapi.Asset {
	return rowToAssetWithDetails(row, tags, nil)
}

// rowToAssetWithDetails populates both `tags` (flat string list,
// backwards-compat) and `tag_details` (typed Phase 1.14.B
// projection). Callers that don't have the detailed list can call
// rowToAsset above which leaves tag_details empty (omitted from
// JSON — pointer field).
func rowToAssetWithDetails(row GetAssetRow, tags []string, details []ListAssetTagsDetailedRow) openapi.Asset {
	a := openapi.Asset{
		Id:               openapi_types.UUID(row.ID.Bytes),
		Title:            row.Title,
		AssetType:        row.AssetType,
		Status:           openapi.AssetStatus(row.Status),
		ProcessingStatus: openapi.AssetProcessingStatus(row.ProcessingStatus),
		CreatedAt:        row.CreatedAt.Time,
		UpdatedAt:        row.UpdatedAt.Time,
		Tags:             tags,
	}
	if len(details) > 0 {
		td := make([]openapi.AssetTagDetail, 0, len(details))
		for _, d := range details {
			item := openapi.AssetTagDetail{
				Value:  d.Tag,
				Source: openapi.AssetTagDetailSource(d.Source),
			}
			if d.Confidence != nil {
				c := float64(*d.Confidence)
				item.Confidence = &c
			}
			if d.CreatedByProvider != nil && *d.CreatedByProvider != "" {
				v := *d.CreatedByProvider
				item.CreatedByProvider = &v
			}
			if d.CreatedByModel != nil && *d.CreatedByModel != "" {
				v := *d.CreatedByModel
				item.CreatedByModel = &v
			}
			td = append(td, item)
		}
		a.TagDetails = &td
	}
	if row.Description != "" {
		d := row.Description
		a.Description = &d
	}
	a.OwnerUserRef = row.OwnerUserRef
	a.FileHash = row.FileHash
	a.FileExtension = row.FileExtension
	a.FileSizeBytes = row.FileSizeBytes
	if row.StateID.Valid {
		v := openapi_types.UUID(row.StateID.Bytes)
		a.StateId = &v
	}
	if len(row.Thumbhash) > 0 {
		// Base64-encoded for JSON transport. The frontend
		// thumbhash decoder accepts both base64 and the raw byte
		// array; base64 is the more common wire format.
		v := base64.StdEncoding.EncodeToString(row.Thumbhash)
		a.Thumbhash = &v
	}
	if len(row.Metadata) > 0 && string(row.Metadata) != "{}" {
		var m map[string]interface{}
		if err := json.Unmarshal(row.Metadata, &m); err == nil {
			a.Metadata = &m
		}
	}
	if a.Tags == nil {
		a.Tags = []string{}
	}
	return a
}

// The CreateAsset/UpdateAsset/ListAsset return shapes have the same
// column set but distinct Go types (sqlc generates one per query).
// These helpers project them onto a common GetAssetRow we already
// have a marshaller for.
func rowToAssetRow(r CreateAssetRow) GetAssetRow {
	return GetAssetRow{
		ID:               r.ID,
		Title:            r.Title,
		Description:      r.Description,
		AssetType:        r.AssetType,
		OwnerUserRef:     r.OwnerUserRef,
		Status:           r.Status,
		FileHash:         r.FileHash,
		FileExtension:    r.FileExtension,
		FileSizeBytes:    r.FileSizeBytes,
		Metadata:         r.Metadata,
		OriginServerID:   r.OriginServerID,
		StateID:          r.StateID,
		ProcessingStatus: r.ProcessingStatus,
		Thumbhash:        r.Thumbhash,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

func updateRowToGetRow(r UpdateAssetRow) GetAssetRow {
	return GetAssetRow{
		ID:               r.ID,
		Title:            r.Title,
		Description:      r.Description,
		AssetType:        r.AssetType,
		OwnerUserRef:     r.OwnerUserRef,
		Status:           r.Status,
		FileHash:         r.FileHash,
		FileExtension:    r.FileExtension,
		FileSizeBytes:    r.FileSizeBytes,
		Metadata:         r.Metadata,
		OriginServerID:   r.OriginServerID,
		StateID:          r.StateID,
		ProcessingStatus: r.ProcessingStatus,
		Thumbhash:        r.Thumbhash,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

func listRowToGetRow(r ListAssetsPageRow) GetAssetRow {
	return GetAssetRow{
		ID:               r.ID,
		Title:            r.Title,
		Description:      r.Description,
		AssetType:        r.AssetType,
		OwnerUserRef:     r.OwnerUserRef,
		Status:           r.Status,
		FileHash:         r.FileHash,
		FileExtension:    r.FileExtension,
		FileSizeBytes:    r.FileSizeBytes,
		Metadata:         r.Metadata,
		OriginServerID:   r.OriginServerID,
		StateID:          r.StateID,
		ProcessingStatus: r.ProcessingStatus,
		Thumbhash:        r.Thumbhash,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

func isImageExt(ext *string) bool {
	if ext == nil {
		return false
	}
	e := strings.ToLower(strings.TrimPrefix(*ext, "."))
	_, ok := dispatch.ImageExts[e]
	return ok
}

// assetTypeFor returns the canonical asset_type ref for a file
// extension, used by createAsset to auto-promote uploads to the right
// category. Returns 0 (unset) when we don't have a strong opinion —
// the caller's explicit choice still wins.
//
// Type refs (seeded in migrations 00027 + 00031 + 00033 + 00034):
//
//	1 Image · 2 Document · 3 Video · 4 Audio · 5 3D Object · 6 Archive
//	7 Font · 8 Comic · 10 Ebook · 11 Audiobook · 12 Texture
//	13 Sprite · 14 Code
//
// Editor-source files (psd / ai / eps / sketch / etc.) land in Image
// alongside finished raster outputs. Texture / sprite / audiobook
// recognised only by dedicated file extensions — generic png / mp3
// stay Image / Audio because the extension can't tell them apart.
func assetTypeFor(ext string) int64 {
	if ext == "" {
		return 0
	}
	e := strings.ToLower(strings.TrimPrefix(ext, "."))
	switch e {
	case "ttf", "otf", "ttc", "otc", "woff", "woff2":
		return 7 // Font
	case "cbr", "cbz", "cb7":
		return 8 // Comic
	case "epub", "mobi", "azw", "azw3", "fb2", "lit", "prc", "pdb":
		return 10 // Ebook
	case "m4b", "aax":
		return 11 // Audiobook
	case "dds", "ktx", "ktx2", "basis", "sbsar", "sbs":
		return 12 // Texture
	case "aseprite", "ase", "pyxel":
		return 13 // Sprite
	case "py", "js", "jsx", "ts", "tsx", "mjs", "cjs",
		"c", "cpp", "cc", "cxx", "h", "hpp", "hh",
		"cs", "java", "go", "rs", "rb", "php", "swift", "kt", "kts", "scala",
		"sh", "bash", "zsh", "fish", "ps1", "bat", "cmd",
		"lua", "gd", "tres", "tscn",
		"mel", "ms", "mxs", "hda", "vex",
		"hlsl", "glsl", "vert", "frag", "shader", "cginc", "usf":
		return 14 // Code
	case "psd", "psb", "ai", "sketch", "fig", "xd", "eps", "cdr",
		"afdesign", "afphoto", "afpub", "clip", "ora", "kra":
		return 1 // Image (editor-source files belong with finished raster)
	}
	if _, ok := dispatch.ModelExts[e]; ok {
		return 5 // 3D Object
	}
	if _, ok := dispatch.VideoExts[e]; ok {
		return 3 // Video
	}
	if _, ok := dispatch.ImageExts[e]; ok {
		return 1 // Image
	}
	switch e {
	case "zip", "rar", "7z", "tar", "gz", "bz2", "xz", "tgz", "tbz2", "txz":
		return 6 // Archive
	case "mp3", "wav", "flac", "aac", "ogg", "m4a", "opus", "wma":
		return 4 // Audio
	case "pdf", "doc", "docx", "txt", "md", "rtf", "odt":
		return 2 // Document
	}
	return 0
}

func needsProcessing(ext *string) bool {
	if ext == nil {
		return false
	}
	e := strings.ToLower(strings.TrimPrefix(*ext, "."))
	if _, ok := dispatch.ImageExts[e]; ok {
		return true
	}
	if _, ok := dispatch.VideoExts[e]; ok {
		return true
	}
	if _, ok := dispatch.ModelExts[e]; ok {
		return true
	}
	if _, ok := dispatch.AudioExts[e]; ok {
		return true
	}
	if _, ok := dispatch.PDFExts[e]; ok {
		return true
	}
	if _, ok := dispatch.FontExts[e]; ok {
		return true
	}
	if _, ok := dispatch.EbookExts[e]; ok {
		return true
	}
	if _, ok := dispatch.EPSExts[e]; ok {
		return true
	}
	if _, ok := dispatch.PSDExts[e]; ok {
		return true
	}
	if _, ok := dispatch.ComicExts[e]; ok {
		return true
	}
	if _, ok := dispatch.TextExts[e]; ok {
		return true
	}
	if _, ok := dispatch.ArchiveExts[e]; ok {
		return true
	}
	return false
}

// computeThumbhash downloads the original bytes, decodes them as an
// image, and runs thumbhash. Returns nil on any failure — the caller
// stores nil as NULL and the feed card falls back to a neutral
// placeholder. This is intentionally soft-fail: we'd rather have an
// asset without a placeholder than fail the upload.
func computeThumbhash(ctx context.Context, svc *storage.Service, hash string, logger *slog.Logger) []byte {
	if svc == nil {
		return nil
	}
	rc, _, err := svc.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelDebug, "assets.create.thumbhash.download_failed",
			slog.String("hash", hash),
			slog.String("err", err.Error()),
		)
		return nil
	}
	defer rc.Close()
	// Cap the decoder input; thumbhash needs the whole image but a
	// 100 MB ProRes still has us read the header only via
	// image.Decode + the JPEG/PNG decoders. The 25 MB cap protects
	// us from someone uploading a multi-GB tiff and us OOMing on
	// the decode.
	limited := io.LimitReader(rc, 25*1024*1024)
	img, _, err := image.Decode(limited)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelDebug, "assets.create.thumbhash.decode_failed",
			slog.String("hash", hash),
			slog.String("err", err.Error()),
		)
		return nil
	}
	return thumbhash.EncodeImage(img)
}

// Compile-time assertion: catches openapi-codegen signature drift.
var _ interface {
	CreateAsset(context.Context, openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error)
	GetAsset(context.Context, openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error)
	UpdateAsset(context.Context, openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error)
	DeleteAsset(context.Context, openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error)
	ListAssets(context.Context, openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error)
	DownloadAssetFile(context.Context, openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error)
	DownloadAssetVariant(context.Context, openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error)
	AddAssetTags(context.Context, openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error)
	RemoveAssetTag(context.Context, openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error)
} = (*Handler)(nil)
