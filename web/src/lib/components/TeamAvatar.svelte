<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts" module>
  /** Two initials — the same fallback the profile header uses for a user
   *  with no avatar.
   *
   *  Exported from the module context rather than kept local because
   *  this is the ONE definition for a team: the rail had its own copy
   *  before #982 and a second one would be a second answer to "what does
   *  this team look like when it has no picture". */
  export function teamInitials(name: string): string {
    const parts = name.trim().split(/\s+/).slice(0, 2);
    return parts.map((p) => p.slice(0, 1).toUpperCase()).join('') || '?';
  }
</script>

<script lang="ts">
  /**
   * A team's picture, or its initials tile (#982).
   *
   * # Why one component for three surfaces
   *
   * The rail, the /teams directory card and the team page header all
   * answer the same question — "what does this team look like?" — and
   * before this they all answered it with a different local copy of
   * `initials()`. One component means the fallback ladder is written
   * once, so a team that loses its picture degrades identically
   * everywhere instead of three ways.
   *
   * # The fallback ladder
   *
   *   hero_asset_id present  →  the `col` rendition
   *   absent, or the image fails to load  →  initials
   *
   * The second rung is not paranoia. `hero_asset_id` is a RENDER ANSWER
   * the server re-derives per read, so the common failure — the asset
   * stopped being public — never reaches the client at all: the field is
   * simply absent and this renders initials. What `failed` catches is the
   * narrow race where the field was true when the payload was built and
   * the rendition went away before the browser fetched it. A broken-image
   * glyph in a navigation strip is worse than initials, and it is the
   * kind of thing nobody notices until a screenshot.
   *
   * `alt=""` on purpose: every surface renders the team's NAME beside
   * this, so announcing the picture too would make a screen reader say
   * the team twice. It is decoration next to a label, not content.
   */
  interface TeamLike {
    id: string;
    name: string;
    hero_asset_id?: string | null;
  }

  let {
    team,
    /** Tailwind sizing/shape classes — the surfaces differ (a 7×7 chip
     *  circle vs a 10×10 card square), so the shape is the caller's
     *  decision and only the CONTENT is shared. */
    class: cls = 'h-7 w-7 rounded-full',
    /** Font size for the initials rung, matched to `class` by the caller. */
    textClass = 'text-[0.65rem]',
  }: { team: TeamLike; class?: string; textClass?: string } = $props();

  let failed = $state(false);

  // Reset when the team (or its picture) changes, or a single failure
  // would stick to every team that later reuses this component instance.
  $effect(() => {
    void team.hero_asset_id;
    failed = false;
  });

  const heroUrl = $derived(
    team.hero_asset_id ? `/api/v1/assets/${team.hero_asset_id}/variants/col` : null,
  );
</script>

{#if heroUrl && !failed}
  <img
    src={heroUrl}
    alt=""
    class="{cls} shrink-0 object-cover"
    loading="lazy"
    data-testid="team-hero"
    onerror={() => (failed = true)}
  />
{:else}
  <span
    class="{cls} flex shrink-0 items-center justify-center bg-state-hover {textClass}
           font-semibold text-fg-muted"
    aria-hidden="true"
    data-testid="team-initials">{teamInitials(team.name)}</span
  >
{/if}
