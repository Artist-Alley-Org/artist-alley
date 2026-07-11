<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // SpriteTool body — thin wrapper that mounts the existing
  // SpriteToolPanel against ctx.spriteSession. The panel was
  // already self-contained; we just adapt the prop shape so it
  // plugs into the registry-driven shell.
  //
  // SpriteToolPanel's internal Tips section gets removed in this
  // same commit — the shell renders Tips via the contract's Tips
  // component instead, so the visual footer stays identical but
  // there's one global owner.

  import SpriteToolPanel from '../../SpriteToolPanel.svelte';
  import type { ToolContext } from '../contract';

  let { ctx }: { ctx: ToolContext } = $props();
</script>

{#if ctx.spriteSession}
  <SpriteToolPanel bind:session={ctx.spriteSession} />
{/if}
