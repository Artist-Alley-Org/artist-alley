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
	"github.com/mscrnt/artist-alley/app/internal/iiif/presentation"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/posts"
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

	// registry is kept for cross-package invalidations. Soft-deleting
	// or restoring an asset changes what every post holding it renders,
	// and those posts are cached in posts/ (#920). Nil-safe — the
	// helper no-ops on a nil registry.
	registry *cache.Registry

	// manifests is the IIIF Presentation manifest cache (#935).
	// presentation.LoadAsset selects the asset's title + description
	// and applies EntityAsset's ROW predicate, so a PATCH, a delete
	// and a restore each change what the manifest says — and the
	// manifest is cached under its own domain, which nothing on those
	// paths was evicting.
	//
	// Injected post-construction via SetManifestCache: the cache is
	// built at the composition root AFTER this handler (it needs the
	// same registry), and a constructor parameter would have forced
	// that ordering to change. Nil-safe on every method, so tests and
	// any build without IIIF wired simply no-op.
	manifests *presentation.Cache

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

// SetManifestCache injects the IIIF Presentation manifest cache so
// asset writes can evict it (#935). Boot wire is the only caller; the
// cache is constructed after this handler, which is why this is a
// setter and not a constructor argument.
func (h *Handler) SetManifestCache(c *presentation.Cache) { h.manifests = c }

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
	h := &Handler{Pool: pool, Storage: storageSvc, Logger: logger, Jobs: jobSvc, SysConfig: sysCfg, registry: registry}
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

	// #953 — the team gate, the SAME rule POST /posts runs (#954), from
	// the same home (visibility.CanAssignToTeam). Before this there was
	// no way at all to put an uploaded asset in a team: the seeder set
	// the column, the API never could, so `sensitivity='team'`,
	// team-scoped `assets.admin` (#930), the team-scoped field plane
	// (#939) and team-scoped publication (#938) all worked on seeded
	// rows and on nothing a user made.
	//
	// The grant half asks about `assets.admin`, the code
	// canMutateAsset consults — assignment is what CONFERS that
	// team-scoped right over this row, so the code that names the right
	// is the one entitled to hand it out.
	//
	// Runs BEFORE the dedup pre-check and before the transaction: a
	// refusal must not first tell the caller whether they already own a
	// file with these bytes, and must never write a row it rolls back.
	//
	// The refusal is 404 "team not found" — identical to the FK
	// violation below, so an unauthorised team and a nonexistent one
	// cannot be told apart.
	var teamID pgtype.UUID
	if in.TeamId != nil {
		ok, gErr := h.mayAssignToTeam(ctx, id, uuid.UUID(*in.TeamId))
		if gErr != nil {
			return nil, fmt.Errorf("assets: team gate: %w", gErr)
		}
		if !ok {
			return openapi.CreateAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
			}, nil
		}
		teamID = pgtype.UUID{Bytes: uuid.UUID(*in.TeamId), Valid: true}
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
	// #1115 — the mature self-label, refused when the instance
	// disallows it. Checked BEFORE any storage or thumbhash work: a
	// request that is going to be refused should not first spend a
	// synchronous image decode on it.
	mature := in.Mature != nil && *in.Mature
	if ok, err := h.matureWriteAllowed(ctx, mature); err != nil {
		return nil, fmt.Errorf("assets: mature policy: %w", err)
	} else if !ok {
		return openapi.CreateAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: errMatureNotAllowed.Error()},
		}, nil
	}

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
	// The DB-side partial unique index from migration 00001
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
		TeamID:           teamID,
		Mature:           mature,
	})
	if err != nil {
		// The team gate above already refused every team this caller
		// cannot assign to, absent ones included, so a 23503 here means
		// the team was hard-deleted in the gap. Same 404 as the gate —
		// a race must not be distinguishable from a refusal either.
		if isFKViolation(err, "assets_team_id_fkey") {
			return openapi.CreateAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "team not found"},
			}, nil
		}
		// Every OTHER foreign key this INSERT carries a client value
		// into (#966). Unlike team_id above, none of these hides
		// anything: an asset type, a workflow state and an uploaded
		// object are not private, so the honest answer is "you named
		// one that does not exist" and the honest status is 400.
		//
		// Without this the row's constraint name reached the caller
		// verbatim. NewStrictHandler's default response-error handler
		// is `http.Error(w, err.Error(), 500)`, so `fmt.Errorf("assets:
		// insert: %w", err)` below put pgx's full message — table,
		// column, constraint, SQLSTATE — in the body of a 500 that
		// ordinary bad input could trigger unauthenticated-adjacent.
		// The message says the field the CALLER sent and nothing about
		// the schema it landed in.
		if msg, bad := createAssetFKMessage(err); bad {
			return openapi.CreateAsset400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: msg},
			}, nil
		}
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
			ScrubAvailable:   full.ScrubAvailable,
			ProcessingStatus: ptr(openapi.AssetWithDedupProcessingStatus(strDefault((*string)(full.ProcessingStatus), ""))),
			Status:           ptr(openapi.AssetWithDedupStatus(strDefault((*string)(full.Status), ""))),
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
		Title:            &r.Title,
		AssetType:        &r.AssetType,
		Status:           ptr(openapi.AssetStatus(r.Status)),
		ProcessingStatus: ptr(openapi.AssetProcessingStatus(r.ProcessingStatus)),
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
		out.CreatedAt = &r.CreatedAt.Time
	}
	if r.UpdatedAt.Valid {
		out.UpdatedAt = &r.UpdatedAt.Time
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

// isFKViolation reports whether err is a Postgres SQLSTATE 23503
// (foreign_key_violation) raised by the NAMED constraint. Twin of
// posts.isFKError; matched on SQLSTATE + constraint name, never on
// message text, which varies across Postgres versions.
//
// The constraint name matters as much as the code: a bare "23503 means
// team not found" would answer 404 "team not found" for a violation of
// any other FK on the row, which is how a 500 gets dressed up as a
// client error.
func isFKViolation(err error, constraint string) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "23503" && pe.ConstraintName == constraint
	}
	return false
}

// createAssetFKConstraints maps every foreign key on `assets` that the
// create path carries a CLIENT-SUPPLIED value into, to the message the
// caller gets when that value names nothing (#966).
//
// The table is enumerated, not derived, and that is the point. #941 and
// #946 both cost weeks because a rule was applied to the one column
// somebody happened to be looking at while its siblings kept the old
// behaviour; `asset_type` was simply the column the stale seed script
// hit first. Every FK on the row is listed here or excluded below with
// a reason, so "did anyone check the others" has an answer in the file.
//
// EXCLUDED, deliberately:
//
//   - assets_team_id_fkey — handled above, and NOT as a 400. Team
//     membership is exactly the thing a caller should not be able to
//     probe for, so an unassignable team and a nonexistent one both
//     answer 404 "team not found". Folding it in here would turn the
//     asset-create endpoint into a team-existence oracle, which is the
//     leak #953 closed. Its absence from this map is load-bearing.
//
// NOT A FOREIGN KEY, so nothing can violate:
//
//   - owner_user_ref — server-set from the identity, and the user
//     tables carry no FK by federation design.
//   - origin_server_id — server-set, always NULL on this path.
//
// The message names the REQUEST FIELD and describes the value in the
// caller's own vocabulary. It must never name a table, a constraint, a
// SQLSTATE or a relation; asset_fk_leak_test.go asserts that directly
// rather than trusting the strings below to stay clean.
var createAssetFKConstraints = map[string]string{
	"assets_asset_type_fkey": "asset_type: no such asset type",
	"assets_state_id_fkey":   "state_id: no such workflow state",
	"assets_file_hash_fkey":  "file_hash: no uploaded object with that hash — upload the bytes first",
}

// createAssetFKMessage classifies an INSERT error as "the caller named
// something that does not exist" and returns the 400 body for it.
//
// Matched on SQLSTATE + constraint name, never on message text — the
// same discipline isFKViolation applies, for the same reason. An
// unrecognised constraint returns false and falls through to the 500,
// because a foreign key nobody enumerated here is a server-side
// surprise and blaming the caller for it would be a second lie.
func createAssetFKMessage(err error) (string, bool) {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23503" {
		return "", false
	}
	msg, known := createAssetFKConstraints[pe.ConstraintName]
	return msg, known
}

// mayAssignToTeam adapts an *auth.Identity to
// visibility.CanAssignToTeam — the SHARED rule behind
// `AssetCreate.team_id` (#953) and `PostCreate.team_id` (#954).
// posts.Handler has the mirror-image adapter; the rule itself exists
// once, in `visibility`, for the reason epic #665 exists.
//
// The SCOPED half asks about `assets.admin` — the code canMutateAsset
// consults, and the right assignment confers on the receiving team.
// Posts ask about `posts.admin` for exactly the same reason. A single
// cross-entity code would let a holder of one plant rows in the other's
// space; a NEW code would be held by nobody, which is how the team tier
// got into the state #953 describes.
func (h *Handler) mayAssignToTeam(ctx context.Context, id *auth.Identity, teamID uuid.UUID) (bool, error) {
	if id == nil {
		return false, nil
	}
	return visibility.CanAssignToTeam(
		ctx,
		h.Pool,
		visibility.NewCaller(&id.UserRef),
		visibility.CapabilityChecker(func(code string) bool { return id.Can(code) }),
		id.ScopedTeams(CapAssetsAdmin),
		teamID,
	)
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
// for the request's caller, AND applies the #899 field withholding.
//
// ONE place, because the class of bug this closes is a response shape
// that disagrees with itself depending on which verb produced it (#655).
// GetAsset had this inline and UpdateAsset had nothing, so a PATCH
// returned preview_available=false for an asset whose GET one
// millisecond later returned true. Any handler emitting an
// openapi.Asset calls this; adding a fifth derived field means editing
// this function, not auditing every caller.
//
// #899: that "any handler emitting an openapi.Asset calls this"
// property is why the withholding lives here too — but it is NOT the
// whole hook, and assuming it was would have left the leak open. The
// BROWSE LIST does not come through here: it computes its flags in SQL
// in one pass (list_page.go) and never calls this function, so it
// applies the same rule at its own site, from its own copy of the same
// decision.
//
// The withholding and the three availability flags are derived from ONE
// readability decision, for the same reason the post preview enrich
// does it that way — deriving them separately would let them disagree
// on a restricted asset.
//
// Reads `out.Id` and `out.FileHash`, so the caller must have populated
// the row first. No-op on the zero-value id.
func (h *Handler) enrichAssetDerived(ctx context.Context, out *openapi.Asset) error {
	assetID := uuid.UUID(out.Id)
	if assetID == uuid.Nil {
		return nil
	}
	// The readability inputs + the owner's display name, in one round
	// trip. This REPLACED a CanReadContent call that loaded three of the
	// same columns: FieldsReadable is the conjunction of the row and
	// content planes rather than the content plane alone, and it is the
	// same function the container surfaces and the browse list use, so
	// the four cannot drift (#899).
	detCaller, detCaps := contentCaller(ctx)
	fieldsRow, ownerName, err := visibility.LoadFieldsRow(ctx, h.Pool, detCaller, assetID, mutationCaps(ctx))
	if err != nil {
		return fmt.Errorf("assets: readability inputs: %w", err)
	}
	readable := visibility.FieldsReadable(fieldsRow, detCaller, detCaps)
	// #939 — TWO decisions from one row. `readable` is the FIELD plane
	// and now admits a team-scoped `assets.admin` holder; `picture` is
	// the same conjunction WITHOUT that disjunct, and gates the
	// thumbhash blur plus the three availability flags. A mutation
	// holder gets a richer placeholder — real fields, no picture — per
	// ADR 0064.
	picture := visibility.PreviewReadable(fieldsRow, detCaller, detCaps)
	if !readable {
		// Replace the WHOLE value, not selected fields — see
		// withheldAsset's doc for why this is a literal and not a
		// blanking pass. Nothing below this line runs, so no derived
		// field can reintroduce a column after the withholding.
		*out = withheldAsset(out.Id, ownerName)
		return nil
	}
	// Source pixel dimensions (#640). The detail response carries them
	// for the same reason the list does — one asset shape, one meaning
	// per field. Its own round trip because sqlc cannot express the
	// nullability of the projection (see the note on GetAsset); detail is
	// not a hot loop, and this is the same trade the tag-details query
	// above already makes.
	//
	// The blur is the picture, not a field (#939). ADR 0064 puts the
	// thumbhash on the BINARY side — "a thumbhash IS a blur" — so a
	// caller who reaches these columns only through a mutation
	// capability gets the fields with the picture cleared.
	if !picture {
		out.Thumbhash = nil
	}
	// Reached only when `readable` — a source resolution is a fact about
	// a file, and #899 retired the earlier reasoning that dimensions ride
	// the same plane as the row rather than the same plane as the bytes.
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
	// passes the content plane — the same `readable` decided above.
	if out.FileHash != nil && *out.FileHash != "" {
		// All three flags in one round trip. ladder_available is computed
		// against the CONFIGURED ladder (#591), never a hardcoded rung
		// list — an operator who drops a rung must move this flag, not
		// silently invalidate it.
		var hasCol, hasLadder, hasScrub bool
		if err := h.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM storage_variants WHERE object_hash = $1 AND variant_key = 'col'),
			        `+sysconfig.LadderSatisfiedSQL("$1", "$2")+`,
			        EXISTS (SELECT 1 FROM storage_variants WHERE object_hash = $1 AND variant_key = 'sprites.vtt')`,
			*out.FileHash, h.ladder(ctx),
		).Scan(&hasCol, &hasLadder, &hasScrub); err != nil {
			return fmt.Errorf("assets: variant check: %w", err)
		}
		// AND with `picture`, not `readable` (#939): these three are a
		// promise the binary handlers must keep, and those still refuse a
		// mutation holder. A true flag on gated bytes is a 403 the client
		// walks straight into.
		hasCol = hasCol && picture
		hasLadder = hasLadder && picture
		hasScrub = hasScrub && picture
		out.PreviewAvailable = &hasCol
		out.LadderAvailable = &hasLadder
		out.ScrubAvailable = &hasScrub
	}
	// One asset shape, one meaning per field: the detail response carries
	// the at-a-glance strip and the provenance for the same reason it
	// carries pixel dimensions. Reached only when `readable`, below the
	// withholding return above.
	page := []openapi.Asset{*out}
	if err := h.decorateCards(ctx, page); err != nil {
		return fmt.Errorf("assets: card decoration: %w", err)
	}
	*out = page[0]
	return nil
}

// ---------------------------------------------------------------------------
// Authorization helpers
// ---------------------------------------------------------------------------

// CapAssetsAdmin lets a holder manage assets that are not theirs —
// metadata edit, soft-delete, and undo of their own delete. Named to
// read alike beside posts.CapPostsAdmin ("posts.admin") and
// collections' "collections.admin", the two capabilities it behaves
// like; migration 00037 seeds it.
//
// It is scope-aware. A grant with `user_capability_grants.team_id = X`
// covers X and every descendant of X, because the resolver
// pre-expands scoped grants through `team_closure` before Can() ever
// runs (auth/middleware.go). That is what makes "a concept art
// director may manage a file belonging to someone on their team" one
// call rather than a tree walk.
//
// It does NOT confer publication. That is a separate set of verbs —
// CapAssetsPublish / CapAssetsArchive / CapAssetsUnarchive below, wired
// by #938 — and the design note in migration 00037 explains why the two
// were never allowed to merge.
//
// The string is declared ONCE, in `visibility`, because #939 made the
// read gate consult it too and `assets` imports `visibility` (the edge
// only runs that way). This alias keeps the name readable at the write
// sites below; it is not a second declaration.
const CapAssetsAdmin = visibility.AssetsAdmin

// canMutateAsset decides whether the caller may edit or delete this
// asset. Owner, a team-scoped or global `assets.admin`, or
// `system.admin`.
//
// Assets were the outlier here: UpdateAsset and DeleteAsset checked
// only that the caller was authenticated, so any signed-in account
// could rewrite or remove every asset in the instance (#930). Posts
// and collections have had canMutatePost / canMutateCollection since
// they were written. This is deliberately ONE helper serving both
// handlers rather than two inline checks, because two copies of a
// security rule drift and the drift is the bug.
//
// The three arguments come from GetAssetMutationSubject, and each of
// the two nullable ones is a trap:
//
//   - ownerRef is *int64: `assets.owner_user_ref` is NULLABLE. A
//     NULL owner must match NOBODY. Dereferencing it blind panics;
//     treating nil as "unowned, so fair game" would hand every
//     ownerless asset to every caller. Only system.admin reaches one.
//
//   - teamID may be invalid: `assets.team_id` is NULLABLE too. An
//     asset with no team has no scope for InTeam to check, so the
//     scoped disjunct is SKIPPED and the caller falls back to owner
//     or a GLOBAL grant. It must never fall back to "no scope
//     required, therefore anyone passes" — a team-scoped grant holder
//     gets nothing from a team-less asset.
//
// And the caller side has the third: the anonymous sentinel carries
// UserRef 0 (auth.Identity.IsAnonymous). visibility/content.go
// documents the same hazard on the read path — an asset owned by ref
// 0 would make a bare `*ownerRef == id.UserRef` hand ownership to
// every anonymous visitor. So non-anonymity is established BEFORE any
// ownership comparison, and ref 0 is refused as an owner outright.
//
// # Where the rule actually lives now (#822)
//
// The logic moved to visibility.AssetMutationCaps.MayMutateOwned and
// this is the adapter that hands it an *auth.Identity. It moved because
// `PATCH /assets/{id}` is no longer the only way to change
// `assets.title`: a field definition can declare itself a view onto that
// column, so `PUT /assets/{id}/fields/{field_id}` writes it too — and
// the metadata package cannot import this one (this one imports it).
// The alternative was a second copy of an authorisation rule, which is
// what the paragraph above already says is the bug.
func canMutateAsset(id *auth.Identity, ownerRef *int64, teamID pgtype.UUID) bool {
	if id == nil || id.IsAnonymous() {
		return false
	}
	var team *uuid.UUID
	if teamID.Valid {
		t := uuid.UUID(teamID.Bytes)
		team = &t
	}
	caps := visibility.ResolveAssetMutationCaps(
		func(code string) bool { return id.Can(code) },
		id.ScopedTeams(CapAssetsAdmin),
	)
	return caps.MayMutateOwned(id.UserRef, ownerRef, team)
}

// The publication verbs (#938). Seeded in migration 00001 and granted
// to roles there, they gated NOTHING until this file consulted them:
// changing `status` was owner-or-system.admin, so publication could not
// be delegated at all. An operator who granted `assets.publish` to a
// lead had delegated nothing and had no way to find that out — the
// "accepted but inert" defect class, in capability clothing.
//
// They are deliberately SEPARATE from CapAssetsAdmin. `assets.admin` is
// content management; `status` is a disclosure lever, because
// visibility/predicate.go demands `status = 'active'` before an
// anonymous reader may see the row. Folding these into `assets.admin`
// would undo the boundary #936 drew — the same separation Kubernetes
// draws by withholding `escalate` / `bind` from ordinary edit rights.
//
// `assets.submit` and `assets.review` are NOT here. Their seeded
// descriptions name a `pending_review` status, and the live constraint
// is `CHECK (status = ANY (ARRAY['draft','active','archived']))` — they
// gate the exit from a state that does not exist, so there is nothing
// to enforce. Adding it is a schema decision belonging to #895/#896/
// #897; #951 tracks the choice between building it and deleting them.
// Migration 00038 rewrites their descriptions to say so plainly, so an
// operator reading the capabilities table is not sold a no-op.
const (
	CapAssetsPublish   = "assets.publish"
	CapAssetsArchive   = "assets.archive"
	CapAssetsUnarchive = "assets.unarchive"
)

// assetStatusCapabilities maps a status transition onto the capability
// codes a NON-OWNER must hold to perform it, and reports whether the
// pair is one this handler recognises (an unknown pair fails closed).
//
// The live enum is `draft | active | archived`, so there are six
// ordered pairs, and the three verbs do not partition them one-to-one.
// The rule that resolves the overlaps, and the reason for it:
//
//	ENTERING `active` ALWAYS requires assets.publish, with no
//	substitute.
//
// That is the only security-critical clause. `active` is the state the
// anonymous read branch tests for, so `→ active` is THE disclosure act;
// if any other verb could reach it, that verb would silently be a
// publication right and the separation above would be decorative.
// LEAVING `active` is not a disclosure — it only removes reach — so it
// does not need the same protection, and the remaining transitions are
// governed by whichever verb names them.
//
//	draft     → active    publish              the verb's own transition
//	archived  → active    publish + unarchive  a disclosure AND an exit from archived
//	draft     → archived  archive              entering archived; source is irrelevant,
//	                                           neither state is publicly reachable
//	active    → archived  archive              retiring published work; de-disclosure
//	archived  → draft     unarchive            leaving archived, to a private state
//	active    → draft     publish              retraction — the inverse of the publish
//	                                           decision, and there is no `assets.unpublish`
//
// Two of those deserve their reasoning stated, because the mapping is
// not forced:
//
//   - `draft → archived` takes `archive` alone. Archiving a draft is
//     entering the archive, which is what the verb means; requiring
//     `publish` too would mean a lead who may retire work cannot retire
//     work that was never published, which is nonsense, and neither
//     endpoint is anonymously readable so nothing is disclosed.
//
//   - `active → draft` (unpublishing) takes `publish`. Whoever may
//     decide a thing is public may decide it is not — retraction is the
//     publish decision pointing the other way, and it is exactly what
//     the withheld right in #936 described. Handing it to `archive`
//     would make `archive` mean two different things, and handing it to
//     nobody would leave a published asset un-retractable by the very
//     person trusted to have published it.
//
// `archived → active` requiring BOTH is the one conjunction, and it is
// deliberate: an `unarchive` holder must not be able to publish (that
// is a side door into `active`), and a `publish` holder does not
// thereby get to decide what comes out of the archive. `unarchive`
// alone is still a real, useful grant — it performs `archived → draft`,
// returning work to its owner for rework.
func assetStatusCapabilities(from, to string) ([]string, bool) {
	switch {
	case from == to:
		return nil, true
	case to == "active" && from == "draft":
		return []string{CapAssetsPublish}, true
	case to == "active" && from == "archived":
		return []string{CapAssetsPublish, CapAssetsUnarchive}, true
	case to == "archived":
		return []string{CapAssetsArchive}, true
	case to == "draft" && from == "archived":
		return []string{CapAssetsUnarchive}, true
	case to == "draft" && from == "active":
		return []string{CapAssetsPublish}, true
	}
	return nil, false
}

// canTransitionAssetStatus decides whether the caller may move this
// asset from `from` to `to`. It REPLACES canSetAssetStatus, which
// answered the coarser "may you touch status at all" as owner-or-
// system.admin; there is deliberately no second predicate beside this
// one, because two copies of a security rule drift and the drift is the
// bug (the note on canMutateAsset says the same thing).
//
// It mirrors canMutateAsset's shape exactly — anonymous refused,
// system.admin, owner, team-scoped grant, global grant — and for the
// same reasons, including the two nullable traps documented there:
// a NULL owner matches nobody, and a team-less asset SKIPS the scoped
// disjunct rather than treating "no scope required" as "anyone passes".
//
// It resolves the scoped question through id.Can(code, auth.InTeam(…))
// and nothing else. That is not a shortcut, it is the only correct
// route: `Identity.scopedCaps` is built by the resolver from FOUR
// inputs — direct grants, role_capabilities reached by a recursive walk
// of roles.parent_id carrying user_roles.team_id, minus
// user_capability_revokes, then expanded through team_closure. A
// team-scoped ROLE assignment produces ZERO rows in
// user_capability_grants, and these three verbs are granted through a
// role in the baseline (role_capabilities, 00001), so any hand-rolled
// derivation from the grants table would miss precisely the path that
// matters here.
//
// Every required code must pass. See assetStatusCapabilities for which
// codes each transition requires and why.
func canTransitionAssetStatus(
	id *auth.Identity,
	ownerRef *int64,
	teamID pgtype.UUID,
	from, to string,
) bool {
	if id == nil || id.IsAnonymous() {
		return false
	}
	// system.admin is the global override everywhere, and reaches
	// NULL-owner and team-less assets too.
	if id.Can(auth.SuperAdminCapability) {
		return true
	}
	// The owner keeps their own publication lever, unconditionally.
	// Both sides must be a real user: ref 0 is the anonymous sentinel.
	if ownerRef != nil && *ownerRef != 0 && id.UserRef != 0 && *ownerRef == id.UserRef {
		return true
	}
	codes, known := assetStatusCapabilities(from, to)
	if !known {
		return false
	}
	for _, code := range codes {
		if !hasAssetCapability(id, code, teamID) {
			return false
		}
	}
	// codes is empty only for from == to, which the handler does not
	// route here; treat it as permitted rather than as a bare `true`
	// for an unrecognised pair, which the !known branch already caught.
	return true
}

// hasAssetCapability answers one capability code against one asset's
// team scope, the way canMutateAsset does inline: prefer the
// team-scoped question when the asset actually HAS a team, and fall
// back to a GLOBAL holding. A team-scoped grant confers nothing on a
// team-less asset.
func hasAssetCapability(id *auth.Identity, code string, teamID pgtype.UUID) bool {
	if teamID.Valid && id.Can(code, auth.InTeam(uuid.UUID(teamID.Bytes))) {
		return true
	}
	return id.Can(code)
}

// ---------------------------------------------------------------------------
// UpdateAsset
// ---------------------------------------------------------------------------

func (h *Handler) UpdateAsset(
	ctx context.Context,
	req openapi.UpdateAssetRequestObject,
) (openapi.UpdateAssetResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil || caller.IsAnonymous() {
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

	// Read the row the gate and the concurrency check both need,
	// inside the tx and BEFORE the UPDATE. Order matters twice over:
	//
	//   - Before the UPDATE, so a refused caller changes nothing. A
	//     gate that answers 403 after writing is a gate that does not
	//     exist, and a status-only assertion would not catch it.
	//   - Inside the tx, so the authorisation decision and the
	//     optimistic-concurrency comparison are made against the same
	//     version of the row that the UPDATE then locks.
	subject, err := q.GetAssetMutationSubject(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.UpdateAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: load mutation subject: %w", err)
	}
	// ONE request, TWO planes, and #938 is the issue that separated them.
	//
	// `assets.admin` is content management. `status` is a disclosure
	// lever — visibility/predicate.go demands `status = 'active'` before
	// an anonymous reader may see the row — and the publication verbs
	// govern it. Before this the handler required content-management
	// authority for the whole request, which had two consequences: the
	// publication verbs enforced nothing, and publication could not be
	// delegated WITHOUT also handing over the power to rewrite. Both
	// halves of that bundling are the bug.
	//
	// So each plane is gated by its own rule and neither implies the
	// other. `changesStatus` is compared against the CURRENT value, so a
	// PATCH that merely echoes the status back is not a transition —
	// the boundary is about CHANGING who can reach the asset.
	touchesContent := titlePtr != nil || descPtr != nil || metaJSON != nil || in.Tags != nil
	changesStatus := statusPtr != nil && *statusPtr != subject.Status

	// The content plane. Skipped ONLY for a request whose sole effect is
	// a status transition. A no-op PATCH still lands here — it commits an
	// UPDATE and bumps updated_at, so it is a write on someone's asset
	// even when no column changes value.
	if touchesContent || !changesStatus {
		if !canMutateAsset(caller, subject.OwnerUserRef, subject.TeamID) {
			return openapi.UpdateAsset403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "you may not edit this asset"},
			}, nil
		}
	}
	// The publication plane, gated PER TRANSITION: which verb is needed
	// depends on which way the status moves.
	//
	// A holder who passes here but not the content gate still cannot read
	// what they published if the read rule refuses them — the 200 body is
	// built by enrichAssetDerived, which applies the #899/#939 field
	// withholding to every openapi.Asset this handler emits. A publish
	// grant is a right to decide reachability, not a side door into the
	// field plane.
	if changesStatus {
		if !canTransitionAssetStatus(caller, subject.OwnerUserRef, subject.TeamID, subject.Status, *statusPtr) {
			// Name the capability. A bare "forbidden" leaves an operator
			// guessing which of three verbs they failed to grant, and the
			// codes are already visible to them in the admin capability
			// list.
			codes, _ := assetStatusCapabilities(subject.Status, *statusPtr)
			msg := fmt.Sprintf("changing this asset's status from %q to %q is reserved to its owner", subject.Status, *statusPtr)
			if len(codes) > 0 {
				msg += " or a holder of " + strings.Join(codes, " and ")
			}
			return openapi.UpdateAsset403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: msg},
			}, nil
		}
	}

	// Phase 1.16 optimistic-concurrency check. Done inside the tx
	// so two simultaneous edits can't both pass the gate + both
	// commit (the tx isolation guarantees this row is locked by
	// the UPDATE that follows). Caller opts in by passing
	// if_unchanged_since; absent = legacy last-write-wins.
	if in.IfUnchangedSince != nil {
		// Truncate both sides to microsecond precision — Postgres
		// stores timestamptz at µs while Go's JSON marshalling
		// round-trips at ns. A bare equality check would false-
		// positive on the trailing ns.
		stored := subject.UpdatedAt.Time.Truncate(time.Microsecond)
		sent := in.IfUnchangedSince.Truncate(time.Microsecond)
		if !stored.Equal(sent) {
			return openapi.UpdateAsset409JSONResponse{
				Error:     "asset was edited by someone else after your last load; reload and try again",
				UpdatedAt: subject.UpdatedAt.Time,
			}, nil
		}
	}

	// #1115. narg, so an absent field leaves the flag alone — the same
	// PATCH contract every other column here honours, and what lets the
	// artist's own edit and the operator's override be one endpoint.
	var maturePtr *bool
	if req.Body != nil && req.Body.Mature != nil {
		if ok, merr := h.matureWriteAllowed(ctx, *req.Body.Mature); merr != nil {
			return nil, fmt.Errorf("assets: mature policy: %w", merr)
		} else if !ok {
			return openapi.UpdateAsset400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: errMatureNotAllowed.Error()},
			}, nil
		}
		maturePtr = req.Body.Mature
	}

	row, err := q.UpdateAsset(ctx, UpdateAssetParams{
		ID:          pgID,
		Title:       titlePtr,
		Description: descPtr,
		Status:      statusPtr,
		Metadata:    metaJSON,
		Mature:      maturePtr,
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

	// #935. This path invalidated NOTHING — not even the posts cache
	// #920 wired into delete and restore. Editing an asset's title left
	// every post holding it, and its IIIF manifest, serving the old one
	// until the process restarted: the identical defect #920 fixed, on
	// the path nobody covered, and reachable by any owner clicking
	// Save.
	h.invalidateDerivedCaches(ctx, uuid.UUID(pgID.Bytes), "update")

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
	caller := auth.IdentityFromContext(ctx)
	if caller == nil || caller.IsAnonymous() {
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
	// Same gate as UpdateAsset, same helper, checked before the write.
	// The scope comes from the dedicated authorisation probe rather than
	// the read projection: the gate must answer for a row this caller
	// may have no entitlement to read, and borrowing the read query
	// would leave it one edit away from inheriting the read filters.
	// (GetAssetRow does carry team_id since #953 made it settable; that
	// is a projection for the client, not a substitute for this.)
	subject, err := q.GetAssetMutationSubject(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.DeleteAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: load mutation subject: %w", err)
	}
	if !canMutateAsset(caller, subject.OwnerUserRef, subject.TeamID) {
		return openapi.DeleteAsset403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "you may not delete this asset"},
		}, nil
	}
	reason := extractSoftDeleteReason(req.Body)
	if len(reason) > softDeleteReasonMaxLen {
		return openapi.DeleteAsset400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "reason exceeds 500 chars"},
		}, nil
	}
	// deleted_by_user_ref is what makes the delete undoable by the
	// person who did it (#931) — see auth.CanRestoreDeleted.
	deleter := caller.UserRef
	if err := q.SoftDeleteAsset(ctx, SoftDeleteAssetParams{
		ID:               pgID,
		DeletedReason:    softDeleteReasonPtr(reason),
		DeletedByUserRef: &deleter,
	}); err != nil {
		return nil, fmt.Errorf("assets: soft-delete: %w", err)
	}
	if h.Audit != nil {
		h.Audit.AdminAssetSoftDeleted(ctx, nil, uuid.UUID(pgID.Bytes).String(), caller.UserRef, reason)
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
	h.invalidateDerivedCaches(ctx, uuid.UUID(pgID.Bytes), "delete")
	return openapi.DeleteAsset204Response{}, nil
}

// InvalidateAfterRestore is invalidateDerivedCaches' "restore" case,
// exported for callers OUTSIDE this package that put an asset back.
//
// There is exactly one: the composition root's restorer adapter, which
// a granted restoration appeal (#931) goes through instead of
// RestoreAsset. Without this, an appeal would flip deleted_at and leave
// the asset missing from its posts and from the IIIF manifest until the
// process restarted — #920's bug, re-armed on a third path, and
// invisible from the 200 the decider sees.
//
// A hook rather than an export of the whole helper: the caller says
// "this asset is live again", not "run these two evictions", so the SET
// of evictions can grow here without every caller learning about it.
func (h *Handler) InvalidateAfterRestore(ctx context.Context, assetID uuid.UUID) {
	h.invalidateDerivedCaches(ctx, assetID, "restore")
}

// invalidateDerivedCaches evicts every OTHER domain's cached answer
// that this asset write just changed. An asset write touches only the
// asset row, but three caches key on something else entirely and none
// of them can see it happen:
//
//   - posts/ caches the whole rendered post, joined asset payloads
//     included, keyed on the POST (#920);
//   - iiif/presentation caches the manifest, which carries the asset's
//     title and description, keyed on its own domain (#935).
//
// Called from all three write paths — update, delete, restore —
// because all three change what those caches would answer:
//
//	PATCH    retitles the asset. Both caches serve the OLD title.
//	DELETE   removes it from posts (the join reads deleted_at) and
//	         from the manifest (the row predicate does).
//	RESTORE  is the same bug wearing the other hat: the copies were
//	         evicted on delete and repopulated WITHOUT the asset.
//
// The stale answer survives until the process restarts, which is what
// identifies all of this as invalidation rather than a read-rule gap —
// and what let #920 sit unnoticed on the delete path.
//
// Best-effort: the asset write has already committed and succeeded, so
// a cache failure is logged, not propagated. Same discipline as the
// storage-unpin step above. Both callees are nil-safe.
func (h *Handler) invalidateDerivedCaches(ctx context.Context, assetID uuid.UUID, op string) {
	if err := posts.InvalidateForAsset(ctx, h.registry, h.Pool, assetID); err != nil && h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.posts_cache.invalidate.error",
			slog.String("asset_id", assetID.String()),
			slog.String("op", op),
			slog.String("err", err.Error()),
		)
	}
	if err := presentation.InvalidateAssetOn(ctx, h.manifests, assetID); err != nil && h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "assets.manifest_cache.invalidate.error",
			slog.String("asset_id", assetID.String()),
			slog.String("op", op),
			slog.String("err", err.Error()),
		)
	}
}

// ---------------------------------------------------------------------------
// RestoreAsset — Phase 1.55.C-1b
// ---------------------------------------------------------------------------

// RestoreAsset clears deleted_at + deleted_reason on a soft-deleted
// asset. 404 if the asset is already live (or doesn't exist); 403 for
// a caller who didn't delete it and isn't system.admin; 401 for anon.
//
// Until #931 this was system.admin ONLY, while DeleteAsset was open to
// every authenticated user — anyone could remove a studio's library and
// nobody below super-admin could undo it. The gate is now
// auth.CanRestoreDeleted: you undo your own delete, system.admin
// undoes any.
// See that helper for why the rule turns on the DELETER rather than on
// the caller's standing authority.
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
	pgID := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	deletedBy, err := New(h.Pool).GetAssetDeletedBy(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Live or absent — the same 404 the restore itself gives
			// those two cases.
			return openapi.RestoreAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not soft-deleted"},
			}, nil
		}
		return nil, fmt.Errorf("assets: load deleted_by: %w", err)
	}
	if !auth.CanRestoreDeleted(id, deletedBy) {
		return openapi.RestoreAsset403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "this asset was deleted by someone else; ask an administrator to restore it",
			},
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
	// Restore is the same bug wearing the other hat: without this the
	// asset stays MISSING from its posts until a restart, because the
	// cached copies were evicted on delete and re-populated without it.
	h.invalidateDerivedCaches(ctx, uuid.UUID(req.Id), "restore")
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
	// ?liked_by= scopes the page to one user's likes (#1106). Narrowing
	// only, and no authorization decision here — the predicate and the
	// field plane still decide every row, and neither one reads `likes`.
	// See ListAssetsPageGatedParams.LikedByUserRef for the one thing it
	// DOES change: on this page an unreadable row is absent rather than
	// a placeholder.
	var likedBy *int64
	if req.Params.LikedBy != nil {
		likedBy = req.Params.LikedBy
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

	// ?team_id= scopes the page to one team's assets — the assets tab on
	// the team page (#684).
	//
	// No authorization decision is taken here, deliberately. There is no
	// membership check and no liveness probe: the visibility predicate
	// still selects the rows and the field plane still decides which of
	// them arrive as placeholders, and neither reads team_id. So the
	// filter can only ever REMOVE assets from the page this caller would
	// already have been served. A non-member browsing a studio sees its
	// public work and a wall of placeholders where its restricted work
	// is — exactly what unfiltered browse shows them, minus everyone
	// else's rows.
	//
	// It follows that this is not a team-existence probe either: an
	// unknown team, a soft-deleted team and an empty team are one
	// answer, an empty page.
	var teamFilter pgtype.UUID
	if req.Params.TeamId != nil {
		teamFilter = pgtype.UUID{Bytes: *req.Params.TeamId, Valid: true}
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
		TeamID:          teamFilter,
		LikedByUserRef:  likedBy,
		CursorCreatedAt: cursorTs,
		CursorID:        cursorID,
		RowLimit:        fetch,
		Ladder:          h.ladder(ctx),
		MutationCaps:    mutationCaps(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("assets: list: %w", err)
	}
	for i, r := range rows {
		if i >= int(limit) {
			break
		}
		// #899 — an asset whose columns this caller may not receive never
		// gets built into a full record at all. The tag fetch is skipped
		// with it: tags are asset columns, and a withheld row must not
		// spend a query gathering fields it will not emit.
		//
		// The decision was already made, once, by
		// visibility.FieldsReadable inside ListAssetsPageGated — the SAME
		// function GetAsset and the container surfaces use. This site
		// consumes it rather than re-deciding, because a second decision
		// is a second thing to keep in sync.
		if !r.Readable {
			assetsList = append(assetsList,
				withheldAsset(openapi_types.UUID(r.ID.Bytes), r.OwnerDisplayName))
			lastCreatedAt = r.CreatedAt.Time
			lastID = uuid.UUID(r.ID.Bytes)
			continue
		}
		tags, err := q.ListAssetTags(ctx, r.ID)
		if err != nil {
			return nil, fmt.Errorf("assets: list tags: %w", err)
		}
		a := rowToAsset(listRowToGetRow(r.ListAssetsPageRow), tags)
		a.PreviewAvailable = &r.PreviewAvailable
		a.LadderAvailable = &r.LadderAvailable
		a.ScrubAvailable = &r.ScrubAvailable
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

	// The at-a-glance strip + provenance for the whole page (#552), in two
	// queries rather than two per row. Runs last, on the rows already
	// chosen, because it is presentation: it cannot add, remove or reorder
	// an asset, and a failure here must not cost the caller their page.
	if err := h.decorateCards(ctx, assetsList); err != nil {
		return nil, fmt.Errorf("assets: card decoration: %w", err)
	}

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

// mutationCaps resolves the caller's asset-mutation capabilities for
// the READ gates (#939, ADR 0064) — the field plane a mutation holder
// is owed, never the bytes.
//
// Separate from contentCaller because it is a different plane and a
// different shape: contentCaller hands out a CapabilityChecker that
// answers GLOBAL codes only (`id.Can(code)` with no InTeam option), and
// `assets.admin` is team-scoped, so a scoped-only holder answers false
// through that checker. The team set has to travel as data.
//
// Anonymous resolves to the zero value, which permits nothing.
func mutationCaps(ctx context.Context) visibility.AssetMutationCaps {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return visibility.AssetMutationCaps{}
	}
	return visibility.ResolveAssetMutationCaps(
		func(code string) bool { return id.Can(code) },
		id.ScopedTeams(visibility.AssetsAdmin),
	)
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

// ptr returns a pointer to a copy of v. openapi.Asset's fields became
// pointers when #899 shrank the schema's `required` list to the two
// keys a withheld payload carries, so absence is expressible; this is
// the noise that buys it.
func ptr[T any](v T) *T { return &v }

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
		Title:            &row.Title,
		AssetType:        &row.AssetType,
		Status:           ptr(openapi.AssetStatus(row.Status)),
		ProcessingStatus: ptr(openapi.AssetProcessingStatus(row.ProcessingStatus)),
		CreatedAt:        &row.CreatedAt.Time,
		UpdatedAt:        &row.UpdatedAt.Time,
		Tags:             &tags,
		// #1115. A LABEL, not a gate: whether this viewer receives the
		// row at all, and whether its preview is blurred, are decided
		// server-side (ADR 0090 §3). This is here so a client can say
		// what it was given.
		Mature: &row.Mature,
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
	// #953 — the team an asset was created into. Absent, not null, when
	// it has none: the common case, and the same absence discipline the
	// rest of this projection uses.
	if row.TeamID.Valid {
		v := openapi_types.UUID(row.TeamID.Bytes)
		a.TeamId = &v
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
		a.Tags = &[]string{}
	}
	return a
}

// The CreateAsset/UpdateAsset/ListAsset return shapes have the same
// column set but distinct Go types (sqlc generates one per query).
// These helpers project them onto a common GetAssetRow we already
// have a marshaller for.
func rowToAssetRow(r CreateAssetRow) GetAssetRow {
	return GetAssetRow{
		TeamID:           r.TeamID,
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
		TeamID:           r.TeamID,
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
		TeamID:           r.TeamID,
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
// Type refs (all fourteen seeded in migration 00001):
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
