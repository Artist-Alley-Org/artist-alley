<script lang="ts">
  import AccountStubPage from '$components/AccountStubPage.svelte';

  let { data } = $props();
</script>

<AccountStubPage slug={data.item.slug} phase={data.item.phase} />
