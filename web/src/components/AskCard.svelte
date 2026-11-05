<script lang="ts">
  // One ask in the transcript: a permission request or a question form,
  // answered here, collapsing to one line once the log says it was.
  // Everything that is a function is in ../lib/asks.ts.
  //
  // @decision(DL-020)
  import { api } from '../lib/api/client';
  import {
    type Ask,
    type AskState,
    type Draft,
    KnownTypes,
    Other,
    answer,
    askLine,
    emptyDraft,
    permissionSummary,
    shownFields,
    toAnswer,
    webLink,
  } from '../lib/asks';
  import { suppressImplicitSubmit } from '../lib/jobform';
  import Problem from './Problem.svelte';
  import { fieldBase, primary, secondary, textButton } from './ui';

  interface Props {
    /** The session, as the API's path takes it. */
    ref: string;
    ask: Ask;
    askState: AskState;
    /** Whether the session can still be answered: it is live. */
    open: boolean;
  }

  const { ref, ask, askState, open }: Props = $props();

  // Set from the press until the log settles the ask, or the answer
  // fails. The card stays disabled meanwhile.
  let sending = $state(false);
  // A 2xx came back: the sandbox took it, and the log has not said so yet.
  let sent = $state(false);
  let failure = $state<unknown>(undefined);
  let rejecting = $state(false);
  let message = $state('');
  // Spread into a plain object, which $state proxies, as it does not a
  // null-prototype one; a spread keeps a `__proto__` key as a key.
  // svelte-ignore state_referenced_locally
  let draft = $state<Draft>({ ...emptyDraft(ask.fields ?? []) });
  let tried = $state(false);

  const pending = $derived(askState.status === 'pending');
  const busy = $derived(sending && pending);
  const fields = $derived(shownFields(ask.fields ?? [], draft));
  const built = $derived(toAnswer(ask.fields ?? [], draft));

  async function act(post: () => ReturnType<Parameters<typeof answer>[0]>) {
    if (busy) return;
    sending = true;
    failure = undefined;
    try {
      await answer(post);
      sent = true;
    } catch (err) {
      failure = err;
    }
    sending = false;
  }

  const path = () => ({ path: { ref, id: ask.id } });

  function reply(decision: 'once' | 'reject') {
    const body = decision === 'reject' && message.trim() ? { decision, message: message.trim() } : { decision };
    void act(() => api.POST('/api/v1/sessions/{ref}/permissions/{id}', { params: path(), body }));
  }

  function submit(e: SubmitEvent) {
    e.preventDefault();
    tried = true;
    if (built.problems.length) return;
    void act(() => api.POST('/api/v1/sessions/{ref}/forms/{id}', { params: path(), body: { answer: built.answer } }));
  }

  function dismiss() {
    void act(() => api.DELETE('/api/v1/sessions/{ref}/forms/{id}', { params: path() }));
  }

  function toggle(key: string, value: string, on: boolean) {
    const d = draft[key];
    d.picks = on ? [...d.picks, value] : d.picks.filter((p) => p !== value);
  }
</script>

{#if !pending}
  <!-- Answered, dismissed or never answered: one quiet line.
       @decision(DL-020) -->
  <p class="m-0 text-md text-soft" data-testid="ask-settled">
    <span class="mr-2 text-faint">{ask.kind === 'permission' ? permissionSummary(ask) : ask.title || 'Question'}</span
    >{askLine(ask, askState)}
  </p>
{:else if ask.kind === 'permission'}
  <div class="rounded-ctl border border-gold bg-raised px-3.5 py-3" data-testid="ask-permission">
    <p class="m-0 text-sm font-medium text-gold-t">The agent asks permission</p>
    <p class="m-0 mt-1 font-mono text-md break-all text-ink">{permissionSummary(ask)}</p>
    {#if !open}
      <p class="m-0 mt-2 text-sm text-faint">{askLine(ask, askState)}</p>
    {:else if rejecting}
      <label class="mt-3 block text-sm text-soft" for="reject-{ask.id}">What should the agent do instead? (optional)</label>
      <textarea
        id="reject-{ask.id}"
        class="{fieldBase} mt-1 min-h-16 px-2.5 py-2 text-md"
        bind:value={message}
        disabled={busy}
        data-testid="ask-reject-message"
      ></textarea>
      <div class="mt-2 flex flex-wrap items-center gap-2">
        <button type="button" class={primary} disabled={busy} onclick={() => reply('reject')} data-testid="ask-reject-send"
          >{busy ? 'Sending…' : 'Reject'}</button
        >
        <button type="button" class={textButton} disabled={busy} onclick={() => (rejecting = false)}>Back</button>
      </div>
    {:else}
      <div class="mt-3 flex flex-wrap items-center gap-2">
        <button type="button" class={primary} disabled={busy} onclick={() => reply('once')} data-testid="ask-allow"
          >{busy ? 'Sending…' : 'Allow once'}</button
        >
        <button type="button" class={secondary} disabled={busy} onclick={() => (rejecting = true)} data-testid="ask-reject"
          >Reject</button
        >
      </div>
    {/if}
    {#if failure}
      <div class="mt-3"><Problem error={failure} what="that answer" /></div>
    {:else if sent}
      <p class="m-0 mt-2 text-sm text-faint" data-testid="ask-sent">Sent. Waiting for the session to confirm.</p>
    {/if}
  </div>
{:else}
  <form class="rounded-ctl border border-gold bg-raised px-3.5 py-3" data-testid="ask-form" onsubmit={submit} novalidate>
    <p class="m-0 text-sm font-medium text-gold-t">{ask.title || 'The agent asks'}</p>
    {#each fields as f, i (i)}
      {@const d = draft[f.key]}
      {@const name = `${ask.id}-${f.key}`}
      <fieldset class="m-0 mt-3 min-w-0 border-0 p-0" disabled={busy || !open}>
        {#if f.title}<legend class="m-0 p-0 text-md font-semibold text-ink">{f.title}</legend>{/if}
        {#if f.description}<p class="m-0 mt-0.5 text-md text-soft">{f.description}</p>{/if}
        {#if f.type === 'external'}
          {@const href = webLink(f.url)}
          {#if href}
            <a class="text-md text-olive underline" {href} target="_blank" rel="noopener noreferrer">{href}</a>
          {:else if f.url}
            <p class="m-0 mt-1 font-mono text-sm break-all text-soft">{f.url}</p>
          {/if}
        {:else if !KnownTypes.has(f.type)}
          <p class="m-0 mt-1 text-sm text-faint">This question cannot be answered here. Use spov attach.</p>
        {:else if (f.type === 'string' && (f.options ?? []).length) || f.type === 'multiselect'}
          {@const multi = f.type === 'multiselect'}
          <div class="mt-1.5 flex flex-col gap-1">
            {#each f.options ?? [] as o, j (j)}
              <label class="flex cursor-pointer items-baseline gap-2 text-md">
                {#if multi}
                  <input type="checkbox" checked={d.picks.includes(o.value)} onchange={(e) => toggle(f.key, o.value, e.currentTarget.checked)} />
                {:else}
                  <input type="radio" {name} value={o.value} checked={d.picks[0] === o.value} onchange={() => (d.picks = [o.value])} />
                {/if}
                <span>{o.label}{#if o.description}<span class="ml-1.5 text-soft">{o.description}</span>{/if}</span>
              </label>
            {/each}
            {#if f.custom}
              <label class="flex cursor-pointer items-baseline gap-2 text-md">
                {#if multi}
                  <input type="checkbox" checked={d.picks.includes(Other)} onchange={(e) => toggle(f.key, Other, e.currentTarget.checked)} />
                {:else}
                  <input type="radio" {name} value={Other} checked={d.picks[0] === Other} onchange={() => (d.picks = [Other])} />
                {/if}
                <span>Other</span>
              </label>
              {#if d.picks.includes(Other)}
                <input
                  class="{fieldBase} ml-6 w-auto px-2.5 py-1.5 text-md"
                  bind:value={d.other}
                  aria-label="Other: {f.title ?? f.key}"
                  onkeydown={suppressImplicitSubmit}
                />
              {/if}
            {/if}
          </div>
        {:else if f.type === 'string'}
          <input
            class="{fieldBase} mt-1.5 px-2.5 py-1.5 text-md"
            bind:value={d.text}
            placeholder={f.placeholder}
            aria-label={f.title ?? f.key}
            onkeydown={suppressImplicitSubmit}
          />
        {:else if f.type === 'number' || f.type === 'integer'}
          <input
            class="{fieldBase} mt-1.5 w-40 px-2.5 py-1.5 text-md"
            inputmode={f.type === 'integer' ? 'numeric' : 'decimal'}
            bind:value={d.text}
            aria-label={f.title ?? f.key}
            onkeydown={suppressImplicitSubmit}
          />
        {:else if f.type === 'boolean'}
          <div class="mt-1.5 flex gap-4 text-md">
            <label class="flex cursor-pointer items-center gap-2"
              ><input type="radio" {name} checked={d.flag === true} onchange={() => (d.flag = true)} />Yes</label
            >
            <label class="flex cursor-pointer items-center gap-2"
              ><input type="radio" {name} checked={d.flag === false} onchange={() => (d.flag = false)} />No</label
            >
          </div>
        {/if}
      </fieldset>
    {/each}
    {#if !open}
      <p class="m-0 mt-3 text-sm text-faint">{askLine(ask, askState)}</p>
    {:else}
      {#if tried && built.problems.length}
        <ul class="m-0 mt-3 list-none p-0 text-sm text-rust" data-testid="ask-problems">
          {#each built.problems as p, k (k)}<li>{p}</li>{/each}
        </ul>
      {/if}
      <div class="mt-3 flex flex-wrap items-center gap-2">
        <button type="submit" class={primary} disabled={busy} data-testid="ask-submit">{busy ? 'Sending…' : 'Submit'}</button>
        <button type="button" class={secondary} disabled={busy} onclick={dismiss} data-testid="ask-dismiss">Dismiss</button>
      </div>
    {/if}
    {#if failure}
      <div class="mt-3"><Problem error={failure} what="that answer" /></div>
    {:else if sent}
      <p class="m-0 mt-2 text-sm text-faint" data-testid="ask-sent">Sent. Waiting for the session to confirm.</p>
    {/if}
  </form>
{/if}
