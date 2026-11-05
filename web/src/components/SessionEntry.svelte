<script lang="ts">
  // One row of the session list's table: state, session, started,
  // running for, and cost. The last thing said is the session cell's
  // caption, answered from the store in one query for the whole page.
  //
  // The meta line carries the kind and the repositories; requested
  // resources and the sandbox id are on the session page.
  //
  // @decision(DL-058) @decision(DL-055) @decision(DL-060)
  import { age, duration, kindName, lifetime, listState, live, money, repoNames, speaker, when } from '../lib/format';
  import type { Session } from '../lib/format';
  import StateMark from './StateMark.svelte';

  let { session, now }: { session: Session; now: Date } = $props();

  const life = $derived(lifetime(session, now));
  const repos = $derived(repoNames(session));
  // The job driving this session, if one is. Its state is what the
  // State column shows, and its kind names the row.
  //
  // @decision(DL-060)
  const job = $derived(session.job);
  const state = $derived(listState(session));
  const last = $derived(session.last_message);
  const said = $derived(last?.text?.trim() ?? '');

  const trackTone = $derived(!live(session) ? 'bg-rule-2' : life.warn ? 'bg-gold' : 'bg-olive');
</script>

<div
  class="relative grid grid-cols-[minmax(0,1fr)_auto] items-start gap-x-3 gap-y-1 border-b border-rule px-2 pt-3.5 pb-4 hover:bg-raised has-[a:focus-visible]:outline-2 has-[a:focus-visible]:-outline-offset-2 has-[a:focus-visible]:outline-olive md:grid-cols-[124px_minmax(0,1fr)_120px_132px_84px] md:gap-x-5 md:gap-y-0"
  role="row"
>
  <span class="pt-0.5 text-md max-md:col-start-1 max-md:row-start-1" role="cell">
    <StateMark mark={state.mark} word={state.word} />
    {#if state.why}<span class="mt-0.5 ml-3.5 block text-sm text-soft">{state.why}</span>{/if}
  </span>

  <div class="min-w-0 max-md:col-span-2 max-md:row-start-2" role="cell">
    <!-- The name stretches over the whole row, so the row is the link. -->
    <a
      class="text-base font-semibold text-ink no-underline after:absolute after:inset-0 focus-visible:outline-none"
      href={`/session/?id=${encodeURIComponent(session.id)}`}>{session.name || session.id}</a
    >
    <div class="mt-0.5 flex flex-wrap gap-x-3.5 gap-y-0.5 text-md text-soft">
      <span>{job ? `${kindName(job.kind)} job` : 'Interactive'}</span>
      {#each repos as repo (repo)}
        <span class="font-mono text-ink">{repo}</span>
      {/each}
    </div>
    {#if session.error}
      <p class="m-0 mt-1.5 line-clamp-2 max-w-[64ch] text-md text-rust">{session.error}</p>
    {:else if last}
      <!-- Two lines at most: the list is scanned, and one entry that
           runs long pushes every other one off the screen. -->
      <p class="m-0 mt-1.5 line-clamp-2 max-w-[64ch] text-md text-soft">
        <b class="font-semibold text-ink">{speaker(last.role)}:</b>
        {#if said}{said}{:else}…{/if}
      </p>
    {:else}
      <p class="m-0 mt-1.5 max-w-[64ch] text-md text-soft">
        {session.state === 'starting' ? 'Waiting for the sandbox to become ready.' : 'Nothing recorded yet.'}
      </p>
    {/if}
  </div>

  <span class="pt-0.5 text-md text-soft tabular-nums max-md:col-start-2 max-md:row-start-1 max-md:text-right" role="cell"
    >{when(session.created_at, now)}</span
  >

  <div class="pt-0.5 text-md text-soft tabular-nums max-md:col-start-1 max-md:row-start-3" role="cell">
    <span class="text-ink">{duration(age(session, now))}</span>
    <div class="relative mt-1.5 mb-1 h-[3px] max-w-[132px] bg-sunk">
      <span class="absolute inset-y-0 left-0 {trackTone}" style={`width:${(life.fraction * 100).toFixed(1)}%`}></span>
    </div>
    <span class="text-sm {life.warn ? 'font-semibold text-gold-t' : 'text-soft'}">{life.note}</span>
  </div>

  <!-- The column header says "at least"; on a phone it is hidden, so
       the cell says it. @decision(DL-009) @decision(DL-060) -->
  <span
    class="pt-0.5 text-right text-md text-ink tabular-nums max-md:col-start-2 max-md:row-start-3 max-md:self-end"
    role="cell"><span class="text-soft md:hidden">at least&nbsp;</span>{money(session.estimated_cost)}</span
  >
</div>
