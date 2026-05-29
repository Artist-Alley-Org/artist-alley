# Blender script: render an N-frame turntable of an arbitrary 3D
# asset. Driven by preview.model from the Go side.
#
# Invoked as:
#   blender --background --factory-startup --python turntable.py -- \
#     --input  /tmp/asset.fbx \
#     --output /tmp/render    \
#     --frames 36              \
#     --res    512
#
# Output: <output>/frame_0000.png ... frame_NNNN.png. Frame count and
# per-cell resolution come from the caller. The sprite-sheet compose
# step lives in Go (stdlib image/jpeg) — keeping this script focused
# on the one thing only Blender can do, which is "decode the model
# and turn it into pixels."

import argparse
import math
import os
import sys

import bpy
from mathutils import Vector


# ----------------------------------------------------------------------------
# argparse — only consume what comes after `--` (Blender swallows the rest)
# ----------------------------------------------------------------------------

def parse_args() -> argparse.Namespace:
    argv = sys.argv
    if "--" not in argv:
        # Allow running without a separator (still bail with a useful error).
        argv = []
    else:
        argv = argv[argv.index("--") + 1:]
    ap = argparse.ArgumentParser(description="3D turntable renderer")
    ap.add_argument("--input", required=True, help="path to source model")
    ap.add_argument("--output", required=True, help="output directory for frames")
    ap.add_argument("--frames", type=int, default=36)
    ap.add_argument("--res", type=int, default=512)
    ap.add_argument("--samples", type=int, default=32,
                    help="cycles samples per frame (lower = faster, noisier)")
    return ap.parse_args(argv)


# ----------------------------------------------------------------------------
# scene reset
# ----------------------------------------------------------------------------

def reset_scene() -> None:
    """Strip the default scene back to an empty world.

    --factory-startup gives us a scene with a cube + camera + light;
    we don't want any of those (the cube would mess up bbox framing,
    and we install our own camera + lights).
    """
    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    # Clear orphan data so successive runs in a single Blender process
    # don't leak. (preview.model launches a fresh blender per asset,
    # so this is belt-and-suspenders.)
    for block in (bpy.data.meshes, bpy.data.materials,
                  bpy.data.images, bpy.data.cameras, bpy.data.lights):
        for d in list(block):
            block.remove(d)


# ----------------------------------------------------------------------------
# model import — per-extension dispatch
# ----------------------------------------------------------------------------

def import_model(path: str) -> None:
    ext = os.path.splitext(path)[1].lower().lstrip(".")
    if ext in ("glb", "gltf"):
        bpy.ops.import_scene.gltf(filepath=path)
    elif ext == "fbx":
        bpy.ops.import_scene.fbx(filepath=path)
    elif ext == "obj":
        # Blender 4.x ships the new fast OBJ importer at wm.obj_import.
        # 3.x users still on import_scene.obj would set a fallback here.
        bpy.ops.wm.obj_import(filepath=path)
    elif ext == "blend":
        # For .blend we don't import — we open the file as the scene so
        # all of the artist's materials / lighting come along. Camera +
        # light setup below still runs, replacing whatever was in the
        # source so framing stays consistent across all assets.
        bpy.ops.wm.open_mainfile(filepath=path)
        # Clear cameras + lights since we re-add our own canonical rig.
        for obj in list(bpy.data.objects):
            if obj.type in ("CAMERA", "LIGHT"):
                bpy.data.objects.remove(obj, do_unlink=True)
    else:
        raise SystemExit(f"unsupported extension: {ext}")


# ----------------------------------------------------------------------------
# camera framing — fit the bounding box snugly
# ----------------------------------------------------------------------------

def scene_bbox() -> tuple[Vector, Vector]:
    """Return (min, max) corners of the union bbox of all mesh objects."""
    mins = Vector((math.inf, math.inf, math.inf))
    maxs = Vector((-math.inf, -math.inf, -math.inf))
    found = False
    for obj in bpy.context.scene.objects:
        if obj.type != "MESH":
            continue
        found = True
        for corner in obj.bound_box:
            w = obj.matrix_world @ Vector(corner)
            mins.x = min(mins.x, w.x); mins.y = min(mins.y, w.y); mins.z = min(mins.z, w.z)
            maxs.x = max(maxs.x, w.x); maxs.y = max(maxs.y, w.y); maxs.z = max(maxs.z, w.z)
    if not found:
        # No mesh data — render a 1×1×1 cube at origin as a fallback so
        # the worker doesn't crash on a metadata-only / armature-only file.
        return Vector((-0.5, -0.5, -0.5)), Vector((0.5, 0.5, 0.5))
    return mins, maxs


def install_camera(center: Vector, radius: float) -> bpy.types.Object:
    cam_data = bpy.data.cameras.new("turntable-cam")
    cam_data.lens = 50  # standard portrait lens; flatters most subjects
    cam = bpy.data.objects.new("turntable-cam", cam_data)
    bpy.context.scene.collection.objects.link(cam)
    bpy.context.scene.camera = cam

    # Pivot empty at the model center — the camera parents to it so a
    # simple Z rotation of the pivot sweeps the camera around the model.
    pivot = bpy.data.objects.new("turntable-pivot", None)
    pivot.location = center
    bpy.context.scene.collection.objects.link(pivot)

    # Distance + tilt — 25° down from horizontal gives a 3/4 "hero" view.
    tilt_deg = 20
    dist = radius * 2.6
    cam.location = (0, -dist, dist * math.tan(math.radians(tilt_deg)))
    cam.rotation_euler = (math.radians(90 - tilt_deg), 0, 0)
    cam.parent = pivot
    return pivot


# ----------------------------------------------------------------------------
# lighting — neutral 3-point so material + form read consistently
# ----------------------------------------------------------------------------

def install_lights(center: Vector, radius: float) -> None:
    def add(name: str, color: tuple[float, float, float], energy: float,
            pos: tuple[float, float, float]):
        d = bpy.data.lights.new(name, type="AREA")
        d.energy = energy * radius * radius  # scale with model size
        d.color = color
        d.size = radius
        o = bpy.data.objects.new(name, d)
        o.location = (center.x + pos[0] * radius,
                      center.y + pos[1] * radius,
                      center.z + pos[2] * radius)
        # Point at the model centre.
        direction = center - Vector(o.location)
        o.rotation_mode = "QUATERNION"
        o.rotation_quaternion = direction.to_track_quat("-Z", "Y")
        bpy.context.scene.collection.objects.link(o)

    add("key",   (1.0, 0.98, 0.94),  600, ( 2.5,  -1.5,  2.0))   # warm front-left high
    add("fill",  (0.75, 0.85, 1.0),  200, (-2.5,  -0.5,  0.5))   # cool front-right low
    add("rim",   (1.0, 1.0, 1.0),    400, ( 0.0,   2.0,  2.5))   # back-top

    # Soft world background so dark/shadow regions don't go pitch black.
    world = bpy.context.scene.world or bpy.data.worlds.new("World")
    bpy.context.scene.world = world
    world.use_nodes = True
    bg = world.node_tree.nodes.get("Background")
    if bg:
        bg.inputs[0].default_value = (0.05, 0.05, 0.06, 1.0)
        bg.inputs[1].default_value = 1.0


# ----------------------------------------------------------------------------
# render config
# ----------------------------------------------------------------------------

def configure_render(res: int, samples: int) -> None:
    s = bpy.context.scene
    r = s.render
    r.resolution_x = res
    r.resolution_y = res
    r.resolution_percentage = 100
    r.film_transparent = False
    r.image_settings.file_format = "PNG"
    r.image_settings.color_mode = "RGB"
    r.image_settings.color_depth = "8"
    r.image_settings.compression = 15  # smaller PNGs; not lossy

    # Cycles CPU. Eevee needs an OpenGL context (which --background
    # doesn't have without xvfb), so Cycles is the reliable default.
    s.render.engine = "CYCLES"
    s.cycles.device = "CPU"
    s.cycles.samples = samples
    s.cycles.use_denoising = True
    s.cycles.denoiser = "OPENIMAGEDENOISE"
    # Skip slow features that don't read at thumb resolution.
    s.cycles.max_bounces = 4
    s.cycles.diffuse_bounces = 2
    s.cycles.glossy_bounces = 2
    s.cycles.transmission_bounces = 2
    s.cycles.transparent_max_bounces = 4
    s.cycles.volume_bounces = 0

    # sRGB output (the variant ladder expects 8-bit sRGB).
    s.view_settings.view_transform = "Standard"
    s.view_settings.look = "None"


# ----------------------------------------------------------------------------
# render loop
# ----------------------------------------------------------------------------

def render_turntable(pivot: bpy.types.Object, out_dir: str, frames: int) -> None:
    os.makedirs(out_dir, exist_ok=True)
    scene = bpy.context.scene
    for i in range(frames):
        # Y-up rotation around the model. -90° start so frame 0 is the
        # canonical "front" view (camera looking at the model's +Y face).
        pivot.rotation_euler = (0, 0, math.radians(-90 + i * 360.0 / frames))
        scene.render.filepath = os.path.join(out_dir, f"frame_{i:04d}.png")
        bpy.ops.render.render(write_still=True)


# ----------------------------------------------------------------------------
# main
# ----------------------------------------------------------------------------

def main() -> None:
    args = parse_args()
    reset_scene()
    import_model(args.input)

    mins, maxs = scene_bbox()
    center = (mins + maxs) / 2.0
    radius = max((maxs - mins).length / 2.0, 0.1)

    install_camera(center, radius)
    install_lights(center, radius)
    configure_render(args.res, args.samples)

    pivot = bpy.data.objects["turntable-pivot"]
    render_turntable(pivot, args.output, args.frames)


if __name__ == "__main__":
    main()
