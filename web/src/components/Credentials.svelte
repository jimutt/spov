<script lang="ts">
  // Platform credentials: what is stored, and how to store or remove
  // one.
  //
  // The secret is held in a field that is cleared the moment the request
  // returns, is never written to storage, and travels in the request
  // body — never in the path or a query string, where it would reach the
  // server's logs and the browser's history.
  //
  // @decision(DL-055) @decision(DL-042) @decision(DL-062) @decision(DL-056)
  import { api, unwrap, ApiError } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import { credentialFacts, forgeTokenAdvice } from '../lib/settings';
  import Done from './Done.svelte';
  import Problem from './Problem.svelte';
  import { danger, fieldMono, help, label, primary, sectionTitle } from './ui';

  // onChanged fires whenever the stored set moves. The section above
  // this one greys out a provider's models because GET /api/v1/models
  // answered `have_key: false`, and storing that provider's key here
  // does not change an answer already on screen — so without this a
  // reader stores an OpenRouter key and the OpenRouter models stay
  // disabled until they reload, which reads as the key not having
  // worked. A delete is the same coupling in reverse.
  //
  // @decision(DL-062)
  let { onChanged }: { onChanged?: () => void } = $props();

  const creds = resource(async (signal) =>
    unwrap(await api.GET('/api/v1/me/credentials', { signal })),
  );

  let platform = $state('');
  let token = $state('');
  let saving = $state(false);
  let failure = $state<unknown>(undefined);
  let saved = $state<string | null>(null);

  async function save(event: SubmitEvent) {
    event.preventDefault();
    const name = platform.trim();
    if (!name || !token) return;
    // A name of `..` survives encodeURIComponent and resolves a segment
    // away, so this credentialed PUT would go to a path nobody named.
    // The server's own validation never gets the chance, because the
    // request no longer arrives at the credentials endpoint.
    if (/^\.+$/.test(name)) {
      failure = new Error(`${JSON.stringify(name)} is not a platform`);
      return;
    }
    saving = true;
    failure = undefined;
    saved = null;
    try {
      const r = await api.PUT('/api/v1/me/credentials/{platform}', {
        params: { path: { platform: name } },
        body: { token },
      });
      if (!r.response.ok) throw new ApiError(r.response.status, r.error);
      saved = name;
      // Cleared on the way out, whatever happened next: the value has
      // no reason to outlive the request.
      platform = '';
      await creds.refresh();
      onChanged?.();
    } catch (err) {
      failure = err;
    } finally {
      token = '';
      saving = false;
    }
  }

  async function remove(name: string) {
    if (!confirm(`Delete the credential for ${name}?`)) return;
    failure = undefined;
    try {
      const r = await api.DELETE('/api/v1/me/credentials/{platform}', {
        params: { path: { platform: name } },
      });
      if (!r.response.ok) throw new ApiError(r.response.status, r.error);
      await creds.refresh();
      onChanged?.();
    } catch (err) {
      failure = err;
    }
  }

  const enabled = $derived(creds.data?.enabled ?? false);
</script>

<h2 class="{sectionTitle} mb-1.5">Credentials</h2>
<p class="mt-0 mb-5 max-w-[64ch] text-soft">Tokens your sessions use to reach a forge or a model provider.</p>

{#if creds.error}
  <Problem error={creds.error} what="your credentials" />
{/if}
{#if failure}
  <div class="mb-5"><Problem error={failure} what="that credential" /></div>
{/if}

{#if creds.data && !enabled}
  <div class="max-w-[70ch] border border-rule-2 bg-raised px-4 py-3">
    <p class="m-0 font-semibold">This server cannot store credentials</p>
    <p class="mt-1 mb-0 text-md text-soft">
      No encryption key is configured, so there is nowhere safe to keep a token. Sessions fall back
      to whatever credentials the server itself holds. An operator can turn this on by configuring a
      key.
    </p>
  </div>
{:else if creds.data}
  {#if saved}
    <Done class="mb-5">Stored and verified for <span class="font-mono text-ink">{saved}</span>.</Done>
  {/if}

  {#if creds.data.credentials.length === 0}
    <p class="max-w-[62ch] text-soft">Nothing stored yet.</p>
  {:else}
    <div class="mb-8 max-w-[70ch] border-t border-rule">
      {#each creds.data.credentials as cred (cred.platform)}
        <!-- @decision(DL-041) -->
        {@const facts = credentialFacts(cred, Date.now())}
        <div class="flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-rule py-2.5">
          <span class="font-mono text-md">{cred.platform}</span>
          {#if facts}
            <span class="text-md text-soft">{facts}</span>
          {/if}
          <button type="button" class="{danger} ml-auto" onclick={() => remove(cred.platform)}>Delete</button>
        </div>
      {/each}
    </div>
  {/if}

  <h3 class="mt-8 mb-3 text-base font-semibold">Store a credential</h3>
  <form class="max-w-[46ch]" onsubmit={save}>
    <label class={label} for="cred-platform">Platform</label>
    <input
      id="cred-platform"
      class={fieldMono}
      bind:value={platform}
      placeholder="github"
      autocomplete="off"
      required
    />
    <p class="{help} mb-4">
      <code class="font-mono text-ink">github</code>, <code class="font-mono text-ink">forgejo:codefloe.com</code>,
      <code class="font-mono text-ink">gitea:gitea.com</code>, <code class="font-mono text-ink">gitlab:gitlab.com</code>,
      or <code class="font-mono text-ink">provider:ANTHROPIC_API_KEY</code>.
    </p>

    <label class={label} for="cred-token">Token</label>
    <input
      id="cred-token"
      type="password"
      class={fieldMono}
      bind:value={token}
      autocomplete="off"
      spellcheck="false"
      required
    />
    <p class="{help} mb-4">
      Checked against the platform before it is stored, so a token that cannot work is refused here
      rather than failing inside a session.
    </p>
    <!-- @decision(DL-043) -->
    {#if forgeTokenAdvice(platform)}
      <p class="-mt-2 mb-4 max-w-[64ch] text-md text-soft" data-testid="forge-token-advice">
        {forgeTokenAdvice(platform)}
      </p>
    {/if}

    <button type="submit" class={primary} disabled={saving}>{saving ? 'Verifying…' : 'Store'}</button>
  </form>
{:else if !creds.error}
  <p class="text-soft">Loading…</p>
{/if}
