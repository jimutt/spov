<script lang="ts">
  // The caller's OpenCode Console sign-in: connect, see where it stands,
  // disconnect. The server runs the device flow and keeps the grant; this
  // shows the code and asks how the attempt stands. No token reaches the
  // browser.
  //
  // The logic is in ../lib/opencode.ts so a test can reach it.
  //
  // @decision(DL-015) @decision(DL-056)
  import { api, unwrap, ApiError } from '../lib/api/client';
  import { resource } from '../lib/resource.svelte';
  import { attemptLine, connectionLine, sleep, waitForSignIn, type Attempt } from '../lib/opencode';
  import Done from './Done.svelte';
  import Problem from './Problem.svelte';
  import StateMark from './StateMark.svelte';
  import { danger, link, primary, sectionTitle } from './ui';

  // onChanged fires when the sign-in changes, so the model list above
  // stops greying out the OpenCode providers without a reload.
  let { onChanged }: { onChanged?: () => void } = $props();

  const conn = resource(async (signal) => unwrap(await api.GET('/api/v1/me/opencode', { signal })));

  let attempt = $state<Attempt | null>(null);
  let ended = $state('');
  // Whether the attempt that ended connected. Only that is shown as a
  // confirmation. @decision(DL-056)
  let endedOk = $state(false);
  let failure = $state<unknown>(undefined);
  let busy = $state(false);
  let leave: AbortController | null = null;

  // Leaving the page stops the polling.
  $effect(() => () => leave?.abort(new Error('left the page')));

  async function check(id: string): Promise<Attempt> {
    return unwrap(await api.GET('/api/v1/me/opencode/connect/{id}', { params: { path: { id } } }));
  }

  async function connect() {
    if (busy) return;
    busy = true;
    failure = undefined;
    ended = '';
    leave?.abort(new Error('started again'));
    leave = new AbortController();
    try {
      const r = await api.POST('/api/v1/me/opencode/connect');
      if (!r.response.ok) throw new ApiError(r.response.status, r.error);
      attempt = r.data!;
      const done = await waitForSignIn(attempt, check, sleep, leave.signal);
      ended = attemptLine(done);
      endedOk = done.state === 'connected';
      attempt = null;
      if (done.state === 'connected') {
        await conn.refresh();
        onChanged?.();
      }
    } catch (err) {
      if (!leave?.signal.aborted) failure = err;
      attempt = null;
    } finally {
      busy = false;
    }
  }

  async function disconnect() {
    if (!confirm('Forget your OpenCode sign-in? Sessions already running keep it until they end.')) return;
    failure = undefined;
    ended = '';
    try {
      const r = await api.DELETE('/api/v1/me/opencode');
      if (!r.response.ok) throw new ApiError(r.response.status, r.error);
      await conn.refresh();
      onChanged?.();
    } catch (err) {
      failure = err;
    }
  }
</script>

<h2 class="{sectionTitle} mb-1.5">OpenCode</h2>
<p class="mt-0 mb-5 max-w-[64ch] text-soft">
  Sign in with your OpenCode Console account to run OpenCode Go and Zen models. The server holds the
  sign-in and gives it to your sessions through the credential proxy.
</p>

{#if conn.error}
  <Problem error={conn.error} what="your OpenCode sign-in" />
{/if}
{#if failure}
  <div class="mb-5"><Problem error={failure} what="the OpenCode sign-in" /></div>
{/if}

{#if conn.data}
  <p class="mb-4 max-w-[64ch]" data-testid="opencode-line">{connectionLine(conn.data)}</p>
  {#if ended && endedOk}
    <Done class="mb-5">{ended}</Done>
  {:else if ended}
    <p class="mb-5 max-w-[64ch] text-md text-rust">{ended}</p>
  {/if}
  {#if attempt}
    <div class="mb-5 max-w-[64ch] border border-rule-2 bg-raised px-4 py-3">
      <p class="m-0">
        Open <a class="{link} text-ink" href={attempt.verification_url} target="_blank" rel="noopener noreferrer"
          >the OpenCode Console</a
        > and check that it shows this code:
      </p>
      <p class="mt-2 mb-2 font-mono text-[1.375rem] tracking-[0.06em]" data-testid="opencode-code">{attempt.user_code}</p>
      <StateMark class="text-md" mark="wait" word="Waiting for approval…" />
    </div>
  {/if}
  {#if conn.data.enabled}
    <div class="flex flex-wrap gap-2">
      {#if !conn.data.connected || conn.data.needs_reconnect}
        <button type="button" class={primary} disabled={busy} onclick={connect}
          >{busy ? 'Waiting…' : 'Connect OpenCode'}</button
        >
      {/if}
      {#if conn.data.connected || conn.data.needs_reconnect}
        <button type="button" class={danger} onclick={disconnect}>Disconnect</button>
      {/if}
    </div>
  {/if}
{:else if !conn.error}
  <p class="text-soft">Loading…</p>
{/if}
