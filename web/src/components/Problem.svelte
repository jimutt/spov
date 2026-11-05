<script lang="ts">
  // The one place a failure is rendered. Every page hands whatever it
  // caught to this, so the server's own words reach the reader instead
  // of a message the page invented for them.
  //
  // The body is shown as it arrived: the scrub that keeps secrets out of
  // a problem response is the server's, and stays there.
  //
  // @decision(DL-055) @decision(DL-005) @decision(DL-056)
  import { ApiError } from '../lib/api/client';

  let { error, what = 'this page' }: { error: unknown; what?: string } = $props();

  const api = $derived(error instanceof ApiError ? error : undefined);
  const title = $derived(
    api?.problem?.title ?? (error instanceof Error ? error.message : String(error ?? '')),
  );
  const detail = $derived(api?.problem?.detail);
  const status = $derived(api?.status);
</script>

<div class="border border-rust/40 bg-rust/[0.07] px-4 py-3" role="alert">
  <p class="m-0 text-base font-semibold text-ink">
    {title || `Could not load ${what}`}
  </p>
  {#if detail}
    <p class="mt-1 mb-0 max-w-[64ch] text-md text-soft">{detail}</p>
  {/if}
  {#if status}
    <p class="mt-1.5 mb-0 text-sm text-soft">Status {status}</p>
  {/if}
</div>
