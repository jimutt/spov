<script lang="ts">
  // Routed credentials: what is stored and where each is sent, and how to
  // remove one. They are added with `spov credentials add`, which reads
  // the secret without echo; the browser never sees one.
  //
  // @decision(DL-077) @decision(DL-073)
  import { api, unwrap, ApiError } from '../lib/api/client';
  import { routedCredentialLine } from '../lib/format';
  import { resource } from '../lib/resource.svelte';
  import Problem from './Problem.svelte';
  import { danger, sectionTitle } from './ui';

  // A server that can hold none answers 501, which is a state to show,
  // not a failure.
  const routes = resource(async (signal) => {
    const r = await api.GET('/api/v1/me/routed-credentials', { signal });
    if (r.response.status === 501) return null;
    return unwrap(r);
  });

  let failure = $state<unknown>(undefined);
  const list = $derived(routes.data?.routed_credentials ?? []);

  async function remove(name: string) {
    if (!confirm(`Delete the routed credential ${name}?`)) return;
    failure = undefined;
    try {
      const r = await api.DELETE('/api/v1/me/routed-credentials/{name}', { params: { path: { name } } });
      if (!r.response.ok) throw new ApiError(r.response.status, r.error);
      await routes.refresh();
    } catch (err) {
      failure = err;
    }
  }
</script>

<h2 class="{sectionTitle} mt-12 mb-1.5">Routed credentials</h2>
<p class="mt-0 mb-5 max-w-[64ch] text-soft">
  Secrets for a private registry or another service, which the sandbox's proxy sets only on the hosts
  they name. Every session gets them, except one stored to attach only when a launch asks for it.
  Add one with <code class="font-mono text-ink">spov credentials add &lt;name&gt; --host &lt;host&gt; --bearer</code>.
</p>

{#if routes.error}
  <Problem error={routes.error} what="your routed credentials" />
{/if}
{#if failure}
  <div class="mb-5"><Problem error={failure} what="that routed credential" /></div>
{/if}

{#if routes.data === null}
  <p class="max-w-[62ch] text-soft">This server cannot store routed credentials: it has no encryption key.</p>
{:else if routes.data}
  {#if list.length === 0}
    <p class="max-w-[62ch] text-soft">None stored.</p>
  {:else}
    <div class="mb-8 max-w-[70ch] border-t border-rule" data-testid="routed-credentials">
      {#each list as rc (rc.route.name)}
        <div class="flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-rule py-2.5">
          <span class="font-mono text-md">{rc.route.name}</span>
          <span class="text-md text-soft">
            {routedCredentialLine(rc.route)}{rc.route.attach === 'asked' ? ', only when a launch asks' : ''}
          </span>
          {#if rc.missing}
            <span class="text-md text-rust">missing: add it again</span>
          {/if}
          <button type="button" class="{danger} ml-auto" onclick={() => remove(rc.route.name)}>Delete</button>
        </div>
      {/each}
    </div>
  {/if}
{:else if !routes.error}
  <p class="text-soft">Loading…</p>
{/if}
