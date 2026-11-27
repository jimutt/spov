<script lang="ts">
  // The fields that describe a job: its goal, kind, repositories,
  // template, model and reasoning mode, time limit, git access and check.
  // The job form and the schedule form both draw them, so a schedule
  // describes its job exactly as a job is described.
  //
  // @decision(DL-061) @decision(DL-195)
  import {
    clockOptions,
    defaultAccessLabel,
    defaultClockLabel,
    KINDS,
    KIND_BLURB,
    defaultModelNote,
    defaultOptionLabel,
    modelGroups,
    suppressImplicitSubmit,
    keyNote,
    type JobDraft,
  } from '../lib/jobform';
  import { kindName } from '../lib/format';
  import { templateAccessLabel } from '../lib/templates';
  import ModelPicker from './ModelPicker.svelte';
  import ReasoningPicker from './ReasoningPicker.svelte';
  import { formModes, keptMode } from '../lib/reasoning';
  import RepoPicker from './RepoPicker.svelte';
  import TemplatePicker from './TemplatePicker.svelte';
  import { field, fieldBase, fieldMono, help, label, more, moreSummary } from './ui';
  import type { components } from '../lib/api/types';

  type Repos = components['schemas']['ReposOutputBody'];
  type Models = components['schemas']['ModelsOutputBody'];
  type Me = components['schemas']['MeView'];
  type Template = components['schemas']['TemplateView'];
  type Read<T> = { data?: T | null; loaded: boolean };

  let {
    draft = $bindable(),
    repos,
    models,
    me,
    templates,
  }: {
    draft: JobDraft;
    repos: Read<Repos>;
    models: Read<Models>;
    me: Read<Me>;
    templates: Read<Template[]>;
  } = $props();

  const groups = $derived(modelGroups(models.data?.providers));
  // @decision(DL-189)
  const modes = $derived(formModes(models.data?.providers, draft.model));
  const keys = $derived(keyNote(groups));
  // @decision(DL-061)
  const accountDefault = $derived(me.data?.default_model ?? '');
  // @decision(DL-049)
  const clockLimit = $derived(me.data?.quota?.max_job_duration);
  // A chosen template that sets git access is where the unset value
  // comes from, before the profile. @decision(DL-121) @decision(DL-120) @decision(DL-043)
  const chosenTemplate = $derived((templates.data ?? []).find((t) => t.name === draft.template));
  const accessLabel = $derived(
    templateAccessLabel(chosenTemplate) || defaultAccessLabel(me.data?.default_job_git_access, me.loaded),
  );
  const inheritLabel = $derived(defaultOptionLabel(accountDefault, me.loaded));
  const defaultNote = $derived(defaultModelNote(accountDefault, me.loaded, groups));
</script>

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
          : 'border-rule-2 bg-paper hover:bg-sunk'}"
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

<!-- @decision(DL-121) -->
<TemplatePicker id="job-template" bind:value={draft.template} templates={templates.data} />

<div class="grid grid-cols-1 gap-x-5 sm:grid-cols-2">
  <div class="mb-7 min-w-0">
    <label class={label} for="job-model">Model</label>
    <!-- @decision(DL-061) -->
    <ModelPicker
      id="job-model"
      bind:value={draft.model}
      {groups}
      defaultLabel={inheritLabel}
      describedby="job-model-notes"
      onchange={() => (draft.mode = keptMode(draft.mode, formModes(models.data?.providers, draft.model)))}
    />
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

  <!-- @decision(DL-189) -->
  <ReasoningPicker id="job-reasoning" bind:value={draft.mode} {modes} known={models.loaded} />

  <div class="mb-7 min-w-0">
    <label class={label} for="job-clock">Time limit</label>
    <select id="job-clock" class={field} bind:value={draft.clock} aria-describedby="job-clock-note">
      <!-- @decision(DL-049) -->
      <option value="">{defaultClockLabel(clockLimit, me.data?.default_job_duration)}</option>
      {#each clockOptions(clockLimit) as clock (clock)}
        <option value={clock}>{clock}</option>
      {/each}
      <!-- A value set elsewhere (the CLI) that the list does not offer. @decision(DL-195) -->
      {#if draft.clock && !clockOptions(clockLimit).includes(draft.clock)}<option value={draft.clock}>{draft.clock}</option>{/if}
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
