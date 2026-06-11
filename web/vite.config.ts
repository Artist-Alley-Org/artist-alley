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
    // Vite 5+ defaults to rejecting requests whose Host header
    // isn't on this allowlist (returns 403). Locked-down for prod
    // is right, but dev is reachable from several names + we want
    // each enumerated rather than `true` so the list stays the
    // source of truth for "places dev should be reachable from":
    //   - localhost / 127.0.0.1 / [::1]   default (already accepted)
    //   - host.docker.internal            other containers on the host
    //   - vite.aa                         CI runner via docker DNS
    //                                     (network alias declared
    //                                     on `web` in docker-compose.yml)
    allowedHosts: [
      'localhost',
      '127.0.0.1',
      '[::1]',
      'host.docker.internal',
      'vite.aa',
    ],
    hmr: {
      // Vite will infer the client host from the request; no explicit
      // host pin here. If we need it later (cross-network HMR), set
      // VITE_HMR_HOST in the env.
    },
  },
});
