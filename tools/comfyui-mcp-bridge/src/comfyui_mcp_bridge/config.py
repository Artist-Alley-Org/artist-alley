"""Bridge configuration — typed via pydantic-settings so every
knob is documented + validated at startup. Operator sets values
via environment variables (CMB_*).
"""

from __future__ import annotations

from pathlib import Path
from typing import Literal

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict


class BridgeConfig(BaseSettings):
    """All operator knobs in one place.

    Read from environment variables prefixed ``CMB_`` (for
    "ComfyUI MCP Bridge"). A ``.env`` file in the working
    directory is also picked up — useful for development; for
    production the operator should set real environment
    variables in the systemd unit / Docker compose file.
    """

    model_config = SettingsConfigDict(
        env_prefix="CMB_",
        env_file=".env",
        env_file_encoding="utf-8",
        case_sensitive=False,
    )

    # --- ComfyUI connection ---

    comfy_url: str = Field(
        default="http://127.0.0.1:8188",
        description=(
            "Base URL of the operator's ComfyUI instance. The "
            "bridge appends /prompt, /history/{prompt_id}, /view "
            "and /ws to this. Defaults to the ComfyUI dev-server "
            "default."
        ),
    )

    comfy_timeout_s: float = Field(
        default=300.0,
        ge=10.0,
        description=(
            "Hard wall-clock cap (seconds) on one tool invocation. "
            "ComfyUI generation can legitimately take minutes on "
            "low-end GPUs; default 5 minutes is a generous "
            "ceiling. Operator increases this for slow hardware."
        ),
    )

    # --- MCP server (HTTP transport only — stdio is unbound) ---

    http_host: str = Field(
        default="0.0.0.0",
        description=(
            "Interface the HTTP transport binds to. Default 0.0.0.0 "
            "exposes the bridge to the LAN so artist-alley can call "
            "in; bind to 127.0.0.1 if running in the same network "
            "namespace as AA (docker-compose with bridge networking)."
        ),
    )

    http_port: int = Field(
        default=9201,
        ge=1,
        le=65535,
        description="TCP port the HTTP transport listens on.",
    )

    # --- Auth ---

    auth_kind: Literal["none", "bearer", "header"] = Field(
        default="none",
        description=(
            "How the bridge expects callers to authenticate. "
            "'none' — anyone on the LAN can call (use only when "
            "the bridge is bound to a trusted interface). "
            "'bearer' — Authorization: Bearer <CMB_AUTH_TOKEN>. "
            "'header' — <CMB_AUTH_HEADER_NAME>: <CMB_AUTH_TOKEN>. "
            "Must match the operator's MCP-server registration "
            "in artist-alley's /admin/ai/mcp-clients."
        ),
    )

    auth_token: str = Field(
        default="",
        description=(
            "Shared secret for bearer or header auth. Empty + "
            "auth_kind != none → server refuses to start (operator "
            "must pick).  Generate with `openssl rand -hex 32`."
        ),
    )

    auth_header_name: str = Field(
        default="X-API-Key",
        description="Custom header name when auth_kind = 'header'.",
    )

    # --- Workflow paths ---

    workflows_dir: Path = Field(
        default=Path(__file__).parent / "workflows",
        description=(
            "Directory the bridge scans for ComfyUI workflow JSON. "
            "Files named after a typed op (img2img.json, "
            "inpaint.json, outpaint.json, variations.json, "
            "remove_bg.json) override the bundled defaults; any "
            "other *.json file is auto-exposed as a "
            "workflow:<filename-stem> tool."
        ),
    )

    extra_workflows_dir: Path | None = Field(
        default=None,
        description=(
            "Optional second directory checked AFTER workflows_dir. "
            "Lets operators drop custom workflows next to ComfyUI "
            "without editing the bundled bridge files. Same naming "
            "rules; overrides bundled workflows on name conflict."
        ),
    )

    # --- Logging ---

    log_level: Literal["DEBUG", "INFO", "WARNING", "ERROR"] = Field(
        default="INFO",
        description="Python logging level for the bridge.",
    )

    def validate_runtime(self) -> None:
        """Cross-field validation that pydantic can't express
        declaratively. Called from server.main_* on startup so a
        bad config fails fast with a human-readable error.
        """
        if self.auth_kind in ("bearer", "header") and not self.auth_token:
            raise ValueError(
                f"CMB_AUTH_KIND={self.auth_kind!r} requires CMB_AUTH_TOKEN to be set "
                "(generate with `openssl rand -hex 32`)"
            )
        if not self.workflows_dir.exists():
            raise ValueError(
                f"CMB_WORKFLOWS_DIR={self.workflows_dir!r} does not exist — "
                "either fix the path or run the bridge from a directory "
                "where the bundled workflows ship."
            )
