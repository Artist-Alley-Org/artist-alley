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
- **Fat image with baked checkpoint.** The design downloads the model checkpoint
  at image build time, not runtime, so a built image needs no network access at
  boot and air-gapped installs work. Trade-off: the image is large. The image is
  not published, and the build currently fails (see Install).
- **CPU-only default.** Inference takes ~200–500 ms per image on modern CPU;
  ~20–50 ms on GPU. GPU migration is documented below but not automated.
- **Text encoder is deliberately NOT exposed.** This sidecar embeds images
  only. AA's existing text embedding path (Ollama nomic-embed-text) is
  untouched; the two embedding spaces coexist and are never cosine-compared.
- **Runs beside AA as its own container.** There is no Compose profile or
  service for it in the repo.

## Install

There is no working install path from this directory today:

- The image is not published by any workflow, so it has to be built here, and
  the build currently fails. The Dockerfile copies the downloaded model from
  `~/.cache/clip`, but the `open_clip_torch` version the build now resolves
  (3.3.0, allowed by `pyproject.toml`'s `>=2.30,<4`) downloads the OpenAI
  checkpoint through the Hugging Face hub instead, so that directory does not
  exist and the copy step fails.
- There is no Docker Compose profile or service for the sidecar.

For reference, what the app side needs once a sidecar is running:

- Visual search is off by default, and there is not yet an admin screen or API
  to turn it on. The setting is the `search` entry in the `system_config`
  table: `visual.enabled`, and `visual.sidecar_url`, which defaults to
  `http://aa-clip-visual-local:8402` (so a container named
  `aa-clip-visual-local` on the app's Docker network needs no URL change).
- The app registers the sidecar at boot, so it must be restarted after the
  setting changes.
- Once the sidecar is registered, reverse-image search (`POST /search/by-image`)
  is served by it instead of the 501 stub, and existing images can be embedded
  from the admin page `/admin/search/visual-backfill`.

## GPU migration

Swap the base image in the Dockerfile:

```diff
- FROM python:3.12-slim AS base
+ FROM nvidia/cuda:12.1.0-runtime-ubuntu22.04 AS base
+ RUN apt-get update && apt-get install -y python3.12 python3-pip
```

Add `--gpus all` to the `docker run` command. The `open_clip_torch` install
auto-detects CUDA when available; the app code doesn't need to change.

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
