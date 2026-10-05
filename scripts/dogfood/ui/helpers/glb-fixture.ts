// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// Builds a real GLB container in-process, for #754's specs.
//
// ## ⚠️ Why this is BUILT and not a checked-in file
//
// Storage is CONTENT-ADDRESSED and the dogfood stack's database
// PERSISTS between runs. A fixture file on disk uploads once and
// deduplicates forever after: the second run's "upload" resolves to the
// hash already stored, the render job logs `wrote:0`, and a spec that
// asserts "the missing textures are named" passes VACUOUSLY on
// whatever the first run left behind — or on nothing at all.
//
// So every call embeds a NONCE in the JSON chunk. Different bytes,
// different sha256, a genuinely new object every time. The nonce sits
// in a field glTF ignores (`extras`), so it changes the hash without
// changing what the parser sees.
//
// ## The container
//
// glTF 2.0 binary: a 12-byte header (magic `glTF`, version 2, total
// length) followed by chunks, each a 4-byte length + 4-byte type +
// payload padded to a 4-byte boundary. Chunk 0 is the JSON document;
// chunk 1 is the binary buffer. That is exactly what
// `format3d.ReadGLBJSONChunk` walks, so a container built here is read
// by the same code path a real export takes.

const GLB_MAGIC = 0x46546c67; // 'glTF'
const CHUNK_JSON = 0x4e4f534a; // 'JSON'
const CHUNK_BIN = 0x004e4942; // 'BIN\0'

function padTo4(buf: Uint8Array, fill: number): Uint8Array {
  const rem = buf.length % 4;
  if (rem === 0) return buf;
  const out = new Uint8Array(buf.length + (4 - rem));
  out.set(buf);
  out.fill(fill, buf.length);
  return out;
}

export interface GlbFixture {
  bytes: Buffer;
  /** The external paths the document declares, in document order. */
  declared: string[];
  nonce: string;
}

/**
 * A GLB that DECLARES external resources it does not embed.
 *
 * 363 of the 374 GLBs in the seed catalogue do exactly this (#750) — a
 * GLB is not automatically self-contained, an exporter chooses. This
 * builds the common case so the "what does this model still need?"
 * answer has something real to report.
 */
export function buildGlbWithExternalTextures(paths: string[] = []): GlbFixture {
  const nonce = `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  const declared = paths.length > 0 ? paths : [`Textures/planks-${nonce}.png`];

  const doc = {
    asset: { version: '2.0', generator: 'artist-alley dogfood fixture' },
    // The nonce lives in `extras`, which glTF defines as
    // application-specific and every parser ignores. It changes the
    // bytes (and so the content hash) without changing the reference
    // list under test.
    extras: { dogfoodNonce: nonce },
    // A buffer with NO uri is the embedded BIN chunk and needs no
    // companion — so a spec asserting "these three are missing" cannot
    // pass by accidentally counting the geometry.
    buffers: [{ byteLength: 4 }],
    images: declared.map((uri) => ({ uri })),
  };

  const jsonChunk = padTo4(new TextEncoder().encode(JSON.stringify(doc)), 0x20); // pad with spaces
  const binChunk = padTo4(new Uint8Array([1, 2, 3, 4]), 0x00);

  const total = 12 + 8 + jsonChunk.length + 8 + binChunk.length;
  const out = Buffer.alloc(total);
  let o = 0;
  out.writeUInt32LE(GLB_MAGIC, o); o += 4;
  out.writeUInt32LE(2, o); o += 4;
  out.writeUInt32LE(total, o); o += 4;
  out.writeUInt32LE(jsonChunk.length, o); o += 4;
  out.writeUInt32LE(CHUNK_JSON, o); o += 4;
  Buffer.from(jsonChunk).copy(out, o); o += jsonChunk.length;
  out.writeUInt32LE(binChunk.length, o); o += 4;
  out.writeUInt32LE(CHUNK_BIN, o); o += 4;
  Buffer.from(binChunk).copy(out, o);

  return { bytes: out, declared, nonce };
}
