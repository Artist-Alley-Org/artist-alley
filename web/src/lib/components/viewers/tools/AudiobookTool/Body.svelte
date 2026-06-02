<script lang="ts">
  // Audiobookshelf-style side panel for the audiobook reader.
  // Binds the same AudiobookSession the canvas-area AudiobookView
  // binds; both ends mutate the same $state object so flipping a
  // chapter / speed / sleep timer in the panel updates the player
  // without an event bus.
  //
  // Sections (top → bottom):
  //   1. Now playing — cover thumb + title + author + position
  //   2. Chapters    — full chapter list with click-to-jump
  //   3. Playback    — speed slider + auto-rewind + skip-back/fwd
  //   4. Sleep timer — Off / 5 / 15 / 30 / 45 / 60 / end-of-chapter
  //   5. Bookmarks   — time-anchored markers with notes
  //   6. Stats       — duration / time listened / remaining at speed
  //                    / finish ETA at current pace
  //   7. Annotations · coming soon — placeholder for the future
  //                    text-anchored audiobook annotation phase

  import type { ToolContext } from '../contract';
  import type { SleepTimerMode } from '$lib/audiobook/session.svelte';
  import { fmtClock, fmtSpan } from '$lib/audiobook/session.svelte';

  let { ctx }: { ctx: ToolContext } = $props();
  const session = $derived(ctx.audiobookSession);

  const SLEEP_OPTIONS: { id: SleepTimerMode; label: string }[] = [
    { id: 'off',             label: 'Off' },
    { id: '5min',            label: '5 min' },
    { id: '15min',           label: '15 min' },
    { id: '30min',           label: '30 min' },
    { id: '45min',           label: '45 min' },
    { id: '60min',           label: '60 min' },
    { id: 'end-of-chapter',  label: 'End of chapter' },
  ];

  let bookmarkNote = $state('');
  function addBookmark() {
    if (!session) return;
    session.addBookmark(bookmarkNote);
    bookmarkNote = '';
  }

  // Editing a bookmark note inline.
  let editingBmKey = $state<string | null>(null);
  let editingBmDraft = $state('');
  function bmKey(time: number, createdAt: string) { return `${time}:${createdAt}`; }
  function beginEditBm(time: number, createdAt: string, note: string) {
    editingBmKey = bmKey(time, createdAt);
    editingBmDraft = note;
  }
  function saveBm(time: number, createdAt: string) {
    if (!session) return;
    session.setCurrentBookmarkNote(time, createdAt, editingBmDraft.trim());
    editingBmKey = null;
  }

  // Current chapter index — duplicates the view's $derived since
  // panel + view both want it; cheap enough.
  const currentChapterIdx = $derived.by<number>(() => {
    if (!session) return -1;
    const t = session.currentTime;
    for (let i = 0; i < session.chapters.length; i++) {
      const c = session.chapters[i];
      if (t >= c.start && t < c.end) return i;
    }
    return session.chapters.length - 1;
  });

  function formatDate(iso: string): string {
    try { return new Date(iso).toLocaleDateString(); } catch { return iso; }
  }

  // Stats — remaining at speed + ETA wall-clock.
  const remainingAtSpeed = $derived(
    !session ? 0 :
    Math.max(0, (session.durationS - session.currentTime) / Math.max(0.1, session.speed)),
  );
  const finishEta = $derived.by<string>(() => {
    if (!session || remainingAtSpeed <= 0) return '—';
    const at = new Date(Date.now() + remainingAtSpeed * 1000);
    return at.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
  });
</script>

{#if !session}
  <div class="p-4 text-sm text-fg-muted"><p>Audiobook is loading…</p></div>
{:else}
  <div class="flex flex-col">

    <!-- ── 1. Now playing ────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <div class="flex gap-2">
        {#if session.coverUrl}
          <img src={session.coverUrl} alt="cover" class="h-16 w-16 shrink-0 rounded border border-border object-cover" />
        {:else}
          <div class="flex h-16 w-16 shrink-0 items-center justify-center rounded border border-border bg-surface-elevated text-fg-muted/60">
            <svg xmlns="http://www.w3.org/2000/svg" width="28" height="28" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M2 3h6a4 4 0 0 1 4 4v14a3 3 0 0 0-3-3H2z"/><path d="M22 3h-6a4 4 0 0 0-4 4v14a3 3 0 0 1 3-3h7z"/></svg>
          </div>
        {/if}
        <div class="min-w-0 flex-1 text-[11px]">
          <div class="truncate font-medium text-fg">{session.title || ctx.asset.title || 'Untitled'}</div>
          {#if session.author}<div class="truncate text-fg-muted">by {session.author}</div>{/if}
          {#if session.narrator}<div class="truncate text-fg-muted">narrated by {session.narrator}</div>{/if}
          <div class="mt-1 flex items-center gap-1.5 font-mono text-fg-muted/80">
            <span>{fmtClock(session.currentTime)}</span>
            <span class="text-fg-muted/40">/</span>
            <span>{fmtClock(session.durationS)}</span>
            {#if session.speed !== 1}
              <span class="ml-auto rounded bg-accent/20 px-1 text-[10px] text-fg">{session.speed.toFixed(2)}×</span>
            {/if}
          </div>
        </div>
      </div>
    </section>

    <!-- ── 2. Chapters ───────────────────────────────────────── -->
    {#if session.chapters.length > 0}
      <section class="border-b border-border p-3 text-xs">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">Chapters</h3>
          <span class="font-mono text-[10px] text-fg-muted">
            {currentChapterIdx >= 0 ? currentChapterIdx + 1 : '–'} / {session.chapters.length}
          </span>
        </div>
        <div class="max-h-72 space-y-0.5 overflow-y-auto pr-1">
          {#each session.chapters as c, i (i)}
            <button
              type="button"
              onclick={() => session.goToChapter?.(i)}
              class={`flex w-full items-center justify-between gap-2 rounded px-2 py-1 text-left text-[10px] ${i === currentChapterIdx ? 'bg-accent/20 text-accent' : 'text-fg hover:bg-surface-elevated'}`}
              title={c.title || `Chapter ${i + 1}`}
            >
              <span class="truncate">
                <span class="font-mono text-fg-muted/70">{String(i + 1).padStart(2, '0')}</span>
                <span class="ml-2">{c.title || `Chapter ${i + 1}`}</span>
              </span>
              <span class="shrink-0 font-mono text-fg-muted/60">{fmtClock(c.start)}</span>
            </button>
          {/each}
        </div>
      </section>
    {/if}

    <!-- ── 3. Playback ───────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Playback</h3>
      <label class="mb-2 block">
        <span class="mb-1 flex items-center justify-between text-fg-muted">
          <span>Speed</span>
          <span class="font-mono text-fg">{session.speed.toFixed(2)}×</span>
        </span>
        <input
          type="range" min="0.5" max="4" step="0.05"
          value={session.speed}
          oninput={(e) => session.setSpeed(+(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
        />
      </label>
      <label class="mb-2 block">
        <span class="mb-1 flex items-center justify-between text-fg-muted">
          <span>Auto-rewind on resume</span>
          <span class="font-mono text-fg">{session.autoRewindS}s</span>
        </span>
        <input
          type="range" min="0" max="30" step="1"
          value={session.autoRewindS}
          oninput={(e) => session.setAutoRewindS(+(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
        />
      </label>
      <div class="grid grid-cols-2 gap-2">
        <label class="block">
          <span class="mb-1 flex items-center justify-between text-fg-muted">
            <span>Skip back</span>
            <span class="font-mono text-fg">{session.skipBackS}s</span>
          </span>
          <input
            type="range" min="5" max="60" step="5"
            value={session.skipBackS}
            oninput={(e) => session.setSkipBackS(+(e.currentTarget as HTMLInputElement).value)}
            class="w-full accent-accent"
          />
        </label>
        <label class="block">
          <span class="mb-1 flex items-center justify-between text-fg-muted">
            <span>Skip fwd</span>
            <span class="font-mono text-fg">{session.skipFwdS}s</span>
          </span>
          <input
            type="range" min="5" max="120" step="5"
            value={session.skipFwdS}
            oninput={(e) => session.setSkipFwdS(+(e.currentTarget as HTMLInputElement).value)}
            class="w-full accent-accent"
          />
        </label>
      </div>
    </section>

    <!-- ── 4. Sleep timer ────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Sleep timer</h3>
      <div class="grid grid-cols-4 gap-1">
        {#each SLEEP_OPTIONS as opt (opt.id)}
          <button
            type="button"
            onclick={() => session.setSleepTimer(opt.id)}
            class={`col-span-${opt.id === 'end-of-chapter' ? '4' : '1'} rounded border px-1.5 py-1 text-[10px] ${session.sleepTimer === opt.id ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
          >{opt.label}</button>
        {/each}
      </div>
      {#if session.sleepTimer !== 'off'}
        <p class="mt-2 rounded border border-yellow-500/30 bg-yellow-500/10 px-2 py-1 text-[10px] text-fg">
          {#if session.sleepRemaining != null}
            Pauses in <span class="font-mono">{fmtClock(session.sleepRemaining)}</span>.
          {:else}
            Pauses at end of current chapter.
          {/if}
        </p>
      {/if}
    </section>

    <!-- ── 5. Bookmarks ──────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Bookmarks</h3>
      <div class="mb-2 flex gap-1">
        <input
          type="text"
          bind:value={bookmarkNote}
          placeholder="Optional note…"
          onkeydown={(e) => { if (e.key === 'Enter') addBookmark(); }}
          class="flex-1 rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg focus:border-accent focus:outline-none"
        />
        <button
          type="button"
          onclick={addBookmark}
          class="rounded border border-accent bg-accent/15 px-2 py-1 text-[10px] font-medium text-fg hover:bg-accent/25"
          title={`Bookmark at ${fmtClock(session.currentTime)}`}
        >+</button>
      </div>
      {#if session.bookmarks.length === 0}
        <p class="rounded border border-border bg-surface/60 px-2 py-1 text-[10px] leading-snug text-fg-muted">
          No bookmarks yet. Press + to save the current position (optionally with a note).
        </p>
      {:else}
        <div class="space-y-0.5">
          {#each session.bookmarks as bm (bmKey(bm.time, bm.createdAt))}
            <div class="group rounded border border-border bg-surface px-2 py-1">
              <div class="flex items-start gap-1">
                <button
                  type="button"
                  onclick={() => session.seekTo?.(bm.time)}
                  class="flex-1 text-left text-[10px]"
                >
                  <div class="flex items-center justify-between">
                    <span class="font-mono text-fg">{fmtClock(bm.time)}</span>
                    <span class="ml-2 shrink-0 text-fg-muted/70">{formatDate(bm.createdAt)}</span>
                  </div>
                  {#if bm.note && editingBmKey !== bmKey(bm.time, bm.createdAt)}
                    <div class="mt-0.5 text-fg-muted">{bm.note}</div>
                  {/if}
                </button>
                <button
                  type="button"
                  onclick={() => beginEditBm(bm.time, bm.createdAt, bm.note)}
                  class="text-fg-muted opacity-0 hover:text-fg group-hover:opacity-100"
                  aria-label="Edit note"
                  title="Edit note"
                >✎</button>
                <button
                  type="button"
                  onclick={() => session.removeBookmark(bm.time, bm.createdAt)}
                  class="text-fg-muted opacity-0 hover:text-danger group-hover:opacity-100"
                  aria-label="Remove bookmark"
                  title="Remove bookmark"
                >×</button>
              </div>
              {#if editingBmKey === bmKey(bm.time, bm.createdAt)}
                <div class="mt-1 flex gap-1">
                  <input
                    type="text"
                    bind:value={editingBmDraft}
                    onkeydown={(e) => {
                      if (e.key === 'Enter') saveBm(bm.time, bm.createdAt);
                      else if (e.key === 'Escape') { editingBmKey = null; }
                    }}
                    autofocus
                    class="flex-1 rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg focus:border-accent focus:outline-none"
                  />
                  <button
                    type="button"
                    onclick={() => saveBm(bm.time, bm.createdAt)}
                    class="rounded border border-accent bg-accent/15 px-2 py-0.5 text-[10px] font-medium text-fg"
                  >Save</button>
                </div>
              {/if}
            </div>
          {/each}
        </div>
      {/if}
    </section>

    <!-- ── 6. Stats ──────────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Stats</h3>
      <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-[10px]">
        <dt class="text-fg-muted">Duration</dt>
        <dd class="font-mono text-fg">{fmtSpan(session.durationS)}</dd>
        <dt class="text-fg-muted">Listened</dt>
        <dd class="font-mono text-fg">{fmtSpan(session.currentTime)}</dd>
        <dt class="text-fg-muted">Remaining</dt>
        <dd class="font-mono text-fg">{fmtSpan(Math.max(0, session.durationS - session.currentTime))}</dd>
        <dt class="text-fg-muted">At {session.speed.toFixed(2)}×</dt>
        <dd class="font-mono text-fg">{fmtSpan(remainingAtSpeed)}</dd>
        <dt class="text-fg-muted">Finish ETA</dt>
        <dd class="font-mono text-fg">{finishEta}</dd>
        <dt class="text-fg-muted">Chapters</dt>
        <dd class="font-mono text-fg">{session.chapters.length}</dd>
      </dl>
    </section>

    <!-- ── 7. Annotations · coming soon ──────────────────────── -->
    <section class="p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Annotations · coming soon</h3>
      <p class="text-[10px] leading-snug text-fg-muted">
        A future phase will let you anchor a comment to a specific
        moment ("3:42 — narrator emphasis here is wild"); the data
        model is the same generic annotations table the doc viewer
        uses, just with a time anchor instead of a text range.
      </p>
    </section>
  </div>
{/if}
