<script lang="ts">
  // The repository picker both launch forms share: chosen chips, a
  // filter and the caller's repositories as checkboxes. The selection is
  // the caller's; the logic is in ../lib/jobform.ts.
  //
  // @decision(DL-061)
  import {
    type PlatformTier,
    type Repo,
    type RepoRef,
    isSelected,
    refOf,
    repoKey,
    repoMatches,
    repoReadNote,
    repoUnreachable,
    suppressImplicitSubmit,
    toggleRepo,
  } from '../lib/jobform';
  import Icon from './Icon.svelte';

  let {
    selected = $bindable([]),
    repos,
    loaded,
    platforms = [],
  }: { selected?: RepoRef[]; repos: Repo[]; loaded: boolean; platforms?: PlatformTier[] | null } = $props();

  let filter = $state('');
  const matching = $derived(repos.filter((r) => repoMatches(r, filter)));
</script>

{#if selected.length > 0}
  <ul class="m-0 mb-2 flex list-none flex-wrap gap-1.5 p-0">
    {#each selected as ref (repoKey(ref))}
      <li>
        <button
          type="button"
          class="inline-flex cursor-pointer items-center gap-1.5 rounded-ctl border border-olive bg-raised px-2 py-0.5 font-mono text-md text-ink hover:bg-sunk focus-visible:outline-2 focus-visible:outline-olive focus-visible:outline-offset-2"
          onclick={() => (selected = toggleRepo(selected, ref))}
          >{ref.owner}/{ref.name}<Icon name="cross" class="size-2.5 text-soft" /><span class="sr-only">, remove</span
          ></button
        >
      </li>
    {/each}
  </ul>
{/if}
<div class="overflow-hidden rounded-ctl border border-rule-2 bg-raised">
  <input
    class="block w-full border-0 border-b border-rule bg-raised px-2.5 py-2 font-mono text-md text-ink placeholder:text-faint focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-olive"
    bind:value={filter}
    placeholder="Filter repositories"
    aria-label="Filter repositories"
    autocomplete="off"
    onkeydown={suppressImplicitSubmit}
  />
  <div class="max-h-[13rem] overflow-y-auto">
    {#each matching as repo (repoKey(repo))}
      <!-- A repository the browser's credential cannot reach is shown,
           disabled, with why. @decision(DL-042) -->
      {@const why = repoUnreachable(repo, platforms)}
      {@const readNote = why ? '' : repoReadNote(repo, platforms)}
      <label
        class="grid grid-cols-[auto_1fr_auto] items-baseline gap-2.5 px-2.5 py-[7px] {why
          ? 'cursor-default opacity-70'
          : 'cursor-pointer hover:bg-sunk'}"
        title={why || undefined}
      >
        <input
          type="checkbox"
          class="accent-olive-b"
          disabled={why !== '' && !isSelected(selected, refOf(repo))}
          checked={isSelected(selected, refOf(repo))}
          onchange={() => (selected = toggleRepo(selected, refOf(repo)))}
        />
        <span class="font-mono text-md [overflow-wrap:anywhere]"
          >{repo.owner}/{repo.name}{#if why}<span class="mt-0.5 block font-sans text-sm text-soft">{why}</span
            >{:else if readNote}<span class="mt-0.5 block font-sans text-sm text-soft">{readNote}</span>{/if}</span
        >
        <span class="text-sm text-soft">
          {#if repo.archived}<span class="text-gold-t">Archived</span>,{/if}
          {repo.platform}
        </span>
      </label>
    {:else}
      <p class="m-0 px-2.5 py-2 text-md text-soft">
        {loaded ? 'No repository matches.' : 'Loading…'}
      </p>
    {/each}
  </div>
</div>
