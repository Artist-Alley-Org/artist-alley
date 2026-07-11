"""Workflow JSON loader + placeholder-token substitution.

Operators ship ComfyUI workflow exports as plain JSON files in the
configured workflows directory. The bridge reads them at startup,
extracts the ``<<TOKEN>>`` placeholders, and at queue time
substitutes operator-supplied values before posting to ComfyUI.

The substitution is dumb-on-purpose — it walks the JSON tree
substring-replacing the literal ``<<TOKEN>>`` text with the value's
JSON encoding. This means a workflow can use a placeholder where
ComfyUI expects either a string (prompt text) or a number (seed,
steps) without the bridge needing per-token type knowledge: the
operator put the placeholder in a string position, gets a string
back; put it in a number-typed input, the bridge writes a JSON
number there.
"""

from __future__ import annotations

import json
import logging
import re
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)

# Placeholder grammar: <<UPPER_SNAKE>>. Liberal on the inside so
# operators can pick descriptive names; strict on the wrap so
# we don't accidentally match prose inside an operator-added
# description.
PLACEHOLDER_RE = re.compile(r"<<([A-Z][A-Z0-9_]*)>>")


class Workflow:
    """A loaded workflow JSON + its discovered placeholders.

    Equality + hashing fall back to the source path so the
    workflow registry can de-dup overrides cleanly.
    """

    def __init__(self, name: str, path: Path, body: dict[str, Any], meta: dict[str, Any]) -> None:
        self.name = name
        self.path = path
        self.body = body
        self.meta = meta
        self.placeholders = _extract_placeholders(body)

    def render(self, values: dict[str, Any]) -> dict[str, Any]:
        """Return a deep copy with all ``<<TOKEN>>`` occurrences
        replaced by ``values[TOKEN]``. Missing tokens raise
        ``KeyError`` — the bridge catches and surfaces as a tool
        invocation error.
        """
        missing = self.placeholders - set(values.keys())
        if missing:
            raise KeyError(f"workflow {self.name!r} missing placeholder values: {sorted(missing)}")
        return _substitute(self.body, values)


def load_workflows(*dirs: Path) -> dict[str, Workflow]:
    """Scan one or more directories for ``*.json`` files. Later
    directories override earlier ones on filename collision so an
    operator drop-in beats the bundled defaults.

    Files with names that don't match ``[a-z][a-z0-9_]*\\.json`` get
    skipped + logged at WARNING — keeps the tool catalogue clean
    (operators occasionally drop ``Untitled-3.json`` or
    ``SDXL prompt v2.json`` exports straight from ComfyUI).
    """
    valid_name = re.compile(r"^[a-z][a-z0-9_]*\.json$")
    out: dict[str, Workflow] = {}
    for d in dirs:
        if not d or not d.exists():
            continue
        for p in sorted(d.glob("*.json")):
            if not valid_name.match(p.name):
                logger.warning(
                    "skipping workflow %s — filename must be lower-snake-case .json", p
                )
                continue
            try:
                raw = json.loads(p.read_text())
            except json.JSONDecodeError as e:
                logger.error("workflow %s is invalid JSON, skipping: %s", p, e)
                continue
            if not isinstance(raw, dict):
                logger.error("workflow %s top-level must be an object, skipping", p)
                continue
            meta = raw.pop("_meta", {}) if isinstance(raw.get("_meta"), dict) else {}
            name = p.stem
            out[name] = Workflow(name=name, path=p, body=raw, meta=meta)
            logger.info(
                "workflow.loaded name=%s path=%s placeholders=%s",
                name,
                p,
                sorted(out[name].placeholders),
            )
    return out


def _extract_placeholders(body: Any) -> set[str]:
    found: set[str] = set()
    _walk_strings(body, lambda s: found.update(PLACEHOLDER_RE.findall(s)))
    return found


def _walk_strings(node: Any, fn: Any) -> None:
    if isinstance(node, str):
        fn(node)
    elif isinstance(node, dict):
        for v in node.values():
            _walk_strings(v, fn)
    elif isinstance(node, list):
        for v in node:
            _walk_strings(v, fn)


def _substitute(node: Any, values: dict[str, Any]) -> Any:
    """Walk the tree returning a new tree with placeholders
    replaced. String nodes that consist of EXACTLY one
    placeholder get the raw value (preserving its type — int /
    float / dict); strings with mixed content get string
    interpolation (the value's JSON encoding without quoting).
    """
    if isinstance(node, str):
        m = PLACEHOLDER_RE.fullmatch(node)
        if m:
            # Whole-string placeholder — return the raw value so a
            # workflow can use <<SEED>> in a number-typed input.
            return values[m.group(1)]
        # Mixed-content interpolation: replace each placeholder
        # with the str() of its value.
        return PLACEHOLDER_RE.sub(lambda mm: str(values[mm.group(1)]), node)
    if isinstance(node, dict):
        return {k: _substitute(v, values) for k, v in node.items()}
    if isinstance(node, list):
        return [_substitute(v, values) for v in node]
    return node
