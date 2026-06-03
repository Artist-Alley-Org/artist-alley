// DetailsTool — always available, lowest order (slot 0). Hosts
// with richer details (PostHost) register their own DetailsTool
// with a higher .order; the shell renders them all in the
// dropdown so both the generic info and the host-provided details
// stay reachable.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Icon from './Icon.svelte';
import Tips from './Tips.svelte';

export const detailsTool: ToolDef = {
  id: 'details',
  label: 'Details',
  order: 0,
  Icon,
  Body,
  // Details is the always-on default tool — its Tips list the
  // shell-owned global gestures (pan / zoom / reset / fullscreen)
  // plus the tile-mode toggle (image kind only). Hosts that
  // override Details should reuse this same Tips so the gestures
  // stay documented across both bodies.
  Tips,
  isAvailable: () => true,
};

// Re-export the Tips component so PostHost (and any future
// Details-overriding host) can register the same shortcuts list
// alongside their own body.
export { default as DetailsTips } from './Tips.svelte';
