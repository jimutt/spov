<script lang="ts">
  // A subagent in its parent's transcript: one card at the call that
  // started it, with its state, what it did last and what it concluded.
  // Opened, it draws the child's own condensed timeline inline under a
  // rule, through the same window as the main transcript. The card is
  // neither a bubble nor an agent's or spov's line; it is its own thing.
  // Everything that is a function is in ../lib/timeline.ts and
  // ../lib/subagent.ts.
  //
  // @decision(DL-112) @decision(DL-111) @decision(DL-113)
  import { SvelteSet } from 'svelte/reactivity';
  import type { Snippet } from 'svelte';
  import { stateMark, stateWord } from '../lib/subagent';
  import { blocksOf, condense, subagentText, type Entry, type SubagentEntry } from '../lib/timeline';
  import { earlier, earlierLabel, windowOf, type Start } from '../lib/transcriptwindow';
  import EventRun from './EventRun.svelte';
  import Icon from './Icon.svelte';
  import StateMark from './StateMark.svelte';
  import SubagentPrompt from './SubagentPrompt.svelte';
  import { secondary, textButton } from './ui';
  import Self from './SubagentCard.svelte';

  interface Props {
    entry: SubagentEntry;
    /** Every child conversation's own entries, by its OpenCode id. */
    kids: ReadonlyMap<string, Entry[]>;
    /** The pending asks raised in a conversation of a scope, in drawn order. */
    askingIn: (scope: string[]) => string[];
    /** Scroll to an ask by its id. */
    onjump: (id: string) => void;
    /** Draws an agent's words; the page's one place for Markdown. */
    markdown: Snippet<[string, string]>;
    /** The address of a view of this session: a subagent's, or the main one's for "". */
    href: (agent: string) => string;
    /** A click on a link to a view, which the page may take over. */
    onview: (ev: MouseEvent, agent: string) => void;
  }

  const { entry, kids, askingIn, onjump, markdown, href, onview }: Props = $props();

  let open = $state(false);
  // Runs the reader opened inside the card, by the run's stable key.
  const runs = new SvelteSet<string>();
  // The window over the child's blocks: sliding until the reader asks for
  // earlier ones, as the main transcript's is (DL-070).
  let start = $state<Start>(undefined);

  const waiting = $derived(askingIn(entry.scope));
  const live = $derived(entry.state === 'running' || entry.state === 'starting');
  // Blocks are only built once the card is opened: a closed card costs
  // the page nothing for what it holds.
  const blocks = $derived(open ? blocksOf(condense(kids.get(entry.id) ?? [])) : []);
  const windowed = $derived(windowOf(blocks, start));
  const counts = $derived(entry.summary || (entry.id && !live ? 'no tool calls' : ''));
  const label = $derived(entry.title || 'Subagent');

  const toggle =
    'inline-flex min-w-0 cursor-pointer items-center gap-1.5 border-0 bg-transparent p-0 text-left text-md font-medium text-ink ' +
    'focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2';
  const ruleOf = $derived(entry.state === 'failed' ? 'border-rust' : live ? 'border-olive' : 'border-rule-2');
</script>

<div class="min-w-0 border-l-2 pl-3.5 {ruleOf}" data-testid="subagent" data-state={entry.state}>
  <div class="flex flex-wrap items-center gap-x-2.5 gap-y-1">
    {#if entry.id}
      <button type="button" class={toggle} aria-expanded={open} title={subagentText(entry)} onclick={() => (open = !open)}>
        <Icon name="chevron" class="chevron size-3 shrink-0 text-faint" />
        <span class="min-w-0 break-words">{label}</span>
      </button>
    {:else}
      <!-- A call with no child behind it has nothing to open. -->
      <span class="min-w-0 break-words text-md font-medium text-ink">{label}</span>
    {/if}
    {#if entry.agent}<span class="font-mono text-sm text-faint" data-testid="subagent-agent">{entry.agent}</span>{/if}
    <StateMark class="text-sm" mark={stateMark(entry.state)} word={stateWord(entry.state)} />
    {#if entry.id}
      <!-- Its own view on this page, at full width. @decision(DL-113) -->
      <a class={textButton} href={href(entry.id)} data-testid="subagent-open" onclick={(ev) => onview(ev, entry.id)}>Open</a>
    {/if}
    {#if waiting.length}
      <button
        type="button"
        class="cursor-pointer rounded-ctl border border-gold bg-raised px-2 py-0.5 text-sm text-gold-t focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2"
        data-testid="subagent-waiting"
        aria-label="{waiting.length === 1 ? '1 question' : `${waiting.length} questions`} from this subagent are waiting on you. Go to the first."
        onclick={() => onjump(waiting[0])}>{waiting.length} waiting on you</button
      >
    {/if}
  </div>

  {#if counts}
    <p class="m-0 mt-0.5 text-sm text-faint" data-testid="subagent-summary">{counts}</p>
  {/if}
  {#if entry.state === 'running' && entry.latest}
    <p class="m-0 mt-0.5 truncate font-mono text-sm text-soft" title={entry.latest} data-testid="subagent-latest">{entry.latest}</p>
  {/if}
  {#if !live && !open && entry.final}
    <!-- What it concluded, which is what the parent acted on. -->
    <div class="mt-1.5 line-clamp-3" data-testid="subagent-final">{@render markdown(entry.final, 'text-md text-soft')}</div>
  {/if}

  {#if open}
    <div class="mt-3 flex flex-col gap-3" data-testid="subagent-work">
      {#if windowed.hidden > 0}
        <div>
          <button type="button" class={secondary} onclick={() => (start = earlier(blocks, start))} data-testid="subagent-earlier">
            {earlierLabel(windowed.hidden)}
          </button>
        </div>
      {/if}
      {#each windowed.shown as block (block.key)}
        {#if block.kind === 'message' && block.entry.role === 'prompt'}
          <SubagentPrompt text={block.entry.text} />
        {:else if block.kind === 'message'}
          <div class="min-w-0" data-testid="subagent-message">{@render markdown(block.entry.text, 'text-read')}</div>
        {:else if block.kind === 'subagent'}
          <Self entry={block.entry} {kids} {askingIn} {onjump} {markdown} {href} {onview} />
        {:else if block.kind === 'events'}
          {@const showAll = runs.has(block.key)}
          <EventRun events={block.events} open={showAll} ontoggle={() => (showAll ? runs.delete(block.key) : runs.add(block.key))} />
        {/if}
      {/each}
      {#if windowed.shown.length === 0}
        <p class="m-0 text-sm text-faint">Nothing recorded from this subagent yet.</p>
      {/if}
    </div>
  {/if}
</div>
