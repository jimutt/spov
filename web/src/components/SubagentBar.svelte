<script lang="ts">
  // The head of a subagent's view: the way back as a breadcrumb, the
  // subagent's label and state, and Previous and Next between the
  // children of its parent. Links are real addresses; the page takes a
  // plain click over so the view changes without a reload.
  //
  // @decision(DL-113) @decision(DL-106)
  import { stateMark, stateWord } from '../lib/subagent';
  import type { Siblings, SubagentNode } from '../lib/subagentview';
  import StateMark from './StateMark.svelte';

  interface Props {
    /** What the session is called. */
    session: string;
    /** The subagent's ancestors from the top, then itself. */
    trail: SubagentNode[];
    siblings?: Siblings;
    /** How many questions from it or its own are waiting on the reader. */
    waiting: number;
    href: (agent: string) => string;
    onview: (ev: MouseEvent, agent: string) => void;
  }

  const { session, trail, siblings, waiting, href, onview }: Props = $props();

  const node = $derived(trail[trail.length - 1]);
  const above = $derived(trail.slice(0, -1));
  const link =
    'min-w-0 truncate text-soft underline decoration-rule-2 underline-offset-[3px] hover:text-ink focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2';
  const step =
    'text-soft underline decoration-rule-2 underline-offset-[3px] hover:text-ink focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2';
</script>

<div class="mb-6 max-w-[var(--log-w)]" data-testid="subagent-bar">
  <!-- Truncates the label and keeps the way back, so a long one never
       pushes it off a phone's screen. @decision(DL-106) -->
  <nav aria-label="Where you are" class="flex min-w-0 items-baseline gap-1.5 text-md">
    <a class="{link} max-w-[45%] shrink-0" href={href('')} data-testid="subagent-back" onclick={(ev) => onview(ev, '')}>{session}</a>
    {#each above as up, i (up.id)}
      <span class="text-faint max-sm:hidden" aria-hidden="true">›</span>
      <a class="{link} max-w-[30%] max-sm:hidden" href={href(up.id)} data-testid="subagent-ancestor" onclick={(ev) => onview(ev, up.id)}
        >{up.label}</a
      >
      {#if i === above.length - 1}
        <!-- A phone has room for one level up, as a mark. -->
        <span class="text-faint sm:hidden" aria-hidden="true">›</span>
        <a
          class="{link} shrink-0 sm:hidden"
          href={href(up.id)}
          aria-label="Up to {up.label}"
          data-testid="subagent-up"
          onclick={(ev) => onview(ev, up.id)}>…</a
        >
      {/if}
    {/each}
    <span class="text-faint" aria-hidden="true">›</span>
    <span class="min-w-0 flex-1 truncate font-medium text-ink" aria-current="page" title={node.label} data-testid="subagent-crumb">{node.label}</span>
  </nav>

  <div class="mt-3 flex flex-wrap items-center gap-x-2.5 gap-y-1">
    <h2 class="m-0 min-w-0 text-lg font-semibold [overflow-wrap:anywhere]" data-testid="subagent-heading">{node.label}</h2>
    {#if node.agent}<span class="font-mono text-sm text-faint">{node.agent}</span>{/if}
    <StateMark class="text-sm" mark={stateMark(node.state)} word={stateWord(node.state)} />
    {#if waiting > 0}
      <!-- Its questions are answered in the main conversation, where they
           stay. @decision(DL-020) -->
      <a
        class="rounded-ctl border border-gold bg-raised px-2 py-0.5 text-sm text-gold-t no-underline focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2"
        href={href('')}
        data-testid="subagent-waiting"
        onclick={(ev) => onview(ev, '')}>{waiting} waiting on you, in the main conversation</a
      >
    {/if}
  </div>
  <p class="m-0 mt-0.5 text-sm text-faint">{node.calls === 1 ? '1 tool call' : `${node.calls} tool calls`}</p>

  {#if siblings && siblings.total > 1}
    <div class="mt-3 flex items-center justify-between gap-3 text-sm" data-testid="subagent-siblings">
      {#if siblings.previous}
        <a class={step} rel="prev" href={href(siblings.previous.id)} title={siblings.previous.label} data-testid="subagent-previous" onclick={(ev) => onview(ev, siblings.previous!.id)}
          >‹ Previous</a
        >
      {:else}
        <span class="text-faint" data-testid="subagent-previous-none">‹ Previous</span>
      {/if}
      <span class="text-faint tabular-nums">{siblings.place} of {siblings.total}</span>
      {#if siblings.next}
        <a class={step} rel="next" href={href(siblings.next.id)} title={siblings.next.label} data-testid="subagent-next" onclick={(ev) => onview(ev, siblings.next!.id)}
          >Next ›</a
        >
      {:else}
        <span class="text-faint" data-testid="subagent-next-none">Next ›</span>
      {/if}
    </div>
  {/if}
</div>
