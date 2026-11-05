<script lang="ts">
  // The caller's CLI tokens: list and revoke. The token itself was shown
  // once, in the terminal that asked for it, and nothing here can bring
  // it back — only its name and dates.
  //
  // @decision(DL-055) @decision(DL-006) @decision(DL-056)
  import { api, unwrap, ApiError } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import Problem from './Problem.svelte';
  import { danger, pageTitle } from './ui';

  const tokens = resource(async (signal) => unwrap(await api.GET('/api/v1/me/tokens', { signal })));

  let busy = $state<string | null>(null);
  let failure = $state<unknown>(undefined);

  async function revoke(id: string, name: string) {
    if (!confirm(`Revoke "${name}"? Anything signed in with it stops working immediately.`)) return;
    busy = id;
    failure = undefined;
    try {
      const r = await api.DELETE('/api/v1/me/tokens/{id}', { params: { path: { id } } });
      if (!r.response.ok) throw new ApiError(r.response.status, r.error);
      await tokens.refresh();
    } catch (err) {
      failure = err;
    } finally {
      busy = null;
    }
  }

  function when(iso: string | undefined, missing = 'never'): string {
    if (!iso) return missing;
    const d = new Date(iso);
    return Number.isNaN(d.getTime())
      ? missing
      : d.toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' });
  }
</script>

<h1 class="{pageTitle} mb-1.5">Tokens</h1>
<p class="mt-0 mb-7 max-w-[64ch] text-soft">Tokens that can act as you.</p>

{#if tokens.error}
  <Problem error={tokens.error} what="your tokens" />
{/if}
{#if failure}
  <div class="mb-5"><Problem error={failure} what="that token" /></div>
{/if}

{#if tokens.data}
  {#if tokens.data.tokens.length === 0}
    <p class="max-w-[62ch] text-soft">
      No tokens. <code class="bg-sunk px-1 font-mono text-md text-ink">spov login</code> issues one for a
      terminal.
    </p>
  {:else}
    <div class="max-w-[80ch] border-t border-rule">
      {#each tokens.data.tokens as token (token.id)}
        <div class="flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-rule py-3">
          <div class="min-w-0">
            <p class="m-0 flex flex-wrap items-baseline gap-x-3">
              <span class="font-semibold">{token.name}</span>
              {#if token.current}
                <span class="text-md text-olive-b">This session</span>
              {/if}
            </p>
            <p class="m-0 text-md text-soft tabular-nums">
              Added {when(token.created_at, 'on an unknown date')}, last used {when(token.last_used_at)}{#if token.expires_at}, expires
                {when(token.expires_at)}{/if}
            </p>
          </div>
          <button
            type="button"
            class="{danger} ml-auto"
            disabled={busy === token.id}
            onclick={() => revoke(token.id, token.name)}
          >
            {busy === token.id ? 'Revoking…' : 'Revoke'}
          </button>
        </div>
      {/each}
    </div>
  {/if}
{:else if !tokens.error}
  <p class="text-soft">Loading…</p>
{/if}
