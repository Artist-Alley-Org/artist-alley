// Provider unit tests. No live DB / network — the dispatcher seam
// stubs out the wire round-trip, so these are pure decode + error-
// path coverage.

package comfyuimcp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	mcpdispatch "github.com/mscrnt/artist-alley/app/internal/ai/mcp_dispatch"
	"github.com/mscrnt/artist-alley/app/internal/aiedit"
	"github.com/mscrnt/artist-alley/app/internal/aiedit/providers/comfyuimcp"
)

// stubDispatcher records the InvokeOpts it received + returns a
// canned response (or error). Tests inspect captured state to
// assert the provider forwarded args correctly.
type stubDispatcher struct {
	gotOpts mcpdispatch.InvokeOpts
	resp    json.RawMessage
	err     error
}

func (s *stubDispatcher) Invoke(ctx context.Context, opts mcpdispatch.InvokeOpts) (json.RawMessage, error) {
	s.gotOpts = opts
	return s.resp, s.err
}

// cannedImageResponse builds an MCP-spec content-array response
// with an "image" part + a "text" part carrying the metadata JSON.
func cannedImageResponse(t *testing.T, imageBytes []byte, meta map[string]any) json.RawMessage {
	t.Helper()
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	body := map[string]any{
		"content": []map[string]any{
			{"type": "image", "data": base64.StdEncoding.EncodeToString(imageBytes), "mimeType": "image/png"},
			{"type": "text", "text": string(metaJSON)},
		},
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return out
}

func TestProvider_Capabilities_OnlyImg2Img(t *testing.T) {
	p := comfyuimcp.NewProvider(&stubDispatcher{}, "comfyui-lan")
	caps := p.Capabilities()
	if !caps.Img2Img {
		t.Errorf("Img2Img cap should be true")
	}
	if caps.InPaint || caps.OutPaint || caps.Variations || caps.RemoveBackground {
		t.Errorf("E-1 must NOT advertise E-2 ops: %+v", caps)
	}
}

func TestProvider_Img2Img_HappyPath_DecodesBytesAndMetadata(t *testing.T) {
	imageBytes := []byte("\x89PNG\r\n\x1a\nfake-png-bytes")
	stub := &stubDispatcher{
		resp: cannedImageResponse(t, imageBytes, map[string]any{
			"prompt":    "watercolour sketch",
			"seed_used": 42,
			"model":     "sdxl",
			"steps":     20,
		}),
	}
	p := comfyuimcp.NewProvider(stub, "comfyui-lan")

	srcID := uuid.New()
	out, err := p.Img2Img(context.Background(), aiedit.Img2ImgRequest{
		SourceAssetID:   srcID,
		SourceImageURL:  "http://aa.lan/file/abc",
		Prompt:          "watercolour sketch",
		DenoiseStrength: 0.65,
		Steps:           20,
		Seed:            42,
	})
	if err != nil {
		t.Fatalf("Img2Img: %v", err)
	}

	// Dispatcher saw the right server + tool + args.
	if stub.gotOpts.ServerName != "comfyui-lan" {
		t.Errorf("ServerName = %q, want comfyui-lan", stub.gotOpts.ServerName)
	}
	if stub.gotOpts.Tool != comfyuimcp.ToolImg2Img {
		t.Errorf("Tool = %q, want %q", stub.gotOpts.Tool, comfyuimcp.ToolImg2Img)
	}
	if stub.gotOpts.AssetID != srcID {
		t.Errorf("AssetID = %v, want %v", stub.gotOpts.AssetID, srcID)
	}
	if got := stub.gotOpts.Arguments["prompt"]; got != "watercolour sketch" {
		t.Errorf("prompt arg = %v, want watercolour sketch", got)
	}
	if got, ok := stub.gotOpts.Arguments["denoise_strength"].(float64); !ok || got != 0.65 {
		t.Errorf("denoise_strength arg = %v (%T), want 0.65", got, stub.gotOpts.Arguments["denoise_strength"])
	}

	// Bytes round-trip.
	if string(out.ImageBytes) != string(imageBytes) {
		t.Errorf("ImageBytes mismatch")
	}
	if out.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", out.ContentType)
	}
	// Metadata decoded.
	if out.SeedUsed != 42 {
		t.Errorf("SeedUsed = %d, want 42", out.SeedUsed)
	}
	if out.PromptUsed != "watercolour sketch" {
		t.Errorf("PromptUsed = %q", out.PromptUsed)
	}
	if out.ModelUsed != "sdxl" {
		t.Errorf("ModelUsed = %q, want sdxl", out.ModelUsed)
	}
	if out.GenerationMetadata["model"] != "sdxl" {
		t.Errorf("GenerationMetadata not populated: %+v", out.GenerationMetadata)
	}
}

func TestProvider_Img2Img_ZeroValueKnobs_NotForwarded(t *testing.T) {
	stub := &stubDispatcher{
		resp: cannedImageResponse(t, []byte("png"), nil),
	}
	p := comfyuimcp.NewProvider(stub, "comfyui-lan")
	_, err := p.Img2Img(context.Background(), aiedit.Img2ImgRequest{
		SourceImageURL: "http://x/img",
		Prompt:         "x",
		// All knobs zero — should NOT appear in args so bridge
		// applies its own defaults.
	})
	if err != nil {
		t.Fatalf("Img2Img: %v", err)
	}
	for _, k := range []string{"denoise_strength", "steps", "seed"} {
		if _, ok := stub.gotOpts.Arguments[k]; ok {
			t.Errorf("zero-value knob %q should not have been forwarded; got %v", k, stub.gotOpts.Arguments[k])
		}
	}
}

func TestProvider_Img2Img_ServerNotConfigured_ReturnsErr(t *testing.T) {
	p := comfyuimcp.NewProvider(&stubDispatcher{}, "")
	_, err := p.Img2Img(context.Background(), aiedit.Img2ImgRequest{Prompt: "x"})
	if !errors.Is(err, aiedit.ErrServerNotConfigured) {
		t.Errorf("got %v, want ErrServerNotConfigured", err)
	}
}

func TestProvider_Img2Img_DispatcherError_Propagates(t *testing.T) {
	stub := &stubDispatcher{err: mcpdispatch.ErrPrivacyBlocked}
	p := comfyuimcp.NewProvider(stub, "comfyui-lan")
	_, err := p.Img2Img(context.Background(), aiedit.Img2ImgRequest{Prompt: "x"})
	if !errors.Is(err, mcpdispatch.ErrPrivacyBlocked) {
		t.Errorf("got %v, want ErrPrivacyBlocked propagated unchanged", err)
	}
}

func TestProvider_Img2Img_MalformedResponse_NoImage_ReturnsErr(t *testing.T) {
	// Response with only a text part — missing image content.
	body := `{"content":[{"type":"text","text":"no image here"}]}`
	stub := &stubDispatcher{resp: json.RawMessage(body)}
	p := comfyuimcp.NewProvider(stub, "comfyui-lan")
	_, err := p.Img2Img(context.Background(), aiedit.Img2ImgRequest{Prompt: "x"})
	if !errors.Is(err, aiedit.ErrBridgeResponseMalformed) {
		t.Errorf("got %v, want ErrBridgeResponseMalformed", err)
	}
}

func TestProvider_Img2Img_MalformedResponse_BadBase64_ReturnsErr(t *testing.T) {
	body := `{"content":[{"type":"image","data":"@@@not-base64@@@","mimeType":"image/png"}]}`
	stub := &stubDispatcher{resp: json.RawMessage(body)}
	p := comfyuimcp.NewProvider(stub, "comfyui-lan")
	_, err := p.Img2Img(context.Background(), aiedit.Img2ImgRequest{Prompt: "x"})
	if !errors.Is(err, aiedit.ErrBridgeResponseMalformed) {
		t.Errorf("got %v, want ErrBridgeResponseMalformed", err)
	}
}

func TestProvider_Img2Img_MalformedResponse_EmptyContent_ReturnsErr(t *testing.T) {
	body := `{"content":[]}`
	stub := &stubDispatcher{resp: json.RawMessage(body)}
	p := comfyuimcp.NewProvider(stub, "comfyui-lan")
	_, err := p.Img2Img(context.Background(), aiedit.Img2ImgRequest{Prompt: "x"})
	if !errors.Is(err, aiedit.ErrBridgeResponseMalformed) {
		t.Errorf("got %v, want ErrBridgeResponseMalformed", err)
	}
}

func TestProvider_Img2Img_MissingMetadata_StillSucceeds(t *testing.T) {
	// Response with just an image — no text metadata. The provider
	// returns the bytes; SeedUsed/PromptUsed/ModelUsed stay zero.
	imageBytes := []byte("png-bytes")
	body, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "image", "data": base64.StdEncoding.EncodeToString(imageBytes), "mimeType": "image/png"},
		},
	})
	stub := &stubDispatcher{resp: body}
	p := comfyuimcp.NewProvider(stub, "comfyui-lan")
	out, err := p.Img2Img(context.Background(), aiedit.Img2ImgRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Img2Img: %v", err)
	}
	if string(out.ImageBytes) != string(imageBytes) {
		t.Errorf("bytes mismatch")
	}
	if out.SeedUsed != 0 || out.PromptUsed != "" || out.ModelUsed != "" {
		t.Errorf("missing metadata should leave typed fields zero, got seed=%d prompt=%q model=%q",
			out.SeedUsed, out.PromptUsed, out.ModelUsed)
	}
}

func TestProvider_OtherOps_ReturnUnsupportedOp(t *testing.T) {
	p := comfyuimcp.NewProvider(&stubDispatcher{}, "comfyui-lan")
	cases := []struct {
		name string
		call func() error
	}{
		{"InPaint", func() error {
			_, err := p.InPaint(context.Background(), aiedit.InPaintRequest{})
			return err
		}},
		{"OutPaint", func() error {
			_, err := p.OutPaint(context.Background(), aiedit.OutPaintRequest{})
			return err
		}},
		{"Variations", func() error {
			_, err := p.Variations(context.Background(), aiedit.VariationsRequest{})
			return err
		}},
		{"RemoveBackground", func() error {
			_, err := p.RemoveBackground(context.Background(), aiedit.RemoveBgRequest{})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if !errors.Is(err, aiedit.ErrUnsupportedOp) {
				t.Errorf("%s err = %v, want ErrUnsupportedOp", c.name, err)
			}
		})
	}
}

func TestProvider_Name(t *testing.T) {
	p := comfyuimcp.NewProvider(&stubDispatcher{}, "comfyui-lan")
	if p.Name() != comfyuimcp.Name {
		t.Errorf("Name = %q, want %q", p.Name(), comfyuimcp.Name)
	}
	if !strings.HasPrefix(p.Name(), "comfy") {
		t.Errorf("provider name should describe the backend: %q", p.Name())
	}
}
