// DetailsTool — always available, lowest order (slot 0). Hosts
// with richer details (PostHost) register their own DetailsTool
// with a higher .order; the shell renders them all in the
// dropdown so both the generic info and the host-provided details
// stay reachable.

import type { ToolDef } from '../contract';
import Body from './Body.svelte';
import Icon from './Icon.svelte';

export const detailsTool: ToolDef = {
  id: 'details',
  label: 'Details',
  order: 0,
  Icon,
  Body,
  isAvailable: () => true,
  // No Tips — Details is read-only info, no shortcuts to document.
};
