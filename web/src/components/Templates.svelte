<script lang="ts">
  // Sandbox templates: named bundles of sandbox settings a launch can
  // pick. The list, one form to add or edit, and Delete. Every control
  // has a "not set" state that leaves its setting out of the template.
  // The logic is ../lib/templates.ts.
  //
  // @decision(DL-121) @decision(DL-119)
  import { tick } from 'svelte';
  import { api, unwrap, ApiError } from '../lib/api/client';
  import { sentence } from '../lib/format';
  import { resource } from '../lib/resource.svelte';
  import { noEnterSubmit } from '../lib/routedform';
  import {
    EGRESS,
    credentialRows,
    draftFromTemplate,
    emptyTemplateDraft,
    listTemplates,
    missingLine,
    takesDomains,
    templateProblems,
    templateSummary,
    toTemplateBody,
    togglePicked,
    type Template,
  } from '../lib/templates';
  import ConfirmAct from './ConfirmAct.svelte';
  import Done from './Done.svelte';
  import Problem from './Problem.svelte';
  import { field, fieldMono, help, label, primary, secondary, sectionTitle, textButton } from './ui';

  // Null is a server that keeps none (501): nothing is drawn.
  const templates = resource(listTemplates);
  // The checklist's rows. A server that holds no routed credentials
  // answers 501, which is an empty list here.
  const own = resource(async (signal) => {
    const r = await api.GET('/api/v1/me/routed-credentials', { signal });
    if (r.response.status === 501) return [];
    return (await unwrap(r)).routed_credentials ?? [];
  });
  const org = resource(async (signal) => {
    const r = await api.GET('/api/v1/org/routed-credentials', { signal });
    if (r.response.status === 501) return [];
    return (await unwrap(r)).routed_credentials ?? [];
  });

  const list = $derived(templates.data ?? []);

  // One form for an add and an edit; null is closed. The draft is a
  // copy taken when the form opens and nothing seeds it afterwards, so
  // a refresh of the list never writes over what is being typed.
  let mode = $state<'add' | 'edit' | null>(null);
  let draft = $state(emptyTemplateDraft());
  // One flag for every write of this section. @decision(DL-121)
  let pending = $state(false);
  let failure = $state<unknown>(undefined);
  let note = $state('');
  let formEl = $state<HTMLFormElement | undefined>(undefined);

  const problems = $derived(mode ? templateProblems(draft, mode, list.map((t) => t.name)) : []);
  const rows = $derived(credentialRows(own.data, org.data, draft.picked));
  // An egress an older client or the command stored stays selectable.
  const egressOptions = $derived(
    draft.egress && !(EGRESS as readonly string[]).includes(draft.egress) ? [...EGRESS, draft.egress] : [...EGRESS],
  );

  async function open(next: 'add' | 'edit', from?: Template) {
    failure = undefined;
    note = '';
    draft = from ? draftFromTemplate(from) : emptyTemplateDraft();
    mode = next;
    // A credential stored a moment ago, in the section above, belongs
    // in the checklist.
    void own.refresh();
    void org.refresh();
    await tick();
    formEl?.querySelector<HTMLElement>('input:not([disabled]), select')?.focus();
  }

  function close() {
    mode = null;
    draft = emptyTemplateDraft();
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (pending || !mode || problems.length > 0) return;
    const body = toTemplateBody(draft);
    if (!body) return;
    const name = draft.name.trim();
    pending = true;
    failure = undefined;
    note = '';
    try {
      const r = await api.PUT('/api/v1/me/templates/{name}', { params: { path: { name } }, body });
      if (!r.response.ok) throw new ApiError(r.response.status, r.error);
      note = `Saved ${name}.`;
      close();
      await templates.refresh();
    } catch (err) {
      failure = err;
    } finally {
      pending = false;
    }
  }

  async function remove(name: string) {
    if (pending) return;
    pending = true;
    failure = undefined;
    note = '';
    try {
      const r = await api.DELETE('/api/v1/me/templates/{name}', { params: { path: { name } } });
      if (!r.response.ok) throw new ApiError(r.response.status, r.error);
      if (mode === 'edit' && draft.name === name) close();
    } catch (err) {
      failure = err;
    } finally {
      pending = false;
    }
    await templates.refresh();
  }
</script>

<!-- Nothing until the server has answered, and nothing on a 501. -->
{#if templates.data || (templates.error && !templates.loaded)}
  <section class="mb-12" data-testid="templates">
    <h2 class="{sectionTitle} mb-1.5">Sandbox templates</h2>
    <p class="mt-0 mb-5 max-w-[64ch] text-soft">
      Named sets of sandbox settings you pick when you start a session or a job. A launch that picks none
      is unchanged, and a template sets only what you give it here.
    </p>

    {#if templates.error}
      <div class="mb-5"><Problem error={templates.error} what="your sandbox templates" /></div>
    {/if}
    {#if failure}
      <div class="mb-5"><Problem error={failure} what="that template" /></div>
    {/if}
    {#if note}
      <Done status class="mb-5">{note}</Done>
    {/if}

    {#if templates.data}
      {#if list.length === 0}
        <p class="max-w-[62ch] text-soft">None yet.</p>
      {:else}
        <div class="mb-5 max-w-[70ch] border-t border-rule" data-testid="template-list">
          {#each list as t (t.name)}
            {@const summary = templateSummary(t)}
            {@const missing = missingLine(t)}
            <div class="flex flex-wrap items-start gap-x-4 gap-y-2 border-b border-rule py-2.5">
              <div class="min-w-0 flex-1 basis-[28ch]">
                <p class="m-0 font-mono text-md">{t.name}</p>
                {#if t.description}<p class="m-0 mt-0.5 text-md [overflow-wrap:anywhere]">{t.description}</p>{/if}
                <p class="m-0 mt-0.5 text-md text-soft [overflow-wrap:anywhere]">
                  {summary ? sentence(summary) : 'Sets nothing'}.
                </p>
                {#if missing}<p class="m-0 mt-0.5 text-md text-rust">{missing}</p>{/if}
              </div>
              <div class="ml-auto flex flex-wrap items-start gap-2">
                <button type="button" class={secondary} disabled={pending} onclick={() => open('edit', t)}>Edit</button>
                <ConfirmAct
                  label="Delete"
                  question="Delete {t.name}? Sessions already started from it keep their settings."
                  yes="Delete it"
                  run={() => remove(t.name)}
                  testid="template-delete-{t.name}"
                />
              </div>
            </div>
          {/each}
        </div>
      {/if}

      {#if mode === null}
        <button type="button" class={secondary} data-testid="template-add" onclick={() => open('add')}
          >Add template</button
        >
      {:else}
        <!-- Enter in a field never submits. @decision(DL-121) -->
        <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
        <form
          class="max-w-[46ch]"
          data-testid="template-form"
          bind:this={formEl}
          onsubmit={save}
          onkeydown={noEnterSubmit}
          novalidate
        >
          <h3 class="mt-6 mb-3 text-base font-semibold">
            {#if mode === 'add'}Add a template{:else}Edit <span class="font-mono">{draft.name}</span>{/if}
          </h3>

          <label class={label} for="template-name">Name</label>
          <!-- The name is the key: no rename. -->
          <input
            id="template-name"
            class="{fieldMono} mb-4"
            bind:value={draft.name}
            disabled={mode === 'edit'}
            placeholder="acme-backend"
            autocomplete="off"
            autocapitalize="off"
            spellcheck="false"
          />

          <label class={label} for="template-description"
            >Description <span class="font-normal text-soft">(optional)</span></label
          >
          <input id="template-description" class="{field} mb-4" bind:value={draft.description} maxlength="200" autocomplete="off" />

          <label class={label} for="template-egress">Egress</label>
          <select id="template-egress" class="{field} mb-4" bind:value={draft.egress}>
            <option value="">Not set</option>
            {#each egressOptions as preset (preset)}
              <option value={preset}>{preset}</option>
            {/each}
          </select>
          {#if takesDomains(draft.egress)}
            <label class={label} for="template-domains"
              >More domains <span class="font-normal text-soft">(optional)</span></label
            >
            <textarea
              id="template-domains"
              class="{fieldMono} resize-y"
              rows="2"
              bind:value={draft.domains}
              placeholder="registry.acme.com"
              autocomplete="off"
              autocapitalize="off"
              spellcheck="false"
            ></textarea>
            <p class="{help} mb-4">Added to what {draft.egress} allows, one per line.</p>
          {/if}

          <fieldset class="mb-4 min-w-0 border-0 p-0" data-testid="template-credentials">
            <legend class={label}>Routed credentials</legend>
            <div class="flex flex-col gap-1">
              <label class="inline-flex cursor-pointer items-baseline gap-2">
                <input type="radio" class="accent-olive-b" name="template-credentials" value="default" bind:group={draft.credentials} />
                <span>Default <span class="text-soft">(not set: every one stored to attach always)</span></span>
              </label>
              <label class="inline-flex cursor-pointer items-baseline gap-2">
                <input type="radio" class="accent-olive-b" name="template-credentials" value="none" bind:group={draft.credentials} />
                None
              </label>
              <label class="inline-flex cursor-pointer items-baseline gap-2">
                <input type="radio" class="accent-olive-b" name="template-credentials" value="only" bind:group={draft.credentials} />
                Only these
              </label>
            </div>
            {#if draft.credentials === 'only'}
              <div class="mt-2 overflow-hidden rounded-ctl border border-rule-2 bg-raised">
                {#each rows as row (row.name)}
                  <label class="grid cursor-pointer grid-cols-[auto_1fr] items-baseline gap-2.5 px-2.5 py-[7px] hover:bg-sunk">
                    <input
                      type="checkbox"
                      class="accent-olive-b"
                      checked={row.checked}
                      onchange={() => (draft.picked = togglePicked(draft.picked, row.name))}
                    />
                    <span class="min-w-0">
                      <span class="font-mono text-md">{row.name}</span>
                      {#if row.owner === 'org'}<span class="text-sm text-soft">, the organisation's</span>{/if}
                      <span class="mt-0.5 block text-sm [overflow-wrap:anywhere] {row.owner === 'missing' ? 'text-rust' : 'text-soft'}"
                        >{row.line}</span
                      >
                    </span>
                  </label>
                {:else}
                  <p class="m-0 px-2.5 py-2 text-md text-soft">
                    {own.loaded || own.error ? 'You hold no routed credential yet. Add one above.' : 'Loading…'}
                  </p>
                {/each}
              </div>
              <p class={help}>Exactly these are attached, ones stored to attach only when asked included.</p>
            {/if}
          </fieldset>

          <fieldset class="mb-4 min-w-0 border-0 p-0">
            <legend class={label}>Variables</legend>
            {#each draft.env as row, i (i)}
              <div class="mb-2 grid grid-cols-[1fr_1fr_auto] items-center gap-2">
                <input
                  class="{fieldMono} min-w-0"
                  bind:value={row.name}
                  placeholder="NAME"
                  aria-label="Variable {i + 1} name"
                  autocomplete="off"
                  autocapitalize="off"
                  spellcheck="false"
                />
                <input
                  class="{fieldMono} min-w-0"
                  bind:value={row.value}
                  placeholder="value"
                  aria-label="Variable {i + 1} value"
                  autocomplete="off"
                  autocapitalize="off"
                  spellcheck="false"
                />
                <button type="button" class={textButton} onclick={() => (draft.env = draft.env.filter((_, j) => j !== i))}
                  >Remove<span class="sr-only"> variable {i + 1}</span></button
                >
              </div>
            {/each}
            <button type="button" class={secondary} onclick={() => (draft.env = [...draft.env, { name: '', value: '' }])}
              >Add variable</button
            >
            <p class={help}>Not secret: anyone who can read a session sees them. A token belongs in a routed credential.</p>
          </fieldset>

          <label class={label} for="template-addons">Add-ons</label>
          <textarea
            id="template-addons"
            class="{fieldMono} resize-y"
            rows="2"
            bind:value={draft.addons}
            placeholder="postgres@16"
            autocomplete="off"
            autocapitalize="off"
            spellcheck="false"
          ></textarea>
          <p class="{help} mb-4">One per line, as <code class="font-mono text-ink">name</code> or <code class="font-mono text-ink">name@version</code>. Empty is not set.</p>

          <label class={label} for="template-apt">apt packages</label>
          <textarea
            id="template-apt"
            class="{fieldMono} resize-y"
            rows="2"
            bind:value={draft.apt}
            placeholder="libvips-dev"
            autocomplete="off"
            autocapitalize="off"
            spellcheck="false"
          ></textarea>
          <p class="{help} mb-4">One per line. Empty is not set.</p>

          <label class={label} for="template-access">Git access</label>
          <select id="template-access" class={field} bind:value={draft.access} aria-describedby="template-access-note">
            <option value="">Not set</option>
            <option value="repos">The launch's repositories</option>
            <option value="all">Every repository your credentials reach</option>
          </select>
          <p id="template-access-note" class="mt-1.5 mb-4 max-w-[64ch] text-md {draft.access === 'all' ? 'text-gold-t' : 'text-soft'}">
            {#if draft.access === 'all'}
              The agent can push to and open pull requests on repositories the launch did not choose. Where the
              credential is the deployment's rather than your own, it applies only if an admin has turned it on.
            {:else}
              Which repositories a sandbox's git credentials are used on.
            {/if}
          </p>

          {#if problems.length > 0}
            <p class="mt-0 mb-3 max-w-[52ch] text-md text-soft" data-testid="template-problems">
              {sentence(problems.join('; '))}.
            </p>
          {/if}
          <div class="flex flex-wrap gap-2">
            <button type="submit" class={primary} disabled={pending || problems.length > 0}
              >{pending ? 'Saving…' : 'Save'}</button
            >
            <button type="button" class={secondary} disabled={pending} onclick={close}>Cancel</button>
          </div>
        </form>
      {/if}
    {/if}
  </section>
{/if}
