<script lang="ts">
  // Standalone post page. Renders the same PostModal component that
  // the browse-feed overlay uses; the difference is the close
  // behavior (back to / if there's no in-app history, otherwise
  // history.back()).
  //
  // This route is hit by:
  //   - Shared / bookmarked URLs
  //   - Cmd/Ctrl+click on a PostCard (browser opens in a new tab,
  //     which boots the standalone page)
  //   - Direct paste into the address bar
  //
  // The /?post= overlay path (browse → modal) is preferred for
  // in-feed clicks since it preserves the feed underneath.

  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import PostModal from '$components/PostModal.svelte';

  const postId = $derived(page.params.id ?? '');

  async function handleClose() {
    // If the user came from somewhere in our app, go back; otherwise
    // land on the browse feed. document.referrer is empty for
    // direct-nav so we default to /.
    if (window.history.length > 1 && document.referrer.startsWith(window.location.origin)) {
      window.history.back();
    } else {
      await goto('/');
    }
  }
</script>

<svelte:head>
  <title>Post — artist-alley</title>
</svelte:head>

<PostModal {postId} onClose={handleClose} standalone />
