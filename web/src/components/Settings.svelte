<script lang="ts">
  // The account's settings, in sections: the default model, then
  // credentials, the OpenCode sign-in, then git access, then the jobs'
  // time limit, then git identity. Sections rather than tabs, since the
  // forms are all short.
  //
  // The logic is in ../lib/settings.ts so a test can reach it.
  //
  // @decision(DL-062) @decision(DL-043) @decision(DL-049) @decision(DL-056)
  import { api, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import {
    NO_DEFAULT,
    accessValue,
    changed,
    emptyField,
    builtInDurationLabel,
    jobDurationOptions,
    jobDurationValue,
    keyNote,
    modelGroups,
    saveDefaultModel,
    saveGitAccess,
    saveIdentity,
    saveJobDuration,
    savedAccessNote,
    savedIdentityNote,
    savedJobDurationNote,
    savedNote,
    seedField,
    type Field,
  } from '../lib/settings';
  import Credentials from './Credentials.svelte';
  import RoutedCredentials from './RoutedCredentials.svelte';
  import Templates from './Templates.svelte';
  import ModelPicker from './ModelPicker.svelte';
  import OpenCodeConnect from './OpenCodeConnect.svelte';
  import OrgSettings from './OrgSettings.svelte';
  import Allowlist from './Allowlist.svelte';
  import Done from './Done.svelte';
  import Problem from './Problem.svelte';
  import { field, help, label as fieldLabel, more, moreSummary, pageTitle, primary, sectionTitle } from './ui';

  const me = resource(async (signal) => unwrap(await api.GET('/api/v1/me', { signal })));
  const models = resource(async (signal) => unwrap(await api.GET('/api/v1/models', { signal })));

  // Each field holds what the server last reported and what the control
  // edits, separately, so a note can report what the SERVER now holds
  // rather than what was typed at it.
  let model = $state<Field>(emptyField());
  let gitName = $state<Field>(emptyField());
  let gitEmail = $state<Field>(emptyField());
  let saving = $state(false);
  let failure = $state<unknown>(undefined);
  let note = $state('');
  let savingIdentity = $state(false);
  let identityFailure = $state<unknown>(undefined);
  let identityNote = $state('');
  // @decision(DL-043)
  let sessionAccess = $state<Field>(emptyField());
  let jobAccess = $state<Field>(emptyField());
  let savingAccess = $state(false);
  let accessFailure = $state<unknown>(undefined);
  let accessNote = $state('');
  // @decision(DL-049)
  let jobDuration = $state<Field>(emptyField());
  let savingDuration = $state(false);
  let durationFailure = $state<unknown>(undefined);
  let durationNote = $state('');

  // The folding rule lives in settings.ts; this only feeds it, once per
  // field. A seam a component holds is a seam nothing can watch, and
  // both of its guards look like one-liners.
  $effect(() => {
    model = seedField(me.loaded, me.data?.default_model ?? NO_DEFAULT, model);
  });
  // @decision(DL-062)
  $effect(() => {
    gitName = seedField(me.loaded, me.data?.git_name ?? '', gitName);
  });
  $effect(() => {
    gitEmail = seedField(me.loaded, me.data?.git_email ?? '', gitEmail);
  });
  // @decision(DL-043)
  $effect(() => {
    sessionAccess = seedField(me.loaded, accessValue(me.data?.default_git_access), sessionAccess);
  });
  $effect(() => {
    jobAccess = seedField(me.loaded, accessValue(me.data?.default_job_git_access), jobAccess);
  });
  // @decision(DL-049)
  $effect(() => {
    jobDuration = seedField(me.loaded, jobDurationValue(me.data?.default_job_duration), jobDuration);
  });

  const groups = $derived(modelGroups(models.data?.providers));
  // Both notes are statements about the account, so neither may be made
  // from a list that failed to arrive: with no providers, keyNote says
  // "no provider has a key", which is a claim about the caller caused
  // by a transport error — sitting beside a Problem block saying the
  // list could not be read.
  const keys = $derived(models.loaded ? keyNote(groups) : '');
  const dirty = $derived(changed(model));
  // @decision(DL-062)
  const identityDirty = $derived(changed(gitName, gitEmail));
  // @decision(DL-043)
  const accessDirty = $derived(changed(sessionAccess, jobAccess));
  // @decision(DL-049)
  const durationDirty = $derived(changed(jobDuration));
  const durations = $derived(jobDurationOptions(me.data?.quota?.max_job_duration, jobDuration.value));
  // Any PROFILE save blocks every profile button, not just its own; see
  // DL-062. The credentials form is deliberately not included: it
  // writes a different endpoint and row, and already refuses its own
  // double-submit.
  //
  // @decision(DL-062) @decision(DL-014)
  const pending = $derived(saving || savingIdentity || savingAccess || savingDuration);

  // A stale note is worse than none: "will run on X" beside a select
  // showing Y, or a refusal of a model no longer chosen.
  function reconsider() {
    note = '';
    failure = undefined;
  }

  // @decision(DL-062)
  async function saveTheIdentity(event: SubmitEvent) {
    event.preventDefault();
    if (pending) return;
    savingIdentity = true;
    identityFailure = undefined;
    identityNote = '';
    try {
      const saved = await saveIdentity(gitName.value, gitEmail.value, (body) =>
        api.PATCH('/api/v1/me', { body }),
      );
      const name = saved.git_name ?? '';
      const email = saved.git_email ?? '';
      gitName = { stored: name, value: name };
      gitEmail = { stored: email, value: email };
      identityNote = savedIdentityNote(saved);
      // Same race as the model save: a /me already in flight would land
      // with the OLD profile and, the fields no longer being diverged,
      // seed it straight back over this write.
      void me.refresh();
    } catch (err) {
      identityFailure = err;
    } finally {
      savingIdentity = false;
    }
  }

  // @decision(DL-043)
  async function saveTheAccess(event: SubmitEvent) {
    event.preventDefault();
    if (pending) return;
    savingAccess = true;
    accessFailure = undefined;
    accessNote = '';
    try {
      const saved = await saveGitAccess(sessionAccess.value, jobAccess.value, (body) =>
        api.PATCH('/api/v1/me', { body }),
      );
      const s = accessValue(saved.default_git_access);
      const j = accessValue(saved.default_job_git_access);
      sessionAccess = { stored: s, value: s };
      jobAccess = { stored: j, value: j };
      accessNote = savedAccessNote(saved);
      void me.refresh();
    } catch (err) {
      accessFailure = err;
    } finally {
      savingAccess = false;
    }
  }

  // @decision(DL-049)
  async function saveTheDuration(event: SubmitEvent) {
    event.preventDefault();
    if (pending) return;
    savingDuration = true;
    durationFailure = undefined;
    durationNote = '';
    try {
      const saved = await saveJobDuration(jobDuration.value, (body) =>
        api.PATCH('/api/v1/me', { body }),
      );
      const d = jobDurationValue(saved.default_job_duration);
      jobDuration = { stored: d, value: d };
      durationNote = savedJobDurationNote(saved);
      void me.refresh();
    } catch (err) {
      durationFailure = err;
    } finally {
      savingDuration = false;
    }
  }

  function reconsiderDuration() {
    durationNote = '';
    durationFailure = undefined;
  }

  function reconsiderAccess() {
    accessNote = '';
    accessFailure = undefined;
  }

  function reconsiderIdentity() {
    identityNote = '';
    identityFailure = undefined;
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (pending) return;
    saving = true;
    failure = undefined;
    note = '';
    try {
      const saved = await saveDefaultModel(model.value, (body) =>
        api.PATCH('/api/v1/me', { body }),
      );
      const stored = saved.default_model ?? NO_DEFAULT;
      model = { stored, value: stored };
      note = savedNote(saved);
      // The profile this page holds is now behind the one it just
      // wrote. A /me already in flight would otherwise land with the
      // OLD default and, since the draft is no longer diverged, seed it
      // straight back over the save. refresh() abandons that request.
      void me.refresh();
    } catch (err) {
      failure = err;
    } finally {
      saving = false;
    }
  }
</script>

<h1 class="{pageTitle} mb-8">Settings</h1>

<!-- Sections are set apart by space and their heading, not rules. Each
     has one sentence of help; the rest is behind a disclosure named for
     its question. @decision(DL-056) -->
<section class="mb-12">
  <h2 class="{sectionTitle} mb-1.5">Default model</h2>
  <p class="mt-0 mb-1 max-w-[64ch] text-soft">
    What a session or job runs on when it names no model of its own.
  </p>
  <details class="{more} mb-5">
    <summary class={moreSummary}>How to override it</summary>
    <p class="mt-1.5 mb-0">
      Every launch can still override it: <code class="font-mono text-ink">--model</code> on the command
      line, or the field on the job form.
    </p>
  </details>

  {#if me.error}
    <div class="mb-5"><Problem error={me.error} what="your settings" /></div>
  {/if}
  {#if models.error}
    <div class="mb-5"><Problem error={models.error} what="the model list" /></div>
  {/if}
  {#if failure}
    <div class="mb-5"><Problem error={failure} what="that model" /></div>
  {/if}

  <form id="model-form" class="max-w-[46ch]" onsubmit={save}>
    <label class={fieldLabel} for="settings-model">Model</label>
    <div class="mb-5">
      <!-- @decision(DL-061) -->
      <ModelPicker
        id="settings-model"
        bind:value={model.value}
        {groups}
        defaultLabel="No default: OpenCode chooses"
        onchange={reconsider}
      />
      <!-- Prose beside the select, not only an attribute on it: a
           disabled option is commonly skipped by a screen reader. -->
      {#if keys}
        <p class={help}>{keys}</p>
      {/if}
    </div>

    <!-- role="status": it reports the server's answer, and nothing
         went wrong, so it is announced politely. -->
    {#if note}
      <Done status class="mb-4">{note}</Done>
    {/if}

    <!-- Explains why Save is disabled until the profile arrives. -->
    {#if !me.loaded && me.error}
      <p class="mb-4 max-w-[64ch] text-md text-gold-t">
        Your current default could not be read, so there is nothing here to change yet.
      </p>
    {/if}

    <button type="submit" class={primary} disabled={pending || !dirty}>{saving ? 'Saving…' : 'Save'}</button>
  </form>
</section>

<!-- The OpenCode sign-in comes before the pasted keys: it is the
     provider set-up most people will use. @decision(DL-015) -->
<section class="mb-12">
  <OpenCodeConnect onChanged={() => void models.refresh()} />
</section>

<section class="mb-12">
  <!-- Storing or removing a key changes which models can be picked
       above, and GET /api/v1/models has already answered. -->
  <Credentials onChanged={() => void models.refresh()} />
  <!-- @decision(DL-077) @decision(DL-118) -->
  <RoutedCredentials admin={me.data?.admin ?? false} />
</section>

<!-- Its own section, which a server that keeps no templates leaves
     out. @decision(DL-121) -->
<Templates />

<!-- Admin only; a server that keeps no App, or no allowlist, draws
     nothing, and a non-admin's page asks for neither.
     @decision(DL-062) @decision(DL-129) -->
{#if me.data?.admin}
  <section class="mb-12">
    <OrgSettings />
  </section>
  <section class="mb-12">
    <Allowlist />
  </section>
{/if}

<!-- @decision(DL-043) -->
<section class="mb-12">
  <h2 class="{sectionTitle} mb-1.5">Git access</h2>
  <p class="mt-0 mb-1 max-w-[64ch] text-soft">
    Which repositories a sandbox's git credentials are used on when the launch does not say.
  </p>
  <details class="{more} mb-5">
    <summary class={moreSummary}>The choices, and what overrides them</summary>
    <p class="mt-1.5 mb-0">
      Only the ones it starts with, or every repository they reach. Jobs have their own setting, since
      they run unattended. <code class="font-mono text-ink">--git-access</code> and the job form override
      both.
    </p>
  </details>

  {#if accessFailure}
    <div class="mb-5"><Problem error={accessFailure} what="your git access" /></div>
  {/if}

  <form id="access-form" class="max-w-[46ch]" onsubmit={saveTheAccess}>
    <label class={fieldLabel} for="session-access">Sessions</label>
    <select id="session-access" class="{field} mb-4" bind:value={sessionAccess.value} onchange={reconsiderAccess}>
      <option value="repos">The session's repositories</option>
      <option value="all">Every repository they reach</option>
    </select>

    <label class={fieldLabel} for="job-access">Jobs</label>
    <select id="job-access" class="{field} mb-4" bind:value={jobAccess.value} onchange={reconsiderAccess}>
      <option value="repos">The job's repositories</option>
      <option value="all">Every repository they reach</option>
    </select>

    {#if accessNote}
      <Done status class="mb-4">{accessNote}</Done>
    {/if}

    {#if !me.loaded && me.error}
      <p class="mb-4 max-w-[64ch] text-md text-gold-t">
        Your current git access could not be read, so there is nothing here to change yet.
      </p>
    {/if}

    <button type="submit" class={primary} disabled={pending || !accessDirty}
      >{savingAccess ? 'Saving…' : 'Save'}</button
    >
  </form>
</section>

<!-- @decision(DL-049) -->
<section class="mb-12">
  <h2 class="{sectionTitle} mb-1.5">Job time limit</h2>
  <p class="mt-0 mb-1 max-w-[64ch] text-soft">How long a job may run unattended when it does not say.</p>
  <details class="{more} mb-5">
    <summary class={moreSummary}>What counts, and what overrides it</summary>
    <p class="mt-1.5 mb-0">
      Time spent steering a job does not count. <code class="font-mono text-ink">--max-duration</code>,
      <code class="font-mono text-ink">sandbox.job_max_duration</code> in your config, and the job form
      override it.
    </p>
  </details>

  {#if durationFailure}
    <div class="mb-5"><Problem error={durationFailure} what="your job time limit" /></div>
  {/if}

  <form id="duration-form" class="max-w-[46ch]" onsubmit={saveTheDuration}>
    <label class={fieldLabel} for="job-duration">Jobs</label>
    <select id="job-duration" class="{field} mb-4" bind:value={jobDuration.value} onchange={reconsiderDuration}>
      <option value="">{builtInDurationLabel(me.data?.quota?.max_job_duration)}</option>
      {#each durations as d (d)}
        <option value={d}>{d}</option>
      {/each}
    </select>

    {#if durationNote}
      <Done status class="mb-4">{durationNote}</Done>
    {/if}

    <button type="submit" class={primary} disabled={pending || !durationDirty}
      >{savingDuration ? 'Saving…' : 'Save'}</button
    >
  </form>
</section>

<!-- Relies on DL-014's PATCH creating a missing profile row.
     @decision(DL-062) -->
<section>
  <h2 class="{sectionTitle} mb-1.5">Git identity</h2>
  <p class="mt-0 mb-1 max-w-[64ch] text-soft">
    Who commits are authored as, which need not be who you sign in as.
  </p>
  <details class="{more} mb-5">
    <summary class={moreSummary}>Which launches use it</summary>
    <p class="mt-1.5 mb-0">
      It applies to launches that name no author of their own, which is what the browser starts;
      <code class="font-mono text-ink">spov fly</code> and <code class="font-mono text-ink">spov jobs create</code>
      send your local git config instead. Leave both empty to use the server's own identity.
    </p>
  </details>

  {#if identityFailure}
    <div class="mb-5"><Problem error={identityFailure} what="your git identity" /></div>
  {/if}

  <!-- novalidate: type="email" is for the keyboard only. The server
       accepts any author, so the browser must not add a rule. -->
  <form id="identity-form" class="max-w-[46ch]" onsubmit={saveTheIdentity} novalidate>
    <label class={fieldLabel} for="git-name">Name</label>
    <input
      id="git-name"
      class="{field} mb-4"
      bind:value={gitName.value}
      oninput={reconsiderIdentity}
      placeholder="Ada Lovelace"
      autocomplete="name"
      aria-describedby="git-identity-notes"
    />

    <label class={fieldLabel} for="git-email">E-mail</label>
    <input
      id="git-email"
      type="email"
      class="{field} mb-4"
      bind:value={gitEmail.value}
      oninput={reconsiderIdentity}
      placeholder="ada@example.com"
      autocomplete="email"
      spellcheck="false"
      aria-describedby="git-identity-notes"
    />

    <!-- Pointed at by both inputs. A half-identity warning nobody hears
         is a warning that did not run. -->
    <div id="git-identity-notes">
      {#if identityNote}
        <Done status class="mb-4">{identityNote}</Done>
      {/if}

      {#if !me.loaded && me.error}
        <p class="mb-4 max-w-[64ch] text-md text-gold-t">
          Your current identity could not be read, so there is nothing here to change yet.
        </p>
      {/if}
    </div>

    <button type="submit" class={primary} disabled={pending || !identityDirty}
      >{savingIdentity ? 'Saving…' : 'Save'}</button
    >
  </form>
</section>
