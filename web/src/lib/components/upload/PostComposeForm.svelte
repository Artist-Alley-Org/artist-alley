<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The compose form at the bottom of the upload modal. Title,
  // description, visibility, tags, post-mode toggle, draft toggle,
  // collection prefill, and the "Just upload as assets — no post"
  // escape hatch.
  //
  // All bindings flow into upload.compose, which the store reads at
  // submit() time. State is intentionally lightweight: no validation
  // beyond what the server enforces, and no async dependencies at all.
  //
  // WHAT USED TO BE HERE: a "workflow state" <select>, populated from
  // GET /workflow/states?domain=post, whose options were the raw state
  // rows — "Work in progress" and "Published" — and whose value was a
  // state UUID posted straight through as `state_id`. It rendered
  // (the domain has two states, so its `states.length > 1` guard
  // passed), it asked the artist about a state machine, and picking
  // "Work in progress" did nothing whatsoever, because no read path in
  // the product looked at post state.
  //
  // ADR 0091 decision 7 makes that choice real and makes it one
  // question: publish now, or save a draft. The control is a checkbox
  // beside the submit button rather than a dropdown of state names,
  // and the wire carries a boolean rather than a UUID — so there is no
  // longer any way for this form to point a post at a state in another
  // domain.

  import { upload } from '$stores/upload.svelte';
  import { t } from '$stores/lang.svelte';

  // Tag chip input.
  let tagDraft = $state('');
  function commitTag() {
    const t = tagDraft.trim().toLowerCase();
    if (!t) return;
    if (upload.compose.tags.includes(t)) {
      tagDraft = '';
      return;
    }
    upload.compose.tags = [...upload.compose.tags, t];
    tagDraft = '';
  }
  function removeTag(t: string) {
    upload.compose.tags = upload.compose.tags.filter((x) => x !== t);
  }
  function handleTagKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      commitTag();
    } else if (e.key === 'Backspace' && tagDraft === '' && upload.compose.tags.length > 0) {
      upload.compose.tags = upload.compose.tags.slice(0, -1);
    }
  }
</script>

<div class="space-y-4 rounded-lg border border-border bg-surface-elevated p-4">
  <!-- "Create a post" toggle. When off, the rest of the form
       collapses — the upload still happens, just no post wraps the
       resulting assets.

       It is NOT off-able when the modal was opened from a collection
       (#1161, ADR 0091 decision 3). A collection holds posts, so
       dropping files there without one used to write a membership row
       nothing rendered and now would write nothing at all: either way
       the artist watches an upload succeed into an empty collection.
       The control stays visible and says why rather than vanishing,
       because a control that disappears looks like a bug. -->
  <label class="flex items-center gap-2 text-sm">
    <input
      type="checkbox"
      bind:checked={upload.compose.enabled}
      disabled={upload.postRequired}
      data-testid="upload-compose-enabled"
      class="h-4 w-4 rounded border-border-strong accent-accent disabled:opacity-50"
    />
    <span class="font-medium text-fg">{t('upload.compose.toggle')}</span>
  </label>
  {#if upload.postRequired}
    <p class="-mt-2 text-xs text-fg-muted" data-testid="upload-post-required">
      {t('upload.compose.post_required')}
    </p>
  {/if}

  {#if upload.compose.enabled}
    <!-- Title + description -->
    <div class="space-y-2">
      <input
        type="text"
        bind:value={upload.compose.title}
        placeholder={t('upload.compose.title_placeholder')}
        maxlength="500"
        class="w-full rounded border border-border-strong bg-surface px-3 py-2 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        aria-label={t('upload.compose.title_aria')}
      />
      <textarea
        bind:value={upload.compose.description}
        placeholder={t('upload.compose.description_placeholder')}
        rows="2"
        class="w-full resize-y rounded border border-border-strong bg-surface px-3 py-2 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        aria-label={t('upload.compose.description_aria')}
      ></textarea>
    </div>

    <!-- Visibility + post mode + workflow state — three pickers in a row. -->
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 md:grid-cols-3">
      <!-- Visibility. THE SAME FOUR TIERS AS /create, IN THE SAME ORDER,
           WITH THE SAME LABELS (#1240).

           This select used to offer three — public / followers / private
           — while the store it binds to defaults to `org-only`. A
           <select> bound to a value none of its options carry renders
           BLANK, so the modal showed no tier at all and then posted
           `org-only` anyway: the control disagreed with the request it
           produced, which is the one thing a form must never do.

           `org-only` is first because it is the default, and a default
           that is not offered cannot be shown honestly. The labels come
           from the `create` catalogue rather than a second set here, so
           the two create surfaces cannot drift into describing the same
           tier differently — the same reason UploadModal already reads
           create.open_full_page. -->
      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">{t('upload.compose.visibility_label')}</span>
        <select
          bind:value={upload.compose.visibility}
          data-testid="upload-visibility"
          class="w-full rounded border border-border-strong bg-surface-elevated px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        >
          <option value="org-only">{t('create.vis_org_only')}</option>
          <option value="followers">{t('create.vis_followers')}</option>
          <option value="private">{t('create.vis_private')}</option>
          <option value="public">{t('create.vis_public')}</option>
        </select>
        <span class="mt-1 block text-fg-muted">{t('create.visibility_help')}</span>
      </label>

      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">{t('upload.compose.post_mode_label')}</span>
        <select
          bind:value={upload.compose.mode}
          class="w-full rounded border border-border-strong bg-surface-elevated px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        >
          <option value="one-post">{t('upload.compose.mode_one_post')}</option>
          <option value="one-per-file">{t('upload.compose.mode_one_per_file')}</option>
        </select>
      </label>

      <label class="flex items-start gap-2 text-xs">
        <input
          type="checkbox"
          bind:checked={upload.compose.draft}
          class="mt-0.5 rounded border-border-strong focus-visible:ring-2 focus-visible:ring-ring"
        />
        <span>
          <span class="block text-fg">{t('upload.compose.draft_label')}</span>
          <span class="block text-fg-muted">{t('upload.compose.draft_hint')}</span>
        </span>
      </label>
    </div>

    <!-- Tags -->
    <div>
      <p class="mb-1 text-xs text-fg-muted">{t('upload.compose.tags_label')}</p>
      <div class="flex flex-wrap items-center gap-1.5 rounded border border-border bg-surface-elevated px-2 py-1.5">
        {#each upload.compose.tags as tag (tag)}
          <span class="inline-flex items-center gap-1 rounded-full bg-surface px-2 py-0.5 text-xs text-fg">
            #{tag}
            <button
              type="button"
              onclick={() => removeTag(tag)}
              class="text-fg-muted hover:text-fg"
              aria-label={t('upload.compose.remove_tag_aria', { tag })}
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
                <path d="M18 6 6 18M6 6l12 12" />
              </svg>
            </button>
          </span>
        {/each}
        <input
          type="text"
          bind:value={tagDraft}
          onkeydown={handleTagKeydown}
          onblur={commitTag}
          placeholder={upload.compose.tags.length === 0 ? t('upload.compose.tag_placeholder') : '+'}
          class="min-w-[8rem] flex-1 bg-transparent px-1 py-0.5 text-sm placeholder:text-fg-muted/60 focus:outline-none"
        />
      </div>
    </div>

    {#if upload.compose.collectionId}
      <p class="rounded bg-accent/10 px-3 py-2 text-xs text-accent">
        {t('upload.compose.collection_prefill_note')}
      </p>
    {/if}
  {/if}
</div>
