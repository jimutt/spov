<script lang="ts">
  // Two icon buttons, each named for a screen reader and in a tooltip.
  // No choice follows the system; a press pins one (theme.ts).
  //
  // @decision(DL-055) @decision(DL-056)
  import { onMount } from 'svelte';
  import { choose, resolve, type Theme } from './theme';
  import Icon from './Icon.svelte';

  // Rendered on the server too, where there is no document to read, so
  // the real theme is only known once mounted.
  let active = $state<Theme | null>(null);

  onMount(() => {
    active = resolve();
  });

  function pick(t: Theme) {
    choose(t);
    active = t;
  }

  const base =
    'grid h-[26px] w-[30px] cursor-pointer place-items-center border-0 bg-transparent text-faint ' +
    'transition-colors hover:text-ink aria-pressed:bg-sunk aria-pressed:text-ink ' +
    'focus-visible:outline-2 focus-visible:outline-olive focus-visible:-outline-offset-2 motion-reduce:transition-none';
</script>

<div class="inline-flex overflow-hidden rounded-ctl border border-rule-2" role="group" aria-label="Colour theme">
  <button
    type="button"
    class={base}
    aria-pressed={active === 'light'}
    aria-label="Light"
    title="Light"
    onclick={() => pick('light')}
  >
    <Icon name="sun" />
  </button>
  <button
    type="button"
    class="{base} border-l border-rule-2"
    aria-pressed={active === 'dark'}
    aria-label="Dark"
    title="Dark"
    onclick={() => pick('dark')}
  >
    <Icon name="moon" />
  </button>
</div>
