# ComfyUI MCP bridge — operator setup

Phase 1.14.E-1 ships AA's first internal MCP caller: image-to-image generation that dispatches to ComfyUI via a small Python bridge. This doc walks an operator from zero to a working **Generate variation (AI)** button on image assets.

The bridge lives at [tools/comfyui-mcp-bridge/](../../tools/comfyui-mcp-bridge/) in this repo. It's a sibling project, not part of the AA Go binary — operators run it next to ComfyUI on their own infrastructure.

## What you need before starting

- A running ComfyUI instance you can reach over HTTP (locally or on the LAN). The bridge defaults assume `http://127.0.0.1:8188`.
- An SDXL base checkpoint (`sd_xl_base_1.0.safetensors`) in ComfyUI's `models/checkpoints/`. The bundled `img2img.json` workflow uses this — other base models (SD 1.5, Flux, Pony, SD3) work, but you'll need to drop in your own workflow JSON (see "Custom workflows" below).
- Python 3.11+ on the host that'll run the bridge. Docker is the easier path; bare-metal install also works.
- An artist-alley admin account.

## What gets wired

The end-to-end flow once it's set up:

1. User clicks **Generate variation (AI)** on an image asset, enters a prompt.
2. AA's HTTP handler validates the request (auth, capability, source-is-image, server configured) + enqueues an `aiedit.img2img` job.
3. The job worker fetches the source bytes, calls `mcp.dispatch.Invoke` against the operator-registered MCP server.
4. The bridge translates the MCP `tools/call` into a ComfyUI prompt queue, waits on the progress WebSocket, fetches the result via `/view`, returns it base64-encoded in an MCP content array.
5. AA decodes the bytes, mints a new asset via the canonical upload pipeline, writes a `creative_lineage` row linking the derivative to the source.
6. Frontend polls the job, navigates the viewer to the new asset.

Privacy / budget / audit gates from Phase 1.53.A all apply automatically — sensitive assets won't route to a cloud-classified bridge, calls hit the per-server cost cap, every invocation lands a row in `ai_provider_call`.

## Install the bridge

### Option A — Docker (recommended)

```bash
cd tools/comfyui-mcp-bridge
docker build -t comfyui-mcp-bridge:latest .

# Generate a shared secret you'll paste into AA's admin UI later.
export CMB_AUTH_TOKEN=$(openssl rand -hex 32)

docker run -d --name comfyui-mcp-bridge \
  -p 9201:9201 \
  -e CMB_COMFY_URL=http://host.docker.internal:8188 \
  -e CMB_AUTH_KIND=bearer \
  -e CMB_AUTH_TOKEN=$CMB_AUTH_TOKEN \
  --add-host=host.docker.internal:host-gateway \
  comfyui-mcp-bridge:latest

# Sanity check — should print the bridge's banner.
curl http://localhost:9201/
```

For a longer-running setup with custom workflow JSONs bind-mounted from the host, see [docker-compose.example.yml](../../tools/comfyui-mcp-bridge/docker-compose.example.yml).

### Option B — bare-metal Python

```bash
cd tools/comfyui-mcp-bridge
python -m venv .venv && source .venv/bin/activate
pip install -e .

export CMB_COMFY_URL=http://127.0.0.1:8188
export CMB_AUTH_KIND=bearer
export CMB_AUTH_TOKEN=$(openssl rand -hex 32)

comfyui-mcp-bridge
```

Either way, you should see the banner log line:

```
INFO comfyui_mcp_bridge.server comfyui-mcp-bridge listening on http://0.0.0.0:9201
```

## Register the bridge in AA

1. Sign in as an admin. Navigate to **Admin → System → AI → MCP clients**.
2. Click **Register a new MCP server**, fill in:
   - **Name:** `comfyui-lan` (or whatever — must be unique per AA install; you'll reference this name in step 4)
   - **URL:** `http://<bridge-host>:9201/` — if AA + bridge share a host, `http://localhost:9201/` works; for AA-in-docker pointing at bridge-on-host, use `http://host.docker.internal:9201/`
   - **Transport:** `http`
   - **Privacy class:** `local` (assumes the bridge is on your LAN; pick `cloud` only if the bridge is hosted somewhere the public internet can reach — this gates which assets AA will route through)
   - **Auth kind:** `bearer`
   - **Auth secret:** paste the `CMB_AUTH_TOKEN` you generated above
   - **Enabled:** ✓
   - Leave rate limits + health interval at defaults.
3. Click the new row to open the detail page. Add a tool grant:
   - **Tool name:** `img2img`
   - **Additional capability:** `mcp.client.images.write`
   - **Cost estimate (µ$):** `10000` (= $0.01 — fine placeholder for local GPU runs; raise for hosted bridges with metered billing)
   - **Enabled:** ✓
4. Navigate to **Admin → System → AI → image-edit server**. Set the value to the **name** you picked in step 2 (e.g. `comfyui-lan`). Save.
5. Confirm the per-server health badge on the MCP clients list page flips to **healthy** within the next 60 seconds. If it stays **unreachable**, see "Troubleshooting" below.

## Try it

1. Navigate to any image asset (kind = photo).
2. In the top-right of the viewer canvas, click **Generate variation (AI)**.
3. Enter a prompt — e.g. "watercolour sketch, soft lighting".
4. Click **Generate**.

The popover shows "Waiting for ComfyUI to finish" while the job runs (30 s – 5 min depending on your GPU + workflow steps). On completion, you're navigated to the new derivative asset; opening its detail panel later (E-2 surface) will show the lineage row pointing back at the source.

## Custom workflows

The bundled `img2img.json` assumes SDXL. If you run a different base model — SD 1.5, Flux, Pony, SD3, custom finetunes — drop your own workflow JSON in:

```bash
# Docker — bind-mount this directory at run time:
mkdir -p ./operator-workflows
# Edit your workflow in ComfyUI, export the API-format JSON.
cp ~/Downloads/my_sdxl_workflow.json ./operator-workflows/img2img.json
docker restart comfyui-mcp-bridge
```

Use `<<UPPER_SNAKE_CASE>>` placeholders in the JSON for the values the bridge substitutes at queue time:

- `<<PROMPT>>` — caller's prompt text
- `<<SOURCE_IMAGE_NAME>>` — name ComfyUI assigned to the uploaded source after `/upload/image` (use in `LoadImage.inputs.image`)
- `<<DENOISE>>` — caller's denoise strength, defaulting to 0.7
- `<<STEPS>>` — caller's step count, defaulting to 20
- `<<SEED>>` — caller's seed, or a fresh random integer when caller sent 0

The bridge auto-discovers any other `*.json` file you drop and exposes it as a `workflow:<filename>` tool, but **AA-side only `img2img` is currently wired through the typed surface**. To call a custom workflow:<name> tool from AA you'd add a `workflow:my_thing` row to the per-server tool grants and write a future feature against `mcpdispatch.Dispatcher.Invoke`. Phase 1.14.E-2 will expand the AA-side surface.

### Flux Kontext (recommended for img2img)

If you have a Flux-capable GPU, [Flux Kontext Dev](https://huggingface.co/black-forest-labs/FLUX.1-Kontext-dev) is the right base model for image editing — purpose-built for instruction-following edits while preserving composition, much better at "change the boats to glass yachts, keep the wave intact" than vanilla SDXL img2img.

A tested, ready-to-drop-in Kontext workflow lives at [`tools/comfyui-mcp-bridge/examples/flux_kontext_img2img.json`](../../tools/comfyui-mcp-bridge/examples/flux_kontext_img2img.json):

```bash
# Pick where operator workflows live (any path the bridge has read access to)
export CMB_EXTRA_WORKFLOWS_DIR=$HOME/aa-bridge-workflows
mkdir -p "$CMB_EXTRA_WORKFLOWS_DIR"

# Drop in the Kontext example, renaming to the typed-op slot it overrides
cp tools/comfyui-mcp-bridge/examples/flux_kontext_img2img.json \
   "$CMB_EXTRA_WORKFLOWS_DIR/img2img.json"

# Confirm the bridge picks up the override on restart — the placeholders
# line in the log should change from DENOISE+PROMPT+SEED+SOURCE+STEPS to
# just PROMPT+SEED+SOURCE+STEPS (Kontext doesn't expose denoise).
docker restart comfyui-mcp-bridge
```

Required models (the example's `_meta` block lists the HuggingFace URLs):

- `models/diffusion_models/flux1-dev-kontext_fp8_scaled.safetensors` (~11 GB; FP8 scaled fits in 16 GB VRAM)
- `models/vae/ae.safetensors` (Flux VAE)
- `models/text_encoders/clip_l.safetensors`
- `models/text_encoders/t5xxl_fp8_e4m3fn_scaled.safetensors`

**Tested 2026-06-22** against ComfyUI 0.17.2 on an RTX 5090 (32 GB VRAM): cold-start (first call, model load included) was 43 s; subsequent calls landed in **14 s wall-clock** on a 1024-edge source. Output dimensions match Kontext's native 1248×832 / 1024×1024 / etc. depending on the source aspect ratio.

**Why CFG=1 / denoise=1 in the Kontext workflow** — Kontext blends the reference image via the `ReferenceLatent` conditioning node, NOT via partial-noise injection like SDXL img2img. Setting denoise<1 or CFG>1 produces degraded output. The example's `_meta.tuning_notes` documents this for future maintainers.

## Privacy + cost decision tree

Re-cap of [docs/operator/mcp-client-setup.md](mcp-client-setup.md) applied to ComfyUI specifically:

- **Bridge on your LAN / same host → `privacy_class=local`.** Sensitive assets (restricted / embargo) can route here.
- **Bridge on a public VPS (you control it) → `privacy_class=cloud`** unless you trust the operator (yourself) to never inspect / retain bytes. The AA dispatcher will block sensitive assets from routing to a cloud-classified server when the inference privacy lock is on (default).
- **Bridge in front of a hosted ComfyUI cloud (Replicate / Modal / Banana) → `privacy_class=cloud`, always.** Operator-side data retention is out of your control.

**Cost estimate** is operator-declared per tool grant. Local GPU on your box is effectively $0 — but leaving it at 0 disables the budget gate signal entirely. Recommended:

| Bridge runs on | Suggested `cost_estimate_micros` |
|---|---|
| Your local GPU | `10000` ($0.01 — placeholder so calls show up in the dashboard) |
| Hosted ComfyUI you pay per minute | `50000` ($0.05) per typical img2img call |
| Hosted ComfyUI with massive models (Flux Pro, SD3 Max) | `200000` ($0.20) |

## Capability strategy

The img2img tool grant should require `mcp.client.images.write` as the additional capability. That cap is seeded to the Admin role in migration 00013; if you want artists to invoke it, grant the cap to their role via **Admin → Roles & capabilities**.

For more restrictive setups (some artists allowed, some not) mint a per-team cap like `team.studioA.aiedit.images.write` and gate the tool grant on it.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Bridge banner doesn't print on startup | ComfyUI URL wrong / unreachable. Check `CMB_COMFY_URL`; the bridge does NOT pre-connect to ComfyUI on startup, but a bad URL surfaces on the first invocation. |
| AA health badge stuck **unknown** | Health checker goroutine not running. Restart `app`; confirm the registration is **enabled**. |
| AA health badge **unreachable** | Bridge URL wrong, port-mapping broken, or auth mismatch. Run `curl -H "Authorization: Bearer $CMB_AUTH_TOKEN" -X POST http://<bridge>:9201/ -d '{"jsonrpc":"2.0","id":1,"method":"ping"}'` from inside the AA app container to confirm. |
| AA health badge **degraded** | Bridge reachable but `tools/list` errors. Check bridge logs for workflow-load errors — usually a malformed operator-supplied workflow JSON. |
| Click Generate, get "no image-edit server configured" | `Admin → System → AI → image-edit server` not set. Re-read the registration name + paste it exactly into that field. |
| Click Generate, get "your account doesn't have the mcp.client.images.write capability" | Cap not granted to your role. Admin grants via **Admin → Roles & capabilities**. |
| Click Generate, job runs, then "ComfyUI workflow failed at CheckpointLoaderSimple" | Model file missing. Bundled workflow expects `sd_xl_base_1.0.safetensors`; if you have a different name, drop a custom `img2img.json`. |
| Job runs but no output image | Custom workflow doesn't have a `SaveImage` node, or its output node never executes. Open the workflow in ComfyUI's web UI + run it manually first to confirm it produces an image. |
| Output asset has wrong dimensions | The bundled workflow doesn't include a resize/scale node — ComfyUI generates whatever the latent VAEEncoded source dimensions imply. Edit `img2img.json` to add `ImageScale` or run a different base workflow. |

## What's NOT here (E-1 scope cut)

- No inpaint / outpaint / variations / remove_bg AA-side surface yet — the bridge advertises the tool names but AA only wires `img2img`. E-2 ships the mask UI + the typed handlers on the AA side.
- No per-tool tier gating — Community / Pro / Enterprise distinctions are E-3, gated on the licensing arc.
- No federation of MCP server registrations — each AA instance has its own bridge.
- No live progress streaming — the popover polls `/jobs/{id}` every 2 s. v1.0 of the bridge will push progress events via the MCP notifications channel.

See [ADR 0026](../../docs/adr/0026-creative-tools.md) for the full vision; [ADR 0051](../../docs/adr/0051-artist-alley-as-mcp-client.md) for the foundation this builds on.
