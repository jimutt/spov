import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import type { Mock } from 'vitest';
import { api, loginURL, nav, probe } from './client';

// The transport is injected per request: openapi-fetch captures
// globalThis.fetch when the client is created, which happens on import,
// so stubbing the global afterwards would never be seen.
function transport(response: () => Response) {
  return vi.fn(async (_request: Request) => response());
}

// The funnel is stubbed for the whole file: jsdom does not implement
// navigation, and a 401 anywhere in here would otherwise try to perform
// one as a side effect of testing something else.
let went: Mock<(url: string) => void>;
const realGo = nav.go;
beforeEach(() => {
  went = vi.fn<(url: string) => void>();
  nav.go = went;
  history.pushState({}, '', '/');
});
afterEach(() => {
  nav.go = realGo;
});

describe('api', () => {
  test('sends the session cookie and resolves against the same origin', async () => {
    const fetchMock = transport(
      () =>
        new Response(JSON.stringify({ id: 'u1', email: 'me@example.com' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
    );

    const { data, error } = await api.GET('/api/v1/me', { fetch: fetchMock });
    expect(error).toBeUndefined();
    expect(data?.email).toBe('me@example.com');

    // Cookie auth is the whole of the browser's credential, so a
    // request that drops it is an unauthenticated request.
    const [request] = fetchMock.mock.lastCall!;
    expect(request.credentials).toBe('same-origin');
    expect(new URL(request.url).pathname).toBe('/api/v1/me');
    expect(new URL(request.url).origin).toBe(location.origin);
  });

  test('a problem response arrives as a typed error, not a throw', async () => {
    const fetchMock = transport(
      () =>
        new Response(JSON.stringify({ title: 'Unauthorized', status: 401 }), {
          status: 401,
          headers: { 'Content-Type': 'application/problem+json' },
        }),
    );

    const { data, error } = await api.GET('/api/v1/me', { fetch: fetchMock });
    expect(data).toBeUndefined();
    // Every field of Huma's ErrorModel is optional; the UI must never
    // assume `detail` is there.
    expect(error?.title).toBe('Unauthorized');
    expect(error?.detail).toBeUndefined();
  });

  test('path parameters are substituted, not concatenated', async () => {
    const fetchMock = transport(() => new Response('{}', { status: 200 }));
    await api.GET('/api/v1/sessions/{ref}', {
      params: { path: { ref: 'a session/with slashes' } },
      fetch: fetchMock,
    });
    const [request] = fetchMock.mock.lastCall!;
    expect(new URL(request.url).pathname).toBe('/api/v1/sessions/a%20session%2Fwith%20slashes');
  });
});

describe('the 401 funnel', () => {
  // Every page reaches the API through `api`, so an expired session has
  // to become a trip to the login flow from exactly one place. These
  // tests are about which callers are exempt, because an exemption that
  // silently stops working turns the landing page into a redirect loop.
  //
  // @decision(DL-055)
  function unauthorized() {
    return transport(
      () =>
        new Response(JSON.stringify({ title: 'Unauthorized', status: 401 }), {
          status: 401,
          headers: { 'Content-Type': 'application/problem+json' },
        }),
    );
  }

  test('a 401 sends the viewer to the login flow, carrying where they were', async () => {
    history.pushState({}, '', '/usage/?tab=spend');
    await api.GET('/api/v1/me/usage', { fetch: unauthorized() });
    expect(went).toHaveBeenCalledTimes(1);
    expect(went.mock.lastCall![0]).toBe(`/login?next=${encodeURIComponent('/usage/?tab=spend')}`);
  });

  test('the probe client is exempt, so the landing page can render signed out', async () => {
    history.pushState({}, '', '/');
    const { response } = await probe.GET('/api/v1/me', {
      fetch: unauthorized(),
    });
    expect(response.status).toBe(401);
    expect(went).not.toHaveBeenCalled();
  });

  test('a 401 while already in the login flow does not loop', async () => {
    history.pushState({}, '', '/login');
    await api.GET('/api/v1/me', { fetch: unauthorized() });
    expect(went).not.toHaveBeenCalled();
  });

  test('a success is left alone', async () => {
    history.pushState({}, '', '/');
    await api.GET('/api/v1/me', {
      fetch: transport(
        () =>
          new Response(JSON.stringify({ user_id: 'u1' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
      ),
    });
    expect(went).not.toHaveBeenCalled();
  });

  test('next is a path, never an origin', () => {
    // The server decides where it will send a browser, but handing it an
    // absolute URL is how an open redirect gets introduced.
    const url = loginURL({ pathname: '/session/', search: '?id=01J' });
    const next = new URLSearchParams(url.split('?')[1]).get('next');
    expect(next).toBe('/session/?id=01J');
    expect(next!.startsWith('/')).toBe(true);
    expect(next).not.toMatch(/^https?:/);
    expect(next).not.toMatch(/^\/\//);
  });
});
