// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.n16f.net/thumbhash"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// ---------------------------------------------------------------------------
// #645 — a preview rendered from a NON-IMAGE source must produce a
// thumbhash.
//
// These drive the real jobs.Handler.Handle path with the external
// renderers faked at the exec boundary (ffmpeg/ffprobe/node are not in
// the test image), because that boundary is exactly where the bug
// lived: the handlers produced a perfectly good rendered preview and
// then never hashed it. A test that called the hashing helper directly
// would have passed on the broken code.
// ---------------------------------------------------------------------------

func thumbhashTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	envOr := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	dsn := "host=" + envOr("AA_DB_HOST", "postgres") +
		" port=" + envOr("AA_DB_PORT", "5432") +
		" user=" + envOr("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// previewTestRig is the shared fixture: an fs-backed storage service, a
// sysconfig store on the test database, and a discard logger.
type previewTestRig struct {
	pool    *pgxpool.Pool
	storage *storage.Service
	sysCfg  *sysconfig.Store
	logger  *slog.Logger
}

func newPreviewTestRig(t *testing.T) *previewTestRig {
	t.Helper()
	pool := thumbhashTestPool(t)
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	svc.TempDir = t.TempDir()
	return &previewTestRig{
		pool:    pool,
		storage: svc,
		sysCfg:  sysconfig.NewStore(pool),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// seedPreviewAsset inserts an asset + its storage_object with a fresh
// random hash, and uploads `body` as the object's original bytes so the
// handlers' stage() step finds something to download. thumbhash starts
// NULL, which is the whole point.
func (r *previewTestRig) seedPreviewAsset(t *testing.T, ext string, body []byte) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	hash := hex.EncodeToString(raw)
	if _, err := r.storage.Backend.Put(ctx, hash, storage.VariantOriginal, bytes.NewReader(body)); err != nil {
		t.Fatalf("put original: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, $2, 'application/octet-stream', 'fs')
		ON CONFLICT (hash) DO NOTHING
	`, hash, len(body)); err != nil {
		t.Fatalf("seed storage_object: %v", err)
	}
	id := uuid.New()
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO assets (
			id, title, asset_type, status,
			file_hash, file_extension, file_size_bytes, sensitivity
		) VALUES ($1, $2, 1, 'active', $3, $4, $5, 'public')
	`, id, "thumbhash-645-"+hash[:8], hash, ext, len(body)); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = r.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
		_, _ = r.pool.Exec(context.Background(), `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
	})
	return id, hash
}

// thumbhashOf reads the stored thumbhash for an asset. nil when NULL.
func (r *previewTestRig) thumbhashOf(t *testing.T, id uuid.UUID) []byte {
	t.Helper()
	var th []byte
	if err := r.pool.QueryRow(context.Background(),
		`SELECT thumbhash FROM assets WHERE id = $1`, id).Scan(&th); err != nil {
		t.Fatalf("read thumbhash: %v", err)
	}
	return th
}

// writeWaveformPNG writes a stand-in for ffmpeg's showwavespic output:
// a wide, mostly TRANSPARENT canvas with a white waveform band down the
// middle. The transparency matters — see
// TestThumbhash_TransparentWaveformIsNotFlat.
func writeWaveformPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	mid := h / 2
	for x := 0; x < w; x++ {
		// A crude two-lobe envelope so the hash has something to encode
		// beyond a flat bar.
		amp := (h / 3) * (1 + x%7) / 8
		if x > w/2 {
			amp = (h / 4) * (1 + x%5) / 6
		}
		for y := mid - amp; y <= mid+amp; y++ {
			if y < 0 || y >= h {
				continue
			}
			img.Set(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 235})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}

// writeTurntablePNG writes a stand-in for a rendered turntable frame:
// an opaque dark background with a bright blob, i.e. what a rendered
// 3D model looks like to a hashing pass.
func writeTurntablePNG(t *testing.T, path string, side int) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			img.Set(x, y, color.NRGBA{R: 24, G: 24, B: 32, A: 255})
		}
	}
	cx, cy, rad := side/2, side/2, side/3
	for y := cy - rad; y < cy+rad; y++ {
		for x := cx - rad; x < cx+rad; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= rad*rad {
				img.Set(x, y, color.NRGBA{R: 220, G: 140, B: 60, A: 255})
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}

// writeShim drops an executable shell script and returns its path.
// Used to stand in for ffmpeg / ffprobe / node, which the plain golang
// test image does not carry.
func writeShim(t *testing.T, dir, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("exec shims need a POSIX shell")
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write shim %s: %v", name, err)
	}
	return p
}

// ---------------------------------------------------------------------------
// Audio — the largest bucket in #645 (194 ogg) and the thinnest tile in
// masonry, so the blank flash is at its most visible there.
// ---------------------------------------------------------------------------

func TestPreviewAudio_StampsThumbhashFromWaveform(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "ogg", []byte("not really ogg, the shims don't care"))

	shimDir := t.TempDir()
	fixture := filepath.Join(shimDir, "wave-fixture.png")
	writeWaveformPNG(t, fixture, 2048, 384)

	// ffprobe shim: emit the minimum JSON probeMetadata can parse.
	ffprobe := writeShim(t, shimDir, "ffprobe", `
echo '{"format":{"duration":"12.5","format_name":"ogg","bit_rate":"128000","tags":{}},"streams":[{"codec_type":"audio","codec_name":"vorbis","sample_rate":"44100","channels":2}]}'
`)
	// ffmpeg shim: the real invocation ends with the output path, and
	// the only output the ladder consumes is the waveform PNG.
	ffmpeg := writeShim(t, shimDir, "ffmpeg", `
for last; do :; done
cp `+fixture+` "$last"
`)

	h := NewAudioHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	h.FFmpegPath = ffmpeg
	h.FFprobePath = ffprobe
	h.TempDir = t.TempDir()

	payload, _ := json.Marshal(AudioPayload{
		AssetID: assetID, FileHash: hash, FileExtension: "ogg",
	})
	if _, err := h.Handle(t.Context(), &jobs.Claim{
		ID: uuid.New(), Type: jobs.TypePreviewAudio, Payload: payload,
	}); err != nil {
		t.Fatalf("preview.audio Handle: %v", err)
	}

	// The ladder ran — this is the precondition the bug did NOT break.
	if _, err := rig.storage.Backend.Stat(t.Context(), hash, "col"); err != nil {
		t.Fatalf("col variant missing, the fixture never reached the ladder: %v", err)
	}
	// ...and this is the assertion that fails on pre-#645 code.
	th := rig.thumbhashOf(t, assetID)
	if len(th) == 0 {
		t.Fatal("audio asset has no thumbhash after a successful preview job (#645)")
	}
	if _, err := thumbhash.DecodeImage(th); err != nil {
		t.Errorf("stored thumbhash does not decode: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 3D — 326 of the 618 affected assets are glb/obj/fbx.
// ---------------------------------------------------------------------------

func TestPreviewModel_StampsThumbhashFromTurntable(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "glb", []byte("glTF stand-in; the worker shim doesn't parse it"))

	shimDir := t.TempDir()
	fixture := filepath.Join(shimDir, "frame-fixture.png")
	writeTurntablePNG(t, fixture, 512)

	// three.js worker shim (#500 — Blender is no longer in the image, so
	// this stands in for `node /app/threejs/worker.mjs`). The handler
	// invokes it as `<node> <script> --input .. --workdir .. --output ..`
	// and reads poster.png + turntable/ + views/ back out of --output.
	//
	// The script path must live in a directory that also has
	// node_modules/three, because that is exactly what threeJSAvailable
	// checks — the shim has to satisfy the real availability probe, not
	// bypass it.
	workerDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workerDir, "node_modules", "three"), 0o755); err != nil {
		t.Fatal(err)
	}
	workerScript := filepath.Join(workerDir, "worker.mjs")
	if err := os.WriteFile(workerScript, []byte("// stand-in; the node shim never reads it"), 0o644); err != nil {
		t.Fatal(err)
	}
	node := writeShim(t, shimDir, "node", `
while [ $# -gt 0 ]; do
  case "$1" in
    --output)
      mkdir -p "$2/turntable" "$2/views"
      cp `+fixture+` "$2/poster.png"
      cp `+fixture+` "$2/turntable/frame_0000.png"
      cp `+fixture+` "$2/views/top.png"
      cp `+fixture+` "$2/views/bottom.png"
      shift 2 ;;
    *) shift ;;
  esac
done
exit 0
`)

	h := NewModelHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	h.NodePath = node
	h.ThreeJSScript = workerScript
	h.TempDir = t.TempDir()
	h.Frames = 1

	payload, _ := json.Marshal(ModelPayload{
		AssetID: assetID, FileHash: hash, FileExtension: "glb",
	})
	if _, err := h.Handle(t.Context(), &jobs.Claim{
		ID: uuid.New(), Type: jobs.TypePreview3D, Payload: payload,
	}); err != nil {
		t.Fatalf("preview.model Handle: %v", err)
	}

	if _, err := rig.storage.Backend.Stat(t.Context(), hash, "col"); err != nil {
		t.Fatalf("col variant missing, the fixture never reached the ladder: %v", err)
	}
	th := rig.thumbhashOf(t, assetID)
	if len(th) == 0 {
		t.Fatal("3D asset has no thumbhash after a successful preview job (#645)")
	}
	if _, err := thumbhash.DecodeImage(th); err != nil {
		t.Errorf("stored thumbhash does not decode: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The backfill sweep — what fixes the 618 assets already on disk.
// ---------------------------------------------------------------------------

func TestThumbhashBackfill_StampsFromRenderedVariantAndIsIdempotent(t *testing.T) {
	rig := newPreviewTestRig(t)
	ctx := t.Context()

	// An asset in exactly the #645 state: rendered rungs in storage,
	// thumbhash NULL, source format that is not an image.
	assetID, hash := rig.seedPreviewAsset(t, "ogg", []byte("audio bytes"))
	seedRenderedRungs(t, rig, hash)

	h := NewThumbhashBackfillHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	payload, _ := json.Marshal(ThumbhashBackfillPayload{})
	raw, err := h.Handle(ctx, &jobs.Claim{
		ID: uuid.New(), Type: JobTypeThumbhashBackfill, Payload: payload,
	})
	if err != nil {
		t.Fatalf("backfill Handle: %v", err)
	}
	var res ThumbhashBackfillResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.Stamped == 0 {
		t.Fatalf("backfill stamped nothing (scanned=%d failed=%d)", res.Scanned, res.Failed)
	}

	first := rig.thumbhashOf(t, assetID)
	if len(first) == 0 {
		t.Fatal("backfill left the asset without a thumbhash (#645)")
	}

	// Acceptance 4: a re-run must not recompute or change anything.
	raw2, err := h.Handle(ctx, &jobs.Claim{
		ID: uuid.New(), Type: JobTypeThumbhashBackfill, Payload: payload,
	})
	if err != nil {
		t.Fatalf("backfill re-run: %v", err)
	}
	var res2 ThumbhashBackfillResult
	if err := json.Unmarshal(raw2, &res2); err != nil {
		t.Fatalf("decode re-run result: %v", err)
	}
	for _, id := range []uuid.UUID{assetID} {
		if got := rig.thumbhashOf(t, id); !bytes.Equal(got, first) {
			t.Errorf("re-run changed the thumbhash: %x -> %x", first, got)
		}
	}
	// The asset must no longer be in the population at all.
	page, err := h.listPage(ctx, pgtype.UUID{}, 100)
	if err != nil {
		t.Fatalf("listPage: %v", err)
	}
	for _, row := range page {
		if uuid.UUID(row.ID.Bytes) == assetID {
			t.Error("asset still in the backfill population after being stamped")
		}
	}
}

// Never overwrite: an asset that already carries a thumbhash keeps the
// one it has, whatever the rendered bytes look like. This is the
// SetAssetThumbhashIfMissing contract the whole backfill's safety rests
// on (acceptance 4).
func TestThumbhashBackfill_NeverOverwritesAnExistingHash(t *testing.T) {
	rig := newPreviewTestRig(t)
	ctx := t.Context()

	assetID, hash := rig.seedPreviewAsset(t, "glb", []byte("model bytes"))
	seedRenderedRungs(t, rig, hash)

	sentinel := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}
	if _, err := rig.pool.Exec(ctx,
		`UPDATE assets SET thumbhash = $2 WHERE id = $1`, assetID, sentinel); err != nil {
		t.Fatalf("preset thumbhash: %v", err)
	}

	// Direct call rather than the sweep: the sweep's own population
	// filter would exclude this row, so going through the writer is the
	// only way to prove the writer itself refuses.
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	setThumbhashIfMissing(ctx, rig.pool, rig.logger, "test", assetID, img)

	if got := rig.thumbhashOf(t, assetID); !bytes.Equal(got, sentinel) {
		t.Errorf("existing thumbhash was overwritten: %x -> %x", sentinel, got)
	}
}

// seedRenderedRungs writes the rungs a preview handler would have
// produced — the state the 618 affected assets are in.
func seedRenderedRungs(t *testing.T, rig *previewTestRig, hash string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	wide := filepath.Join(dir, "wave.png")
	writeWaveformPNG(t, wide, 1024, 192)
	body, err := os.ReadFile(wide)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	for _, key := range []string{"col", "preview"} {
		if _, err := rig.storage.Backend.Put(ctx, hash, key, bytes.NewReader(body)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		if err := storage.New(rig.pool).UpsertVariant(ctx, storage.UpsertVariantParams{
			ObjectHash: hash, VariantKey: key,
			SizeBytes: int64(len(body)), ContentType: "image/png",
			Metadata: []byte("{}"),
		}); err != nil {
			t.Fatalf("upsert %s: %v", key, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Acceptance 5 — a hash taken from a mostly-transparent waveform must
// decode to something with structure, not a flat nothing.
//
// The risk is real, not hypothetical: ffmpeg renders the waveform
// white-on-fully-transparent (`WaveformBackground: "00000000"`), so most
// source pixels are alpha 0 with RGB 0, and an encoder that averaged
// straight RGB would emit a uniform near-black card.
//
// WHERE THE STRUCTURE ACTUALLY LIVES. Measured on a real stored
// waveform hash, decoded through the same `thumbhash` npm decoder the
// frontend uses: 23 bytes, 32x6 (aspect 5.0 — the wide waveform shape
// survives), alpha ranging 0..176 with 73% of pixels near-transparent,
// and *straight* colour that is CONSTANT white. So the silhouette is
// carried entirely by the ALPHA plane, not by luminance. That is the
// correct outcome and not a degradation: the `col` variant the card
// fades into is itself transparent (encodeImage promotes alpha sources
// to lossless), so a translucent white blur over the card's own
// backdrop is exactly the picture that is about to arrive. Compositing
// onto an opaque matte before hashing would make it WORSE — it would
// paint a solid rectangle the real image does not have.
//
// This test asserts against the alpha plane for that reason. Note that
// Go's thumbhash.DecodeImage returns PREMULTIPLIED RGBA, so luminance
// appears to vary here purely as a consequence of alpha; asserting on
// luminance would be asserting on alpha by accident, and would silently
// stop meaning anything if the decoder ever returned straight alpha.
// ---------------------------------------------------------------------------

func TestThumbhash_TransparentWaveformKeepsItsSilhouetteInAlpha(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "wave.png")
	writeWaveformPNG(t, p, 1024, 192)
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	hash := thumbhash.EncodeImage(src)
	if len(hash) == 0 {
		t.Fatal("EncodeImage produced no bytes for a transparent waveform")
	}
	decoded, err := thumbhash.DecodeImage(hash)
	if err != nil {
		t.Fatalf("DecodeImage: %v", err)
	}
	b := decoded.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		t.Fatal("decoded placeholder is empty")
	}

	// The decoded placeholder must keep the source's landscape shape,
	// or the card would letterbox a square blur into a 16:3 tile.
	if b.Dx() <= b.Dy() {
		t.Errorf("decoded placeholder is %dx%d — a 1024x192 source must stay landscape",
			b.Dx(), b.Dy())
	}

	var minA, maxA uint32 = 0xffff, 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			_, _, _, a := decoded.At(x, y).RGBA()
			if a < minA {
				minA = a
			}
			if a > maxA {
				maxA = a
			}
		}
	}
	if minA > 0x4000 {
		t.Errorf("decoded placeholder is opaque everywhere (min alpha %d) — the blur "+
			"would paint a solid rectangle over the card backdrop", minA)
	}
	if maxA < 0x4000 {
		t.Errorf("decoded placeholder is transparent everywhere (max alpha %d) — the "+
			"waveform silhouette was lost", maxA)
	}
	if maxA-minA < 0x2000 {
		t.Errorf("alpha is flat (range %d) — no silhouette survived the hash", maxA-minA)
	}
	if testing.Verbose() {
		fmt.Printf("thumbhash: %d bytes, decoded %dx%d, alpha %d..%d\n",
			len(hash), b.Dx(), b.Dy(), minA, maxA)
	}
}

// ladderReadbackKeys must prefer the CONTAIN rungs (aspect-preserving,
// which is what the card actually renders) over the square `col` crop,
// smallest first, and must always end with `col` as the fallback.
func TestThumbhashBackfill_PrefersContainRungOverColCrop(t *testing.T) {
	rig := newPreviewTestRig(t)
	keys := ladderReadbackKeys(t.Context(), rig.sysCfg)
	if len(keys) < 2 {
		t.Fatalf("expected contain rungs plus the col fallback, got %v", keys)
	}
	if keys[len(keys)-1] != "col" {
		t.Errorf("last candidate = %q, want the col fallback", keys[len(keys)-1])
	}
	if keys[0] == "col" {
		t.Error("col is first — a square centre-crop would be hashed into a " +
			"contain-shaped tile (#640/#646)")
	}
}
