"""A/B harness: render each 3D format with Cycles AND Workbench so we
can pick the better engine per format.

Run inside the app container with Blender available:

  docker compose exec -T app blender --background --factory-startup \
    --python /app/blender/ab_engine_test.py -- --out /tmp/ab

When it finishes, /tmp/ab/index.html shows every (format, engine)
pair side by side. Copy it out with:

  docker compose cp app:/tmp/ab ./_ab

Then open ./_ab/index.html in a browser.
"""

import argparse
import os
import subprocess
import sys
import time
from typing import Iterable

import bpy

SCRIPT_DIR = "/app/blender"  # bind-resolved at runtime inside the container


# Formats we care about. Each tuple is (format-name, file-extension,
# blender-export-op-or-callable). Some formats need an addon enabled
# before export.
FORMATS: list[tuple[str, str, str]] = [
    ("glb",  "glb",  "export_scene.gltf"),       # default export_format=GLB
    ("gltf", "gltf", "export_scene.gltf"),       # GLTF_SEPARATE
    ("obj",  "obj",  "wm.obj_export"),
    ("fbx",  "fbx",  "export_scene.fbx"),
    ("dae",  "dae",  "wm.collada_export"),
    ("ply",  "ply",  "wm.ply_export"),
    ("stl",  "stl",  "wm.stl_export"),
    ("3ds",  "3ds",  "export_scene.max3ds"),     # 4.x op name; falls back below
    ("x3d",  "x3d",  "export_scene.x3d"),
    ("usd",  "usd",  "wm.usd_export"),
    ("usdz", "usdz", "wm.usd_export"),
    ("abc",  "abc",  "wm.alembic_export"),
    ("blend","blend","wm.save_as_mainfile"),
]


def parse_args() -> argparse.Namespace:
    argv = sys.argv[sys.argv.index("--") + 1:] if "--" in sys.argv else []
    p = argparse.ArgumentParser()
    p.add_argument("--out", default="/tmp/ab", help="output directory")
    p.add_argument("--res", type=int, default=384, help="render resolution")
    return p.parse_args(argv)


def reset_scene() -> None:
    bpy.ops.wm.read_factory_settings(use_empty=True)
    for c in (bpy.data.objects, bpy.data.meshes, bpy.data.materials,
              bpy.data.lights, bpy.data.cameras, bpy.data.images,
              bpy.data.textures):
        for d in list(c):
            c.remove(d)


def make_textured_sample(name: str) -> None:
    """A torus with a procedural checker so Cycles' PBR shine has
    something to play with — that's where Cycles should pull ahead."""
    reset_scene()
    bpy.ops.mesh.primitive_torus_add(
        major_radius=2.0, minor_radius=0.7,
        major_segments=64, minor_segments=24,
    )
    obj = bpy.context.active_object
    obj.name = name
    mat = bpy.data.materials.new(f"{name}_mat")
    mat.use_nodes = True
    tree = mat.node_tree
    bsdf = tree.nodes.get("Principled BSDF")
    checker = tree.nodes.new("ShaderNodeTexChecker")
    checker.inputs["Color1"].default_value = (0.85, 0.45, 0.20, 1.0)
    checker.inputs["Color2"].default_value = (0.10, 0.20, 0.40, 1.0)
    checker.inputs["Scale"].default_value = 6.0
    coord = tree.nodes.new("ShaderNodeTexCoord")
    tree.links.new(coord.outputs["Generated"], checker.inputs["Vector"])
    tree.links.new(checker.outputs["Color"], bsdf.inputs["Base Color"])
    bsdf.inputs["Roughness"].default_value = 0.35
    bsdf.inputs["Metallic"].default_value = 0.4
    obj.data.materials.append(mat)


def export_format(fmt: str, op_name: str, path: str) -> bool:
    """Run the export op. Returns True on success, False on a missing
    operator or runtime failure (we log + skip rather than abort)."""
    ns, op = op_name.split(".", 1)
    ns_obj = getattr(bpy.ops, ns, None)
    if ns_obj is None:
        print(f"[skip] {fmt}: namespace bpy.ops.{ns} missing")
        return False
    fn = getattr(ns_obj, op, None)
    if fn is None:
        # Try fallback names for ops Blender renamed across versions.
        fallbacks = {
            "export_scene.max3ds": ["export_scene.autodesk_3ds"],
            "wm.obj_export":       ["export_scene.obj"],
            "wm.ply_export":       ["export_mesh.ply"],
            "wm.stl_export":       ["export_mesh.stl"],
        }
        for fb in fallbacks.get(op_name, []):
            ns2, op2 = fb.split(".", 1)
            ns2_obj = getattr(bpy.ops, ns2, None)
            if ns2_obj is None:
                continue
            fn2 = getattr(ns2_obj, op2, None)
            if fn2 is not None:
                fn = fn2
                break
    if fn is None:
        print(f"[skip] {fmt}: op {op_name} not resolvable")
        return False
    try:
        kw = {"filepath": path}
        if op_name == "export_scene.gltf" and fmt == "gltf":
            kw["export_format"] = "GLTF_SEPARATE"
        fn(**kw)
        return True
    except Exception as e:
        print(f"[skip] {fmt}: export raised {e}")
        return False


def render_with_engine(sample_path: str, engine: str, out_dir: str, res: int) -> tuple[bool, float]:
    """Re-invoke Blender (turntable.py) with the engine flag. Renders 1
    frame at azimuth 0 to keep the harness fast (12 formats × 2 engines
    × 36 frames = 864 renders would be unbearable).
    Returns (success, elapsed_seconds).
    """
    os.makedirs(out_dir, exist_ok=True)
    poster_path = os.path.join(out_dir, "poster.png")
    t0 = time.time()
    cmd = [
        "blender", "--background", "--factory-startup",
        "--disable-autoexec", "--python-exit-code", "1",
        "--python", os.path.join(SCRIPT_DIR, "turntable.py"),
        "--",
        "--input", sample_path,
        "--poster-output", poster_path,
        "--poster-res", str(res),
    ] if engine == "workbench" else [
        # For Cycles, use turntable mode but only render 1 frame.
        "blender", "--background", "--factory-startup",
        "--disable-autoexec", "--python-exit-code", "1",
        "--python", os.path.join(SCRIPT_DIR, "turntable.py"),
        "--",
        "--input", sample_path,
        "--output", out_dir,
        "--engine", "cycles",
        "--frames", "1",
        "--res", str(res),
        "--samples", "32",
    ]
    try:
        r = subprocess.run(cmd, capture_output=True, timeout=180)
        elapsed = time.time() - t0
        ok = r.returncode == 0
        if not ok:
            print(f"[render] {engine} failed: {r.stderr.decode(errors='replace')[-400:]}")
        return ok, elapsed
    except Exception as e:
        return False, time.time() - t0


def write_index(out: str, rows: list[dict]) -> None:
    """Static HTML grid: one row per format, columns for cycles +
    workbench with timings underneath."""
    style = """
    body { font-family: system-ui, sans-serif; background: #111; color: #eee;
           margin: 0; padding: 24px; }
    h1 { margin-top: 0; }
    table { border-collapse: collapse; }
    td, th { padding: 12px; text-align: center; }
    th { font-size: 14px; opacity: 0.7; }
    .fmt { font-weight: 600; font-size: 18px; }
    img { width: 384px; height: 384px; image-rendering: auto;
          background: #222; border-radius: 6px; }
    .miss { width: 384px; height: 384px; background: #2a1a1a;
            color: #b88; display: flex; align-items: center;
            justify-content: center; border-radius: 6px; }
    .t { opacity: 0.6; font-size: 13px; margin-top: 6px; }
    """
    cells = []
    for row in rows:
        f = row["fmt"]
        def col(engine: str) -> str:
            if not row[engine]["ok"]:
                return f"<td><div class='miss'>render failed</div></td>"
            rel = row[engine]["rel"]
            return f"<td><img src='{rel}'><div class='t'>{row[engine]['secs']:.1f}s</div></td>"
        cells.append(
            f"<tr><td class='fmt'>{f}</td>{col('cycles')}{col('workbench')}</tr>"
        )
    html = f"""<!doctype html>
<html><head><meta charset='utf-8'><title>3D engine A/B</title>
<style>{style}</style></head>
<body>
<h1>Per-format Cycles vs Workbench</h1>
<p>Pick the engine that reads better for each format. Workbench is ~5×
faster — go workbench unless Cycles clearly wins on visible PBR.</p>
<table><tr><th></th><th>Cycles</th><th>Workbench</th></tr>
{''.join(cells)}
</table></body></html>
"""
    with open(os.path.join(out, "index.html"), "w") as f:
        f.write(html)


def main() -> None:
    args = parse_args()
    os.makedirs(args.out, exist_ok=True)
    samples_dir = os.path.join(args.out, "samples")
    os.makedirs(samples_dir, exist_ok=True)

    # 1) Build the canonical sample scene + export to every format.
    make_textured_sample("ab_torus")
    exported: list[tuple[str, str]] = []
    for fmt, ext, op in FORMATS:
        path = os.path.join(samples_dir, f"ab_torus.{ext}")
        if export_format(fmt, op, path) and os.path.exists(path):
            exported.append((fmt, path))
            print(f"[exported] {fmt}: {os.path.getsize(path)} bytes")
        else:
            print(f"[exported] {fmt}: FAILED")

    # 2) Render each one with both engines.
    rows: list[dict] = []
    for fmt, src in exported:
        out_cycles = os.path.join(args.out, fmt, "cycles")
        out_workbench = os.path.join(args.out, fmt, "workbench")
        ok_c, t_c = render_with_engine(src, "cycles", out_cycles, args.res)
        ok_w, t_w = render_with_engine(src, "workbench", out_workbench, args.res)
        rows.append({
            "fmt": fmt,
            "cycles":    {"ok": ok_c, "rel": f"{fmt}/cycles/turntable/frame_0000.png", "secs": t_c},
            "workbench": {"ok": ok_w, "rel": f"{fmt}/workbench/poster.png",            "secs": t_w},
        })
        print(f"[rendered] {fmt}: cycles={t_c:.1f}s workbench={t_w:.1f}s")

    # 3) Write the comparison page.
    write_index(args.out, rows)
    print(f"\nopen {args.out}/index.html")


if __name__ == "__main__":
    main()
