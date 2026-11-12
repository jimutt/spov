<script lang="ts">
  // People: who may sign in to this server, for an admin. One list, one
  // add field, and per row an admin toggle and Remove behind a second
  // press. Every rule is the server's; ../lib/allowlist.ts mirrors them
  // so the page does not offer an act the server would refuse, and a
  // refusal re-reads the list. One `pending` flag covers every request.
  //
  // @decision(DL-129) @decision(DL-128) @decision(DL-132)
  import { api, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import {
    type AllowlistEntryView,
    blockedReason,
    canDemote,
    canRemove,
    entryLabel,
    entryPath,
    entryRef,
    isPartialRemoval,
    refPath,
    refProblem,
    refusalText,
    removalQuestion,
    sortEntries,
  } from '../lib/allowlist';
  import { day } from '../lib/format';
  import ConfirmAct from './ConfirmAct.svelte';
  import Problem from './Problem.svelte';
  import { danger, field, help, label as fieldLabel, primary, secondary, sectionTitle } from './ui';

  // A 501 is a server with no allowlist (local mode): the section is not
  // drawn at all, as the Organisation section does.
  let unsupported = $state(false);
  const list = resource(async (signal) =>
    unwrap(
      await api.GET('/api/v1/allowlist', { signal }).then((r) => {
        if (r.response.status === 501) unsupported = true;
        return r;
      }),
    ),
  );

  let text = $state('');
  let makeAdmin = $state(false);
  let pending = $state(false);
  // A refused add, shown beside the field.
  let addFailure = $state<unknown>(undefined);
  // A refused row act, shown above the list.
  let rowFailure = $state<unknown>(undefined);
  // A removal whose entry is gone but whose off-boarding did not
  // finish: its row has vanished, so the retry is offered here.
  let retry = $state<{ path: string; label: string } | null>(null);
  let added = $state<AllowlistEntryView | null>(null);

  const all = $derived(list.data?.entries ?? []);
  const entries = $derived(sortEntries(all));
  const ref = $derived(entryRef(text));
  const typed = $derived(text.trim() ? refProblem(ref) : '');
  const now = new Date();

  async function add(e: SubmitEvent) {
    e.preventDefault();
    if (pending || !ref || refProblem(ref)) return;
    pending = true;
    addFailure = undefined;
    added = null;
    try {
      added = await unwrap(
        await api.PUT('/api/v1/allowlist/{entry}', {
          params: { path: { entry: refPath(ref) } },
          body: { admin: makeAdmin },
        }),
      );
      text = '';
      makeAdmin = false;
    } catch (err) {
      addFailure = err;
    } finally {
      pending = false;
    }
    await list.refresh();
  }

  async function setAdmin(entry: AllowlistEntryView, admin: boolean) {
    if (pending) return;
    pending = true;
    rowFailure = undefined;
    try {
      await unwrap(
        await api.PUT('/api/v1/allowlist/{entry}', { params: { path: { entry: entryPath(entry) } }, body: { admin } }),
      );
    } catch (err) {
      rowFailure = err;
    } finally {
      pending = false;
    }
    await list.refresh();
  }

  async function remove(path: string, label: string) {
    if (pending) return;
    pending = true;
    rowFailure = undefined;
    retry = null;
    try {
      await unwrap(await api.DELETE('/api/v1/allowlist/{entry}', { params: { path: { entry: path } } }));
      if (added && entryPath(added) === path) added = null;
    } catch (err) {
      rowFailure = err;
      if (isPartialRemoval(err)) retry = { path, label };
    } finally {
      pending = false;
    }
    await list.refresh();
  }
</script>

<!-- Nothing until the server has answered, so a local server's 501 does
     not flash the heading. -->
{#if !unsupported && (list.data || list.error)}
  <h2 class="{sectionTitle} mb-1.5">People</h2>
  <p class="mt-0 mb-5 max-w-[64ch] text-soft">
    Who may sign in to this server. Add a person by their GitHub username, or by the primary, verified address of
    their GitHub account. Only admins see this section.
  </p>

  <form class="mb-6 max-w-[46ch]" onsubmit={add} data-testid="allowlist-add">
    <label class={fieldLabel} for="allowlist-entry">GitHub username or address</label>
    <input
      id="allowlist-entry"
      class={field}
      bind:value={text}
      placeholder="@octocat or someone@example.com"
      autocomplete="off"
      autocapitalize="off"
      spellcheck="false"
      aria-invalid={typed !== '' || addFailure !== undefined}
      aria-describedby="allowlist-entry-help"
    />
    <p id="allowlist-entry-help" class="{help} mb-3" data-testid="allowlist-add-note">
      {typed || 'A username is looked up on GitHub, and the name it has there is shown back.'}
    </p>
    <label class="mb-3 flex items-center gap-2 text-md">
      <input type="checkbox" bind:checked={makeAdmin} data-testid="allowlist-add-admin" />
      Administrator, who may change this list
    </label>
    <button type="submit" class={primary} disabled={pending || !ref || typed !== ''}
      >{pending ? 'Working…' : 'Add'}</button
    >
    {#if addFailure}
      <p class="m-0 mt-3 max-w-[64ch] text-md text-rust" role="alert" data-testid="allowlist-add-failure">{refusalText(addFailure)}</p>
    {/if}
    {#if added}
      <p class="mt-3 mb-0 text-md text-ink" role="status" data-testid="allowlist-added">
        Added {entryLabel(added)}{#if added.name}, {added.name}{/if}.
      </p>
    {/if}
  </form>

  {#if list.error}
    <div class="mb-5"><Problem error={list.error} what="the people on this server" /></div>
  {/if}
  {#if rowFailure}
    <div class="mb-3" data-testid="allowlist-row-failure"><Problem error={rowFailure} what="that change" /></div>
  {/if}
  {#if retry}
    <div class="mb-5 max-w-[64ch]" data-testid="allowlist-retry">
      <p class="m-0 mb-2 text-md text-soft">
        {retry.label} is off the list, but not everything of theirs was ended. Retry to finish.
      </p>
      <button type="button" class={secondary} disabled={pending} onclick={() => retry && remove(retry.path, retry.label)}
        >Retry</button
      >
    </div>
  {/if}

  <ul class="m-0 max-w-[64ch] list-none p-0" data-testid="allowlist">
    {#each entries as e (entryPath(e))}
      {@const blocked = blockedReason(e, all)}
      <li class="border-t border-rule py-3" data-testid="allowlist-entry">
        <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
          <span class="font-semibold text-ink break-all">{entryLabel(e)}</span>
          {#if e.name}<span class="text-soft">{e.name}</span>{/if}
          {#if e.admin}<span class="text-sm text-olive" data-testid="allowlist-admin">admin</span>{/if}
          {#if e.you}<span class="text-sm text-soft" data-testid="allowlist-you">you</span>{/if}
        </div>
        <p class="m-0 mt-0.5 text-sm text-soft" data-testid="allowlist-seen">
          {e.last_login_at ? `Last signed in ${day(e.last_login_at, now)}` : 'Never signed in'}
        </p>
        <div class="mt-2 flex flex-wrap items-start gap-2">
          {#if e.admin}
            <button
              type="button"
              class={secondary}
              disabled={pending || !canDemote(e, all)}
              onclick={() => setAdmin(e, false)}
              data-testid="allowlist-demote">Remove admin rights</button
            >
          {:else}
            <button
              type="button"
              class={secondary}
              disabled={pending}
              onclick={() => setAdmin(e, true)}
              data-testid="allowlist-promote">Make admin</button
            >
          {/if}
          {#if canRemove(e, all)}
            <ConfirmAct
              label="Remove"
              question={removalQuestion(e)}
              yes="Remove"
              run={() => remove(entryPath(e), entryLabel(e))}
              testid="allowlist-remove"
            />
          {:else}
            <button type="button" class={danger} disabled data-testid="allowlist-remove">Remove</button>
          {/if}
        </div>
        {#if blocked}<p class="m-0 mt-1.5 text-sm text-soft" data-testid="allowlist-blocked">{blocked}</p>{/if}
      </li>
    {/each}
  </ul>
{/if}
