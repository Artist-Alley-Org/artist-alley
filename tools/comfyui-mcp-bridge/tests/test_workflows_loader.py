"""Unit tests for the workflow loader + placeholder substitution.

No live ComfyUI / network needed; these are pure JSON + string
tests covering the corners operators are most likely to hit.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from comfyui_mcp_bridge.workflows_loader import (
    Workflow,
    _extract_placeholders,
    _substitute,
    load_workflows,
)


def test_placeholder_extraction_finds_all_tokens() -> None:
    body = {
        "3": {"inputs": {"seed": "<<SEED>>", "steps": "<<STEPS>>", "cfg": 8}},
        "6": {"inputs": {"text": "<<PROMPT>>"}},
        "_aside": "this <<NOT_A_TOKEN>> still counts because the regex is lenient",
    }
    found = _extract_placeholders(body)
    assert found == {"SEED", "STEPS", "PROMPT", "NOT_A_TOKEN"}


def test_substitute_whole_string_preserves_value_type() -> None:
    body = {
        "ksampler": {"seed": "<<SEED>>", "steps": "<<STEPS>>", "denoise": "<<DENOISE>>"},
    }
    out = _substitute(body, {"SEED": 12345, "STEPS": 20, "DENOISE": 0.7})
    assert out == {"ksampler": {"seed": 12345, "steps": 20, "denoise": 0.7}}


def test_substitute_mixed_content_interpolates_to_string() -> None:
    body = {"label": "seed=<<SEED>> for prompt <<PROMPT>>"}
    out = _substitute(body, {"SEED": 42, "PROMPT": "watercolour sketch"})
    assert out == {"label": "seed=42 for prompt watercolour sketch"}


def test_workflow_render_rejects_missing_placeholder() -> None:
    wf = Workflow(
        name="x",
        path=Path("/tmp/x.json"),
        body={"n": {"seed": "<<SEED>>", "prompt": "<<PROMPT>>"}},
        meta={},
    )
    with pytest.raises(KeyError) as exc_info:
        wf.render({"SEED": 1})  # missing PROMPT
    assert "PROMPT" in str(exc_info.value)


def test_load_workflows_skips_invalid_filenames(tmp_path: Path) -> None:
    (tmp_path / "img2img.json").write_text(json.dumps({"_meta": {"v": 1}, "n": {}}))
    (tmp_path / "Untitled-3.json").write_text("{}")  # invalid name → skipped
    (tmp_path / "SDXL prompt v2.json").write_text("{}")  # spaces → skipped
    (tmp_path / "not-json.txt").write_text("hello")  # not .json → skipped

    out = load_workflows(tmp_path)
    assert set(out.keys()) == {"img2img"}


def test_load_workflows_second_dir_overrides_first(tmp_path: Path) -> None:
    bundled = tmp_path / "bundled"
    operator = tmp_path / "operator"
    bundled.mkdir()
    operator.mkdir()
    (bundled / "img2img.json").write_text(json.dumps({"version": "bundled"}))
    (operator / "img2img.json").write_text(json.dumps({"version": "operator"}))

    out = load_workflows(bundled, operator)
    assert out["img2img"].body == {"version": "operator"}


def test_load_workflows_skips_invalid_json(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    (tmp_path / "broken.json").write_text("{not valid json")
    (tmp_path / "good.json").write_text(json.dumps({"x": {}}))

    out = load_workflows(tmp_path)
    assert set(out.keys()) == {"good"}
