<script lang="ts">
  // A run of consecutive structural events: a folded summary until the
  // reader opens it. Open, a thought is drawn whole and readable, and a
  // tool call is one line that opens on its own to its whole input.
  // Drawn alike in the main transcript and inside a subagent's card.
  // Below `lg` an opened run is a sheet over the page; from `lg` it opens
  // in place under a sticky summary.
  //
  // @decision(DL-058) @decision(DL-112) @decision(DL-158) @decision(DL-167)
  import { tick } from 'svelte';
  import { SvelteSet } from 'svelte/reactivity';
  import {
    CollapseFrom,
    Thought,
    clipDetail,
    latestAction,
    runSummary,
    shown,
    toolDetail,
    toolInput,
    type EventEntry,
  } from '../lib/timeline';
  import { touchFirst } from '../lib/steer';
  import { lockScroll } from '../lib/scrolllock';
  import Icon from './Icon.svelte';
  import { secondary, textButton } from './ui';

  interface Props {
    events: EventEntry[];
    /** Whether the reader opened the run. */
    open: boolean;
    ontoggle: () => void;
    /** Whether this is the newest run of a turn still in flight. */
    live?: boolean;
  }

  const { events, open, ontoggle, live = false }: Props = $props();
  const foldable = $derived(events.length >= CollapseFrom);
  const summary = $derived(runSummary(events));
  // @decision(DL-168)
  const latest = $derived(live && !open ? latestAction(events) : '');

  // Whether the page is wide enough to open a run in place. Without
  // matchMedia (jsdom) it is.
  //
  // @decision(DL-167)
  const WideQuery = '(min-width: 64rem)';
  let wide = $state(true);
  $effect(() => {
    const mq = window.matchMedia?.(WideQuery);
    if (!mq) return;
    wide = mq.matches;
    const onChange = () => (wide = mq.matches);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  });
  // Whether the reader's own press opened the run as a sheet: decided
  // when it opens, so a viewport crossing `lg` meanwhile neither turns an
  // inline run into a sheet nor a sheet into an inline run.
  let asSheet = $state(false);
  const sheet = $derived(open && foldable && asSheet);
  const visible = $derived(shown(events, open && !sheet));

  let root = $state<HTMLElement | null>(null);
  let head = $state<HTMLElement | null>(null);
  let toggleButton = $state<HTMLElement | null>(null);
  let closeButton = $state<HTMLElement | null>(null);

  // The sheet: Escape, Close and Back close it. Opening pushes an entry
  // for the same address, so Back pops that rather than leaving the page,
  // and Close takes it back the same way. The page behind keeps still.
  //
  // @decision(DL-167)
  let pushed = false;
  let closing = false;
  $effect(() => {
    if (!sheet) return;
    history.pushState({ spovRun: true }, '', location.href);
    pushed = true;
    closing = false;
    const onPop = () => {
      if (!pushed) return;
      pushed = false;
      closing = false;
      asSheet = false;
      ontoggle();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close();
    };
    window.addEventListener('popstate', onPop);
    window.addEventListener('keydown', onKey);
    const release = lockScroll();
    const frame = requestAnimationFrame(() => closeButton?.focus({ preventScroll: true }));
    return () => {
      window.removeEventListener('popstate', onPop);
      window.removeEventListener('keydown', onKey);
      release();
      cancelAnimationFrame(frame);
      // The page behind may have followed new blocks meanwhile: focus
      // must not scroll it back to the summary.
      toggleButton?.focus({ preventScroll: true });
    };
  });

  // Once: a second Close or Escape before the first one's popstate
  // would go back past the sheet's entry and leave the page.
  function close(): void {
    if (closing) return;
    if (pushed) {
      closing = true;
      history.back();
    } else {
      asSheet = false;
      ontoggle();
    }
  }

  // Folding a run opened in place whose top has scrolled above its
  // sticky summary keeps the summary where the reader saw it.
  //
  // @decision(DL-167) @decision(DL-163)
  async function toggle(): Promise<void> {
    if (sheet) {
      close();
      return;
    }
    if (!open) asSheet = foldable && !wide;
    if (!open || sheet || !root || !head) {
      ontoggle();
      return;
    }
    const stuck = head.getBoundingClientRect().top;
    if (root.getBoundingClientRect().top >= stuck - 1) {
      ontoggle();
      return;
    }
    ontoggle();
    await tick();
    const by = root.getBoundingClientRect().top - stuck;
    window.scrollBy?.({ top: by, behavior: touchFirst() ? 'smooth' : 'instant' });
  }

  // The calls the reader opened, by entry key. A second press each, so
  // opening a run to read its thoughts never spills every call's input.
  // @decision(DL-158)
  const opened = new SvelteSet<string>();
  function flip(key: string): void {
    if (opened.has(key)) opened.delete(key);
    else opened.add(key);
  }

  // An event's name without the `session.` every one of them starts
  // with. Display only: grouping and keys use the real name.
  function eventName(name: string): string {
    return name.replace(/^session\./, '');
  }
</script>

{#snippet lines(entries: EventEntry[], spaced: boolean)}
  <ul class="m-0 list-none p-0 font-mono text-sm leading-[1.7] text-soft {spaced ? 'mt-1' : ''}">
    {#each entries as entry (entry.key)}
      {#if entry.calls}
        {@const input = toolInput(entry.calls.input)}
        {@const detail = toolDetail(entry.calls.input)}
        {@const failed = entry.calls.state === 'failed'}
        {@const isOpen = opened.has(entry.key)}
        <li class="min-w-0" data-testid="tool-call">
          <button
            type="button"
            class="flex w-full min-w-0 cursor-pointer items-baseline border-0 bg-transparent p-0 text-left font-mono text-sm leading-[1.7] text-soft hover:text-ink focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-1"
            aria-expanded={isOpen}
            title={input ? `${entry.calls.name}: ${input}` : entry.calls.name}
            onclick={() => flip(entry.key)}
          >
            <span class="inline-block min-w-[9ch] shrink-0 pr-2 {failed ? 'text-rust' : 'text-faint'}">{entry.calls.name || 'tool'}</span>
            <span class="min-w-0 {isOpen ? 'break-all' : 'truncate'}">{input}</span>
          </button>
          {#if isOpen && detail && detail !== input}
            <pre
              class="m-0 mt-1 mb-2 max-h-80 overflow-auto rounded-ctl border border-rule bg-sunk px-3 py-2 font-mono text-sm whitespace-pre-wrap break-words text-ink"
              data-testid="tool-detail">{detail}</pre>
          {/if}
        </li>
        {#if failed}
          <li
            class="pl-[9ch] font-sans text-rust {isOpen ? 'whitespace-pre-wrap break-words' : 'truncate'}"
            title={entry.calls.error}
          >
            {entry.calls.error ? `Failed: ${isOpen ? clipDetail(entry.calls.error) : entry.calls.error}` : 'Failed'}
          </li>
        {/if}
      {:else if entry.name === Thought && entry.text.trim()}
        <!-- Whole and in the body's own colour, set apart by a rule:
             a model that says little to the reader often says more
             here. @decision(DL-158) -->
        <li
          class="my-1.5 border-l-2 border-rule-2 pl-3 font-sans text-md leading-relaxed whitespace-pre-wrap break-words text-soft"
          data-testid="thought"
        >
          {entry.text.trim()}
        </li>
      {:else}
        <li class="truncate" title={entry.text ? `${entry.name}: ${entry.text}` : entry.name}>
          <span class="inline-block min-w-[13ch] pr-2 text-faint">{eventName(entry.name)}</span>{entry.text}
        </li>
      {/if}
    {/each}
  </ul>
{/snippet}

<div class="min-w-0" bind:this={root}>
  {#if foldable}
    <!-- Sticky only while open in place, so it can be folded from
         anywhere in a long run. @decision(DL-167) -->
    <div
      bind:this={head}
      class="flex min-w-0 items-baseline gap-1.5 {open && !sheet ? 'sticky top-[var(--header-h,0px)] z-10 -mx-1 bg-paper px-1 py-0.5' : ''}"
      data-testid="run-head"
    >
      <button
        type="button"
        bind:this={toggleButton}
        aria-expanded={open}
        class="{textButton} max-w-full shrink-0"
        onclick={toggle}
      >
        <Icon name="chevron" class="chevron size-3 shrink-0" />
        {summary}
      </button>
      {#if latest}
        <!-- @decision(DL-168) -->
        <span class="min-w-0 truncate font-mono text-sm text-faint" data-testid="run-latest">· {latest}</span>
      {/if}
    </div>
  {/if}
  {#if visible.length}
    {@render lines(visible, foldable)}
  {/if}
</div>

{#if sheet}
  <!-- Over the whole page, as the details sheet is. @decision(DL-167) @decision(DL-106) -->
  <div class="fixed inset-0 z-30 flex flex-col bg-paper" role="dialog" aria-modal="true" aria-label={summary} data-testid="run-sheet">
    <div class="flex items-center justify-between gap-3 border-b border-rule bg-raised px-4 py-2">
      <span class="min-w-0 truncate text-base font-semibold">{summary}</span>
      <button type="button" bind:this={closeButton} class={secondary} onclick={close}>Close</button>
    </div>
    <div class="min-h-0 flex-1 overflow-y-auto overscroll-contain px-4 pt-2 pb-10">
      {@render lines(events, false)}
    </div>
  </div>
{/if}
