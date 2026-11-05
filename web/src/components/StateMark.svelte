<script lang="ts">
  // A state as a shape, a colour and a word, so it never rests on colour
  // alone. Sessions, jobs and gates all draw through this; `format.ts`
  // decides which mark a state gets. It does not animate.
  //
  // @decision(DL-056)
  import type { Mark } from '../lib/format';

  let {
    mark,
    word,
    class: klass = '',
  }: { mark: Mark; word: string; class?: string } = $props();

  const ink: Record<Mark, string> = {
    run: 'text-olive-b',
    done: 'text-olive-b',
    part: 'text-gold-t',
    wait: 'text-gold-t',
    fail: 'text-rust',
    stop: 'text-stone',
  };
</script>

<span class="inline-flex items-center gap-1.5 whitespace-nowrap {ink[mark]} {klass}" data-mark={mark}>
  {#if mark === 'run'}
    <span class="size-2 shrink-0 rounded-full bg-olive" aria-hidden="true"></span>
  {:else if mark === 'wait'}
    <span class="size-2 shrink-0 rounded-full border-2 border-gold" aria-hidden="true"></span>
  {:else if mark === 'stop'}
    <span class="size-2 shrink-0 rounded-full border-2 border-stone" aria-hidden="true"></span>
  {:else if mark === 'fail'}
    <span class="mx-px size-[7px] shrink-0 rotate-45 rounded-[1px] bg-rust" aria-hidden="true"></span>
  {:else}
    <svg class="size-3 shrink-0 {mark === 'part' ? 'text-gold' : 'text-olive'}" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M2.2 6.4l2.6 2.5 5-5.8" />
    </svg>
  {/if}
  <span>{word}</span>
</span>
