# Blender script: render a turntable + top/bottom reference views of an
# arbitrary 3D asset. Driven by preview.model on the Go side.
#
# Invoked as:
#   blender --background --factory-startup --python turntable.py -- \
#     --input  /tmp/asset.fbx \
#     --output /tmp/render    \
#     --frames 36              \
#     --res    512
#
# Output layout:
#   <output>/turntable/frame_0000.png ... frame_NNNN.png
#   <output>/views/top.png
#   <output>/views/bottom.png
#
# The turntable frames are what the Go handler composes into the sprite
# sheet (hover-scrub UI) AND uploads individually as
# `turntable/NNNN.png` variants for CLIP training data. The top/bottom
# views are reference-only single shots stored as `views/top.png` and
# `views/bottom.png` variants — not part of the scrub.
#
# Camera framing borrows the iterative-fit approach from the
# Projects/thumbnails_renderer codebase: center the model at the world
# origin, compute an initial distance via FOV math, then iteratively
# refine by projecting bbox corners to NDC and checking whether they
# fall outside the frame. Way more reliable than my first pass's
# `dist = maxDim * 2.2` heuristic — handles tiny / huge / lopsided
# models consistently.

import argparse
import math
import os
import sys

import bpy
import mathutils
from mathutils import Vector


# ----------------------------------------------------------------------------
# argparse — only consume what comes after `--` (Blender swallows the rest)
# ----------------------------------------------------------------------------

def parse_args() -> argparse.Namespace:
    argv = sys.argv
    if "--" not in argv:
        argv = []
    else:
        argv = argv[argv.index("--") + 1:]
    ap = argparse.ArgumentParser(description="3D turntable + top/bottom renderer")
    ap.add_argument("--input", required=True, help="path to source model")
    ap.add_argument("--output", required=False, default="",
                    help="output directory root (turntable mode)")
    ap.add_argument("--poster-output", required=False, default="",
                    help="if set, render a single workbench poster to this path "
                         "and exit. Skips the slow Cycles turntable + reference "
                         "views.")
    ap.add_argument("--poster-res", type=int, default=384,
                    help="poster render resolution (square)")
    ap.add_argument("--iso-output", required=False, default="",
                    help="if set, render a single CYCLES isometric shot "
                         "(azimuth 45°, elevation 30°) to this path and exit. "
                         "Used as the col/preview/screen thumbnail for 3D "
                         "assets — workbench poster paints textured Kenney "
                         "models magenta because it can't bind texture nodes "
                         "the same way Cycles does, so the iso pass replaces "
                         "the workbench poster once the rest of the turntable "
                         "has staged companions.")
    ap.add_argument("--iso-res", type=int, default=512,
                    help="iso render resolution (square)")
    ap.add_argument("--iso-samples", type=int, default=32,
                    help="cycles samples for the iso shot")
    ap.add_argument("--frames", type=int, default=36)
    ap.add_argument("--res", type=int, default=512)
    ap.add_argument("--engine", default="cycles",
                    choices=("cycles", "workbench"),
                    help="render engine for the turntable + reference views. "
                         "Cycles = full PBR (slow, photoreal). Workbench = "
                         "viewport-style matcap (fast, consistent across "
                         "untextured formats). Poster pass always uses "
                         "workbench regardless of this setting.")
    ap.add_argument("--samples", type=int, default=32,
                    help="cycles samples per frame (lower = faster, noisier)")
    return ap.parse_args(argv)


# ----------------------------------------------------------------------------
# scene reset + import + default material
# ----------------------------------------------------------------------------

def reset_scene() -> None:
    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for block in (bpy.data.meshes, bpy.data.materials,
                  bpy.data.images, bpy.data.cameras, bpy.data.lights):
        for d in list(block):
            block.remove(d)


def import_model(path: str) -> None:
    ext = os.path.splitext(path)[1].lower().lstrip(".")
    # Blender 4.x ships native operators for every format below; older
    # 3.x docker tags use the legacy import_mesh/import_scene names.
    # If you bump the Blender image and an operator stops resolving,
    # the fallback chain on each branch tries the older name.
    if ext in ("glb", "gltf"):
        bpy.ops.import_scene.gltf(filepath=path)
    elif ext == "fbx":
        bpy.ops.import_scene.fbx(filepath=path)
    elif ext == "obj":
        _try_ops(("wm.obj_import", "import_scene.obj"), filepath=path)
    elif ext == "dae":
        bpy.ops.wm.collada_import(filepath=path)
    elif ext == "ply":
        _try_ops(("wm.ply_import", "import_mesh.ply"), filepath=path)
    elif ext == "stl":
        _try_ops(("wm.stl_import", "import_mesh.stl"), filepath=path)
    elif ext == "3ds":
        _try_ops(("import_scene.max3ds", "import_scene.autodesk_3ds"), filepath=path)
    elif ext in ("x3d", "wrl"):
        bpy.ops.import_scene.x3d(filepath=path)
    elif ext in ("usd", "usda", "usdc", "usdz"):
        bpy.ops.wm.usd_import(filepath=path)
    elif ext == "abc":
        bpy.ops.wm.alembic_import(filepath=path)
    elif ext == "blend":
        bpy.ops.wm.open_mainfile(filepath=path)
        for obj in list(bpy.data.objects):
            if obj.type in ("CAMERA", "LIGHT"):
                bpy.data.objects.remove(obj, do_unlink=True)
    else:
        raise SystemExit(f"unsupported extension: {ext}")


def _try_ops(op_names, **kwargs) -> None:
    """Resolve and call the first operator name that exists.

    Blender 4.x renamed several importers (`import_mesh.ply` →
    `wm.ply_import`); the runtime errors out if you call a renamed
    operator on an older build. This walks a fallback list so the same
    script works against any Blender image we'd realistically ship.
    """
    last_err = None
    for name in op_names:
        ns, op = name.split(".", 1)
        ops_ns = getattr(bpy.ops, ns, None)
        if ops_ns is None:
            continue
        op_fn = getattr(ops_ns, op, None)
        if op_fn is None:
            continue
        try:
            op_fn(**kwargs)
            return
        except Exception as e:  # operator exists but failed at runtime
            last_err = e
            continue
    raise SystemExit(
        f"no usable Blender importer for {op_names}: {last_err}"
    )


def ensure_default_material() -> None:
    """Neutral grey Principled BSDF for meshes without materials.

    Cycles renders empty material slots as the canonical magenta-pink
    'missing shader' indicator. Untextured FBX/OBJ exports trip this
    constantly. We only fill the gaps — never stomp on artist intent.
    """
    fallback = None
    for obj in bpy.context.scene.objects:
        if obj.type != "MESH":
            continue
        mesh = obj.data
        needs_default = (
            not mesh.materials
            or len(mesh.materials) == 0
            or all(m is None for m in mesh.materials)
        )
        if not needs_default:
            continue
        if fallback is None:
            fallback = bpy.data.materials.new(name="aa_default_grey")
            fallback.use_nodes = True
            principled = fallback.node_tree.nodes.get("Principled BSDF")
            if principled is not None:
                principled.inputs["Base Color"].default_value = (0.55, 0.55, 0.55, 1.0)
                principled.inputs["Roughness"].default_value = 0.55
                for k in ("Metallic", "Metalness"):
                    if k in principled.inputs:
                        principled.inputs[k].default_value = 0.0
                        break
        while mesh.materials:
            mesh.materials.pop()
        mesh.materials.append(fallback)


# ----------------------------------------------------------------------------
# bbox + centering — borrowed pattern from thumbnails_renderer
# ----------------------------------------------------------------------------

def scene_bounds() -> tuple[Vector, Vector, Vector] | tuple[None, None, None]:
    """Compute world-space (min, max, dimensions) over all mesh objects."""
    mins = [math.inf] * 3
    maxs = [-math.inf] * 3
    for obj in bpy.context.scene.objects:
        if obj.type != "MESH":
            continue
        for corner in obj.bound_box:
            w = obj.matrix_world @ Vector(corner)
            for i in range(3):
                mins[i] = min(mins[i], w[i])
                maxs[i] = max(maxs[i], w[i])
    if mins[0] == math.inf:
        return None, None, None
    dims = Vector([maxs[i] - mins[i] for i in range(3)])
    return Vector(mins), Vector(maxs), dims


def bbox_center(mins: Vector, maxs: Vector) -> Vector:
    """World-space center of the combined mesh bbox."""
    return (mins + maxs) / 2.0


# ----------------------------------------------------------------------------
# camera setup — FOV math + iterative NDC frame fit
# ----------------------------------------------------------------------------

def install_camera(target: Vector, dimensions: Vector) -> tuple[bpy.types.Object, bpy.types.Object]:
    """Camera + pivot. Camera is parented to pivot at the bbox center.

    The pivot pattern is what B-12b-11 originally used and reliably
    rendered every shape correctly. The FOV-math approach I tried in
    the first B-12d pass had a subtle bug that left every orbital
    frame empty — the pivot-based approach sidesteps it because the
    camera + target relationship is anchored by the parent transform.
    Top + bottom views unparent the camera transiently.

    Lens / clip planes match thumbnails_renderer's guidance: 35mm for
    headroom, clip planes scaled to bbox so near doesn't clip and far
    doesn't cull.
    """
    cam_data = bpy.data.cameras.new("aa-cam")
    cam_data.lens = 35
    max_dim = max(max(dimensions), 0.1)
    cam_data.clip_start = max(0.001, max_dim * 0.001)
    cam_data.clip_end = max_dim * 100
    cam = bpy.data.objects.new("aa-cam", cam_data)
    bpy.context.scene.collection.objects.link(cam)
    bpy.context.scene.camera = cam

    pivot = bpy.data.objects.new("aa-pivot", None)
    pivot.location = target
    bpy.context.scene.collection.objects.link(pivot)
    return cam, pivot


def initial_distance(cam_data: bpy.types.Camera,
                     dimensions: Vector,
                     elevation_deg: float,
                     aspect_ratio: float = 1.0) -> float:
    """FOV-based starting distance that fits the bbox in frame.

    Mirrors thumbnails_renderer's `calculate_camera_distance` — derive
    a viewing distance from the camera's actual FOV instead of the
    "2.6×radius" heuristic that's wrong for any non-cube model.
    """
    sensor_w = cam_data.sensor_width
    lens = cam_data.lens
    fov_h = 2 * math.atan(sensor_w / (2 * lens))
    fov_v = fov_h / aspect_ratio

    width, depth, height = dimensions.x, dimensions.y, dimensions.z
    elev = math.radians(elevation_deg)
    apparent_h = abs(height) * math.cos(elev) + abs(depth) * math.sin(elev)
    apparent_w = max(abs(width), abs(depth))

    padding = 1.2
    dist_for_h = (apparent_h * padding) / (2 * math.tan(fov_v / 2))
    dist_for_w = (apparent_w * padding) / (2 * math.tan(fov_h / 2))
    return max(dist_for_h, dist_for_w, 0.01)


def position_camera(cam: bpy.types.Object,
                    target: Vector,
                    distance: float,
                    azimuth_deg: float,
                    elevation_deg: float,
                    view: str = "orbital") -> None:
    """Place the camera at `distance` from `target`, aimed at `target`.

    `to_track_quat('-Z', 'Y')` is the canonical Blender camera
    orientation: camera looks along its local -Z (forward), local +Y
    is up. Same call works for orbital + top + bottom because Blender
    picks an arbitrary perpendicular when direction is parallel to up.
    """
    if view == "top":
        offset = Vector((0, 0, distance))
    elif view == "bottom":
        offset = Vector((0, 0, -distance))
    else:
        az = math.radians(azimuth_deg)
        el = math.radians(elevation_deg)
        offset = Vector((
            distance * math.cos(el) * math.sin(az),
            -distance * math.cos(el) * math.cos(az),
            distance * math.sin(el),
        ))
    cam.location = target + offset
    direction = target - cam.location  # points from camera to target
    cam.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()


def check_in_frame(scene: bpy.types.Scene,
                   cam: bpy.types.Object,
                   bounds: tuple[Vector, Vector],
                   margin: float = 0.05) -> tuple[bool, float]:
    """Project bbox corners to NDC and report worst overflow.

    Returns (in_frame, overflow). overflow is 0 when the bbox is
    inside the frame; positive values say how far outside in NDC units.
    A value of 0.5 means a corner is at 1.5x the visible half-width.
    """
    mins, maxs = bounds
    corners = [
        Vector((x, y, z))
        for x in (mins.x, maxs.x)
        for y in (mins.y, maxs.y)
        for z in (mins.z, maxs.z)
    ]
    cam_matrix = cam.matrix_world.normalized().inverted()
    aspect = scene.render.resolution_x / scene.render.resolution_y
    cam_data = cam.data
    factor = cam_data.lens / (cam_data.sensor_width / 2)
    frame_limit = 1.0 - margin

    overflow = 0.0
    for c in corners:
        cs = cam_matrix @ c
        if cs.z >= 0:
            # Behind camera — treat as a huge overflow so the caller
            # backs off.
            return False, 2.0
        ndc_x = cs.x * factor / -cs.z
        ndc_y = cs.y * factor / -cs.z * aspect
        ox = max(abs(ndc_x) - frame_limit, 0.0)
        oy = max(abs(ndc_y) - frame_limit, 0.0)
        overflow = max(overflow, ox, oy)
    return overflow == 0.0, overflow


def fit_camera(scene: bpy.types.Scene,
               cam: bpy.types.Object,
               target: Vector,
               bounds: tuple[Vector, Vector],
               dimensions: Vector,
               azimuth_deg: float,
               elevation_deg: float,
               view: str = "orbital",
               max_iter: int = 5) -> float:
    """Place the camera at a fitting distance pointed at `target`.

    Iteratively backs off until the bbox fits in the frame. Returns
    the final distance — turntable uses the same radius for every
    frame so the model doesn't appear to dolly during the spin.
    """
    aspect = scene.render.resolution_x / scene.render.resolution_y
    distance = initial_distance(cam.data, dimensions, elevation_deg, aspect)
    for _ in range(max_iter):
        position_camera(cam, target, distance, azimuth_deg, elevation_deg, view=view)
        in_frame, overflow = check_in_frame(scene, cam, bounds)
        if in_frame:
            return distance
        distance *= 1.0 + overflow + 0.1
    return distance


# ----------------------------------------------------------------------------
# lighting — neutral 3-point (model is centered at origin)
# ----------------------------------------------------------------------------

def install_lights(target: Vector, dimensions: Vector) -> None:
    radius = max(max(dimensions) / 2.0, 0.1)

    def add(name, color, energy, pos):
        d = bpy.data.lights.new(name, type="AREA")
        d.energy = energy * radius * radius
        d.color = color
        d.size = radius
        o = bpy.data.objects.new(name, d)
        o.location = (
            target.x + pos[0] * radius,
            target.y + pos[1] * radius,
            target.z + pos[2] * radius,
        )
        direction = target - Vector(o.location)
        o.rotation_mode = "QUATERNION"
        o.rotation_quaternion = direction.to_track_quat("-Z", "Y")
        bpy.context.scene.collection.objects.link(o)

    # Aim for a soft matcap-style read like the workbench poster:
    # weak directional lights + strong neutral ambient so the surface
    # shape carries through gradients instead of harsh specular bands.
    # Direct lights cut ~70% from the v1 levels; the world background
    # is now the dominant illuminator (3× the v1 strength) at a
    # mid-grey so the model sits in even soft light.
    add("key",  (1.0, 0.98, 0.94), 180, ( 2.5, -1.5,  2.0))
    add("fill", (0.75, 0.85, 1.0),  60, (-2.5, -0.5,  0.5))
    add("rim",  (1.0, 1.0, 1.0),   120, ( 0.0,  2.0,  2.5))

    world = bpy.context.scene.world or bpy.data.worlds.new("World")
    bpy.context.scene.world = world
    world.use_nodes = True
    bg = world.node_tree.nodes.get("Background")
    if bg:
        bg.inputs[0].default_value = (0.35, 0.35, 0.38, 1.0)
        bg.inputs[1].default_value = 1.0


# ----------------------------------------------------------------------------
# render config
# ----------------------------------------------------------------------------

def _configure_film(res: int) -> None:
    """Shared output settings — both engines write PNG at `res` square.
    View transform is set by the per-engine configurers below: Cycles
    wants AgX to roll off highlights from area lights; Workbench's
    studio matcap already reads as a neutral mid-tone, so it stays on
    Standard (which the test users said looks good).
    """
    s = bpy.context.scene
    r = s.render
    r.resolution_x = res
    r.resolution_y = res
    r.resolution_percentage = 100
    r.film_transparent = False
    r.image_settings.file_format = "PNG"
    r.image_settings.color_mode = "RGB"
    r.image_settings.color_depth = "8"
    r.image_settings.compression = 15


def _set_view_transform(name: str, look: str = "None") -> None:
    s = bpy.context.scene
    enum_keys = s.view_settings.bl_rna.properties["view_transform"].enum_items.keys()
    if name in enum_keys:
        s.view_settings.view_transform = name
        s.view_settings.look = look
    else:
        s.view_settings.view_transform = "Standard"
        s.view_settings.look = "None"


def configure_render(res: int, samples: int) -> None:
    """Cycles config — used for the turntable + reference views."""
    _configure_film(res)
    s = bpy.context.scene
    s.render.engine = "CYCLES"
    s.cycles.device = "CPU"
    s.cycles.samples = samples
    s.cycles.use_denoising = True
    s.cycles.denoiser = "OPENIMAGEDENOISE"
    s.cycles.max_bounces = 4
    s.cycles.diffuse_bounces = 2
    s.cycles.glossy_bounces = 2
    s.cycles.transmission_bounces = 2
    s.cycles.transparent_max_bounces = 4
    s.cycles.volume_bounces = 0
    _set_view_transform("AgX", look="AgX - Base Contrast")


def configure_workbench_render(res: int) -> None:
    """Workbench engine — no raytracing, just rasterised solid + textures.

    Roughly 20-30× faster than Cycles at the cost of no GI / no PBR /
    no proper shadows. Good enough for the col / preview / screen
    thumbnail ladder where the user just needs to recognise the model
    while the slow Cycles turntable is still rendering.
    """
    _configure_film(res)
    s = bpy.context.scene
    s.render.engine = "BLENDER_WORKBENCH"
    # Studio lighting + matcap-ish shading reads as 3D without needing
    # any of the scene's lights to render.
    shading = s.display.shading
    shading.light = "STUDIO"
    shading.color_type = "TEXTURE"  # picks up texture maps if present
    shading.show_shadows = False
    shading.show_cavity = True
    shading.cavity_type = "WORLD"
    _set_view_transform("Standard", look="None")


def render_to(path: str) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    bpy.context.scene.render.filepath = path
    bpy.ops.render.render(write_still=True)


def detect_animation_range(scene) -> tuple[int, int] | None:
    """Find the longest action in the scene + return (start, end) in
    integer Blender frames. Returns None when nothing is animated.

    Covers all three places Blender keeps timed motion that turntable
    renders can show:

      - object-level actions (rigid transforms, armature pose data)
      - mesh shape-key actions (morph targets from glTF / FBX)
      - armature object actions (skeletal animation drivers)

    We deliberately don't read scene.frame_end because the glTF
    importer doesn't always bump it — it leaves the scene at the
    factory default (1..250) and stashes the real range in each
    action's frame_range.
    """
    max_end = float(scene.frame_start)
    min_start = float(scene.frame_start)
    found = False

    def consider(action):
        nonlocal max_end, min_start, found
        if action is None:
            return
        rng = action.frame_range
        if rng[1] > rng[0]:
            found = True
            if rng[0] < min_start:
                min_start = rng[0]
            if rng[1] > max_end:
                max_end = rng[1]

    for obj in scene.objects:
        if obj.animation_data:
            consider(obj.animation_data.action)
        if obj.type == 'MESH' and obj.data and obj.data.shape_keys:
            sk = obj.data.shape_keys
            if sk.animation_data:
                consider(sk.animation_data.action)

    if not found:
        return None
    return (int(round(min_start)), int(round(max_end)))


# ----------------------------------------------------------------------------
# main
# ----------------------------------------------------------------------------

def main() -> None:
    args = parse_args()
    reset_scene()
    import_model(args.input)
    ensure_default_material()

    # Don't try to relocate the model — glTF imports' parent hierarchy
    # makes `obj.location -= center` mutations unreliable. Just aim the
    # camera at the model's world-space bbox center; the math works
    # identically whether the model sits at the origin or out at
    # (0, 0, 1_000_000).
    bounds = scene_bounds()
    mins, maxs, dimensions = bounds
    if mins is None:
        mins, maxs = Vector((-0.5, -0.5, -0.5)), Vector((0.5, 0.5, 0.5))
        dimensions = Vector((1.0, 1.0, 1.0))
    bounds_t = (mins, maxs)
    target = bbox_center(mins, maxs)

    install_lights(target, dimensions)
    cam, pivot = install_camera(target, dimensions)

    # Poster mode — a single workbench render at azimuth 0, no Cycles,
    # no top/bottom views. The model handler invokes us in this mode
    # first to seed the col thumbnail before kicking off the long
    # Cycles turntable run.
    if args.poster_output:
        configure_workbench_render(args.poster_res)
        scene = bpy.context.scene
        aspect = scene.render.resolution_x / scene.render.resolution_y
        cam_data = cam.data
        fov_h = 2 * math.atan(cam_data.sensor_width / (2 * cam_data.lens))
        fov_v = fov_h / aspect
        w, d, hgt = dimensions.x, dimensions.y, dimensions.z
        padding = 1.15
        t = math.radians(20)
        v_ext = abs(hgt) * math.cos(t) + abs(d) * math.sin(t)
        h_ext = max(abs(w), abs(d))
        d_v = (v_ext * padding) / (2 * math.tan(fov_v / 2)) if v_ext > 0 else 0
        d_h = (h_ext * padding) / (2 * math.tan(fov_h / 2)) if h_ext > 0 else 0
        distance = max(d_v, d_h, 0.1)
        cam.parent = pivot
        cam.location = (0, -distance, distance * math.tan(t))
        cam.rotation_euler = (math.radians(70), 0, 0)
        pivot.rotation_euler = (0, 0, math.radians(-90))
        render_to(args.poster_output)
        return

    # Isometric thumbnail mode — single Cycles frame at azimuth 45°,
    # elevation 30° (classic isometric / 3-quarter view). Replaces
    # the workbench poster's col/preview/screen output once the rest
    # of the pipeline has staged companions. Workbench renders these
    # same models magenta-pink for textured Kenney assets because its
    # texture-node binding behaves differently from Cycles; the iso
    # pass shares the Cycles path the turntable already uses, so it
    # picks up textures correctly when companions are in place.
    if args.iso_output:
        configure_render(args.iso_res, args.iso_samples)
        scene = bpy.context.scene
        aspect = scene.render.resolution_x / scene.render.resolution_y
        cam_data = cam.data
        fov_h = 2 * math.atan(cam_data.sensor_width / (2 * cam_data.lens))
        fov_v = fov_h / aspect
        w, d, hgt = dimensions.x, dimensions.y, dimensions.z
        padding = 1.20  # iso view ends up corner-to-corner; extra margin
        elev = math.radians(30)
        # Fit using the orbital extent at 30° tilt (taller silhouette
        # than the 20° turntable, so a bit more distance).
        v_ext = abs(hgt) * math.cos(elev) + abs(d) * math.sin(elev)
        h_ext = max(abs(w), abs(d))
        d_v = (v_ext * padding) / (2 * math.tan(fov_v / 2)) if v_ext > 0 else 0
        d_h = (h_ext * padding) / (2 * math.tan(fov_h / 2)) if h_ext > 0 else 0
        distance = max(d_v, d_h, 0.1)
        cam.parent = pivot
        cam.location = (0, -distance, distance * math.tan(elev))
        cam.rotation_euler = (math.radians(90) - elev, 0, 0)
        pivot.rotation_euler = (0, 0, math.radians(-90 + 45))  # azimuth 45°
        render_to(args.iso_output)
        return

    if args.engine == "workbench":
        configure_workbench_render(args.res)
    else:
        configure_render(args.res, args.samples)
    scene = bpy.context.scene

    if not args.output:
        raise SystemExit("turntable mode requires --output")

    # Per-view distance via FOV math — replaces the old `radius * 2.6`
    # heuristic, which over-zoomed wide/tall models because the bbox
    # diagonal is the wrong "size" when the model isn't cubic. A tall
    # thin character (Kenney) was getting 35% of the frame because the
    # diagonal-based distance was 35% too far for its actual silhouette.
    aspect = scene.render.resolution_x / scene.render.resolution_y
    cam_data = cam.data
    fov_h = 2 * math.atan(cam_data.sensor_width / (2 * cam_data.lens))
    fov_v = fov_h / aspect
    padding = 1.15  # leave ~15% margin so the bbox doesn't kiss the edges

    def fit_distance(view: str, tilt_deg: float = 0.0) -> float:
        """Distance from target that fits the bbox in the frame.

        Different apparent extents per view type:
          orbital — at azimuth 0, vertical = height*cos(t) + depth*sin(t),
                    horizontal = width. Over a full spin the widest the
                    horizontal extent ever gets is max(width, depth).
          top     — XY footprint = max(width, depth). Distance must
                    also clear the model's vertical extent or the
                    camera ends up inside a tall model.
          bottom  — symmetrical to top.
        """
        w, d, hgt = dimensions.x, dimensions.y, dimensions.z
        if view in ("top", "bottom"):
            v_ext = max(abs(w), abs(d))
            h_ext = max(abs(w), abs(d))
        else:
            t = math.radians(tilt_deg)
            v_ext = abs(hgt) * math.cos(t) + abs(d) * math.sin(t)
            h_ext = max(abs(w), abs(d))
        d_v = (v_ext * padding) / (2 * math.tan(fov_v / 2)) if v_ext > 0 else 0
        d_h = (h_ext * padding) / (2 * math.tan(fov_h / 2)) if h_ext > 0 else 0
        framing = max(d_v, d_h, 0.1)
        if view in ("top", "bottom"):
            # Camera lives on the Z axis at target.z ± framing; the
            # model itself extends ±height/2 either side of target.z.
            # If framing < height/2 the camera ends up inside the
            # model. Add half-height as clearance.
            framing += abs(hgt) / 2.0
        return framing

    # ─── Turntable: N orbital frames around Z, 20° tilt ────────────────
    tilt_deg = 20
    tilt_rad = math.radians(tilt_deg)
    distance = fit_distance("orbital", tilt_deg)
    cam.parent = pivot
    cam.location = (0, -distance, distance * math.tan(tilt_rad))
    cam.rotation_euler = (math.radians(90 - tilt_deg), 0, 0)
    turntable_dir = os.path.join(args.output, "turntable")

    # Animation-aware playhead: when the imported scene carries a
    # morph / armature / object-level animation, distribute the full
    # clip evenly across the camera orbit so the model actually
    # animates while the camera circles it. With no animation, every
    # turntable frame stays at frame 1 (rest pose).
    anim_range = detect_animation_range(scene)
    if anim_range is not None:
        anim_start, anim_end = anim_range
    else:
        anim_start = anim_end = scene.frame_start

    for i in range(args.frames):
        pivot.rotation_euler = (0, 0, math.radians(-90 + i * 360.0 / args.frames))
        if anim_end > anim_start:
            # i in [0, frames-1] → t in [0, 1].
            t = i / max(1, args.frames - 1)
            frame = int(round(anim_start + t * (anim_end - anim_start)))
            scene.frame_set(frame)
        else:
            scene.frame_set(scene.frame_start)
        render_to(os.path.join(turntable_dir, f"frame_{i:04d}.png"))

    # ─── Reference views: top + bottom ─────────────────────────────────
    # Pin to the rest pose. The reference views are about giving the
    # post-detail page a clean "top-down" and "looking-up" inset —
    # not a moment from the animation.
    scene.frame_set(scene.frame_start)
    cam.parent = None
    views_dir = os.path.join(args.output, "views")

    top_distance = fit_distance("top")
    cam.location = (target.x, target.y, target.z + top_distance)
    direction = target - Vector(cam.location)
    cam.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()
    render_to(os.path.join(views_dir, "top.png"))

    bot_distance = fit_distance("bottom")
    cam.location = (target.x, target.y, target.z - bot_distance)
    direction = target - Vector(cam.location)
    cam.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()
    render_to(os.path.join(views_dir, "bottom.png"))


if __name__ == "__main__":
    main()
