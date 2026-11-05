<script lang="ts">
  // Start a job from the browser. POSTs /api/v1/jobs — the same
  // endpoint `spov jobs create` uses — and on success leaves for the session's
  // page. The logic is in ../lib/jobform.ts so a test can reach it.
  //
  // @decision(DL-061) @decision(DL-056)
  import { api, nav, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import {
    Committed,
    clockOptions,
    defaultAccessLabel,
    defaultClockLabel,
    KINDS,
    KIND_BLURB,
    createAndGo,
    defaultModelNote,
    defaultOptionLabel,
    draftProblems,
    emptyDraft,
    modelGroups,
    suppressImplicitSubmit,
    keyNote,
    repoPhrase,
    runsFor,
    spokenLength,
  } from '../lib/jobform';
  import { kindName, sentence } from '../lib/format';
  import Problem from './Problem.svelte';
  import ModelPicker from './ModelPicker.svelte';
  import RepoPicker from './RepoPicker.svelte';
  import { field, fieldBase, fieldMono, help, label, more, moreSummary, pageTitle, primaryLarge } from './ui';

  const repos = resource(async (signal) => unwrap(await api.GET('/api/v1/repos', { signal })));
  const models = resource(async (signal) => unwrap(await api.GET('/api/v1/models', { signal })));
  // A third request on load, and the form reads none of them on a
  // timer. Without it the select cannot say what leaving it alone will
  // actually run (DL-014).
  //
  // @decision(DL-061)
  const me = resource(async (signal) => unwrap(await api.GET('/api/v1/me', { signal })));

  let draft = $state(emptyDraft());
  let starting = $state(false);
  let failure = $state<unknown>(undefined);

  const groups = $derived(modelGroups(models.data?.providers));
  const problems = $derived(draftProblems(draft));
  const keys = $derived(keyNote(groups));
  // @decision(DL-061)
  const accountDefault = $derived(me.data?.default_model ?? '');
  // @decision(DL-049)
  const clockLimit = $derived(me.data?.quota?.max_job_duration);
  // @decision(DL-043)
  const accessLabel = $derived(defaultAccessLabel(me.data?.default_job_git_access, me.loaded));
  const inheritLabel = $derived(defaultOptionLabel(accountDefault, me.loaded));
  const defaultNote = $derived(defaultModelNote(accountDefault, me.loaded, groups));
  // What the submit bar says the job will spend. @decision(DL-056)
  const runs = $derived(runsFor(draft.clock, clockLimit, me.data?.default_job_duration));

  async function start(event: SubmitEvent) {
    event.preventDefault();
    if (problems.length > 0 || starting) return;
    starting = true;
    failure = undefined;
    try {
      await createAndGo(
        draft,
        (body) => api.POST('/api/v1/jobs', { body }),
        nav.go,
      );
    } catch (err) {
      failure = err;
      // The submit is re-enabled ONLY for a refusal. A Committed error
      // is a job the server already has, and this endpoint has no
      // idempotency key — so offering the button again would buy a
      // second sandbox for a job that exists. A success navigates away
      // and must not re-enable either.
      if (!(err instanceof Committed)) starting = false;
    }
  }
</script>


<div class="max-w-[720px]">
  <h1 class="{pageTitle} mb-1.5">New job</h1>
  <p class="mt-0 mb-8 max-w-[64ch] text-soft">
    An agent works in a sandbox until it says it is done. Then spov judges what it left behind, with
    your check if you give one.
  </p>

  {#if failure}
    <div class="mb-6"><Problem error={failure} what="that job" /></div>
  {/if}
  {#if repos.error}
    <div class="mb-6"><Problem error={repos.error} what="your repositories" /></div>
  {/if}
  {#if models.error}
    <div class="mb-6"><Problem error={models.error} what="the model list" /></div>
  {/if}
  <!-- Reported rather than swallowed: without /me the select cannot say
       which model leaving it alone will run, and a form that quietly
       stopped knowing that is the mismatch DL-061 exists to prevent.
       @decision(DL-061) -->
  {#if me.error}
    <div class="mb-6"><Problem error={me.error} what="your account default" /></div>
  {/if}

  <!-- One sentence of help per field; the rest is behind a disclosure
       named for its question, so the sentences that record a decision
       stay one click away. @decision(DL-056) -->
  <form onsubmit={start}>
    <div class="mb-7">
      <label class={label} for="job-goal">Goal</label>
      <textarea
        id="job-goal"
        class="{fieldBase} resize-y px-2.5 py-2 text-read"
        rows="5"
        bind:value={draft.goal}
        placeholder="Fix the flaky test in internal/cli and open a pull request."
        required
      ></textarea>
      <p class={help}>The agent sees this for the whole run. Say what done looks like.</p>
    </div>

    <fieldset class="mb-7 min-w-0 border-0 p-0">
      <legend class={label}>Result</legend>
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        {#each KINDS as kind (kind)}
          <label
            class="flex cursor-pointer gap-2.5 rounded-ctl border px-3.5 py-3 has-focus-visible:outline-2 has-focus-visible:outline-olive {draft.kind ===
            kind
              ? 'border-olive bg-raised shadow-[inset_0_0_0_1px_var(--olive)]'
              : 'border-rule-2 bg-paper hover:bg-raised'}"
          >
            <input type="radio" class="mt-1 accent-olive-b" name="kind" value={kind} bind:group={draft.kind} />
            <span>
              <span class="block font-semibold">{kindName(kind)}</span>
              <span class="mt-0.5 block text-md text-soft">{KIND_BLURB[kind]}</span>
            </span>
          </label>
        {/each}
      </div>
    </fieldset>

    <fieldset class="mb-7 min-w-0 border-0 p-0">
      <legend class={label}>Repositories</legend>
      {#if repos.data?.warnings?.length}
        <p class="mt-0 mb-3 max-w-[64ch] text-md text-gold-t">
          {repos.data.warnings.join(' ')}
        </p>
      {/if}
      <RepoPicker bind:selected={draft.repos} repos={repos.data?.repos ?? []} loaded={repos.loaded} platforms={repos.data?.platforms} />
      <p class={help}>
        Each one is cloned into <code class="font-mono text-ink">/workspace</code>. The agent starts there,
        not inside a repository.
      </p>
    </fieldset>

    <div class="grid grid-cols-1 gap-x-5 sm:grid-cols-2">
      <div class="mb-7 min-w-0">
        <label class={label} for="job-model">Model</label>
        <!-- @decision(DL-061) -->
        <ModelPicker id="job-model" bind:value={draft.model} {groups} defaultLabel={inheritLabel} describedby="job-model-notes" />
        <!-- Pointed at by the select's aria-describedby. Both notes exist
             because a disabled option is commonly skipped by a screen
             reader, and the first one explains the value already selected —
             so prose that is never announced is prose that did not run. -->
        <div id="job-model-notes">
          {#if defaultNote}
            <p class={help}>{defaultNote}</p>
          {/if}
          {#if keys}
            <p class={help}>{keys}</p>
          {/if}
        </div>
      </div>

      <div class="mb-7 min-w-0">
        <label class={label} for="job-clock">Time limit</label>
        <select id="job-clock" class={field} bind:value={draft.clock} aria-describedby="job-clock-note">
          <!-- @decision(DL-049) -->
          <option value="">{defaultClockLabel(clockLimit, me.data?.default_job_duration)}</option>
          {#each clockOptions(clockLimit) as clock (clock)}
            <option value={clock}>{clock}</option>
          {/each}
        </select>
        <p id="job-clock-note" class={help}>When it runs out, spov stops the job, keeps what it made and judges it.</p>
        <details class={more}>
          <summary class={moreSummary}>How the time limit counts</summary>
          <p class="mt-1.5 mb-0">
            Time you spend steering the job does not count, and resuming it continues with what is left.
            The default is set in Settings.
          </p>
        </details>
      </div>
    </div>

    <div class="grid grid-cols-1 gap-x-5 sm:grid-cols-2">
      <!-- @decision(DL-043) -->
      <div class="mb-7 min-w-0">
        <label class={label} for="job-access">Git access</label>
        <select id="job-access" class={field} bind:value={draft.access} aria-describedby="job-access-note">
          <option value="">{accessLabel}</option>
          <option value="repos">This job's repositories</option>
          <option value="all">Every repository your credentials reach</option>
        </select>
        <p id="job-access-note" class={help}>Which repositories the job's git credentials are used on.</p>
        <details class={more}>
          <summary class={moreSummary}>What every repository allows</summary>
          <p class="mt-1.5 mb-0">
            The agent can push to and open pull requests on repositories you did not choose here. Where
            the credential is the deployment's rather than your own, it is allowed only if an admin has
            turned it on, and a job that asks for it is refused otherwise.
          </p>
        </details>
      </div>

      <div class="mb-7 min-w-0">
        <label class={label} for="job-check">Check <span class="font-normal text-soft">(optional)</span></label>
        <input
          id="job-check"
          class={fieldMono}
          bind:value={draft.check}
          placeholder="./spov/scripts/check.sh"
          autocomplete="off"
          spellcheck="false"
          onkeydown={suppressImplicitSubmit}
        />
        <p class={help}>Run when the work ends, so the verdict has an exit status.</p>
        <details class={more}>
          <summary class={moreSummary}>How the check is run</summary>
          <p class="mt-1.5 mb-0">
            It runs in the sandbox at <code class="font-mono text-ink">/workspace</code>, so name the
            repository: <code class="font-mono text-ink">./&lt;repo&gt;/scripts/check.sh</code>. Without
            it the verdict has only the agent's account of one. A command that cannot be run stops the
            job succeeding, so leaving this empty is better than guessing at it.
          </p>
        </details>
      </div>
    </div>

    <!-- The one warning on this form, beside the button that spends.
         The bold is data, not emphasis. @decision(DL-056) @decision(DL-061) -->
    <div
      class="sticky bottom-0 mt-9 flex flex-wrap items-center justify-between gap-x-5 gap-y-2.5 border-t border-rule bg-paper pt-3.5 pb-4"
    >
      {#if problems.length > 0}
        <p class="m-0 max-w-[52ch] text-md text-soft">{sentence(problems.join('; '))}.</p>
      {:else}
        <p class="m-0 max-w-[52ch] text-md text-soft">
          Works unattended for up to <b class="font-semibold text-ink"
            >{runs === null ? 'its time limit' : spokenLength(runs)}</b
          >
          on <b class="font-semibold text-ink">{repoPhrase(draft.repos)}</b>. Its sandbox is billed for as long as
          it runs, which can be longer.
        </p>
      {/if}
      <button type="submit" class={primaryLarge} disabled={starting || problems.length > 0}
        >{starting ? 'Starting…' : 'Start job'}</button
      >
    </div>
  </form>
</div>
