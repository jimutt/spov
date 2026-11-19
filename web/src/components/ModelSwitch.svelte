<script lang="ts">
  // The running session's model, beside Send, and the switch to another.
  // The list is read only when the picker is first used; the page shows
  // the model the log says is selected, not this control's choice.
  //
  // Beside it, the model's reasoning modes, when the session's sandbox
  // lists any, drawn as one more cell of the composer's options. The
  // page reads the list; the logic is in ../lib/reasoning.ts.
  //
  // @decision(DL-014) @decision(DL-061) @decision(DL-161)
  import { api, unwrap } from '../lib/api/client';
  import { modelGroups, withoutVariant, type ModelGroup } from '../lib/jobform';
  import { switchTo } from '../lib/modelpick';
  import { modeOf, modesOf, switchedTo, withMode, type ModelVariants } from '../lib/reasoning';
  import ModelPicker from './ModelPicker.svelte';
  import Problem from './Problem.svelte';

  const {
    ref,
    current,
    variants,
    onStale,
  }: {
    ref: string;
    current: string;
    /** The session's models' reasoning modes, as the page read them. */
    variants?: ModelVariants;
    /** Asks the page to read them again. */
    onStale?: () => void;
  } = $props();

  let groups = $state<ModelGroup[]>([]);
  let loaded = false;
  let chosen = $state('');
  let mode = $state('');
  let switching = $state(false);
  let failure = $state<unknown>(undefined);

  const modes = $derived(modesOf(variants, current));

  // Follows the session's model and mode until the reader picks another.
  $effect(() => {
    if (switching) return;
    chosen = withoutVariant(current);
    mode = modeOf(current);
  });

  async function load() {
    // @decision(DL-161)
    if (!variants?.read) onStale?.();
    if (loaded) return;
    loaded = true;
    try {
      const body = await unwrap(await api.GET('/api/v1/models'));
      groups = modelGroups(body?.providers);
    } catch (err) {
      loaded = false;
      failure = err;
    }
  }

  async function send(model: string) {
    if (!model || model === current || switching) return;
    switching = true;
    failure = undefined;
    try {
      await switchTo(model, (m) =>
        api.POST('/api/v1/sessions/{ref}/model', { params: { path: { ref } }, body: { model: m } }),
      );
    } catch (err) {
      failure = err;
      chosen = withoutVariant(current);
      mode = modeOf(current);
      // The list may have changed under the page.
      //
      // @decision(DL-161)
      onStale?.();
    } finally {
      switching = false;
    }
  }

  // Another model keeps the mode when it offers it.
  //
  // @decision(DL-161)
  function picked() {
    if (!chosen || chosen === withoutVariant(current)) return;
    void send(switchedTo(variants, current, chosen));
  }

  function pickedMode() {
    void send(withMode(current, mode));
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="w-full max-w-[22rem] min-w-[14rem]" onfocusin={load} onpointerdown={load} data-testid="model-switch">
  <ModelPicker
    id="composer-model"
    bind:value={chosen}
    {groups}
    defaultLabel={current ? withoutVariant(current) : "OpenCode's default"}
    disabled={switching}
    onchange={picked}
    allowCustom={false}
    showDefault={false}
  />
  {#if switching}<p class="m-0 mt-1 text-sm text-soft">Switching…</p>{/if}
  {#if failure}<div class="mt-2"><Problem error={failure} what="switching the model" /></div>{/if}
</div>
<!-- One more cell of the options, never a second copy.
     @decision(DL-161) @decision(DL-117) -->
{#if modes.length > 0}
  <label class="flex items-center gap-2 text-md text-soft" data-testid="reasoning-switch">
    Reasoning
    <select
      class="rounded-ctl border border-rule-2 bg-raised px-2 py-1 text-md text-ink disabled:opacity-60 focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-1"
      data-testid="composer-reasoning"
      bind:value={mode}
      disabled={switching}
      onchange={pickedMode}
    >
      <option value="">Default</option>
      {#each modes as m (m)}<option value={m}>{m}</option>{/each}
    </select>
  </label>
{/if}
