package aiedit

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	openapitypes "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// JobTypeImg2Img is the canonical job type identifier for img2img
// generation. Workers route to [Img2ImgJobHandler] on this string.
const JobTypeImg2Img jobs.JobType = "aiedit.img2img"

// AssetReader is the narrow surface the HTTP handler needs to
// validate the source asset before enqueueing. Consumer-defined so
// tests don't import the full assets package.
type AssetReader interface {
	// AssetTypeAndSensitivity returns the source asset's type code
	// (1 = image), sensitivity tier, and a found flag. Errors
	// surface DB issues only — "not found" returns (_, _, false, nil).
	AssetTypeAndSensitivity(ctx context.Context, id uuid.UUID) (assetType int64, sensitivity string, found bool, err error)
}

// AIEditConfigReader is the narrow surface for reading the
// operator-set MCP server name. *sysconfig.Store satisfies it via a
// thin adapter wired at boot.
type AIEditConfigReader interface {
	ImageEditServer(ctx context.Context) (string, error)
}

// JobEnqueuer is the narrow surface to enqueue an img2img job.
// *jobs.Service satisfies it.
type JobEnqueuer interface {
	Enqueue(ctx context.Context, t jobs.JobType, payload any, opts jobs.EnqueueOpts) (uuid.UUID, error)
}

// HTTPHandler wires the HTTP-side of the aiedit subsystem. One per
// app process; constructed at boot.
type HTTPHandler struct {
	assets   AssetReader
	cfg      AIEditConfigReader
	enqueuer JobEnqueuer
}

// NewHTTPHandler wires the handler.
func NewHTTPHandler(assets AssetReader, cfg AIEditConfigReader, enqueuer JobEnqueuer) *HTTPHandler {
	return &HTTPHandler{assets: assets, cfg: cfg, enqueuer: enqueuer}
}

// Img2ImgPayload is the JSON shape the job worker receives. Kept
// internal to the aiedit package — both producer (the HTTP handler)
// and consumer (the job handler) live here.
type Img2ImgPayload struct {
	SourceAssetID   uuid.UUID `json:"source_asset_id"`
	Prompt          string    `json:"prompt"`
	DenoiseStrength float64   `json:"denoise_strength,omitempty"`
	Steps           int       `json:"steps,omitempty"`
	Seed            int64     `json:"seed,omitempty"`

	// CallerUserRef + CallerCaps re-build the auth.Identity inside
	// the job worker so the dispatcher's capability + audit gates
	// see the same caller that submitted the request. Persisting
	// the caps onto the payload (rather than re-reading from the
	// user row at job run time) means the call uses the caps the
	// user HAD at submit time — operationally correct even if a
	// role grant is revoked mid-job. Caps on this payload are a
	// snapshot, not a live reference.
	CallerUserRef int64    `json:"caller_user_ref"`
	CallerCaps    []string `json:"caller_caps"`

	// Sensitivity is the source asset's tier at submit time. Same
	// snapshot semantics as the caps — if an asset is reclassified
	// while the job runs, the dispatcher's privacy gate uses the
	// submit-time tier (a tier downgrade mid-job doesn't suddenly
	// allow cloud routing for what was a restricted source).
	Sensitivity string `json:"sensitivity"`

	// ServerName is the operator-configured MCP server at submit
	// time. Snapshotted for the same reason — if the operator
	// re-points aiedit.image_edit_server mid-job, the job uses the
	// server it was authorised against.
	ServerName string `json:"server_name"`
}

// GenerateImg2ImgVariation — POST /assets/{id}/edit/img2img.
//
// Enqueues the async job + returns 202 + the job id. Synchronous
// rejections (no auth, missing config, non-image source) happen
// here; bridge errors surface inside the job.
func (h *HTTPHandler) GenerateImg2ImgVariation(
	ctx context.Context,
	req openapi.GenerateImg2ImgVariationRequestObject,
) (openapi.GenerateImg2ImgVariationResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GenerateImg2ImgVariation401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Prompt) == "" {
		return openapi.GenerateImg2ImgVariation400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "prompt is required"},
		}, nil
	}

	// Capability gate. mcp.client.use is the dispatcher umbrella;
	// mcp.client.images.write is the per-tool gate operators pin on
	// the img2img grant. We check both up-front so the synchronous
	// 403 is meaningful (vs an async dispatcher rejection 30
	// seconds into a queued job).
	if !id.Can("mcp.client.use") || !id.Can("mcp.client.images.write") {
		return openapi.GenerateImg2ImgVariation403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "mcp.client.use + mcp.client.images.write required",
			},
		}, nil
	}

	srcID := uuid.UUID(req.Id)
	assetType, sensitivity, found, err := h.assets.AssetTypeAndSensitivity(ctx, srcID)
	if err != nil {
		return nil, fmt.Errorf("aiedit: lookup source: %w", err)
	}
	if !found {
		return openapi.GenerateImg2ImgVariation404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "source asset not found"},
		}, nil
	}
	// asset_type=1 is the "image / photo" type per the seeded
	// asset_type rows. Future image-only types (illustrations,
	// stickers) would extend this list.
	if assetType != 1 {
		return openapi.GenerateImg2ImgVariation422JSONResponse{
			UnprocessableEntityJSONResponse: openapi.UnprocessableEntityJSONResponse{
				Error: ErrSourceNotImage.Error(),
			},
		}, nil
	}

	serverName, err := h.cfg.ImageEditServer(ctx)
	if err != nil {
		return nil, fmt.Errorf("aiedit: lookup config: %w", err)
	}
	if serverName == "" {
		return openapi.GenerateImg2ImgVariation409JSONResponse{
			ConflictJSONResponse: openapi.ConflictJSONResponse{Error: ErrServerNotConfigured.Error()},
		}, nil
	}

	// Snapshot the caller's caps onto the payload — see the
	// Img2ImgPayload doc for why.
	caps := make([]string, len(id.Capabilities))
	copy(caps, id.Capabilities)

	payload := Img2ImgPayload{
		SourceAssetID:   srcID,
		Prompt:          strings.TrimSpace(req.Body.Prompt),
		DenoiseStrength: ptrFloat(req.Body.DenoiseStrength),
		Steps:           ptrInt(req.Body.Steps),
		Seed:            ptrInt64(req.Body.Seed),
		CallerUserRef:   id.UserRef,
		CallerCaps:      caps,
		Sensitivity:     sensitivity,
		ServerName:      serverName,
	}

	jobID, err := h.enqueuer.Enqueue(ctx, JobTypeImg2Img, payload, jobs.EnqueueOpts{})
	if err != nil {
		return nil, fmt.Errorf("aiedit: enqueue job: %w", err)
	}

	return openapi.GenerateImg2ImgVariation202JSONResponse{
		JobId:         openapitypes.UUID(jobID),
		SourceAssetId: openapitypes.UUID(srcID),
	}, nil
}

func ptrFloat(p *float32) float64 {
	if p == nil {
		return 0
	}
	return float64(*p)
}
func ptrInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
func ptrInt64(p *int) int64 {
	if p == nil {
		return 0
	}
	return int64(*p)
}
