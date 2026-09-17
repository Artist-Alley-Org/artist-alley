// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom
//
// Builds a real, minimal EPUB in-process, for #1417's spec.
//
// ## Why it is BUILT and not a checked-in file
//
// The same reason glb-fixture.ts gives: storage is content-addressed and
// the dogfood database persists between runs, so a file on disk uploads
// once and deduplicates forever after. Every call embeds a nonce in the
// package identifier, so each run produces a genuinely new object that
// the ebook worker has to process from scratch.
//
// ## Why not a seeded epub
//
// The suite runs against more than one corpus (the coding stack's deep
// seed and CI's shallow one), and a spec that reaches for "some epub
// that is already there" is a spec that skips wherever there is none.
// Building the member here is what makes the case run everywhere.
//
// ## The container
//
// An EPUB is a ZIP: `mimetype` first and STORED (no compression), then
// META-INF/container.xml pointing at the package document, then the
// package document (OPF) and one XHTML chapter. Every entry is stored,
// which keeps this a few dozen lines and needs nothing beyond node's
// own crc32. preview/epub.go opens it with Go's archive/zip, reads
// container.xml, parses the OPF and marks the asset ready, cover or no
// cover; there is deliberately no cover, so the only raster on the post
// is the PNG the spec puts beside it.

import zlib from 'node:zlib';

interface Entry {
  name: string;
  data: Buffer;
}

function dosTime(): { time: number; date: number } {
  // A fixed timestamp; the nonce is what makes the bytes differ.
  return { time: (12 << 11) | (0 << 5) | 0, date: ((2026 - 1980) << 9) | (1 << 5) | 1 };
}

/** A ZIP with every entry STORED, written by hand. */
function storedZip(entries: Entry[]): Buffer {
  const { time, date } = dosTime();
  const locals: Buffer[] = [];
  const centrals: Buffer[] = [];
  let offset = 0;
  for (const e of entries) {
    const name = Buffer.from(e.name, 'utf8');
    const crc = zlib.crc32(e.data) >>> 0;
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4); // version needed
    local.writeUInt16LE(0, 6); // flags
    local.writeUInt16LE(0, 8); // method: stored
    local.writeUInt16LE(time, 10);
    local.writeUInt16LE(date, 12);
    local.writeUInt32LE(crc, 14);
    local.writeUInt32LE(e.data.length, 18);
    local.writeUInt32LE(e.data.length, 22);
    local.writeUInt16LE(name.length, 26);
    local.writeUInt16LE(0, 28);
    const central = Buffer.alloc(46);
    central.writeUInt32LE(0x02014b50, 0);
    central.writeUInt16LE(20, 4); // version made by
    central.writeUInt16LE(20, 6); // version needed
    central.writeUInt16LE(0, 8);
    central.writeUInt16LE(0, 10);
    central.writeUInt16LE(time, 12);
    central.writeUInt16LE(date, 14);
    central.writeUInt32LE(crc, 16);
    central.writeUInt32LE(e.data.length, 20);
    central.writeUInt32LE(e.data.length, 24);
    central.writeUInt16LE(name.length, 28);
    central.writeUInt16LE(0, 30); // extra
    central.writeUInt16LE(0, 32); // comment
    central.writeUInt16LE(0, 34); // disk
    central.writeUInt16LE(0, 36); // internal attrs
    central.writeUInt32LE(0, 38); // external attrs
    central.writeUInt32LE(offset, 42);
    locals.push(local, name, e.data);
    centrals.push(central, name);
    offset += local.length + name.length + e.data.length;
  }
  const centralStart = offset;
  const centralBytes = centrals.reduce((n, b) => n + b.length, 0);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(0, 4);
  end.writeUInt16LE(0, 6);
  end.writeUInt16LE(entries.length, 8);
  end.writeUInt16LE(entries.length, 10);
  end.writeUInt32LE(centralBytes, 12);
  end.writeUInt32LE(centralStart, 16);
  end.writeUInt16LE(0, 20);
  return Buffer.concat([...locals, ...centrals, end]);
}

export interface EpubFixture {
  bytes: Buffer;
  nonce: string;
}

/**
 * A valid EPUB 3 with one chapter and no cover. `title` is the Dublin
 * Core title in the package document; keep it free of kind words, the
 * same rule the asset's own title follows.
 */
export function buildMinimalEpub(title: string): EpubFixture {
  const nonce = `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  const xml = (s: string) => Buffer.from(s.trim() + '\n', 'utf8');
  const entries: Entry[] = [
    { name: 'mimetype', data: Buffer.from('application/epub+zip', 'ascii') },
    {
      name: 'META-INF/container.xml',
      data: xml(`
<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`),
    },
    {
      name: 'OEBPS/content.opf',
      data: xml(`
<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="uid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="uid">urn:uuid:aa-1417-${nonce}</dc:identifier>
    <dc:title>${title}</dc:title>
    <dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="ch1" href="chapter1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="ch1"/>
  </spine>
</package>`),
    },
    {
      name: 'OEBPS/chapter1.xhtml',
      data: xml(`
<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml">
  <head><title>${title}</title></head>
  <body><p>${title}</p></body>
</html>`),
    },
  ];
  return { bytes: storedZip(entries), nonce };
}
