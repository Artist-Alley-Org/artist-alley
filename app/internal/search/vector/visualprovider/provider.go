package visualprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// Provider is the narrow surface the by-image handler + the visual
// embed job consume. Kept as an interface so tests inject a fake
// without spinning up the Python sidecar.
type Provider interface {
	// EmbedImage encodes one image (any format Pillow reads) into a
	// unit-length CLIP visual embedding. Returns the raw float
	// vector + the model metadata carried into the persistent row.
	EmbedImage(ctx context.Context, imageBytes []byte) (Embedding, error)
	// Health returns the last known health state. Called by the
	// admin dashboard and the by-image handler's fast-path gate.
	Health(ctx context.Context) (Health, error)
	// Info returns the sidecar's declared model / checkpoint / dim.
	// Boot uses this to reject a registration attempt when the dim
	// doesn't match the schema (768).
	Info() Info
}

// Embedding is the sidecar's response mapped into Go. Kept flat so
// the by-image handler can pass the vector straight through to
// pgvector without an intermediate DTO.
type Embedding struct {
	Vector     []float32
	Dim        int
	Model      string
	Checkpoint string
}

// Health captures the sidecar's readiness. Loading = model still
// warming up; OK = ready; Error = the sidecar returned 5xx or the
// HTTP call itself failed.
type Health struct {
	Status string // "ok" | "loading" | "error" | "unreachable"
	Error  string // populated when Status != "ok"
	Model  string
	Dim    int
}

// Info is the registration-time snapshot of the sidecar's config.
type Info struct {
	BaseURL    string
	Model      string
	Checkpoint string
	Dim        int
}

// ErrSidecarUnreachable is returned when the HTTP call to the
// sidecar failed at the network layer (connection refused, DNS
// miss, timeout). The by-image handler treats this as "provider
// unavailable" — the operator's sysconfig said the sidecar
// should be there, but it's not; falls back to the 501 stub
// response so the client isn't left hanging on a request that
// won't complete.
var ErrSidecarUnreachable = errors.New("visualprovider: sidecar unreachable")

// ErrDimMismatch fires when the sidecar returns an embedding whose
// dimension doesn't match the schema's expected 768. Refuses to
// write the row rather than corrupt the vector column.
var ErrDimMismatch = errors.New("visualprovider: dim mismatch — schema expects 768")

// LocalProvider is the production implementation talking to
// aa-clip-visual-local via HTTP. Composes a stdlib *http.Client with
// per-call context so callers can enforce their own budgets.
//
// Not concurrent-safe on the health cache field, but embed calls
// are safe (stateless).
type LocalProvider struct {
	BaseURL string
	Client  *http.Client
	info    Info
	// SchemaDim is the pgvector column dim (768 for asset_visual_embedding).
	// Cross-check on every embed prevents writing incompatible
	// vectors when someone points the provider at a wrong-dim
	// sidecar.
	SchemaDim int
}

// New constructs a LocalProvider. Callers set the base URL from
// sysconfig (search.visual.sidecar_url). Timeout is the per-request
// budget; 5s default matches the sysconfig knob.
func New(baseURL string, timeout time.Duration) *LocalProvider {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &LocalProvider{
		BaseURL:   baseURL,
		Client:    &http.Client{Timeout: timeout},
		SchemaDim: 768,
	}
}

// Bootstrap fetches the sidecar's /health + /version endpoints and
// populates the Info cache. Called once at boot BEFORE registration
// so a misconfigured sidecar doesn't accept requests it can't serve.
// Returns Health so the caller can decide whether to register.
func (p *LocalProvider) Bootstrap(ctx context.Context) (Health, error) {
	h, err := p.Health(ctx)
	if err != nil {
		return h, err
	}
	if h.Status != "ok" {
		return h, nil
	}
	if h.Dim != p.SchemaDim {
		return h, fmt.Errorf("%w: sidecar=%d schema=%d", ErrDimMismatch, h.Dim, p.SchemaDim)
	}
	p.info = Info{
		BaseURL:    p.BaseURL,
		Model:      h.Model,
		Checkpoint: "", // populated on the first successful embed; /health doesn't carry it
		Dim:        h.Dim,
	}
	return h, nil
}

// Info returns the cached sidecar metadata. Zero-value until
// Bootstrap succeeds.
func (p *LocalProvider) Info() Info { return p.info }

// Health probes the sidecar's /health endpoint.
func (p *LocalProvider) Health(ctx context.Context) (Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/health", nil)
	if err != nil {
		return Health{Status: "error", Error: err.Error()}, err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return Health{Status: "unreachable", Error: err.Error()}, fmt.Errorf("%w: %v", ErrSidecarUnreachable, err)
	}
	defer resp.Body.Close()
	var body struct {
		Status     string `json:"status"`
		Error      string `json:"error,omitempty"`
		Model      string `json:"model,omitempty"`
		Checkpoint string `json:"checkpoint,omitempty"`
		Dim        int    `json:"dim,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Health{Status: "error", Error: "unparseable health response: " + err.Error()}, err
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		// Sidecar returned 503 — either loading or errored. Both
		// paths carry an explicit status.
		if body.Status == "" {
			body.Status = "loading"
		}
	}
	if body.Status == "" {
		body.Status = "ok"
	}
	return Health{
		Status: body.Status,
		Error:  body.Error,
		Model:  body.Model,
		Dim:    body.Dim,
	}, nil
}

// EmbedImage POSTs the bytes as multipart/form-data to /embed/image
// + parses the response. Returns ErrSidecarUnreachable on network
// failure; returns a wrapped error for 4xx/5xx.
func (p *LocalProvider) EmbedImage(ctx context.Context, imageBytes []byte) (Embedding, error) {
	if len(imageBytes) == 0 {
		return Embedding{}, errors.New("visualprovider: empty image bytes")
	}
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	part, err := mw.CreateFormFile("file", "upload.bin")
	if err != nil {
		return Embedding{}, err
	}
	if _, err := part.Write(imageBytes); err != nil {
		return Embedding{}, err
	}
	if err := mw.Close(); err != nil {
		return Embedding{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/embed/image", buf)
	if err != nil {
		return Embedding{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := p.Client.Do(req)
	if err != nil {
		return Embedding{}, fmt.Errorf("%w: %v", ErrSidecarUnreachable, err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return Embedding{}, fmt.Errorf("visualprovider: sidecar %d: %s", resp.StatusCode, string(rawBody))
	}
	var body struct {
		Embedding  []float32 `json:"embedding"`
		Dim        int       `json:"dim"`
		Model      string    `json:"model"`
		Checkpoint string    `json:"checkpoint"`
	}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return Embedding{}, fmt.Errorf("visualprovider: unparseable embed response: %w", err)
	}
	if body.Dim != p.SchemaDim {
		return Embedding{}, fmt.Errorf("%w: sidecar=%d schema=%d", ErrDimMismatch, body.Dim, p.SchemaDim)
	}
	if len(body.Embedding) != body.Dim {
		return Embedding{}, fmt.Errorf("visualprovider: dim mismatch: claimed=%d actual=%d", body.Dim, len(body.Embedding))
	}
	// Cache checkpoint on first successful embed so subsequent
	// registrations don't need to re-issue an /embed.
	if p.info.Checkpoint == "" {
		p.info.Checkpoint = body.Checkpoint
	}
	return Embedding{
		Vector:     body.Embedding,
		Dim:        body.Dim,
		Model:      body.Model,
		Checkpoint: body.Checkpoint,
	}, nil
}
