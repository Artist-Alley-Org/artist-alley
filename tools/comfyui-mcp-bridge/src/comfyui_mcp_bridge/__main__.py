"""Allow ``python -m comfyui_mcp_bridge`` invocation. Default
transport is HTTP — operators serving Claude Desktop via stdio
invoke ``comfyui-mcp-bridge-stdio`` instead.
"""

from .server import main_http

if __name__ == "__main__":
    main_http()
