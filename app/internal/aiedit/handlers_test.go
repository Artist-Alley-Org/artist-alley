// HTTP handler integration tests. Use stub adapters (consumer-
// defined interfaces from handlers.go) so no live DB is needed —
// the handler itself is pure dispatch + validation. The full
// asset / sysconfig integration is verified by the boot wire +
// the end-to-end smoke at the OpenAPI level.

package aiedit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	openapitypes "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/aiedit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// stubAssets implements aiedit.AssetReader. Returns the configured
// asset type / sensitivity / found / err in order.
type stubAssets struct {
	assetType   int64
	sensitivity string
	found       bool
	err         error
}

func (s stubAssets) AssetTypeAndSensitivity(
	_ context.Context, _ uuid.UUID,
) (int64, string, bool, error) {
	return s.assetType, s.sensitivity, s.found, s.err
}

// stubConfig implements aiedit.AIEditConfigReader.
type stubConfig struct {
	server string
	err    error
}

func (s stubConfig) ImageEditServer(_ context.Context) (string, error) {
	return s.server, s.err
}

// stubEnqueuer implements aiedit.JobEnqueuer + records the call.
type stubEnqueuer struct {
	gotType    jobs.JobType
	gotPayload aiedit.Img2ImgPayload
	retID      uuid.UUID
	retErr     error
}

func (s *stubEnqueuer) Enqueue(_ context.Context, t jobs.JobType, payload any, _ jobs.EnqueueOpts) (uuid.UUID, error) {
	s.gotType = t
	if p, ok := payload.(aiedit.Img2ImgPayload); ok {
		s.gotPayload = p
	}
	return s.retID, s.retErr
}

func newHandler(assets aiedit.AssetReader, cfg aiedit.AIEditConfigReader, enq aiedit.JobEnqueuer) *aiedit.HTTPHandler {
	return aiedit.NewHTTPHandler(assets, cfg, enq)
}

func authedCtx(caps ...string) context.Context {
	id := &auth.Identity{UserRef: 7, AuthMethod: "session", Capabilities: caps}
	return auth.WithIdentity(context.Background(), id)
}

func reqWithPrompt(p string) openapi.GenerateImg2ImgVariationRequestObject {
	return openapi.GenerateImg2ImgVariationRequestObject{
		Id: openapitypes.UUID(uuid.New()),
		Body: &openapi.Img2ImgRequest{
			Prompt: p,
		},
	}
}

func TestImg2ImgHandler_NoAuth_Returns401(t *testing.T) {
	h := newHandler(stubAssets{}, stubConfig{}, &stubEnqueuer{})
	resp, err := h.GenerateImg2ImgVariation(context.Background(), reqWithPrompt("hi"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, ok := resp.(openapi.GenerateImg2ImgVariation401JSONResponse); !ok {
		t.Errorf("got %T, want 401", resp)
	}
}

func TestImg2ImgHandler_MissingPrompt_Returns400(t *testing.T) {
	h := newHandler(stubAssets{}, stubConfig{}, &stubEnqueuer{})
	resp, err := h.GenerateImg2ImgVariation(
		authedCtx("mcp.client.use", "mcp.client.images.write"),
		reqWithPrompt("   "),
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, ok := resp.(openapi.GenerateImg2ImgVariation400JSONResponse); !ok {
		t.Errorf("got %T, want 400", resp)
	}
}

func TestImg2ImgHandler_MissingCap_Returns403(t *testing.T) {
	h := newHandler(stubAssets{}, stubConfig{}, &stubEnqueuer{})
	resp, err := h.GenerateImg2ImgVariation(
		authedCtx("mcp.client.use"), // missing mcp.client.images.write
		reqWithPrompt("hi"),
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, ok := resp.(openapi.GenerateImg2ImgVariation403JSONResponse); !ok {
		t.Errorf("got %T, want 403", resp)
	}
}

func TestImg2ImgHandler_AssetNotFound_Returns404(t *testing.T) {
	h := newHandler(stubAssets{found: false}, stubConfig{server: "comfyui-lan"}, &stubEnqueuer{})
	resp, err := h.GenerateImg2ImgVariation(
		authedCtx("mcp.client.use", "mcp.client.images.write"),
		reqWithPrompt("hi"),
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, ok := resp.(openapi.GenerateImg2ImgVariation404JSONResponse); !ok {
		t.Errorf("got %T, want 404", resp)
	}
}

func TestImg2ImgHandler_NonImageAsset_Returns422(t *testing.T) {
	h := newHandler(
		stubAssets{found: true, assetType: 4, sensitivity: "public"}, // 4 = audio
		stubConfig{server: "comfyui-lan"},
		&stubEnqueuer{},
	)
	resp, err := h.GenerateImg2ImgVariation(
		authedCtx("mcp.client.use", "mcp.client.images.write"),
		reqWithPrompt("hi"),
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, ok := resp.(openapi.GenerateImg2ImgVariation422JSONResponse); !ok {
		t.Errorf("got %T, want 422", resp)
	}
}

func TestImg2ImgHandler_ServerNotConfigured_Returns409(t *testing.T) {
	h := newHandler(
		stubAssets{found: true, assetType: 1, sensitivity: "public"},
		stubConfig{server: ""}, // empty = disabled
		&stubEnqueuer{},
	)
	resp, err := h.GenerateImg2ImgVariation(
		authedCtx("mcp.client.use", "mcp.client.images.write"),
		reqWithPrompt("hi"),
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, ok := resp.(openapi.GenerateImg2ImgVariation409JSONResponse); !ok {
		t.Errorf("got %T, want 409", resp)
	}
}

func TestImg2ImgHandler_HappyPath_Returns202_WithJobAndSourceIDs(t *testing.T) {
	jobID := uuid.New()
	enq := &stubEnqueuer{retID: jobID}
	h := newHandler(
		stubAssets{found: true, assetType: 1, sensitivity: "restricted"},
		stubConfig{server: "comfyui-lan"},
		enq,
	)

	req := reqWithPrompt("watercolour sketch")
	resp, err := h.GenerateImg2ImgVariation(
		authedCtx("mcp.client.use", "mcp.client.images.write"),
		req,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	ok, accept := resp.(openapi.GenerateImg2ImgVariation202JSONResponse)
	if !accept {
		t.Fatalf("got %T, want 202", resp)
	}
	if openapi.AssetEditJobAccepted(ok).JobId != openapitypes.UUID(jobID) {
		t.Errorf("JobId = %v, want %v", openapi.AssetEditJobAccepted(ok).JobId, jobID)
	}
	if openapi.AssetEditJobAccepted(ok).SourceAssetId != req.Id {
		t.Errorf("SourceAssetId = %v, want %v", openapi.AssetEditJobAccepted(ok).SourceAssetId, req.Id)
	}

	// Payload validation — the enqueuer captured the right job
	// type + caller snapshot + sensitivity + server name.
	if enq.gotType != aiedit.JobTypeImg2Img {
		t.Errorf("job type = %q, want %q", enq.gotType, aiedit.JobTypeImg2Img)
	}
	if enq.gotPayload.CallerUserRef != 7 {
		t.Errorf("CallerUserRef = %d, want 7", enq.gotPayload.CallerUserRef)
	}
	if enq.gotPayload.Sensitivity != "restricted" {
		t.Errorf("Sensitivity = %q, want restricted", enq.gotPayload.Sensitivity)
	}
	if enq.gotPayload.ServerName != "comfyui-lan" {
		t.Errorf("ServerName = %q, want comfyui-lan", enq.gotPayload.ServerName)
	}
	if enq.gotPayload.Prompt != "watercolour sketch" {
		t.Errorf("Prompt = %q, want 'watercolour sketch'", enq.gotPayload.Prompt)
	}
	wantCaps := []string{"mcp.client.use", "mcp.client.images.write"}
	if len(enq.gotPayload.CallerCaps) != len(wantCaps) {
		t.Errorf("CallerCaps len = %d, want %d", len(enq.gotPayload.CallerCaps), len(wantCaps))
	}
}
