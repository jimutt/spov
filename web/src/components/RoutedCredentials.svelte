<script lang="ts">
  // Routed credentials: what is stored and where each is sent, a form to
  // add or replace one, and Delete. The secret is typed into the form,
  // held only in its state and cleared when the write settles; no
  // response carries one back. The form's logic is ../lib/routedform.ts.
  //
  // @decision(DL-118) @decision(DL-077) @decision(DL-073)
  import { tick } from 'svelte';
  import { api, unwrap, ApiError } from '../lib/api/client';
  import { routedCredentialLine, sentence } from '../lib/format';
  import { resource } from '../lib/resource.svelte';
  import {
    PRESETS,
    PRESET_BLURB,
    draftFromRoute,
    emptyRouteDraft,
    heldNames,
    noEnterSubmit,
    routeProblems,
    storedNote,
    toRoutedBody,
    type Owner,
    type Route,
    type RoutedBody,
  } from '../lib/routedform';
  import ConfirmAct from './ConfirmAct.svelte';
  import Done from './Done.svelte';
  import Problem from './Problem.svelte';
  import { danger, field, fieldMono, help, label, primary, secondary, sectionTitle } from './ui';

  // Every member sees the organisation's, which their sessions get too;
  // only an admin can change one. @decision(DL-074)
  let { admin = false }: { admin?: boolean } = $props();

  // A server that can hold none answers 501, which is a state to show,
  // not a failure.
  const routes = resource(async (signal) => {
    const r = await api.GET('/api/v1/me/routed-credentials', { signal });
    if (r.response.status === 501) return null;
    return unwrap(r);
  });

  const orgRoutes = resource(async (signal) => {
    const r = await api.GET('/api/v1/org/routed-credentials', { signal });
    if (r.response.status === 501) return null;
    return unwrap(r);
  });

  let failure = $state<unknown>(undefined);
  const list = $derived(routes.data?.routed_credentials ?? []);
  const orgList = $derived(orgRoutes.data?.routed_credentials ?? []);
  // The owner choice is an admin's, and only where the organisation's
  // endpoints answered: a local server's 501 leaves it out.
  const canStoreForOrg = $derived(admin && !!orgRoutes.data);

  // One form for an add and a replace; null is closed.
  let mode = $state<'add' | 'replace' | null>(null);
  let draft = $state(emptyRouteDraft());
  let saving = $state(false);
  let note = $state('');
  let formEl = $state<HTMLFormElement | undefined>(undefined);

  const problems = $derived(
    mode ? routeProblems(draft, mode, heldNames(draft.owner === 'org' ? orgList : list)) : [],
  );
  // Replacing the organisation's reaches every member's sessions, so
  // its save is a second press, as its Delete is.
  const confirmFirst = $derived(mode === 'replace' && draft.owner === 'org');

  async function open(next: 'add' | 'replace', from?: { route: Route; owner: Owner }) {
    failure = undefined;
    note = '';
    draft = from ? draftFromRoute(from.route, from.owner) : emptyRouteDraft();
    mode = next;
    await tick();
    formEl?.querySelector<HTMLElement>('input:not([disabled]), select')?.focus();
  }

  function close() {
    mode = null;
    draft = emptyRouteDraft();
  }

  async function put(owner: Owner, name: string, body: RoutedBody) {
    const params = { path: { name } };
    const r =
      owner === 'org'
        ? await api.PUT('/api/v1/org/routed-credentials/{name}', { params, body })
        : await api.PUT('/api/v1/me/routed-credentials/{name}', { params, body });
    if (!r.response.ok) throw new ApiError(r.response.status, r.error);
  }

  async function save() {
    if (saving || !mode || problems.length > 0) return;
    const owner = draft.owner;
    const body = toRoutedBody(draft);
    saving = true;
    failure = undefined;
    note = '';
    try {
      await put(owner, body.route.name, body);
      note = storedNote(body.route.name, owner);
      close();
      await (owner === 'org' ? orgRoutes : routes).refresh();
    } catch (err) {
      // The typed route stays for another try.
      failure = err;
    } finally {
      // Whatever happened, the secret does not outlive the request.
      draft.secret = '';
      saving = false;
    }
  }

  function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!confirmFirst) void save();
  }

  async function removeOrg(name: string) {
    if (!confirm(`Delete the organisation's routed credential ${name}? Every member's later sessions lose it.`)) return;
    failure = undefined;
    note = '';
    try {
      const r = await api.DELETE('/api/v1/org/routed-credentials/{name}', { params: { path: { name } } });
      if (!r.response.ok) throw new ApiError(r.response.status, r.error);
      await orgRoutes.refresh();
    } catch (err) {
      failure = err;
    }
  }

  async function remove(name: string) {
    if (!confirm(`Delete the routed credential ${name}?`)) return;
    failure = undefined;
    note = '';
    try {
      const r = await api.DELETE('/api/v1/me/routed-credentials/{name}', { params: { path: { name } } });
      if (!r.response.ok) throw new ApiError(r.response.status, r.error);
      await routes.refresh();
    } catch (err) {
      failure = err;
    }
  }
</script>

<h2 class="{sectionTitle} mt-12 mb-1.5">Routed credentials</h2>
<p class="mt-0 mb-5 max-w-[64ch] text-soft">
  Secrets for a private registry or another service, which the sandbox's proxy sets only on the hosts
  they name. Every session gets them, except one stored to attach only when a launch or a sandbox
  template asks for it.
</p>

{#if routes.error}
  <Problem error={routes.error} what="your routed credentials" />
{/if}
{#if failure}
  <div class="mb-5"><Problem error={failure} what="that routed credential" /></div>
{/if}
{#if note}
  <Done status class="mb-5">{note}</Done>
{/if}

{#if routes.data === null}
  <p class="max-w-[62ch] text-soft">This server cannot store routed credentials: it has no encryption key.</p>
{:else if routes.data}
  {#if list.length === 0}
    <p class="max-w-[62ch] text-soft">None stored.</p>
  {:else}
    <div class="mb-5 max-w-[70ch] border-t border-rule" data-testid="routed-credentials">
      {#each list as rc (rc.route.name)}
        <div class="flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-rule py-2.5">
          <span class="font-mono text-md">{rc.route.name}</span>
          <span class="text-md text-soft [overflow-wrap:anywhere]">
            {routedCredentialLine(rc.route)}{rc.route.attach === 'asked' ? ', only when a launch asks' : ''}
          </span>
          {#if rc.missing}
            <span class="text-md text-rust">its secret is missing: replace it</span>
          {/if}
          <span class="ml-auto flex gap-2">
            <button
              type="button"
              class={secondary}
              disabled={saving}
              onclick={() => open('replace', { route: rc.route, owner: 'me' })}>Replace</button
            >
            <button type="button" class={danger} onclick={() => remove(rc.route.name)}>Delete</button>
          </span>
        </div>
      {/each}
    </div>
  {/if}

  {#if mode === null}
    <button type="button" class="{secondary} mb-8" data-testid="routed-add" onclick={() => open('add')}
      >Add credential</button
    >
  {:else}
    <!-- Enter in a field never submits: the button is the only way to
         store a secret. @decision(DL-118) -->
    <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
    <form
      class="mb-8 max-w-[46ch]"
      data-testid="routed-form"
      bind:this={formEl}
      onsubmit={submit}
      onkeydown={noEnterSubmit}
      novalidate
    >
      <h3 class="mt-6 mb-3 text-base font-semibold">
        {#if mode === 'add'}Add a routed credential{:else}Replace <span class="font-mono">{draft.name}</span
          >{draft.owner === 'org' ? ", the organisation's" : ''}{/if}
      </h3>

      {#if mode === 'add' && canStoreForOrg}
        <fieldset class="mb-4 min-w-0 border-0 p-0" data-testid="routed-owner">
          <legend class={label}>Whose</legend>
          <div class="flex flex-wrap gap-x-5 gap-y-1">
            <label class="inline-flex cursor-pointer items-center gap-2">
              <input type="radio" class="accent-olive-b" name="routed-owner" value="me" bind:group={draft.owner} />
              Mine
            </label>
            <label class="inline-flex cursor-pointer items-center gap-2">
              <input type="radio" class="accent-olive-b" name="routed-owner" value="org" bind:group={draft.owner} />
              Organisation
            </label>
          </div>
          {#if draft.owner === 'org'}
            <p class={help}>Every member's sessions get it. One of their own on the same host takes its place.</p>
          {/if}
        </fieldset>
      {/if}

      <label class={label} for="routed-name">Name</label>
      <!-- The name is the key, so a replace cannot change it. -->
      <input
        id="routed-name"
        class="{fieldMono} mb-4"
        bind:value={draft.name}
        disabled={mode === 'replace'}
        placeholder="acme-npm"
        autocomplete="off"
        autocapitalize="off"
        spellcheck="false"
      />

      <label class={label} for="routed-kind">Kind</label>
      <select id="routed-kind" class={field} bind:value={draft.kind}>
        <option value="custom">Hosts and a scheme</option>
        {#each PRESETS as preset (preset)}
          <option value={preset}>{preset} registry</option>
        {/each}
      </select>
      <p class="{help} mb-4">
        {#if draft.kind === 'custom'}
          Any service: you name the hosts and how the secret is sent.
        {:else}
          {PRESET_BLURB[draft.kind]} The client's config is written in the sandbox for it.
        {/if}
      </p>

      {#if draft.kind === 'custom'}
        <label class={label} for="routed-hosts">Hosts</label>
        <textarea
          id="routed-hosts"
          class="{fieldMono} resize-y"
          rows="2"
          bind:value={draft.hosts}
          placeholder="registry.acme.com"
          autocomplete="off"
          autocapitalize="off"
          spellcheck="false"
        ></textarea>
        <p class="{help} mb-4">Exact host names, one per line. The secret is set nowhere else.</p>

        <label class={label} for="routed-paths">Path prefixes <span class="font-normal text-soft">(optional)</span></label>
        <textarea
          id="routed-paths"
          class="{fieldMono} resize-y"
          rows="2"
          bind:value={draft.paths}
          placeholder="/repo/"
          autocomplete="off"
          autocapitalize="off"
          spellcheck="false"
        ></textarea>
        <p class="{help} mb-4">Set it only below these paths on its hosts, one per line.</p>

        <label class={label} for="routed-scheme">Sent as</label>
        <select id="routed-scheme" class="{field} mb-4" bind:value={draft.scheme}>
          <option value="">Choose one</option>
          <option value="bearer">Authorization: Bearer</option>
          <option value="basic">Basic auth, with a username</option>
          <option value="header">The value of a header</option>
        </select>

        {#if draft.scheme === 'basic'}
          <label class={label} for="routed-user">Username</label>
          <input id="routed-user" class="{fieldMono} mb-4" bind:value={draft.user} autocomplete="off" autocapitalize="off" spellcheck="false" />
        {:else if draft.scheme === 'header'}
          <label class={label} for="routed-header">Header name</label>
          <input
            id="routed-header"
            class="{fieldMono} mb-4"
            bind:value={draft.header}
            placeholder="X-Api-Key"
            autocomplete="off"
            spellcheck="false"
          />
        {/if}
      {:else}
        <label class={label} for="routed-url">Registry address</label>
        <input
          id="routed-url"
          class={fieldMono}
          bind:value={draft.url}
          placeholder="https://npm.acme.com/"
          inputmode="url"
          autocomplete="off"
          autocapitalize="off"
          spellcheck="false"
        />
        <p class="{help} mb-4">
          Its host and path are where the secret is set.{#if draft.widened.length > 0}
            This credential was stored for a wider path, which is kept: {draft.widened.join(', ')}.{/if}
        </p>

        {#if draft.kind === 'npm'}
          <label class={label} for="routed-scope">Scope <span class="font-normal text-soft">(optional)</span></label>
          <input
            id="routed-scope"
            class={fieldMono}
            bind:value={draft.scope}
            placeholder="@acme"
            autocomplete="off"
            autocapitalize="off"
            spellcheck="false"
          />
          <p class="{help} mb-4">The scope this registry serves, when it is not the default registry.</p>
        {:else}
          <label class={label} for="routed-user"
            >Username{#if draft.kind !== 'maven'}
              <span class="font-normal text-soft">(optional)</span>{/if}</label
          >
          <input id="routed-user" class="{fieldMono} mb-4" bind:value={draft.user} autocomplete="off" autocapitalize="off" spellcheck="false" />
        {/if}
        {#if draft.kind === 'maven'}
          <label class={label} for="routed-repo-id">Repository id <span class="font-normal text-soft">(optional)</span></label>
          <input id="routed-repo-id" class={fieldMono} bind:value={draft.repoID} autocomplete="off" autocapitalize="off" spellcheck="false" />
          <p class="{help} mb-4">Its id in the project's pom.xml. The credential's name when empty.</p>
        {/if}
      {/if}

      <label class={label} for="routed-attach">Attached</label>
      <select id="routed-attach" class="{field} mb-4" bind:value={draft.attach}>
        <option value="always">To every session</option>
        <option value="asked">Only when a launch or a template asks</option>
      </select>

      <label class={label} for="routed-secret">Secret</label>
      <input
        id="routed-secret"
        type="password"
        class={fieldMono}
        bind:value={draft.secret}
        autocomplete="off"
        autocapitalize="off"
        spellcheck="false"
      />
      <p class="{help} mb-4">
        {#if mode === 'replace'}A replace stores the route and the secret together, so type it again.{/if}
        Stored unchecked and never shown again.
      </p>

      {#if problems.length > 0}
        <p class="mt-0 mb-3 max-w-[52ch] text-md text-soft" data-testid="routed-problems">
          {sentence(problems.join('; '))}.
        </p>
      {/if}
      <div class="flex flex-wrap items-start gap-2">
        {#if confirmFirst && problems.length === 0}
          <ConfirmAct
            label="Replace for everyone"
            question="Replace {draft.name}? Every member's later sessions get the new route and secret."
            yes="Replace it"
            no="Not yet"
            run={save}
            testid="routed-org-replace"
          />
        {:else}
          <button type="submit" class={primary} disabled={saving || problems.length > 0 || confirmFirst}
            >{saving ? 'Storing…' : mode === 'replace' ? 'Replace' : 'Store'}</button
          >
        {/if}
        <button type="button" class={secondary} disabled={saving} onclick={close}>Cancel</button>
      </div>
    </form>
  {/if}
{:else if !routes.error}
  <p class="text-soft">Loading…</p>
{/if}

{#if orgList.length > 0}
  <h3 class="mt-8 mb-3 text-base font-semibold">The organisation's</h3>
  <p class="mt-0 mb-3 max-w-[64ch] text-soft">
    An admin defines these, and your sessions get them too. One of your own on the same host takes its
    place.
  </p>
  <div class="mb-8 max-w-[70ch] border-t border-rule" data-testid="org-routed-credentials">
    {#each orgList as rc (rc.route.name)}
      <div class="flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-rule py-2.5">
        <span class="font-mono text-md">{rc.route.name}</span>
        <span class="text-md text-soft [overflow-wrap:anywhere]">
          {routedCredentialLine(rc.route)}{rc.route.attach === 'asked' ? ', only when a launch asks' : ''}
        </span>
        {#if admin}
          <span class="ml-auto flex gap-2">
            <!-- Only where the personal form can be drawn. -->
            {#if routes.data}
              <button
                type="button"
                class={secondary}
                disabled={saving}
                onclick={() => open('replace', { route: rc.route, owner: 'org' })}>Replace</button
              >
            {/if}
            <button type="button" class={danger} onclick={() => removeOrg(rc.route.name)}>Delete</button>
          </span>
        {/if}
      </div>
    {/each}
  </div>
{/if}
