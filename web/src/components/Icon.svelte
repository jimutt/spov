<script lang="ts" module>
  export type IconName = 'chevron' | 'down' | 'external' | 'cross' | 'sun' | 'moon' | 'info' | 'menu' | 'back' | 'send' | 'sliders';
</script>

<script lang="ts">
  // The app's few icons, as inline SVG in currentColor. Always beside a
  // word or carrying an aria-label on their control, so they are hidden
  // from assistive technology here.
  //
  // @decision(DL-056)
  let { name, class: klass = 'size-3.5' }: { name: IconName; class?: string } = $props();
</script>

<svg
  class={klass}
  viewBox="0 0 24 24"
  fill="none"
  stroke="currentColor"
  stroke-width="2"
  stroke-linecap="round"
  stroke-linejoin="round"
  aria-hidden="true"
>
  {#if name === 'chevron'}
    <path d="M9 5l7 7-7 7" />
  {:else if name === 'down'}
    <path d="M5 9l7 7 7-7" />
  {:else if name === 'external'}
    <path d="M9 5h10v10M19 5L6 18" />
  {:else if name === 'cross'}
    <path d="M6 6l12 12M18 6L6 18" />
  {:else if name === 'sun'}
    <circle cx="12" cy="12" r="4" />
    <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
  {:else if name === 'info'}
    <circle cx="12" cy="12" r="9" />
    <path d="M12 11v5M12 8h.01" />
  {:else if name === 'menu'}
    <path d="M4 7h16M4 12h16M4 17h16" />
  {:else if name === 'back'}
    <path d="M15 5l-7 7 7 7" />
  {:else if name === 'send'}
    <path d="M12 19V5M5 12l7-7 7 7" />
  {:else if name === 'sliders'}
    <path d="M4 7h10M18 7h2M4 17h2M10 17h10" />
    <circle cx="16" cy="7" r="2" />
    <circle cx="8" cy="17" r="2" />
  {:else if name === 'moon'}
    <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />
  {/if}
</svg>
