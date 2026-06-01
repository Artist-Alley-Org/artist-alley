<script lang="ts">
  // Sprite-tool Tips — extracted verbatim from the SpriteToolPanel
  // footer that's now retired. Renders <dt>/<dd> pairs as direct
  // children of the shell's TipsSection <dl> grid.
  import type { ToolContext } from '../contract';
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  let { ctx: _ctx }: { ctx: ToolContext } = $props();
</script>

<dt class="font-mono text-fg">Space</dt><dd class="text-fg-muted">Play / pause</dd>
<dt class="font-mono text-fg">,</dt><dd class="text-fg-muted">Previous frame</dd>
<dt class="font-mono text-fg">.</dt><dd class="text-fg-muted">Next frame</dd>
<dt class="font-mono text-fg">F3</dt><dd class="text-fg-muted">Toggle onion skin</dd>
<dt class="col-span-2 mt-1 text-fg-muted/70">Timeline strip</dt>
<dt class="font-mono text-fg">Click</dt><dd class="text-fg-muted">Jump to that frame · clears any active sub-range</dd>
<dt class="font-mono text-fg">Drag</dt><dd class="text-fg-muted">Drag across tiles to set a loop range</dd>
<dt class="font-mono text-fg">Shift+click</dt><dd class="text-fg-muted">Extend the existing range to that frame</dd>
<dt class="col-span-2 mt-1 text-fg-muted/70">Slicing</dt>
<dt class="font-mono text-fg">Cell W / H</dt><dd class="text-fg-muted">Sprite size in pixels — auto-guessed from detection on load</dd>
<dt class="font-mono text-fg">Start / End</dt><dd class="text-fg-muted">Frame range to loop — narrow to one row of a multi-section sheet</dd>
<dt class="col-span-2 mt-1 text-fg-muted/70">Auto-detect</dt>
<dt class="font-mono text-fg">Alpha</dt><dd class="text-fg-muted">Use the PNG's transparency channel (most pixel-art sheets)</dd>
<dt class="font-mono text-fg">Colour</dt><dd class="text-fg-muted">Treat a single colour as background — for sheets without alpha</dd>
<dt class="font-mono text-fg">Merge gap</dt><dd class="text-fg-muted">Glue separated pixels (floating hats, accessories) into one sprite</dd>
<dt class="col-span-2 mt-1 text-fg-muted/70">Save</dt>
<dt class="font-mono text-fg">Companion JSON</dt><dd class="text-fg-muted">Persists frames + tags as a sidecar — reloads pick them up automatically</dd>
<dt class="col-span-2 mt-1 text-fg-muted/70">Export</dt>
<dt class="font-mono text-fg">GIF</dt><dd class="text-fg-muted">Animated GIF of the current frame range at the current FPS</dd>
<dt class="font-mono text-fg">Sheet</dt><dd class="text-fg-muted">Packed PNG + TexturePacker JSON describing the layout</dd>
<dt class="font-mono text-fg">Zip</dt><dd class="text-fg-muted">Each frame as an independent PNG, ordered by filename</dd>
