<script lang="ts">
  // A destructive control that acts only on a second press. The first
  // press asks in place, and "Keep it" takes focus so a stray Enter or
  // Space backs out. The caller owns the request and its failure, since
  // a refused act re-reads the page and may unmount this.
  //
  // @decision(DL-057) @decision(DL-056)
  import { tick } from 'svelte';
  import { danger, dangerConfirm, secondary } from './ui';

  let {
    label,
    question,
    yes,
    run,
    testid,
  }: {
    label: string;
    question: string;
    yes: string;
    run: () => Promise<void>;
    testid: string;
  } = $props();

  let asking = $state(false);
  let pending = $state(false);
  let keep = $state<HTMLButtonElement | undefined>(undefined);
  let opener = $state<HTMLButtonElement | undefined>(undefined);

  async function ask() {
    asking = true;
    await tick();
    keep?.focus();
  }

  async function back() {
    asking = false;
    await tick();
    opener?.focus();
  }

  async function confirm() {
    if (pending) return;
    pending = true;
    try {
      await run();
    } finally {
      pending = false;
      asking = false;
      // The disabled buttons dropped focus. If the act left this
      // control on the page, focus returns to it; if not, `opener` is
      // unbound and nothing happens.
      await tick();
      opener?.focus();
    }
  }

  // On both buttons rather than the group, which is not interactive;
  // focus is on one of them while the question is open.
  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape' && !pending) {
      e.preventDefault();
      void back();
    }
  }
</script>

{#if asking}
  <div role="group" aria-label={label} data-testid={`${testid}-ask`}>
    <p class="m-0 mb-2 text-sm text-soft">{question}</p>
    <div class="flex flex-wrap gap-2">
      <button
        type="button"
        data-testid={`${testid}-yes`}
        class={dangerConfirm}
        disabled={pending}
        onkeydown={onKeydown}
        onclick={confirm}>{pending ? 'Working…' : yes}</button
      >
      <button
        type="button"
        data-testid={`${testid}-keep`}
        class={secondary}
        disabled={pending}
        bind:this={keep}
        onkeydown={onKeydown}
        onclick={back}>Keep it</button
      >
    </div>
  </div>
{:else}
  <button
    type="button"
    data-testid={testid}
    class={danger}
    bind:this={opener}
    onclick={ask}>{label}</button
  >
{/if}
