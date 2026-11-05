<script lang="ts">
  // Start an interactive session from the browser: repositories, then
  // Start. Everything else is the account's interactive default. POSTs
  // /api/v1/sessions and leaves for the session's page, which shows the
  // launch. The logic is in ../lib/sessionform.ts.
  //
  // @decision(DL-061)
  import { api, nav, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import { Committed, defaultOptionLabel, keyNote, modelGroups, type RepoRef } from '../lib/jobform';
  import { sessionProblems, startAndGo } from '../lib/sessionform';
  import { sentence } from '../lib/format';
  import ModelPicker from './ModelPicker.svelte';
  import Problem from './Problem.svelte';
  import RepoPicker from './RepoPicker.svelte';
  import { help, label, pageTitle, primaryLarge } from './ui';

  const repos = resource(async (signal) => unwrap(await api.GET('/api/v1/repos', { signal })));
  // @decision(DL-061)
  const models = resource(async (signal) => unwrap(await api.GET('/api/v1/models', { signal })));
  const me = resource(async (signal) => unwrap(await api.GET('/api/v1/me', { signal })));

  let selected = $state<RepoRef[]>([]);
  let model = $state('');
  const groups = $derived(modelGroups(models.data?.providers));
  const keys = $derived(keyNote(groups));
  const inheritLabel = $derived(defaultOptionLabel(me.data?.default_model ?? '', me.loaded));
  let starting = $state(false);
  let failure = $state<unknown>(undefined);
  const problems = $derived(sessionProblems(selected));

  async function start(event: SubmitEvent) {
    event.preventDefault();
    if (problems.length > 0 || starting) return;
    starting = true;
    failure = undefined;
    try {
      await startAndGo(selected, (body) => api.POST('/api/v1/sessions', { body }), nav.go, model);
    } catch (err) {
      failure = err;
      // Re-enabled only for a refusal: a Committed session exists, and a
      // second press would buy a second sandbox. @decision(DL-061)
      if (!(err instanceof Committed)) starting = false;
    }
  }
</script>

<div class="max-w-[720px]">
  <h1 class="{pageTitle} mb-1.5">New session</h1>
  <p class="mt-0 mb-8 max-w-[64ch] text-soft">
    A sandbox with your repositories and an agent you talk to from its page. Its git access and limits are
    your defaults, set in Settings.
  </p>

  {#if failure}
    <div class="mb-6"><Problem error={failure} what="that session" /></div>
  {/if}
  {#if repos.error}
    <div class="mb-6"><Problem error={repos.error} what="your repositories" /></div>
  {/if}

  <form onsubmit={start}>
    <fieldset class="mb-7 min-w-0 border-0 p-0">
      <legend class={label}>Repositories</legend>
      {#if repos.data?.warnings?.length}
        <p class="mt-0 mb-3 max-w-[64ch] text-md text-gold-t">{repos.data.warnings.join(' ')}</p>
      {/if}
      <RepoPicker bind:selected repos={repos.data?.repos ?? []} loaded={repos.loaded} platforms={repos.data?.platforms} />
      <p class={help}>
        Each one is cloned into <code class="font-mono text-ink">/workspace</code>.
      </p>
    </fieldset>

    <!-- Optional: left alone, the session runs on your default.
         @decision(DL-061) -->
    <div class="mb-7 max-w-[46ch]">
      <label class={label} for="session-model">Model <span class="font-normal text-soft">(optional)</span></label>
      <ModelPicker id="session-model" bind:value={model} {groups} defaultLabel={inheritLabel} describedby="session-model-note" />
      {#if keys}<p id="session-model-note" class={help}>{keys}</p>{/if}
      {#if models.error}<div class="mt-2"><Problem error={models.error} what="the model list" /></div>{/if}
    </div>

    <div class="mt-9 flex flex-wrap items-center justify-between gap-x-5 gap-y-2.5 border-t border-rule pt-3.5 pb-4">
      <p class="m-0 max-w-[52ch] text-md text-soft">
        {#if problems.length > 0}{sentence(problems.join('; '))}.{:else}Its sandbox is billed until you end the
          session or it goes idle.{/if}
      </p>
      <button type="submit" class={primaryLarge} disabled={starting || problems.length > 0}
        >{starting ? 'Starting…' : 'Start session'}</button
      >
    </div>
  </form>
</div>
