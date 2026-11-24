<script>
  // One cover image, blurred when the daemon says it is explicit.
  //
  // An <img src> cannot read a response header, and whether a cover is explicit
  // is only known once the daemon has answered — it comes back as
  // X-Cover-Explicit. So this fetches the bytes itself, reads the header, and
  // shows the result from a blob.
  //
  // Both screens that show covers use this, rather than each deciding for
  // itself. The blur previously existed only in the sidebar, so the same
  // picture was covered in one place and bare in the other.
  import { onDestroy } from 'svelte';

  /** Cover URL on the local daemon, or '' for no cover. */
  export let src = '';
  export let alt = '';
  /** Reveal an explicit cover — the parent decides, on hover or focus. */
  export let revealed = false;

  let objectUrl = '';
  let explicit = false;
  let failed = false;
  let loadedFor = '';
  let attempt = 0;
  let retryTimer = null;
  let destroyed = false;
  let reqSeq = 0;
  let controller = null;

  // Blob URLs are held by the browser until released, and a scan grid builds
  // hundreds of these.
  const release = () => {
    if (objectUrl) URL.revokeObjectURL(objectUrl);
    objectUrl = '';
  };
  const cancelRetry = () => {
    clearTimeout(retryTimer);
    retryTimer = null;
  };
  const abortPending = () => {
    if (controller) {
      controller.abort();
      controller = null;
    }
  };
  onDestroy(() => {
    destroyed = true;
    reqSeq++;
    abortPending();
    release();
    cancelRetry();
  });

  // Retry delays for a cover that could not be fetched this time. The first
  // two cover the usual cause, which is over in a second; the last two reach
  // past a daemon that is restarting. Finite, because a sidebar of fifty
  // games should not keep asking forever.
  const retryDelays = [1500, 5000, 15000, 45000];

  async function load(url, isRetry = false) {
    if (destroyed) return;
    if (!url) {
      reqSeq++;
      abortPending();
      cancelRetry();
      release();
      failed = false;
      explicit = false;
      loadedFor = '';
      attempt = 0;
      return;
    }
    if (loadedFor === url && !isRetry) return;
    cancelRetry();
    abortPending();
    loadedFor = url;
    const mySeq = ++reqSeq;
    const ac = new AbortController();
    controller = ac;
    try {
      const res = await fetch(url, isRetry ? { cache: 'reload', signal: ac.signal } : { signal: ac.signal });
      if (destroyed || mySeq !== reqSeq) return;
      if (!res.ok) {
        giveUp();
        return;
      }
      explicit = res.headers.get('X-Cover-Explicit') === '1';
      const blob = await res.blob();
      if (destroyed || mySeq !== reqSeq) return;
      release();
      objectUrl = URL.createObjectURL(blob);
      failed = false;
      attempt = 0;
    } catch {
      if (destroyed || mySeq !== reqSeq) return;
      release();
      failed = true;
      if (attempt < retryDelays.length) {
        const delay = retryDelays[attempt++];
        retryTimer = setTimeout(() => load(url, true), delay);
      }
    } finally {
      if (controller === ac) controller = null;
    }
  }

  function giveUp() {
    release();
    failed = true;
    attempt = 0;
  }

  $: load(src);
  $: hidden = failed || !objectUrl;
</script>

{#if !hidden}
  <img
    src={objectUrl}
    {alt}
    class:explicit={explicit && !revealed}
    loading="lazy"
    on:load
  />
{/if}

<style>
  /* Explicit art is blurred, not removed: the tile keeps the game's shape and
     colour so a shelf stays recognisable, while nothing in it is legible. The
     slight scale hides the soft edge a blur leaves at the border; the parent
     clips it. */
  img.explicit {
    filter: blur(14px) saturate(0.7);
    transform: scale(1.15);
  }
  img {
    transition: filter 120ms ease, transform 120ms ease;
  }
</style>
