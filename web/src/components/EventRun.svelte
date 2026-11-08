<script lang="ts">
  // A run of consecutive structural events: a folded summary until the
  // reader opens it, then one mono line per tool call or event. Drawn
  // alike in the main transcript and inside a subagent's card.
  //
  // @decision(DL-058) @decision(DL-112)
  import { CollapseFrom, Thought, runSummary, shown, toolInput, type EventEntry } from '../lib/timeline';
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
          {@const failed = entry.calls.state === 'failed'}
          <li class="truncate" title={input ? `${entry.calls.name}: ${input}` : entry.calls.name}>
            <span class="inline-block min-w-[9ch] pr-2 {failed ? 'text-rust' : 'text-faint'}">{entry.calls.name || 'tool'}</span
            >{input}
          </li>
          {#if failed}
            <li class="truncate pl-[9ch] font-sans text-rust" title={entry.calls.error}>
              {entry.calls.error ? `Failed: ${entry.calls.error}` : 'Failed'}
            </li>
          {/if}
        {:else if entry.name === Thought && entry.text.trim()}
          <li class="line-clamp-2 font-sans whitespace-normal text-faint italic" title={entry.text}>
            {entry.text}
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
