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
  /** Set true when the host will supply a tips snippet too. When
   *  false we don't register a Tips component so the shell footer
   *  collapses for this tool. */
  hasTips?: boolean;
  /** Optional gate. Defaults to "always available" — most host
   *  tools are scoped to their host already, so an additional
   *  filter is rarely needed. */
  isAvailable?: (ctx: ToolContext) => boolean;
}

export function defineSnippetTool(opts: DefineSnippetToolOpts): ToolDef {
  return {
    id: opts.id,
    label: opts.label,
    labelFn: opts.labelFn,
    order: opts.order,
    Icon: opts.Icon,
    Body: SnippetBody,
    Tips: opts.hasTips ? SnippetTips : undefined,
    isAvailable: opts.isAvailable ?? (() => true),
  };
}
