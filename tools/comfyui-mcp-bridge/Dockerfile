# comfyui-mcp-bridge — Dockerfile.
#
# Lightweight image — Python 3.12 slim base + bridge deps. ~150 MB
# compressed. Runs as non-root; default command serves the HTTP
# transport.
#
# Build:  docker build -t comfyui-mcp-bridge:latest .
# Run:    docker run --rm -p 9201:9201 \
#           -e CMB_COMFY_URL=http://host.docker.internal:8188 \
#           -e CMB_AUTH_KIND=bearer \
#           -e CMB_AUTH_TOKEN=$(openssl rand -hex 32) \
#           comfyui-mcp-bridge:latest

FROM python:3.12-slim AS base

# Non-root user for the runtime.
RUN groupadd --system --gid 1100 bridge && \
    useradd  --system --uid 1100 --gid bridge --home /home/bridge --create-home bridge

WORKDIR /opt/comfyui-mcp-bridge

# Install build tooling only in a builder stage to keep the final
# image lean.
FROM base AS builder

RUN apt-get update && \
    apt-get install -y --no-install-recommends build-essential && \
    rm -rf /var/lib/apt/lists/*

COPY pyproject.toml ./
COPY src/ ./src/
COPY README.md ./

RUN pip install --no-cache-dir --upgrade pip && \
    pip wheel --no-cache-dir --wheel-dir /wheels .

# ----- Runtime image -----
FROM base AS runtime

COPY --from=builder /wheels /wheels
RUN pip install --no-cache-dir --upgrade pip && \
    pip install --no-cache-dir /wheels/comfyui_mcp_bridge*.whl && \
    pip install --no-cache-dir /wheels/*.whl && \
    rm -rf /wheels

# Drop privileges + workdir for operator drop-ins.
USER bridge
WORKDIR /home/bridge

# Operator-overridable: drop custom workflow JSONs into
# /home/bridge/workflows; the bundled defaults under
# /opt/.../workflows still load and the bind-mount overrides
# them on name collision.
ENV CMB_EXTRA_WORKFLOWS_DIR=/home/bridge/workflows \
    CMB_HTTP_HOST=0.0.0.0 \
    CMB_HTTP_PORT=9201

EXPOSE 9201

# HEALTHCHECK hits the GET / sanity endpoint — returns name +
# protocol_version + workflow count. If the bridge can't even
# serve its banner, the container is unhealthy.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD python -c "import urllib.request, sys; \
        r = urllib.request.urlopen('http://127.0.0.1:9201/', timeout=3); \
        sys.exit(0 if r.status == 200 else 1)"

ENTRYPOINT ["comfyui-mcp-bridge"]
