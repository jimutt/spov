<script lang="ts">
  // One schedule: its cadence and job, its next runs as the server
  // computes them, its runs with each job linked, and Run now, Pause or
  // Resume, Edit and Delete, the irreversible two behind a second press.
  //
  // @decision(DL-195) @decision(DL-057)
  import { ApiError, api, nav, unwrap, type Problem as ProblemBody } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import { kindName } from '../lib/format';
  import { cadenceWords, jobPage, outcomeWord, pausedLine, whenIn } from '../lib/schedule';
  import ConfirmAct from './ConfirmAct.svelte';
  import Problem from './Problem.svelte';
  import { link, pageTitle, secondary } from './ui';

  const ref = typeof location === 'undefined' ? '' : (new URLSearchParams(location.search).get('ref') ?? '');
  const sched = resource(
    async (signal) => unwrap(await api.GET('/api/v1/schedules/{ref}', { params: { path: { ref } }, signal })),
    { when: () => ref !== '' },
  );
  let failure = $state<unknown>(undefined);
  let started = $state('');
  let busy = $state(false);
  const s = $derived(sched.data);

  async function act(fn: () => Promise<void>) {
    failure = undefined;
    busy = true;
    try {
      await fn();
    } catch (err) {
      failure = err;
    } finally {
      busy = false;
      void sched.refresh();
    }
  }

  const path = () => ({ params: { path: { ref: s!.id } } });
  const runNow = () =>
    act(async () => {
      started = (await unwrap(await api.POST('/api/v1/schedules/{ref}/run', path()))).job_id;
    });
  const setPaused = (paused: boolean) =>
    act(async () => {
      await unwrap(await api.POST(paused ? '/api/v1/schedules/{ref}/pause' : '/api/v1/schedules/{ref}/resume', path()));
    });
  async function remove() {
    failure = undefined;
    busy = true;
    try {
      const r = await api.DELETE('/api/v1/schedules/{ref}', path());
      if (!r.response.ok) throw new ApiError(r.response.status, r.error as ProblemBody | undefined);
      nav.replace('/schedules/');
    } catch (err) {
      failure = err;
      busy = false;
    }
  }
</script>

{#if !ref}
  <p class="text-soft">No schedule named. <a href="/schedules/">Your schedules</a></p>
{:else if sched.error && !s}
  <Problem error={sched.error} what="the schedule" />
{:else if !s}
  <p class="text-soft">Loading…</p>
{:else}
  <div class="max-w-[760px]">
    <p class="m-0 mb-1 text-md"><a class="text-soft" href="/schedules/">Schedules</a></p>
    <h1 class="{pageTitle} mb-1.5 break-words" data-testid="schedule-name">{s.name}</h1>
    <p class="mt-0 mb-5 text-soft">{cadenceWords(s.cron, s.zone)}</p>

    {#if failure}
      <div class="mb-5"><Problem error={failure} what="that" /></div>
    {/if}
    {#if started}
      <p class="mb-5 text-md" data-testid="schedule-started">
        Started a run: <a class={link} href={jobPage(started)}>open its job</a>.
      </p>
    {/if}
    {#if s.state === 'paused'}
      <p class="mb-5 max-w-[64ch] text-md text-gold-t" data-testid="schedule-paused">
        {pausedLine(s.paused_reason)}. It runs again from its next slot once resumed.
      </p>
    {/if}

    <div class="mb-7 flex flex-wrap items-center gap-2.5">
      <ConfirmAct
        label="Run now"
        question="Start a run of this schedule now? It is a job of its own."
        yes="Run now"
        no="Not now"
        tone="safe"
        testid="schedule-run"
        disabled={busy}
        run={runNow}
      />
      {#if s.state === 'paused'}
        <button type="button" class={secondary} disabled={busy} onclick={() => setPaused(false)} data-testid="schedule-resume"
          >Resume</button
        >
      {:else}
        <button type="button" class={secondary} disabled={busy} onclick={() => setPaused(true)} data-testid="schedule-pause"
          >Pause</button
        >
      {/if}
      <a class={secondary} href={`/schedules/edit?ref=${encodeURIComponent(s.id)}`}>Edit</a>
      <ConfirmAct
        label="Delete"
        question="Delete this schedule and its run history? The jobs it started are kept."
        yes="Delete schedule"
        testid="schedule-delete"
        disabled={busy}
        run={remove}
      />
    </div>

    <dl class="mb-8 grid grid-cols-[max-content_minmax(0,1fr)] gap-x-5 gap-y-2 text-md">
      <dt class="text-soft">Job</dt>
      <dd class="m-0">{kindName(s.job.kind)} job on {(s.job.repos ?? []).map((r) => `${r.owner}/${r.name}`).join(', ')}</dd>
      <dt class="text-soft">Goal</dt>
      <dd class="m-0 whitespace-pre-wrap break-words">{s.job.goal}</dd>
      {#if s.job.model}
        <dt class="text-soft">Model</dt>
        <dd class="m-0 break-words font-mono">{s.job.model}</dd>
      {/if}
      {#if s.failures > 0}
        <dt class="text-soft">Failed runs in a row</dt>
        <dd class="m-0 text-rust">{s.failures} of 3 before it pauses</dd>
      {/if}
      {#if s.next_runs.length > 0}
        <dt class="text-soft">Next runs</dt>
        <dd class="m-0" data-testid="schedule-next">{s.next_runs.map((t) => whenIn(t, s.zone)).join(' · ')}</dd>
      {/if}
    </dl>

    <h2 class="mb-2 text-base font-semibold">Runs</h2>
    {#if !s.history?.length}
      <p class="text-soft">Not run yet.</p>
    {:else}
      <div class="border-t border-rule" role="table" aria-label="Runs">
        <div class="sr-only" role="rowgroup">
          <div role="row"><span role="columnheader">When</span><span role="columnheader">Outcome</span></div>
        </div>
        <div role="rowgroup">
          {#each s.history as f, i (i)}
            <div class="grid grid-cols-[minmax(0,9rem)_minmax(0,1fr)] gap-x-4 border-b border-rule px-2 py-2.5 text-md" role="row">
              <span class="text-soft tabular-nums" role="cell">{whenIn(f.slot, s.zone)}</span>
              <span class="min-w-0" role="cell">
                {#if f.job_id && f.outcome === 'started'}
                  <a class={link} href={jobPage(f.job_id)}>{outcomeWord(f)}</a>
                {:else}
                  <span class={f.outcome === 'failed' ? 'text-rust' : ''}>{outcomeWord(f)}</span>
                {/if}
                {#if f.reason && f.reason !== 'already run'}<span class="block break-words text-soft">{f.reason}</span>{/if}
              </span>
            </div>
          {/each}
        </div>
      </div>
    {/if}
  </div>
{/if}
