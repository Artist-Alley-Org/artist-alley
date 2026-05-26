import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

export default defineConfig({
  plugins: [tailwindcss(), sveltekit()],
  server: {
    port: 5173,
    strictPort: true,
    // Dev API routing during the strangler-fig phase: nginx splits
    // /api/v1/legacy/* (FastCGI → php-fpm → aa_api/) from /api/v1/*
    // (HTTP → Go). Vite forwards the union to nginx as a single
    // upstream so component code stays unaware of the split. See
    // ADR 0015. Override the target with AA_API_PROXY_TARGET when
    // running Vite outside Docker against a different host.
    proxy: {
      '/api': {
        target: process.env.AA_API_PROXY_TARGET ?? 'http://nginx:80',
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
