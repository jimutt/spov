<script lang="ts">
  // A confirmation: a saved setting, a stored key, a finished sign-in.
  // Plain soft text after an olive tick, with no stripe. `status` makes
  // it a polite live region, for a note that reports a server's answer.
  //
  // @decision(DL-056)
  import type { Snippet } from 'svelte';

  let {
    status = false,
    class: klass = '',
    children,
  }: { status?: boolean; class?: string; children: Snippet } = $props();
</script>

<p class="m-0 flex max-w-[64ch] items-baseline gap-2 text-md text-soft {klass}" role={status ? 'status' : undefined}>
  <svg class="size-3 shrink-0 translate-y-px text-olive" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path d="M2.2 6.4l2.6 2.5 5-5.8" />
  </svg>
  <span>{@render children()}</span>
</p>
