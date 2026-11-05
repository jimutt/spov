<script lang="ts">
  // How much of a concurrency limit is in use: "1 of 3 sessions" and one
  // tick per unit. At the limit the count turns gold, since the next
  // launch of that kind is refused until one ends. Without a limit (an
  // older server) it says the count alone.
  //
  // @decision(DL-060)
  let {
    used,
    limit,
    noun,
    testid,
  }: { used: number; limit: number; noun: string; testid?: string } = $props();

  const full = $derived(limit > 0 && used >= limit);
  // One node per unit, so the row is bounded however large a limit the
  // server reports.
  const ticks = $derived(Math.min(Math.max(limit, 0), 24));
</script>

<span
  class="inline-flex items-center"
  data-testid={testid}
  title={full ? `At your limit: another is refused until one ends.` : undefined}
>
  <span
    ><b class="font-semibold tabular-nums {full ? 'text-gold-t' : 'text-ink'}">{used}</b>{limit ? ` of ${limit}` : ''}
    {noun}</span
  >
  {#if ticks}
    <span class="ml-2 inline-flex gap-0.5" aria-hidden="true">
      {#each Array.from({ length: ticks }, (_, i) => i) as i (i)}
        <i class="block h-1.5 w-2.5 {i < used ? (full ? 'bg-gold' : 'bg-olive') : 'bg-rule-2'}"></i>
      {/each}
    </span>
  {/if}
</span>
