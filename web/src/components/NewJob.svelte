<script lang="ts">
  // Start a job from the browser. POSTs /api/v1/jobs — the same
  // endpoint `spov jobs create` uses — and on success leaves for the session's
  // page. The logic is in ../lib/jobform.ts so a test can reach it.
  //
  // @decision(DL-061) @decision(DL-056) @decision(DL-121)
  import { api, nav, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import { Committed, createAndGo, draftProblems, emptyDraft, repoPhrase, runsFor, spokenLength } from '../lib/jobform';
  import { sentence } from '../lib/format';
  import { listTemplates } from '../lib/templates';
  import Problem from './Problem.svelte';
  import JobFields from './JobFields.svelte';
  import { pageTitle, primaryLarge } from './ui';

  const repos = resource(async (signal) => unwrap(await api.GET('/api/v1/repos', { signal })));
  const models = resource(async (signal) => unwrap(await api.GET('/api/v1/models', { signal })));
  // A third request on load, and the form reads none of them on a
  // timer. Without it the select cannot say what leaving it alone will
  // actually run (DL-014).
  //
  // @decision(DL-061)
  const me = resource(async (signal) => unwrap(await api.GET('/api/v1/me', { signal })));

  // A failed read draws no picker and stops no launch. @decision(DL-121)
  const templates = resource(listTemplates);

  let draft = $state(emptyDraft());
  let starting = $state(false);
  let failure = $state<unknown>(undefined);

  const problems = $derived(draftProblems(draft));
  // @decision(DL-049)
  const clockLimit = $derived(me.data?.quota?.max_job_duration);
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
        // The job's session page takes the form's place in the history.
        // @decision(DL-156)
        nav.replace,
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
    <!-- @decision(DL-195) -->
    <JobFields bind:draft {repos} {models} {me} {templates} />

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
