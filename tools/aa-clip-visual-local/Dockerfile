# aa-clip-visual-local — CLIP visual encoder sidecar.
#
# Fat image with baked CLIP ViT-L/14 checkpoint (~1.7 GB). Model
# downloads at BUILD time, not runtime — deploy is one docker pull,
# first-boot latency is model-load-only (~10–30 s cold), and
# air-gapped installs work.
#
# Build:   docker build -t aa-clip-visual-local:latest .
# Run:     docker run --rm -p 8402:8402 aa-clip-visual-local:latest
# Compose: docker compose --profile visual-search up -d aa-clip-visual-local
#
# GPU variant: swap the base image to nvidia/cuda:12.1.0-runtime-ubuntu22.04
# and add `--gpus all` to the docker run. open_clip auto-detects CUDA.

FROM python:3.12-slim AS base

RUN groupadd --system --gid 1101 clip && \
    useradd  --system --uid 1101 --gid clip --home /home/clip --create-home clip

WORKDIR /opt/aa-clip-visual-local

# ---- Builder stage: build the wheel + pre-download the model ----
FROM base AS builder

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    rm -rf /var/lib/apt/lists/*

COPY pyproject.toml README.md ./
COPY src/ ./src/

RUN pip install --no-cache-dir --upgrade pip && \
    pip wheel --no-cache-dir --wheel-dir /wheels .

# Pre-download the CLIP checkpoint into the builder's HF cache so
# it lands in the final image alongside the wheels. Runtime container
# copies both together — no network at first boot.
RUN pip install --no-cache-dir /wheels/*.whl && \
    python -c "\
import open_clip; \
_ = open_clip.create_model_and_transforms('ViT-L-14', pretrained='openai')" \
    && python -c "import os; \
os.makedirs('/wheels/clip-cache', exist_ok=True); \
import shutil; \
shutil.copytree(os.path.expanduser('~/.cache/clip'), '/wheels/clip-cache', dirs_exist_ok=True)"

# ---- Runtime image ----
FROM base AS runtime

COPY --from=builder /wheels /wheels
COPY --from=builder /wheels/clip-cache /home/clip/.cache/clip

RUN pip install --no-cache-dir --upgrade pip && \
    pip install --no-cache-dir /wheels/*.whl && \
    rm -rf /wheels && \
    chown -R clip:clip /home/clip/.cache

USER clip
WORKDIR /home/clip

ENV AA_CLIP_HOST=0.0.0.0 \
    AA_CLIP_PORT=8402 \
    AA_CLIP_MODEL=ViT-L-14 \
    AA_CLIP_CHECKPOINT=openai \
    PYTHONUNBUFFERED=1

EXPOSE 8402

# Model load is async — the healthcheck stays "starting" for the
# ~15-30 s cold-boot window. AA's provider-registration retry loop
# tolerates that; direct callers see 503 with Retry-After.
HEALTHCHECK --interval=15s --timeout=5s --start-period=45s --retries=3 \
    CMD python -c "\
import urllib.request, sys; \
r = urllib.request.urlopen('http://127.0.0.1:8402/health', timeout=3); \
sys.exit(0 if r.status == 200 else 1)"

ENTRYPOINT ["aa-clip-visual-local"]
