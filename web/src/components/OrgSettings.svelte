<script lang="ts">
  // The organisation's GitHub App, for an admin: where it comes from,
  // what it reaches, a form to store or replace it, Remove, and who
  // changed it. The key is write-only. Logic in ../lib/orgsettings.ts;
  // what it all means is docs/github-access.md.
  //
  // @decision(DL-062) @decision(DL-042)
  import { api, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import {
    type AppDraft,
    appProblems,
    checkLine,
    eventLine,
    missingLine,
    sourceLine,
    toAppBody,
  } from '../lib/orgsettings';
  import { clock } from '../lib/format';
  import ConfirmAct from './ConfirmAct.svelte';
  import Problem from './Problem.svelte';
  import { field, fieldMono, help, label as fieldLabel, more, moreSummary, primary, sectionTitle } from './ui';

  // A 501 is a server that keeps no App (local mode): the section is not
  // drawn at all.
  let unsupported = $state(false);
  // One draft per platform, written by its inputs.
  let drafts = $state<Record<string, AppDraft>>({});
  const org = resource(async (signal) => {
    const body = unwrap(
      await api.GET('/api/v1/org/github', { signal }).then((r) => {
        if (r.response.status === 501) unsupported = true;
        return r;
      }),
    );
    return body;
  });

  let saving = $state('');
  let failure = $state<unknown>(undefined);

  function draftFor(platform: string): AppDraft {
    return drafts[platform] ?? { appID: '', account: '', key: '' };
  }

  // Written from an input handler, never while drawing.
  function setDraft(platform: string, field: keyof AppDraft, value: string) {
    drafts[platform] = { ...draftFor(platform), [field]: value };
  }

  async function store(platform: string, e: SubmitEvent) {
    e.preventDefault();
    const d = draftFor(platform);
    if (saving || appProblems(d).length) return;
    saving = platform;
    failure = undefined;
    try {
      await unwrap(await api.PUT('/api/v1/org/github/{platform}', { params: { path: { platform } }, body: toAppBody(d) }));
      // The key is never kept in the page after it is sent.
      drafts[platform] = { appID: '', account: '', key: '' };
      await org.refresh();
    } catch (err) {
      failure = err;
    } finally {
      saving = '';
    }
  }

  async function remove(platform: string) {
    failure = undefined;
    try {
      await unwrap(await api.DELETE('/api/v1/org/github/{platform}', { params: { path: { platform } } }));
    } catch (err) {
      failure = err;
    }
    await org.refresh();
  }
</script>

<!-- Nothing until the server has answered, so a local server's 501 does
     not flash the heading. -->
{#if !unsupported && (org.data || org.error)}
  <h2 class="{sectionTitle} mb-1.5">Organisation</h2>
  <p class="mt-0 mb-1 max-w-[64ch] text-soft">
    The GitHub App everyone on this deployment can use. Only admins see this section.
  </p>
  <details class="{more} mb-5">
    <summary class={moreSummary}>What the App gives people</summary>
    <p class="mt-1.5 mb-0">
      Everyone allowed onto spov can use every repository it reaches. Their own credentials come first, unless their
      CLI config prefers the shared one. Replacing or removing it applies from the next token: running sessions keep
      theirs for up to an hour. See <code class="font-mono text-ink">docs/github-access.md</code>.
    </p>
  </details>

  {#if org.error}
    <div class="mb-5"><Problem error={org.error} what="the organisation's App" /></div>
  {/if}
  {#if failure}
    <div class="mb-5"><Problem error={failure} what="that App" /></div>
  {/if}

  {#each org.data?.platforms ?? [] as v (v.platform)}
    {@const check = checkLine(v)}
    {@const missing = missingLine(v)}
    {@const d = draftFor(v.platform)}
    {@const problems = appProblems(d)}
    <div class="mb-6 max-w-[64ch]" data-testid="org-github">
      <h3 class="m-0 text-base font-semibold">
        {v.platform}{#if v.app_id}<span class="ml-2 font-mono text-md font-normal text-soft">App {v.app_id}</span>{/if}
      </h3>
      <p class="m-0 mt-1 text-md text-soft">{sourceLine(v)}</p>
      {#if check}
        <p class="m-0 mt-1 text-md {check.ok ? 'text-ink' : 'text-rust'}" data-testid="org-github-check">{check.text}</p>
      {/if}
      {#if missing}<p class="m-0 mt-1 text-md text-gold-t">{missing}</p>{/if}
      {#if v.check?.repos?.length}
        <details class="{more} mt-1">
          <summary class={moreSummary}>Repositories it reaches</summary>
          <ul class="m-0 mt-1 list-none p-0 font-mono text-sm text-soft">
            {#each v.check.repos as r (r)}<li>{r}</li>{/each}
            {#if v.check.repo_count > v.check.repos.length}<li>…and {v.check.repo_count - v.check.repos.length} more</li>{/if}
          </ul>
        </details>
      {/if}

      {#if v.source !== 'environment'}
        <form class="mt-4 max-w-[46ch]" onsubmit={(e) => store(v.platform, e)} data-testid="org-github-form">
          <label class={fieldLabel} for="app-id-{v.platform}">{v.source === 'stored' ? 'Replace with' : 'App ID'}</label>
          <input
            id="app-id-{v.platform}"
            class="{field} mb-3"
            value={d.appID}
            oninput={(e) => setDraft(v.platform, 'appID', e.currentTarget.value)}
            placeholder="App ID or client ID"
            autocomplete="off"
          />
          <label class={fieldLabel} for="app-account-{v.platform}">Installed on <span class="font-normal text-soft">(optional)</span></label>
          <input
            id="app-account-{v.platform}"
            class="{field} mb-3"
            value={d.account}
            oninput={(e) => setDraft(v.platform, 'account', e.currentTarget.value)}
            placeholder="the user or organisation; the configured account when empty"
            autocomplete="off"
          />
          <label class={fieldLabel} for="app-key-{v.platform}">Private key</label>
          <textarea
            id="app-key-{v.platform}"
            class="{fieldMono} mb-1 min-h-28"
            value={d.key}
            oninput={(e) => setDraft(v.platform, 'key', e.currentTarget.value)}
            placeholder="-----BEGIN RSA PRIVATE KEY-----"
            autocomplete="off"
            spellcheck="false"
          ></textarea>
          <p class="{help} mb-3">Checked before it is stored, and never shown again.</p>
          <button type="submit" class={primary} disabled={saving !== '' || problems.length > 0}
            >{saving === v.platform ? 'Checking…' : 'Check and store'}</button
          >
        </form>
        {#if v.source === 'stored'}
          <div class="mt-3">
            <ConfirmAct
              label="Remove the App"
              question="Remove it? New tokens stop; running sessions keep theirs until they expire."
              yes="Remove it"
              run={() => remove(v.platform)}
              testid="org-github-remove"
            />
          </div>
        {/if}
      {/if}
    </div>
  {/each}

  {#if org.data?.events?.length}
    <details class="{more} max-w-[64ch]">
      <summary class={moreSummary}>Who changed it</summary>
      <ul class="m-0 mt-1 list-none p-0 text-sm text-soft">
        {#each org.data.events as e, i (i)}
          <li><span class="tabular-nums">{clock(e.at)}</span> {eventLine(e)}</li>
        {/each}
      </ul>
    </details>
  {/if}
{/if}
