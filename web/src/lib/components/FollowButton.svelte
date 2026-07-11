<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Reusable Follow/Following/Blocked button (Phase 1.17.G2).
  //
  // Anchored on a target user's ref. The component owns its own
  // relationship state — fetches /users/{ref}/relationship on mount,
  // hits POST/DELETE /users/{ref}/follow on click. The parent does
  // not need to manage anything beyond passing the target ref.
  //
  // Three rendered states:
  //   - is_self          → renders nothing (the viewer is the target)
  //   - is_blocked_by_*  → renders a disabled "Blocked" label,
  //                        no click affordance (matches common social UX)
  //   - is_following     → "Following" pill with hover→"Unfollow"
  //   - default          → "Follow" pill
  //
  // The relationship round-trip is cheap (~ms via the back-end
  // cache) so we re-fetch on click rather than mutating local state
  // optimistically; that keeps the rendered state in sync if a
  // server-side cache invalidation fires for some other reason
  // (e.g. the target blocked the viewer mid-render).
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface Props {
    /** Target user's ref. The viewer's own ref is inferred from
        the auth store; passing the same value renders nothing. */
    targetRef: number;
    /** Optional callback after a successful follow/unfollow. Lets the
        parent (e.g. profile page) refresh follower counts. */
    onchange?: (now_following: boolean) => void;
    /** Compact mode for tight headers (smaller padding, no icon). */
    compact?: boolean;
  }

  let { targetRef, onchange, compact = false }: Props = $props();

  interface Relationship {
    is_self: boolean;
    is_following: boolean;
    is_followed_by: boolean;
    is_blocked_by_me: boolean;
    is_blocked_by_them: boolean;
  }

  let rel = $state<Relationship | null>(null);
  let pending = $state(false);
  let hovering = $state(false);
  let error = $state<string | null>(null);

  onMount(() => {
    void load();
  });

  // Re-load when targetRef changes (e.g. PostHost cycles through a
  // playlist of posts by different authors).
  $effect(() => {
    void targetRef;
    void load();
  });

  async function load(): Promise<void> {
    if (!targetRef) return;
    try {
      const r = await api.GET('/users/{ref}/relationship', {
        params: { path: { ref: targetRef } },
      });
      if (r.data) rel = r.data as Relationship;
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to load';
    }
  }

  async function toggle(): Promise<void> {
    if (!rel || pending || rel.is_self) return;
    if (rel.is_blocked_by_me || rel.is_blocked_by_them) return;
    pending = true;
    error = null;
    try {
      if (rel.is_following) {
        const r = await api.DELETE('/users/{ref}/follow', {
          params: { path: { ref: targetRef } },
        });
        if (r.error) throw new Error(t('social.unfollow_failed'));
        await load();
        onchange?.(false);
      } else {
        const r = await api.POST('/users/{ref}/follow', {
          params: { path: { ref: targetRef } },
        });
        if (r.error) throw new Error(t('social.follow_failed'));
        await load();
        onchange?.(true);
      }
    } catch (e) {
      error = e instanceof Error ? e.message : 'Action failed';
    } finally {
      pending = false;
    }
  }

  const sizeClass = $derived(compact ? 'px-2 py-0.5 text-xs' : 'px-3 py-1 text-sm');

  // The visible label depends on state AND hover — "Following" flips
  // to "Unfollow" while the cursor is over it, so the user knows the
  // click is the destructive action.
  const label = $derived(
    rel?.is_blocked_by_me
      ? t('social.blocked_by_me')
      : rel?.is_blocked_by_them
        ? t('social.blocked_by_them')
        : rel?.is_following
          ? hovering
            ? t('social.unfollow')
            : t('social.following')
          : t('social.follow'),
  );

  const variantClass = $derived(
    rel?.is_blocked_by_me || rel?.is_blocked_by_them
      ? 'cursor-not-allowed border-fg-muted/40 bg-surface text-fg-muted'
      : rel?.is_following
        ? hovering
          ? 'border-danger/40 bg-danger/10 text-danger'
          : 'border-border bg-surface text-fg'
        : 'border-accent bg-accent text-on-accent hover:bg-accent/90',
  );
</script>

{#if rel && !rel.is_self}
  <button
    type="button"
    class={`inline-flex items-center gap-1.5 rounded-full border font-medium transition-colors ${sizeClass} ${variantClass}`}
    onclick={toggle}
    onmouseenter={() => (hovering = true)}
    onmouseleave={() => (hovering = false)}
    disabled={pending || rel.is_blocked_by_me || rel.is_blocked_by_them}
    aria-pressed={rel.is_following}
    title={error ?? undefined}
  >
    {label}
  </button>
{/if}
