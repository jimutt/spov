<script lang="ts">
  // The signed-in address in the header. It uses `probe`, not `api`:
  // every page carries this, so a 401 here would redirect the landing
  // page before it could render its signed-out state.
  //
  // @decision(DL-055) @decision(DL-056)
  import { probe } from '../lib/api/client';

  let email = $state<string | null>(null);

  $effect(() => {
    const ctl = new AbortController();
    void (async () => {
      const { data, response } = await probe.GET('/api/v1/me', { signal: ctl.signal });
      if (ctl.signal.aborted || !response.ok) return;
      email = data?.email ?? data?.user_id ?? null;
    })().catch(() => {
      // The header is decoration; a page that failed to load says so
      // itself.
    });
    return () => ctl.abort();
  });
</script>

{#if email}
  <span class="text-md text-soft">{email}</span>
{/if}
