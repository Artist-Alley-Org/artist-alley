import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

export default defineConfig({
  plugins: [tailwindcss(), sveltekit()],
  server: {
    port: 5173,
    strictPort: true,
    // /api/v1 hits the Go binary directly during dev. In prod the Go
    // binary serves both /api/v1 and the embedded frontend, so this
    // proxy goes away.
    proxy: {
      '/api': {
        target: process.env.AA_API_PROXY_TARGET ?? 'http://app:8080',
        changeOrigin: true,
      },
    },
    // HMR through the dev container: bind to all interfaces so the
    // host browser can reach the websocket.
    host: '0.0.0.0',
    hmr: {
      // Vite will infer the client host from the request; no explicit
      // host pin here. If we need it later (cross-network HMR), set
      // VITE_HMR_HOST in the env.
    },
  },
});
