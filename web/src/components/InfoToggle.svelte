<script lang="ts">
  // A small (i) control that shows or hides an explanation the caller
  // draws elsewhere, for detail a reader wants on demand rather than by
  // default. The caller owns `open` and places the note, so the control
  // can sit inside a heading or a row while its note goes below.
  //
  // @decision(DL-057)
  import Icon from './Icon.svelte';

  let {
    open = $bindable(false),
    label,
    controls,
  }: { open?: boolean; label: string; controls?: string } = $props();
</script>

<button
  type="button"
  class="inline-flex size-5 shrink-0 cursor-pointer items-center justify-center rounded-full border-0 bg-transparent p-0 align-middle text-faint hover:text-ink focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2 {open
    ? 'text-ink'
    : ''}"
  aria-label={label}
  aria-expanded={open}
  aria-controls={controls}
  title={label}
  onclick={() => (open = !open)}
>
  <Icon name="info" class="size-3.5" />
</button>
