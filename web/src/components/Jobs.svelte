<script lang="ts">
  // The jobs page: the caller's jobs, newest first, filtered by what they
  // need. A row links to the job's session page; there is no job detail
  // route. Polled, and a store read, so no sandbox is touched.
  //
  // Below `md` the page is a single line of limit and Start, a filter row
  // that does not wrap, and rows of four lines or fewer: the goal, the
  // state and kind with the time, and how it came out.
  //
  // @decision(DL-060) @decision(DL-057) @decision(DL-018) @decision(DL-106)
  import { untrack } from 'svelte';
  import { api, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import { duration, jobMark, jobWord, kindName } from '../lib/format';
  import {
    FILTERS,
    type Show,
    goalLine,
    jobAge,
    outcomeOf,
    searchFor,
    showFrom,
    statesFor,
    UNFINISHED,
  } from '../lib/joblist';
  import Icon from './Icon.svelte';
  import LimitMeter from './LimitMeter.svelte';
  import Problem from './Problem.svelte';
  import StateMark from './StateMark.svelte';
  import { pageTitle, primary, secondary } from './ui';
  import { PageSize, appendPage, merged, olderLabel } from '../lib/paging';

  // Read at initialisation: there is no location while Astro renders
  // the static HTML.
  let show = $state<Show>(typeof location === 'undefined' ? 'all' : showFrom(location.search));

  // One page of a filter's jobs, newest first, tagged with the filter
  // it answered. @decision(DL-060)
  async function listed(which: Show, signal?: AbortSignal, before?: string) {
    const state = statesFor(which);
    const body = await unwrap(
      await api.GET('/api/v1/jobs', {
        params: { query: { ...(state ? { state } : {}), limit: PageSize, ...(before ? { before } : {}) } },
        signal,
      }),
    );
    return { show: which, jobs: body?.jobs ?? [], next: body?.next ?? '', total: body?.total ?? 0 };
  }

  // Tagged with the filter it answered, so a switch never draws the
  // previous filter's rows under the new one's name. `show` is read
  // untracked: `choose` is what reloads on a switch, not the resource's
  // effect.
  const list = resource((signal) => listed(untrack(() => show), signal), { every: 10_000 });
  // The unfinished jobs, whatever the filter: they are what the job
  // limit counts, and the ones waiting on you are among them.
  //
  // @decision(DL-060)
  const inProgress = resource(
    async (signal) => {
      const body = await unwrap(
        await api.GET('/api/v1/jobs', { params: { query: { state: UNFINISHED.join(','), limit: 200 } }, signal }),
      );
      return { jobs: body?.jobs ?? [], total: body?.total ?? 0 };
    },
    { every: 10_000 },
  );
  // The limit, read once: it changes only when an admin sets it.
  const me = resource(async (signal) => unwrap(await api.GET('/api/v1/me', { signal })));
  const jobLimit = $derived(me.data?.quota?.max_concurrent_jobs ?? 0);

  const first = $derived(list.data?.show === show ? list.data : undefined);

  // Older pages of the filter on screen, read once on request. Tagged
  // with their filter like the first page, and dropped on a switch.
  //
  // @decision(DL-060)
  type Job = NonNullable<typeof list.data>['jobs'][number];
  let older = $state<{ show: Show; jobs: Job[]; next: string } | undefined>(undefined);
  let olderBusy = $state(false);
  let olderError = $state<unknown>(undefined);
  // Bumped by a filter switch, so a read that started before one is
  // dropped even if the reader has switched back since.
  let generation = 0;
  const appended = $derived(older?.show === show ? older : undefined);
  const olderCursor = $derived(appended ? appended.next : (first?.next ?? ''));
  const rows = $derived(first ? merged(first.jobs, appended?.jobs ?? []) : undefined);
  async function showOlder(): Promise<void> {
    if (olderBusy || !olderCursor) return;
    const which = show;
    const mine = generation;
    olderBusy = true;
    olderError = undefined;
    try {
      const page = await listed(which, undefined, olderCursor);
      if (mine !== generation) return;
      older = { show: which, jobs: appendPage(appended?.jobs ?? [], page.jobs), next: page.next };
    } catch (err) {
      if (mine === generation) olderError = err;
    } finally {
      if (mine === generation) olderBusy = false;
    }
  }
  const needsYou = $derived(
    (inProgress.data?.jobs ?? []).filter((j) => j.state === 'blocked' || j.state === 'steered').length,
  );

  function choose(next: Show) {
    if (next === show) return;
    show = next;
    older = undefined;
    generation++;
    olderBusy = false;
    olderError = undefined;
    history.replaceState(null, '', `${location.pathname}${searchFor(next)}`);
    void list.refresh();
  }

  let now = $state(new Date());
  $effect(() => {
    const t = setInterval(() => (now = new Date()), 30_000);
    return () => clearInterval(t);
  });

  const empty: Record<Show, string> = {
    all: 'No jobs yet.',
    active: 'Nothing is running.',
    'needs-you': 'Nothing is waiting on you.',
    finished: 'No job has finished yet.',
  };

</script>

<!-- The limit and Start share a line on a phone, whose header already
     names the page. @decision(DL-106) -->
<div class="max-md:mb-4 max-md:flex max-md:items-center max-md:justify-between max-md:gap-3">
  <div class="mb-2.5 flex flex-wrap items-center justify-between gap-x-6 gap-y-3 max-md:order-2 max-md:mb-0">
    <h1 class="{pageTitle} max-md:sr-only">Jobs</h1>
    <a class={primary} href="/jobs/new/">Start a job</a>
  </div>

  <!-- The job limit, as the sessions page's summary line draws it.
       @decision(DL-060) -->
  <div class="mb-5 text-md text-soft max-md:order-1 max-md:mb-0 max-md:text-sm">
    {#if inProgress.data}
      <LimitMeter used={inProgress.data.total} limit={jobLimit} noun="jobs in progress" testid="limit-jobs" />
    {/if}
  </div>
</div>

<div
  class="mb-5 flex flex-wrap gap-1.5 max-md:-mx-4 max-md:mb-3 max-md:flex-nowrap max-md:overflow-x-auto max-md:px-4 max-md:pb-1"
  role="group"
  aria-label="Show"
>
  {#each FILTERS as f (f.show)}
    <button
      type="button"
      data-testid={`jobs-show-${f.show}`}
      aria-pressed={show === f.show}
      class="cursor-pointer rounded-ctl border px-3 py-1 text-md whitespace-nowrap max-md:shrink-0 focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2 {show ===
      f.show
        ? 'border-olive bg-raised font-medium text-ink'
        : 'border-rule-2 text-soft hover:bg-raised hover:text-ink'}"
      onclick={() => choose(f.show)}
    >
      {f.label}{#if f.show === 'needs-you' && needsYou > 0}<span
          class="ml-1.5 font-semibold text-gold-t tabular-nums"
          data-testid="jobs-needs-you-count">{needsYou}</span
        >{/if}
    </button>
  {/each}
</div>

{#if list.error}
  <div class="mb-5"><Problem error={list.error} what="your jobs" /></div>
{/if}

{#if rows === undefined}
  {#if !list.error}<p class="text-soft">Loading…</p>{/if}
{:else if rows.length === 0}
  <p class="max-w-[62ch] text-soft" data-testid="jobs-empty">{empty[show]}</p>
{:else}
  <!-- The session list's row treatment: the state in its own column,
       the kind in words. @decision(DL-060) @decision(DL-056) -->
  <div class="border-t border-rule" role="table" aria-label="Jobs">
    <div role="rowgroup">
      <div
        class="grid grid-cols-[124px_minmax(0,1fr)_96px] gap-x-5 border-b border-rule px-2 py-2 text-sm text-soft max-md:sr-only"
        role="row"
      >
        <span role="columnheader">State</span>
        <span role="columnheader">Job</span>
        <span class="text-right" role="columnheader">Running for</span>
      </div>
    </div>
    <div role="rowgroup">
      {#each rows as job (job.id)}
        {@const outcome = outcomeOf(job)}
        <div
          class="relative grid grid-cols-[minmax(0,1fr)_auto] items-start gap-x-3 gap-y-1 border-b border-rule px-2 pt-3.5 pb-4 max-md:py-3 hover:bg-raised has-[a:focus-visible]:outline-2 has-[a:focus-visible]:-outline-offset-2 has-[a:focus-visible]:outline-olive md:grid-cols-[124px_minmax(0,1fr)_96px] md:gap-x-5 md:gap-y-0"
          role="row"
          data-testid="jobs-row"
        >
          <span class="pt-0.5 text-md max-md:col-start-1 max-md:row-start-2 max-md:pt-0 max-md:text-sm" role="cell">
            <StateMark mark={jobMark(job.state, job.partial)} word={jobWord(job.state, job.partial)} /><span
              class="text-soft before:mx-2 before:content-['·'] md:hidden">{kindName(job.kind)} job</span
            >
          </span>
          <div class="min-w-0 max-md:contents" role="cell">
            <a
              class="text-base font-semibold text-ink no-underline after:absolute after:inset-0 focus-visible:outline-none max-md:col-span-2 max-md:row-start-1 max-md:line-clamp-2"
              href={`/session/?id=${encodeURIComponent(job.session_id)}`}>{goalLine(job.goal) || job.id}</a
            >
            <p class="m-0 mt-0.5 text-md text-soft max-md:hidden">{kindName(job.kind)} job</p>
            {#if outcome.kind === 'link'}
              <!-- Above the row's own stretched link, so it opens the
                   pull request rather than the session. -->
              <a
                class="relative z-[1] mt-1 flex max-w-full items-center gap-1 font-mono text-md text-soft hover:text-ink max-md:col-span-2 max-md:row-start-3 max-md:mt-0 max-md:font-sans max-md:text-sm"
                href={outcome.href}
                target="_blank"
                rel="noreferrer noopener"
                ><span class="truncate max-md:hidden">{outcome.text}</span><span class="truncate md:hidden">{outcome.short}</span><Icon name="external" class="size-3.5 shrink-0" /></a
              >
            {:else if outcome.kind === 'note'}
              <p
                class="m-0 mt-1 line-clamp-2 max-w-[70ch] text-md max-md:col-span-2 max-md:row-start-3 max-md:mt-0 max-md:text-sm {outcome.tone ===
                'fail'
                  ? 'text-rust'
                  : 'text-soft'}"
              >
                {outcome.text}
              </p>
            {/if}
          </div>
          <span
            class="pt-0.5 text-right text-md text-soft tabular-nums max-md:col-start-2 max-md:row-start-2 max-md:pt-0 max-md:text-sm"
            role="cell">{duration(jobAge(job, now))}</span
          >
        </div>
      {/each}
    </div>
  </div>
{/if}

<!-- Older jobs of this filter on request, read once. @decision(DL-060) -->
{#if rows && olderCursor}
  <div class="mt-5">
    <button type="button" class={secondary} disabled={olderBusy} onclick={showOlder} data-testid="jobs-older">
      {olderBusy ? 'Loading…' : olderLabel(first?.total, rows.length)}
    </button>
  </div>
{/if}
{#if olderError}
  <div class="mt-3"><Problem error={olderError} what="older jobs" /></div>
{/if}
