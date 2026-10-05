# comfyui-mcp-bridge

Model Context Protocol (MCP) server that exposes [ComfyUI](https://github.com/comfyanonymous/ComfyUI) workflows as MCP tools. Originally built for [artist-alley](https://github.com/Artist-Alley-Org/artist-alley)'s Phase 1.14.E-1 image-edit subsystem, but vendor-neutral — any MCP client (Claude Desktop, custom agents, the AA dispatcher) can call it.

**Status:** beta. The img2img tool path is fully wired against a default SDXL workflow. The other four typed tools (inpaint, outpaint, variations, remove_bg) ship as placeholders — drop your own workflow JSON in to enable them.

## What it does

ComfyUI itself doesn't speak MCP — it has a JSON workflow API + a WebSocket progress stream. This bridge:

- Loads a directory of `*.json` ComfyUI workflow exports.
- Advertises each as an MCP tool via the standard `tools/list` method.
- Translates `tools/call` invocations into ComfyUI prompt queue + WS-wait + image fetch round-trips.
- Returns the result as an MCP-spec content array (one `image` part + one `text` part with metadata JSON).

It exposes **two surfaces**:

1. **Typed image-edit ops** — five named tools with stable input schemas (`img2img`, `inpaint`, `outpaint`, `variations`, `remove_bg`). Each backed by a workflow JSON of the same name. Operators override the bundled defaults by dropping their own JSON into the configured extra-workflows directory.
2. **Arbitrary `workflow:<name>` tools** — any other `*.json` file in the workflows directory auto-becomes a tool. The bridge derives the input schema from `<<TOKEN>>` placeholder slots in the workflow JSON. Useful for one-off pipelines (a specific upscaler chain, a stylized output pass) that don't fit the typed ops.

## Quickstart

```bash
# 1. Install (from this directory)
pip install -e .

# 2. Configure (or set CMB_* env vars in your shell)
export CMB_COMFY_URL=http://127.0.0.1:8188
export CMB_AUTH_KIND=bearer
export CMB_AUTH_TOKEN=$(openssl rand -hex 32)

# 3. Start the HTTP transport (canonical AA integration)
comfyui-mcp-bridge

# Or — start stdio mode for Claude Desktop:
comfyui-mcp-bridge-stdio
```

Then in artist-alley:

1. Navigate to **Admin → System → AI → MCP clients**.
2. Register a new server: `name=comfyui-lan`, `url=http://<host>:9201/mcp`, `auth_kind=bearer`, paste the same `CMB_AUTH_TOKEN`, `privacy_class=local`.
3. Add a tool grant for `img2img` with `additional_capability=mcp.client.images.write` + a cost estimate that makes sense for your hardware.
4. Set `Admin → System → AI → image-edit server` to `comfyui-lan`.
5. Open any image asset; click **Generate variation (AI)**; enter a prompt.

Full walkthrough: see `docs/operator/comfyui-mcp-bridge-setup.md` in the artist-alley repo.

## Adding custom workflows

Drop a ComfyUI workflow export at `${CMB_EXTRA_WORKFLOWS_DIR}/your_workflow.json`. Use `<<UPPER_SNAKE_CASE>>` tokens as placeholders for caller-supplied values. Examples:

```json
{
  "_meta": {
    "description": "Style transfer via the StyleAlign node",
    "version": 1
  },
  "3": {
    "inputs": {"seed": "<<SEED>>", "steps": "<<STEPS>>", "denoise": "<<DENOISE>>", ...},
    "class_type": "KSampler"
  },
  "6": {
    "inputs": {"text": "<<PROMPT>>", "clip": ["4", 1]},
    "class_type": "CLIPTextEncode"
  }
}
```

Restart the bridge; the workflow becomes callable as `workflow:your_workflow` over MCP. AA-side operators then add a tool grant for that name to authorise invocation.

**Special placeholder**: if your workflow uses `<<SOURCE_IMAGE_NAME>>` in a `LoadImage` node, the bridge auto-uploads the caller's `source_image_base64` argument to ComfyUI's `/upload/image` endpoint and substitutes the filed name. `<<SEED>>` of 0 gets resolved to a fresh random seed and echoed back in the metadata response.

### Pre-baked examples

`examples/` ships operator-ready workflow JSONs that override the bundled defaults. Current set:

- **`flux_kontext_img2img.json`** — drop at `$CMB_EXTRA_WORKFLOWS_DIR/img2img.json` to replace the bundled SDXL workflow with a Flux Kontext Dev workflow. Edit-focused base model, much better at instruction-following ("change the boats to glass yachts, keep the wave"). Tested 14 s wall-clock on RTX 5090. See [`examples/README.md`](examples/README.md) for the install + model-prereq walkthrough.

## Configuration reference

All knobs are environment variables prefixed `CMB_`. See `src/comfyui_mcp_bridge/config.py` for the typed definitions + per-field docstrings.

| Variable | Default | Purpose |
|---|---|---|
| `CMB_COMFY_URL` | `http://127.0.0.1:8188` | Where ComfyUI listens. |
| `CMB_COMFY_TIMEOUT_S` | `300` | Hard cap on one tool invocation. |
| `CMB_HTTP_HOST` | `0.0.0.0` | HTTP bind interface. |
| `CMB_HTTP_PORT` | `9201` | HTTP bind port. |
| `CMB_AUTH_KIND` | `none` | One of `none`, `bearer`, `header`. |
| `CMB_AUTH_TOKEN` | (empty) | Required when auth_kind ≠ none. |
| `CMB_AUTH_HEADER_NAME` | `X-API-Key` | Used when auth_kind = header. |
| `CMB_WORKFLOWS_DIR` | bundled | Where to scan for workflow JSON. |
| `CMB_EXTRA_WORKFLOWS_DIR` | (unset) | Second dir, scanned after; overrides bundled on name conflict. |
| `CMB_LOG_LEVEL` | `INFO` | DEBUG / INFO / WARNING / ERROR. |

## Development

```bash
pip install -e ".[dev]"
pytest             # 7 unit tests on the workflow loader
ruff check src tests
mypy src
```

## Roadmap

- **0.2** — pre-baked workflows for inpaint / outpaint / variations / remove_bg using SDXL nodes that ship with stock ComfyUI installs.
- **0.3** — multi-output workflows: `variations` op fans out N seeds into one ComfyUI batch and returns all images in one content array.
- **0.4** — mTLS auth + Vault secret reference for the AA `auth_secret_ref` field.
- **1.0** — async progress events streamed back through the MCP `notifications` channel so AA can render a real progress bar instead of polling /jobs/{id}.

## License

AGPL-3.0-or-later. See `LICENSE` (top of the artist-alley repo).
