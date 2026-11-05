<script lang="ts">
  // The one model control: a field that opens a searchable list grouped
  // by provider, newest first, with a deliberate "not listed" entry under
  // each provider the caller has a key for. The logic is in
  // ../lib/modelpick.ts; the groups are jobform.ts's `modelGroups`.
  //
  // @decision(DL-061)
  import { tick } from 'svelte';
  import { suppressImplicitSubmit, type ModelGroup } from '../lib/jobform';
  import { customProblem, customRef, filterGroups, pickNote } from '../lib/modelpick';
  import Icon from './Icon.svelte';
  import { fieldBase, secondary, textButton } from './ui';

  let {
    value = $bindable(''),
    groups,
    defaultLabel,
    id,
    disabled = false,
    describedby,
    onchange,
    allowCustom = true,
    showDefault = true,
  }: {
    value?: string;
    groups: ModelGroup[];
    /** What the empty value means, named as DL-061 says. */
    defaultLabel: string;
    id: string;
    disabled?: boolean;
    describedby?: string;
    /** Called after the reader chooses, as a select's change is. */
    onchange?: () => void;
    /** Whether a model not listed may be typed (not on a switch, DL-014). */
    allowCustom?: boolean;
    /** Whether the empty value is offered as the first entry. */
    showDefault?: boolean;
  } = $props();

  let open = $state(false);
  let query = $state('');
  // The provider whose custom entry is open, and what is typed in it.
  let customFor = $state('');
  let customID = $state('');
  let root = $state<HTMLElement | null>(null);
  let trigger = $state<HTMLButtonElement | null>(null);
  let search = $state<HTMLInputElement | null>(null);

  const shown = $derived(filterGroups(groups, query));
  const problem = $derived(customFor ? customProblem(customFor, customID) : '');
  const note = $derived(pickNote(groups, value));

  async function show() {
    if (disabled) return;
    open = !open;
    if (open) {
      query = '';
      customFor = '';
      await tick();
      search?.focus();
    }
  }

  function close() {
    open = false;
    trigger?.focus();
  }

  function choose(ref: string) {
    value = ref;
    close();
    onchange?.();
  }

  function useCustom(provider: string) {
    if (customProblem(provider, customID)) return;
    choose(customRef(provider, customID));
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
    }
  }

  // A press outside closes the list.
  $effect(() => {
    if (!open) return;
    const away = (e: PointerEvent) => {
      if (root && !root.contains(e.target as Node)) open = false;
    };
    window.addEventListener('pointerdown', away);
    return () => window.removeEventListener('pointerdown', away);
  });

  const option =
    'flex w-full cursor-pointer items-baseline gap-2 border-0 bg-transparent px-2.5 py-1.5 text-left font-mono text-md text-ink hover:bg-sunk disabled:cursor-default disabled:text-faint disabled:hover:bg-transparent';
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div class="relative" bind:this={root} onkeydown={onKeydown} data-testid="model-picker">
  <button
    type="button"
    {id}
    bind:this={trigger}
    class="{fieldBase} flex items-center justify-between gap-2 px-2.5 py-2 text-left text-base"
    aria-haspopup="listbox"
    aria-expanded={open}
    aria-describedby={describedby}
    {disabled}
    onclick={show}
    data-testid="model-picker-value"
  >
    <span class="min-w-0 truncate {value ? 'font-mono text-md' : ''}">{value || defaultLabel}</span>
    <Icon name="chevron" class="size-3 shrink-0 text-soft" />
  </button>
  {#if note}
    <p class="m-0 mt-1.5 text-sm {note.refused ? 'text-rust' : 'text-gold-t'}" data-testid="model-picker-custom">{note.text}</p>
  {/if}
  {#if open}
    <div class="absolute z-20 mt-1 w-full min-w-[22rem] overflow-hidden rounded-ctl border border-rule-2 bg-paper shadow-md">
      <input
        bind:this={search}
        bind:value={query}
        class="block w-full border-0 border-b border-rule bg-raised px-2.5 py-2 text-md text-ink placeholder:text-faint focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-olive"
        placeholder="Search models"
        aria-label="Search models"
        autocomplete="off"
        spellcheck="false"
        onkeydown={suppressImplicitSubmit}
        data-testid="model-picker-search"
      />
      <div class="max-h-80 overflow-y-auto py-1" role="listbox" aria-label="Models">
        {#if !query && showDefault}
          <button type="button" role="option" aria-selected={value === ''} class="{option} font-sans" onclick={() => choose('')}
            >{defaultLabel}</button
          >
        {/if}
        {#each shown as g (g.provider)}
          <div class="mt-1 px-2.5 pt-1.5 pb-0.5 text-xs font-medium tracking-wide text-faint uppercase">{g.label}</div>
          {#each g.models as ref (ref)}
            <button
              type="button"
              role="option"
              aria-selected={value === ref}
              class={option}
              disabled={!g.available}
              onclick={() => choose(ref)}
              data-testid="model-picker-option"
              >{ref.slice(g.provider.length + 1)}{#if g.beta?.includes(ref)}<span
                  class="rounded-[3px] border border-rule-2 px-1 font-sans text-xs text-soft">beta</span
                >{/if}</button
            >
          {/each}
          {#if g.available && allowCustom}
            {#if customFor === g.provider}
              <div class="px-2.5 py-1.5" data-testid="model-picker-custom-entry">
                <label class="block text-sm text-soft" for="{id}-custom-{g.provider}">A {g.name} model not in the list</label>
                <div class="mt-1 flex gap-2">
                  <input
                    id="{id}-custom-{g.provider}"
                    class="{fieldBase} min-w-0 flex-1 px-2.5 py-1.5 font-mono text-md"
                    bind:value={customID}
                    placeholder="model-id"
                    autocomplete="off"
                    spellcheck="false"
                    onkeydown={(e) => {
                      if (e.key === 'Enter') {
                        e.preventDefault();
                        useCustom(g.provider);
                      }
                    }}
                  />
                  <button type="button" class={secondary} disabled={problem !== ''} onclick={() => useCustom(g.provider)}>Use</button>
                </div>
                {#if customID && problem}<p class="m-0 mt-1 text-sm text-rust">{problem}</p>{/if}
              </div>
            {:else}
              <div class="px-2.5 py-1">
                <button
                  type="button"
                  class={textButton}
                  onclick={async () => {
                    customFor = g.provider;
                    customID = '';
                    await tick();
                    document.getElementById(`${id}-custom-${g.provider}`)?.focus();
                  }}>Use a model not listed…</button
                >
              </div>
            {/if}
          {/if}
        {:else}
          <p class="m-0 px-2.5 py-2 text-md text-soft">No model matches.</p>
        {/each}
      </div>
    </div>
  {/if}
</div>
