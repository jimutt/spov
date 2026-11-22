<script lang="ts">
  // A synthetic message OpenCode or a plugin added, never spov's words:
  // a shell command's result as one quiet line, opened to the command and
  // its output; anything else folded under OpenCode's own summary. The
  // transcript and a subagent's card both draw it here.
  //
  // @decision(DL-180) @decision(DL-173)
  import type { Snippet } from 'svelte';
  import type { MessageEntry } from '../lib/timeline';
  import { commandLine, injectedLabel, shellEnd, shellFailed, shellWho } from '../lib/injected';
  import Icon from './Icon.svelte';
  import { textButton } from './ui';

  const {
    entry,
    markdown,
    class: cls = '',
  }: { entry: MessageEntry; markdown: Snippet<[string, string]>; class?: string } = $props();
  const shell = $derived(entry.injected?.shell);
</script>

{#if shell}
  <details class="{cls} group/injected" data-testid="shell-result">
    <summary
      class="flex min-w-0 cursor-pointer list-none items-baseline gap-1.5 text-sm text-soft hover:text-ink [&::-webkit-details-marker]:hidden"
      title={shell.command}
    >
      <Icon name="chevron" class="chevron size-3 shrink-0 self-center" />
      <span class="shrink-0">{shellWho(shell)}</span>
      {#if shell.command.trim()}<code class="min-w-0 truncate font-mono text-sm">{commandLine(shell.command)}</code>{/if}
      <span class="shrink-0 {shellFailed(shell) ? 'text-rust' : 'text-faint'}" data-testid="shell-end">{shellEnd(shell)}</span>
    </summary>
    <div class="mt-1.5 border-l-2 border-rule-2 pl-3">
      {#if shell.command.trim()}
        <pre
          class="m-0 max-h-40 overflow-auto font-mono text-sm whitespace-pre-wrap break-words text-ink"
          data-testid="shell-command">{shell.command}</pre>
      {/if}
      {#if entry.text.trim()}
        <pre
          class="m-0 mt-2 max-h-80 overflow-auto rounded-ctl border border-rule bg-sunk px-3 py-2 font-mono text-sm whitespace-pre-wrap break-words text-ink"
          data-testid="shell-output">{entry.text}</pre>
      {:else}
        <p class="m-0 mt-1 text-sm text-faint">No output.</p>
      {/if}
      {#if shell.truncated}
        <p class="m-0 mt-1 text-sm text-faint" data-testid="shell-truncated">OpenCode cut the output short.</p>
      {/if}
    </div>
  </details>
{:else}
  <details class="{cls} group/injected" data-testid="injected">
    <summary class="{textButton} list-none [&::-webkit-details-marker]:hidden">
      <Icon name="chevron" class="chevron size-3" />{injectedLabel(entry.injected)}
    </summary>
    <div class="mt-1.5 border-l-2 border-rule-2 pl-3">{@render markdown(entry.text, 'text-md text-soft')}</div>
  </details>
{/if}
