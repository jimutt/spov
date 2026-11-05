// @decision(DL-055)
import { defineConfig } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
  plugins: [svelte()],
  // Without this Svelte resolves to its server build, where `mount`
  // does not exist. A plain Vite config rather than Astro's, since
  // Astro's getViteConfig() forces SSR resolution and fails the same
  // way.
  resolve: { conditions: ['browser'] },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts'],
    // Stylesheets are processed rather than replaced with '', so a test
    // can read one back as text. logo.test.ts needs it: the favicon's
    // colour literals are copies of --ink, checked against the
    // original.
    //
    // @decision(DL-056)
    css: true,
  },
});
