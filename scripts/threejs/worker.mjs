// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// Production headless three.js preview renderer (#498, epic #496, ADR 0069).
//
// The render step behind the Go ModelHandler (app/internal/preview/model.go):
// given a staged model + its companions in a work dir, it renders a
// turntable + poster + reference views and writes them in the fixed
// on-disk layout the Go side's sprite/ladder/frame fanning reads:
//
//   <output>/turntable/frame_0000.png ... frame_{N-1}.png   (res × res)
//   <output>/poster.png                                     (posterRes²)
//   <output>/views/top.png, <output>/views/bottom.png       (res × res)
//
// Rendering is headless Chromium (Puppeteer) on SwiftShader software
// WebGL — the exact stack #497 proved: no GPU required, ~20-30× faster
// than Blender Cycles was, equal-or-better PBR fidelity. The same three.js
// loaders the interactive viewer uses (ModelView.svelte) load the model,
// so anything the viewer renders, this renders identically.
//
// Usage:
//   node worker.mjs --input <model> --workdir <dir> --output <dir> \
//                   [--frames 36] [--res 512] [--poster-res 2048]
//
// The model + its companions must both live under --workdir so the
// loaders resolve sibling .bin/textures by relative URL (the Go handler's
// stageCompanions writes them there). Exit 0 on success, non-zero on any
// failure so the Go handler can fail (and retry) the preview job.
//
// Formats: the dispatch in render.html's loadModel() is the source of
// truth; threeJSExts in model.go mirrors it and smoke.mjs covers it.

import http from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const THREE_DIR = path.join(HERE, 'node_modules', 'three');

function parseArgs(argv) {
  const a = {};
  for (let i = 0; i < argv.length; i += 2) {
    if (!argv[i].startsWith('--')) throw new Error(`bad arg: ${argv[i]}`);
    a[argv[i].slice(2)] = argv[i + 1];
  }
  return a;
}

const CONTENT_TYPES = {
  '.html': 'text/html', '.js': 'text/javascript', '.mjs': 'text/javascript',
  '.glb': 'model/gltf-binary', '.gltf': 'model/gltf+json', '.fbx': 'application/octet-stream',
  '.obj': 'text/plain', '.mtl': 'text/plain', '.bin': 'application/octet-stream',
  '.stl': 'model/stl', '.ply': 'application/octet-stream', '.dae': 'model/vnd.collada+xml',
  '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg',
  '.webp': 'image/webp', '.wasm': 'application/wasm',
};

// Serve render.html, three's module graph (/three/*), and the work dir
// (/models/* — model + staged companions). Path-normalised so a
// crafted URL can't escape the served roots.
function startServer(workDir) {
  return new Promise((resolve) => {
    const serveFrom = (root, rel, res) => {
      const abs = path.normalize(path.join(root, rel));
      if (!abs.startsWith(path.normalize(root))) { res.statusCode = 403; return res.end('forbidden'); }
      fs.readFile(abs, (err, data) => {
        if (err) { res.statusCode = 404; return res.end('nf'); }
        res.setHeader('Content-Type', CONTENT_TYPES[path.extname(abs).toLowerCase()] || 'application/octet-stream');
        res.end(data);
      });
    };
    const server = http.createServer((req, res) => {
      const url = decodeURIComponent(req.url.split('?')[0]);
      if (url === '/favicon.ico') { res.statusCode = 204; return res.end(); }
      if (url === '/' || url === '/render.html') return serveFrom(HERE, 'render.html', res);
      if (url.startsWith('/three/')) return serveFrom(THREE_DIR, url.slice('/three/'.length), res);
      if (url.startsWith('/models/')) return serveFrom(workDir, url.slice('/models/'.length), res);
      res.statusCode = 404; res.end('nf');
    });
    server.listen(0, '127.0.0.1', () => resolve(server));
  });
}

function dataUrlToBuffer(u) {
  return Buffer.from(u.slice(u.indexOf(',') + 1), 'base64');
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const input = args.input;
  const workDir = args.workdir || path.dirname(input);
  const output = args.output;
  const frames = parseInt(args.frames || '36', 10);
  const res = parseInt(args.res || '512', 10);
  const posterRes = parseInt(args['poster-res'] || '2048', 10);
  if (!input || !output) throw new Error('usage: --input <model> --output <dir> [--workdir <dir>]');

  const ext = path.extname(input).slice(1).toLowerCase();
  const modelName = path.basename(input);

  fs.mkdirSync(path.join(output, 'turntable'), { recursive: true });
  fs.mkdirSync(path.join(output, 'views'), { recursive: true });

  const server = await startServer(workDir);
  const port = server.address().port;

  // Per-run writable dirs. The container's `app` user has no home dir, so
  // Chromium's default user-data + crashpad paths (HOME-relative) aren't
  // writable — point them at a temp dir instead, or launch fails with
  // "chrome_crashpad_handler: --database is required".
  const userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'aa-chromium-'));

  const browser = await puppeteer.launch({
    headless: true,
    userDataDir,
    // The container's `app` user has HOME=/home/app but no such dir
    // (--no-create-home), so Chromium/crashpad can't write there. Point
    // HOME at the writable per-run dir.
    env: { ...process.env, HOME: userDataDir },
    args: [
      '--no-sandbox', '--disable-setuid-sandbox',
      // Container hardening: small /dev/shm → use /tmp; no writable HOME →
      // steer crashpad off + into /tmp so the browser process launches.
      '--disable-dev-shm-usage',
      '--disable-crash-reporter', '--crash-dumps-dir=/tmp',
      // Software WebGL via SwiftShader — servers/CI have no GPU. Chromium
      // gates SwiftShader-for-WebGL behind --enable-unsafe-swiftshader.
      '--use-gl=angle', '--use-angle=swiftshader', '--enable-unsafe-swiftshader',
      '--enable-webgl', '--ignore-gpu-blocklist',
    ],
  });

  try {
    const page = await browser.newPage();
    // Surface page console + errors on stderr so a loader failure is
    // diagnosable from the Go handler's captured output.
    page.on('console', (m) => { if (m.type() === 'error') console.error('[page]', m.text()); });
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));

    await page.goto(`http://127.0.0.1:${port}/render.html`, { waitUntil: 'load' });
    await page.waitForFunction('window.__ready === true', { timeout: 30000 });
    const gl = await page.evaluate('window.__glInfo()');
    console.error(`gl backend: ${gl.renderer}`);

    const modelUrl = `http://127.0.0.1:${port}/models/${encodeURIComponent(modelName)}`;
    const out = await page.evaluate(
      (u, e, o) => window.__renderAll(u, e, o),
      modelUrl, ext, { frames, res, posterRes },
    );

    // Write in the layout the Go handler reads.
    out.turntable.forEach((u, i) => {
      fs.writeFileSync(
        path.join(output, 'turntable', `frame_${String(i).padStart(4, '0')}.png`),
        dataUrlToBuffer(u),
      );
    });
    fs.writeFileSync(path.join(output, 'poster.png'), dataUrlToBuffer(out.poster));
    fs.writeFileSync(path.join(output, 'views', 'top.png'), dataUrlToBuffer(out.top));
    fs.writeFileSync(path.join(output, 'views', 'bottom.png'), dataUrlToBuffer(out.bottom));

    // One machine-readable line on stdout for the Go handler to log.
    process.stdout.write(JSON.stringify({
      ok: true,
      frames: out.turntable.length,
      triangles: out.meta.triangles,
      gl: gl.renderer,
      timings: out.timings,
    }) + '\n');
  } finally {
    await browser.close();
    server.close();
    fs.rmSync(userDataDir, { recursive: true, force: true });
  }
}

main().catch((e) => { console.error('worker error:', e.stack || e.message || e); process.exit(1); });
