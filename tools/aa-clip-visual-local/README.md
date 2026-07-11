# aa-clip-visual-local

Optional sidecar that serves CLIP visual embeddings for artist-alley's reverse-image
search endpoint (`POST /search/by-image`). Phase 1.16.B-3-followup — closes #183.

## What it is

A small HTTP service that wraps [OpenCLIP](https://github.com/mlfoundations/open_clip)
`ViT-L/14` (OpenAI checkpoint, 768-dim) behind two endpoints:

- `POST /embed/image` — multipart upload → `{"embedding": [...], "dim": 768, "model": "ViT-L-14", "checkpoint": "openai"}`
- `GET /health` — liveness probe (200 when the model is loaded)
- `GET /version` — semver + upstream model versions

**AA never imports this code.** The sidecar is a separate process; the Go app
talks to it over HTTP. Operators who install this sidecar get reverse-image
search; operators who don't get the existing 501 stub response with a helpful
error body.

## Design decisions

- **OpenCLIP ViT-L/14, OpenAI checkpoint.** 768-dim, ~890M params, widest ecosystem
  support. Not laion-2b or datacomp — those checkpoints have different embedding
  distributions; operators who want them can override via `AA_CLIP_MODEL` and
  `AA_CLIP_CHECKPOINT`.
- **Fat image with baked checkpoint.** Model checkpoint downloads at image build
  time, not runtime. Deploy is one docker pull, no first-boot latency, and
  air-gapped installs work. Trade-off: image is ~2 GB compressed.
- **CPU-only default.** Inference takes ~200–500 ms per image on modern CPU;
  ~20–50 ms on GPU. GPU migration is documented below but not automated.
- **Text encoder is deliberately NOT exposed.** This sidecar embeds images
  only. AA's existing text embedding path (Ollama nomic-embed-text) is
  untouched; the two embedding spaces coexist and are never cosine-compared.
- **Sidecar-visible-to-AA via Docker Compose profile `visual-search`.** Same
  optional-profile pattern as `comfyui-mcp-bridge` (aa's ComfyUI sidecar).

## Install

```bash
# From artist-alley repo root:
docker compose --profile visual-search up -d aa-clip-visual-local

# Wait for the model to load (~10–30 s cold on first boot):
docker compose logs -f aa-clip-visual-local | grep -m1 "clip visual sidecar ready"

# Verify:
curl http://localhost:8402/health
# {"status": "ok", "model": "ViT-L-14", "checkpoint": "openai", "dim": 768}
```

Then in AA sysconfig (via `/admin/system/*`):

- `search.visual.enabled = true`
- `search.visual.sidecar_url = http://aa-clip-visual-local:8402` (Docker DNS)

Restart the AA app container. Boot logs should show:

```
INFO  search.visual.provider.registered  url=... model=ViT-L-14 dim=768
```

`POST /search/by-image` now serves 200 instead of the 501 stub. Existing image
assets don't have embeddings yet — trigger a backfill via `POST /admin/search/reindex`
with `modality=visual` (see the AA docs for the exact shape).

## GPU migration

Swap the base image in the Dockerfile:

```diff
- FROM python:3.12-slim AS base
+ FROM nvidia/cuda:12.1.0-runtime-ubuntu22.04 AS base
+ RUN apt-get update && apt-get install -y python3.12 python3-pip
```

Add `--gpus all` to the `docker compose` invocation or set `deploy.resources.reservations.devices`
in `docker-compose.yml`. The `open_clip_torch` install auto-detects CUDA when
available; the app code doesn't need to change.

## Configuration (env vars)

| Env var | Default | Meaning |
|---|---|---|
| `AA_CLIP_MODEL` | `ViT-L-14` | OpenCLIP model name |
| `AA_CLIP_CHECKPOINT` | `openai` | Pretrained checkpoint |
| `AA_CLIP_HOST` | `0.0.0.0` | Listen host |
| `AA_CLIP_PORT` | `8402` | Listen port |
| `AA_CLIP_MAX_UPLOAD_BYTES` | `10485760` | 10 MB per-request max |

## Endpoints

### `GET /health`

Returns 200 when the model is loaded and ready. AA polls this at boot to decide
whether to register the visual provider.

```json
{"status": "ok", "model": "ViT-L-14", "checkpoint": "openai", "dim": 768}
```

### `GET /version`

Semver + upstream versions. Consumed by `/admin/search/health` for the
"Visual Search" subsystem card.

```json
{"sidecar_version": "1.0.0", "torch": "2.4.1", "open_clip_torch": "2.26.1"}
```

### `POST /embed/image`

Multipart upload with a single `file` field. Content-type must be `image/*`
(JPEG, PNG, WebP tested; anything Pillow reads should work).

```json
{
  "embedding": [0.0142, -0.0031, ...],
  "dim": 768,
  "model": "ViT-L-14",
  "checkpoint": "openai"
}
```

Errors:

- `400` — no file, non-image content-type, or Pillow couldn't decode
- `413` — file larger than `AA_CLIP_MAX_UPLOAD_BYTES`
- `503` — model still loading (retry after `Retry-After` seconds)

## Development

Local run without Docker (for iteration):

```bash
cd tools/aa-clip-visual-local/
pip install -e .
uvicorn aa_clip_visual_local.main:app --host 0.0.0.0 --port 8402
```

First boot downloads the CLIP checkpoint (~1.7 GB) to `~/.cache/clip`. In the
Dockerfile this happens at build time so runtime boots are fast.

Tests:

```bash
pip install -e '.[dev]'
pytest tests/
```

## Rebuild + republish

Image tags follow AA's own semver. Bumping the model or torch versions warrants
a minor bump; adding endpoints without breaking existing ones is a patch. AA's
Go-side provider pins the sidecar's `dim` + `model` fields — if the sidecar
starts returning a different dim, the provider refuses to register (guards
against accidental model swaps producing incompatible embeddings).

## References

- OpenCLIP: https://github.com/mlfoundations/open_clip
- CLIP paper: https://arxiv.org/abs/2103.00020
- AA reverse-image search phase brief: #183
