// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// 3D preview smoke test (#500) — the replacement for CI's Blender smoke.
//
// The Blender smoke it replaces existed for #470: the release image had
// shipped with NO Blender because the two runtime stages drifted, and
// every 3D preview failed in production while local compose was fine.
// Blender left the image in #500, but that failure mode did not — the
// three.js worker has its own set of things that are only missing at
// RENDER time (Chromium's dlopen deps, the node_modules copy, the
// importmap paths, a loader that vanished from three's addons). A clean
// `docker build` proves none of it.
//
// So: run this INSIDE the built image and it drives the exact chain a
// real upload drives — node → puppeteer → Chromium/SwiftShader →
// render.html → a three.js loader → PNG frames on disk — once per
// supported format, and asserts the pixels are actually there.
//
//   docker run --rm --entrypoint node <image> /app/threejs/smoke.mjs
//
// Every fixture is synthesised here at runtime, so the repo carries no
// binary 3D fixtures and the smoke has no dependency on the (gitignored)
// seed corpus.
//
// Assertions per format, in order of how quietly each one fails:
//   1. worker.mjs exits 0
//   2. poster.png + the requested turntable frames + views/{top,bottom}
//      exist in the layout app/internal/preview/model.go reads
//   3. the poster has non-transparent pixels — the check that catches a
//      working pipeline rendering an empty scene, which is what a broken
//      loader or a dead GL context actually looks like on disk.
//
// The format list MUST stay in sync with loadModel() in render.html and
// threeJSExts in app/internal/preview/model.go. Drift shows up here as a
// failed smoke instead of as "worker exit 1" on a user's upload.

import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import zlib from 'node:zlib';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WORKER = path.join(HERE, 'worker.mjs');

// Small on purpose: the smoke proves the chain runs, not that it is
// fast. 4 frames at 64² keeps a 7-format sweep inside a CI step.
const FRAMES = 4;
const RES = 64;
const POSTER_RES = 128;

// ---------------------------------------------------------------------------
// Fixtures — the same unit cube in every format we claim to render.
// ---------------------------------------------------------------------------

// 8 corners of a cube, and 12 triangles over them (CCW, outward).
const CUBE_V = [
  [-1, -1, -1], [1, -1, -1], [1, 1, -1], [-1, 1, -1],
  [-1, -1, 1], [1, -1, 1], [1, 1, 1], [-1, 1, 1],
];
const CUBE_TRIS = [
  [4, 5, 6], [4, 6, 7], // +Z
  [1, 0, 3], [1, 3, 2], // -Z
  [5, 1, 2], [5, 2, 6], // +X
  [0, 4, 7], [0, 7, 3], // -X
  [3, 7, 6], [3, 6, 2], // +Y
  [0, 1, 5], [0, 5, 4], // -Y
];

function objFixture() {
  const lines = CUBE_V.map(([x, y, z]) => `v ${x} ${y} ${z}`);
  for (const [a, b, c] of CUBE_TRIS) lines.push(`f ${a + 1} ${b + 1} ${c + 1}`);
  return Buffer.from(`# smoke cube\n${lines.join('\n')}\n`, 'utf8');
}

function stlFixture() {
  // ASCII STL: one facet per triangle, normals left at 0 (the spec allows
  // it and STLLoader recomputes; the render path calls computeVertexNormals
  // for geometry without them anyway).
  const facets = CUBE_TRIS.map(([a, b, c]) => {
    const v = (i) => `      vertex ${CUBE_V[i].join(' ')}`;
    return [
      '  facet normal 0 0 0', '    outer loop', v(a), v(b), v(c),
      '    endloop', '  endfacet',
    ].join('\n');
  });
  return Buffer.from(`solid smoke\n${facets.join('\n')}\nendsolid smoke\n`, 'utf8');
}

function plyFixture() {
  const header = [
    'ply', 'format ascii 1.0',
    `element vertex ${CUBE_V.length}`,
    'property float x', 'property float y', 'property float z',
    `element face ${CUBE_TRIS.length}`,
    'property list uchar int vertex_indices',
    'end_header',
  ];
  const body = [
    ...CUBE_V.map((v) => v.join(' ')),
    ...CUBE_TRIS.map((t) => `3 ${t.join(' ')}`),
  ];
  return Buffer.from(`${header.join('\n')}\n${body.join('\n')}\n`, 'utf8');
}

// glTF buffer: 8 vec3 positions (float32) then 36 uint16 indices. 96 is
// already 4-byte aligned, so the index bufferView needs no padding.
function gltfBuffer() {
  const positions = Buffer.alloc(CUBE_V.length * 12);
  CUBE_V.forEach(([x, y, z], i) => {
    positions.writeFloatLE(x, i * 12);
    positions.writeFloatLE(y, i * 12 + 4);
    positions.writeFloatLE(z, i * 12 + 8);
  });
  const flat = CUBE_TRIS.flat();
  const indices = Buffer.alloc(flat.length * 2);
  flat.forEach((v, i) => indices.writeUInt16LE(v, i * 2));
  return { bin: Buffer.concat([positions, indices]), posLen: positions.length, idxCount: flat.length };
}

// glTF 2.0 JSON. `bufferUri` null produces the GLB flavour (buffer backed
// by the BIN chunk); a data: URI produces the standalone .gltf flavour.
function gltfJSON(bufferUri) {
  const { bin, posLen, idxCount } = gltfBuffer();
  const buffer = { byteLength: bin.length };
  if (bufferUri) buffer.uri = bufferUri;
  return {
    asset: { version: '2.0', generator: 'artist-alley smoke' },
    scene: 0,
    scenes: [{ nodes: [0] }],
    nodes: [{ mesh: 0 }],
    meshes: [{ primitives: [{ attributes: { POSITION: 0 }, indices: 1, mode: 4 }] }],
    buffers: [buffer],
    bufferViews: [
      { buffer: 0, byteOffset: 0, byteLength: posLen, target: 34962 },
      { buffer: 0, byteOffset: posLen, byteLength: idxCount * 2, target: 34963 },
    ],
    accessors: [
      {
        bufferView: 0, componentType: 5126, count: CUBE_V.length, type: 'VEC3',
        min: [-1, -1, -1], max: [1, 1, 1],
      },
      { bufferView: 1, componentType: 5123, count: idxCount, type: 'SCALAR' },
    ],
  };
}

function gltfFixture() {
  const { bin } = gltfBuffer();
  const json = gltfJSON(`data:application/octet-stream;base64,${bin.toString('base64')}`);
  return Buffer.from(JSON.stringify(json), 'utf8');
}

function glbFixture() {
  const { bin } = gltfBuffer();
  const pad = (buf, filler) => {
    const rem = buf.length % 4;
    return rem === 0 ? buf : Buffer.concat([buf, Buffer.alloc(4 - rem, filler)]);
  };
  const jsonChunk = pad(Buffer.from(JSON.stringify(gltfJSON(null)), 'utf8'), 0x20);
  const binChunk = pad(bin, 0x00);
  const header = Buffer.alloc(12);
  header.write('glTF', 0, 'ascii');
  header.writeUInt32LE(2, 4);
  header.writeUInt32LE(12 + 8 + jsonChunk.length + 8 + binChunk.length, 8);
  const chunkHead = (len, type) => {
    const b = Buffer.alloc(8);
    b.writeUInt32LE(len, 0);
    b.write(type, 4, 'ascii');
    return b;
  };
  return Buffer.concat([
    header,
    chunkHead(jsonChunk.length, 'JSON'), jsonChunk,
    chunkHead(binChunk.length, 'BIN\0'), binChunk,
  ]);
}

function daeFixture() {
  const positions = CUBE_V.flat().join(' ');
  // ColladaLoader wants a <p> of index pairs when two <input>s are
  // declared, so emit vertex+normal pairs and give every face the same
  // (unused, recomputed) normal.
  const p = CUBE_TRIS.flat().map((i) => `${i} 0`).join(' ');
  return Buffer.from(`<?xml version="1.0" encoding="utf-8"?>
<COLLADA xmlns="http://www.collada.org/2005/11/COLLADASchema" version="1.4.1">
  <asset><up_axis>Y_UP</up_axis></asset>
  <library_geometries>
    <geometry id="cube-mesh" name="cube">
      <mesh>
        <source id="cube-positions">
          <float_array id="cube-positions-array" count="${CUBE_V.length * 3}">${positions}</float_array>
          <technique_common>
            <accessor source="#cube-positions-array" count="${CUBE_V.length}" stride="3">
              <param name="X" type="float"/><param name="Y" type="float"/><param name="Z" type="float"/>
            </accessor>
          </technique_common>
        </source>
        <source id="cube-normals">
          <float_array id="cube-normals-array" count="3">0 0 1</float_array>
          <technique_common>
            <accessor source="#cube-normals-array" count="1" stride="3">
              <param name="X" type="float"/><param name="Y" type="float"/><param name="Z" type="float"/>
            </accessor>
          </technique_common>
        </source>
        <vertices id="cube-vertices">
          <input semantic="POSITION" source="#cube-positions"/>
        </vertices>
        <triangles count="${CUBE_TRIS.length}">
          <input semantic="VERTEX" source="#cube-vertices" offset="0"/>
          <input semantic="NORMAL" source="#cube-normals" offset="1"/>
          <p>${p}</p>
        </triangles>
      </mesh>
    </geometry>
  </library_geometries>
  <library_visual_scenes>
    <visual_scene id="Scene" name="Scene">
      <node id="cube" name="cube" type="NODE">
        <instance_geometry url="#cube-mesh"/>
      </node>
    </visual_scene>
  </library_visual_scenes>
  <scene><instance_visual_scene url="#Scene"/></scene>
</COLLADA>
`, 'utf8');
}

// FBXLoader decides ASCII-vs-binary with isFbxFormatASCII(), which samples
// the first 20 characters at triangular offsets (0, 1, 3, 6, 10, … 190) and
// declares the file "not ASCII" — i.e. rejects it as "Unknown format" — if
// ANY sampled character happens to equal the corresponding character of
// "Kaydara FBX Binary  \\". It is a coincidence check, and our first draft
// lost it on one character at offset 105.
//
// Rather than tune the header text until the dice come up right (a trap for
// whoever next edits a comment in this fixture), open with a run of ';' long
// enough to cover every sampled offset. ';' appears nowhere in that magic
// string, so no sample can match — the detector is now satisfied by
// construction. A line starting with ';' is an FBX comment, which the
// TextParser skips.
const FBX_ASCII_GUARD = `;${';'.repeat(200)}`;

function fbxFixture() {
  // ASCII FBX 7.4. FBXLoader's TextParser needs the FBXVersion line, a
  // Geometry with Vertices + PolygonVertexIndex, a Model, and the
  // Connections that bind geometry→model→root. FBX terminates each
  // polygon by bit-inverting its last index (~i), which is how the
  // parser finds face boundaries.
  const verts = CUBE_V.flat().join(',');
  const idx = CUBE_TRIS.map(([a, b, c]) => `${a},${b},${~c}`).join(',');
  const normals = CUBE_TRIS.flatMap(() => [0, 0, 1, 0, 0, 1, 0, 0, 1]).join(',');
  return Buffer.from(`${FBX_ASCII_GUARD}
; FBX 7.4.0 project file — artist-alley 3D preview smoke fixture

FBXHeaderExtension:  {
	FBXHeaderVersion: 1003
	FBXVersion: 7400
}
Definitions:  {
	Version: 100
	Count: 2
}
Objects:  {
	Geometry: 1000, "Geometry::cube", "Mesh" {
		Vertices: *${CUBE_V.length * 3} {
			a: ${verts}
		}
		PolygonVertexIndex: *${CUBE_TRIS.length * 3} {
			a: ${idx}
		}
		GeometryVersion: 124
		LayerElementNormal: 0 {
			Version: 101
			Name: ""
			MappingInformationType: "ByPolygonVertex"
			ReferenceInformationType: "Direct"
			Normals: *${CUBE_TRIS.length * 9} {
				a: ${normals}
			}
		}
		Layer: 0 {
			Version: 100
			LayerElement:  {
				Type: "LayerElementNormal"
				TypedIndex: 0
			}
		}
	}
	Model: 2000, "Model::cube", "Mesh" {
		Version: 232
		Properties70:  {
			P: "Lcl Translation", "Lcl Translation", "", "A",0,0,0
		}
		Shading: T
		Culling: "CullingOff"
	}
}
Connections:  {
	C: "OO",2000,0
	C: "OO",1000,2000
}
`, 'utf8');
}

// The formats this image claims to render. Keep in sync with
// render.html loadModel() + model.go threeJSExts.
const FORMATS = [
  { ext: 'glb', make: glbFixture },
  { ext: 'gltf', make: gltfFixture },
  { ext: 'obj', make: objFixture },
  { ext: 'fbx', make: fbxFixture },
  { ext: 'stl', make: stlFixture },
  { ext: 'ply', make: plyFixture },
  { ext: 'dae', make: daeFixture },
];

// ---------------------------------------------------------------------------
// PNG inspection — "did anything actually render?"
//
// The frames are transparent-background RGBA8. A pipeline that runs
// perfectly but renders nothing writes a fully-transparent PNG, which
// passes every exists/size check. So decode for real: parse IHDR, inflate
// the IDAT stream, undo the per-scanline filters, and count pixels with
// alpha > 0. No image dependency — node:zlib is enough.
// ---------------------------------------------------------------------------

function opaquePixelCount(file) {
  const buf = fs.readFileSync(file);
  if (buf.length < 8 || buf.readUInt32BE(0) !== 0x89504e47) throw new Error(`${file}: not a PNG`);

  let off = 8;
  let ihdr = null;
  const idat = [];
  while (off + 8 <= buf.length) {
    const len = buf.readUInt32BE(off);
    const type = buf.toString('ascii', off + 4, off + 8);
    const data = buf.subarray(off + 8, off + 8 + len);
    if (type === 'IHDR') {
      ihdr = {
        width: data.readUInt32BE(0), height: data.readUInt32BE(4),
        depth: data[8], colorType: data[9], interlace: data[12],
      };
    } else if (type === 'IDAT') idat.push(data);
    else if (type === 'IEND') break;
    off += 12 + len;
  }
  if (!ihdr) throw new Error(`${file}: no IHDR`);
  if (ihdr.depth !== 8 || ihdr.colorType !== 6 || ihdr.interlace !== 0) {
    throw new Error(`${file}: expected non-interlaced RGBA8, got depth=${ihdr.depth} colorType=${ihdr.colorType} interlace=${ihdr.interlace}`);
  }

  const raw = zlib.inflateSync(Buffer.concat(idat));
  const bpp = 4;
  const stride = ihdr.width * bpp;
  const out = Buffer.alloc(ihdr.height * stride);
  let src = 0;
  for (let y = 0; y < ihdr.height; y++) {
    const filter = raw[src++];
    const line = raw.subarray(src, src + stride);
    src += stride;
    const cur = out.subarray(y * stride, (y + 1) * stride);
    const prev = y > 0 ? out.subarray((y - 1) * stride, y * stride) : null;
    for (let x = 0; x < stride; x++) {
      const a = x >= bpp ? cur[x - bpp] : 0;      // left
      const b = prev ? prev[x] : 0;               // up
      const c = prev && x >= bpp ? prev[x - bpp] : 0; // upper-left
      let v = line[x];
      switch (filter) {
        case 0: break;
        case 1: v += a; break;
        case 2: v += b; break;
        case 3: v += (a + b) >> 1; break;
        case 4: {
          const p = a + b - c;
          const pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c);
          v += (pa <= pb && pa <= pc) ? a : (pb <= pc ? b : c);
          break;
        }
        default: throw new Error(`${file}: bad PNG filter ${filter} on row ${y}`);
      }
      cur[x] = v & 0xff;
    }
  }

  let opaque = 0;
  for (let i = 3; i < out.length; i += 4) if (out[i] > 0) opaque++;
  return { opaque, total: ihdr.width * ihdr.height };
}

// ---------------------------------------------------------------------------

function renderOne(ext, make, root) {
  const work = fs.mkdtempSync(path.join(root, `${ext}-`));
  const out = path.join(work, 'out');
  const model = path.join(work, `smoke.${ext}`);
  fs.writeFileSync(model, make());

  const r = spawnSync(process.execPath, [
    WORKER,
    '--input', model,
    '--workdir', work,
    '--output', out,
    '--frames', String(FRAMES),
    '--res', String(RES),
    '--poster-res', String(POSTER_RES),
  ], { encoding: 'utf8', timeout: 240_000 });

  if (r.status !== 0) {
    const tail = (r.stderr || '').trim().split('\n').slice(-20).join('\n');
    throw new Error(`worker exit ${r.status}${r.signal ? ` (signal ${r.signal})` : ''}\n${tail}`);
  }

  const expected = [
    path.join(out, 'poster.png'),
    path.join(out, 'views', 'top.png'),
    path.join(out, 'views', 'bottom.png'),
    ...Array.from({ length: FRAMES }, (_, i) =>
      path.join(out, 'turntable', `frame_${String(i).padStart(4, '0')}.png`)),
  ];
  for (const f of expected) {
    if (!fs.existsSync(f) || fs.statSync(f).size === 0) {
      throw new Error(`missing or empty output: ${path.relative(out, f)}`);
    }
  }

  const poster = opaquePixelCount(path.join(out, 'poster.png'));
  if (poster.opaque === 0) {
    throw new Error('poster.png is fully transparent — the chain ran but rendered an empty scene');
  }

  let gl = 'unknown';
  const m = (r.stderr || '').match(/gl backend: (.+)/);
  if (m) gl = m[1].trim();
  return { files: expected.length, opaque: poster.opaque, total: poster.total, gl };
}

const root = fs.mkdtempSync(path.join(os.tmpdir(), 'aa-3d-smoke-'));
const failures = [];
try {
  for (const { ext, make } of FORMATS) {
    process.stdout.write(`  ${ext.padEnd(5)} … `);
    try {
      const r = renderOne(ext, make, root);
      const pct = ((r.opaque / r.total) * 100).toFixed(1);
      process.stdout.write(`ok  (${r.files} files, poster ${pct}% covered, gl=${r.gl})\n`);
    } catch (e) {
      process.stdout.write('FAILED\n');
      failures.push(`${ext}: ${e.message}`);
    }
  }
} finally {
  fs.rmSync(root, { recursive: true, force: true });
}

if (failures.length) {
  console.error('\n3D preview smoke FAILED:\n');
  for (const f of failures) console.error(`  - ${f}\n`);
  process.exit(1);
}
console.log(`\n3D preview smoke OK: ${FORMATS.length}/${FORMATS.length} formats rendered.`);
