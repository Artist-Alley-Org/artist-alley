<script lang="ts">
  // Small count-badge pill used by the messages icon and (eventually)
  // admin menu pending-approval counter. Hides itself when count <= 0
  // so the caller can render unconditionally.

  interface Props {
    count: number;
    /** Cap and render as "99+" past this number. Default 99. */
    cap?: number;
    /** Accessible label. Default: "{count} notifications". */
    label?: string;
  }

  let { count, cap = 99, label }: Props = $props();

  const display = $derived(count > cap ? `${cap}+` : String(count));
</script>

{#if count > 0}
  <span
    class="pointer-events-none absolute -right-1 -top-1 inline-flex h-4 min-w-[1rem] items-center justify-center rounded-full bg-red-500 px-1 text-[10px] font-semibold leading-none text-white"
    aria-label={label ?? `${count} notifications`}
  >
    {display}
  </span>
{/if}
