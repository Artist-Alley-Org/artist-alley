"""MCP server entry points — HTTP (canonical AA integration) +
stdio (Claude Desktop / single-process clients).

The bridge speaks MCP's JSON-RPC 2.0 protocol directly rather than
linking against the official ``mcp`` SDK at this layer — gives us
freedom to upgrade SDK versions without churning the wire shape AA
already depends on. The methods we implement:

  - ``initialize`` — handshake; returns protocol version + server
    info + the tools capability so clients know we'll respond to
    tools/list.
  - ``tools/list`` — returns every typed op + every operator-
    supplied workflow:<name> as a tool with its input schema.
  - ``tools/call`` — dispatches to the handler matching the
    requested tool name; returns an MCP content array.
  - ``ping`` — JSON-RPC health probe AA's per-server checker hits.

Anything else gets a -32601 (method not found).
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import signal
import sys
from typing import Any, Awaitable, Callable

from .comfy_client import ComfyClient
from .config import BridgeConfig
from .handlers import (
    TYPED_OPS,
    ToolError,
    handle_dynamic_workflow,
    handle_e2_stub,
    handle_img2img,
)
from .workflows_loader import Workflow, load_workflows

logger = logging.getLogger(__name__)


PROTOCOL_VERSION = "2024-11-05"
SERVER_NAME = "comfyui-mcp-bridge"
SERVER_VERSION = "0.1.0"


class Server:
    """The MCP server's request dispatcher. Holds the Comfy client
    + the workflow registry; routes JSON-RPC requests to the
    matching method.

    Stateless across requests beyond those two — every tool call
    builds its own placeholder map + uploads source bytes anew.
    """

    def __init__(self, cfg: BridgeConfig) -> None:
        self.cfg = cfg
        self.client = ComfyClient(cfg.comfy_url, timeout_s=cfg.comfy_timeout_s)
        dirs = [cfg.workflows_dir]
        if cfg.extra_workflows_dir is not None:
            dirs.append(cfg.extra_workflows_dir)
        self.workflows = load_workflows(*dirs)

    async def aclose(self) -> None:
        await self.client.aclose()

    # ------------------------------------------------------------------
    # JSON-RPC dispatch
    # ------------------------------------------------------------------

    async def handle(self, request: dict[str, Any]) -> dict[str, Any] | None:
        """Dispatch a single JSON-RPC request. Returns the JSON-RPC
        response dict, or None for a notification (no id).
        """
        rpc_id = request.get("id")
        method = request.get("method")
        params = request.get("params") or {}

        if not isinstance(method, str):
            return _rpc_error(rpc_id, -32600, "Invalid Request: method must be a string")

        try:
            handler = self._method_handlers().get(method)
            if handler is None:
                if rpc_id is None:
                    return None  # ignore unknown notifications
                return _rpc_error(rpc_id, -32601, f"Method not found: {method}")
            result = await handler(params)
        except ToolError as e:
            return _rpc_error(rpc_id, -32000, str(e))
        except Exception as e:  # noqa: BLE001
            logger.exception("unhandled error in method %s", method)
            return _rpc_error(rpc_id, -32603, f"Internal error: {e}")

        if rpc_id is None:
            return None
        return {"jsonrpc": "2.0", "id": rpc_id, "result": result}

    def _method_handlers(self) -> dict[str, Callable[[dict[str, Any]], Awaitable[Any]]]:
        return {
            "initialize": self._initialize,
            "ping": self._ping,
            "tools/list": self._tools_list,
            "tools/call": self._tools_call,
        }

    # ------------------------------------------------------------------
    # Method implementations
    # ------------------------------------------------------------------

    async def _initialize(self, params: dict[str, Any]) -> dict[str, Any]:
        _ = params
        return {
            "protocolVersion": PROTOCOL_VERSION,
            "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION},
            "capabilities": {"tools": {"listChanged": False}},
        }

    async def _ping(self, params: dict[str, Any]) -> dict[str, Any]:
        _ = params
        return {}

    async def _tools_list(self, params: dict[str, Any]) -> dict[str, Any]:
        _ = params
        tools: list[dict[str, Any]] = []

        # Typed ops first — operators see the named surface at the
        # top of admin listings.
        for name, meta in TYPED_OPS.items():
            tools.append(
                {
                    "name": name,
                    "description": meta["description"],
                    "inputSchema": meta["schema"],
                }
            )

        # Then operator-supplied workflows. Skip the names already
        # claimed by typed ops (they're served by typed handlers,
        # not the dynamic shell).
        typed_names = set(TYPED_OPS.keys())
        for name, wf in self.workflows.items():
            if name in typed_names:
                continue
            tools.append(
                {
                    "name": f"workflow:{name}",
                    "description": (
                        wf.meta.get("description")
                        or f"Operator-supplied ComfyUI workflow ({name})."
                    ),
                    "inputSchema": _schema_for_workflow(wf),
                }
            )

        return {"tools": tools}

    async def _tools_call(self, params: dict[str, Any]) -> dict[str, Any]:
        name = params.get("name")
        args = params.get("arguments") or {}
        if not isinstance(name, str):
            raise ToolError("tools/call: name must be a string")

        # Typed op?
        if name in TYPED_OPS:
            return await self._call_typed(name, args)

        # Dynamic workflow:<n> op?
        if name.startswith("workflow:"):
            wf_name = name.split(":", 1)[1]
            wf = self.workflows.get(wf_name)
            if wf is None:
                raise ToolError(f"unknown workflow: {wf_name!r}")
            return await handle_dynamic_workflow(client=self.client, workflow=wf, args=args)

        raise ToolError(f"unknown tool: {name!r}")

    async def _call_typed(self, name: str, args: dict[str, Any]) -> dict[str, Any]:
        wf_name = TYPED_OPS[name]["workflow"]
        wf = self.workflows.get(wf_name)
        if wf is None:
            raise ToolError(
                f"no workflow JSON for op {name!r} (expected {wf_name}.json in workflows dir)"
            )
        if name == "img2img":
            return await handle_img2img(client=self.client, workflow=wf, args=args)
        # E-2 ops bundled as stubs. Reject cleanly; the workflow
        # was loaded but the operator hasn't supplied a real one
        # (the bundled JSON is a placeholder).
        if wf.meta.get("status") == "stub":
            return await handle_e2_stub(op_name=name, args=args)
        # Real operator-supplied workflow for this op — use the
        # dynamic shell so they don't need a per-op handler patch.
        return await handle_dynamic_workflow(client=self.client, workflow=wf, args=args)


def _schema_for_workflow(wf: Workflow) -> dict[str, Any]:
    """Derive a tools/list inputSchema from a workflow's
    placeholders. Each placeholder becomes a top-level string
    property; the operator's _meta.schema field overrides if
    present.
    """
    meta_schema = wf.meta.get("schema")
    if isinstance(meta_schema, dict):
        return meta_schema
    props: dict[str, Any] = {}
    for ph in sorted(wf.placeholders):
        props[ph] = {"type": "string", "description": f"Substituted for <<{ph}>> in the workflow JSON."}
    return {
        "type": "object",
        "properties": props,
        "required": sorted(wf.placeholders),
        "additionalProperties": True,
    }


def _rpc_error(rpc_id: Any, code: int, message: str) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": rpc_id, "error": {"code": code, "message": message}}


# ---------------------------------------------------------------------------
# HTTP transport
# ---------------------------------------------------------------------------


async def serve_http(server: Server, cfg: BridgeConfig) -> None:
    """Tiny stdlib-based HTTP server. Bound to one path (``/``)
    that accepts POST JSON-RPC requests. Authentication is checked
    before dispatch.

    Not built on FastAPI / starlette to keep the bridge's runtime
    dependency footprint minimal — the JSON-RPC shape is small
    enough that hand-rolling beats pulling a web framework.
    """
    from aiohttp import web  # local import — aiohttp is optional

    async def handle_post(request: web.Request) -> web.Response:
        if not _check_auth(request.headers, cfg):
            return web.Response(status=401, text="unauthorized")
        try:
            body = await request.json()
        except Exception:
            return web.json_response(
                _rpc_error(None, -32700, "Parse error"),
                status=400,
            )
        # Batch + single both accepted per JSON-RPC 2.0.
        if isinstance(body, list):
            responses = []
            for req in body:
                resp = await server.handle(req)
                if resp is not None:
                    responses.append(resp)
            return web.json_response(responses)
        resp = await server.handle(body)
        if resp is None:
            return web.Response(status=204)
        return web.json_response(resp)

    async def handle_get(_: web.Request) -> web.Response:
        # AA's health checker calls tools/list via POST; the GET
        # here is just for "curl https://bridge/" sanity.
        return web.json_response(
            {
                "name": SERVER_NAME,
                "version": SERVER_VERSION,
                "protocol_version": PROTOCOL_VERSION,
                "transport": "http",
                "workflows_loaded": len(server.workflows),
            }
        )

    app = web.Application()
    app.router.add_post("/", handle_post)
    app.router.add_post("/mcp", handle_post)
    app.router.add_get("/", handle_get)

    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, cfg.http_host, cfg.http_port)
    await site.start()

    logger.info("comfyui-mcp-bridge listening on http://%s:%d", cfg.http_host, cfg.http_port)

    # Run forever; clean shutdown on SIGTERM/SIGINT.
    stop = asyncio.Event()

    def _signal() -> None:
        logger.info("shutting down (signal received)")
        stop.set()

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGTERM, signal.SIGINT):
        try:
            loop.add_signal_handler(sig, _signal)
        except NotImplementedError:
            # Windows — fall back to KeyboardInterrupt
            pass

    try:
        await stop.wait()
    finally:
        await runner.cleanup()
        await server.aclose()


def _check_auth(headers: Any, cfg: BridgeConfig) -> bool:
    if cfg.auth_kind == "none":
        return True
    if cfg.auth_kind == "bearer":
        got = headers.get("authorization", "")
        return got == f"Bearer {cfg.auth_token}"
    if cfg.auth_kind == "header":
        got = headers.get(cfg.auth_header_name, "")
        return got == cfg.auth_token
    return False


# ---------------------------------------------------------------------------
# stdio transport
# ---------------------------------------------------------------------------


async def serve_stdio(server: Server) -> None:
    """Read JSON-RPC requests from stdin (one per line), write
    responses to stdout. Simple length-prefix-free framing — line
    JSON is what Claude Desktop + the official MCP stdio
    transport emit when configured for the bridge.
    """
    loop = asyncio.get_event_loop()
    while True:
        line = await loop.run_in_executor(None, sys.stdin.readline)
        if not line:
            break  # EOF
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError:
            err = _rpc_error(None, -32700, "Parse error")
            print(json.dumps(err), flush=True)
            continue
        resp = await server.handle(req)
        if resp is not None:
            print(json.dumps(resp), flush=True)
    await server.aclose()


# ---------------------------------------------------------------------------
# CLI entry points
# ---------------------------------------------------------------------------


def main_http() -> None:
    """Entry point registered as the ``comfyui-mcp-bridge`` script."""
    _configure_logging()
    cfg = BridgeConfig()
    cfg.validate_runtime()
    server = Server(cfg)
    try:
        asyncio.run(serve_http(server, cfg))
    except KeyboardInterrupt:
        pass


def main_stdio() -> None:
    """Entry point registered as the ``comfyui-mcp-bridge-stdio`` script."""
    # stdio MUST NOT log to stdout — that's the JSON-RPC stream.
    _configure_logging(stream=sys.stderr)
    cfg = BridgeConfig()
    cfg.validate_runtime()
    server = Server(cfg)
    try:
        asyncio.run(serve_stdio(server))
    except KeyboardInterrupt:
        pass


def _configure_logging(*, stream: Any = sys.stdout) -> None:
    cfg = BridgeConfig()  # second read is cheap + safe
    logging.basicConfig(
        level=getattr(logging, cfg.log_level),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
        stream=stream,
    )


# CLI shim so `python -m comfyui_mcp_bridge` also works.
if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=SERVER_NAME)
    parser.add_argument(
        "--transport",
        choices=["http", "stdio"],
        default="http",
        help="Transport to serve.",
    )
    ns = parser.parse_args()
    if ns.transport == "stdio":
        main_stdio()
    else:
        main_http()
