<script lang="ts">
  // One session: its state, what it was asked for, and its conversation.
  // Reads from the store, not the sandbox.
  //
  // @decision(DL-058) @decision(DL-018) @decision(DL-057) @decision(DL-106)
  import { tick, untrack } from 'svelte';
  import { SvelteSet } from 'svelte/reactivity';
  import { api, unwrap } from '../lib/api/client';
  import { logStream, type LogEvent } from '../lib/api/live';
  import { resource } from '../lib/resource.svelte';
  import {
    GoalLimit,
    clip,
    clock,
    clockBar,
    credentialLine,
    routedCredentialLine,
    routedCredentialTag,
    credentialTag,
    duration,
    age,
    gateMark,
    gateName,
    jobMark,
    jobWord,
    jobRunning,
    kindName,
    lifetime,
    listState,
    money,
    repoHeadline,
    repoNames,
    sentence,
    sessionMark,
    sessionSettling,
    speaker,
    stateWord,
    verdictSummary,
    wallClock,
    envRows,
  } from '../lib/format';
  import {
    CollapseFrom,
    Thought,
    blocksOf,
    buildTimeline,
    condense,
    runSummary,
    noteText,
    seamText,
    shown,
    shownWarnings,
    stopNote,
    timeDividers,
    toolInput,
    trimBlankLines,
  } from '../lib/timeline';
  import type { Conversation } from '../lib/timeline';
  import { Tail, atBottom } from '../lib/tail';
  import { earlier, earlierLabel, held, windowOf, type Start } from '../lib/transcriptwindow';
  import { hasMarkup, render } from '../lib/markdown';
  import {
    STATUS_WORDS,
    changeNounOf,
    deliverableMark,
    fileURL,
    imageSource,
    missingImage,
    pullRequestName,
    pullRequests,
    reports,
    sizeText,
    type Deliverable,
  } from '../lib/deliverables';
  import {
    type Echo,
    type EchoTrack,
    cancel,
    composerShown,
    inboxClosed,
    queuedNote,
    echoNote,
    turnInFlight,
    turnsEnded,
    unseen,
  } from '../lib/steer';
  import {
    CancelQuestion,
    EndQuestion,
    cancellable,
    endable,
    ended,
  } from '../lib/jobcontrols';
  import { askStates, pendingAsks } from '../lib/asks';
  import {
    ResumeWatchEvery,
    followsAResume,
    resumed,
    resumeOffered,
    resumeQuestion,
    watchesForAResume,
  } from '../lib/resume';
  import { currentModel } from '../lib/modelpick';
  import AskCard from './AskCard.svelte';
  import Composer from './Composer.svelte';
  import ConfirmAct from './ConfirmAct.svelte';
  import Icon from './Icon.svelte';
  import InfoToggle from './InfoToggle.svelte';
  import Problem from './Problem.svelte';
  import ResumeComposer from './ResumeComposer.svelte';
  import StateMark from './StateMark.svelte';
  import { primary, secondary, secondarySmall, textButton } from './ui';

  // The session is named by the query string rather than the path.
  // Read at initialisation, not in an $effect: location is not
  // reactive, and there is no location while Astro renders the static
  // HTML — hydration runs this again in the browser.
  const search = typeof location === 'undefined' ? null : location.search;
  const resolved = search !== null;
  const named = search === null ? '' : (new URLSearchParams(search).get('id') ?? '');
  // A reference of `..` survives encodeURIComponent and would resolve a
  // segment away, sending this credentialed request to a path the
  // caller never named.
  const ref = /^\.+$/.test(named) ? '' : named;

  // Raw, not proxied: both are only ever replaced whole, and a deep
  // proxy over a long conversation makes every read of it a trap.
  //
  // @decision(DL-070)
  let conversations = $state.raw<Conversation[]>([]);
  let warnings = $state<string[]>([]);
  let transcriptError = $state<unknown>(undefined);
  let readTranscript = $state(false);
  // @decision(DL-058) @decision(DL-070)
  let activity = $state.raw<LogEvent[]>([]);
  let connected = $state(true);
  // @decision(DL-018)
  let tailEnded = $state(false);
  let tailError = $state<unknown>(undefined);

  // Messages this browser has sent and the session has not said back.
  // Held here, not in the composer: an echo sorts, keys and renders as
  // the message it is about to become, on this transcript.
  //
  // @decision(DL-059)
  let echoes = $state<Echo[]>([]);
  const track: EchoTrack = {
    open: (e: Echo) => {
      echoes = [...echoes, e];
      void followSend();
    },
    sent: (key: string, id: string) =>
      (echoes = echoes.map((e) => (e.key === key ? { ...e, id, sending: false } : e))),
    failed: (key: string) => (echoes = echoes.filter((e) => e.key !== key)),
  };
  // Reconciled by filtering on every render, never removed on arrival:
  // pruning it in an effect would re-dirty its own dependency.
  //
  // @decision(DL-059)
  const echoing = $derived(
    unseen(echoes, conversations, activity).map((e) => ({
      key: e.key,
      id: e.id,
      at: e.at,
      text: e.text,
      note: echoNote(e),
    })),
  );

  let now = $state(new Date());
  $effect(() => {
    const t = setInterval(() => (now = new Date()), 30_000);
    return () => clearInterval(t);
  });

  // Polls while the session is still starting, stops once it settles. A
  // store read, so DL-018's idle-clock rule is not engaged.
  //
  // @decision(DL-017) @decision(DL-018)
  const session = resource(
    async (signal) =>
      unwrap(await api.GET('/api/v1/sessions/{ref}', { params: { path: { ref } }, signal })),
    { every: 5_000, when: () => stillStarting },
  );
  // A $derived boolean, not the session object read directly: an object
  // freshly parsed on every answer would tear the resource's effect
  // down and reload on every round trip.
  //
  // @decision(DL-017) @decision(DL-057)
  const stillStarting = $derived(sessionSettling(session.data?.state, session.error !== undefined));

  // Reads can overlap — mount, a break, and the end of the stream all
  // trigger one — and they finish in whatever order the network gives
  // them. Only the newest may write, or a slow earlier answer overwrites
  // a newer one.
  let reading = 0;

  /** loadTranscript reads the conversation. Called on mount, after a
   *  break, and once when the stream ends — never on a timer. */
  async function loadTranscript(signal: AbortSignal): Promise<void> {
    const mine = ++reading;
    try {
      const body = await unwrap(
        await api.GET('/api/v1/sessions/{ref}/messages', { params: { path: { ref } }, signal }),
      );
      if (signal.aborted || mine !== reading) return;
      conversations = (body?.conversations ?? []) as Conversation[];
      warnings = body?.warnings ?? [];
      transcriptError = undefined;
      readTranscript = true;
    } catch (err) {
      if (!signal.aborted && mine === reading) transcriptError = err;
    }
  }

  $effect(() => {
    if (!ref) return;
    const ctl = new AbortController();
    void loadTranscript(ctl.signal);
    return () => ctl.abort();
  });

  // Batches a burst of replayed frames into one update on the following
  // macrotask, rather than re-sorting and re-grouping per frame.
  //
  // @decision(DL-058)
  let pending: LogEvent[] = [];
  let flush = 0;
  function absorb(ev: LogEvent): void {
    pending.push(ev);
    if (flush) return;
    flush = setTimeout(() => {
      flush = 0;
      activity = [...activity, ...pending];
      pending = [];
    }, 0) as unknown as number;
  }

  // Bumped when a resumed session's log is reopened; the stream starts
  // again after the last position the page holds. @decision(DL-090)
  let streamEpoch = $state(0);
  $effect(() => {
    if (!ref) return;
    void streamEpoch;
    const after = untrack(() => activity.at(-1)?.pos);
    const ctl = new AbortController();
    void (async () => {
      for await (const ev of logStream(ref, {
        signal: ctl.signal,
        after,
        onConnected: (c) => (connected = c),
        onGap: () => {
          // @decision(DL-058)
          void loadTranscript(ctl.signal);
        },
        onEnded: () => {
          // @decision(DL-018)
          tailEnded = true;
          void loadTranscript(ctl.signal);
          // The row is polled only while it is starting, so without
          // this the header keeps saying `running` after the sandbox
          // has gone. @decision(DL-017)
          void session.refresh();
        },
      })) {
        absorb(ev);
      }
    })().catch((err) => {
      // The page keeps whatever it already read, but a tail that broke
      // for good must say so: silence there reads as "still live".
      if (!ctl.signal.aborted) tailError = err;
    });
    return () => {
      ctl.abort();
      clearTimeout(flush);
      flush = 0;
      pending = [];
    };
  });

  /**
   * The conversation and the live stream laid on one timeline, with
   * consecutive structural events grouped into folded runs. Plumbing is
   * dropped and each tool call joined into one line first.
   *
   * @decision(DL-058)
   */
  const blocks = $derived(blocksOf(condense(buildTimeline(conversations, activity, echoing, inboxEnded))));

  // Only a window of the newest blocks is drawn. It slides while the
  // page follows the tail and is held once the reader leaves the end.
  //
  // @decision(DL-070)
  let start = $state<Start>(undefined);
  const windowed = $derived(windowOf(blocks, start));

  // A queued message can no longer be read once the session has ended or
  // its job's run is over: it is then drawn as not processed.
  //
  // @decision(DL-048) @decision(DL-019)
  const inboxEnded = $derived(inboxClosed(s?.state, tailEnded, job?.state));

  // Every ask's status, from the log; a pending one on a session that has
  // ended reads as not answered.
  //
  // @decision(DL-020)
  const asks = $derived(askStates(activity, inboxEnded));
  const asking = $derived(pendingAsks(asks) > 0);
  // The model the session is on: the log's newest selection, else its
  // launch's. @decision(DL-014)
  const model = $derived(currentModel(activity, s?.model));

  // Cancelling a queued message. Whether it was still waiting is read from
  // the log, so the row says "Cancelling…" until the log says either way.
  //
  // @decision(DL-019)
  const cancelling = new SvelteSet<string>();
  let messageCancelFailure = $state<unknown>(undefined);
  async function cancelQueued(id: string) {
    if (cancelling.has(id)) return;
    cancelling.add(id);
    messageCancelFailure = undefined;
    try {
      await cancel(id, (msg) =>
        api.DELETE('/api/v1/sessions/{ref}/messages/{id}', { params: { path: { ref, id: msg } } }),
      );
    } catch (err) {
      cancelling.delete(id);
      messageCancelFailure = err;
    }
  }

  // Whether a turn is running, read out of the log already held rather
  // than asked of anything. Decides whether Interrupt is drawn.
  //
  // @decision(DL-059)
  const inFlight = $derived(turnInFlight(activity));
  const turnsSeenEnded = $derived(turnsEnded(activity));
  const openWarnings = $derived(shownWarnings(warnings, blocks.length));

  // Runs the reader opened, by the run's stable key. Expanding reads
  // only from what the page already holds: no request, and nothing that
  // touches a sandbox.
  const expanded = new SvelteSet<string>();

  /**
   * markdownFor renders one message's text, or null when it has no
   * markup and the plain text will do. Memoised on the source text: the
   * template asks twice per message and the timeline re-renders on
   * every arriving frame.
   *
   * @decision(DL-063)
   */
  let previewOpen = $state(false);
  // Whether the pointer went down on the backdrop rather than inside
  // the panel. See the click handler.
  let backdropPress = false;
  // The element focus returns to when the preview closes, so a reader
  // who opened it from the keyboard is put back where they were rather
  // than at the top of the document.
  let previewOpener: HTMLElement | null = null;
  let previewPanel = $state<HTMLElement | null>(null);
  // The report being read, its body once fetched, and why it could not
  // be. A body is fetched only when its preview opens, from the file
  // endpoint: no job view carries one.
  //
  // @decision(DL-051)
  let previewing = $state<Deliverable | undefined>(undefined);
  let previewBody = $state('');
  let previewFailed = $state('');
  // Counts report opens, so a fetch can tell it is no longer the latest.
  let previewSeq = 0;
  // The report as sanitised HTML, rendered only while the preview is
  // open. Its images resolve to the job's own collected files.
  //
  // @decision(DL-063)
  // Keyed on the job's id, a string: the job object is re-parsed on
  // every poll, and a 10 MiB report must not re-render every 10 s.
  const previewJobID = $derived(job?.id ?? '');
  const reportHTML = $derived(
    previewOpen && previewBody && previewJobID
      ? render(previewBody, imageSource(previewJobID, window.location.origin))
      : null,
  );

  // openReport fetches one report's body and opens the preview on it.
  //
  // @decision(DL-051)
  async function openReport(d: Deliverable, opener: HTMLElement) {
    if (!job || !d.path) return;
    previewing = d;
    previewBody = '';
    previewFailed = '';
    previewOpener = opener;
    previewOpen = true;
    // A body that arrives after another report was opened, or after the
    // preview closed, is dropped.
    const seq = ++previewSeq;
    const current = () => previewOpen && seq === previewSeq;
    try {
      const r = await fetch(new URL(fileURL(job.id, d.path), window.location.origin), { credentials: 'same-origin' });
      if (!r.ok) throw new Error(`the report could not be read (${r.status})`);
      const body = await r.text();
      if (current()) previewBody = body;
    } catch (e) {
      if (current()) previewFailed = e instanceof Error ? e.message : String(e);
    }
  }

  const rendered = new Map<string, string | null>();
  function markdownFor(text: string | undefined): string | null {
    const source = text ?? '';
    if (!source) return null;
    if (rendered.has(source)) return rendered.get(source) ?? null;
    const html = hasMarkup(source) ? render(source) : null;
    rendered.set(source, html);
    return html;
  }

  // One row of the transcript: a single column, so every kind of row
  // caps at the same --log-w. Who spoke is in the layout, and who and
  // when are under each message, shown on hover, focus or tap.
  //
  // @decision(DL-058) @decision(DL-057)
  const row = 'max-w-[var(--log-w)]';
  const revealed = new SvelteSet<string>();
  function toggleDetails(key: string) {
    // A tap that ends a text selection is not a request for details.
    if (typeof window !== 'undefined' && window.getSelection()?.toString()) return;
    if (revealed.has(key)) revealed.delete(key);
    else revealed.add(key);
  }
  const details = (key: string) =>
    `mt-1 text-xs text-faint tabular-nums transition-opacity duration-150 ${
      revealed.has(key) ? 'opacity-100' : 'opacity-0 group-hover:opacity-100 group-focus-within:opacity-100'
    }`;
  // @decision(DL-058)
  // Over the drawn window, so its first block has its quiet time.
  // @decision(DL-058) @decision(DL-070)
  const dividers = $derived(timeDividers(windowed.shown, now));

  // An event's name without the `session.` every one of them starts
  // with. Display only: grouping and keys use the real name.
  //
  // @decision(DL-058)
  function eventName(name: string): string {
    return name.replace(/^session\./, '');
  }

  const s = $derived(session.data);
  const life = $derived(s ? lifetime(s, now) : undefined);

  // The job this session is running, if it is running one. A store
  // read, so polling it does not reset the idle clock.
  //
  // @decision(DL-057) @decision(DL-018)
  // Declared before the resource that sets it, so the `when` gate below
  // does not read a binding in its own initialiser.
  //
  // True only for DL-046's 501 (no supervisor on this deployment): any
  // other non-OK status must not set it, or a transient 502 would stop
  // the resource polling for good.
  let jobsUnsupported = $state(false);
  const jobs = resource(
    async (signal) =>
      unwrap(
        await api
          .GET('/api/v1/jobs', { params: { query: { session: ref } }, signal })
          .then((r) => {
            if (r.response.status === 501) jobsUnsupported = true;
            return r;
          }),
      ),
    { every: 10_000, when: () => askAgain },
  );
  // At most one: the filter is on a session, and a session has at most
  // one job.
  const job = $derived(jobs.data?.jobs?.[0]);

  // Whether asking again could learn anything. `jobs.loaded` covers the
  // ordinary session: a job's session is created with the job in one
  // transaction, so one that answered once with no job will never gain
  // one.
  //
  // @decision(DL-057) @decision(DL-045)
  const askAgain = $derived(
    !jobsUnsupported && (job !== undefined ? jobRunning(job) : !jobs.loaded),
  );

  const jobGates = $derived(job?.gates ?? []);

  // The goal folded to its first lines, the explanations the sidebar
  // keeps behind a control, and which of them the reader opened. Not
  // remembered across a reload.
  //
  // @decision(DL-057)
  const goalClip = $derived(clip(job?.goal ?? '', GoalLimit));
  let goalOpen = $state(false);
  let envOpen = $state(false);
  const credentialNotes = new SvelteSet<string>();

  // The job's inventory, by kind. Bodies are never in it.
  //
  // @decision(DL-051)
  const jobPRs = $derived(pullRequests(job));
  const jobReports = $derived(reports(job));
  // @decision(DL-049)
  const jobClock = $derived(wallClock(job));
  // @decision(DL-057)
  const bar = $derived(s ? clockBar(s, job, now) : null);
  // Cancel job and End session. Each failure is held here, not in the
  // control: a refusal re-reads the page, which can withdraw the control,
  // and the message has to outlive it. The re-read is awaited, so the
  // control stays pending until the page shows the act's outcome.
  //
  // @decision(DL-057)
  let cancelFailure = $state<unknown>(undefined);
  let endFailure = $state<unknown>(undefined);
  async function reread(): Promise<void> {
    await Promise.all([session.refresh(), jobs.refresh()]);
  }
  async function cancelJob(): Promise<void> {
    cancelFailure = undefined;
    try {
      await ended(() => api.DELETE('/api/v1/jobs/{id}', { params: { path: { id: job?.id ?? '' } } }));
    } catch (err) {
      cancelFailure = err;
    } finally {
      await reread();
    }
  }
  // Resume, from this page or from anywhere else. A stopped session that
  // can resume is read again every 30 s, a store read; once it is
  // starting or running again the transcript and the log are reopened.
  //
  // @decision(DL-090) @decision(DL-018)
  let resumeFailure = $state<unknown>(undefined);
  async function resumeSession(): Promise<void> {
    resumeFailure = undefined;
    try {
      await resumed((body) => api.POST('/api/v1/sessions/{ref}/resume', { params: { path: { ref } }, body }));
    } catch (err) {
      resumeFailure = err;
    } finally {
      await reread();
    }
  }
  const watching = $derived(watchesForAResume(s?.state, s?.resume));
  $effect(() => {
    if (!watching) return;
    const t = setInterval(() => void session.refresh(), ResumeWatchEvery);
    return () => clearInterval(t);
  });
  let sawStopped = $state(false);
  $effect(() => {
    if (s?.state === 'stopped' || s?.state === 'failed') sawStopped = true;
  });
  $effect(() => {
    if (!followsAResume(tailEnded, sawStopped, s?.state)) return;
    tailEnded = false;
    sawStopped = false;
    tailError = undefined;
    untrack(() => {
      streamEpoch++;
      const ctl = new AbortController();
      void loadTranscript(ctl.signal);
    });
  });
  async function endSession(): Promise<void> {
    endFailure = undefined;
    try {
      await ended(() => api.DELETE('/api/v1/sessions/{ref}', { params: { path: { ref } } }));
    } catch (err) {
      endFailure = err;
    } finally {
      await reread();
    }
  }

  // The page scrolls in the document rather than in a container, so the
  // scrolling element is the document's.
  //
  // @decision(DL-057)
  const tail = new Tail();
  function scroller(): HTMLElement | null {
    if (typeof document === 'undefined') return null;
    return (document.scrollingElement as HTMLElement | null) ?? document.documentElement;
  }

  // Whether the reader is at the end right now, as measured — not
  // `tail.following`. Content growth moves the end without a scroll
  // event, so a control keyed on the latch alone would go stale until
  // the reader next moved; both the scroll handler and the growth
  // effect below re-measure it.
  //
  // @decision(DL-057)
  let atEnd = $state(true);
  function measure(): void {
    const el = scroller();
    if (el) atEnd = atBottom(el);
  }
  function onScroll(): void {
    const el = scroller();
    if (!el) return;
    tail.onScroll(el);
    atEnd = atBottom(el);
    // @decision(DL-070)
    start = tail.following ? undefined : held(blocks, start);
    anchoring();
  }

  // Scroll anchoring is off while the page follows the tail: a sliding
  // window drops blocks above the reader, and anchoring would move them
  // up by that height in the same frame the page moved them to the end.
  //
  // @decision(DL-070)
  function anchoring(): void {
    if (typeof document !== 'undefined') document.documentElement.style.overflowAnchor = tail.following ? 'none' : '';
  }
  $effect(() => {
    anchoring();
    return () => {
      document.documentElement.style.overflowAnchor = '';
    };
  });

  // Draws one more window above the first block, then scrolls by what
  // that added, so the block the reader was looking at stays put.
  //
  // @decision(DL-070)
  // Anchoring is off meanwhile, so only this correction moves the page.
  async function showEarlier(ev: MouseEvent): Promise<void> {
    // The first block's own row, not its time divider, which goes once
    // the block is no longer first.
    let first = (ev.currentTarget as HTMLElement).parentElement?.nextElementSibling ?? null;
    while (first?.matches('[data-testid="time-divider"]')) first = first.nextElementSibling;
    const before = first?.getBoundingClientRect().top;
    tail.release();
    document.documentElement.style.overflowAnchor = 'none';
    start = earlier(blocks, start);
    await tick();
    const after = first?.getBoundingClientRect().top;
    if (before !== undefined && after !== undefined && after !== before) scroller()?.scrollBy(0, after - before);
    anchoring();
  }
  // Sending is a request to see the answer: the page returns to the end
  // and follows again, wherever the reader had scrolled to.
  //
  // @decision(DL-106) @decision(DL-057)
  async function followSend(): Promise<void> {
    start = undefined;
    await tick();
    requestAnimationFrame(toEnd);
  }
  function toEnd(): void {
    const el = scroller();
    if (!el) return;
    // `auto` rather than the default: a smooth scroll animates over
    // several hundred milliseconds, and every frame of it fires a
    // scroll event from part-way up the page — which releases the latch
    // the jump just re-armed, so the page stops following the moment it
    // arrives. Jumping outright also matches what the control says it
    // does.
    el.scrollTo({ top: tail.resume(el), behavior: 'auto' });
    anchoring();
    measure();
  }
  $effect(() => {
    window.addEventListener('scroll', onScroll, { passive: true });
    return () => window.removeEventListener('scroll', onScroll);
  });
  // The page also follows growth no event announces: a font arriving, an
  // image, the dock changing height. Only while the latch is armed, as
  // with a new block, so a reader up the page is left where they are.
  //
  // @decision(DL-106) @decision(DL-057)
  $effect(() => {
    if (typeof ResizeObserver === 'undefined') return;
    const watch = new ResizeObserver(() => {
      const el = scroller();
      if (!el) return;
      const to = tail.onGrow(el);
      if (to !== undefined) el.scrollTo({ top: to, behavior: 'auto' });
      measure();
    });
    watch.observe(document.body);
    return () => watch.disconnect();
  });
  // Below `lg` the sidebar is a sheet over the page, opened from the
  // title bar. Escape closes it, focus moves in on open and back to the
  // button on close, and the page behind does not scroll.
  //
  // @decision(DL-106)
  let infoOpen = $state(false);
  let infoButton = $state<HTMLElement | null>(null);
  let infoClose = $state<HTMLElement | null>(null);
  // The height of the docked composer, for the control that floats above it.
  let dockH = $state(0);
  let dock = $state<HTMLElement | null>(null);
  $effect(() => {
    if (!dock) return;
    const el = dock;
    const read = () => (dockH = Math.round(el.getBoundingClientRect().height));
    read();
    if (typeof ResizeObserver === 'undefined') return;
    const watch = new ResizeObserver(read);
    watch.observe(el);
    return () => watch.disconnect();
  });
  $effect(() => {
    if (!infoOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') infoOpen = false;
    };
    window.addEventListener('keydown', onKey);
    const restore = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    const frame = requestAnimationFrame(() => infoClose?.focus());
    return () => {
      window.removeEventListener('keydown', onKey);
      document.body.style.overflow = restore;
      cancelAnimationFrame(frame);
      infoButton?.focus();
    };
  });
  // The preview's keyboard and focus behaviour, in one effect. Escape
  // is bound on the window so it works without a click inside first;
  // focus moves into the panel on open and returns to the opening
  // control on close; the page behind is stopped from scrolling.
  //
  // @decision(DL-063)
  $effect(() => {
    if (!previewOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') previewOpen = false;
    };
    window.addEventListener('keydown', onKey);
    const restore = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    // After the panel is in the DOM.
    const frame = requestAnimationFrame(() => previewPanel?.focus());
    return () => {
      window.removeEventListener('keydown', onKey);
      document.body.style.overflow = restore;
      cancelAnimationFrame(frame);
      previewOpener?.focus();
      previewOpener = null;
    };
  });
  // Re-runs whenever the rendered timeline changes, which is what
  // "grew" means here. Reading blocks.length is what registers the
  // dependency; the scroll itself waits a frame so it measures the
  // layout that includes the new rows rather than the one before them.
  //
  // It follows only while the latch is armed — an event arriving while
  // the reader is up the page must not yank them back down.
  $effect(() => {
    void blocks.length;
    const el = scroller();
    if (!el) return;
    const frame = requestAnimationFrame(() => {
      const to = tail.onGrow(el);
      if (to !== undefined) el.scrollTo({ top: to, behavior: 'auto' });
      // Growing moved the end away from a reader who is not following,
      // and that fires no scroll event — so the control's visibility is
      // re-measured here or it goes stale until the reader moves.
      measure();
    });
    return () => cancelAnimationFrame(frame);
  });
</script>

{#if !resolved}
  <p class="text-soft">Loading…</p>
{:else if !ref}
  <p class="text-soft">No session was named. <a class="underline" href="/">Back to the list</a>.</p>
{:else if session.error}
  <Problem error={session.error} what="this session" />
{:else if s}
  <!-- Below `lg` the docked composer is the last thing on the page, so
       the main column's own bottom padding is taken back.
       @decision(DL-106) -->
  <div
    class="grid grid-cols-1 gap-[var(--col-gap)] max-lg:-mb-[var(--main-pad-y)] lg:grid-cols-[minmax(0,1fr)_var(--side-w)]"
    style:--dock-h="{dockH}px"
  >
    <!-- The clock bar, in the page's head from `lg` and in the details
         sheet below it. @decision(DL-057) @decision(DL-106) -->
    {#snippet clockStrip(klass: string)}
      {#if bar}
        <div class={klass} role="img" aria-label={bar.label}>
          <div class="relative h-1 bg-sunk">
            <span
              class="absolute inset-y-0 left-0 {bar.warn || !bar.now ? 'bg-gold' : 'bg-olive'}"
              style={`width:${(bar.fraction * 100).toFixed(1)}%`}
            ></span>
            {#if bar.now}
              <span
                class="absolute -top-1 -bottom-1 w-0.5 -translate-x-1/2 bg-ink"
                style={`left:${(bar.fraction * 100).toFixed(1)}%`}
              ></span>
            {/if}
          </div>
          <div class="relative mt-1.5 flex justify-between gap-4 text-sm text-soft tabular-nums" aria-hidden="true">
            <span>{bar.start}</span>
            <!-- Hidden near either end, where it would sit on top of
                 the end captions. -->
            {#if bar.now && bar.fraction > 0.15 && bar.fraction < 0.75}
              <span
                class="absolute -translate-x-1/2 text-ink max-sm:hidden"
                style={`left:${(bar.fraction * 100).toFixed(1)}%`}>{bar.now}</span
              >
            {/if}
            <span class={bar.warn ? 'font-semibold text-gold-t' : ''}
              >{bar.endLabel} <b class="font-semibold {bar.warn ? 'text-gold-t' : 'text-ink'}">{bar.endValue}</b></span
            >
          </div>
        </div>
      {/if}
    {/snippet}
    <!-- Ordered rather than laid out in source order: below `lg` the
         grid is one column and the sidebar is a sheet over the
         transcript, so it is ordered first.
         @decision(DL-057) @decision(DL-106) -->
    <div class="order-2 min-w-0 lg:order-1">
      <!-- Below `lg`: one slim bar that stays on screen, with the way back,
           the title, the state and the button for everything else. The
           page's own header has scrolled away by then.
           @decision(DL-106) -->
      {#if true}
        {@const st = listState(s)}
        {@const lead = repoHeadline(s)}
        <div
          class="sticky top-0 z-10 -mx-[var(--page-pad-x)] -mt-[var(--main-pad-y)] mb-4 flex items-center gap-1 border-b border-rule bg-raised px-1.5 py-1.5 lg:hidden"
          data-testid="session-bar"
        >
          <a
            href="/"
            aria-label="Back to the sessions"
            class="grid size-10 shrink-0 place-items-center rounded-ctl text-ink no-underline focus-visible:outline-2 focus-visible:outline-olive focus-visible:-outline-offset-2"
          >
            <Icon name="back" class="size-5" />
          </a>
          <div class="min-w-0 flex-1">
            <p class="m-0 truncate text-base leading-tight font-semibold">
              {#if lead}<span class="font-normal text-soft">{lead.owner}</span>{lead.name}{#if lead.more > 0}<span
                    class="ml-1.5 text-sm font-normal text-soft">+{lead.more}</span
                  >{/if}{:else}{s.name || s.id}{/if}
            </p>
            <p class="m-0 flex min-w-0 items-center gap-x-2 text-sm whitespace-nowrap text-soft">
              <StateMark mark={st.mark} word={st.word} />
              {#if st.why}<span class="truncate">{st.why}</span>
              {:else}<span class="tabular-nums">{duration(age(s, now))}</span>{/if}
              {#if lead}<span class="min-w-0 truncate">{s.name || s.id}</span>{/if}
            </p>
          </div>
          <button
            type="button"
            bind:this={infoButton}
            class="grid size-10 shrink-0 cursor-pointer place-items-center rounded-ctl border-0 bg-transparent text-ink focus-visible:outline-2 focus-visible:outline-olive focus-visible:-outline-offset-2"
            aria-label="Session details"
            aria-expanded={infoOpen}
            aria-controls="session-details"
            data-testid="session-details-open"
            onclick={() => (infoOpen = !infoOpen)}
          >
            <Icon name="info" class="size-5" />
          </button>
        </div>
      {/if}

      <!-- The title, one facts line, and the clock bar.
           @decision(DL-057) -->
      <div class="mb-7 max-lg:hidden">
        <h1 class="m-0 mb-1.5 text-xl font-semibold tracking-[-0.01em] [overflow-wrap:anywhere]">
          {s.name || s.id}
        </h1>
        <div class="flex flex-wrap items-center gap-x-[18px] gap-y-1 text-md text-soft">
          <StateMark
            mark={sessionMark(s.state)}
            word={s.state === 'running'
              ? `Running for ${duration(age(s, now))}`
              : s.state === 'starting'
                ? 'Starting'
                : `${stateWord(s.state)} after ${duration(age(s, now))}`}
          />
          {#each repoNames(s) as repo (repo)}
            <span class="font-mono text-ink">{repo}</span>
          {/each}
          {#if s.sandbox_id}
            <span>Sandbox <span class="font-mono text-ink">{s.sandbox_id}</span></span>
          {/if}
        </div>
        {@render clockStrip('mt-4 max-w-[72ch]')}
      </div>

      {#if s.error}
        <div class="mb-6"><Problem error={new Error(s.error)} what="this session" /></div>
      {/if}

      {#if transcriptError}
        <div class="mb-6"><Problem error={transcriptError} what="the conversation" /></div>
      {/if}

      <!-- Dropped as soon as anything is on screen: a warning here
           describes the one-off transcript read, not the live stream,
           and must not outlive it. Derived rather than cleared, so it
           returns if a re-read on a gap finds the same thing again.
           @decision(DL-017) -->
      {#each openWarnings as warning (warning)}
        <div class="{row} mb-3">
          <p class="m-0 text-md text-gold-t">{warning}</p>
        </div>
      {/each}

      <!-- @decision(DL-070) -->
      {#if windowed.hidden > 0}
        <div class="{row} mb-6">
          <button type="button" class={secondary} onclick={showEarlier} data-testid="transcript-earlier">
            {earlierLabel(windowed.hidden)}
          </button>
        </div>
      {/if}
      {#each windowed.shown as block (block.key)}
        <!-- A quiet time before the first entry and after a pause.
             @decision(DL-058) -->
        {#if dividers.has(block.key)}
          <div class="{row} my-6 flex items-center gap-3 text-xs text-faint tabular-nums" data-testid="time-divider">
            <span class="h-px flex-1 bg-rule" aria-hidden="true"></span>
            <span>{dividers.get(block.key)}</span>
            <span class="h-px flex-1 bg-rule" aria-hidden="true"></span>
          </div>
        {/if}
        {#if block.kind === 'message'}
          {@const synthetic = block.entry.role === 'synthetic'}
          {@const mine = block.entry.role === 'user'}
          <!-- Only a SYNTHETIC's text is read as the stop marker, or a
               human typing it or the agent quoting it would end a run
               that never ended. @decision(DL-058) @decision(DL-048) -->
          {@const stopped = synthetic ? stopNote(block.entry.text) : null}
          <!-- Who spoke is the layout: yours on the right, the agent's
               plain, spov's tagged and quieter, never mistaken for the
               agent's. Who and when sit under it, shown on hover, focus
               or tap; a tap is a pointer's stand-in for hover, and the
               text stays in the document for a screen reader.
               @decision(DL-058) @decision(DL-057) -->
          <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
          <div
            class="group {row} mb-5 flex flex-col {mine ? 'items-end' : 'items-start'}"
            data-testid="message"
            onclick={() => toggleDetails(block.key)}
          >
            <!-- A message on its way is the same bubble, faded; it comes
                 into full view as the transcript records it, keyed as
                 one element. @decision(DL-059) -->
            <div
              class="min-w-0 transition-opacity duration-500 ease-out motion-reduce:transition-none {mine ? 'max-w-[85%] rounded-ctl bg-raised px-3.5 py-2.5' : 'w-full'} {block
                .entry.pending
                ? 'opacity-55'
                : 'opacity-100'}"
              title={block.entry.pending || undefined}
            >
              {#if synthetic}
                <span class="mb-0.5 block text-xs font-medium tracking-wide text-faint" data-testid="spov-tag">spov</span>
              {/if}
              {#if stopped}
                <!-- The marker's text must not be reworded to read better
                     — the supervisor matches on it. An unparseable payload
                     falls through to the raw line rather than being
                     hidden. @decision(DL-058) @decision(DL-048) -->
                <p class="m-0 text-md text-soft">{sentence(stopped)}</p>
              {:else if markdownFor(block.entry.text)}
                <!-- The HTML is sanitised (lib/markdown.ts) — one of two
                     places on the page where Svelte's escaping is
                     deliberately bypassed. @decision(DL-063) -->
                <!-- eslint-disable-next-line svelte/no-at-html-tags -->
                <div class={`md-body md-message ${synthetic ? 'text-md text-soft' : 'text-read'}`}>
                  {@html markdownFor(block.entry.text)}
                </div>
              {:else}
                <!-- Blank lines at either end are dropped: kept by
                     pre-wrap, they draw as an empty band under the words.
                     @decision(DL-058) -->
                <p class={`m-0 whitespace-pre-wrap ${synthetic ? 'text-md text-soft' : 'text-read'}`}>
                  {trimBlankLines(block.entry.text)}
                </p>
              {/if}
              {#if block.entry.pending}
                <!-- An accepted message the recorder hasn't stored yet
                     stays on screen rather than vanishing on answer — a
                     recorder that attached late (DL-017) stores nothing.
                     Said to a screen reader only; the fade says it to the
                     eye. @decision(DL-059) -->
                <span class="sr-only" data-testid="echo-note">{block.entry.pending}</span>
              {/if}
              <!-- A queued message waits below the log until the agent
                   reads it, and can be cancelled until then. Processed
                   needs no word. @decision(DL-019) @decision(DL-048) -->
              {#if block.entry.status === 'queued' && block.entry.id}
                {@const id = block.entry.id}
                <span class="mt-1 flex flex-wrap items-center gap-x-3 text-sm text-faint" data-testid="message-queued">
                  {#if cancelling.has(id)}
                    Cancelling…
                  {:else}
                    {queuedNote(block.entry.delivery, block.entry.after)}
                    {#if composerShown(s.state, tailEnded)}
                      <button type="button" class={textButton} data-testid="message-cancel" onclick={() => cancelQueued(id)}
                        >Cancel</button
                      >
                    {/if}
                  {/if}
                </span>
              {:else if block.entry.status === 'cancelled'}
                <span class="mt-1 block text-sm text-faint" data-testid="message-cancelled">Cancelled before the agent read it.</span>
              {:else if block.entry.status === 'unprocessed'}
                <span class="mt-1 block text-sm text-gold-t" data-testid="message-unprocessed"
                  >Not processed: the run ended before the agent read it.</span
                >
              {/if}
            </div>
            <div class={details(block.key)} data-testid="message-details">
              {speaker(block.entry.role)}{#if block.entry.at}{' · '}{clock(new Date(block.entry.at).toISOString())}{/if}
            </div>
          </div>
        {:else if block.kind === 'seam'}
          <!-- Where the recorder was not listening. Drawn across the
               column rather than folded into a run, so an incomplete
               transcript is never mistaken for a quiet one. A broken
               rule for a broken record. @decision(DL-058) @decision(DL-057) -->
          <div class="{row} mt-1 mb-6">
            <p class="m-0 min-w-0 border-y border-dashed border-rust py-1.5 text-md text-rust">
              {#if block.entry.at}<span class="mr-2 tabular-nums">{clock(new Date(block.entry.at).toISOString())}</span>{/if}{seamText(
                block.entry,
              )}
            </p>
          </div>
        {:else if block.kind === 'ask'}
          <!-- Where the agent asked, beside the call it holds up.
               @decision(DL-020) -->
          <div class="{row} mb-5" data-testid="ask">
            <AskCard
              {ref}
              ask={block.entry.ask}
              askState={asks.get(block.entry.ask.id) ?? { status: 'pending' }}
              open={composerShown(s.state, tailEnded)}
            />
          </div>
        {:else if block.kind === 'note'}
          <!-- What spov did with the job: one of spov's quiet lines, like
               the stop marker, not a rule across the column.
               @decision(DL-058) -->
          <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
          <div class="group {row} mb-5" onclick={() => toggleDetails(block.key)}>
            <span class="mb-0.5 block text-xs font-medium tracking-wide text-faint">spov</span>
            <p class="m-0 min-w-0 text-md text-soft">{sentence(noteText(block.entry))}</p>
            {#if block.entry.at}
              <div class={details(block.key)}>spov · {clock(new Date(block.entry.at).toISOString())}</div>
            {/if}
          </div>
        {:else}
          {@const showAll = expanded.has(block.key)}
          {@const visible = shown(block.events, showAll)}
          {@const foldable = block.events.length >= CollapseFrom}
          <!-- Machine output, so mono, one line each. A folded run is its
               summary alone; a tool call is one line however many events
               OpenCode sent about it. @decision(DL-057) @decision(DL-058)
               @decision(DL-058) -->
          <div class="{row} -mt-1.5 mb-5">
            <div class="min-w-0">
              {#if foldable}
                <button
                  type="button"
                  aria-expanded={showAll}
                  class={textButton}
                  onclick={() => (showAll ? expanded.delete(block.key) : expanded.add(block.key))}
                >
                  <Icon name="chevron" class="chevron size-3" />
                  {runSummary(block.events)}
                </button>
              {/if}
              {#if visible.length}
                <ul class="m-0 list-none p-0 font-mono text-sm leading-[1.7] text-soft {foldable ? 'mt-1' : ''}">
                  {#each visible as entry (entry.key)}
                    {#if entry.calls}
                      {@const input = toolInput(entry.calls.input)}
                      {@const failed = entry.calls.state === 'failed'}
                      <li class="truncate" title={input ? `${entry.calls.name}: ${input}` : entry.calls.name}>
                        <span class="inline-block min-w-[9ch] pr-2 {failed ? 'text-rust' : 'text-faint'}"
                          >{entry.calls.name || 'tool'}</span
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
          </div>
        {/if}
      {/each}

      <!-- The warnings above already say when nothing was recorded and
           why, so this is only for a session that was recorded and had
           nothing to say. -->
      {#if readTranscript && blocks.length === 0 && openWarnings.length === 0}
        <div class={row}>
          <p class="m-0 text-soft">Nothing has been said in this session yet.</p>
        </div>
      {/if}

      {#if !connected}
        <div class="{row} mt-4">
          <StateMark class="text-md" mark={tailEnded ? 'stop' : 'wait'} word={tailEnded ? 'Recording ended' : 'Not live, reconnecting'} />
        </div>
      {/if}

      {#if tailError}
        <div class="mt-4 max-w-[var(--log-w)]"><Problem error={tailError} what="the activity tail" /></div>
      {/if}

      <!-- At the foot of the transcript rather than fixed to the
           viewport: the "Jump to latest" control below is already
           viewport-anchored there. Whether it is drawn is
           `composerShown`'s, which reads both the row and the stream —
           see lib/steer.ts. @decision(DL-059) -->
      {#if messageCancelFailure}
        <div class="mt-4"><Problem error={messageCancelFailure} what="cancelling that message" /></div>
      {/if}

      <!-- Below `lg` the composer is docked to the foot of the screen, as in
           any chat, and rides above the keyboard. Its height is published
           as --dock-h for "Jump to latest". @decision(DL-106) -->
      <div
        class={composerShown(s.state, tailEnded) || (s.resume && resumeOffered(s.state, s.resume) && !job)
          ? 'max-lg:sticky max-lg:bottom-0 max-lg:z-10 max-lg:-mx-[var(--page-pad-x)] max-lg:border-t max-lg:border-rule max-lg:bg-raised max-lg:px-[var(--page-pad-x)] max-lg:pt-2 max-lg:pb-[calc(0.5rem+env(safe-area-inset-bottom))]'
          : ''}
        bind:this={dock}
        data-testid="dock"
      >
      {#if composerShown(s.state, tailEnded)}
        <Composer
          {ref}
          {inFlight}
          turnsEnded={turnsSeenEnded}
          {asking}
          {model}
          jobKnown={jobs.loaded}
          jobState={job?.state}
          jobId={job?.id}
          onEcho={track}
          onJobChanged={() => void jobs.refresh()}
        />
      {:else if s.resume && resumeOffered(s.state, s.resume) && !job}
        <ResumeComposer {ref} resume={s.resume} onResumed={reread} />
      {/if}
      </div>
    </div>

    <!-- Shown whenever the reader is not AT the end, measured fresh —
         not whenever the latch is released, since content growth moves
         the end without a scroll event. Stays for a finished session
         too: the end no longer moves, but a reader scrolled up still
         has a long way back. @decision(DL-057) -->
    <!-- `sticky bottom-6` inside a zero-height, `top-full` wrapper puts
         it at the foot of the visible transcript column and keeps it
         there as the page scrolls, without pushing the transcript
         around. -->
    {#if !atEnd}
      <div
        class="pointer-events-none fixed bottom-[calc(var(--dock-h,0px)+0.75rem)] left-1/2 z-10 lg:bottom-6 flex w-full max-w-[var(--page-w)] -translate-x-1/2 px-[var(--page-pad-x)] lg:pr-[calc(var(--side-w)+var(--col-gap)+var(--page-pad-x))]"
      >
        <!-- Capped at --log-w and centred inside that, matching the
             transcript's own rows — centring on the grid column
             instead would leave the control right of the text.
             @decision(DL-057) -->
        <div class="w-full max-w-[var(--log-w)] text-center">
          <button
            type="button"
            onclick={toEnd}
            class="{secondarySmall} pointer-events-auto shadow-sm"
          >
            <Icon name="down" class="size-3.5" />
            Jump to latest
          </button>
        </div>
      </div>
    {/if}

    <!-- `self-start` is required: a stretched grid item is as tall as
         the transcript, and a sticky element never moves within its own
         box — so without it, sticky would do nothing. Groups are set
         apart by space, not rules. @decision(DL-057) -->
    <!-- Below `lg` it is the sheet the title bar opens, full screen over
         the page; from `lg` it is the sticky sidebar. One element, so the
         page holds a single copy of every control in it.
         @decision(DL-106) -->
    <aside
      id="session-details"
      class="{infoOpen
        ? 'max-lg:fixed max-lg:inset-0 max-lg:z-30 max-lg:flex max-lg:overflow-y-auto max-lg:bg-paper max-lg:px-4 max-lg:pb-10'
        : 'hidden'} order-1 flex-col gap-7 text-md lg:order-2 lg:flex lg:sticky lg:top-[calc(var(--header-h)+var(--main-pad-y))] lg:max-h-[calc(100vh-var(--header-h)-var(--main-pad-y)-1rem)] lg:self-start lg:overflow-y-auto"
      aria-label="About this session"
    >
      <div
        class="sticky top-0 z-10 -mx-4 -mb-3 flex items-center justify-between gap-3 border-b border-rule bg-raised px-4 py-2 lg:hidden"
      >
        <span class="text-base font-semibold">Session details</span>
        <button type="button" bind:this={infoClose} class={secondary} onclick={() => (infoOpen = false)}>Close</button>
      </div>
      {#if bar}
        <div class="lg:hidden">{@render clockStrip('')}</div>
      {/if}
      <!-- Repeats the heading: on a long transcript it is far up the
           page, and a scrolled reader loses track of which session
           this is. -->
      <section>
        <h2 class="m-0 mb-1 text-base font-semibold [overflow-wrap:anywhere]">
          {s.name || s.id}
        </h2>
        <StateMark mark={sessionMark(s.state)} word={stateWord(s.state)} />
        <!-- @decision(DL-057) -->
        {#if endable(s.state, jobs.loaded, job?.state)}
          <div class="mt-3">
            <ConfirmAct
              label="End session"
              question={EndQuestion}
              yes="Yes, end it"
              run={endSession}
              testid="end-session"
            />
          </div>
        {/if}
        {#if endFailure}
          <div class="mt-3"><Problem error={endFailure} what="ending the session" /></div>
        {/if}
        <!-- @decision(DL-090) -->
        {#if s.resume && resumeOffered(s.state, s.resume)}
          <div class="mt-3">
            <ConfirmAct
              label="Resume"
              question={resumeQuestion(s.resume)}
              yes="Resume"
              no="Not now"
              tone="safe"
              run={resumeSession}
              testid="resume-session"
            />
          </div>
        {/if}
        {#if resumeFailure}
          <div class="mt-3"><Problem error={resumeFailure} what="resuming the session" /></div>
        {/if}
      </section>

      {#if job}
        <!-- Shown here rather than on a page of its own — see
             AGENTS.md, "A job is shown on its session's page". Headed by
             its kind, with its state on the same line.
             @decision(DL-057) -->
        <section>
          <h2 class="m-0 mb-2 flex flex-wrap items-baseline justify-between gap-x-2 text-base font-semibold">
            <span>{kindName(job.kind)} job</span>
            <StateMark class="text-md font-normal" mark={jobMark(job.state, job.partial)} word={jobWord(job.state, job.partial)} />
          </h2>
          <!-- @decision(DL-049) -->
          {#if jobClock?.endsAt}
            <dl class="m-0 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
              <dt
                class="text-soft"
                title="How long the job may run unattended. When it runs out, spov stops the job, keeps what it produced and judges it."
              >
                Stops at
              </dt>
              <dd class="m-0 text-right tabular-nums">
                {clock(jobClock.endsAt)}
                <span class="text-soft">({jobClock.length} limit)</span>
              </dd>
            </dl>
          {:else if jobClock}
            <p class="m-0 text-soft tabular-nums">{jobClock.note} ({jobClock.length} limit)</p>
          {/if}

          <!-- Folded to its first lines: a goal can be pages long, and
               it is in the transcript whole. @decision(DL-057) -->
          <p class="mt-2.5 mb-0 leading-snug text-soft [overflow-wrap:anywhere]" data-testid="job-goal">
            {goalOpen || !goalClip.clipped ? job.goal : goalClip.head}
          </p>
          {#if goalClip.clipped}
            <button type="button" class="{textButton} mt-1" aria-expanded={goalOpen} onclick={() => (goalOpen = !goalOpen)}>
              {goalOpen ? 'Show less' : 'Show all'}
            </button>
          {/if}

          {#if job.partial}
            <!-- @decision(DL-050) -->
            <p class="mt-2.5 mb-0 text-sm leading-snug text-gold-t">
              Succeeded with part of what was asked. The verdict below says what is missing.
            </p>
          {/if}

          {#if jobPRs.length || jobReports.length}
            <!-- Each deliverable on its own line, with what was found
                 about it. A report is offered to read or save rather
                 than rendered in a sidebar column.
                 @decision(DL-051) @decision(DL-057) -->
            <h3 class="mt-4 mb-1.5 text-md font-semibold">
              <!-- @decision(DL-051) -->
              {job.kind === 'pr' ? (changeNounOf(jobPRs) === 'merge request' ? 'Merge requests' : 'Pull requests') : 'Reports'}
            </h3>
            <ul class="m-0 list-none p-0" data-testid="deliverables">
              {#each jobPRs as d ([d.platform, d.repository, d.branch].join('\u0000'))}
                <li class="grid grid-cols-[1fr_auto] gap-x-2 py-1">
                  {#if d.status === 'verified' && d.url && /^https?:\/\//.test(d.url)}
                    <a class="font-medium" href={d.url} target="_blank" rel="noreferrer noopener">
                      {pullRequestName(d)}
                      <Icon name="external" class="inline size-3.5" />
                    </a>
                  {:else}
                    <span class="font-medium">{pullRequestName(d)}</span>
                  {/if}
                  <StateMark mark={deliverableMark(d.status)} word={STATUS_WORDS[d.status]} />
                  <span class="col-span-2 text-sm leading-snug text-soft [overflow-wrap:anywhere]">
                    {d.repository}{d.title ? ` · ${d.title}` : ''}{d.detail ? ` · ${d.detail}` : ''}
                  </span>
                </li>
              {/each}
              {#each jobReports as d (d.path)}
                <li class="py-1.5">
                  <div class="grid grid-cols-[1fr_auto] gap-x-2">
                    <span class="font-medium [overflow-wrap:anywhere]">{d.title || d.path}</span>
                    <StateMark mark={deliverableMark(d.status)} word={STATUS_WORDS[d.status]} />
                  </div>
                  <p class="m-0 text-sm text-soft [overflow-wrap:anywhere]">
                    <span class="font-mono">{d.path}</span>{d.size ? ` · ${sizeText(d.size)}` : ''}{d.detail
                      ? ` · ${d.detail}`
                      : ''}
                  </p>
                  {#if d.status === 'verified' && d.path}
                    <!-- Preview for skimming without leaving the app;
                         download for keeping. @decision(DL-063) -->
                    <div class="mt-1.5 flex flex-wrap gap-2">
                      <button
                        type="button"
                        class={secondarySmall}
                        onclick={(e) => openReport(d, e.currentTarget as HTMLElement)}
                      >
                        Read
                      </button>
                      <a class={secondarySmall} href={fileURL(job.id, d.path, true)}>Download</a>
                    </div>
                  {/if}
                </li>
              {/each}
            </ul>
          {/if}

          {#if job.unfinished}
            <!-- @decision(DL-050) -->
            <h3 class="mt-4 mb-1.5 text-md font-semibold">Left unfinished</h3>
            <p class="m-0 text-sm leading-snug whitespace-pre-wrap text-soft [overflow-wrap:anywhere]">
              {job.unfinished}
            </p>
          {/if}

          {#if job.error}
            <p class="mt-2.5 mb-0 text-sm leading-snug {job.state === 'failed' ? 'text-rust' : 'text-soft'}">
              {job.error}
            </p>
          {/if}

          {#if jobGates.length}
            {@const summary = verdictSummary(jobGates)}
            <!-- Folded to one mark and word, gate by gate when opened, and
                 never as the stored blob (DL-050). Opens by itself when a
                 gate failed or the deliverable was partial, the cases a
                 reader needs the detail for. A succeeded job has a verdict
                 too, so this is not drawn as an error.
                 @decision(DL-057) @decision(DL-050) -->
            <details class="mt-4" open={summary.open} data-testid="verdict">
              <summary
                class="flex cursor-pointer list-none items-center justify-between gap-x-2 text-md font-semibold select-none focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2"
              >
                <span class="flex items-center gap-1.5">
                  <Icon name="chevron" class="chevron size-3 text-faint" />
                  Verdict
                </span>
                <StateMark class="font-normal" mark={summary.mark} word={summary.word} />
              </summary>
              <ul class="m-0 mt-1.5 list-none p-0">
                {#each jobGates as gate (gate.gate)}
                  <li class="grid grid-cols-[1fr_auto] gap-x-2 py-1">
                    <span class="font-medium">{gateName(gate.gate)}</span>
                    <StateMark mark={gateMark(gate.outcome)} word={stateWord(gate.outcome)} />
                    {#if gate.detail}
                      <span class="col-span-2 text-sm leading-snug text-soft [overflow-wrap:anywhere]">{gate.detail}</span>
                    {/if}
                  </li>
                {/each}
              </ul>
            </details>
          {:else if job.verdict}
            <!-- A verdict this build's parser could not turn into
                 gates; shown so a reader sees the reasoning rather than
                 nothing. -->
            <h3 class="mt-4 mb-1.5 text-md font-semibold">Verdict</h3>
            <p class="m-0 text-sm whitespace-pre-wrap text-soft">{job.verdict}</p>
          {/if}

          <!-- @decision(DL-057) -->
          {#if job.id && cancellable(job.state)}
            <div class="mt-3">
              <ConfirmAct
                label="Cancel job"
                question={CancelQuestion}
                yes="Yes, cancel"
                run={cancelJob}
                testid="cancel-job"
              />
            </div>
          {/if}
          {#if cancelFailure}
            <div class="mt-3"><Problem error={cancelFailure} what="cancelling the job" /></div>
          {/if}
        </section>
      {/if}

      <!-- Collapsed by default (DL-057). A native <details> rather than
           a $state flag: keyboard accessible and findable in-page with
           nothing to manage. @decision(DL-057) -->
      <details data-testid="sandbox-details">
        <summary
          class="flex cursor-pointer list-none items-center gap-1.5 text-base font-semibold select-none focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2"
        >
          <Icon name="chevron" class="chevron size-3 text-faint" />
          Sandbox
        </summary>
        <dl class="m-0 mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
          <dt class="text-soft">State</dt>
          <dd class="m-0 text-right"><StateMark mark={sessionMark(s.state)} word={stateWord(s.state)} /></dd>
          <dt class="text-soft">Age</dt>
          <dd class="m-0 text-right tabular-nums">{duration(age(s, now))}</dd>
          {#if life}
            <dt class="text-soft">Limit</dt>
            <dd class="m-0 text-right tabular-nums {life.warn ? 'font-semibold text-gold-t' : ''}">
              {life.note.replace(/^of /, '')}
            </dd>
          {/if}
          {#if s.sandbox?.cpu}
            <dt class="text-soft">CPU</dt>
            <dd class="m-0 text-right tabular-nums">{s.sandbox.cpu}</dd>
          {/if}
          {#if s.sandbox?.memory_mib}
            <dt class="text-soft">Memory</dt>
            <dd class="m-0 text-right tabular-nums">{s.sandbox.memory_mib} MiB</dd>
          {/if}
          {#if s.egress}
            <dt class="text-soft">Egress</dt>
            <dd class="m-0 text-right">{sentence(s.egress)}</dd>
          {/if}
          <!-- A floor, and sandbox compute only: model tokens are not
               metered. @decision(DL-110) -->
          <dt class="text-soft">Sandbox cost</dt>
          <dd class="m-0 text-right text-rust tabular-nums">at least {money(s.estimated_cost)}</dd>
        </dl>
        <p class="mt-2 mb-0 text-sm leading-snug text-soft">
          From the sandbox's requested resources and elapsed time. Modal bills the higher of requested and
          actual use, so it can only be more. It does not include model tokens, which spov never meters.
        </p>
        <!-- The session's own variables, beside spov's; not secret, so
             shown as they are. @decision(DL-096) @decision(DL-057) -->
        {#if envRows(s.env).length}
          <h3 class="m-0 mt-3 mb-1 flex items-center gap-1 text-sm font-semibold">
            Environment
            <InfoToggle label="About the environment variables" controls="env-note" bind:open={envOpen} />
          </h3>
          {#if envOpen}
            <p id="env-note" class="mt-0 mb-1.5 text-sm leading-snug text-soft">
              Set in the sandbox from sandbox.env and --env, beside spov's own. They are not secret: anyone who can
              read this session sees them. A token belongs in a routed credential.
            </p>
          {/if}
          <dl class="m-0 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1" data-testid="session-env">
            {#each envRows(s.env) as row (row.name)}
              <dt class="font-mono text-soft [overflow-wrap:anywhere]">{row.name}</dt>
              <dd class="m-0 text-right font-mono whitespace-pre-wrap [overflow-wrap:anywhere]">{row.value}</dd>
            {/each}
          </dl>
        {/if}
      </details>

      {#if repoNames(s).length || s.landing || s.credentials?.length || s.routed_credentials?.length}
        <section>
          <h2 class="m-0 mb-2 text-base font-semibold">Workspace</h2>
          <dl class="m-0 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
            {#each repoNames(s) as repo (repo)}
              <dt class="text-soft">Repository</dt>
              <dd class="m-0 text-right font-mono [overflow-wrap:anywhere]">{repo}</dd>
            {/each}
            {#if s.landing}
              <dt class="text-soft">Landed in</dt>
              <dd class="m-0 text-right font-mono [overflow-wrap:anywhere]">{s.landing}</dd>
            {/if}
          </dl>
          <!-- What the sandbox holds for each platform, never the secret:
               the kind, and anything unusual about where it is kept or how
               far it reaches, with the full sentence on demand.
               @decision(DL-043) @decision(DL-034) @decision(DL-057) -->
          {#each s.credentials ?? [] as cred (cred.platform)}
            {@const line = credentialLine(cred.kind, cred.holder, cred.forge, cred.access, s.git_access)}
            {@const noteID = `credential-note-${cred.platform}`}
            <div class="mt-1" data-testid="sandbox-credential">
              <div class="grid grid-cols-[auto_1fr] items-center gap-x-3">
                <span class="font-mono">{cred.platform}</span>
                <span class="flex items-center justify-end gap-1 text-right text-soft">
                  {credentialTag(cred.kind, cred.holder, cred.access, cred.tier)}
                  <InfoToggle
                    label={`About the ${cred.platform} credential`}
                    controls={noteID}
                    bind:open={
                      () => credentialNotes.has(cred.platform),
                      (v) => (v ? credentialNotes.add(cred.platform) : credentialNotes.delete(cred.platform))
                    }
                  />
                </span>
              </div>
              {#if credentialNotes.has(cred.platform)}
                <p id={noteID} class="mt-1 mb-1 text-sm leading-snug text-soft">{line.note}</p>
              {/if}
            </div>
          {/each}
          <!-- Each routed credential the proxy holds, with its hosts written
               out, never the secret. @decision(DL-077) -->
          {#each s.routed_credentials ?? [] as rc (rc.name)}
            <div class="mt-1" data-testid="routed-credential">
              <div class="grid grid-cols-[auto_1fr] items-center gap-x-3">
                <span class="font-mono">{rc.name}</span>
                <span class="text-right text-soft">{routedCredentialTag(rc.tier, rc.preset)}</span>
              </div>
              <p class="mt-0.5 mb-1 text-right text-sm text-soft [overflow-wrap:anywhere]">{routedCredentialLine(rc)}</p>
            </div>
          {/each}
        </section>
      {/if}

    </aside>
  </div>

  <!-- The report, read without leaving the page. A modal rather than a
       section on the page, since the sidebar is not where a document is
       read. Dismissed by Escape, the backdrop and its own control.
       Content is sanitised (lib/markdown.ts) — the second of the two
       places Svelte's escaping is bypassed. @decision(DL-063) -->
  {#if previewOpen}
    <!-- bg-ink/40 is wrong here: --ink is near-white in dark mode, so
         it would brighten the page instead of dimming it. A fixed black
         is the one colour that dims either palette. @decision(DL-056) -->
    <div
      class="fixed inset-0 z-30 flex items-start justify-center overflow-y-auto bg-black/50 p-4 sm:p-8"
      role="presentation"
      onpointerdown={(e) => (backdropPress = e.target === e.currentTarget)}
      onclick={(e) => {
        // The backdrop only, and the press has to have STARTED there.
        // A click's target is the common ancestor of press and release,
        // so a drag-select that begins on the report's text and ends
        // outside the panel fires this handler with the backdrop as its
        // target — dismissing the modal as the reader finishes
        // selecting a word.
        if (e.target === e.currentTarget && backdropPress) previewOpen = false;
        backdropPress = false;
      }}
    >
      <div
        class="w-full max-w-[min(112ch,calc(100vw-2rem))] border border-rule bg-paper shadow-sm"
        bind:this={previewPanel}
        tabindex="-1"
        role="dialog"
        aria-modal="true"
        aria-label="The report this job produced"
      >
        <div
          class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b border-rule bg-raised px-5 py-3"
        >
          <h2 class="m-0 min-w-0 font-mono text-md font-normal [overflow-wrap:anywhere]">
            {previewing?.title || previewing?.path}
          </h2>
          <div class="flex shrink-0 items-center gap-2">
            {#if job && previewing?.path}
              <a class={secondary} href={fileURL(job.id, previewing.path, true)}>Download</a>
            {/if}
            <button type="button" class={secondary} onclick={() => (previewOpen = false)}>
              Close
            </button>
          </div>
        </div>
        <div class="px-5 py-4 text-read">
          {#if previewFailed}
            <p class="m-0 text-rust">{previewFailed}</p>
          {:else if !previewBody}
            <p class="m-0 text-soft">Loading the report…</p>
          {:else if reportHTML}
            <!-- An image spov did not collect answers 404, and becomes its
                 alt text rather than a broken picture. @decision(DL-051) -->
            <div class="md-body" onerrorcapture={missingImage}>{@html reportHTML}</div>
          {:else}
            <!-- The renderer declined, so the reader sees exactly what
                 the agent wrote rather than an error about it. -->
            <p class="m-0 font-mono text-md leading-relaxed whitespace-pre-wrap">
              {previewBody}
            </p>
          {/if}
        </div>
      </div>
    </div>
  {/if}
{:else}
  <p class="text-soft">Loading…</p>
{/if}
