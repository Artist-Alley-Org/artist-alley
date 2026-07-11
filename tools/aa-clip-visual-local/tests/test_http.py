"""HTTP-surface tests for the aa-clip-visual-local sidecar.

Uses a fake ModelHolder so the tests run without torch — CI can
validate the HTTP contract cheaply. The real model loads at container
startup in production; a separate smoke test could exercise the full
stack against a running image.
"""

import io
import threading
import pytest
from fastapi.testclient import TestClient

from aa_clip_visual_local.main import Config, ModelHolder, create_app


def _make_holder(*, ready: bool = True, err: str | None = None, dim: int = 768):
    """Build a fake holder that returns a deterministic embedding."""
    h = ModelHolder(cfg=Config())
    h.torch_version = "fake-2.0"
    h.open_clip_version = "fake-2.0"
    h.dim = dim
    h.error = err
    if ready:
        h.ready.set()
    else:
        h.ready = threading.Event()  # unset — /health returns 503
    def fake_embed(_data: bytes) -> list[float]:
        return [0.1] * dim
    h.embed_bytes = fake_embed  # type: ignore[assignment]
    return h


def test_health_ok_when_ready():
    app = create_app(cfg=Config(), holder=_make_holder(ready=True))
    client = TestClient(app)
    r = client.get("/health")
    assert r.status_code == 200
    body = r.json()
    assert body["status"] == "ok"
    assert body["dim"] == 768
    assert body["model"] == "ViT-L-14"
    assert body["checkpoint"] == "openai"


def test_health_503_when_still_loading():
    app = create_app(cfg=Config(), holder=_make_holder(ready=False))
    client = TestClient(app)
    r = client.get("/health")
    assert r.status_code == 503
    assert r.headers["Retry-After"] == "10"
    assert r.json() == {"status": "loading"}


def test_health_503_on_model_error():
    app = create_app(cfg=Config(), holder=_make_holder(err="OOM"))
    client = TestClient(app)
    r = client.get("/health")
    assert r.status_code == 503
    body = r.json()
    assert body["status"] == "error"
    assert "OOM" in body["error"]


def test_version_returns_semver_and_upstream():
    app = create_app(cfg=Config(), holder=_make_holder())
    client = TestClient(app)
    r = client.get("/version")
    assert r.status_code == 200
    body = r.json()
    assert body["sidecar_version"]
    assert body["torch"] == "fake-2.0"
    assert body["open_clip_torch"] == "fake-2.0"
    assert body["dim"] == 768


def test_embed_image_happy_path():
    app = create_app(cfg=Config(), holder=_make_holder(dim=768))
    client = TestClient(app)
    files = {"file": ("test.jpg", io.BytesIO(b"\xff\xd8\xff\xe0" + b"\x00" * 100), "image/jpeg")}
    r = client.post("/embed/image", files=files)
    assert r.status_code == 200
    body = r.json()
    assert body["dim"] == 768
    assert len(body["embedding"]) == 768
    assert body["model"] == "ViT-L-14"


def test_embed_image_rejects_non_image_content_type():
    app = create_app(cfg=Config(), holder=_make_holder())
    client = TestClient(app)
    files = {"file": ("test.txt", io.BytesIO(b"not an image"), "text/plain")}
    r = client.post("/embed/image", files=files)
    assert r.status_code == 400
    assert "content-type" in r.json()["detail"].lower()


def test_embed_image_rejects_oversized_upload():
    app = create_app(cfg=Config(max_upload_bytes=100), holder=_make_holder())
    client = TestClient(app)
    files = {"file": ("big.jpg", io.BytesIO(b"\x00" * 200), "image/jpeg")}
    r = client.post("/embed/image", files=files)
    assert r.status_code == 413


def test_embed_image_503_when_model_not_ready():
    app = create_app(cfg=Config(), holder=_make_holder(ready=False))
    client = TestClient(app)
    files = {"file": ("test.jpg", io.BytesIO(b"\xff\xd8\xff\xe0"), "image/jpeg")}
    r = client.post("/embed/image", files=files)
    assert r.status_code == 503


def test_embed_image_400_on_decode_error():
    """Empty upload trips the empty-body guard; other decode failures
    (bad bytes, unsupported format) surface as 400 via the exception
    handler."""
    app = create_app(cfg=Config(), holder=_make_holder())
    client = TestClient(app)
    files = {"file": ("empty.jpg", io.BytesIO(b""), "image/jpeg")}
    r = client.post("/embed/image", files=files)
    assert r.status_code == 400
    assert "empty" in r.json()["detail"].lower()
