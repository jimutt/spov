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
  // One node per unit, up to 24, so the row is bounded however large a
  // limit the server reports. Past the cap a tick stands for a share of
  // the limit, not one unit, or a large limit would read as full long
  // before it is.
  //
  // @decision(DL-060) @decision(DL-164)
  const ticks = $derived(Math.min(Math.max(limit, 0), 24));
  const lit = $derived.by(() => {
    if (limit <= 0 || ticks === 0) return 0;
    const inUse = Math.min(Math.max(used, 0), limit);
    return Math.round((inUse / limit) * ticks);
  });
  // Sixteen full ticks are the most that fit beside the label on the
  // narrowest phone; a longer row becomes smaller ticks rather than
  // overflowing or squeezing the words.
  const narrow = $derived(ticks > 16);
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
    <span class="ml-2 inline-flex gap-0.5 {narrow ? 'max-sm:gap-px' : ''}" aria-hidden="true">
      {#each Array.from({ length: ticks }, (_, i) => i) as i (i)}
        <i
          class="block h-1.5 w-2.5 {narrow ? 'max-sm:h-1 max-sm:w-1.5' : ''} {i < lit ? (full ? 'bg-gold' : 'bg-olive') : 'bg-rule-2'}"
        ></i>
      {/each}
    </span>
  {/if}
</span>
