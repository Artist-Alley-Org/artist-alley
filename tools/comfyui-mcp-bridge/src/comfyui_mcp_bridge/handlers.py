"""Per-tool handlers.

Each handler:
  1. Validates the MCP tool args against its declared input schema
     (the schema-side of MCP's tools/list contract).
  2. Uploads source images to ComfyUI via /upload/image.
  3. Renders the workflow template with operator + caller-supplied
     values.
  4. Queues the workflow + waits for completion.
  5. Fetches the output image bytes + composes the MCP content
     array response (image part + text part with metadata JSON).

The img2img handler is the only one that's fully wired for E-1.
The other typed ops + the dynamic ``workflow:<name>`` ops use the
same execution shell but with their own workflow JSON; for E-1
they advertise via tools/list but error gracefully ("workflow not
configured") when invoked unless the operator supplied a real JSON.
"""

from __future__ import annotations

import base64
import json
import logging
import random
import uuid
from typing import Any

from .comfy_client import ComfyClient, ComfyError
from .workflows_loader import Workflow

logger = logging.getLogger(__name__)


class ToolError(Exception):
    """Raised on any tool-side failure. The server layer maps to
    the MCP JSON-RPC error shape.
    """

    def __init__(self, message: str, *, transient: bool = False) -> None:
        super().__init__(message)
        self.transient = transient


# ---------------------------------------------------------------------------
# Typed-op input schemas — published in tools/list
# ---------------------------------------------------------------------------

IMG2IMG_SCHEMA: dict[str, Any] = {
    "type": "object",
    "required": ["prompt", "source_image_base64"],
    "additionalProperties": False,
    "properties": {
        "prompt": {
            "type": "string",
            "minLength": 1,
            "maxLength": 2000,
            "description": "Describes the variation to generate.",
        },
        "source_image_base64": {
            "type": "string",
            "description": "Base64-encoded source image bytes.",
        },
        "source_content_type": {
            "type": "string",
            "description": "MIME type of the source bytes (image/png, image/jpeg, image/webp).",
            "default": "image/png",
        },
        "denoise_strength": {
            "type": "number",
            "minimum": 0,
            "maximum": 1,
            "default": 0.7,
            "description": "0 = no change; 1 = ignore source entirely.",
        },
        "steps": {
            "type": "integer",
            "minimum": 1,
            "maximum": 100,
            "default": 20,
        },
        "seed": {
            "type": "integer",
            "description": "0 = random.",
            "default": 0,
        },
    },
}

INPAINT_SCHEMA: dict[str, Any] = {
    "type": "object",
    "required": ["prompt", "source_image_base64", "mask_image_base64"],
    "additionalProperties": False,
    "properties": {
        "prompt": {"type": "string", "minLength": 1, "maxLength": 2000},
        "source_image_base64": {"type": "string"},
        "mask_image_base64": {
            "type": "string",
            "description": "Base64-encoded single-channel PNG mask. Opaque = regenerate, transparent = preserve.",
        },
        "source_content_type": {"type": "string", "default": "image/png"},
        "denoise_strength": {"type": "number", "minimum": 0, "maximum": 1, "default": 0.85},
        "steps": {"type": "integer", "minimum": 1, "maximum": 100, "default": 25},
        "seed": {"type": "integer", "default": 0},
    },
}

OUTPAINT_SCHEMA: dict[str, Any] = {
    "type": "object",
    "required": ["prompt", "source_image_base64"],
    "additionalProperties": False,
    "properties": {
        "prompt": {"type": "string", "minLength": 1, "maxLength": 2000},
        "source_image_base64": {"type": "string"},
        "source_content_type": {"type": "string", "default": "image/png"},
        "extend_top": {"type": "integer", "minimum": 0, "default": 0},
        "extend_right": {"type": "integer", "minimum": 0, "default": 0},
        "extend_bottom": {"type": "integer", "minimum": 0, "default": 0},
        "extend_left": {"type": "integer", "minimum": 0, "default": 0},
        "steps": {"type": "integer", "minimum": 1, "maximum": 100, "default": 25},
        "seed": {"type": "integer", "default": 0},
    },
}

VARIATIONS_SCHEMA: dict[str, Any] = {
    "type": "object",
    "required": ["source_image_base64"],
    "additionalProperties": False,
    "properties": {
        "source_image_base64": {"type": "string"},
        "source_content_type": {"type": "string", "default": "image/png"},
        "count": {"type": "integer", "minimum": 1, "maximum": 8, "default": 4},
        "seed": {"type": "integer", "default": 0},
    },
}

REMOVE_BG_SCHEMA: dict[str, Any] = {
    "type": "object",
    "required": ["source_image_base64"],
    "additionalProperties": False,
    "properties": {
        "source_image_base64": {"type": "string"},
        "source_content_type": {"type": "string", "default": "image/png"},
    },
}


# Maps the typed tool name → (input schema, workflow name in the
# loader registry, value-builder fn). The value-builder turns
# validated args into the placeholder map the workflow renderer
# expects.
TYPED_OPS: dict[str, dict[str, Any]] = {
    "img2img": {
        "schema": IMG2IMG_SCHEMA,
        "workflow": "img2img",
        "description": "Generate a variation of the source image guided by a prompt.",
    },
    "inpaint": {
        "schema": INPAINT_SCHEMA,
        "workflow": "inpaint",
        "description": "Regenerate the masked region of the source image.",
    },
    "outpaint": {
        "schema": OUTPAINT_SCHEMA,
        "workflow": "outpaint",
        "description": "Extend the source image's canvas in the named directions.",
    },
    "variations": {
        "schema": VARIATIONS_SCHEMA,
        "workflow": "variations",
        "description": "Produce N variations of the source image without prompt steering.",
    },
    "remove_bg": {
        "schema": REMOVE_BG_SCHEMA,
        "workflow": "remove_bg",
        "description": "Remove the background of the source image.",
    },
}


# ---------------------------------------------------------------------------
# Shared execution shell
# ---------------------------------------------------------------------------


async def _execute(
    *,
    client: ComfyClient,
    workflow: Workflow,
    placeholders: dict[str, Any],
    expect_count: int = 1,
) -> tuple[list[bytes], list[str]]:
    """Queue the rendered workflow + return (image_bytes_list,
    image_mime_list). Raises ToolError on any ComfyUI-side
    failure; the server layer maps to MCP error responses.
    """
    try:
        rendered = workflow.render(placeholders)
    except KeyError as e:
        raise ToolError(f"workflow render: {e}", transient=False) from e

    try:
        prompt_id = await client.queue_prompt(rendered)
        await client.wait_for_completion(prompt_id)
        history = await client.get_history(prompt_id)
    except ComfyError as e:
        raise ToolError(str(e), transient=e.transient) from e

    outputs = history.get("outputs", {})
    images: list[tuple[bytes, str]] = []
    for node_outputs in outputs.values():
        for img in node_outputs.get("images", []):
            filename = img.get("filename")
            subfolder = img.get("subfolder", "")
            folder_type = img.get("type", "output")
            if not filename:
                continue
            try:
                data = await client.fetch_view(filename, subfolder, folder_type)
            except ComfyError as e:
                raise ToolError(f"fetch output {filename}: {e}", transient=e.transient) from e
            mime = _guess_mime_from_filename(filename)
            images.append((data, mime))
            if len(images) >= expect_count:
                break
        if len(images) >= expect_count:
            break

    if not images:
        raise ToolError(
            "ComfyUI workflow finished without emitting any images — "
            "check that the workflow has a SaveImage node and the output "
            "node was reached",
            transient=False,
        )

    return [d for d, _ in images], [m for _, m in images]


def _guess_mime_from_filename(name: str) -> str:
    lower = name.lower()
    if lower.endswith(".jpg") or lower.endswith(".jpeg"):
        return "image/jpeg"
    if lower.endswith(".webp"):
        return "image/webp"
    return "image/png"


def _resolve_seed(requested: int) -> int:
    """Empty / zero seed → random. Returned to caller via metadata
    so they can reproduce.
    """
    if requested == 0:
        # 32-bit unsigned to match ComfyUI's seed range
        # (ComfyUI accepts 64-bit but most samplers wrap).
        return random.randint(1, 2**32 - 1)
    return requested


def _build_content_response(image_bytes: bytes, mime: str, metadata: dict[str, Any]) -> dict[str, Any]:
    """Compose the MCP-spec tools/call content array our AA-side
    provider expects: one "image" part + one "text" part
    carrying JSON metadata.
    """
    return {
        "content": [
            {
                "type": "image",
                "data": base64.b64encode(image_bytes).decode("ascii"),
                "mimeType": mime,
            },
            {
                "type": "text",
                "text": json.dumps(metadata),
            },
        ],
    }


# ---------------------------------------------------------------------------
# img2img — the only fully-wired op for E-1
# ---------------------------------------------------------------------------


async def handle_img2img(
    *,
    client: ComfyClient,
    workflow: Workflow,
    args: dict[str, Any],
) -> dict[str, Any]:
    """img2img tool handler. AA-side calls this via the bridge's
    tools/call entry point.
    """
    prompt: str = args["prompt"]
    source_b64: str = args["source_image_base64"]
    source_ct: str = args.get("source_content_type") or "image/png"
    denoise: float = float(args.get("denoise_strength", 0.7))
    steps: int = int(args.get("steps", 20))
    seed: int = _resolve_seed(int(args.get("seed", 0)))

    try:
        source_bytes = base64.b64decode(source_b64, validate=True)
    except (ValueError, base64.binascii.Error) as e:
        raise ToolError(f"source_image_base64 invalid: {e}", transient=False) from e

    # Upload source under a unique name so concurrent calls don't
    # collide on the LoadImage node's input slot.
    upload_name = f"aaedit_{uuid.uuid4().hex}_{_ext_for_mime(source_ct)}"
    try:
        filed_name = await client.upload_image(upload_name, source_bytes, source_ct)
    except ComfyError as e:
        raise ToolError(f"upload source: {e}", transient=e.transient) from e

    placeholders = {
        "PROMPT": prompt,
        "SOURCE_IMAGE_NAME": filed_name,
        "DENOISE": denoise,
        "STEPS": steps,
        "SEED": seed,
    }

    images, mimes = await _execute(
        client=client,
        workflow=workflow,
        placeholders=placeholders,
        expect_count=1,
    )

    metadata = {
        "prompt": prompt,
        "seed_used": seed,
        "steps": steps,
        "denoise_strength": denoise,
        "model": workflow.meta.get("model_assumption", "unknown"),
        "workflow": workflow.name,
        "workflow_version": workflow.meta.get("version", 1),
    }
    return _build_content_response(images[0], mimes[0], metadata)


def _ext_for_mime(mime: str) -> str:
    return {
        "image/png": "source.png",
        "image/jpeg": "source.jpg",
        "image/webp": "source.webp",
    }.get(mime, "source.png")


# ---------------------------------------------------------------------------
# E-2 stubs — typed ops + dynamic workflow:<name> handler
# ---------------------------------------------------------------------------


async def handle_e2_stub(
    *,
    op_name: str,
    args: dict[str, Any],
) -> dict[str, Any]:
    """Used by inpaint/outpaint/variations/remove_bg in E-1.
    Returns a structured error so the AA side surfaces a clean
    "not yet implemented" message rather than a generic
    invocation failure.
    """
    _ = args
    raise ToolError(
        f"tool {op_name!r} is bundled at the bridge as a placeholder; "
        f"drop a real workflow JSON at CMB_EXTRA_WORKFLOWS_DIR/{op_name}.json + "
        "restart the bridge to enable",
        transient=False,
    )


async def handle_dynamic_workflow(
    *,
    client: ComfyClient,
    workflow: Workflow,
    args: dict[str, Any],
) -> dict[str, Any]:
    """workflow:<name> handler — generic execution of any
    operator-supplied workflow. The args map keys match the
    workflow's placeholders (e.g. <<PROMPT>> ↔ args["PROMPT"]).

    Source bytes (if any) get pre-uploaded under a stable
    placeholder name SOURCE_IMAGE_NAME so workflows can opt in
    to the same image-upload path img2img uses.
    """
    placeholders: dict[str, Any] = {}
    for key, value in args.items():
        placeholders[key.upper()] = value

    # If the workflow declares a SOURCE_IMAGE placeholder and the
    # caller passed source_image_base64, upload + substitute the
    # filed name.
    if "SOURCE_IMAGE_NAME" in workflow.placeholders and "source_image_base64" in args:
        try:
            source_bytes = base64.b64decode(args["source_image_base64"], validate=True)
        except (ValueError, base64.binascii.Error) as e:
            raise ToolError(f"source_image_base64 invalid: {e}", transient=False) from e
        upload_name = f"aaedit_{uuid.uuid4().hex}.png"
        try:
            filed_name = await client.upload_image(upload_name, source_bytes, "image/png")
        except ComfyError as e:
            raise ToolError(f"upload source: {e}", transient=e.transient) from e
        placeholders["SOURCE_IMAGE_NAME"] = filed_name

    # Resolve seed if requested.
    if "SEED" in workflow.placeholders:
        seed_in = int(placeholders.get("SEED", 0) or 0)
        placeholders["SEED"] = _resolve_seed(seed_in)

    images, mimes = await _execute(
        client=client,
        workflow=workflow,
        placeholders=placeholders,
        expect_count=1,
    )

    metadata = {
        "workflow": workflow.name,
        "workflow_version": workflow.meta.get("version", 1),
        "seed_used": placeholders.get("SEED"),
    }
    return _build_content_response(images[0], mimes[0], metadata)
