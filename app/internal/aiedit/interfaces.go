package aiedit

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ImageEditProvider is the contract every backend implements. The
// surface ships ALL ops in 1.14.E-1; only [Img2Img] has a real
// implementation. The other four ops return [ErrUnsupportedOp] from
// every provider in this PR — 1.14.E-2 flips them as implementations
// land. Stable interface from day one means future commits add
// implementations, never reshape the call sites.
//
// Implementations must be safe for concurrent use across many
// goroutines (the job worker calls into them concurrently).
type ImageEditProvider interface {
	// Name returns the stable identifier the audit + dispatch
	// layers record (e.g. "comfyuimcp").
	Name() string

	// Capabilities reports which ops the concrete provider supports.
	// The viewer queries this to decide which buttons to render in
	// the Creative tools panel (E-2); E-1's button only fires if
	// Capabilities().Img2Img is true.
	Capabilities() Capabilities

	// Img2Img generates a variation of the source image guided by a
	// prompt. The only op implemented in 1.14.E-1.
	Img2Img(ctx context.Context, req Img2ImgRequest) (Img2ImgResult, error)

	// InPaint regenerates the masked region. E-2 — returns
	// [ErrUnsupportedOp] in E-1.
	InPaint(ctx context.Context, req InPaintRequest) (Img2ImgResult, error)

	// OutPaint extends the canvas. E-2 — returns [ErrUnsupportedOp]
	// in E-1.
	OutPaint(ctx context.Context, req OutPaintRequest) (Img2ImgResult, error)

	// Variations returns N variations of the source without a
	// prompt. E-2 — returns [ErrUnsupportedOp] in E-1.
	Variations(ctx context.Context, req VariationsRequest) (VariationsResult, error)

	// RemoveBackground returns the source with the background
	// removed. E-2 — returns [ErrUnsupportedOp] in E-1.
	RemoveBackground(ctx context.Context, req RemoveBgRequest) (Img2ImgResult, error)
}

// Capabilities reports which ops a provider implements. Bool fields
// rather than a bitmask because the set is small + named — a viewer
// switch on `if caps.Img2Img` reads better than bitfield masking.
type Capabilities struct {
	Img2Img          bool
	InPaint          bool
	OutPaint         bool
	Variations       bool
	RemoveBackground bool
}

// Img2ImgRequest is the input shape every Img2Img-capable provider
// accepts. Provider-specific knobs that don't fit here (ComfyUI's
// custom workflow name, OpenAI's quality setting) ride in the
// dispatcher's [InvokeOpts.Arguments] map via provider-side wrapping
// rather than polluting this typed surface.
type Img2ImgRequest struct {
	// SourceAssetID is the asset the variation derives from. The
	// provider resolves the bytes via [SourceImageURL] — passing the
	// ID here lets the audit + lineage layers cross-reference.
	SourceAssetID uuid.UUID

	// SourceImageBytes is the raw source-image bytes. The provider
	// base64-encodes them into the MCP tool args so the bridge
	// doesn't need credentials to fetch the source independently.
	//
	// Tradeoff: payload size grows by ~33%. For typical asset sizes
	// (1-10 MB) that's still well under MCP server limits; for
	// larger sources we may switch to presigned-URL handoff later
	// (E-2 work; FS backend would need a temporary-signed-URL
	// shim).
	SourceImageBytes []byte

	// SourceContentType describes [SourceImageBytes] (e.g.
	// "image/png"). The bridge uses it to pick the right ComfyUI
	// loader node.
	SourceContentType string

	// Prompt steers the variation. Empty is allowed for providers
	// that support unconditional re-rendering; ComfyUI treats empty
	// as "minor variation, no semantic change".
	Prompt string

	// DenoiseStrength in [0, 1] — 0 = no change, 1 = ignore source
	// entirely (effectively txt2img). ComfyUI default is 0.7. Zero
	// value here means "use provider default".
	DenoiseStrength float64

	// Steps caps inference steps. Provider-specific defaults apply
	// when 0; ComfyUI bridge defaults to 20.
	Steps int

	// Seed for reproducibility. 0 = random seed; the provider
	// returns the actual seed used in [Img2ImgResult.SeedUsed].
	Seed int64
}

// Img2ImgResult is the typed return shape. Provider-specific
// metadata (model name, sampler, custom node info) ride in
// [GenerationMetadata] for the lineage row.
type Img2ImgResult struct {
	// ImageBytes is the generated image's raw bytes. The handler
	// streams these through [storage.Service.UploadOriginal] to mint
	// a derivative asset.
	ImageBytes []byte

	// ContentType — usually "image/png". Provider must set;
	// downstream callers do NOT sniff.
	ContentType string

	// SeedUsed is the seed the provider actually rolled with — for
	// reproducibility ("regenerate with this prompt + seed").
	SeedUsed int64

	// PromptUsed echoes the prompt; redundant on success but
	// useful when the provider rewrote it (e.g. safety filters).
	PromptUsed string

	// ModelUsed identifies the underlying model (e.g.
	// "stable-diffusion-xl"). Recorded in lineage metadata.
	ModelUsed string

	// GenerationMetadata is the provider-specific knob bag (sampler,
	// CFG, custom node values, …). JSON-serialisable; gets persisted
	// into creative_lineage.generation_metadata alongside the typed
	// fields above.
	GenerationMetadata map[string]any
}

// InPaintRequest extends Img2ImgRequest with a mask. E-2.
type InPaintRequest struct {
	Img2ImgRequest

	// MaskImageURL points to a single-channel PNG where opaque
	// pixels mark "regenerate this area" and transparent pixels
	// mark "preserve". E-2 will source this from the viewer's mask
	// drawing tool.
	MaskImageURL string
}

// OutPaintRequest extends Img2ImgRequest with canvas-extension
// directions. E-2.
type OutPaintRequest struct {
	Img2ImgRequest

	// ExtendPixels expands the canvas by this many pixels per side
	// (top, right, bottom, left). Zero values leave that side
	// untouched.
	ExtendPixelsTop    int
	ExtendPixelsRight  int
	ExtendPixelsBottom int
	ExtendPixelsLeft   int
}

// VariationsRequest asks for N variations of the source with no
// prompt steering. E-2.
type VariationsRequest struct {
	SourceAssetID  uuid.UUID
	SourceImageURL string
	Count          int   // 1-4 typical; provider may cap further
	Seed           int64 // base seed; provider rolls Seed, Seed+1, ...
}

// VariationsResult bundles N image results.
type VariationsResult struct {
	Images []Img2ImgResult
}

// RemoveBgRequest extracts the foreground. E-2.
type RemoveBgRequest struct {
	SourceAssetID  uuid.UUID
	SourceImageURL string
}

// ---------------------------------------------------------------------------
// Error sentinels
// ---------------------------------------------------------------------------

// ErrUnsupportedOp is what every E-2 op returns from every provider
// in 1.14.E-1. The job handler classifies this as a permanent error
// (no retry); the viewer hides buttons for ops that don't show up
// in [Capabilities].
var ErrUnsupportedOp = errors.New("aiedit: operation not implemented by this provider")

// ErrSourceNotImage is returned by the HTTP handler when the caller
// invokes an image-edit op on a non-image asset (audio, video, doc,
// archive, …). Maps to HTTP 400.
var ErrSourceNotImage = errors.New("aiedit: source asset is not an image")

// ErrServerNotConfigured is returned when system_config aiedit
// settings don't name an MCP server to dispatch to. Maps to HTTP
// 409 (Conflict — server-side configuration mismatch). The operator
// remediates via the admin UI.
var ErrServerNotConfigured = errors.New("aiedit: no MCP image-edit server configured")

// ErrBridgeResponseMalformed is what providers wrap around a bridge
// response that doesn't satisfy the locked contract (missing image
// content, base64 decode failure, …). Maps to a transient error at
// the job layer so the worker retries — bridges sometimes get
// upgraded mid-call and a single malformed response shouldn't fail
// the job permanently.
var ErrBridgeResponseMalformed = errors.New("aiedit: MCP bridge response did not satisfy the img2img contract")
