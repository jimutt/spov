<script lang="ts">
  // The sandbox template control both launch forms share. Its first
  // option is None, which is what every load opens on: the choice is
  // the caller's state and is never remembered, here or in storage.
  // The caller reads the list (../lib/templates.ts) and sends the name.
  //
  // @decision(DL-121)
  import { missingLine, templateSummary, type Template } from '../lib/templates';
  import { sentence } from '../lib/format';
  import { field, help, label, link } from './ui';

  // `templates` is null on a server that keeps none and undefined until
  // the list arrives; neither draws anything.
  let {
    value = $bindable(''),
    templates,
    id,
  }: { value?: string; templates: Template[] | null | undefined; id: string } = $props();

  const chosen = $derived((templates ?? []).find((t) => t.name === value));
  const summary = $derived(chosen ? templateSummary(chosen) : '');
  const missing = $derived(chosen ? missingLine(chosen) : '');
</script>

{#if templates}
  <div class="mb-7 max-w-[46ch]" data-testid="template-picker">
    {#if templates.length === 0}
      <p class="m-0 text-md text-soft">
        No sandbox templates yet. <a class="{link} text-ink" href="/settings/">Make one in Settings</a> to pick a set of
        credentials, variables and add-ons here.
      </p>
    {:else}
      <label class={label} for={id}>Template <span class="font-normal text-soft">(optional)</span></label>
      <select {id} class={field} bind:value aria-describedby="{id}-note">
        <option value="">None</option>
        {#each templates as t (t.name)}
          <option value={t.name}>{t.name}</option>
        {/each}
      </select>
      <div id="{id}-note">
        {#if chosen}
          <p class={help} data-testid="template-summary">
            {summary ? `${sentence(summary)}.` : 'Sets nothing.'}
            {#if chosen.description}<span class="[overflow-wrap:anywhere]">{chosen.description}</span>{/if}
          </p>
          {#if missing}<p class="mt-1.5 mb-0 max-w-[64ch] text-md text-rust">{missing}</p>{/if}
        {:else}
          <p class={help}>A sandbox template's settings apply where you set none here. Manage them in Settings.</p>
        {/if}
      </div>
    {/if}
  </div>
{/if}
