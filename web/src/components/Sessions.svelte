<script lang="ts">
  // The sessions list: the landing page, and the one page that must be
  // able to render a signed-out state rather than redirect. It probes
  // /me through `probe` for exactly that reason.
  //
  // @decision(DL-055) @decision(DL-018) @decision(DL-060) @decision(DL-106)
  import { ApiError, api, probe, loginURL, unwrap } from '../lib/api/client';
  // Aliased: `Problem` is also the error component imported below.
  import type { Problem as ProblemBody } from '../lib/api/client';
  import { sessionsStream } from '../lib/api/live';
  import { resource } from '../lib/resource.svelte';
  import type { Session } from '../lib/format';
  import { byLiveThenRecent } from '../lib/format';
  import { limitUse } from '../lib/joblist';
  import { PageSize, appendPage, merged, olderLabel, sessionsCursor } from '../lib/paging';
  import LimitMeter from './LimitMeter.svelte';
  import SessionEntry from './SessionEntry.svelte';
  import Problem from './Problem.svelte';
  import Logo from './Logo.svelte';
  import StateMark from './StateMark.svelte';
  import { link, pageTitle, primary, secondary } from './ui';

  type Me = { quota?: { max_concurrent_sessions?: number; max_concurrent_jobs?: number } };

  let me = $state<Me | null>(null);
  let signedOut = $state(false);
  let checked = $state(false);
  // A probe that failed for any reason other than "not signed in".
  // Without this the page renders its signed-in layout with nothing in
  // it: the usage gate never opens, the stream never starts, and
  // nothing tells the viewer why.
  let probeError = $state<unknown>(undefined);
  let sessions = $state<Session[]>([]);
  let streamError = $state<unknown>(undefined);
  let connected = $state(true);
  let seen = $state(false);

  // Ages advance on their own. This is a clock, not a request: nothing
  // here touches a sandbox.
  let now = $state(new Date());
  $effect(() => {
    const t = setInterval(() => (now = new Date()), 30_000);
    return () => clearInterval(t);
  });

  $effect(() => {
    const ctl = new AbortController();
    void probe
      .GET('/api/v1/me', { signal: ctl.signal })
      .then(({ data, error, response }) => {
        if (ctl.signal.aborted) return;
        if (response.status === 401) {
          signedOut = true;
        } else if (response.ok) {
          me = data as Me;
        } else {
          probeError = new ApiError(response.status, error as ProblemBody | undefined);
        }
        checked = true;
      })
      .catch((err) => {
        if (ctl.signal.aborted) return;
        probeError = err;
        checked = true;
      });
    return () => ctl.abort();
  });

  // Gated on knowing there is somebody signed in. Without the gate this
  // fires on the signed-out landing page, 401s, and the funnel redirects
  // to /login — so the signed-out state below could never be seen.
  const usage = resource(
    async (signal) => unwrap(await api.GET('/api/v1/me/usage', { signal })),
    { every: 60_000, when: () => me !== null },
  );

  // The stream starts only once we know there is somebody to stream to.
  $effect(() => {
    if (!me) return;
    const ctl = new AbortController();
    void (async () => {
      for await (const ev of sessionsStream({
        signal: ctl.signal,
        onConnected: (c) => (connected = c),
      })) {
        sessions = ev.sessions ?? [];
        seen = true;
        streamError = undefined;
      }
    })().catch((err) => {
      if (!ctl.signal.aborted) streamError = err;
    });
    return () => ctl.abort();
  });

  // Older pages the reader asked for, read once and appended below the
  // streamed ones. `olderNext` is the cursor after the last one read:
  // undefined before any, "" once nothing older is left.
  //
  // @decision(DL-060)
  let older = $state<Session[]>([]);
  let olderNext = $state<string | undefined>(undefined);
  let olderTotal = $state<number | undefined>(undefined);
  let olderBusy = $state(false);
  let olderError = $state<unknown>(undefined);
  const olderCursor = $derived(olderNext ?? sessionsCursor(sessions));
  async function showOlder(): Promise<void> {
    if (olderBusy || !olderCursor) return;
    olderBusy = true;
    olderError = undefined;
    try {
      const page = await unwrap(
        await api.GET('/api/v1/sessions', { params: { query: { before: olderCursor, limit: PageSize } } }),
      );
      older = appendPage(older, (page?.sessions ?? []) as Session[]);
      olderNext = page?.next ?? '';
      olderTotal = page?.total;
    } catch (err) {
      olderError = err;
    } finally {
      olderBusy = false;
    }
  }

  // A copy: sort() mutates, and mutating the array the stream assigns
  // would reorder it under the next event rather than re-deriving.
  const shown = $derived(merged(sessions, older));
  const ordered = $derived([...shown].sort(byLiveThenRecent));

  // What each concurrency limit counts, and the limits themselves.
  //
  // @decision(DL-060)
  const use = $derived(limitUse(sessions));
  const sessionLimit = $derived(me?.quota?.max_concurrent_sessions ?? 0);
  const jobLimit = $derived(me?.quota?.max_concurrent_jobs ?? 0);

  const since = $derived.by(() => {
    const iso = usage.data?.since;
    if (!iso) return '';
    const d = new Date(iso);
    return Number.isNaN(d.getTime())
      ? ''
      : `Since ${d.toLocaleDateString(undefined, { day: 'numeric', month: 'long' })}`;
  });
</script>


{#if signedOut}
  <!-- Signed-out page, centred as one column around the mark. The logo is
       decorative: the heading names the product.
       @decision(DL-056) -->
  <div class="flex flex-col items-center py-10 text-center">
    <Logo size={72} class="mb-6 text-faint" />
    <h1 class="{pageTitle} mb-3 max-w-[40ch]">Sign in to see your sessions</h1>
    <p class="m-0 max-w-[62ch] text-soft">
      spov keeps your sessions, usage and credentials behind your account.
    </p>
    <p class="mt-5">
      <a class={primary} href={typeof location === 'undefined' ? '/login' : loginURL(location)}>Sign in</a>
    </p>
  </div>
{:else if probeError}
  <Problem error={probeError} what="your account" />
{:else if checked}
  <div class="mb-2.5 flex flex-wrap items-center justify-between gap-x-6 gap-y-3 max-md:mb-3">
    <!-- The header's menu button already says where a phone is, so the
         heading is for a screen reader there. @decision(DL-106) -->
    <h1 class="{pageTitle} max-md:sr-only">Sessions</h1>
    <!-- The two launch forms' entry points besides the empty state.
         @decision(DL-061) -->
    <div class="flex flex-wrap gap-2 max-md:grid max-md:w-full max-md:grid-cols-2">
      <a class={secondary} href="/jobs/new/">Start a job</a>
      <a class={primary} href="/sessions/new/">Start a session</a>
    </div>
  </div>

  <!-- One summary line in place of the metrics row: both concurrency
       limits, then the month's sessions and sandbox hours. A job's session
       counts as its job, not as a session. Cost lives on the usage page
       and beside the sandbox, never as a total here.
       @decision(DL-060) @decision(DL-116) -->
  <div class="mb-7 flex flex-wrap items-center gap-x-7 gap-y-1.5 text-md text-soft max-md:mb-4 max-md:gap-x-5 max-md:text-sm">
    <LimitMeter used={use.sessions} limit={sessionLimit} noun="sessions" testid="limit-sessions" />
    <LimitMeter used={use.jobs} limit={jobLimit} noun="jobs" testid="limit-jobs" />
    {#if usage.data}
      <span class="max-md:hidden"
        >{since ? `${since}: ` : ''}<b class="font-semibold text-ink tabular-nums">{usage.data.sessions}</b>
        {usage.data.sessions === 1 ? 'session' : 'sessions'},
        <b class="font-semibold text-ink tabular-nums">{usage.data.running_hours}</b> sandbox hours</span
      >
    {/if}
  </div>

  {#if streamError}
    <div class="mb-5"><Problem error={streamError} what="your sessions" /></div>
  {:else if !connected}
    <p class="mb-5"><StateMark class="text-md" mark="wait" word="Reconnecting, so this list may be behind" /></p>
  {/if}

  {#if !seen}
    <p class="text-soft">Loading your sessions.</p>
  {:else if sessions.length === 0}
    <p class="max-w-[62ch] text-soft">
      No sessions yet. <a class="{link} text-ink" href="/sessions/new/">Start a session</a> or
      <a class="{link} text-ink" href="/jobs/new/">a job</a>, or run
      <code class="bg-sunk px-1 font-mono text-md text-ink">spov fly</code> from a terminal.
    </p>
  {:else}
    <!-- A table: columns let the eye run down one fact.
         @decision(DL-060) @decision(DL-116) -->
    <div class="border-t border-rule" role="table" aria-label="Sessions">
      <div role="rowgroup">
        <div
          class="grid grid-cols-[124px_minmax(0,1fr)_120px_132px] gap-x-5 border-b border-rule px-2 py-2 text-sm text-soft max-md:sr-only"
          role="row"
        >
          <span role="columnheader">State</span>
          <span role="columnheader">Session</span>
          <span role="columnheader">Started</span>
          <span role="columnheader">Running for</span>
        </div>
      </div>
      <div role="rowgroup">
        {#each ordered as session (session.id)}
          <SessionEntry {session} {now} />
        {/each}
      </div>
    </div>
    <!-- Older sessions on request, read once rather than streamed.
         @decision(DL-060) -->
    {#if olderCursor}
      <div class="mt-5">
        <button type="button" class={secondary} disabled={olderBusy} onclick={showOlder} data-testid="sessions-older">
          {olderBusy ? 'Loading…' : olderLabel(olderTotal, shown.length)}
        </button>
      </div>
    {/if}
    {#if olderError}
      <div class="mt-3"><Problem error={olderError} what="older sessions" /></div>
    {/if}
  {/if}
{/if}
