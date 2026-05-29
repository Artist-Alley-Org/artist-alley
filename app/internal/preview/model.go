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
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// ModelPayload — JSON body for a preview.model job.
type ModelPayload struct {
	AssetID       uuid.UUID `json:"asset_id"`
	FileHash      string    `json:"file_hash"`
	FileExtension string    `json:"file_extension"`
}

// ModelResult — what the worker writes back to jobs.result for the
// admin queue view.
type ModelResult struct {
	Variants   []string `json:"variants"`
	Skipped    []string `json:"skipped"`
	FrameCount int      `json:"frame_count"`
	WorkS      float64  `json:"work_s"`
}

// ModelHandler renders an N-frame turntable of a 3D asset via headless
// Blender, fans the first frame into the standard raster ladder
// (col / preview / screen / hires), and composes the rest into a
// sprite sheet + WebVTT cues that the existing hover-scrub UI already
// understands (same scheme as preview.video sprites).
//
// Idempotent: every output is checked against the storage backend
// before re-rendering. A re-queued job only fills the gaps.
type ModelHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	// BlenderPath / ScriptPath override the executable + script lookups.
	// Empty BlenderPath = `blender` from PATH. Empty ScriptPath defaults
	// to /app/blender/turntable.py — the container layout the Dockerfile
	// ships.
	BlenderPath string
	ScriptPath  string

	TempDir string

	// Conservative: 3D source files larger than this are rejected.
	// 500 MB is plenty for a single dense asset; whole-studio bundles
	// belong somewhere else.
	MaxSourceBytes int64

	// Per-job wallclock cap. Blender Cycles on CPU at 36 frames × 512²
	// × 32 samples lands around 60-180s on a modern desktop; cap at 15
	// min so a pathological mesh doesn't hold a worker forever.
	MaxJobDuration time.Duration

	// Turntable resolution + frame count. The defaults give a 36-frame
	// loop at 10° steps (6×6 sprite grid).
	Frames  int
	Res     int
	Samples int
}

// NewModelHandler — recommended constructor with sensible defaults.
func NewModelHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *ModelHandler {
	return &ModelHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 500 * 1024 * 1024,
		MaxJobDuration: 15 * time.Minute,
		Frames:         36,
		Res:            512,
		Samples:        32,
	}
}

func (h *ModelHandler) Type() jobs.JobType { return jobs.TypePreview3D }

// modelExts mirrors the dispatcher map in assets.handler.go.
//
// Native Blender importers cover the bulk of the list. mview is the
// odd one: Blender can't read it, so we convert the archive to glTF
// binary in-process via the Go mview/glb decoder before staging it
// for the turntable render. The browser viewer also has a live
// marmoset.js path as a fallback for formats this decoder doesn't
// yet cover (animated rigs, skinning).
var modelExts = map[string]struct{}{
	"glb": {}, "gltf": {}, "fbx": {}, "obj": {}, "blend": {}, "mview": {},
	"dae": {}, "ply": {}, "stl": {}, "3ds": {}, "x3d": {}, "wrl": {},
	"usd": {}, "usda": {}, "usdc": {}, "usdz": {}, "abc": {},
}

func isModelExt(ext string) bool {
	_, ok := modelExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
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
	// relative paths. Blender's importers resolve external resources
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
	//      to the .glb so the standard Blender turntable below
	//      picks it up; the conversion gives us real turntable +
	//      reference views matching the rest of the 3D pipeline.
	// A conversion failure is non-fatal: the safety-net thumbnail
	// already populated the raster ladder, marmoset.js still serves
	// the live view, and we just skip the heavy Blender frames.
	if strings.EqualFold(strings.TrimPrefix(p.FileExtension, "."), "mview") {
		h.extractMviewThumbBestEffort(jobCtx, p.FileHash, work.sourcePath)
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
	posterDone := h.variantExists(jobCtx, p.FileHash, "col")
	spritesDone := h.variantExists(jobCtx, p.FileHash, "sprites.jpg")
	framesDone := h.variantExists(jobCtx, p.FileHash, "turntable/0000.png")
	viewsDone := h.variantExists(jobCtx, p.FileHash, "views/top.png")
	if posterDone && spritesDone && framesDone && viewsDone {
		result.Skipped = append(result.Skipped, "col", "sprites", "frames", "views")
		h.markReady(jobCtx, p.AssetID)
		result.WorkS = time.Since(started).Seconds()
		return json.Marshal(result)
	}

	// Fast poster: render a single workbench frame in ~1s and fan it
	// through the raster ladder before launching the slow Cycles
	// turntable. Lets the browse card show a real thumbnail while the
	// 22-second turntable render is still in flight. Best-effort: any
	// failure logs and falls through — the Cycles path below will fill
	// col from frame 0 once it finishes.
	if !posterDone {
		posterPath := filepath.Join(work.dir, "poster.png")
		if err := h.renderPoster(jobCtx, work.sourcePath, posterPath); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.model.poster_render_failed",
				slog.String("asset_id", p.AssetID.String()),
				slog.String("err", err.Error()))
		} else if err := h.fanRasterLadder(jobCtx, p.FileHash, posterPath); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.model.poster_fan_failed",
				slog.String("err", err.Error()))
		} else {
			result.Variants = append(result.Variants, "poster")
			posterDone = true // skip the Cycles-frame-0 ladder below
		}
	}

	// Blender writes to <out>/turntable/*.png + <out>/views/*.png so
	// the Go side and the script speak the same layout.
	renderOut := work.dir
	if err := h.renderTurntable(jobCtx, work.sourcePath, renderOut); err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, fmt.Errorf("preview.model: render: %w", err)
	}
	framesDir := filepath.Join(renderOut, "turntable")
	viewsDir := filepath.Join(renderOut, "views")

	// --- raster ladder: fan frame 0 into col / preview / screen / hires ---
	if posterDone {
		result.Skipped = append(result.Skipped, "raster")
	} else {
		firstFrame := filepath.Join(framesDir, "frame_0000.png")
		if err := h.fanRasterLadder(jobCtx, p.FileHash, firstFrame); err != nil {
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
	} else if err := h.writeFrameVariants(jobCtx, p.FileHash, framesDir); err != nil {
		return nil, fmt.Errorf("preview.model: frame variants: %w", err)
	} else {
		result.Variants = append(result.Variants, "frames")
	}

	// --- reference views: top + bottom -----------------------------------
	// Single PNG renders the viewer can offer as 'top-down' / 'looking
	// up' insets later. Not part of the scrub.
	if viewsDone {
		result.Skipped = append(result.Skipped, "views")
	} else if err := h.writeReferenceViews(jobCtx, p.FileHash, viewsDir); err != nil {
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

	h.markReady(jobCtx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// writeFrameVariants uploads each turntable frame as its own variant
// under `turntable/NNNN.png`. Idempotent per-variant so a re-queue
// after a partial upload only fills the gaps.
func (h *ModelHandler) writeFrameVariants(ctx context.Context, hash, framesDir string) error {
	for i := 0; i < h.Frames; i++ {
		key := fmt.Sprintf("turntable/%04d.png", i)
		if h.variantExists(ctx, hash, key) {
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
// view — a missing file is a soft fail so a Blender render that wrote
// only one view doesn't tank the whole job.
func (h *ModelHandler) writeReferenceViews(ctx context.Context, hash, viewsDir string) error {
	for _, name := range []string{"top", "bottom"} {
		key := "views/" + name + ".png"
		if h.variantExists(ctx, hash, key) {
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
func (h *ModelHandler) extractMviewThumbBestEffort(ctx context.Context, hash, mviewPath string) {
	if h.variantExists(ctx, hash, "col") {
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
	if err := h.fanThumbBytes(ctx, hash, thumb); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.mview_thumb_fan_failed",
			slog.String("err", err.Error()))
	}
}

// convertMviewToGLB runs the in-process Marmoset → glTF binary
// converter and writes the result next to the .mview source in the
// workdir. Returns the path to the new .glb so the caller can swap
// the Blender input over.
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
func (h *ModelHandler) fanThumbBytes(ctx context.Context, hash string, jpegBytes []byte) error {
	if h.SysConfig == nil {
		return nil
	}
	cfg, err := h.SysConfig.GetPreviews(ctx)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	src, err := jpeg.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		return fmt.Errorf("decode thumb: %w", err)
	}
	for _, v := range cfg.Variants {
		if v.Key == storage.VariantOriginal {
			continue
		}
		if h.variantExists(ctx, hash, v.Key) {
			continue
		}
		dst := resizeFor(src, v)
		var buf bytes.Buffer
		contentType, err := encodeImage(&buf, dst, v)
		if err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.mview_encode_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
			continue
		}
		if _, err := h.Storage.Backend.Put(ctx, hash, v.Key, bytes.NewReader(buf.Bytes())); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.mview_put_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
			continue
		}
		_ = storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
			ObjectHash:  hash,
			VariantKey:  v.Key,
			SizeBytes:   int64(buf.Len()),
			ContentType: contentType,
			Metadata:    []byte("{}"),
		})
	}
	return nil
}

// stageCompanions writes every companion the asset has into the
// Blender workdir at the relative path the user declared. Loaders
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
// Blender's importer dispatch is extension-driven.
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
// Blender invocation
// ---------------------------------------------------------------------------

func (h *ModelHandler) renderTurntable(ctx context.Context, src, framesDir string) error {
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		return fmt.Errorf("mkdir frames: %w", err)
	}
	// --background: no UI / no GL window. Audio is auto-disabled in
	//   background mode per the docs, so no separate -noaudio needed.
	// --factory-startup: ignore user prefs (CI determinism).
	// --disable-autoexec (-Y): refuse to auto-run Python embedded in
	//   uploaded .blend files. Without this, an attacker could ship a
	//   .blend whose driver expressions execute arbitrary Python under
	//   the worker UID. Default-off when run from CLI, but explicit
	//   here is belt-and-suspenders.
	// --python-exit-code 1: critical. Without it, Blender exits 0 even
	//   if our script raises an unhandled exception, and the Go handler
	//   thinks the render succeeded.
	// --engine: A/B test against a textured torus showed Workbench
	//   wins on every format except .blend, where the source often
	//   carries full PBR setups that Cycles needs to render
	//   faithfully. See engineForExt().
	// `--` separates Blender's args from the script's argv.
	cmd := exec.CommandContext(ctx, h.blenderBin(),
		"--background",
		"--factory-startup",
		"--disable-autoexec",
		"--python-exit-code", "1",
		"--python", h.scriptPath(),
		"--",
		"--input", src,
		"--output", framesDir,
		"--engine", engineForExt(filepath.Ext(src)),
		"--frames", strconv.Itoa(h.Frames),
		"--res", strconv.Itoa(h.Res),
		"--samples", strconv.Itoa(h.Samples),
	)
	var stderr, stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// Blender prints render progress to stdout; surface the tail of
		// stderr (where actual errors land) in the wrapped message.
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 800 {
			tail = "..." + tail[len(tail)-800:]
		}
		return fmt.Errorf("blender exit: %w: %s", err, tail)
	}
	return nil
}

// renderPoster invokes the same turntable.py with --poster-output, which
// short-circuits to a single Workbench-engine render (~1s) instead of
// the 22s Cycles turntable. Used to seed the col / preview / screen
// thumbnail ladder while the full render is still in flight.
func (h *ModelHandler) renderPoster(ctx context.Context, src, posterPath string) error {
	if err := os.MkdirAll(filepath.Dir(posterPath), 0o755); err != nil {
		return fmt.Errorf("mkdir poster: %w", err)
	}
	cmd := exec.CommandContext(ctx, h.blenderBin(),
		"--background",
		"--factory-startup",
		"--disable-autoexec",
		"--python-exit-code", "1",
		"--python", h.scriptPath(),
		"--",
		"--input", src,
		"--poster-output", posterPath,
	)
	var stderr, stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 800 {
			tail = "..." + tail[len(tail)-800:]
		}
		return fmt.Errorf("blender poster: %w: %s", err, tail)
	}
	return nil
}

// engineForExt picks the best-looking Blender render engine for a
// given 3D source extension, determined by the A/B sweep at
// scripts/blender/ab_engine_test.py against a textured torus.
//
// Workbench (matcap-style viewport shading) reads better for almost
// every format: it produces clean, evenly lit thumbnails with crisp
// silhouettes that match the static col poster — no over-bright
// Cycles specular blooming. The exception is .blend, where the
// source usually ships its own PBR shader graph, lights, and HDRI
// that only Cycles can render faithfully.
//
// .mview converts to .glb before reaching this function (see
// convertMviewToGLB), so it inherits glb's workbench choice.
func engineForExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "blend":
		return "cycles"
	default:
		return "workbench"
	}
}

func (h *ModelHandler) blenderBin() string {
	if h.BlenderPath != "" {
		return h.BlenderPath
	}
	return "blender"
}
func (h *ModelHandler) scriptPath() string {
	if h.ScriptPath != "" {
		return h.ScriptPath
	}
	return "/app/blender/turntable.py"
}

// ---------------------------------------------------------------------------
// Raster ladder — load frame 0, fan into col / preview / screen / hires.
// ---------------------------------------------------------------------------

func (h *ModelHandler) fanRasterLadder(ctx context.Context, hash, framePath string) error {
	if h.SysConfig == nil {
		return nil
	}
	cfg, err := h.SysConfig.GetPreviews(ctx)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	f, err := os.Open(framePath)
	if err != nil {
		return fmt.Errorf("open frame: %w", err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return fmt.Errorf("decode frame: %w", err)
	}
	for _, v := range cfg.Variants {
		if v.Key == storage.VariantOriginal {
			continue
		}
		if h.variantExists(ctx, hash, v.Key) {
			continue
		}
		dst := resizeFor(src, v)
		var buf bytes.Buffer
		contentType, err := encodeImage(&buf, dst, v)
		if err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.raster_encode_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
			continue
		}
		if _, err := h.Storage.Backend.Put(ctx, hash, v.Key, bytes.NewReader(buf.Bytes())); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.model.raster_put_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
			continue
		}
		_ = storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
			ObjectHash:  hash,
			VariantKey:  v.Key,
			SizeBytes:   int64(buf.Len()),
			ContentType: contentType,
			Metadata:    []byte("{}"),
		})
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sprite sheet + WebVTT — turntable as a "video" of N frames so the
// frontend's existing hover-scrub code (PostCard / AssetCard) lights up
// for 3D for free.
// ---------------------------------------------------------------------------

const (
	modelSpriteCols = 6
	modelSpriteRows = 6
	modelSpriteCell = 160 // px per cell; 36 cells × 160 = 960² sprite sheet
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

func (h *ModelHandler) variantExists(ctx context.Context, hash, key string) bool {
	_, err := h.Storage.Backend.Stat(ctx, hash, key)
	return err == nil
}

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
