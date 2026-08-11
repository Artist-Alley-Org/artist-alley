<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Follow / Following pill for a team (#577).
  //
  // Shaped after FollowButton (the user one) so the two read as one
  // gesture: same pill, same hover→"Unfollow" flip, same tokens. Two
  // deliberate differences:
  //
  //   1. State comes from the shared `channels` store rather than from
  //      a per-component fetch. Following here has to move the rail on
  //      browse, and it does, because they read the same $state.
  //   2. It is OPTIMISTIC. A user relationship has states the client
  //      cannot predict (the target may have blocked you), so that
  //      button re-fetches. A team follow is a bookmark with two
  //      states and one writer, so waiting for a round trip before the
  //      pill moves is just lag. The store reverts on failure.
  //
  // Renders nothing for a signed-out visitor: /teams is members-only
  // this sprint (teams.read is Base-gated and anonymous holds nothing),
  // so there is no follow to offer.
  import { channels, type Channel } from '$stores/channels.svelte';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';

  interface Props {
    team: Channel;
    /** Compact mode for directory cards (smaller padding). */
    compact?: boolean;
  }
  let { team, compact = false }: Props = $props();

  let hovering = $state(false);

  const following = $derived(channels.isFollowing(team.id));
  const pending = $derived(channels.isPending(team.id));

  const sizeClass = $derived(compact ? 'px-2 py-0.5 text-xs' : 'px-3 py-1 text-sm');

  // "Following" flips to "Unfollow" under the cursor, so the click's
  // destructive meaning is visible before it happens.
  const label = $derived(
    following ? (hovering ? t('channels.unfollow') : t('channels.following')) : t('channels.follow'),
  );

  const variantClass = $derived(
    following
      ? hovering
        ? 'border-danger/40 bg-danger/10 text-danger'
        : 'border-secondary/40 bg-secondary-container text-on-secondary-container'
      : 'border-accent bg-accent text-on-accent hover:bg-accent/90',
  );
</script>

{#if auth.user}
  <button
    type="button"
    data-testid="team-follow-button"
    class={`inline-flex items-center gap-1.5 rounded-full border font-medium transition-colors disabled:opacity-60 ${sizeClass} ${variantClass}`}
    onclick={() => void channels.toggle(team)}
    onmouseenter={() => (hovering = true)}
    onmouseleave={() => (hovering = false)}
    disabled={pending}
    aria-pressed={following}
  >
    {label}
  </button>
{/if}
