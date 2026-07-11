"""ComfyUI HTTP + WebSocket client.

Knows nothing about MCP — pure wrapper around ComfyUI's REST API
(``POST /prompt``, ``GET /history/{id}``, ``GET /view``) and its
WebSocket progress stream. Handler code composes against this to
queue a workflow + wait for the result + fetch the output bytes.

The client is async-first (httpx + websockets) because ComfyUI
generation is dominated by wall-clock time, not CPU on this side
— blocking the event loop on `requests.get` would prevent
serving other concurrent MCP calls.
"""

from __future__ import annotations

import asyncio
import json
import logging
import uuid
from typing import Any
from urllib.parse import urlparse, urlunparse

import httpx
import websockets

logger = logging.getLogger(__name__)


class ComfyError(Exception):
    """Raised on any ComfyUI-side failure. Carries the underlying
    HTTP status / WS close code in ``.status`` when available so
    the handler layer can classify retryable vs permanent.
    """

    def __init__(self, message: str, *, status: int | None = None, transient: bool = False) -> None:
        super().__init__(message)
        self.status = status
        self.transient = transient


class ComfyClient:
    """Minimal ComfyUI client. One instance per bridge process —
    httpx + websockets clients are thread-safe across asyncio
    tasks.
    """

    def __init__(self, base_url: str, *, timeout_s: float = 300.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout_s = timeout_s
        # Long timeout because the bridge handler waits on the WS
        # for the generation to finish; the underlying httpx pool
        # only needs short reads (queue, history, view).
        self._http = httpx.AsyncClient(timeout=30.0)
        self._ws_url = _http_to_ws(self.base_url) + "/ws"
        # Stable client_id so ComfyUI threads our queued prompts
        # through the same WS subscription.
        self._client_id = uuid.uuid4().hex

    async def aclose(self) -> None:
        await self._http.aclose()

    # ------------------------------------------------------------------
    # Queue + execution
    # ------------------------------------------------------------------

    async def queue_prompt(self, workflow_json: dict[str, Any]) -> str:
        """POST a workflow to /prompt and return the prompt_id
        ComfyUI assigned. Pure queueing — doesn't wait for the
        workflow to run.
        """
        body = {"prompt": workflow_json, "client_id": self._client_id}
        try:
            resp = await self._http.post(f"{self.base_url}/prompt", json=body)
        except httpx.HTTPError as e:
            raise ComfyError(f"ComfyUI unreachable: {e}", transient=True) from e

        if resp.status_code >= 500:
            raise ComfyError(
                f"ComfyUI server error: {resp.status_code} {resp.text[:200]}",
                status=resp.status_code,
                transient=True,
            )
        if resp.status_code >= 400:
            # 400-class is operator-side (bad workflow, missing
            # model). Don't retry.
            raise ComfyError(
                f"ComfyUI rejected workflow: {resp.status_code} {resp.text[:500]}",
                status=resp.status_code,
                transient=False,
            )

        try:
            data = resp.json()
        except json.JSONDecodeError as e:
            raise ComfyError(f"ComfyUI returned non-JSON: {resp.text[:200]}", transient=True) from e

        prompt_id = data.get("prompt_id")
        if not isinstance(prompt_id, str):
            raise ComfyError(
                f"ComfyUI /prompt response missing prompt_id: {data!r}", transient=False
            )
        logger.info("comfy.queued prompt_id=%s", prompt_id)
        return prompt_id

    async def wait_for_completion(self, prompt_id: str, *, timeout_s: float | None = None) -> None:
        """Block on the WS progress stream until ComfyUI reports
        the prompt finished. Raises ComfyError on timeout or WS
        close.

        Uses the ``executed`` message ComfyUI emits when the
        terminal node of the workflow finishes — once we see one
        for our prompt_id we're done. The ``executing`` message
        with ``node=None`` also indicates completion in older
        ComfyUI versions; we check for both.
        """
        deadline = timeout_s if timeout_s is not None else self.timeout_s
        url = f"{self._ws_url}?clientId={self._client_id}"

        try:
            async with websockets.connect(url, ping_interval=20, close_timeout=5) as ws:
                async with asyncio.timeout(deadline):
                    async for raw in ws:
                        if isinstance(raw, bytes):
                            # Binary preview frames — ignore;
                            # we want the JSON status messages.
                            continue
                        try:
                            msg = json.loads(raw)
                        except json.JSONDecodeError:
                            continue
                        if msg.get("type") == "executing":
                            data = msg.get("data") or {}
                            if data.get("prompt_id") == prompt_id and data.get("node") is None:
                                # Terminal node done.
                                return
                        elif msg.get("type") == "executed":
                            data = msg.get("data") or {}
                            if data.get("prompt_id") == prompt_id:
                                return
                        elif msg.get("type") == "execution_error":
                            data = msg.get("data") or {}
                            if data.get("prompt_id") == prompt_id:
                                node = data.get("node_type", "unknown")
                                err = data.get("exception_message", "unspecified")
                                raise ComfyError(
                                    f"ComfyUI workflow failed at {node}: {err}",
                                    transient=False,
                                )
        except TimeoutError as e:
            raise ComfyError(
                f"ComfyUI did not finish within {deadline:.0f}s", transient=True
            ) from e
        except websockets.WebSocketException as e:
            raise ComfyError(f"ComfyUI WS connection lost: {e}", transient=True) from e

    async def get_history(self, prompt_id: str) -> dict[str, Any]:
        """Fetch the execution history for prompt_id — tells us
        which output node(s) emitted bytes and where to fetch
        them from /view.
        """
        resp = await self._http.get(f"{self.base_url}/history/{prompt_id}")
        if resp.status_code != 200:
            raise ComfyError(
                f"ComfyUI /history returned {resp.status_code}",
                status=resp.status_code,
                transient=resp.status_code >= 500,
            )
        hist = resp.json()
        if prompt_id not in hist:
            raise ComfyError(
                f"prompt_id {prompt_id} not in history yet (race?)", transient=True
            )
        return hist[prompt_id]

    async def fetch_view(self, filename: str, subfolder: str = "", folder_type: str = "output") -> bytes:
        """Fetch one output image's bytes via /view."""
        params = {"filename": filename, "subfolder": subfolder, "type": folder_type}
        resp = await self._http.get(f"{self.base_url}/view", params=params)
        if resp.status_code != 200:
            raise ComfyError(
                f"ComfyUI /view returned {resp.status_code} for {filename}",
                status=resp.status_code,
                transient=resp.status_code >= 500,
            )
        return resp.content

    # ------------------------------------------------------------------
    # Image upload
    # ------------------------------------------------------------------

    async def upload_image(self, name: str, image_bytes: bytes, content_type: str) -> str:
        """POST bytes to /upload/image so a downstream LoadImage
        node can reference them by ``name``. Returns the name
        ComfyUI actually filed the bytes under (usually equal to
        ``name`` but the server may rename on collision).
        """
        files = {"image": (name, image_bytes, content_type)}
        resp = await self._http.post(f"{self.base_url}/upload/image", files=files)
        if resp.status_code != 200:
            raise ComfyError(
                f"ComfyUI /upload/image returned {resp.status_code}: {resp.text[:200]}",
                status=resp.status_code,
                transient=resp.status_code >= 500,
            )
        try:
            data = resp.json()
        except json.JSONDecodeError as e:
            raise ComfyError(
                f"ComfyUI /upload/image returned non-JSON: {resp.text[:200]}", transient=True
            ) from e
        name_out = data.get("name")
        if not isinstance(name_out, str):
            raise ComfyError(
                f"ComfyUI /upload/image response missing name: {data!r}", transient=False
            )
        return name_out


def _http_to_ws(url: str) -> str:
    """ComfyUI's WebSocket lives at the same host:port as the
    HTTP server but on ws:// (or wss:// over TLS)."""
    parsed = urlparse(url)
    scheme = "wss" if parsed.scheme == "https" else "ws"
    return urlunparse((scheme, parsed.netloc, parsed.path, "", "", ""))
