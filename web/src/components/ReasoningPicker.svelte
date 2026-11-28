<script lang="ts">
  // A form's Reasoning choice, beside its model: Default, then the
  // chosen model's modes as GET /api/v1/models offers them. Drawn only
  // when the model has modes, or holds one no longer offered, which is
  // shown disabled and selected. The logic is in ../lib/reasoning.ts.
  //
  // @decision(DL-189) @decision(DL-161)
  import { field, help, label } from './ui';

  let {
    value = $bindable(''),
    modes,
    id,
    disabled = false,
    known = true,
    note = '',
    onchange,
  }: {
    value?: string;
    modes: string[];
    id: string;
    disabled?: boolean;
    /** Whether the model list was read, so a mode can be said not to be offered. */
    known?: boolean;
    /** What a mode no longer offered means where it is drawn. */
    note?: string;
    /** Called after the reader chooses, as a select's change is. */
    onchange?: () => void;
  } = $props();

  // A held mode the list was not read for is drawn as it is, unjudged.
  const held = $derived(value && !modes.includes(value) ? value : '');
  const unoffered = $derived(known ? held : '');
</script>

{#if modes.length > 0 || held}
  <div class="mb-7 min-w-0" data-testid="form-reasoning">
    <label class={label} for={id}>Reasoning</label>
    <select
      {id}
      class={field}
      bind:value
      {disabled}
      {onchange}
      aria-describedby={unoffered && note ? `${id}-note` : undefined}
    >
      <option value="">Default</option>
      {#each modes as m (m)}<option value={m}>{m}</option>{/each}
      {#if unoffered}<option value={unoffered} disabled>{unoffered} (not offered)</option>
      {:else if held}<option value={held}>{held}</option>{/if}
    </select>
    {#if unoffered && note}
      <p id="{id}-note" class={help}>{note}</p>
    {/if}
  </div>
{/if}
