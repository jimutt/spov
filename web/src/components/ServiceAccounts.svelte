<script lang="ts">
  // Service accounts: the identities pipelines run as, for an admin. A
  // create form, then one row per account with an inline author edit,
  // its tokens on demand and Delete behind a second press. A token the
  // server issues is held in `issued` alone and shown under its account;
  // while it is shown every control that could hide it is disabled, and
  // only Done, leaving the page or its account turning out deleted clear
  // it. One act runs at a time (`acting`), and every act re-reads the
  // list. The logic is in ../lib/serviceaccounts.ts.
  //
  // @decision(DL-142) @decision(DL-140) @decision(DL-141)
  import { tick } from 'svelte';
  import { api, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import { refusalText } from '../lib/allowlist';
  import {
    type Lifetime,
    type ServiceAccountView,
    type ServiceTokenView,
    DefaultDays,
    MaxDays,
    accountGone,
    accountPath,
    authorPatch,
    copyText,
    createRequest,
    dayText,
    deletedText,
    deletionQuestion,
    expiryText,
    isDeleted,
    issuerText,
    labelProblem,
    lastUsedText,
    lifetimeProblem,
    nameProblem,
    partialDeletion,
    sortAccounts,
    tokenCountText,
    tokenPath,
    tokenRequest,
  } from '../lib/serviceaccounts';
  import ConfirmAct from './ConfirmAct.svelte';
  import Problem from './Problem.svelte';
  import { field, help, label as fieldLabel, primary, secondary, sectionTitle } from './ui';

  // A 501 is a server with no service accounts (local mode): the
  // section is not drawn at all, as the People section does. Every
  // answer, the resource's own re-reads included, drops what belongs to
  // an account it no longer lists as live.
  let unsupported = $state(false);
  const list = resource(async (signal) => {
    const r = await api.GET('/api/v1/service-accounts', { signal });
    if (r.response.status === 501) unsupported = true;
    const body = await unwrap(r);
    if (!signal.aborted) settle(body.accounts);
    return body;
  });

  // The act in flight, named so only its own control says Working.
  let acting = $state('');
  const pending = $derived(acting !== '');

  // The create form.
  let name = $state('');
  let newGitName = $state('');
  let newGitEmail = $state('');
  let createFailure = $state<unknown>(undefined);
  let created = $state('');

  // A refused row act, shown above the list.
  let rowFailure = $state<unknown>(undefined);
  // A delete whose off-boarding did not finish; its row offers Delete again.
  let partial = $state('');

  // The inline author edit, on one row at a time.
  let editing = $state<{ name: string; gitName: string; gitEmail: string } | null>(null);
  let editFailure = $state<unknown>(undefined);

  // The open account's tokens. `tokenRead` numbers the reads, so only
  // the newest one's answer is drawn.
  let open = $state<string | null>(null);
  let tokens = $state<ServiceTokenView[] | null>(null);
  let tokenRead = 0;
  let tokensFailure = $state<unknown>(undefined);
  let tokenFailure = $state<unknown>(undefined);
  let tokenLabel = $state('');
  let lifetime = $state<Lifetime>('default');
  let days = $state('');
  let issueFailure = $state<unknown>(undefined);
  // The one place an issued token is held.
  let issued = $state<{ account: string; label: string; token: string } | null>(null);
  let copied = $state<'' | 'copied' | 'failed'>('');
  let panel = $state<HTMLElement | undefined>(undefined);
  let labelField = $state<HTMLInputElement | undefined>(undefined);

  const shown = $derived(issued !== null);
  const accounts = $derived(sortAccounts(list.data?.accounts ?? []));
  const typedName = $derived(name.trim() ? nameProblem(name) : '');
  const typedLabel = $derived(tokenLabel.trim() ? labelProblem(tokenLabel) : '');
  const typedDays = $derived(lifetimeProblem(lifetime, days));
  const origin = typeof location === 'undefined' ? '' : location.origin;
  // Taken again whenever an answer arrives, so the dates stay relative
  // to the time they were read.
  const now = $derived.by(() => {
    void list.data;
    void tokens;
    return new Date();
  });

  function forget() {
    issued = null;
    copied = '';
  }

  function settle(listed: readonly ServiceAccountView[] | null) {
    if (issued && accountGone(listed, issued.account)) forget();
    if (open && accountGone(listed, open)) {
      open = null;
      tokens = null;
    }
    if (editing && accountGone(listed, editing.name)) editing = null;
  }

  async function done() {
    forget();
    await tick();
    labelField?.focus();
  }

  async function create(e: SubmitEvent) {
    e.preventDefault();
    if (pending || shown || nameProblem(name)) return;
    acting = 'create';
    createFailure = undefined;
    created = '';
    try {
      const a = await unwrap(
        await api.POST('/api/v1/service-accounts', { body: createRequest(name, newGitName, newGitEmail) }),
      );
      created = a.name;
      name = '';
      newGitName = '';
      newGitEmail = '';
    } catch (err) {
      createFailure = err;
    } finally {
      acting = '';
    }
    await list.refresh();
  }

  function startEdit(a: ServiceAccountView) {
    if (shown) return;
    editing = { name: a.name, gitName: a.git_name, gitEmail: a.git_email };
    editFailure = undefined;
  }

  async function saveEdit(e: SubmitEvent, a: ServiceAccountView) {
    e.preventDefault();
    const path = accountPath(a.name);
    const body = editing ? authorPatch(a, editing.gitName, editing.gitEmail) : null;
    if (pending || !path || !body) return;
    acting = 'edit';
    editFailure = undefined;
    try {
      await unwrap(await api.PATCH('/api/v1/service-accounts/{name}', { params: { path: { name: path } }, body }));
      editing = null;
    } catch (err) {
      editFailure = err;
    } finally {
      acting = '';
    }
    await list.refresh();
  }

  async function remove(a: ServiceAccountView) {
    const path = accountPath(a.name);
    if (pending || shown || !path) return;
    acting = `delete:${a.name}`;
    rowFailure = undefined;
    partial = '';
    let failure: unknown;
    if (open === a.name) closeTokens();
    if (editing?.name === a.name) editing = null;
    try {
      await unwrap(await api.DELETE('/api/v1/service-accounts/{name}', { params: { path: { name: path } } }));
    } catch (err) {
      failure = err;
      rowFailure = err;
    } finally {
      acting = '';
    }
    await list.refresh();
    if (failure !== undefined && partialDeletion(failure, list.data?.accounts, a.name)) partial = a.name;
  }

  function closeTokens() {
    open = null;
    tokens = null;
    tokenRead++;
  }

  function openTokens(account: string) {
    open = account;
    tokens = null;
    tokensFailure = undefined;
    tokenFailure = undefined;
    issueFailure = undefined;
    tokenLabel = '';
    lifetime = 'default';
    days = '';
    void loadTokens(account);
  }

  function toggleTokens(a: ServiceAccountView) {
    if (pending || shown) return;
    const was = open;
    closeTokens();
    if (was !== a.name) openTokens(a.name);
  }

  async function loadTokens(account: string) {
    const path = accountPath(account);
    if (!path) return;
    const seq = ++tokenRead;
    try {
      const body = await unwrap(
        await api.GET('/api/v1/service-accounts/{name}/tokens', { params: { path: { name: path } } }),
      );
      if (seq === tokenRead && open === account) {
        tokens = body.tokens ?? [];
        tokensFailure = undefined;
      }
    } catch (err) {
      if (seq === tokenRead && open === account) tokensFailure = err;
    }
  }

  async function issue(e: SubmitEvent) {
    e.preventDefault();
    const account = open;
    const path = account ? accountPath(account) : null;
    if (pending || shown || !account || !path || labelProblem(tokenLabel) || lifetimeProblem(lifetime, days)) return;
    acting = 'issue';
    issueFailure = undefined;
    try {
      const t = await unwrap(
        await api.POST('/api/v1/service-accounts/{name}/tokens', {
          params: { path: { name: path } },
          body: tokenRequest(tokenLabel, lifetime, days),
        }),
      );
      // Kept and shown under its account whatever is open now.
      if (open !== account) openTokens(account);
      issued = { account, label: t.name, token: t.token };
      copied = '';
      tokenLabel = '';
      lifetime = 'default';
      days = '';
    } catch (err) {
      if (open === account) issueFailure = err;
    } finally {
      acting = '';
    }
    if (issued) {
      await tick();
      panel?.focus();
    }
    await Promise.all([loadTokens(account), list.refresh()]);
  }

  async function revoke(t: ServiceTokenView) {
    const account = open;
    const path = account ? accountPath(account) : null;
    const id = tokenPath(t.id);
    if (pending || shown || !account || !path || !id) return;
    acting = 'revoke';
    tokenFailure = undefined;
    try {
      await unwrap(
        await api.DELETE('/api/v1/service-accounts/{name}/tokens/{id}', { params: { path: { name: path, id } } }),
      );
    } catch (err) {
      if (open === account) tokenFailure = err;
    } finally {
      acting = '';
    }
    await Promise.all([loadTokens(account), list.refresh()]);
  }

  async function copy() {
    if (!issued) return;
    copied = (await copyText(issued.token, navigator.clipboard)) ? 'copied' : 'failed';
  }
</script>

<!-- A page put away (closed, navigated from, or into the back/forward
     cache) takes the token with it. -->
<svelte:window onpagehide={forget} />

<!-- Nothing until the server has answered, so a local server's 501 does
     not flash the heading. -->
{#if !unsupported && (list.data || list.error)}
  <h2 class="{sectionTitle} mb-1.5">Service accounts</h2>
  <p class="mt-0 mb-5 max-w-[64ch] text-soft">
    Accounts a pipeline runs as, so its jobs are not a person's. They never sign in; they use a token issued here.
    Only admins see this section.
  </p>

  <form class="mb-6 max-w-[46ch]" onsubmit={create} data-testid="service-account-create" novalidate>
    <label class={fieldLabel} for="service-account-name">Name</label>
    <input
      id="service-account-name"
      class={field}
      bind:value={name}
      placeholder="release-bot"
      autocomplete="off"
      autocapitalize="off"
      spellcheck="false"
      aria-invalid={typedName !== '' || createFailure !== undefined}
      aria-describedby="service-account-name-help"
    />
    <p id="service-account-name-help" class="{help} mb-3" data-testid="service-account-name-note">
      {typedName || 'Lower-case letters, digits and hyphens. A name cannot be used again once deleted.'}
    </p>
    <label class={fieldLabel} for="service-account-git-name">Commit author name</label>
    <input
      id="service-account-git-name"
      class="{field} mb-3"
      bind:value={newGitName}
      placeholder="The account's name"
      autocomplete="off"
    />
    <label class={fieldLabel} for="service-account-git-email">Commit author address</label>
    <input
      id="service-account-git-email"
      type="email"
      class={field}
      bind:value={newGitEmail}
      placeholder="name@service.invalid"
      autocomplete="off"
      spellcheck="false"
    />
    <p class="{help} mb-3">Leave either empty for the default shown.</p>
    <button type="submit" class={primary} disabled={pending || shown || nameProblem(name) !== ''}
      >{acting === 'create' ? 'Working…' : 'Create'}</button
    >
    {#if createFailure}
      <p class="m-0 mt-3 max-w-[64ch] text-md text-rust" role="alert" data-testid="service-account-create-failure">
        {refusalText(createFailure)}
      </p>
    {/if}
    {#if created}
      <p class="mt-3 mb-0 text-md text-ink" role="status" data-testid="service-account-created">
        Created {created}. Open its tokens to issue one.
      </p>
    {/if}
  </form>

  {#if list.error}
    <div class="mb-5"><Problem error={list.error} what="the service accounts" /></div>
  {/if}
  {#if rowFailure}
    <div class="mb-3" data-testid="service-account-row-failure"><Problem error={rowFailure} what="that change" /></div>
  {/if}
  {#if partial}
    <p class="m-0 mb-5 max-w-[64ch] text-md text-soft" data-testid="service-account-partial">
      {partial} is deleted, but not everything of its was ended. Delete again on its row to finish.
    </p>
  {/if}

  <ul class="m-0 max-w-[64ch] list-none p-0" data-testid="service-accounts">
    {#each accounts as a (a.name)}
      {@const deleted = isDeleted(a)}
      <li class="border-t border-rule py-3" data-testid="service-account">
        <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
          <span class="font-semibold text-ink break-all">{a.name}</span>
          {#if deleted}
            <span class="text-sm text-rust" data-testid="service-account-deleted">{deletedText(a, now)}</span>
          {/if}
        </div>
        <p class="m-0 mt-0.5 text-sm text-soft break-all" data-testid="service-account-author">
          {a.git_name} &lt;{a.git_email}&gt;
        </p>
        <p class="m-0 mt-0.5 text-sm text-soft" data-testid="service-account-use">
          {tokenCountText(a.tokens)} · {lastUsedText(a.last_used_at, now)}
        </p>

        {#if editing && editing.name === a.name && !deleted}
          <form class="mt-2 max-w-[46ch]" onsubmit={(e) => saveEdit(e, a)} data-testid="service-account-edit" novalidate>
            <label class={fieldLabel} for="service-account-edit-name">Commit author name</label>
            <input id="service-account-edit-name" class="{field} mb-3" bind:value={editing.gitName} autocomplete="off" />
            <label class={fieldLabel} for="service-account-edit-email">Commit author address</label>
            <input
              id="service-account-edit-email"
              type="email"
              class="{field} mb-3"
              bind:value={editing.gitEmail}
              autocomplete="off"
              spellcheck="false"
            />
            {#if editFailure}
              <p class="m-0 mb-3 max-w-[64ch] text-md text-rust" role="alert" data-testid="service-account-edit-failure">
                {refusalText(editFailure)}
              </p>
            {/if}
            <div class="flex flex-wrap gap-2">
              <button
                type="submit"
                class={primary}
                disabled={pending || !authorPatch(a, editing.gitName, editing.gitEmail)}
                >{acting === 'edit' ? 'Working…' : 'Save'}</button
              >
              <button type="button" class={secondary} onclick={() => (editing = null)}>Cancel</button>
            </div>
          </form>
        {/if}

        <div class="mt-2 flex flex-wrap items-start gap-2">
          {#if deleted}
            <button
              type="button"
              class={secondary}
              disabled={pending || shown}
              aria-label={`Delete ${a.name} again`}
              onclick={() => remove(a)}
              data-testid="service-account-delete-again"
              >{acting === `delete:${a.name}` ? 'Working…' : 'Delete again'}</button
            >
          {:else}
            <button
              type="button"
              class={secondary}
              aria-expanded={open === a.name}
              aria-label={`Tokens for ${a.name}`}
              disabled={pending || shown}
              onclick={() => toggleTokens(a)}
              data-testid="service-account-tokens-toggle">Tokens</button
            >
            {#if !(editing && editing.name === a.name)}
              <button
                type="button"
                class={secondary}
                aria-label={`Change author of ${a.name}`}
                disabled={shown}
                onclick={() => startEdit(a)}
                data-testid="service-account-edit-open">Change author</button
              >
            {/if}
            <ConfirmAct
              label="Delete"
              question={deletionQuestion(a)}
              yes="Delete"
              run={() => remove(a)}
              testid="service-account-delete"
              name={`Delete ${a.name}`}
              disabled={pending || shown}
            />
          {/if}
        </div>

        {#if open === a.name && !deleted}
          <div class="mt-3 border-l-2 border-rule-2 pl-3" data-testid="service-account-tokens">
            {#if issued && issued.account === a.name}
              <div
                class="mb-4 border border-olive/40 bg-olive/[0.07] px-4 py-3"
                role="region"
                aria-label="The new token"
                tabindex="-1"
                bind:this={panel}
                data-testid="service-token-issued"
              >
                <p class="m-0 mb-1 text-base font-semibold text-ink">Token {issued.label} for {issued.account}</p>
                <p class="m-0 mb-2 max-w-[64ch] text-md text-soft">
                  This is the only time it is shown, and it cannot be shown again. Put it in the pipeline's secret store
                  now. It stays on this page until you press Done or leave the page.
                </p>
                <code
                  class="mb-2 block select-all break-all bg-raised px-2.5 py-2 font-mono text-md text-ink"
                  data-testid="service-token-text">{issued.token}</code
                >
                <p class="m-0 mb-3 max-w-[64ch] text-md text-soft">
                  The pipeline sets <code class="font-mono text-ink">SPOV_SERVER</code> to
                  <code class="font-mono text-ink break-all">{origin}</code> and
                  <code class="font-mono text-ink">SPOV_SERVER_TOKEN</code> to this token.
                </p>
                <div class="flex flex-wrap gap-2">
                  <button type="button" class={secondary} onclick={copy} data-testid="service-token-copy"
                    >{copied === 'copied'
                      ? 'Copied'
                      : copied === 'failed'
                        ? 'Could not copy: select the text'
                        : 'Copy'}</button
                  >
                  <button type="button" class={primary} onclick={done} data-testid="service-token-done">Done</button>
                </div>
                <p class="m-0 mt-2 text-sm text-soft" data-testid="service-token-hold">
                  Press Done before doing anything else here.
                </p>
              </div>
            {/if}

            {#if tokensFailure}
              <div class="mb-3"><Problem error={tokensFailure} what="its tokens" /></div>
            {/if}
            {#if tokenFailure}
              <div class="mb-3" data-testid="service-token-failure"><Problem error={tokenFailure} what="that token" /></div>
            {/if}

            {#if tokens}
              {#if tokens.length === 0}
                <p class="m-0 mb-3 text-sm text-soft" data-testid="service-tokens-none">No tokens.</p>
              {/if}
              <ul class="m-0 mb-3 list-none p-0" data-testid="service-tokens">
                {#each tokens as t (t.id)}
                  <li class="border-t border-rule py-2" data-testid="service-token">
                    <span class="font-semibold text-ink break-all">{t.name}</span>
                    <p class="m-0 mt-0.5 text-sm text-soft break-all">
                      Issued {dayText(t.created_at, now)} by {issuerText(t)} · {expiryText(t, now)} · {lastUsedText(
                        t.last_used_at,
                        now,
                      )}
                    </p>
                    <div class="mt-1.5">
                      <ConfirmAct
                        label="Revoke"
                        question={`Revoke ${t.name}? A pipeline using it is refused from now on.`}
                        yes="Revoke"
                        run={() => revoke(t)}
                        testid="service-token-revoke"
                        name={`Revoke token ${t.name} of ${a.name}`}
                        disabled={pending || shown}
                      />
                    </div>
                  </li>
                {/each}
              </ul>
            {/if}

            <form class="max-w-[46ch]" onsubmit={issue} data-testid="service-token-create" novalidate>
              <label class={fieldLabel} for="service-token-label">Token label</label>
              <input
                id="service-token-label"
                class={field}
                bind:value={tokenLabel}
                bind:this={labelField}
                placeholder="release workflow"
                autocomplete="off"
                aria-invalid={typedLabel !== ''}
                aria-describedby="service-token-label-help"
              />
              <p id="service-token-label-help" class="{help} mb-3">
                {typedLabel || 'Which pipeline holds it, so you know which to revoke.'}
              </p>
              <label class={fieldLabel} for="service-token-lifetime">Lifetime</label>
              <select id="service-token-lifetime" class="{field} mb-3" bind:value={lifetime}>
                <option value="default">{DefaultDays} days</option>
                <option value="days">Another number of days</option>
                <option value="never">Never expires</option>
              </select>
              {#if lifetime === 'days'}
                <label class={fieldLabel} for="service-token-days">Days</label>
                <input
                  id="service-token-days"
                  class={field}
                  bind:value={days}
                  inputmode="numeric"
                  autocomplete="off"
                  aria-invalid={days.trim() !== '' && typedDays !== ''}
                  aria-describedby="service-token-days-help"
                />
                <p id="service-token-days-help" class="{help} mb-3" data-testid="service-token-days-note">
                  {days.trim() && typedDays ? typedDays : `1 to ${MaxDays}.`}
                </p>
              {/if}
              <button
                type="submit"
                class={primary}
                disabled={pending || shown || labelProblem(tokenLabel) !== '' || typedDays !== ''}
                >{acting === 'issue' ? 'Working…' : 'Issue token'}</button
              >
              {#if issueFailure}
                <p class="m-0 mt-3 max-w-[64ch] text-md text-rust" role="alert" data-testid="service-token-create-failure">
                  {refusalText(issueFailure)}
                </p>
              {/if}
            </form>
          </div>
        {/if}
      </li>
    {/each}
  </ul>
{/if}
