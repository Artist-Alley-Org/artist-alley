<script lang="ts">
  // Admin dropdown — gear icon → list of admin sections.
  //
  // Visibility-gated on `auth.can('system.admin')` so non-admin users
  // never see it. Backend enforces every action regardless; this is
  // a render-time hide for ergonomics, not a security gate.
  //
  // Pill counter is wired but always 0 today — when workflow
  // approvals / role-grant requests / etc. land, the count comes
  // from their respective stores.

  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import Menu from '$components/Menu.svelte';
  import Pill from '$components/Pill.svelte';

  const isAdmin = $derived(auth.can('system.admin'));
  const pendingCount = 0; // future: pending workflow approvals etc.
</script>

{#if isAdmin}
  <Menu align="right">
    {#snippet trigger({ open })}
      <span
        class="relative inline-flex h-9 w-9 items-center justify-center rounded-full hover:bg-surface-elevated"
        title={t('admin_menu.title')}
        aria-label={t('admin_menu.title')}
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="12" cy="12" r="3"/>
          <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z"/>
        </svg>
        <Pill count={pendingCount} label="admin alerts" />
        <span class="sr-only">{open ? 'close admin menu' : 'open admin menu'}</span>
      </span>
    {/snippet}

    <a href="/admin" role="menuitem" class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated">
      {t('admin_menu.title')}
    </a>
    <div class="border-t border-border"></div>
    <a href="/admin/users" role="menuitem" class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated">
      {t('admin_menu.users')}
    </a>
    <a href="/admin/roles" role="menuitem" class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated">
      {t('admin_menu.roles')}
    </a>
    <a href="/admin/workflow" role="menuitem" class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated">
      {t('admin_menu.workflow')}
    </a>
    <a href="/admin/fields" role="menuitem" class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated">
      {t('admin_menu.fields')}
    </a>
    <a href="/admin/resource-types" role="menuitem" class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated">
      {t('admin_menu.resource_types')}
    </a>
    <div class="border-t border-border"></div>
    <a href="/admin/system" role="menuitem" class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated">
      {t('admin_menu.system')}
    </a>
    <a href="/admin/system/log" role="menuitem" class="block px-3 py-1.5 text-sm text-fg hover:bg-surface-elevated">
      {t('admin_menu.system_log')}
    </a>
  </Menu>
{/if}
