// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Helper for hosts that want to register a tool whose body / tips
// live as snippets in the host's own scope (PostHost's
// postSocialPane reads PostHost-local state — `post`, `author`,
// `likeBusy`, etc — so extracting it into a standalone Body would
// mean a 25-prop surface). Hosts call defineSnippetTool() to build
// a ToolDef whose Body + Tips delegate to SnippetBody / SnippetTips,
// which look up the snippet via the conventional hostHooks key.
//
// Usage (in the host file):
//   const postDetailsTool = defineSnippetTool({
//     id: 'post-details', label: 'Post', order: 1, Icon: PostIcon,
//     hasTips: true,
//   });
//   // ...
//   <AssetPlaylist
//     customTools={[postDetailsTool]}
//     hostHooks={{ [snippetToolHookKey('post-details')]: { body: postSocialPane, tips: postTips } }}
//   />

import type { ToolDef, ToolContext } from './contract';
import type { Component } from 'svelte';
import SnippetBody from './SnippetBody.svelte';
import SnippetTips from './SnippetTips.svelte';

interface DefineSnippetToolOpts {
  id: string;
  label: string;
  order: number;
  Icon: Component<{ ctx: ToolContext }>;
  /** Optional dynamic label — same semantics as ToolDef.labelFn.
   *  Lets a host's snippet-tool return a ctx-derived header (e.g.
   *  "{post title} Details") without needing a custom ToolDef. */
  labelFn?: (ctx: ToolContext) => string;
  /** Set true when the host will supply a tips snippet too. The
   *  default SnippetTips adapter reads the snippet from hostHooks.
   *  Mutually exclusive with `Tips` below. */
  hasTips?: boolean;
  /** Drop in a specific Tips component (e.g. shared DetailsTips)
   *  instead of the snippet adapter. Useful when an override tool
   *  wants to reuse the built-in tool's tips verbatim — PostHost
   *  overrides Details but the gestures (pan / zoom / tile) are
   *  the same. */
  Tips?: Component<{ ctx: ToolContext }>;
  /** Optional gate. Defaults to "always available" — most host
   *  tools are scoped to their host already, so an additional
   *  filter is rarely needed. */
  isAvailable?: (ctx: ToolContext) => boolean;
  /** Pass through ToolDef flags for tools that need them. */
  noCollapse?: boolean;
  supportsCompact?: boolean;
}

export function defineSnippetTool(opts: DefineSnippetToolOpts): ToolDef {
  return {
    id: opts.id,
    label: opts.label,
    labelFn: opts.labelFn,
    order: opts.order,
    Icon: opts.Icon,
    Body: SnippetBody,
    Tips: opts.Tips ?? (opts.hasTips ? SnippetTips : undefined),
    isAvailable: opts.isAvailable ?? (() => true),
    noCollapse: opts.noCollapse,
    supportsCompact: opts.supportsCompact,
  };
}
