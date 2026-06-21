<script lang="ts">
  // Phase 1.14.B — Similar assets panel.
  //
  // Fetches /assets/{id}/similar for the given anchor asset and
  // renders a grid of compact cards: thumbhash + title + a strip
  // of source-coloured tag badges. Distance shows as a small chip
  // on hover so an operator can audit the ranking.
  //
  // Empty states:
  //   - anchor_has_embedding=false → "Embedding pending. Check back in a moment."
  //   - results.length=0           → "No similar assets in your library yet."
  //   - fetch error                → terse error, retry button.
  //
  // Mounted by DetailsTool/Body.svelte when an asset is open, but
  // designed standalone so the standalone /assets/[id] route can
  // also use it directly.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import AssetTagBadge from './AssetTagBadge.svelte';

  interface Props {
    assetId: string;
    /** Limit. Default 12 = the 4-col grid's 3-row tile field. */
    limit?: number;
  }

  let { assetId, limit = 12 }: Props = $props();

  type ApiAsset = {
    id: string;
    title: string;
    thumbhash?: string | null;
    tag_details?: Array<{
      value: string;
      source: 'manual' | 'ai' | 'import';
      confidence?: number | null;
    }>;
  };
  type Result = { asset: ApiAsset; distance: number };

  let results = $state<Result[]>([]);
  let anchorHasEmbedding = $state(true);
  let loading = $state(true);
  let error = $state<string | null>(null);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    error = null;
    try {
      const { data, error: apiErr } = await api.GET('/assets/{id}/similar', {
        params: { path: { id: assetId }, query: { limit } },
      });
      if (apiErr) {
        error = (apiErr as { error?: string }).error ?? t('asset.similar.load_error');
        return;
      }
      if (data) {
        const d = data as { results?: Result[]; anchor_has_embedding?: boolean };
        results = d.results ?? [];
        anchorHasEmbedding = d.anchor_has_embedding ?? false;
      }
    } catch (e) {
      error = String(e);
    } finally {
      loading = false;
    }
  }
</script>

<section class="space-y-3">
  <header class="flex items-baseline justify-between gap-2">
    <h3 class="text-sm font-medium text-fg">{t('asset.similar.title')}</h3>
    {#if !loading && results.length > 0}
      <span class="text-[10px] text-fg-muted">{t('asset.similar.count', { n: results.length })}</span>
    {/if}
  </header>

  {#if loading}
    <p class="text-xs text-fg-muted">{t('common.loading')}</p>
  {:else if error}
    <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-xs text-danger">
      {error}
      <button type="button" onclick={() => void load()} class="ml-2 underline hover:no-underline">{t('common.retry')}</button>
    </p>
  {:else if !anchorHasEmbedding}
    <p class="rounded border border-border bg-surface-elevated px-3 py-2 text-xs text-fg-muted">
      {t('asset.similar.embedding_pending')}
    </p>
  {:else if results.length === 0}
    <p class="rounded border border-border bg-surface-elevated px-3 py-2 text-xs text-fg-muted">
      {t('asset.similar.empty')}
    </p>
  {:else}
    <div class="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-4">
      {#each results as r (r.asset.id)}
        <a
          href={`/posts/by-asset/${r.asset.id}`}
          class="block rounded-md border border-border bg-surface p-2 transition hover:border-border-strong hover:bg-surface-elevated"
          title={t('asset.similar.distance_label', { d: r.distance.toFixed(3) })}
        >
          <div class="aspect-square w-full overflow-hidden rounded bg-surface-elevated">
            {#if r.asset.thumbhash}
              <!-- thumbhash decode is lazy via <img>; the real
                   thumbhash decoder runs in viewers/. For this
                   compact card the raw thumbhash is a placeholder
                   only — clicking opens the real surface. -->
              <div class="h-full w-full bg-fg-muted/10"></div>
            {/if}
          </div>
          <p class="mt-1.5 line-clamp-2 text-xs font-medium text-fg">{r.asset.title || '—'}</p>
          {#if r.asset.tag_details && r.asset.tag_details.length > 0}
            <div class="mt-1 flex flex-wrap gap-1">
              {#each r.asset.tag_details.slice(0, 3) as td (td.value)}
                <AssetTagBadge
                  value={td.value}
                  source={td.source}
                  confidence={td.confidence ?? null}
                />
              {/each}
            </div>
          {/if}
        </a>
      {/each}
    </div>
  {/if}
</section>
