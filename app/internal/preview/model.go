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
var modelExts = map[string]struct{}{
	"glb": {}, "gltf": {}, "fbx": {}, "obj": {}, "blend": {},
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

	result := ModelResult{FrameCount: h.Frames}

	// If both the col variant AND the sprite sheet already exist, the
	// render is a no-op. Cheap path for re-queues.
	posterDone := h.variantExists(jobCtx, p.FileHash, "col")
	spritesDone := h.variantExists(jobCtx, p.FileHash, "sprites.jpg")
	if posterDone && spritesDone {
		result.Skipped = append(result.Skipped, "col", "sprites")
		h.markReady(jobCtx, p.AssetID)
		result.WorkS = time.Since(started).Seconds()
		return json.Marshal(result)
	}

	framesDir := filepath.Join(work.dir, "frames")
	if err := h.renderTurntable(jobCtx, work.sourcePath, framesDir); err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, fmt.Errorf("preview.model: render: %w", err)
	}

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
