<script lang="ts">
  // Usage, polled once a minute. Every figure is a floor and says so in
  // words, not only as a symbol: the payload's `estimated` is always
  // true, and a page that rendered these as settled would be the way
  // that guarantee gets lost.
  //
  // @decision(DL-055) @decision(DL-009) @decision(DL-056)
  import { api, unwrap } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import { money } from '../lib/format';
  import Problem from './Problem.svelte';
  import { pageTitle, sectionTitle } from './ui';

  const usage = resource(
    async (signal) => unwrap(await api.GET('/api/v1/me/usage', { signal })),
    { every: 60_000 },
  );

  const since = $derived.by(() => {
    const iso = usage.data?.since;
    if (!iso) return '';
    const d = new Date(iso);
    return Number.isNaN(d.getTime())
      ? ''
      : d.toLocaleDateString(undefined, { day: 'numeric', month: 'long', year: 'numeric' });
  });
</script>

<h1 class="{pageTitle} mb-1.5">Usage</h1>
<p class="mt-0 mb-7 text-soft">{since ? `Since ${since}` : 'This month'}</p>

{#if usage.error}
  <Problem error={usage.error} what="your usage" />
{/if}

{#if usage.data}
  <!-- Plain figures at body size, not a row of big numbers. Spend says
       "at least" in words. @decision(DL-056) @decision(DL-009) -->
  <dl class="m-0 mb-10 grid max-w-[46ch] grid-cols-[auto_1fr] gap-x-6 gap-y-2">
    <dt class="text-soft">Sessions started</dt>
    <dd class="m-0 text-right font-semibold tabular-nums">{usage.data.sessions}</dd>
    <dt class="text-soft">Sandbox hours, wall clock</dt>
    <dd class="m-0 text-right font-semibold tabular-nums">{usage.data.running_hours}</dd>
    <dt class="text-soft">Spend</dt>
    <dd class="m-0 text-right font-semibold text-rust tabular-nums">at least {money(usage.data.estimated_cost)}</dd>
  </dl>

  <section id="how-spend-is-estimated" class="max-w-[70ch]">
    <h2 class="{sectionTitle} mb-2">How spend is estimated</h2>
    <p class="mt-0 mb-3 text-soft">
      Every figure is a floor. It is worked out from the resources each session asked for and how long
      it ran. Modal charges the higher of requested and actual use, so the real amount can only be more.
      spov never sees an invoice, and nothing here should be reconciled against one.
    </p>
    <p class="m-0 text-soft">
      On the session list, ages update live and each cost is a floor in the same way. A session close to
      its time limit says so in gold, because a sandbox costs money while nobody is looking at it.
    </p>
  </section>
{:else if !usage.error}
  <p class="text-soft">Loading…</p>
{/if}
