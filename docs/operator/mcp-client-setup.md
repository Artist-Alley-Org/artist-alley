# MCP client subsystem — operator setup

Phase 1.53.A turns artist-alley into a **client** of Model Context
Protocol servers. An MCP server is any process that exposes
JSON-RPC 2.0 endpoints implementing the MCP spec (`tools/list`,
`tools/call`, etc.). Operators register servers by URL, mark which
tools the local install is allowed to invoke, and the dispatcher
calls them inside the existing privacy + budget + audit guard chain.

This phase ships **plumbing only**. No feature currently calls
`mcp.invoke()` on its own — operators register servers and grant
tools so other phases (1.14.E "AI write-back", 1.55 "agentic
workflow" etc.) can route through them. The validation reference
described below is ComfyUI exposed as MCP via an external bridge.

## Fresh-install state

On a clean install the MCP client subsystem is **dormant by
default**:

- `mcp.client.enabled` is `false` in `system_config`. Even after
  you register a server, no call fires until you flip this on.
- No servers are seeded. The list at `/admin/ai/mcp-clients` is
  empty.
- The two capabilities (`mcp.client.use` for the umbrella + a
  per-tool `additional_capability` you choose) are seeded to the
  Admin role. Grant `mcp.client.use` more broadly to let
  non-admins invoke whitelisted tools.

## Why operator-explicit?

MCP servers can expose **any** tool the server author chose to
expose. The standard's tool list is whatever `tools/list` returns
at runtime. We deliberately do **not** auto-import that list —
each tool must be added to the whitelist before it can be invoked,
and registration includes a privacy class that gates which assets
may flow to it. The model is: the server author tells you what
exists; the operator tells artist-alley what's allowed.

## Registering a server (walkthrough)

1. Navigate **Admin → System → AI → MCP clients**, click
   "Register a new MCP server".
2. **Name** — operator-chosen handle. Used in routing decisions
   (`mcp.invoke("comfyui-prod", "img2img", …)`); must be unique
   per instance.
3. **URL** — full JSON-RPC endpoint (`http://comfy.lan:9201/mcp`
   for a local bridge; `https://mcp.example.com/v1` for a hosted
   one).
4. **Transport** — `http` for v1. `stdio` is reserved for a future
   in-process bridge.
5. **Privacy class** — see the decision tree below. This is the
   single most important field on registration.
6. **Auth** — `none` for unauthenticated local bridges; `bearer`
   if the server expects `Authorization: Bearer <token>`;
   `header` for a custom header (paste its name into "Header
   name"); `mtls` is reserved.
7. **Rate limits** — sensible defaults are `2/sec` and `60/min`.
   The provider enforces this via a token-bucket; the dispatcher
   blocks until a token is available.
8. **Health interval** — how often the per-server goroutine polls
   `tools/list` to refresh status. `60s` is fine for hosted
   servers; drop to `30s` for local bridges where you want quick
   feedback when restarting ComfyUI.
9. Leave **Enabled** unchecked on create — flip it once you've
   added the tool whitelist below.

After registering, click the row to open the detail page and add
tool grants.

## Privacy classification decision tree

Privacy class lives on the **server registration**, not per call.
It marks the destination, not the call. Pick once at registration
time; everything routed through that server inherits the class.

```
Q1. Does the server's URL terminate inside your trust boundary?
    (LAN, VPC, same host, ssh-tunneled localhost, ...)
    ├── YES → Q2.
    └── NO  → privacy_class = cloud.

Q2. Does the operator of that server retain or log request bytes?
    ├── NO  → privacy_class = local.
    └── YES → privacy_class = cloud, even if the box is in your
              datacentre. "Local" is about who can see the bytes,
              not where the box is.
```

Effect at call time:

- When the AI inference subsystem's
  `lock_sensitive_to_local` is on (the default), assets at
  sensitivity tier `restricted` or `embargo` can route only to
  servers where `privacy_class = local`. A `tools/call` against a
  `cloud` server with a sensitive asset returns
  `privacy_blocked` — audited but not executed.

## Per-tool capability strategies

Every tool on the whitelist requires `mcp.client.use` (the
umbrella). Additionally, each row can require an extra capability
beyond that. The intent is fine-grained authorisation for
high-stakes tools.

| Tool kind                       | Suggested extra cap                | Reasoning                                                                 |
|---------------------------------|------------------------------------|---------------------------------------------------------------------------|
| Read-only enrichment            | (none)                             | Tag suggestions, captioning — every authed user.                          |
| Image generation                | `mcp.client.images.write`          | Writes derived assets; restrict to artists/admins.                        |
| Image search / reverse-lookup   | `mcp.client.images.read`           | Reads image bytes; gate on the same cap as the rest of the read surface.  |
| Destructive ops (delete, purge) | `system.admin` (admin-only)        | Use the existing wildcard cap rather than minting a new one.              |
| Cost-heavy (LLM, video)         | A custom cap you mint per tool     | Mint `mcp.client.<tool>.use` if you want named ownership.                 |

`mcp.client.images.read` and `mcp.client.images.write` are seeded
at migration time so the common case (a bridge that does
img2img / txt2img + reverse image search) needs no manual
capability work.

## Cost estimation guidance

`cost_estimate_micros` on each tool grant feeds the budget gate.
The budget gate uses the same `ai_provider_call` accounting as
the rest of Phase 1.14.A — `cost_usd_micros` is summed per
billing period; once a hard cap is hit, further `mcp.invoke()`
calls return `budget_blocked` until the next period.

Pick a number per tool that reflects the **operator's bill**
when the tool runs, not the model's compute cost:

| Tool kind                 | Suggested `cost_estimate_micros`  | $ per call |
|---------------------------|-----------------------------------|------------|
| Local model on your box   | `0`                               | $0         |
| Hosted small / chat       | `1_000` – `5_000`                 | $0.001–$0.005 |
| Hosted image generation   | `10_000` – `50_000`               | $0.01–$0.05   |
| Hosted video / long LLM   | `100_000` – `1_000_000`           | $0.10–$1.00   |

The values are flat per-call — the budget gate doesn't know about
input tokens or pixel count. Round up; the gate is a guardrail,
not an exact meter.

## Worked example — ComfyUI as MCP

ComfyUI doesn't speak MCP natively. The community pattern is to
run a **bridge** (a small process that translates MCP `tools/call`
into ComfyUI workflow runs). Several exist; pick one that runs as
a long-lived HTTP server on your LAN.

A typical wiring:

1. Run the bridge — example: `mcp-comfy --listen 0.0.0.0:9201
   --comfy-url http://comfy.lan:8188`. The bridge exposes
   `tools/list` returning the workflows you've published as
   tools (`txt2img`, `img2img`, `upscale`, …).
2. **Register the server** in artist-alley:
   - Name: `comfyui-lan`
   - URL: `http://comfy.lan:9201/mcp`
   - Privacy class: `local` (Q1=YES + Q2=NO — your box, your
     bytes)
   - Auth: `none` (it's on your LAN; if it's exposed to the
     internet, put a bearer token in front)
   - Health interval: `30s` (you'll restart ComfyUI more often
     than the default 60s presumes)
3. **Grant tools** on the detail page. Repeat for each workflow:
   - Tool name: must match what the bridge returns from
     `tools/list` (e.g. `img2img`).
   - Additional capability: `mcp.client.images.write` for
     anything that produces new bytes.
   - Cost estimate: `10000` for a local GPU run is a fine
     placeholder — it costs you electricity, not API credits, but
     leaving it at `0` removes the budget signal entirely.
4. **Enable** the server on the list page once the tools are in.
5. Open `/admin/ai/usage` after invoking a tool a few times —
   `provider=mcp:comfyui-lan` rows will appear there.

The health badge on the list page should flip to **healthy**
within one tick of the health interval. If it stays **unreachable**,
check the bridge logs and `last_health_error` (hover the badge for
the tooltip).

## What stays out of MCP

- **Federation** — MCP registrations are per-instance. They do
  not federate. A federated peer can't see your `mcp.client.use`
  capability and can't trigger an `mcp.invoke()` on your behalf.
  This is deliberate: every operator decides their own
  destinations.
- **Auto-discovery** — there is no "scan your network for MCP
  servers" flow. Every registration is an explicit operator
  decision.
- **Per-user secrets** — `api_secret_ref` is per-server, not
  per-user. The intended model is: the operator wires up the
  bridge with their own credentials; users get gated invocation
  through capabilities, not credentials.

## Troubleshooting

| Symptom                                                              | Likely cause                                                                                                            |
|----------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------|
| Health badge stuck at **unknown**                                    | Goroutine not running. Check `app` logs for `mcp.health.start`. Confirm the server registration is **enabled**.         |
| Health badge **unreachable**, `tools/list` works via curl            | Bearer/header auth misconfigured. Confirm the header name + secret match what curl sends. Watch the bridge access logs. |
| `tools/call` returns `privacy_blocked`                               | Asset's sensitivity is `restricted`/`embargo` and the server's privacy class is `cloud`. Reclassify or pick another.    |
| `tools/call` returns `budget_blocked`                                | Per-period AI cost is at the cap. Raise the cap at `/admin/ai/config` or wait for period rollover.                      |
| Server returns `tool_not_whitelisted`                                | The tool name in the call doesn't match a row on the detail page. Tool names are case-sensitive.                        |
| `last_health_error` shows the right body but status is **degraded**  | Server responded to the JSON-RPC but `tools/list` returned a permanent error code. The bridge is probably misconfigured. |
