// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// #497 spike — headless three.js turntable worker (ADR 0069 / epic #496).
// EXPERIMENTAL. Proves a Puppeteer + SwiftShader-WebGL renderer can
// stand in for Blender's preview.model render step. Produces the SAME
// output shapes as app/internal/preview/model.go so it could later be a
// drop-in for #498:
//   - 36-frame turntable @ 512² (10° steps)
//   - 6×6 sprite sheet (160px cell → 960²) sprites.jpg (q75) + WebVTT
//   - raster ladder from frame 0: col(320² cover) / preview(1024) /
//     screen(1920) / hires(4096)  — webp
//
// Usage: node worker.mjs <model1> [<model2> ...]
//   outputs to out/<basename>/ + appends a row to out/results.json

import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer';
import sharp from 'sharp';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const THREE_DIR = path.join(HERE, 'node_modules', 'three');
const OUT = path.join(HERE, 'out');

// --- output-shape constants (mirror model.go) ------------------------------
const FRAMES = 36;
const RES = 512;
const SPRITE_COLS = 6, SPRITE_ROWS = 6, SPRITE_CELL = 160; // → 960²
const TURNTABLE_SECONDS = 4.0;
const LADDER = [
  { key: 'col', maxDim: 320, cover: true, quality: 82 },
  { key: 'preview', maxDim: 1024, cover: false, quality: 86 },
  { key: 'screen', maxDim: 1920, cover: false, quality: 90 },
  { key: 'hires', maxDim: 4096, cover: false, quality: 95 },
];
const SPRITE_BG = { r: 26, g: 26, b: 28 }; // JPEG has no alpha; flatten neutral

const CONTENT_TYPES = {
  '.html': 'text/html', '.js': 'text/javascript', '.mjs': 'text/javascript',
  '.glb': 'model/gltf-binary', '.gltf': 'model/gltf+json', '.fbx': 'application/octet-stream',
  '.obj': 'text/plain', '.bin': 'application/octet-stream', '.png': 'image/png',
  '.jpg': 'image/jpeg', '.wasm': 'application/wasm',
};

// Serve render.html, /three/* (module graph), and the model dir.
function startServer(modelDir) {
  return new Promise((resolve) => {
    const server = http.createServer((req, res) => {
      const url = decodeURIComponent(req.url.split('?')[0]);
      let file;
      if (url === '/' || url === '/render.html') file = path.join(HERE, 'render.html');
      else if (url.startsWith('/three/')) file = path.join(THREE_DIR, url.slice('/three/'.length));
      else if (url.startsWith('/models/')) file = path.join(modelDir, url.slice('/models/'.length));
      else { res.statusCode = 404; return res.end('not found'); }
      fs.readFile(file, (err, data) => {
        if (err) { res.statusCode = 404; return res.end('nf: ' + file); }
        res.setHeader('Content-Type', CONTENT_TYPES[path.extname(file).toLowerCase()] || 'application/octet-stream');
        res.end(data);
      });
    });
    server.listen(0, '127.0.0.1', () => resolve(server));
  });
}

function dataUrlToBuffer(u) {
  return Buffer.from(u.slice(u.indexOf(',') + 1), 'base64');
}

async function writeSprite(frames, outDir) {
  const cells = await Promise.all(
    frames.slice(0, SPRITE_COLS * SPRITE_ROWS).map((buf) =>
      sharp(buf).resize(SPRITE_CELL, SPRITE_CELL, { fit: 'contain', background: SPRITE_BG }).toBuffer(),
    ),
  );
  const composite = cells.map((input, i) => ({
    input,
    left: (i % SPRITE_COLS) * SPRITE_CELL,
    top: Math.floor(i / SPRITE_COLS) * SPRITE_CELL,
  }));
  await sharp({
    create: {
      width: SPRITE_COLS * SPRITE_CELL, height: SPRITE_ROWS * SPRITE_CELL,
      channels: 3, background: SPRITE_BG,
    },
  }).composite(composite).jpeg({ quality: 75 }).toFile(path.join(outDir, 'sprites.jpg'));

  // WebVTT: each cell owns 1/loaded of a TURNTABLE_SECONDS loop, xywh-indexed.
  const loaded = Math.min(frames.length, SPRITE_COLS * SPRITE_ROWS);
  const interval = TURNTABLE_SECONDS / loaded;
  const ts = (s) => {
    const m = Math.floor(s / 60), sec = (s % 60).toFixed(3).padStart(6, '0');
    return `00:${String(m).padStart(2, '0')}:${sec}`;
  };
  let vtt = 'WEBVTT\n\n';
  for (let i = 0; i < loaded; i++) {
    const x = (i % SPRITE_COLS) * SPRITE_CELL, y = Math.floor(i / SPRITE_COLS) * SPRITE_CELL;
    vtt += `${ts(i * interval)} --> ${ts((i + 1) * interval)}\n`;
    vtt += `sprites.jpg#xywh=${x},${y},${SPRITE_CELL},${SPRITE_CELL}\n\n`;
  }
  fs.writeFileSync(path.join(outDir, 'sprites.vtt'), vtt);
}

async function fanLadder(frame0, outDir) {
  for (const v of LADDER) {
    const img = sharp(frame0);
    if (v.cover) img.resize(v.maxDim, v.maxDim, { fit: 'cover' });
    else img.resize(v.maxDim, v.maxDim, { fit: 'inside', withoutEnlargement: true });
    await img.webp({ quality: v.quality }).toFile(path.join(outDir, `${v.key}.webp`));
  }
}

async function main() {
  const models = process.argv.slice(2);
  if (!models.length) { console.error('usage: node worker.mjs <model...>'); process.exit(2); }
  fs.mkdirSync(OUT, { recursive: true });

  const browser = await puppeteer.launch({
    headless: true,
    args: [
      '--no-sandbox', '--disable-setuid-sandbox',
      // Force software WebGL via SwiftShader — the exact stack #497 must
      // evaluate (CI/servers have no GPU). Chromium 131 gates SwiftShader
      // for WebGL behind --enable-unsafe-swiftshader.
      '--use-gl=angle', '--use-angle=swiftshader', '--enable-unsafe-swiftshader',
      '--enable-webgl', '--ignore-gpu-blocklist',
    ],
  });

  const results = [];
  for (const modelPath of models) {
    const abs = path.resolve(modelPath);
    const name = path.basename(abs).replace(/\.[^.]+$/, '');
    const ext = path.extname(abs).slice(1);
    const outDir = path.join(OUT, name);
    fs.mkdirSync(outDir, { recursive: true });
    const server = await startServer(path.dirname(abs));
    const port = server.address().port;
    const page = await browser.newPage();
    const row = { name, ext, ok: false };
    try {
      await page.goto(`http://127.0.0.1:${port}/render.html`, { waitUntil: 'load' });
      await page.waitForFunction('window.__ready === true', { timeout: 20000 });
      row.gl = await page.evaluate('window.__glInfo()');

      const modelUrl = `http://127.0.0.1:${port}/models/${encodeURIComponent(path.basename(abs))}`;
      const t0 = Date.now();
      const out = await page.evaluate(
        (u, e, o) => window.__renderTurntable(u, e, o),
        modelUrl, ext, { frames: FRAMES, res: RES },
      );
      const captureMs = Date.now() - t0;

      const frames = out.urls.map(dataUrlToBuffer);
      // Persist a couple of raw frames for the visual comparison.
      fs.writeFileSync(path.join(outDir, 'frame_0000.png'), frames[0]);
      fs.writeFileSync(path.join(outDir, 'frame_0018.png'), frames[18]);

      const compT0 = Date.now();
      await writeSprite(frames, outDir);
      await fanLadder(frames[0], outDir);
      const compMs = Date.now() - compT0;

      row.ok = true;
      row.triangles = out.meta.triangles;
      row.timings = { ...out.timings, capture_ms: captureMs, composite_ms: compMs };
      console.log(`OK  ${name} (${ext}) tris=${out.meta.triangles} gl=${row.gl.renderer} render=${out.timings.render_ms}ms total=${captureMs + compMs}ms`);
    } catch (e) {
      row.error = String(e.message || e);
      console.error(`ERR ${name} (${ext}): ${row.error}`);
    } finally {
      await page.close();
      server.close();
    }
    results.push(row);
  }

  await browser.close();
  fs.writeFileSync(path.join(OUT, 'results.json'), JSON.stringify(results, null, 2));
  console.log(`\nwrote ${OUT}/results.json (${results.filter((r) => r.ok).length}/${results.length} ok)`);
}

main().catch((e) => { console.error(e); process.exit(1); });
