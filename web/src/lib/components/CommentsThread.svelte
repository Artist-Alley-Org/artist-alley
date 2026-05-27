<script lang="ts">
  // Comment thread for the post modal. Loads /posts/{id}/comments,
  // groups by root_id, renders replies indented by depth, exposes a
  // composer at the bottom + per-comment Reply that opens an inline
  // composer scoped to that root.
  //
  // Author display resolves /users/{ref} per unique author seen.
  // Lookups hit the user.profile cache (Phase 1.13.D-4 caching) so
  // repeat authors are warm.
  //
  // Deletion: authors can delete their own (button shows when
  // currentUserRef matches author_user_ref). Moderator override
  // exists server-side via comments.delete.any but isn't surfaced
  // here yet — the response just succeeds when the API allows it.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';

  interface Comment {
    id: string;
    target_kind: 'post' | 'asset' | 'collection';
    target_id: string;
    parent_id?: string | null;
    root_id: string;
    depth: number;
    author_user_ref: number;
    body: string;
    body_html: string;
    like_count: number;
    edited_at?: string | null;
    created_at: string;
    updated_at: string;
  }

  interface UserPublic {
    ref: number;
    username: string;
    display_name: string;
    avatar_url?: string | null;
  }

  interface Props {
    postId: string;
  }

  let { postId }: Props = $props();

  let items = $state<Comment[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);
  let authors = $state<Map<number, UserPublic | null>>(new Map());

  // Top-level composer state.
  let newBody = $state('');
  let posting = $state(false);

  // Per-root inline-reply composer: maps rootId → draft + reply parent id.
  // Replies open under the specific comment clicked; the parent_id is
  // that comment's id, but it's nested inside its root group.
  let replyTargets = $state<Map<string, string>>(new Map()); // rootId -> parentId
  let replyDrafts = $state<Map<string, string>>(new Map());

  // ---- Data fetch ---------------------------------------------------------

  onMount(() => {
    void loadThread();
  });

  async function loadThread() {
    loading = true;
    error = null;
    try {
      const { data, error: apiErr } = await api.GET('/posts/{id}/comments', {
        params: { path: { id: postId } },
      });
      if (apiErr || !data) {
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? 'Failed to load comments',
        );
      }
      items = (data.items ?? []) as Comment[];
      void resolveAuthors(items);
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to load comments';
    } finally {
      loading = false;
    }
  }

  async function resolveAuthor(ref: number) {
    if (authors.has(ref)) return;
    // Mark as pending so concurrent fetches don't duplicate.
    authors.set(ref, null);
    authors = new Map(authors);
    try {
      const { data } = await api.GET('/users/{ref}', {
        params: { path: { ref } },
      });
      if (data) {
        authors.set(ref, data as UserPublic);
        authors = new Map(authors);
      }
    } catch {
      // soft-fail: keep as null, we'll render the ref number as placeholder
    }
  }

  async function resolveAuthors(comments: Comment[]) {
    const refs = new Set<number>();
    for (const c of comments) refs.add(c.author_user_ref);
    await Promise.all(Array.from(refs).map((r) => resolveAuthor(r)));
  }

  // ---- Threading view -----------------------------------------------------

  interface ThreadGroup {
    root: Comment;
    replies: Comment[];
  }

  const grouped = $derived.by<ThreadGroup[]>(() => {
    const byRoot = new Map<string, ThreadGroup>();
    for (const c of items) {
      if (c.id === c.root_id) {
        // Root comment.
        const existing = byRoot.get(c.root_id);
        if (existing) {
          existing.root = c;
        } else {
          byRoot.set(c.root_id, { root: c, replies: [] });
        }
      } else {
        const g = byRoot.get(c.root_id);
        if (g) g.replies.push(c);
        else byRoot.set(c.root_id, { root: c, replies: [c] }); // defensive
      }
    }
    return Array.from(byRoot.values()).filter((g) => g.root && g.root.id === g.root.root_id);
  });

  // ---- Composer actions ---------------------------------------------------

  async function postComment(body: string, parentId?: string): Promise<Comment | null> {
    const trimmed = body.trim();
    if (!trimmed) return null;
    posting = true;
    try {
      const { data, error: apiErr } = await api.POST('/posts/{id}/comments', {
        params: { path: { id: postId } },
        body: { body: trimmed, parent_id: parentId ?? null },
      });
      if (apiErr || !data) {
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? 'Failed to post',
        );
      }
      const created = data as Comment;
      // Resolve author (likely already cached — self-author is the
      // current user, whose UserPublic was fetched by the sidebar).
      void resolveAuthor(created.author_user_ref);
      items = [...items, created];
      return created;
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to post';
      return null;
    } finally {
      posting = false;
    }
  }

  async function submitTopLevel() {
    const c = await postComment(newBody);
    if (c) newBody = '';
  }

  function openReply(rootId: string, parentId: string) {
    replyTargets.set(rootId, parentId);
    replyTargets = new Map(replyTargets);
    if (!replyDrafts.has(rootId)) {
      replyDrafts.set(rootId, '');
      replyDrafts = new Map(replyDrafts);
    }
  }

  function cancelReply(rootId: string) {
    replyTargets.delete(rootId);
    replyDrafts.delete(rootId);
    replyTargets = new Map(replyTargets);
    replyDrafts = new Map(replyDrafts);
  }

  async function submitReply(rootId: string) {
    const parentId = replyTargets.get(rootId);
    const draft = replyDrafts.get(rootId) ?? '';
    if (!parentId) return;
    const c = await postComment(draft, parentId);
    if (c) {
      cancelReply(rootId);
    }
  }

  async function deleteComment(id: string) {
    try {
      const { error: apiErr } = await api.DELETE('/comments/{id}', {
        params: { path: { id } },
      });
      if (apiErr) {
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? 'Failed to delete',
        );
      }
      items = items.filter((c) => c.id !== id);
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to delete';
    }
  }

  // ---- Formatters ---------------------------------------------------------

  function relativeTime(iso: string): string {
    const d = new Date(iso).getTime();
    const now = Date.now();
    const sec = Math.round((now - d) / 1000);
    if (sec < 60) return 'just now';
    const min = Math.round(sec / 60);
    if (min < 60) return `${min}m`;
    const hr = Math.round(min / 60);
    if (hr < 24) return `${hr}h`;
    const day = Math.round(hr / 24);
    if (day < 30) return `${day}d`;
    return `${Math.round(day / 30)}mo`;
  }

  function initials(name: string): string {
    const parts = name.trim().split(/\s+/).slice(0, 2);
    return parts.map((p) => p[0]?.toUpperCase() ?? '').join('') || '?';
  }

  function displayName(ref: number): string {
    const u = authors.get(ref);
    return u?.display_name ?? `user ${ref}`;
  }
</script>

<div class="space-y-3 text-sm">
  <!-- Top-level composer. Disabled while submitting. -->
  <form
    onsubmit={(e) => { e.preventDefault(); void submitTopLevel(); }}
    class="space-y-2"
  >
    <textarea
      bind:value={newBody}
      placeholder="Add a comment…"
      rows="2"
      maxlength="10000"
      disabled={posting}
      class="w-full resize-y rounded-md border border-border bg-surface px-3 py-2 text-fg placeholder:text-fg-muted/70 focus-visible:border-border-strong focus:outline-none disabled:opacity-50"
    ></textarea>
    <div class="flex justify-end">
      <button
        type="submit"
        disabled={posting || !newBody.trim()}
        class="rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-white transition-colors hover:bg-accent/90 disabled:cursor-not-allowed disabled:opacity-50"
      >
        {posting ? 'Posting…' : 'Comment'}
      </button>
    </div>
  </form>

  {#if error}
    <p role="alert" class="rounded-md border border-danger/40 bg-danger-container px-3 py-2 text-xs text-danger">
      {error}
    </p>
  {/if}

  {#if loading}
    <div class="space-y-3">
      {#each Array(3) as _, i (i)}
        <div class="flex gap-2">
          <div class="h-8 w-8 shrink-0 animate-pulse rounded-full bg-border"></div>
          <div class="flex-1 space-y-1">
            <div class="h-3 w-1/3 animate-pulse rounded bg-border"></div>
            <div class="h-3 w-5/6 animate-pulse rounded bg-border"></div>
          </div>
        </div>
      {/each}
    </div>
  {:else if grouped.length === 0}
    <p class="py-4 text-center text-xs text-fg-muted">
      No comments yet — be the first.
    </p>
  {:else}
    {#each grouped as group (group.root.id)}
      {@const root = group.root}
      {@const author = authors.get(root.author_user_ref)}
      <div class="space-y-2 border-t border-border pt-3">
        <!-- Root comment row. -->

        <div class="flex gap-2">
          <div class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-accent/20 text-[10px] font-semibold text-accent">
            {#if author?.avatar_url}
              <img src={author.avatar_url} alt="" class="h-full w-full rounded-full object-cover" />
            {:else}
              {initials(displayName(root.author_user_ref))}
            {/if}
          </div>
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-1.5 text-xs">
              <span class="font-medium text-fg">{displayName(root.author_user_ref)}</span>
              <span class="text-fg-muted" title={new Date(root.created_at).toLocaleString()}>
                · {relativeTime(root.created_at)}
              </span>
              {#if root.edited_at}
                <span class="text-fg-muted" title="Edited {new Date(root.edited_at).toLocaleString()}">(edited)</span>
              {/if}
            </div>
            <p class="mt-0.5 whitespace-pre-wrap text-fg">{root.body}</p>
            <div class="mt-1 flex items-center gap-3 text-xs text-fg-muted">
              <button
                type="button"
                class="hover:text-fg"
                onclick={() => openReply(group.root.id, root.id)}
              >
                Reply
              </button>
              {#if auth.user?.ref === root.author_user_ref}
                <button
                  type="button"
                  class="hover:text-danger"
                  onclick={() => deleteComment(root.id)}
                >
                  Delete
                </button>
              {/if}
            </div>
          </div>
        </div>

        <!-- Replies — indented but not infinitely. Cap indent at depth
             4 so deep threads stay readable. -->
        {#each group.replies as reply (reply.id)}
          {@const replyAuthor = authors.get(reply.author_user_ref)}
          {@const indent = Math.min(reply.depth, 4) * 16}
          <div class="flex gap-2" style="margin-left: {indent}px">
            <div class="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-accent/20 text-[10px] font-semibold text-accent">
              {#if replyAuthor?.avatar_url}
                <img src={replyAuthor.avatar_url} alt="" class="h-full w-full rounded-full object-cover" />
              {:else}
                {initials(displayName(reply.author_user_ref))}
              {/if}
            </div>
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-1.5 text-xs">
                <span class="font-medium text-fg">{displayName(reply.author_user_ref)}</span>
                <span class="text-fg-muted" title={new Date(reply.created_at).toLocaleString()}>
                  · {relativeTime(reply.created_at)}
                </span>
                {#if reply.edited_at}
                  <span class="text-fg-muted">(edited)</span>
                {/if}
              </div>
              <p class="mt-0.5 whitespace-pre-wrap text-fg">{reply.body}</p>
              <div class="mt-1 flex items-center gap-3 text-xs text-fg-muted">
                <button
                  type="button"
                  class="hover:text-fg"
                  onclick={() => openReply(group.root.id, reply.id)}
                >
                  Reply
                </button>
                {#if auth.user?.ref === reply.author_user_ref}
                  <button
                    type="button"
                    class="hover:text-danger"
                    onclick={() => deleteComment(reply.id)}
                  >
                    Delete
                  </button>
                {/if}
              </div>
            </div>
          </div>
        {/each}

        <!-- Inline reply composer for this root, if open. -->
        {#if replyTargets.has(group.root.id)}
          <form
            onsubmit={(e) => { e.preventDefault(); void submitReply(group.root.id); }}
            class="space-y-2 pl-10"
          >
            <textarea
              bind:value={() => replyDrafts.get(group.root.id) ?? '', (v) => {
                replyDrafts.set(group.root.id, v);
                replyDrafts = new Map(replyDrafts);
              }}
              placeholder="Reply…"
              rows="2"
              maxlength="10000"
              disabled={posting}
              class="w-full resize-y rounded-md border border-border bg-surface px-3 py-2 text-fg placeholder:text-fg-muted/70 focus-visible:border-border-strong focus:outline-none disabled:opacity-50"
            ></textarea>
            <div class="flex justify-end gap-2">
              <button
                type="button"
                onclick={() => cancelReply(group.root.id)}
                disabled={posting}
                class="rounded-md border border-border px-2.5 py-1 text-xs text-fg-muted hover:border-fg-muted/60 hover:text-fg disabled:opacity-50"
              >
                Cancel
              </button>
              <button
                type="submit"
                disabled={posting || !(replyDrafts.get(group.root.id)?.trim())}
                class="rounded-md bg-accent px-2.5 py-1 text-xs font-medium text-white hover:bg-accent/90 disabled:cursor-not-allowed disabled:opacity-50"
              >
                {posting ? 'Posting…' : 'Reply'}
              </button>
            </div>
          </form>
        {/if}
      </div>
    {/each}
  {/if}
</div>
