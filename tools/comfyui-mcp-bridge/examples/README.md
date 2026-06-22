# Example workflows

Operator-ready ComfyUI workflow JSONs that override the bundled defaults. Copy one to `$CMB_EXTRA_WORKFLOWS_DIR/<filename>` (matching the typed-op name in `src/comfyui_mcp_bridge/workflows/`) and the bridge picks it up on next start.

## What's here

| File | Replaces | Models needed | Notes |
|---|---|---|---|
| `flux_kontext_img2img.json` | `img2img` | Flux Kontext Dev FP8 + Flux text encoders + Flux VAE | Edit-focused base model; CFG=1 / denoise=1 are Kontext-correct. Tested on RTX 5090: ~14 s wall-clock on a 1024-edge source. |

## How to use

```bash
# 1. Pick the operator-workflows directory.
export CMB_EXTRA_WORKFLOWS_DIR=$HOME/aa-bridge-workflows
mkdir -p "$CMB_EXTRA_WORKFLOWS_DIR"

# 2. Copy the example you want, renaming to the typed-op slot it overrides.
cp tools/comfyui-mcp-bridge/examples/flux_kontext_img2img.json \
   "$CMB_EXTRA_WORKFLOWS_DIR/img2img.json"

# 3. Confirm your ComfyUI install has the models the workflow's _meta block
#    lists under `required_models`. Download links are in `download_urls`.

# 4. Restart the bridge.
docker restart comfyui-mcp-bridge  # OR Ctrl+C and re-run if bare-metal
```

The bridge logs the loaded path + extracted placeholders on startup — confirm your override shows up:

```
INFO workflow.loaded name=img2img path=/.../$CMB_EXTRA_WORKFLOWS_DIR/img2img.json placeholders=['PROMPT', 'SEED', 'SOURCE_IMAGE_NAME', 'STEPS']
```

## Adding your own

`<<UPPER_SNAKE_CASE>>` placeholder tokens get substituted at queue time. The substitution is type-preserving: a token in a number-valued JSON slot (`"seed": "<<SEED>>"`) gets a number written back; a token in a string-valued slot (`"image": "<<SOURCE_IMAGE_NAME>>"`) gets a string. Mixed-content strings (`"label": "seed=<<SEED>>"`) interpolate the value's string form.

Special placeholders:

- `<<SOURCE_IMAGE_NAME>>` — triggers source-bytes auto-upload to ComfyUI's `/upload/image`; substituted with the filed name. Use this in any `LoadImage` node that should consume the caller's source.
- `<<SEED>>` — when caller passes 0, gets resolved to a fresh random `int32` + echoed back in the response's metadata.

Add the `_meta` block at the top with `description`, `version`, and (for examples) `required_models` + `download_urls`. Per-workflow tuning notes belong there too — anyone re-using your workflow benefits from the rationale.
