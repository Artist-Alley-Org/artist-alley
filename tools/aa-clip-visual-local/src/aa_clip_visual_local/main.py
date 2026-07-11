"""aa-clip-visual-local — HTTP surface for CLIP visual encoding.

Ships two production endpoints (`POST /embed/image`, `GET /health`) and
one informational endpoint (`GET /version`). The AA app polls `/health`
at boot to decide whether to register the visual provider.

Design notes:

- Model loads at process boot, not per-request. Cold-boot latency
  (~10–30 s) shows up in `/health` returning 503 with a Retry-After
  header until load completes.
- Inference is CPU-only by default; open_clip auto-detects CUDA when
  the container is launched with `--gpus all`.
- No auth. AA reaches the sidecar over the Docker network; operators
  who expose the sidecar externally should front it with their own
  reverse proxy.
"""

from __future__ import annotations

import io
import logging
import os
import sys
import threading
import time
from dataclasses import dataclass, field
from typing import Any

from fastapi import FastAPI, File, HTTPException, Request, UploadFile
from fastapi.responses import JSONResponse

from . import __version__

log = logging.getLogger("aa_clip_visual_local")
logging.basicConfig(
    level=os.environ.get("AA_CLIP_LOG_LEVEL", "INFO"),
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)


@dataclass
class Config:
    model: str = os.environ.get("AA_CLIP_MODEL", "ViT-L-14")
    checkpoint: str = os.environ.get("AA_CLIP_CHECKPOINT", "openai")
    host: str = os.environ.get("AA_CLIP_HOST", "0.0.0.0")
    port: int = int(os.environ.get("AA_CLIP_PORT", "8402"))
    max_upload_bytes: int = int(os.environ.get("AA_CLIP_MAX_UPLOAD_BYTES", str(10 * 1024 * 1024)))


@dataclass
class ModelHolder:
    """Lazy-loaded CLIP model + preprocess pipeline.

    Held on the ASGI app's state so tests can inject a fake without
    importing torch. Never accessed except via lock — the model
    loads on a background thread at startup; `/embed/image` blocks
    on the ready event before consuming.
    """
    cfg: Config
    _model: Any = None
    _preprocess: Any = None
    _tokenizer: Any = None
    _device: str = "cpu"
    ready: threading.Event = field(default_factory=threading.Event)
    error: str | None = None
    dim: int = 0
    torch_version: str = ""
    open_clip_version: str = ""

    def load(self) -> None:
        """Loads the CLIP model in-process. Blocks until ready."""
        try:
            import torch
            import open_clip
            self.torch_version = torch.__version__
            self.open_clip_version = open_clip.__version__
            self._device = "cuda" if torch.cuda.is_available() else "cpu"
            log.info(
                "loading clip model=%s checkpoint=%s device=%s",
                self.cfg.model, self.cfg.checkpoint, self._device,
            )
            model, _, preprocess = open_clip.create_model_and_transforms(
                self.cfg.model, pretrained=self.cfg.checkpoint, device=self._device,
            )
            model.eval()
            # Probe dimension by encoding a 1×3×224×224 zero tensor —
            # cheaper than reading the model's config attributes across
            # open_clip versions where the config field names shift.
            with torch.no_grad():
                probe = torch.zeros(1, 3, 224, 224, device=self._device)
                self.dim = int(model.encode_image(probe).shape[-1])
            self._model = model
            self._preprocess = preprocess
            log.info("clip visual sidecar ready dim=%d", self.dim)
            self.ready.set()
        except Exception as e:  # pragma: no cover — surfaces at /health
            self.error = f"{type(e).__name__}: {e}"
            log.exception("clip model load failed")
            self.ready.set()  # unblock waiters; they'll see .error

    def embed_bytes(self, data: bytes) -> list[float]:
        """Preprocess + encode + normalize one image. Returns a list
        of `dim` floats (unit-length so cosine similarity = dot product)."""
        import torch
        from PIL import Image
        img = Image.open(io.BytesIO(data)).convert("RGB")
        tensor = self._preprocess(img).unsqueeze(0).to(self._device)
        with torch.no_grad():
            vec = self._model.encode_image(tensor)
            vec = vec / vec.norm(dim=-1, keepdim=True)
        return vec.squeeze(0).cpu().tolist()


def create_app(cfg: Config | None = None, holder: ModelHolder | None = None) -> FastAPI:
    """Constructs the FastAPI app. Tests build the app with a fake
    holder that skips torch entirely."""
    cfg = cfg or Config()
    holder = holder or ModelHolder(cfg=cfg)

    app = FastAPI(
        title="aa-clip-visual-local",
        version=__version__,
        docs_url=None,     # no Swagger UI — this is a machine-only surface
        redoc_url=None,
    )
    app.state.cfg = cfg
    app.state.holder = holder

    # Kick off model load in background at startup so /health becomes
    # useful quickly. Tests inject a preloaded holder + skip this.
    @app.on_event("startup")
    def _load_model() -> None:  # pragma: no cover — startup hook
        if holder.ready.is_set():
            return
        t = threading.Thread(target=holder.load, daemon=True, name="clip-load")
        t.start()

    @app.get("/health")
    def health() -> JSONResponse:
        if not holder.ready.is_set():
            return JSONResponse(
                {"status": "loading"},
                status_code=503,
                headers={"Retry-After": "10"},
            )
        if holder.error:
            return JSONResponse(
                {"status": "error", "error": holder.error},
                status_code=503,
            )
        return JSONResponse({
            "status": "ok",
            "model": cfg.model,
            "checkpoint": cfg.checkpoint,
            "dim": holder.dim,
        })

    @app.get("/version")
    def version() -> dict:
        return {
            "sidecar_version": __version__,
            "torch": holder.torch_version,
            "open_clip_torch": holder.open_clip_version,
            "model": cfg.model,
            "checkpoint": cfg.checkpoint,
            "dim": holder.dim,
        }

    @app.post("/embed/image")
    async def embed_image(request: Request, file: UploadFile = File(...)) -> dict:
        if not holder.ready.is_set():
            raise HTTPException(503, "model still loading")
        if holder.error:
            raise HTTPException(503, f"model unavailable: {holder.error}")
        if not (file.content_type or "").startswith("image/"):
            raise HTTPException(400, f"content-type must be image/*, got {file.content_type!r}")
        data = await file.read(cfg.max_upload_bytes + 1)
        if len(data) > cfg.max_upload_bytes:
            raise HTTPException(413, f"file exceeds max {cfg.max_upload_bytes} bytes")
        if not data:
            raise HTTPException(400, "empty upload")
        try:
            vec = holder.embed_bytes(data)
        except Exception as e:
            log.warning("embed failure: %s", e)
            raise HTTPException(400, f"decode/embed failed: {type(e).__name__}: {e}") from e
        return {
            "embedding": vec,
            "dim": holder.dim,
            "model": cfg.model,
            "checkpoint": cfg.checkpoint,
        }

    return app


# Uvicorn's --factory flag calls create_app() with no args at module
# import time; tests import create_app directly and pass their own
# preloaded holder. Uvicorn's default path uses the module-level app.
app = create_app()


def cli() -> None:  # pragma: no cover
    """Console-script entry point: `aa-clip-visual-local` runs uvicorn."""
    import uvicorn
    cfg = Config()
    uvicorn.run(
        "aa_clip_visual_local.main:app",
        host=cfg.host,
        port=cfg.port,
        log_level=os.environ.get("AA_CLIP_LOG_LEVEL", "info").lower(),
    )


if __name__ == "__main__":  # pragma: no cover
    cli()
