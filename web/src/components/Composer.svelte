<script lang="ts">
  // Talk to a running session. POSTs /api/v1/sessions/{ref}/messages.
  // Everything that is a function is in ../lib/steer.ts.
  //
  // Below `md` it is a compact bar: a one-line field that grows with what
  // is typed, a round Send, and one chip that opens the model and the
  // choice of what follows. The same elements make up the desktop form.
  //
  // @decision(DL-059) @decision(DL-019) @decision(DL-052) @decision(DL-106)
  import { tick } from 'svelte';
  import { api, unwrap } from '../lib/api/client';
  import {
    type After,
    type Delivery,
    type EchoTrack,
    type JobState,
    Failed,
    SendShort,
    canStop,
    composerStatus,
    controlsNote,
    deliveryNote,
    draftProblem,
    offersAfter,
    resumable,
    send,
    sendability,
    sendsOnEnter,
    stop,
    stopOutcome,
  } from '../lib/steer';
  import Icon from './Icon.svelte';
  import InfoToggle from './InfoToggle.svelte';
  import ModelSwitch from './ModelSwitch.svelte';
  import Problem from './Problem.svelte';
  import StateMark from './StateMark.svelte';
  import { fieldBase, primary, secondary } from './ui';

  interface Props {
    /** The session to send into, as the API's path takes it. */
    ref: string;
    /**
     * Whether the page has read the session's jobs. "No job" and "not
     * read yet" are both undefined without it.
     */
    jobKnown: boolean;
    /** The state of the session's job, or undefined when it has none. */
    jobState?: JobState;
    /**
     * The job's id, which Resume needs: that endpoint is addressed by
     * job rather than by session. Undefined exactly when `jobState` is.
     */
    jobId?: string;
    /**
     * Whether a turn is running right now, derived by the page from
     * the log it already holds. Interrupt is drawn only while it is true.
     *
     * @decision(DL-059) @decision(DL-053)
     */
    inFlight: boolean;
    /**
     * How many turns the page has seen end (`turnsEnded`), which tells
     * the interrupt's outcome when it no longer describes the last turn.
     *
     * @decision(DL-059)
     */
    turnsEnded?: number;
    /**
     * Whether an ask in the transcript waits on the reader.
     *
     * @decision(DL-020)
     */
    asking?: boolean;
    /**
     * The model the session is on, from the log; undefined draws no
     * switch.
     *
     * @decision(DL-014)
     */
    model?: string;
    /** Called at each step of a send, so the page can draw the echo. */
    onEcho: EchoTrack;
    /**
     * Called after an act that has already moved the job's row, so the
     * page can re-read it rather than waiting out its ten-second poll.
     * Awaited — see `release` — so a control can stay disabled until the
     * page has read what it did. It is `Resource.refresh`, which never
     * rejects.
     */
    onJobChanged?: () => void | Promise<void>;
  }

  const { ref, jobKnown, jobState, jobId, inFlight, turnsEnded = 0, asking = false, model, onEcho, onJobChanged }: Props = $props();

  let text = $state('');
  // What follows this message on a running job. Back to continue after
  // every send, so a handover is never left set by accident.
  //
  // @decision(DL-059) @decision(DL-052)
  let after = $state<After>('continue');
  let sending = $state(false);
  let failure = $state<unknown>(undefined);
  let box = $state<HTMLTextAreaElement | null>(null);
  // The id to send again, set when the last send failed in a way that
  // does not say whether the sandbox took the message.
  //
  // @decision(DL-059)
  let replay = '';
  // The act the replay id was minted for: a retry by another button or
  // with another choice is a new message, not a replay.
  //
  // @decision(DL-059)
  let replayAct = '';

  const allowed = $derived(sendability(jobState));
  const choosesAfter = $derived(offersAfter(jobState, jobKnown));
  const problem = $derived(draftProblem(text));
  // A choice the page no longer offers is sent as the default.
  const following = $derived<After>(choosesAfter ? after : 'continue');
  // Stoppable AND in flight: Interrupt is drawn, and the note may name
  // it, only then.
  //
  // @decision(DL-059)
  const stoppable = $derived(canStop(jobState) && inFlight);
  const note = $derived(deliveryNote(jobState, following, jobKnown, stoppable));

  // Interrupt, and what came back.
  //
  // @decision(DL-059) @decision(DL-053)
  let stopping = $state(false);
  let stopSaid = $state('');
  let stopFailure = $state<unknown>(undefined);
  // The outcome describes the turn that just ended, so it clears when
  // the next one begins. Reads `inFlight` and writes `stopSaid`, which
  // it does not read, so it cannot re-dirty its own dependency.
  //
  // @decision(DL-059) @decision(DL-062)
  $effect(() => {
    if (!inFlight) return;
    stopSaid = '';
    stopFailure = undefined;
  });
  // The interrupted turn's end and the start of the turn it resumes can
  // arrive in one batch, or before the answer, and then `inFlight` never
  // falls for the effect above. So the outcome is also withheld once a
  // turn after the interrupted one has ended.
  //
  // @decision(DL-059)
  let stopEndsAt = $state(0);
  const stopShown = $derived(stopSaid !== '' && turnsEnded <= stopEndsAt);

  // Resume: the endpoint of DL-054. Requires the id as well as the
  // state — they arrive together off one job object.
  //
  // @decision(DL-059) @decision(DL-054)
  const releasable = $derived(resumable(jobState) && jobId !== undefined);

  // One bar above the box: the state, then every control, with what
  // they do behind its (i).
  //
  // @decision(DL-059)
  const status = $derived(composerStatus(jobState, inFlight, asking));
  const explained = $derived(controlsNote(jobState, stoppable, releasable));
  let controlsOpen = $state(false);
  // Whether the options a phone folds away are open.
  //
  // @decision(DL-106)
  let optionsOpen = $state(false);
  const modelShort = $derived(model ? (model.split('#')[0].split('/').pop() ?? model) : '');
  const chip = $derived(
    `${modelShort || 'Options'}${choosesAfter && following === 'handover' ? ' · hand over' : ''}`,
  );
  // The field grows with its text on a phone, to five lines or so. On a
  // wider screen it is the reader's to resize.
  //
  // @decision(DL-106)
  $effect(() => {
    void text;
    if (!box || window.matchMedia?.('(min-width: 48rem)').matches !== false) return;
    box.style.height = 'auto';
    box.style.height = `${Math.min(box.scrollHeight, 128)}px`;
  });
  let noteOpen = $state(false);
  let resuming = $state(false);
  let resumeFailure = $state<unknown>(undefined);

  async function halt() {
    if (stopping) return;
    stopping = true;
    stopFailure = undefined;
    stopSaid = '';
    stopEndsAt = turnsEnded + 1;
    try {
      const was = await stop(() =>
        api.POST('/api/v1/sessions/{ref}/interrupt', {
          params: { path: { ref } },
          // No conversation named — the server resolves it.
          // @decision(DL-019)
          body: {},
        }),
      );
      stopSaid = stopOutcome(was);
      // No re-read here: the row does not move on this call, so the
      // resource's own poll is what finds the move.
      //
      // @decision(DL-053) @decision(DL-052)
    } catch (err) {
      stopFailure = err;
    } finally {
      stopping = false;
    }
  }

  async function release() {
    // `releasable` already withholds the button without a jobId; this
    // is the compiler's guard against a request to `/jobs/undefined`.
    if (!jobId || resuming) return;
    resuming = true;
    resumeFailure = undefined;
    try {
      await unwrap(await api.POST('/api/v1/jobs/{id}/resume', { params: { path: { id: jobId } } }));
      // Held until the re-read lands, not just the 204, or clearing
      // `resuming` early re-enables the button against a stale
      // `steered` and a second press gets a 409.
      //
      // @decision(DL-059) @decision(DL-054)
      await onJobChanged?.();
    } catch (err) {
      resumeFailure = err;
    } finally {
      resuming = false;
    }
  }

  // Interrupt and Resume share one line above the box. The button goes
  // last in the source as well as on the right, so reading order and
  // tab order agree.
  //
  // @decision(DL-056)
  const pane = 'mb-3 max-lg:mb-2 flex flex-wrap items-center justify-between gap-x-4 gap-y-2 text-md';
  const kbd = 'rounded-[3px] border border-b-2 border-rule-2 bg-raised px-1 font-sans text-xs text-soft';

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    await go('steer');
  }

  // Send steers: the agent reads it once its current step finishes.
  //
  // @decision(DL-059) @decision(DL-019)
  async function go(delivery: Delivery) {
    // The markup's `disabled` is not the guard: a form can be submitted
    // by Enter, by requestSubmit or by an extension. A job state that
    // refuses a message is not checked here — the form is not rendered
    // at all in that case.
    if (problem || sending) return;
    const words = text;
    const act = `${delivery}/${following}`;
    if (act !== replayAct) replay = '';
    replayAct = act;
    sending = true;
    failure = undefined;
    try {
      await send(words, delivery, (body) => api.POST('/api/v1/sessions/{ref}/messages', {
        params: { path: { ref } },
        body,
      }), onEcho, replay, undefined, undefined, following);
      replay = '';
      after = 'continue';
      // Cleared on success only, so a refusal leaves the reader's words
      // in the box rather than losing them.
      //
      // @decision(DL-059)
      text = '';
    } catch (err) {
      failure = err instanceof Failed ? err.cause : err;
      replay = err instanceof Failed ? err.replay : '';
    } finally {
      sending = false;
      // The box is disabled while a send is in flight, and a disabled
      // element loses focus, so this restores it. `tick()` rather than
      // a microtask, so it runs after the DOM has caught up with
      // `sending`.
      void tick().then(() => box?.focus());
    }
  }

  function onKeydown(event: KeyboardEvent) {
    if (!sendsOnEnter(event)) return;
    event.preventDefault();
    void go('steer');
  }
</script>

<!-- Anchored at the foot of the transcript's column rather than fixed
     to the viewport — the "Jump to latest" control already occupies
     that spot. Does not disturb the tail latch: the page scrolls
     `document.scrollingElement`, and the growth effect re-measures for
     the taller document. Indented by the transcript's gutter, so it
     sits under the words. @decision(DL-059) @decision(DL-057) -->
<section
  class="mt-8 max-w-[var(--log-w)] max-lg:mt-0 max-lg:max-w-none"
  aria-label="Talk to this session"
>
  {#if failure}
    <div class="mb-4"><Problem error={failure} what="that message" /></div>
  {/if}

  {#if stopFailure}
    <div class="mb-4"><Problem error={stopFailure} what="interrupting the turn" /></div>
  {/if}

  <!-- The state, then the controls: Interrupt only while there is a
       turn to stop (in-flight comes from the log already held, with no
       poll), Resume only where the endpoint accepts it. Outside the
       form: `canStop` and the form's guard are separate rules, and
       sending is refused on a `blocked` job, where Resume matters most.
       @decision(DL-059) @decision(DL-053) @decision(DL-054) -->
  {#if status}
    <div class={pane} data-testid="composer-status">
      <span class="flex items-center gap-2">
        <StateMark mark={status.mark} word={status.word} />
      </span>
      <span class="flex flex-wrap items-center gap-2">
        {#if stoppable}
          <button type="button" data-testid="composer-stop" class={secondary} onclick={halt} disabled={stopping}
            >{stopping ? 'Interrupting…' : 'Interrupt'}</button
          >
        {/if}
        {#if releasable}
          <button type="button" data-testid="composer-resume" class={secondary} onclick={release} disabled={resuming}
            >{resuming ? 'Resuming…' : 'Resume job'}</button
          >
        {/if}
        {#if explained.length > 0}
          <span data-testid="composer-controls-info"
            ><InfoToggle label="What these do" controls="composer-controls-note" bind:open={controlsOpen} /></span
          >
        {/if}
      </span>
    </div>
    {#if controlsOpen && explained.length > 0}
      <div id="composer-controls-note" class="-mt-1 mb-3 text-sm leading-snug text-soft" data-testid="composer-resume-note">
        {#each explained as line (line.control)}
          <p class="m-0"><b class="font-semibold text-ink">{line.control}:</b> {line.does}</p>
        {/each}
      </div>
    {/if}
  {/if}

  {#if stopShown}
    <p class="mt-0 mb-3 text-md text-soft" data-testid="composer-stopped">{stopSaid}</p>
  {/if}

  {#if resumeFailure}
    <div class="mb-4"><Problem error={resumeFailure} what="resuming the job" /></div>
  {/if}

  {#if allowed.can}
    <form
      onsubmit={submit}
      data-testid="composer"
      class="grid grid-cols-[minmax(0,1fr)_auto] items-end gap-2 md:flex md:flex-wrap md:items-center"
    >
      <label class="sr-only" for="composer-text">Message</label>
      <textarea
        id="composer-text"
        bind:this={box}
        bind:value={text}
        onkeydown={onKeydown}
        rows="1"
        enterkeyhint="send"
        disabled={sending}
        class="{fieldBase} resize-none px-3 py-2.5 text-read max-md:max-h-32 max-md:min-h-11 md:min-h-[5.75rem] md:resize-y"
        placeholder="Message this session"
      ></textarea>

      <button
        type="submit"
        data-testid="composer-send"
        class="{primary} max-md:size-11 max-md:rounded-full max-md:p-0"
        disabled={sending || problem !== ''}
        ><Icon name="send" class="size-5 md:hidden" /><span class="max-md:sr-only">{sending ? 'Sending…' : 'Send'}</span></button
      >

      <button
        type="button"
        class="col-span-2 inline-flex min-w-0 cursor-pointer items-center gap-1.5 justify-self-start border-0 bg-transparent p-0 text-sm text-soft focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2 md:hidden"
        aria-expanded={optionsOpen}
        aria-controls="composer-options"
        data-testid="composer-options-toggle"
        onclick={() => (optionsOpen = !optionsOpen)}
      >
        <Icon name="sliders" class="size-4 shrink-0" />
        <span class="truncate">{chip}</span>
        <Icon name="chevron" class="size-3 shrink-0 transition-transform {optionsOpen ? '-rotate-90' : 'rotate-90'}" />
      </button>

      <!-- What a phone folds away; on a wider screen the wrapper is not
           there and its contents lie in the form's own row.
           @decision(DL-106) -->
      <div id="composer-options" class="{optionsOpen ? 'flex' : 'hidden'} col-span-2 flex-col gap-3 md:contents">
        <!-- On a running job only: what follows this one message.
             @decision(DL-059) @decision(DL-052) -->
        {#if choosesAfter}
          <label class="flex items-center gap-2 text-md text-soft">
            Then
            <select
              class="rounded-ctl border border-rule-2 bg-raised px-2 py-1 text-md text-ink disabled:opacity-60 focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-1"
              data-testid="composer-after"
              bind:value={after}
              disabled={sending}
            >
              <option value="continue">continue autonomously</option>
              <option value="handover">hand over to me</option>
            </select>
          </label>
        {/if}

        <!-- Switching applies from the agent's next step.
             @decision(DL-014) -->
        {#if model !== undefined}
          <ModelSwitch {ref} current={model} />
        {/if}

        <span class="text-sm text-faint max-md:hidden md:ml-auto"
          ><kbd class={kbd}>Enter</kbd> to send, <kbd class={kbd}>Shift</kbd> + <kbd class={kbd}>Enter</kbd> for a new
          line</span
        >

        <!-- Says when the message is read and what follows it; names
             Interrupt only while it is on the page.
             @decision(DL-059) -->
        <p class="m-0 flex max-w-[68ch] items-center gap-1.5 text-sm text-soft md:mt-0 md:w-full">
          {SendShort}
          <span data-testid="composer-note-info"
            ><InfoToggle label="When the agent reads it" controls="composer-note" bind:open={noteOpen} /></span
          >
        </p>
        {#if noteOpen}
          <p id="composer-note" class="m-0 max-w-[68ch] text-sm text-soft md:w-full" data-testid="composer-note">{note}</p>
        {/if}
      </div>
    </form>
  {:else}
    <!-- The reason, not an absent box — the same reasoning DL-013 gives
         for keeping an unavailable provider in the model list.
         @decision(DL-059) @decision(DL-052) -->
    <p class="m-0 text-md text-soft" data-testid="composer-refusal">
      {allowed.why} spov will not send a message it knows would not be read.
    </p>
  {/if}

</section>
