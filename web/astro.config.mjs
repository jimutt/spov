// @decision(DL-055)
import { defineConfig } from 'astro/config';
import svelte from '@astrojs/svelte';
import tailwindcss from '@tailwindcss/vite';

// In dev the API is a separate process on another port, so requests reach
// it through Vite's proxy. spov's BrowserGuard checks Host, Sec-Fetch-Site
// and Origin (internal/auth/guard.go): `changeOrigin` fixes Host, and the
// `configure` hook fixes Origin. Without BOTH the server answers 403
// before authentication ever runs, which looks like an auth bug.
const API = process.env.SPOV_API_URL ?? 'http://127.0.0.1:8099';
const toApi = {
  target: API,
  changeOrigin: true,
  configure(proxy) {
    proxy.on('proxyReq', (proxyReq) => {
      if (proxyReq.getHeader('origin')) proxyReq.setHeader('origin', API);
    });
  },
};

export default defineConfig({
  output: 'static',
  integrations: [svelte()],
  vite: {
    plugins: [tailwindcss()],
    // Every path the server owns, so a dev browser follows the real
    // login redirect instead of hitting Astro's 404. Anchored regexes,
    // not bare strings: Vite matches a string key by prefix, which
    // would send /apiary and /devices to the API in dev while
    // Server.ownsPath keeps them for the web app in production.
    server: {
      proxy: {
        '^/api(/|$)': toApi,
        '^/login$': toApi,
        '^/logout$': toApi,
        '^/oauth(/|$)': toApi,
        '^/device(/|$)': toApi,
        '^/docs$': toApi,
        '^/openapi': toApi,
        '^/schemas(/|$)': toApi,
      },
    },
  },
});
