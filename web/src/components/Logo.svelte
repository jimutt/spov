<script lang="ts">
  // The spov mark. One path with fill-rule="evenodd": the two enclosed
  // subpaths are holes, not white shapes, so the interior is genuinely
  // transparent and the mark reads correctly on either theme.
  //
  // It is drawn in currentColor and has no colour of its own, so it
  // follows whatever the surrounding text is set to.
  //
  // @decision(DL-056)
  import { MarkAspect, MarkPath, MarkViewBox } from './logo';

  interface Props {
    /** Rendered height in pixels. The width follows the mark's own
        aspect, which is slightly wider than tall — forcing a square box
        would squash it. */
    size?: number;
    /** Accessible name. Empty (the default) hides the mark from
        assistive technology, which is right beside the `spov` wordmark
        — the name is already there in text, and announcing it twice is
        worse than not at all.

        aria-hidden alone, without role="presentation": ARIA resolves
        the conflict between a presentation role and a global attribute
        by ignoring the role, so the pair is dead weight at best. It is
        also what ThemeToggle's own icons already do. */
    title?: string;
    class?: string;
  }
  let { size = 24, title = '', class: klass = '' }: Props = $props();

</script>

<svg
  viewBox={MarkViewBox}
  width={Math.round(size * MarkAspect)}
  height={size}
  fill="currentColor"
  class={klass}
  role={title ? 'img' : undefined}
  aria-hidden={title ? undefined : 'true'}
  aria-label={title || undefined}
  xmlns="http://www.w3.org/2000/svg"
>
  <path fill-rule="evenodd" d={MarkPath} />
</svg>
