"""comfyui-mcp-bridge — Model Context Protocol server for ComfyUI.

Exposes ComfyUI workflows as MCP tools so any MCP-aware client
(Claude Desktop, artist-alley's MCP-client subsystem from Phase
1.53.A, future agentic workflows in Phase 1.55) can invoke them
without speaking ComfyUI's REST + WebSocket protocol directly.

The bridge ships:

  - Five typed image-edit tools out of the box: ``img2img``,
    ``inpaint``, ``outpaint``, ``variations``, ``remove_bg``.
    Their input schemas are stable across ComfyUI versions; the
    underlying workflow JSON lives under ``workflows/`` and is
    operator-editable.

  - Workflow auto-discovery: any ``*.json`` file in
    ``workflows/`` (or the operator-configured directory) is
    exposed as a ``workflow:<filename-stem>`` tool. Parameter
    schema is derived from ``<<TOKEN>>`` placeholder slots in
    the JSON; operators drop in their own ComfyUI workflow
    exports and they become callable immediately.

  - Two transports: HTTP (the canonical artist-alley
    integration, run as ``comfyui-mcp-bridge``) and stdio (for
    Claude Desktop or other single-process MCP clients, run as
    ``comfyui-mcp-bridge-stdio``).

See the project README for installation + the operator setup doc
in artist-alley (docs/operator/comfyui-mcp-bridge-setup.md) for
the full integration walkthrough.
"""

__version__ = "0.1.0"
