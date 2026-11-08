<script lang="ts">
  // What the parent agent asked a subagent to do: a quiet "Task" row,
  // clamped to a few lines, never a bubble and never the reader's words.
  // Drawn alike in a subagent's card and in its own view.
  //
  // @decision(DL-111) @decision(DL-112) @decision(DL-113)
  import { trimBlankLines } from '../lib/timeline';

  const { text }: { text: string } = $props();

  let open = $state(false);
</script>

<div class="min-w-0" data-testid="subagent-prompt">
  <span class="block text-xs font-medium tracking-wide text-faint">Task</span>
  <p class="m-0 whitespace-pre-wrap text-md text-soft {open ? '' : 'line-clamp-3'}">{trimBlankLines(text)}</p>
  {#if text.length > 240}
    <button
      type="button"
      class="mt-0.5 cursor-pointer border-0 bg-transparent p-0 text-sm text-soft underline"
      aria-expanded={open}
      onclick={() => (open = !open)}>{open ? 'Show less' : 'Show all'}</button
    >
  {/if}
</div>
