<script lang="ts">
  // A run of consecutive structural events: a folded summary until the
  // reader opens it. Open, a thought is drawn whole and readable, and a
  // tool call is one line that opens on its own to its whole input.
  // Drawn alike in the main transcript and inside a subagent's card.
  //
  // @decision(DL-058) @decision(DL-112) @decision(DL-158)
  import { SvelteSet } from 'svelte/reactivity';
  import { CollapseFrom, Thought, runSummary, shown, toolDetail, toolInput, type EventEntry } from '../lib/timeline';
  import Icon from './Icon.svelte';
  import { textButton } from './ui';

  interface Props {
    events: EventEntry[];
    /** Whether the reader opened the run. */
    open: boolean;
    ontoggle: () => void;
  }

  const { events, open, ontoggle }: Props = $props();

  const visible = $derived(shown(events, open));
  const foldable = $derived(events.length >= CollapseFrom);
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

<div class="min-w-0">
  {#if foldable}
    <button type="button" aria-expanded={open} class={textButton} onclick={ontoggle}>
      <Icon name="chevron" class="chevron size-3" />
      {runSummary(events)}
    </button>
  {/if}
  {#if visible.length}
    <ul class="m-0 list-none p-0 font-mono text-sm leading-[1.7] text-soft {foldable ? 'mt-1' : ''}">
      {#each visible as entry (entry.key)}
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
              {entry.calls.error ? `Failed: ${entry.calls.error}` : 'Failed'}
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
  {/if}
</div>
