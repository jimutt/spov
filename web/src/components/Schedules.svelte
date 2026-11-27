<script lang="ts">
  // The caller's schedules: each one's cadence in words, its state, its
  // next run and its last run with the job it started. The logic is in
  // ../lib/schedule.ts.
  //
  // @decision(DL-195)
  import { api, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import { cadenceWords, jobPage, outcomeWord, pausedLine, schedulePage, whenIn } from '../lib/schedule';
  import Problem from './Problem.svelte';
  import { link, pageTitle, primary } from './ui';

  const list = resource(async (signal) => unwrap(await api.GET('/api/v1/schedules', { signal })));
  const rows = $derived(list.data?.schedules ?? undefined);
</script>

<div class="mb-5 flex flex-wrap items-center justify-between gap-x-6 gap-y-3">
  <div>
    <h1 class="{pageTitle} mb-1">Schedules</h1>
    <p class="m-0 text-md text-soft">Jobs that start on a cadence. <a class="text-soft {link}" href="/jobs/">All jobs</a></p>
  </div>
  <a class={primary} href="/schedules/new/">New schedule</a>
</div>

{#if list.error}
  <div class="mb-5"><Problem error={list.error} what="your schedules" /></div>
{/if}

{#if rows === undefined}
  {#if !list.error}<p class="text-soft">Loading…</p>{/if}
{:else if rows.length === 0}
  <p class="max-w-[62ch] text-soft" data-testid="schedules-empty">
    No schedules yet. A schedule starts the same job on a cadence: every day, every Monday morning, the first of
    every month.
  </p>
{:else}
  <div class="border-t border-rule" role="table" aria-label="Schedules">
    <div class="sr-only" role="rowgroup">
      <div role="row"><span role="columnheader">Schedule</span><span role="columnheader">Last run</span></div>
    </div>
    <div role="rowgroup">
      {#each rows as s (s.id)}
        <div
          class="relative grid grid-cols-[minmax(0,1fr)_auto] gap-x-4 gap-y-1 border-b border-rule px-2 py-3.5 hover:bg-sunk has-[a:focus-visible]:outline-2 has-[a:focus-visible]:-outline-offset-2 has-[a:focus-visible]:outline-olive"
          role="row"
          data-testid="schedules-row"
        >
          <div class="min-w-0" role="cell">
            <a
              class="text-base font-semibold text-ink no-underline [overflow-wrap:anywhere] after:absolute after:inset-0 focus-visible:outline-none"
              href={schedulePage(s.id)}>{s.name}</a
            >
            <p class="m-0 mt-0.5 text-md text-soft">{cadenceWords(s.cron, s.zone)}</p>
            {#if s.state === 'paused'}
              <p class="m-0 mt-0.5 line-clamp-2 text-md text-gold-t" data-testid="schedules-paused">
                {pausedLine(s.paused_reason)}
              </p>
            {:else if s.next_run_at}
              <p class="m-0 mt-0.5 text-md text-soft">Next {whenIn(s.next_run_at, s.zone)}</p>
            {/if}
          </div>
          <div class="text-right text-md text-soft" role="cell">
            {#if s.last_run}
              {#if s.last_run.job_id && s.last_run.outcome === 'started'}
                <a class="relative z-[1] text-soft hover:text-ink {link}" href={jobPage(s.last_run.job_id)}
                  >{outcomeWord(s.last_run)} {whenIn(s.last_run.fired_at, s.zone)}</a
                >
              {:else}
                <span class={s.last_run.outcome === 'failed' ? 'text-rust' : ''}>{outcomeWord(s.last_run)}</span>
              {/if}
            {:else}
              <span>Not run yet</span>
            {/if}
          </div>
        </div>
      {/each}
    </div>
  </div>
{/if}
