<script lang="ts">
  // /account home — full tile grid grouped by section.

  import { t } from '$stores/lang.svelte';
  import AdminIcon from '$components/AdminIcon.svelte';
  import { ACCOUNT_GROUPS, itemsByGroup } from '$lib/account/sections';
</script>

<svelte:head><title>{t('account.title')} — artist-alley</title></svelte:head>

<p class="mb-6 text-sm text-fg-muted">{t('account.overview.intro')}</p>

<div class="space-y-8">
  {#each ACCOUNT_GROUPS as group (group.id)}
    <section>
      <header class="mb-3 flex items-center gap-2">
        <span class="text-fg-muted"><AdminIcon name={group.iconKey} size={16} /></span>
        <h2 class="text-sm font-semibold uppercase tracking-wide text-fg-muted">
          {t(`account.groups.${group.id}.title`)}
        </h2>
      </header>

      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {#each itemsByGroup(group.id) as item (item.slug)}
          <a
            href={item.href}
            class="rounded-lg border border-border bg-surface p-4 transition-colors hover:border-accent/50"
          >
            <div class="flex items-start justify-between gap-2">
              <h3 class="text-sm font-medium text-fg">
                {t(`account.items.${item.slug}.title`)}
              </h3>
              {#if item.status === 'stub' && item.phase}
                <span class="shrink-0 rounded-full bg-warning-container px-2 py-0.5 text-[10px] font-medium text-warning">
                  {t('admin.status.phase', { phase: item.phase })}
                </span>
              {/if}
            </div>
            <p class="mt-1 text-xs text-fg-muted">{t(`account.items.${item.slug}.blurb`)}</p>
          </a>
        {/each}
      </div>
    </section>
  {/each}
</div>
