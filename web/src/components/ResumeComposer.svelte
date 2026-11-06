<script lang="ts">
  // The composer of a stopped session that can be resumed: what is typed
  // is sent once the session runs again. Send reads "Resume and send"
  // and acts only on a second press, worded by what will come back.
  // Enter never submits, and a Committed error never re-enables it: a
  // second resume buys nothing but a refusal.
  //
  // @decision(DL-090) @decision(DL-061)
  import { tick } from 'svelte';
  import { api } from '../lib/api/client';
  import { Committed } from '../lib/jobform';
  import { type ResumeView, resumed, resumeQuestion } from '../lib/resume';
  import { draftProblem, newMessageID } from '../lib/steer';
  import Problem from './Problem.svelte';
  import { fieldBase, primary, secondary } from './ui';

  interface Props {
    /** The session to resume, as the API's path takes it. */
    ref: string;
    resume: ResumeView;
    /** Called once the resume was accepted, so the page re-reads. */
    onResumed: () => void | Promise<void>;
  }

  const { ref, resume, onResumed }: Props = $props();

  let text = $state('');
  let arming = $state(false);
  let sending = $state(false);
  let committed = $state(false);
  let failure = $state<unknown>(undefined);
  let confirmButton = $state<HTMLButtonElement | undefined>(undefined);
  const problem = $derived(draftProblem(text));

  async function arm(e: SubmitEvent) {
    e.preventDefault();
    if (sending || committed || problem || !text.trim()) return;
    arming = true;
    await tick();
    confirmButton?.focus();
  }

  async function confirm() {
    if (sending || committed) return;
    sending = true;
    failure = undefined;
    try {
      await resumed(
        (body) => api.POST('/api/v1/sessions/{ref}/resume', { params: { path: { ref } }, body }),
        // No conversation named, as the composer names none: the server
        // takes the only one, and refuses rather than guess among
        // several. @decision(DL-019)
        { text: text.trim(), id: newMessageID() },
      );
      text = '';
      arming = false;
      await onResumed();
    } catch (err) {
      failure = err;
      if (err instanceof Committed) committed = true;
    } finally {
      sending = false;
    }
  }
</script>

<!-- Its one field is a textarea, where Enter is a new line and never a
     submit, so nothing here can resume on Enter. @decision(DL-061) -->
<form class="mt-6" onsubmit={arm} data-testid="resume-composer">
  <label class="mb-1 block text-sm text-soft" for="resume-message">
    This session has stopped. What you write here is sent once it runs again.
  </label>
  <textarea
    id="resume-message"
    class="{fieldBase} min-h-24 w-full"
    bind:value={text}
    disabled={sending || committed}
  ></textarea>
  {#if arming}
    <div role="group" aria-label="Resume and send" class="mt-2" data-testid="resume-composer-ask">
      <p class="m-0 mb-2 text-sm text-soft">{resumeQuestion(resume)}</p>
      <div class="flex flex-wrap gap-2">
        <button
          type="button"
          class={primary}
          bind:this={confirmButton}
          disabled={sending || committed}
          onclick={confirm}
          data-testid="resume-composer-yes">{sending ? 'Resuming…' : 'Resume and send'}</button
        >
        <button type="button" class={secondary} disabled={sending} onclick={() => (arming = false)}>Not now</button>
      </div>
    </div>
  {:else}
    <div class="mt-2">
      <button type="submit" class={primary} disabled={sending || committed || !!problem || !text.trim()}
        >Resume and send</button
      >
    </div>
  {/if}
  {#if failure}
    <div class="mt-3"><Problem error={failure} what="resuming the session" /></div>
  {/if}
</form>
