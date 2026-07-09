<script lang="ts">
  // Cover-thumbnail picker. Three modes, two shipped now, one
  // deferred:
  //   (a) Use one of the post's member assets as the cover. Default.
  //       The user picks which member by clicking a tile.
  //   (b) Crop a region of a member asset → store as new asset → use
  //       that as the cover. DEFERRED — disabled with a "coming
  //       soon" label.
  //   (c) Upload a separate image as the cover. The new asset is
  //       created independently and assigned to
  //       post.cover_thumbnail_asset_id; it is NOT a member of the
  //       post.

  import { upload } from '$stores/upload.svelte';
  import { t } from '$stores/lang.svelte';

  const memberRows = $derived(upload.readyRows);

  // Hidden file input for option (c) — we trigger it programmatically
  // from the "Upload custom cover" button.
  let coverInputEl: HTMLInputElement | undefined = $state();

  // The standalone-cover upload uses the same XHR path as a member
  // upload, but the resulting asset is recorded as the cover thumb
  // without being added to the post's members list.
  let coverFileName = $state<string | null>(null);
  let coverState = $state<'idle' | 'uploading' | 'ready' | 'errored'>('idle');
  let coverProgress = $state(0);
  let coverError = $state<string | null>(null);

  async function handleCoverFile(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    const f = input.files?.[0];
    if (!f) return;
    coverFileName = f.name;
    coverState = 'uploading';
    coverError = null;
    try {
      const hash = await uploadStandaloneCover(f);
      const ext = (() => {
        const dot = f.name.lastIndexOf('.');
        return dot > 0 ? f.name.slice(dot + 1).toLowerCase() : undefined;
      })();
      // Create a hidden asset for the cover. Title falls back to
      // the filename minus extension so the asset detail page has
      // something legible.
      const titleBase = (() => {
        const dot = f.name.lastIndexOf('.');
        return (dot > 0 ? f.name.slice(0, dot) : f.name).replace(/[._-]+/g, ' ').trim() || f.name;
      })();
      const { data, error } = await import('$api/client').then((m) =>
        m.api.POST('/assets', {
          body: {
            title: titleBase,
            asset_type: 1,
            status: 'draft',
            file_hash: hash,
            file_extension: ext,
          },
        }),
      );
      if (error || !data) {
        throw new Error(t('upload.thumbnail.create_cover_error'));
      }
      upload.compose.thumbSeparateAssetId = data.id;
      upload.compose.thumbMode = 'separate';
      coverState = 'ready';
    } catch (e) {
      coverState = 'errored';
      coverError = e instanceof Error ? e.message : t('upload.thumbnail.upload_error');
    } finally {
      // Reset the input so picking the same file again triggers change.
      input.value = '';
    }
  }

  function uploadStandaloneCover(f: File): Promise<string> {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open('POST', '/api/v1/storage/objects', true);
      xhr.setRequestHeader('Content-Type', 'application/octet-stream');
      xhr.setRequestHeader('X-Content-Type', f.type || 'application/octet-stream');
      xhr.responseType = 'json';
      xhr.withCredentials = true;
      xhr.upload.addEventListener('progress', (e) => {
        if (e.lengthComputable) coverProgress = e.loaded / e.total;
      });
      xhr.addEventListener('load', () => {
        if (xhr.status >= 200 && xhr.status < 300 && xhr.response) {
          coverProgress = 1;
          resolve(xhr.response.hash as string);
        } else {
          const err = (xhr.response && (xhr.response as { error?: string }).error) || `HTTP ${xhr.status}`;
          reject(new Error(err));
        }
      });
      xhr.addEventListener('error', () => reject(new Error(t('upload.thumbnail.network_error'))));
      xhr.send(f);
    });
  }

  function pickMember(rowId: string) {
    upload.compose.thumbMode = 'member';
    upload.compose.thumbMemberRowId = rowId;
  }
</script>

<div class="space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
  <p class="text-sm font-medium text-fg">{t('upload.thumbnail.heading')}</p>

  <!-- Mode selector -->
  <div class="flex flex-wrap gap-2">
    <button
      type="button"
      onclick={() => (upload.compose.thumbMode = 'member')}
      class={`rounded-md border px-3 py-1.5 text-xs transition-colors ${
        upload.compose.thumbMode === 'member'
          ? 'border-accent bg-accent/10 text-accent'
          : 'border-border bg-surface-elevated text-fg-muted hover:text-fg'
      }`}
    >
      {t('upload.thumbnail.mode_member')}
    </button>
    <button
      type="button"
      disabled
      class="cursor-not-allowed rounded-md border border-border bg-surface-elevated px-3 py-1.5 text-xs text-fg-muted/60"
      title={t('common.coming_soon')}
    >
      {t('upload.thumbnail.mode_crop')}
    </button>
    <button
      type="button"
      onclick={() => coverInputEl?.click()}
      class={`rounded-md border px-3 py-1.5 text-xs transition-colors ${
        upload.compose.thumbMode === 'separate'
          ? 'border-accent bg-accent/10 text-accent'
          : 'border-border bg-surface-elevated text-fg-muted hover:text-fg'
      }`}
    >
      {t('upload.thumbnail.mode_upload')}
    </button>
    <input
      bind:this={coverInputEl}
      type="file"
      accept="image/*"
      class="hidden"
      onchange={handleCoverFile}
    />
  </div>

  <!-- Member picker grid. Visible when in 'member' mode. -->
  {#if upload.compose.thumbMode === 'member'}
    {#if memberRows.length === 0}
      <p class="rounded bg-surface-elevated px-3 py-2 text-xs text-fg-muted">
        {t('upload.thumbnail.awaiting')}
      </p>
    {:else}
      <div class="grid grid-cols-4 gap-2 sm:grid-cols-6">
        {#each memberRows as r (r.id)}
          <button
            type="button"
            onclick={() => pickMember(r.id)}
            class={`relative aspect-square overflow-hidden rounded border-2 transition-all ${
              upload.compose.thumbMemberRowId === r.id
                ? 'border-accent'
                : 'border-transparent opacity-60 hover:opacity-100'
            }`}
            aria-pressed={upload.compose.thumbMemberRowId === r.id}
            title={r.title}
          >
            <img src={r.objectUrl} alt={r.title} class="h-full w-full object-cover" />
          </button>
        {/each}
      </div>
      <p class="text-xs text-fg-muted">
        {t('upload.thumbnail.default_note')}
      </p>
    {/if}
  {/if}

  <!-- Separate-cover summary. Visible when in 'separate' mode. -->
  {#if upload.compose.thumbMode === 'separate'}
    <div class="rounded border border-border bg-surface-elevated p-3 text-xs">
      {#if coverState === 'idle' && !coverFileName}
        <p class="text-fg-muted">{t('upload.thumbnail.hint')}</p>
      {:else if coverState === 'uploading'}
        <p class="text-fg">{t('upload.thumbnail.uploading', { name: coverFileName ?? '', pct: Math.round(coverProgress * 100) })}</p>
        <div class="mt-1 h-1.5 w-full overflow-hidden rounded-full bg-surface">
          <div class="h-full bg-accent" style="width: {Math.round(coverProgress * 100)}%"></div>
        </div>
      {:else if coverState === 'ready'}
        <p class="text-success">
          {t('upload.thumbnail.ready', { name: coverFileName ?? '' })}
        </p>
      {:else if coverState === 'errored'}
        <p class="text-danger">{t('upload.thumbnail.error_prefix', { error: coverError ?? '' })}</p>
      {/if}
    </div>
  {/if}
</div>
