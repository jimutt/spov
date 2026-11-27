<script lang="ts">
  // A schedule's form, new or edited: a name, a cadence chosen by name or
  // written as cron, a time zone, and the job's own fields, the job
  // form's (JobFields). An edit sends what the form shows over what the
  // schedule holds, so a field set from the CLI is kept. The logic is in
  // ../lib/schedule.ts.
  //
  // @decision(DL-195)
  import { api, nav, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import { emptyDraft, suppressImplicitSubmit } from '../lib/jobform';
  import { sentence } from '../lib/format';
  import { listTemplates } from '../lib/templates';
  import {
    WEEKDAYS,
    browserZone,
    cadenceOf,
    cadenceWords,
    cronOf,
    defaultCadence,
    draftOf,
    editedCadence,
    scheduleJobOf,
    scheduleProblems,
    schedulePage,
    toScheduleRequest,
    type Cadence,
    type CadenceKind,
  } from '../lib/schedule';
  import JobFields from './JobFields.svelte';
  import Problem from './Problem.svelte';
  import { field, fieldMono, help, label, pageTitle, primaryLarge } from './ui';

  let { edit = false }: { edit?: boolean } = $props();

  const ref = typeof location === 'undefined' ? '' : (new URLSearchParams(location.search).get('ref') ?? '');
  const repos = resource(async (signal) => unwrap(await api.GET('/api/v1/repos', { signal })));
  const models = resource(async (signal) => unwrap(await api.GET('/api/v1/models', { signal })));
  const me = resource(async (signal) => unwrap(await api.GET('/api/v1/me', { signal })));
  const templates = resource(listTemplates);
  const stored = resource(
    async (signal) => unwrap(await api.GET('/api/v1/schedules/{ref}', { params: { path: { ref } }, signal })),
    { when: () => edit && ref !== '' },
  );

  let name = $state('');
  let cadence = $state<Cadence>(defaultCadence());
  let zone = $state(browserZone());
  let draft = $state(emptyDraft());
  let saving = $state(false);
  let failure = $state<unknown>(undefined);
  // An edit's form is filled once, from the first read; a later read
  // (the tab shown again) never overwrites what is being typed.
  let seeded = false;
  $effect(() => {
    const s = stored.data;
    if (seeded || !s) return;
    seeded = true;
    name = s.name;
    cadence = cadenceOf(s.cron);
    zone = s.zone;
    draft = draftOf(s.job);
  });

  const problems = $derived(scheduleProblems(name, cadence, draft));
  const kinds: { kind: CadenceKind; label: string }[] = [
    { kind: 'hourly', label: 'Hourly' },
    { kind: 'daily', label: 'Daily' },
    { kind: 'weekly', label: 'Weekly' },
    { kind: 'monthly', label: 'Monthly' },
    { kind: 'custom', label: 'Custom' },
  ];
  const time = $derived(`${String(cadence.hour).padStart(2, '0')}:${String(cadence.minute).padStart(2, '0')}`);

  function setTime(value: string) {
    const [h, m] = value.split(':').map(Number);
    if (Number.isInteger(h) && Number.isInteger(m)) cadence = { ...cadence, hour: h, minute: m };
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (problems.length > 0 || saving) return;
    saving = true;
    failure = undefined;
    try {
      let saved;
      if (edit && stored.data) {
        // Only a changed cadence is sent, which moves the next run.
        saved = await unwrap(
          await api.PATCH('/api/v1/schedules/{ref}', {
            params: { path: { ref: stored.data.id } },
            body: {
              name: name.trim(),
              ...editedCadence(stored.data, cadence, zone),
              job: scheduleJobOf(draft, stored.data.job),
            },
          }),
        );
      } else {
        saved = await unwrap(await api.POST('/api/v1/schedules', { body: toScheduleRequest(name, cadence, zone, draft) }));
      }
      // The form leaves the history once what it saved exists. @decision(DL-156)
      nav.replace(schedulePage(saved.id));
    } catch (err) {
      failure = err;
      saving = false;
    }
  }
</script>

<div class="max-w-[720px]">
  <h1 class="{pageTitle} mb-1.5">{edit ? 'Edit schedule' : 'New schedule'}</h1>
  <p class="mt-0 mb-8 max-w-[64ch] text-soft">
    The same job, started on a cadence, as you. A run is skipped while the previous one is still going or you are at
    your job limit.
  </p>

  {#if failure}
    <div class="mb-6"><Problem error={failure} what="that schedule" /></div>
  {/if}
  {#if stored.error}
    <div class="mb-6"><Problem error={stored.error} what="the schedule" /></div>
  {/if}
  <!-- As the job form reports them. @decision(DL-061) -->
  {#if repos.error}
    <div class="mb-6"><Problem error={repos.error} what="your repositories" /></div>
  {/if}
  {#if models.error}
    <div class="mb-6"><Problem error={models.error} what="the model list" /></div>
  {/if}
  {#if me.error}
    <div class="mb-6"><Problem error={me.error} what="your account default" /></div>
  {/if}

  {#if edit && !ref}
    <p class="text-soft">No schedule named. <a href="/schedules/">Your schedules</a></p>
  {:else if edit && !stored.data}
    {#if !stored.error}<p class="text-soft">Loading…</p>{/if}
  {:else}
  <form onsubmit={save}>
    <div class="mb-7 max-w-[46ch]">
      <label class={label} for="schedule-name">Name</label>
      <input
        id="schedule-name"
        class={fieldMono}
        bind:value={name}
        placeholder="weekly-deps"
        autocomplete="off"
        spellcheck="false"
        onkeydown={suppressImplicitSubmit}
      />
      <p class={help}>
        Lower-case letters, digits, dots, dashes and underscores; <code class="font-mono text-ink">spov jobs schedule</code>
        uses it too.
      </p>
    </div>

    <fieldset class="mb-7 min-w-0 border-0 p-0" data-testid="schedule-cadence">
      <legend class={label}>Runs</legend>
      <div class="mb-3 flex flex-wrap gap-1.5" role="group" aria-label="Cadence">
        {#each kinds as k (k.kind)}
          <button
            type="button"
            aria-pressed={cadence.kind === k.kind}
            class="cursor-pointer rounded-ctl border px-3 py-1 text-md focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2 {cadence.kind ===
            k.kind
              ? 'border-olive bg-raised font-medium text-ink'
              : 'border-rule-2 text-soft hover:bg-sunk hover:text-ink'}"
            onclick={() => (cadence = { ...cadence, kind: k.kind, cron: k.kind === 'custom' ? cronOf(cadence) : cadence.cron })}
            >{k.label}</button
          >
        {/each}
      </div>
      <div class="grid grid-cols-1 gap-x-5 gap-y-3 sm:grid-cols-2">
        {#if cadence.kind === 'hourly'}
          <div class="min-w-0">
            <label class={label} for="schedule-minute">Minute past the hour</label>
            <input
              id="schedule-minute"
              class={field}
              type="number"
              min="0"
              max="59"
              step="1"
              value={cadence.minute}
              onkeydown={suppressImplicitSubmit}
              oninput={(e) =>
                (cadence = { ...cadence, minute: Math.min(59, Math.max(0, Math.trunc(Number(e.currentTarget.value)) || 0)) })}
            />
          </div>
        {:else if cadence.kind === 'custom'}
          <div class="min-w-0 sm:col-span-2">
            <label class={label} for="schedule-cron">Cron expression</label>
            <input
              id="schedule-cron"
              class={fieldMono}
              bind:value={cadence.cron}
              placeholder="0 9 * * 1-5"
              autocomplete="off"
              spellcheck="false"
              onkeydown={suppressImplicitSubmit}
            />
            <p class={help}>Five fields: minute, hour, day of month, month, day of week. At most once an hour.</p>
          </div>
        {:else}
          {#if cadence.kind === 'weekly'}
            <div class="min-w-0">
              <label class={label} for="schedule-weekday">Day</label>
              <select id="schedule-weekday" class={field} bind:value={cadence.weekday}>
                {#each WEEKDAYS as day, i (day)}<option value={i}>{day}</option>{/each}
              </select>
            </div>
          {:else if cadence.kind === 'monthly'}
            <div class="min-w-0">
              <label class={label} for="schedule-day">Day of the month</label>
              <select id="schedule-day" class={field} bind:value={cadence.day}>
                {#each Array.from({ length: 28 }, (_, i) => i + 1) as d (d)}<option value={d}>{d}</option>{/each}
              </select>
            </div>
          {/if}
          <div class="min-w-0">
            <label class={label} for="schedule-time">Time</label>
            <input
              id="schedule-time"
              class={field}
              type="time"
              value={time}
              onkeydown={suppressImplicitSubmit}
              oninput={(e) => setTime(e.currentTarget.value)}
            />
          </div>
        {/if}
        <div class="min-w-0">
          <label class={label} for="schedule-zone">Time zone</label>
          <input
            id="schedule-zone"
            class={fieldMono}
            bind:value={zone}
            autocomplete="off"
            spellcheck="false"
            onkeydown={suppressImplicitSubmit}
          />
        </div>
      </div>
      <p class={help} data-testid="schedule-words">{cadenceWords(cronOf(cadence), zone.trim() || 'UTC')}</p>
    </fieldset>

    <JobFields bind:draft {repos} {models} {me} {templates} />

    <div
      class="sticky bottom-0 mt-9 flex flex-wrap items-center justify-between gap-x-5 gap-y-2.5 border-t border-rule bg-paper pt-3.5 pb-4"
    >
      <p class="m-0 max-w-[52ch] text-md text-soft">
        {problems.length > 0
          ? `${sentence(problems.join('; '))}.`
          : 'Each run is a job of its own, billed like any other, and its next runs show once it is saved.'}
      </p>
      <button type="submit" class={primaryLarge} disabled={saving || problems.length > 0}
        >{saving ? 'Saving…' : 'Save schedule'}</button
      >
    </div>
  </form>
  {/if}
</div>
