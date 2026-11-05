<script lang="ts">
  // The running session's model, beside Send, and the switch to another.
  // The list is read only when the picker is first used; the page shows
  // the model the log says is selected, not this control's choice.
  //
  // @decision(DL-014) @decision(DL-061)
  import { api, unwrap } from '../lib/api/client';
  import { modelGroups, type ModelGroup } from '../lib/jobform';
  import { switchTo } from '../lib/modelpick';
  import ModelPicker from './ModelPicker.svelte';
  import Problem from './Problem.svelte';

  const { ref, current }: { ref: string; current: string } = $props();

  let groups = $state<ModelGroup[]>([]);
  let loaded = false;
  let chosen = $state('');
  let switching = $state(false);
  let failure = $state<unknown>(undefined);

  // Follows the session's model until the reader picks another.
  $effect(() => {
    if (!switching) chosen = current;
  });

  async function load() {
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

  async function picked() {
    const model = chosen;
    if (!model || model === current || switching) return;
    switching = true;
    failure = undefined;
    try {
      await switchTo(model, (m) =>
        api.POST('/api/v1/sessions/{ref}/model', { params: { path: { ref } }, body: { model: m } }),
      );
    } catch (err) {
      failure = err;
      chosen = current;
    } finally {
      switching = false;
    }
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="w-full max-w-[22rem] min-w-[14rem]" onfocusin={load} onpointerdown={load} data-testid="model-switch">
  <ModelPicker
    id="composer-model"
    bind:value={chosen}
    {groups}
    defaultLabel={current ? current : "OpenCode's default"}
    disabled={switching}
    onchange={picked}
    allowCustom={false}
    showDefault={false}
  />
  {#if switching}<p class="m-0 mt-1 text-sm text-soft">Switching…</p>{/if}
  {#if failure}<div class="mt-2"><Problem error={failure} what="switching the model" /></div>{/if}
</div>
