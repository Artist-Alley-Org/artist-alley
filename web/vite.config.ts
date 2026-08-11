import { fileURLToPath } from 'node:url';

import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

// `web/build/` — adapter-static's output, which the Go binary embeds. Absolute
// so the pattern holds wherever the dev server is launched from; see
// `server.watch.ignored` below for why it is excluded.
const adapterOutput = `${fileURLToPath(new URL('./build', import.meta.url))}/**`;

export default defineConfig({
  plugins: [tailwindcss(), sveltekit()],
  server: {
    port: 5173,
    strictPort: true,
    // Poll the filesystem instead of waiting for inotify (#993).
    //
    // Vite documents this exact case: filesystem watching does not
    // work under WSL2 when the project lives on a Windows filesystem,
    // "and this also applies to running on Docker with a WSL2
    // backend" — which is us. The `web` service bind-mounts the host
    // checkout at /app (docker-compose.yml) and that checkout sits on
    // `/mnt/d`, a 9p mount of the Windows D: drive. inotify events do
    // not cross that boundary, so Vite's watcher waits for
    // notifications that never arrive, nothing ever invalidates the
    // module graph, and the dev server keeps serving the transform it
    // cached on the first request. That is why a *full page reload*
    // used to show stale code, and why "HMR doesn't work here" was
    // folklore in this project: HMR was fine, the watcher was deaf.
    //
    // Vite's first recommendation is to edit from WSL2 applications
    // rather than Windows ones. Measured here, that is not sufficient
    // on its own — with polling off, an edit written by `sed` inside
    // WSL2 was just as invisible as one written by a Windows
    // PowerShell process. The rest of that recommendation is the
    // load-bearing half: move the checkout off the Windows
    // filesystem. Until someone decides to do that, polling is the
    // only lever, and it does catch both edit paths.
    //
    // Cost, and what it scales with. Vite calls out "high CPU
    // utilization" and it is real. Polling stats every watched path
    // every `interval`, so the bill is a function of the SIZE OF THE
    // WATCHED SET, not of the interval alone — quote a percentage
    // without saying which tree it was measured on and the number
    // rots as soon as a tree accumulates artifacts (#997). Measure
    // the set, don't infer it from `find`:
    //
    //   server.watcher.getWatched()   // { dir: [names] } once ready
    //
    // Watched entries by tree state, this project, 500ms interval:
    //
    //   fresh checkout, never built      645
    //   after one `npm run build`       1184   (+426 = web/build/)
    //   ...with `ignored` below          757   back to fresh-tree cost
    //
    // Idle `web` container, six `docker stats --no-stream` samples at
    // steady state, one host, one session — percentages of ONE core:
    //
    //    645 watched   27.8–33.0%
    //   1184 watched   37.3–39.7%
    //    757 watched   27.9–31.3%
    //
    // Two warnings about that second column. It is HOST-SPECIFIC: the
    // fresh-tree row measured ~18% on the box where #993 landed and
    // ~30% here, so the portable number is the watched COUNT, and a
    // percentage is only comparable against another sample from the
    // same box in the same session. And roughly 13 points of it is
    // fixed cost — fit the two rows above and the intercept, not the
    // per-path term, is most of the bill. Trimming the watched set
    // further has little headroom left; the remaining lever is moving
    // the checkout off the Windows filesystem so polling can be
    // turned off entirely.
    //
    // The interval sweep below was measured by #993 on a fresh
    // worktree (the 645-entry row), so read those percentages
    // relative to each other, not as absolutes:
    //
    //   no polling   0.0%    save→served: never (the bug)
    //   500ms       ~18%     save→served: 205–349 ms
    //   1000ms      ~8.5%    save→served: 217 ms OR ~2200 ms
    //   2000ms      ~3.4%    (not latency-tested)
    //
    // What is already excluded, and what is not. Vite's
    // resolveChokidarOptions() ignores `.git`, `node_modules`,
    // `test-results` and the cache dir; SvelteKit adds
    // `<root>/.svelte-kit/!(generated)` itself, which is why
    // `.svelte-kit/output` (~700 files) and `.svelte-kit/types` never
    // appear in the watched set while `.svelte-kit/generated` — the
    // route manifest, which MUST stay watched — does. `emptyOutDir`
    // is not a lever here: in `serve` mode SvelteKit leaves
    // `build.outDir` at Vite's default `dist`, a directory this
    // project never creates, so the outDir exclusion Vite derives
    // from it is already a no-op.
    //
    // That leaves `web/build/`, adapter-static's output — ~425
    // entries of compiled assets the Go binary embeds and the dev
    // server never reads. It is not any Vite outDir, so nothing
    // excludes it automatically, and it does not exist until someone
    // runs a production build — which is exactly why the first
    // measurement above missed it. Ignore it explicitly. Everything
    // else stays watched: over-broad ignores here would break
    // `$types` regeneration and route discovery.
    //
    // node_modules alone is ~47k files; if this ever measures
    // dramatically higher than the table, re-run getWatched() and
    // find what is defeating those exclusions before reaching for a
    // bigger interval.
    //
    // 1000ms is the trap, and the reason this is not the obvious
    // round number. Its latency is bimodal and alternates almost
    // perfectly between fast and ~2.2s: 9p reports mtime at
    // 1-second granularity, so a 1000ms poll aliases against the
    // timestamp tick and every other edit waits an extra full cycle.
    // An intermittent two-second stall is exactly the "did my edit
    // land?" doubt this change exists to remove, so we pay the extra
    // CPU to stay off the alias. Raise the interval if you are on
    // battery and can live with the lag — but do not raise it to
    // exactly 1000.
    //
    // Scope of the cost: `server.*` applies to `vite dev` only. It is
    // ignored by `vite build`, `vite preview` and vitest, and no CI
    // job runs the dev server — ci.yml starts the stack with
    // `--scale web=0`, ui-pr.yml and ui-nightly.yml start only `app`
    // + `postgres` against the prod image. So there is no env guard:
    // the only process that pays is a developer's own dev server,
    // which is exactly the one that benefits.
    watch: {
      usePolling: true,
      interval: 500,
      // Additive, not a replacement: Vite concatenates this array
      // with its own default ignores and with the one SvelteKit's
      // plugin contributes. Verified with getWatched().
      ignored: [adapterOutput],
    },
    // Dev API routing: the SPA always talks to a same-origin /api/v1,
    // so there is no CORS preflight and no cookie-domain special case
    // in dev. Vite forwards /api to the compose `nginx` service, whose
    // single `location /` reverse-proxies everything to the Go app on
    // :8080 — the same binary that serves /api/v1 directly in prod
    // (there it also serves the embedded SPA, so component code needs
    // no dev/prod branch). Override the target with
    // AA_API_PROXY_TARGET when running Vite outside Docker against a
    // different host, e.g. a bare `aa` binary on localhost:8080.
    proxy: {
      '/api': {
        target: process.env.AA_API_PROXY_TARGET ?? 'http://nginx:80',
        changeOrigin: true,
      },
      // Phase 1.54.C dual-mount also lives at /iiif/{2,3}/* on the
      // Go app (matches the URL shape 1.54.A + 1.54.B emit via
      // publicBaseURL(r)). Vite must proxy the alias too, otherwise
      // third-party viewers loading from the dev origin fall through
      // to the SPA. Same upstream as /api.
      //
      // changeOrigin is DELIBERATELY false here: the IIIF handlers
      // derive URLs from the request's Host header. changeOrigin=true
      // would rewrite that to "nginx:80" and Presentation manifests
      // would then emit `http://nginx/...` URLs — unreachable from
      // Mirador running in the browser. Preserving Host lets the app
      // emit `http://localhost:5173/...` which the browser CAN reach.
      '/iiif': {
        target: process.env.AA_API_PROXY_TARGET ?? 'http://nginx:80',
        changeOrigin: false,
      },
    },
    // HMR through the dev container: bind to all interfaces so the
    // host browser can reach the websocket. Nothing else is needed —
    // with the watcher fixed above, HMR works end to end from the
    // host browser (a `.svelte` edit patches the component in place;
    // a `.ts` / `.svelte.ts` edit triggers Vite's own page reload).
    // If cross-network HMR ever needs an explicit client host or
    // port, those knobs live on `server.ws`; the `server.hmr.host` /
    // `.port` / `.clientPort` equivalents are marked @deprecated in
    // the Vite 8 typings, and most Docker-HMR advice online still
    // points at them.
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
  },
});
