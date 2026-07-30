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
// importmap paths, the /shared/ modules the Dockerfiles copy in, a
// loader that vanished from three's addons). A clean `docker build`
// proves none of it.
//
// So: run this INSIDE the built image and it drives the exact chain a
// real upload drives — node → puppeteer → Chromium/SwiftShader →
// render.html → modelLoader.js → PNG frames on disk — once per
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
//   3. the poster has non-transparent pixels — catches a working
//      pipeline rendering an empty scene, which is what a broken loader
//      or a dead GL context looks like on disk
//   4. every material came out MeshStandardMaterial — the normalisation
//      modelLoader.js applies. FBX Phong, OBJ Basic and unlit glTF all
//      arrive as something else and DON'T respond to the IBL rig, so a
//      missed upgrade renders a flat silhouette
//   5. for the textured fixtures: the declared texture count actually
//      decoded (worker reports `textured` / `broken_textures`) AND the
//      poster carries saturated, varied colour
//
// (4) and (5) exist because of #689: this page used to carry its own
// loader with no MTLLoader and no upgrade pass, so every OBJ rendered
// as untextured white and every unlit glTF as a flat silhouette — and
// the smoke stayed green, because flat white geometry sails through an
// "are any pixels non-transparent" check. A test that only proves
// something rendered cannot notice that the wrong thing rendered.
//
// The format list MUST stay in sync with LOADABLE_EXTS in
// web/src/lib/3d/modelLoader.js and threeJSExts in
// app/internal/preview/model.go. Drift shows up here as a failed smoke
// instead of as "worker exit 1" on a user's upload.

import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import zlib from 'node:zlib';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WORKER = path.join(HERE, 'worker.mjs');

// Small on purpose: the smoke proves the chain runs, not that it is
// fast. 4 frames at 64² keeps the sweep inside a CI step.
const FRAMES = 4;
const RES = 64;
const POSTER_RES = 128;

// ---------------------------------------------------------------------------
// PNG encoding — for the texture fixtures. Small enough to hand-roll on
// node:zlib, which keeps the "no binary fixtures in the repo" property.
// ---------------------------------------------------------------------------

const CRC_TABLE = (() => {
  const t = new Int32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c;
  }
  return t;
})();

function crc32(buf) {
  let c = -1;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return (c ^ -1) >>> 0;
}

function pngChunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length, 0);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body), 0);
  return Buffer.concat([len, body, crc]);
}

/** RGB8 PNG from a width×height array of [r,g,b] triples. */
function encodePNG(width, height, pixel) {
  const raw = Buffer.alloc(height * (1 + width * 3));
  let o = 0;
  for (let y = 0; y < height; y++) {
    raw[o++] = 0; // filter: none
    for (let x = 0; x < width; x++) {
      const [r, g, b] = pixel(x, y);
      raw[o++] = r; raw[o++] = g; raw[o++] = b;
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8;  // bit depth
  ihdr[9] = 2;  // colour type: truecolour
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk('IHDR', ihdr),
    pngChunk('IDAT', zlib.deflateSync(raw)),
    pngChunk('IEND', Buffer.alloc(0)),
  ]);
}

// Four saturated quadrants. Saturated on purpose: an untextured render
// is white/grey, so "does the poster contain saturated colour" is a
// yes/no answer to "did the texture make it onto the geometry".
const SWATCHES = [
  [220, 30, 30],   // red
  [30, 200, 60],   // green
  [40, 70, 220],   // blue
  [230, 200, 20],  // yellow
];
const CHECKER_PNG = encodePNG(16, 16, (x, y) => SWATCHES[(y < 8 ? 0 : 2) + (x < 8 ? 0 : 1)]);

// ---------------------------------------------------------------------------
// Fixtures.
//
// Untextured formats get the unit cube; the textured ones get a quad
// (4 verts, 2 tris, full 0..1 UVs) facing the poster camera, which is
// the frame the colour assertion reads. Turntable frames past 90° see
// the quad edge-on or back-face — deliberate: the poster is what the
// browse grid shows and what we assert on.
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

// Quad in the XY plane facing +Z (the poster camera's direction).
const QUAD_V = [[-1, -1, 0], [1, -1, 0], [1, 1, 0], [-1, 1, 0]];
const QUAD_UV = [[0, 1], [1, 1], [1, 0], [0, 0]];
const QUAD_TRIS = [[0, 1, 2], [0, 2, 3]];

function objCubeFixture() {
  const lines = CUBE_V.map(([x, y, z]) => `v ${x} ${y} ${z}`);
  for (const [a, b, c] of CUBE_TRIS) lines.push(`f ${a + 1} ${b + 1} ${c + 1}`);
  return Buffer.from(`# smoke cube\n${lines.join('\n')}\n`, 'utf8');
}

// OBJ + MTL + texture in a subdirectory — the shape a real upload has
// (`mtllib` by name, `map_Kd` by relative path). OBJLoader fetches
// neither on its own: modelLoader.js resolves the .mtl from the
// companion list worker.mjs enumerates, and MTLLoader resolves the
// texture relative to the .mtl. #689 was this whole path missing.
function objTexturedFiles() {
  const lines = [
    '# smoke textured quad',
    'mtllib smoke.mtl',
    ...QUAD_V.map(([x, y, z]) => `v ${x} ${y} ${z}`),
    ...QUAD_UV.map(([u, v]) => `vt ${u} ${v}`),
    'vn 0 0 1',
    'usemtl checker',
    ...QUAD_TRIS.map(([a, b, c]) =>
      `f ${a + 1}/${a + 1}/1 ${b + 1}/${b + 1}/1 ${c + 1}/${c + 1}/1`),
  ];
  return {
    'smoke.obj': Buffer.from(`${lines.join('\n')}\n`, 'utf8'),
    'smoke.mtl': Buffer.from('newmtl checker\nKd 1 1 1\nmap_Kd Textures/checker.png\n', 'utf8'),
    'Textures/checker.png': CHECKER_PNG,
  };
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

// glTF buffer for the textured quad: 4 vec3 positions (float32), 4 vec2
// UVs (float32), then 6 uint16 indices. 48 and 80 are both 4-byte
// aligned, so no bufferView needs padding.
function quadBuffer() {
  const positions = Buffer.alloc(QUAD_V.length * 12);
  QUAD_V.forEach(([x, y, z], i) => {
    positions.writeFloatLE(x, i * 12);
    positions.writeFloatLE(y, i * 12 + 4);
    positions.writeFloatLE(z, i * 12 + 8);
  });
  const uvs = Buffer.alloc(QUAD_UV.length * 8);
  QUAD_UV.forEach(([u, v], i) => {
    uvs.writeFloatLE(u, i * 8);
    uvs.writeFloatLE(v, i * 8 + 4);
  });
  const flat = QUAD_TRIS.flat();
  const indices = Buffer.alloc(flat.length * 2);
  flat.forEach((v, i) => indices.writeUInt16LE(v, i * 2));
  return { positions, uvs, indices, idxCount: flat.length };
}

/**
 * glTF 2.0 JSON for the textured quad.
 *
 * opts.imageUri  — set for the standalone .gltf flavour (external
 *                  texture file, staged as a sibling); omit for GLB,
 *                  where the PNG rides in the BIN chunk.
 * opts.bufferUri — as above for the vertex buffer.
 * opts.unlit     — add KHR_materials_unlit, the shape of the asset in
 *                  #689: GLTFLoader produces MeshBasicMaterial for it,
 *                  which ignores the whole lighting rig unless the
 *                  material-upgrade pass runs.
 */
function quadGLTF(opts) {
  const { positions, uvs, indices, idxCount } = quadBuffer();
  const png = opts.imageUri ? null : CHECKER_PNG;
  const pad4 = (n) => (n % 4 === 0 ? 0 : 4 - (n % 4));
  const posOff = 0;
  const uvOff = positions.length;
  const idxOff = uvOff + uvs.length;
  const imgOff = idxOff + indices.length + pad4(idxOff + indices.length);
  const total = png ? imgOff + png.length : idxOff + indices.length;

  const bufferViews = [
    { buffer: 0, byteOffset: posOff, byteLength: positions.length, target: 34962 },
    { buffer: 0, byteOffset: uvOff, byteLength: uvs.length, target: 34962 },
    { buffer: 0, byteOffset: idxOff, byteLength: indices.length, target: 34963 },
  ];
  if (png) bufferViews.push({ buffer: 0, byteOffset: imgOff, byteLength: png.length });

  const buffer = { byteLength: total };
  if (opts.bufferUri) buffer.uri = opts.bufferUri;

  const material = {
    name: 'checker',
    pbrMetallicRoughness: { baseColorTexture: { index: 0 }, metallicFactor: 0, roughnessFactor: 0.6 },
  };
  if (opts.unlit) material.extensions = { KHR_materials_unlit: {} };

  const json = {
    asset: { version: '2.0', generator: 'artist-alley smoke' },
    scene: 0,
    scenes: [{ nodes: [0] }],
    nodes: [{ mesh: 0 }],
    meshes: [{
      primitives: [{ attributes: { POSITION: 0, TEXCOORD_0: 1 }, indices: 2, material: 0, mode: 4 }],
    }],
    materials: [material],
    textures: [{ source: 0, sampler: 0 }],
    samplers: [{}],
    images: [opts.imageUri ? { uri: opts.imageUri } : { bufferView: 3, mimeType: 'image/png' }],
    buffers: [buffer],
    bufferViews,
    accessors: [
      {
        bufferView: 0, componentType: 5126, count: QUAD_V.length, type: 'VEC3',
        min: [-1, -1, 0], max: [1, 1, 0],
      },
      { bufferView: 1, componentType: 5126, count: QUAD_UV.length, type: 'VEC2' },
      { bufferView: 2, componentType: 5123, count: idxCount, type: 'SCALAR' },
    ],
  };
  if (opts.unlit) {
    json.extensionsUsed = ['KHR_materials_unlit'];
    json.extensionsRequired = ['KHR_materials_unlit'];
  }
  const bin = Buffer.alloc(total);
  positions.copy(bin, posOff);
  uvs.copy(bin, uvOff);
  indices.copy(bin, idxOff);
  if (png) png.copy(bin, imgOff);
  return { json, bin };
}

// .gltf flavour: vertex buffer as a data: URI, texture as a sibling file
// the Go handler's stageCompanions would have written next to the model.
function gltfTexturedFiles() {
  const { json, bin } = quadGLTF({
    bufferUri: null, imageUri: 'Textures/checker.png',
  });
  json.buffers[0].uri = `data:application/octet-stream;base64,${bin.toString('base64')}`;
  return {
    'smoke.gltf': Buffer.from(JSON.stringify(json), 'utf8'),
    'Textures/checker.png': CHECKER_PNG,
  };
}

function glbFrom({ json, bin }) {
  const pad = (buf, filler) => {
    const rem = buf.length % 4;
    return rem === 0 ? buf : Buffer.concat([buf, Buffer.alloc(4 - rem, filler)]);
  };
  const jsonChunk = pad(Buffer.from(JSON.stringify(json), 'utf8'), 0x20);
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

// FBX + a texture in a subdirectory — the shape 127 of the seed
// catalogue's 131 FBX have, and the case the untextured fixture above
// could never fail on (#753).
//
// Scope, precisely: this exercises the RENDERER half only. It writes
// `Textures/checker.png` into the work dir itself and worker.mjs
// enumerates what it finds, so nothing here calls format3d's FBX reader
// — that side is covered by fbx_test.go against a real Kenney export.
// What only this case can catch is the loader-side half: FBXLoader
// reduces `Textures\checker.png` to its BASENAME before requesting it
// (`images[id].split('\\').pop()`), so it asks the work dir for
// `checker.png` while the file sits at `Textures/checker.png`. Plain
// relative resolution 404s and the poster comes out untextured; the
// companion manager's basename fallback — which render.html did not pass
// before #753 — is what bridges it.
//
// Verified red-first: with the `manager:` argument removed from
// render.html this case fails with "texture(s) bound but never decoded
// … a 404 or a decode failure inside headless Chromium", and the other
// nine cases stay green — so it pins the FBX path specifically and the
// manager costs GLB/glTF/OBJ nothing.
//
// Backslash separators on purpose: that is what every FBX writes, and a
// fixture using `/` would pass while the real corpus failed.
function fbxTexturedFiles() {
  const verts = QUAD_V.flat().join(',');
  const idx = QUAD_TRIS.map(([a, b, c]) => `${a},${b},${~c}`).join(',');
  const perPolyVertex = QUAD_TRIS.flat();
  const normals = perPolyVertex.flatMap(() => [0, 0, 1]).join(',');
  const uvs = perPolyVertex.flatMap((i) => QUAD_UV[i]).join(',');
  return {
    'smoke.fbx': Buffer.from(`${FBX_ASCII_GUARD}
; FBX 7.4.0 project file — artist-alley textured-FBX smoke fixture

FBXHeaderExtension:  {
	FBXHeaderVersion: 1003
	FBXVersion: 7400
}
Definitions:  {
	Version: 100
	Count: 5
}
Objects:  {
	Geometry: 1000, "Geometry::quad", "Mesh" {
		Vertices: *${QUAD_V.length * 3} {
			a: ${verts}
		}
		PolygonVertexIndex: *${QUAD_TRIS.length * 3} {
			a: ${idx}
		}
		GeometryVersion: 124
		LayerElementNormal: 0 {
			Version: 101
			Name: ""
			MappingInformationType: "ByPolygonVertex"
			ReferenceInformationType: "Direct"
			Normals: *${perPolyVertex.length * 3} {
				a: ${normals}
			}
		}
		LayerElementUV: 0 {
			Version: 101
			Name: "UVMap"
			MappingInformationType: "ByPolygonVertex"
			ReferenceInformationType: "Direct"
			UV: *${perPolyVertex.length * 2} {
				a: ${uvs}
			}
		}
		Layer: 0 {
			Version: 100
			LayerElement:  {
				Type: "LayerElementNormal"
				TypedIndex: 0
			}
			LayerElement:  {
				Type: "LayerElementUV"
				TypedIndex: 0
			}
		}
	}
	Model: 2000, "Model::quad", "Mesh" {
		Version: 232
		Shading: T
		Culling: "CullingOff"
	}
	Material: 3000, "Material::checker", "" {
		Version: 102
		ShadingModel: "phong"
		MultiLayer: 0
		Properties70:  {
			P: "DiffuseColor", "Color", "", "A",1,1,1
		}
	}
	Texture: 4000, "Texture::checker", "" {
		Type: "TextureVideoClip"
		Version: 202
		TextureName: "Texture::checker"
		FileName: "Textures\\checker.png"
		RelativeFilename: "Textures\\checker.png"
		ModelUVTranslation: 0,0
		ModelUVScaling: 1,1
	}
	Video: 5000, "Video::checker", "Clip" {
		Type: "Clip"
		Properties70:  {
		}
		UseMipMap: 0
		Filename: "Textures\\checker.png"
		RelativeFilename: "Textures\\checker.png"
	}
}
Connections:  {
	C: "OO",2000,0
	C: "OO",1000,2000
	C: "OO",3000,2000
	C: "OP",4000,3000, "DiffuseColor"
	C: "OO",5000,4000
}
`, 'utf8'),
    'Textures/checker.png': CHECKER_PNG,
  };
}

// The cases this image claims to render. Every LOADABLE_EXTS entry in
// modelLoader.js appears at least once.
//
//   files    — relative path → bytes, all written into the work dir the
//              way the Go handler stages a model + its companions
//   model    — which of those is the model to render
//   textures — how many materials must end up with a DECODED texture.
//              > 0 also turns on the poster colour-variance check.
const CASES = [
  {
    name: 'glb',
    model: 'smoke.glb',
    files: () => ({ 'smoke.glb': glbFrom(quadGLTF({})) }),
    textures: 1,
  },
  {
    // The #689 asset shape: KHR_materials_unlit. GLTFLoader hands back
    // MeshBasicMaterial, which ignores the IBL + three-point rig
    // entirely; without the upgrade pass the thumbnail is a flat
    // silhouette while the viewer is shaded.
    name: 'glb-unlit',
    model: 'smoke.glb',
    files: () => ({ 'smoke.glb': glbFrom(quadGLTF({ unlit: true })) }),
    textures: 1,
  },
  { name: 'gltf', model: 'smoke.gltf', files: gltfTexturedFiles, textures: 1 },
  { name: 'obj', model: 'smoke.obj', files: objTexturedFiles, textures: 1 },
  {
    name: 'obj-bare',
    model: 'smoke.obj',
    files: () => ({ 'smoke.obj': objCubeFixture() }),
    textures: 0,
  },
  { name: 'fbx', model: 'smoke.fbx', files: () => ({ 'smoke.fbx': fbxFixture() }), textures: 0 },
  { name: 'fbx-textured', model: 'smoke.fbx', files: fbxTexturedFiles, textures: 1 },
  { name: 'stl', model: 'smoke.stl', files: () => ({ 'smoke.stl': stlFixture() }), textures: 0 },
  { name: 'ply', model: 'smoke.ply', files: () => ({ 'smoke.ply': plyFixture() }), textures: 0 },
  { name: 'dae', model: 'smoke.dae', files: () => ({ 'smoke.dae': daeFixture() }), textures: 0 },
];

// ---------------------------------------------------------------------------
// PNG inspection — "did anything actually render, and did it render the
// right thing?"
//
// The frames are transparent-background RGBA8. A pipeline that runs
// perfectly but renders nothing writes a fully-transparent PNG, which
// passes every exists/size check; one that loses its textures writes flat
// white, which passes every alpha check. So decode for real: parse IHDR,
// inflate the IDAT stream, undo the per-scanline filters, and look at the
// pixels. No image dependency — node:zlib is enough.
// ---------------------------------------------------------------------------

function decodeRGBA(file) {
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
  return { width: ihdr.width, height: ihdr.height, rgba: out };
}

// opaque pixel count + how much saturated colour is present. Grey and
// white pixels have saturation ~0 whatever their brightness, so
// `saturated` is exactly "pixels that could only come from a texture or
// a coloured material" — the discriminator flat-white geometry fails.
function posterStats(file) {
  const { width, height, rgba } = decodeRGBA(file);
  let opaque = 0;
  let saturated = 0;
  const buckets = new Set();
  for (let i = 0; i < rgba.length; i += 4) {
    if (rgba[i + 3] === 0) continue;
    opaque++;
    const r = rgba[i], g = rgba[i + 1], b = rgba[i + 2];
    const max = Math.max(r, g, b), min = Math.min(r, g, b);
    if (max > 0 && (max - min) / max >= 0.25) {
      saturated++;
      buckets.add(`${r >> 5},${g >> 5},${b >> 5}`);
    }
  }
  return { opaque, total: width * height, saturated, hues: buckets.size };
}

// ---------------------------------------------------------------------------

function renderOne(testCase, root) {
  const work = fs.mkdtempSync(path.join(root, `${testCase.name}-`));
  const out = path.join(work, 'out');
  for (const [rel, bytes] of Object.entries(testCase.files())) {
    const dst = path.join(work, rel);
    fs.mkdirSync(path.dirname(dst), { recursive: true });
    fs.writeFileSync(dst, bytes);
  }

  const r = spawnSync(process.execPath, [
    WORKER,
    '--input', path.join(work, testCase.model),
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

  const poster = posterStats(path.join(out, 'poster.png'));
  if (poster.opaque === 0) {
    throw new Error('poster.png is fully transparent — the chain ran but rendered an empty scene');
  }

  // The worker's stdout line carries the material census render.html
  // takes after loading. Parse it and hold the load path to its
  // contract, which pixels alone can't express.
  const jsonLine = (r.stdout || '').trim().split('\n').filter((l) => l.startsWith('{')).pop();
  if (!jsonLine) throw new Error(`worker wrote no JSON summary\n${(r.stdout || '').slice(0, 400)}`);
  const summary = JSON.parse(jsonLine);

  const wrongType = (summary.material_types || []).filter((t) => t !== 'MeshStandardMaterial');
  if (wrongType.length > 0) {
    throw new Error(
      `material(s) not normalised to MeshStandardMaterial: ${wrongType.join(', ')} — ` +
      'the upgrade pass in modelLoader.js did not run, so this renders unlit/flat (#689)',
    );
  }

  if (testCase.textures > 0) {
    if (summary.broken_textures.length > 0) {
      throw new Error(
        `texture(s) bound but never decoded: ${summary.broken_textures.join(', ')} — ` +
        'a 404 or a decode failure inside headless Chromium',
      );
    }
    if (summary.textured < testCase.textures) {
      throw new Error(
        `expected ${testCase.textures} textured material(s), worker reports ${summary.textured} ` +
        '— the texture never reached the material (missing MTLLoader / companion / UVs)',
      );
    }
    // …and it has to be visible, not merely attached: an untextured
    // render is white/grey, so saturated pixels can only come from the
    // texture. 2% of the opaque area is a floor a real texture clears
    // easily (the fixture's quad fills the frame).
    const satPct = (poster.saturated / poster.opaque) * 100;
    if (satPct < 2 || poster.hues < 2) {
      throw new Error(
        `poster is ${satPct.toFixed(1)}% saturated across ${poster.hues} colour bucket(s) — ` +
        'the textured fixture rendered flat/greyscale, which is what a lost texture looks like',
      );
    }
  }

  let gl = 'unknown';
  const m = (r.stderr || '').match(/gl backend: (.+)/);
  if (m) gl = m[1].trim();
  return {
    files: expected.length,
    opaque: poster.opaque,
    total: poster.total,
    saturated: poster.saturated,
    materials: summary.materials,
    textured: summary.textured,
    gl,
  };
}

const root = fs.mkdtempSync(path.join(os.tmpdir(), 'aa-3d-smoke-'));
const failures = [];
try {
  for (const testCase of CASES) {
    process.stdout.write(`  ${testCase.name.padEnd(9)} … `);
    try {
      const r = renderOne(testCase, root);
      const pct = ((r.opaque / r.total) * 100).toFixed(1);
      const sat = ((r.saturated / Math.max(r.opaque, 1)) * 100).toFixed(1);
      process.stdout.write(
        `ok  (${r.files} files, poster ${pct}% covered / ${sat}% saturated, ` +
        `${r.textured}/${r.materials} materials textured, gl=${r.gl})\n`,
      );
    } catch (e) {
      process.stdout.write('FAILED\n');
      failures.push(`${testCase.name}: ${e.message}`);
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
console.log(`\n3D preview smoke OK: ${CASES.length}/${CASES.length} cases rendered.`);
