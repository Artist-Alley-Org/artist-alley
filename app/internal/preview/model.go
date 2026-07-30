// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	mview "github.com/mscrnt/mviewer/go"
	xdraw "golang.org/x/image/draw"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/preview/format3d"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// ModelResult — what the worker writes back to jobs.result for the
// admin queue view.
type ModelResult struct {
	Variants   []string `json:"variants"`
	Skipped    []string `json:"skipped"`
	FrameCount int      `json:"frame_count"`
	WorkS      float64  `json:"work_s"`
}

// ModelHandler renders an N-frame turntable of a 3D asset via the
// headless three.js worker (Chromium + SwiftShader), fans the poster
// into the standard raster ladder (col / preview / screen / hires),
// and composes the frames into a sprite sheet + WebVTT cues that the
// existing hover-scrub UI already understands (same scheme as
// preview.video sprites).
//
// Blender left the image entirely in #500 (ADR 0069 amended): the
// worker is the only renderer, and it drives the same three.js loaders
// as the interactive viewer, so the thumbnail and the live view match
// by construction. Formats no loader handles produce no turntable —
// see the renderable-format gate in Handle.
//
// Idempotent: every output is checked against the storage backend
// before re-rendering. A re-queued job only fills the gaps.
type ModelHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	TempDir string

	// Conservative: 3D source files larger than this are rejected.
	// 500 MB is plenty for a single dense asset; whole-studio bundles
	// belong somewhere else.
	MaxSourceBytes int64

	// Per-job wallclock cap. A three.js turntable is seconds rather than
	// minutes, but Chromium launch plus a pathological mesh can drag;
	// cap at 15 min so one job can't hold a worker forever.
	MaxJobDuration time.Duration

	// Turntable resolution + frame count. The defaults give a 36-frame
	// loop at 10° steps (6×6 sprite grid).
	Frames int
	Res    int

	// three.js worker (#498, ADR 0069). NodePath / ThreeJSScript override
	// the executable + script lookups (empty = `node` from PATH /
	// /app/threejs/worker.mjs, the container layout).
	NodePath      string
	ThreeJSScript string
	PosterRes     int
}

// NewModelHandler — recommended constructor with sensible defaults.
func NewModelHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *ModelHandler {
	return &ModelHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 500 * 1024 * 1024,
		MaxJobDuration: 15 * time.Minute,
		Frames:         36,
		Res:            512,
		PosterRes:      2048,
	}
}

func (h *ModelHandler) Type() jobs.JobType { return jobs.TypePreview3D }

// format3dExts is the set of formats served by the in-tree
// preview/format3d Go importer. We convert these to .glb up-front and
// then fall through to the standard turntable, same dance .mview
// already does — the render step reads the post-conversion extension,
// so both land on the three.js worker. Keep in sync with the
// per-format Decode* funcs in the format3d package.
var format3dExts = map[string]format3dDecoder{
	"md2":  format3d.DecodeMD2,
	"md3":  format3d.DecodeMD3,
	"mdl":  format3d.DecodeMDL,
	"ms3d": format3d.DecodeMS3D,
}

type format3dDecoder func(io.Reader) (*format3d.Model, error)

func isModelExt(ext string) bool {
	_, ok := dispatch.ModelExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}

// ---------------------------------------------------------------------------
// Handle
// ---------------------------------------------------------------------------

func (h *ModelHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()
	var p ModelPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.model: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.model: file_hash is required")}
	}
	if !isModelExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.model: extension %q is not a supported 3D format", p.FileExtension)}
	}

	jobCtx, cancel := context.WithTimeout(ctx, h.MaxJobDuration)
	defer cancel()

	h.markProcessing(jobCtx, p.AssetID)

	work, cleanup, err := h.stage(jobCtx, p.FileHash, p.FileExtension)
	if err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}
	defer cleanup()

	// Materialise companions next to the model file at their declared
	// relative paths. three.js's loaders resolve external resources
	// (MTL files, .bin buffers, sibling textures) relative to the
	// loaded file's directory — so as long as the user attached the
	// right files via the companion API, the chain just works:
	//   workdir/src.obj           ← original
	//   workdir/character-a.mtl   ← companion at path 'character-a.mtl'
	//   workdir/Textures/*.png    ← companion at path 'Textures/*'
	// Soft-fail: a download error logs a warning + skips the missing
	// file rather than aborting the whole render. The asset still gets
	// untextured thumbnails, which beats no thumbnails at all.
	if err := h.stageCompanions(jobCtx, p.AssetID, work.dir); err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.model.companions_stage_failed",
			slog.String("asset_id", p.AssetID.String()),
			slog.String("err", err.Error()))
	}

	// .mview gets two passes:
	//   1. Best-effort thumbnail extract — always grabs the
	//      embedded thumbnail.jpg so the col variant exists even
	//      if the GLB conversion fails.
	//   2. In-process convert to GLB. On success we swap sourcePath
	//      to the .glb so the standard turntable below picks it up;
	//      the conversion gives us real turntable + reference views
	//      matching the rest of the 3D pipeline.
	// A conversion failure is non-fatal: the safety-net thumbnail
	// already populated the raster ladder, marmoset.js still serves
	// the live view, and we just skip the turntable.
	// format3d (MD2 / MD3 / MDL / MS3D) — in-tree Go importer. Same
	// shape as .mview: convert to .glb in the workdir, swap sourcePath,
	// fall through to the standard turntable. Soft-fail: a parse
	// failure logs and skips the turntable (raster ladder still
	// works since the source is just bytes).
	if dec, ok := format3dExts[strings.ToLower(strings.TrimPrefix(p.FileExtension, "."))]; ok {
		glbPath, err := h.convertFormat3D(jobCtx, work.sourcePath, dec)
		if err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.model.format3d_convert_failed",
				slog.String("asset_id", p.AssetID.String()),
				slog.String("ext", p.FileExtension),
				slog.String("err", err.Error()))
			h.markReady(jobCtx, p.AssetID)
			return json.Marshal(ModelResult{
				FrameCount: 0,
				WorkS:      time.Since(started).Seconds(),
			})
		}
		work.sourcePath = glbPath
	}

	if strings.EqualFold(strings.TrimPrefix(p.FileExtension, "."), "mview") {
		h.extractMviewThumbBestEffort(jobCtx, p.AssetID, p.FileHash, work.sourcePath, p.Force)
		glbPath, err := h.convertMviewToGLB(jobCtx, work.sourcePath)
		if err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.model.mview_convert_failed",
				slog.String("asset_id", p.AssetID.String()),
				slog.String("err", err.Error()))
			h.markReady(jobCtx, p.AssetID)
			return json.Marshal(ModelResult{
				FrameCount: 0,
				Variants:   []string{"raster"},
				WorkS:      time.Since(started).Seconds(),
			})
		}
		work.sourcePath = glbPath
	}

	result := ModelResult{FrameCount: h.Frames}

	// Cheap re-queue path: if every output we'd produce is already on
	// storage we have nothing to do. Sentinel set = the four anchor
	// variants whose presence guarantees the whole bundle landed in a
	// previous run (raster ladder + sprite sheet + per-frame CLIP set
	// + reference views).
	posterDone := variantDone(jobCtx, h.Storage, p.FileHash, "col", p.Force)
	spritesDone := variantDone(jobCtx, h.Storage, p.FileHash, "sprites.jpg", p.Force)
	framesDone := variantDone(jobCtx, h.Storage, p.FileHash, "turntable/0000.png", p.Force)
	viewsDone := variantDone(jobCtx, h.Storage, p.FileHash, "views/top.png", p.Force)
	// isoDone is the "the ladder was fanned from a textured render"
	// sentinel. It predates #500 (it used to gate a Blender Cycles re-fan
	// that fixed workbench's magenta textures); with three.js as the only
	// renderer the poster IS that textured source, and the sentinel now
	// only serves the re-enqueue early exit below.
	isoDone := variantDone(jobCtx, h.Storage, p.FileHash, "iso/source.png", p.Force)
	if posterDone && spritesDone && framesDone && viewsDone && isoDone {
		result.Skipped = append(result.Skipped, "col", "sprites", "frames", "views", "iso")
		h.markReady(jobCtx, p.AssetID)
		result.WorkS = time.Since(started).Seconds()
		return json.Marshal(result)
	}

	// The worker writes <out>/turntable/*.png + <out>/views/*.png + a
	// poster.png; everything below reads that fixed layout.
	renderOut := work.dir
	framesDir := filepath.Join(renderOut, "turntable")
	viewsDir := filepath.Join(renderOut, "views")
	// After any format3d/mview conversion above, sourcePath is the .glb —
	// so those formats route to three.js too.
	renderExt := strings.ToLower(strings.TrimPrefix(filepath.Ext(work.sourcePath), "."))

	// Renderable-format gate (#500). Blender is gone from the image, so
	// the three.js worker is the only renderer: a format outside its
	// loader set gets no turntable at all. That is a clean, terminal-free
	// skip rather than a job failure — the upload is intact and served,
	// only the generated thumbnail is missing, and marking the asset
	// `failed` over an absent thumbnail would be a lie. Proprietary/exotic
	// formats (.blend, .abc, .usd*, .x3d) regain previews when the Blender
	// converter ships as a plugin (#499).
	if !isThreeJSExt(renderExt) {
		h.Logger.LogAttrs(jobCtx, slog.LevelInfo, "preview.model.no_renderer_for_format",
			slog.String("asset_id", p.AssetID.String()),
			slog.String("ext", renderExt))
		result.FrameCount = 0
		result.Skipped = append(result.Skipped, "render:no_renderer_for_format")
		h.markReady(jobCtx, p.AssetID)
		result.WorkS = time.Since(started).Seconds()
		return json.Marshal(result)
	}

	// A missing/broken worker is an image or deployment fault, not an
	// asset fault: return a retryable error so the job comes back once
	// ops fixes it, instead of silently marking the asset ready with no
	// preview. Deliberately NOT a TerminalError.
	if !h.threeJSAvailable() {
		return nil, fmt.Errorf("preview.model: three.js worker unavailable at %s (node=%s)",
			h.threeJSScriptPath(), h.nodeBin())
	}

	// three.js render step (#498). One pass yields a correctly-textured
	// turntable + a hi-res poster + reference views. Because three.js
	// binds textures the same way the live viewer does, the raster ladder
	// fans straight off the poster.
	if err := h.renderThreeJS(jobCtx, work.sourcePath, work.dir, renderOut); err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, fmt.Errorf("preview.model: render: %w", err)
	}
	posterPath := filepath.Join(renderOut, "poster.png")
	if !posterDone {
		if err := h.fanRasterLadder(jobCtx, p.AssetID, p.FileHash, posterPath, p.Force); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.model.poster_fan_failed",
				slog.String("err", err.Error()))
		} else {
			result.Variants = append(result.Variants, "poster")
			posterDone = true
		}
	}
	// The poster IS the textured ladder source; persist it under the
	// iso sentinel so a re-enqueue can early-exit without re-rendering.
	if !isoDone {
		if err := h.uploadFile(jobCtx, p.FileHash, "iso/source.png", posterPath, "image/png"); err == nil {
			isoDone = true
		}
	}

	// --- raster ladder: fan frame 0 into col / preview / screen / hires ---
	if posterDone {
		result.Skipped = append(result.Skipped, "raster")
	} else {
		firstFrame := filepath.Join(framesDir, "frame_0000.png")
		if err := h.fanRasterLadder(jobCtx, p.AssetID, p.FileHash, firstFrame, p.Force); err != nil {
			return nil, fmt.Errorf("preview.model: raster ladder: %w", err)
		}
		result.Variants = append(result.Variants, "raster")
	}

	// --- per-frame turntable variants (CLIP training set) ----------------
	// Each frame uploaded as `turntable/NNNN.png` so a downstream
	// trainer can pull them by index. PNG (not JPEG) to keep the
	// signal clean for the encoder; the bundle is ~36 × 80KB ≈ 3MB
	// per asset which is cheap at our scale.
	if framesDone {
		result.Skipped = append(result.Skipped, "frames")
	} else if err := h.writeFrameVariants(jobCtx, p.FileHash, framesDir, p.Force); err != nil {
		return nil, fmt.Errorf("preview.model: frame variants: %w", err)
	} else {
		result.Variants = append(result.Variants, "frames")
	}

	// --- reference views: top + bottom -----------------------------------
	// Single PNG renders the viewer can offer as 'top-down' / 'looking
	// up' insets later. Not part of the scrub.
	if viewsDone {
		result.Skipped = append(result.Skipped, "views")
	} else if err := h.writeReferenceViews(jobCtx, p.FileHash, viewsDir, p.Force); err != nil {
		return nil, fmt.Errorf("preview.model: views: %w", err)
	} else {
		result.Variants = append(result.Variants, "views")
	}

	// --- sprite sheet + VTT (turntable scrub) -----------------------------
	if spritesDone {
		result.Skipped = append(result.Skipped, "sprites")
	} else if err := h.writeSprites(jobCtx, p.FileHash, framesDir); err != nil {
		return nil, fmt.Errorf("preview.model: sprites: %w", err)
	} else {
		result.Variants = append(result.Variants, "sprites")
	}

	// The isometric Cycles re-fan that used to live here is gone with
	// Blender (#500): it existed solely to repaint the magenta thumbnails
	// Blender's workbench engine produced for textured models, and the
	// three.js poster is already correctly textured. `iso/source.png` is
	// still written above, as the re-enqueue sentinel.
	result.Skipped = append(result.Skipped, "iso")

	h.markReady(jobCtx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// writeFrameVariants uploads each turntable frame as its own variant
// under `turntable/NNNN.png`. Idempotent per-variant so a re-queue
// after a partial upload only fills the gaps.
func (h *ModelHandler) writeFrameVariants(ctx context.Context, hash, framesDir string, force bool) error {
	for i := 0; i < h.Frames; i++ {
		key := fmt.Sprintf("turntable/%04d.png", i)
		if variantDone(ctx, h.Storage, hash, key, force) {
			continue
		}
		path := filepath.Join(framesDir, fmt.Sprintf("frame_%04d.png", i))
		if err := h.uploadFile(ctx, hash, key, path, "image/png"); err != nil {
			return fmt.Errorf("upload frame %d: %w", i, err)
		}
	}
	return nil
}

// writeReferenceViews uploads top + bottom orthographic-ish reference
// renders as `views/top.png` and `views/bottom.png`. Best-effort per
// view — a missing file is a soft fail so a render that wrote only one
// view doesn't tank the whole job.
func (h *ModelHandler) writeReferenceViews(ctx context.Context, hash, viewsDir string, force bool) error {
	for _, name := range []string{"top", "bottom"} {
		key := "views/" + name + ".png"
		if variantDone(ctx, h.Storage, hash, key, force) {
			continue
		}
		path := filepath.Join(viewsDir, name+".png")
		if _, err := os.Stat(path); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.view_missing",
				slog.String("view", name),
				slog.String("path", path))
			continue
		}
		if err := h.uploadFile(ctx, hash, key, path, "image/png"); err != nil {
			return fmt.Errorf("upload %s: %w", name, err)
		}
	}
	return nil
}

// extractMviewThumbBestEffort lifts the embedded thumbnail.jpg out of
// a .mview archive and fans it through the raster ladder. Best-effort:
// failures log a warning and return, never abort the job — the col
// variant is a "nice to have here, the GLB conversion below produces
// real renders on success".
func (h *ModelHandler) extractMviewThumbBestEffort(ctx context.Context, assetID uuid.UUID, hash, mviewPath string, force bool) {
	if variantDone(ctx, h.Storage, hash, "col", force) {
		return
	}
	f, err := os.Open(mviewPath)
	if err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.mview_open_failed",
			slog.String("err", err.Error()))
		return
	}
	defer f.Close()
	thumb, err := mview.ExtractThumbnail(io.LimitReader(f, h.MaxSourceBytes+1))
	if err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.mview_thumb_extract_failed",
			slog.String("err", err.Error()))
		return
	}
	if err := h.fanThumbBytes(ctx, assetID, hash, thumb, force); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.mview_thumb_fan_failed",
			slog.String("err", err.Error()))
	}
}

// convertFormat3D runs an in-tree format3d decoder against the
// staged source file and writes the resulting glTF binary next to
// it. Returns the .glb path so the caller can swap work.sourcePath
// before the standard turntable runs.
//
// Streams the source through the decoder rather than reading the
// whole file into memory, then asks format3d.WriteGLB to encode
// directly to a file handle. Peak memory ≈ source size + decoded
// mesh, well under the worker's MaxSourceBytes cap.
func (h *ModelHandler) convertFormat3D(ctx context.Context, src string, dec format3dDecoder) (string, error) {
	_ = ctx // dec is pure CPU; ctx cancel checked at the caller's frame
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer in.Close()
	model, err := dec(in)
	if err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	glbPath := strings.TrimSuffix(src, filepath.Ext(src)) + ".glb"
	out, err := os.Create(glbPath)
	if err != nil {
		return "", fmt.Errorf("create glb: %w", err)
	}
	if err := format3d.WriteGLB(model, out); err != nil {
		_ = out.Close()
		_ = os.Remove(glbPath)
		return "", fmt.Errorf("write glb: %w", err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close glb: %w", err)
	}
	return glbPath, nil
}

// convertMviewToGLB runs the in-process Marmoset → glTF binary
// converter and writes the result next to the .mview source in the
// workdir. Returns the path to the new .glb so the caller can swap
// the renderer's input over.
//
// Honours ctx so a worker timeout aborts the conversion cleanly, and
// caps total decompressed payload at MaxSourceBytes (the same ceiling
// we apply to the staged source file). Channel merging is on by
// default — gives proper PBR base color + metallic-roughness in the
// emitted GLB at ~50ms additional decode cost per material.
func (h *ModelHandler) convertMviewToGLB(ctx context.Context, mviewPath string) (string, error) {
	in, err := os.Open(mviewPath)
	if err != nil {
		return "", fmt.Errorf("open mview: %w", err)
	}
	defer in.Close()
	glbPath := strings.TrimSuffix(mviewPath, filepath.Ext(mviewPath)) + ".glb"
	out, err := os.Create(glbPath)
	if err != nil {
		return "", fmt.Errorf("create glb: %w", err)
	}
	if err := mview.ConvertToGLBContext(ctx, in, out,
		mview.WithMaxTotalSize(h.MaxSourceBytes),
		mview.WithMaterialChannelMerge(true),
	); err != nil {
		_ = out.Close()
		_ = os.Remove(glbPath)
		return "", fmt.Errorf("convert: %w", err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close glb: %w", err)
	}
	return glbPath, nil
}

// fanThumbBytes decodes a JPEG and writes it through the raster
// variant ladder. Shared by the mview path; could be repurposed by
// any future "we already have a poster, just resize it" worker.
func (h *ModelHandler) fanThumbBytes(ctx context.Context, assetID uuid.UUID, hash string, jpegBytes []byte, force bool) error {
	src, err := jpeg.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		return fmt.Errorf("decode thumb: %w", err)
	}
	return fanToLadder(ctx, ladderInput{
		Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
		AssetID: assetID, Hash: hash, Src: src, Kind: "model", Source: "mview",
		Overwrite: force,
	})
}

// stageCompanions writes every companion the asset has into the render
// workdir at the relative path the user declared. Loaders
// then resolve relative references (OBJ's mtllib, glTF's .bin uri,
// MTL's map_Kd 'Textures/foo.png') naturally against the workdir.
//
// Per-companion soft-fail: a single bad blob (network blip, GC race)
// shouldn't tank the whole render. The model will just render
// untextured if its texture didn't make it, which is still useful.
func (h *ModelHandler) stageCompanions(ctx context.Context, assetID uuid.UUID, workDir string) error {
	companions, err := assets.New(h.Pool).ListAssetCompanions(
		ctx, pgtype.UUID{Bytes: assetID, Valid: true},
	)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	for _, c := range companions {
		// Defensive: refuse anything that escapes the workdir. The API
		// already rejects '..' and leading '/' at upload, but pinning
		// it again here means a future bypass at the upload layer
		// doesn't immediately become a path-traversal worker exploit.
		if strings.Contains(c.CompanionPath, "..") ||
			strings.HasPrefix(c.CompanionPath, "/") ||
			strings.HasPrefix(c.CompanionPath, "\\") {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.companion_path_rejected",
				slog.String("path", c.CompanionPath))
			continue
		}
		dst := filepath.Join(workDir, c.CompanionPath)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.companion_mkdir_failed",
				slog.String("path", c.CompanionPath),
				slog.String("err", err.Error()))
			continue
		}
		rc, _, err := h.Storage.Download(ctx, c.ObjectHash, storage.VariantOriginal)
		if err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.companion_download_failed",
				slog.String("path", c.CompanionPath),
				slog.String("err", err.Error()))
			continue
		}
		f, err := os.Create(dst)
		if err != nil {
			_ = rc.Close()
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.companion_create_failed",
				slog.String("path", dst),
				slog.String("err", err.Error()))
			continue
		}
		if _, err := io.Copy(f, rc); err != nil {
			_ = f.Close()
			_ = rc.Close()
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.companion_copy_failed",
				slog.String("path", dst),
				slog.String("err", err.Error()))
			continue
		}
		_ = f.Close()
		_ = rc.Close()
	}
	return nil
}

// uploadFile streams a local file into storage at (hash, key) and
// upserts the variant row. Shared by writeFrameVariants and
// writeReferenceViews so the upload path stays in one place.
func (h *ModelHandler) uploadFile(ctx context.Context, hash, key, path, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", key, err)
	}
	defer f.Close()
	if _, err := h.Storage.Backend.Put(ctx, hash, key, f); err != nil {
		return fmt.Errorf("backend put %s: %w", key, err)
	}
	info, err := f.Stat()
	if err == nil {
		_ = storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
			ObjectHash:  hash,
			VariantKey:  key,
			SizeBytes:   info.Size(),
			ContentType: contentType,
			Metadata:    []byte("{}"),
		})
	}
	return nil
}

// ---------------------------------------------------------------------------
// Staging — download original to a temp dir with the right extension.
// Loader dispatch (here and in the worker) is extension-driven.
// ---------------------------------------------------------------------------

func (h *ModelHandler) stage(ctx context.Context, hash, ext string) (workDir, func(), error) {
	base := h.TempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "aa-model-*")
	if err != nil {
		return workDir{}, nil, fmt.Errorf("stage: mkdir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	rc, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: download: %w", err)
	}
	defer rc.Close()
	if info != nil && info.Size > h.MaxSourceBytes {
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: source %d bytes > cap %d", info.Size, h.MaxSourceBytes)
	}

	srcPath := filepath.Join(dir, "src."+strings.ToLower(strings.TrimPrefix(ext, ".")))
	f, err := os.Create(srcPath)
	if err != nil {
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: create: %w", err)
	}
	if _, err := io.CopyN(f, rc, h.MaxSourceBytes+1); err != nil && !errors.Is(err, io.EOF) {
		_ = f.Close()
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: copy: %w", err)
	}
	_ = f.Close()
	return workDir{dir: dir, sourcePath: srcPath}, cleanup, nil
}

// ---------------------------------------------------------------------------
// three.js worker invocation (#498)
// ---------------------------------------------------------------------------

// threeJSExts is the set of formats the headless three.js worker renders
// directly. Formats the in-tree importers convert to .glb first
// (md2/md3/mdl/ms3d/mview) also land here because the render decision
// reads the post-conversion extension.
//
// MUST stay in sync with loadModel() in scripts/threejs/render.html —
// this map is the Go side's copy of that dispatch, and the
// three-formats smoke (scripts/threejs/smoke.mjs) renders one fixture
// per entry so a drift shows up as a failed smoke rather than as
// "worker exit 1" on a real upload.
//
// glb/gltf/fbx/obj are what the interactive viewer loads. stl/ply/dae
// are render-only: Blender used to produce their thumbnails, and #500
// took Blender out of the image, so the worker picks them up via the
// stock three.js loaders (ADR 0069 already listed them as formats
// "three.js parses natively"). The remaining ModelExts entries
// (blend/x3d/wrl/usd*/abc) have no stock loader and get no turntable
// until the Blender converter ships as a plugin (#499).
var threeJSExts = map[string]struct{}{
	"glb": {}, "gltf": {}, "fbx": {}, "obj": {},
	"stl": {}, "ply": {}, "dae": {},
}

func isThreeJSExt(ext string) bool {
	_, ok := threeJSExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}

func (h *ModelHandler) nodeBin() string {
	if h.NodePath != "" {
		return h.NodePath
	}
	return "node"
}

func (h *ModelHandler) threeJSScriptPath() string {
	if h.ThreeJSScript != "" {
		return h.ThreeJSScript
	}
	return "/app/threejs/worker.mjs"
}

func (h *ModelHandler) posterRes() int {
	if h.PosterRes > 0 {
		return h.PosterRes
	}
	return 2048
}

// threeJSAvailable reports whether the headless worker can run: script
// present, node on PATH, and the worker's node_modules installed. With
// Blender out of the image (#500) there is nothing to fall back to, so a
// missing piece is a deployment fault the caller surfaces as a retryable
// job error rather than a silent no-preview.
func (h *ModelHandler) threeJSAvailable() bool {
	script := h.threeJSScriptPath()
	if _, err := os.Stat(script); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(script), "node_modules", "three")); err != nil {
		return false
	}
	if _, err := exec.LookPath(h.nodeBin()); err != nil {
		return false
	}
	return true
}

// renderThreeJS shells out to the headless three.js worker, which writes
// turntable/*.png + views/*.png + poster.png under outDir — the layout
// the fanning below reads. The model + its staged companions both live
// under workDir so the loaders resolve sibling .bin/textures by relative
// URL.
func (h *ModelHandler) renderThreeJS(ctx context.Context, src, workDir, outDir string) error {
	cmd := exec.CommandContext(ctx, h.nodeBin(), h.threeJSScriptPath(),
		"--input", src,
		"--workdir", workDir,
		"--output", outDir,
		"--frames", strconv.Itoa(h.Frames),
		"--res", strconv.Itoa(h.Res),
		"--poster-res", strconv.Itoa(h.posterRes()),
	)
	var stderr, stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 800 {
			tail = "..." + tail[len(tail)-800:]
		}
		return fmt.Errorf("threejs worker exit: %w: %s", err, tail)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Raster ladder — load frame 0, fan into col / preview / screen / hires.
// ---------------------------------------------------------------------------

// fanRasterLadder decodes a rendered PNG and fans it across the
// configured variant ladder.
//
// This used to have an overwrite-aware sibling, because the Blender
// isometric pass needed to paint over the workbench poster's magenta
// col/preview/screen bytes. three.js renders textured on the first
// pass, so there is nothing to repaint and no second writer — the pair
// collapsed back into one function with #500.
func (h *ModelHandler) fanRasterLadder(ctx context.Context, assetID uuid.UUID, hash, framePath string, force bool) error {
	f, err := os.Open(framePath)
	if err != nil {
		return fmt.Errorf("open frame: %w", err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return fmt.Errorf("decode frame: %w", err)
	}
	return fanToLadder(ctx, ladderInput{
		Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
		AssetID: assetID, Hash: hash, Src: src, Kind: "model", Source: "turntable",
		Overwrite: force,
	})
}

// ---------------------------------------------------------------------------
// Sprite sheet + WebVTT — turntable as a "video" of N frames so the
// frontend's existing hover-scrub code (PostCard / AssetCard) lights up
// for 3D for free.
// ---------------------------------------------------------------------------

const (
	modelSpriteCols       = 6
	modelSpriteRows       = 6
	modelSpriteCell       = 160 // px per cell; 36 cells × 160 = 960² sprite sheet
	modelTurntableSeconds = 4.0
)

func (h *ModelHandler) writeSprites(ctx context.Context, hash, framesDir string) error {
	total := modelSpriteCols * modelSpriteRows
	sheet := image.NewRGBA(image.Rect(0, 0, modelSpriteCols*modelSpriteCell, modelSpriteRows*modelSpriteCell))

	loaded := 0
	for i := 0; i < total; i++ {
		framePath := filepath.Join(framesDir, fmt.Sprintf("frame_%04d.png", i))
		f, err := os.Open(framePath)
		if err != nil {
			// If we have fewer frames than cells (caller picked a low
			// --frames), stop the tiling early rather than failing the
			// whole job.
			break
		}
		img, err := png.Decode(f)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("decode frame %d: %w", i, err)
		}
		x := (i % modelSpriteCols) * modelSpriteCell
		y := (i / modelSpriteCols) * modelSpriteCell
		cell := image.Rect(x, y, x+modelSpriteCell, y+modelSpriteCell)
		xdraw.CatmullRom.Scale(sheet, cell, img, img.Bounds(), xdraw.Over, nil)
		loaded++
	}
	if loaded == 0 {
		return errors.New("no frames rendered")
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, sheet, &jpeg.Options{Quality: 75}); err != nil {
		return fmt.Errorf("encode sprite: %w", err)
	}
	if _, err := h.Storage.Backend.Put(ctx, hash, "sprites.jpg", bytes.NewReader(buf.Bytes())); err != nil {
		return fmt.Errorf("put sprite: %w", err)
	}
	_ = storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
		ObjectHash:  hash,
		VariantKey:  "sprites.jpg",
		SizeBytes:   int64(buf.Len()),
		ContentType: "image/jpeg",
		Metadata:    []byte("{}"),
	})

	// WebVTT — give the turntable a synthetic timeline so the hover
	// scrub picks the right cell. Each cell owns 1/loaded of a 4s loop.
	var vtt bytes.Buffer
	vtt.WriteString("WEBVTT\n\n")
	interval := modelTurntableSeconds / float64(loaded)
	for i := 0; i < loaded; i++ {
		start := float64(i) * interval
		end := float64(i+1) * interval
		x := (i % modelSpriteCols) * modelSpriteCell
		y := (i / modelSpriteCols) * modelSpriteCell
		fmt.Fprintf(&vtt, "%s --> %s\nsprites.jpg#xywh=%d,%d,%d,%d\n\n",
			vttTime(start), vttTime(end), x, y, modelSpriteCell, modelSpriteCell)
	}
	if _, err := h.Storage.Backend.Put(ctx, hash, "sprites.vtt", bytes.NewReader(vtt.Bytes())); err != nil {
		return fmt.Errorf("put vtt: %w", err)
	}
	_ = storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
		ObjectHash:  hash,
		VariantKey:  "sprites.vtt",
		SizeBytes:   int64(vtt.Len()),
		ContentType: "text/vtt",
		Metadata:    []byte("{}"),
	})
	return nil
}

// ---------------------------------------------------------------------------
// Shared plumbing — variantExists / mark* mirror VideoHandler.
// ---------------------------------------------------------------------------

func (h *ModelHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *ModelHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *ModelHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
