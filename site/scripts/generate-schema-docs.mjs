// Parse /app/schema.sql and emit a single MDX page documenting each table.
//
// Output: /site/src/content/docs/reference/schema.mdx (gitignored).
//
// The parser is intentionally simple: it expects `CREATE TABLE <name> ( ... );`
// blocks, optionally preceded by `-- comment` lines that get rendered as the
// table's description. Per-column comments and constraints come through
// verbatim as code blocks. Good enough for an evolving schema; revisit if
// schema.sql ever uses fancier DDL.

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(HERE, "../../");
const SRC = path.join(REPO_ROOT, "app", "schema.sql");
const DST = path.resolve(HERE, "../src/content/docs/reference/schema.mdx");

// Comment lines mentioning legacy code (ResourceSpace, the dbstruct emitter,
// the PHP-side schema, etc.) are stripped from descriptions before they
// reach the public docs site. They're still in app/schema.sql for devs.
const LEGACY_HINTS = [
  /resource\s*space/i,
  /\brs\b/,
  /CheckDBStruct/,
  /dbstruct\//,
  /\bRS[- _]?side\b/i,
  /from\s+RS\b/i,
];

function scrubLegacyLines(commentLines) {
  return commentLines.filter((l) => !LEGACY_HINTS.some((re) => re.test(l)));
}

function parseTables(sql) {
  // Strip line comments above each CREATE TABLE for the description.
  const lines = sql.split(/\r?\n/);
  const tables = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const m = line.match(/^\s*CREATE\s+TABLE\s+("?)([\w$]+)\1\s*\(/i);
    if (!m) {
      i++;
      continue;
    }
    const name = m[2];

    // gather leading comment block (consecutive `--` lines directly above)
    const commentLines = [];
    let j = i - 1;
    while (j >= 0 && /^\s*--/.test(lines[j])) {
      commentLines.unshift(lines[j].replace(/^\s*--\s?/, ""));
      j--;
    }
    const description = scrubLegacyLines(commentLines).join("\n").trim();

    // gather the body until the closing `);`
    const bodyLines = [line];
    i++;
    while (i < lines.length) {
      bodyLines.push(lines[i]);
      if (/\)\s*;\s*$/.test(lines[i])) {
        i++;
        break;
      }
      i++;
    }
    tables.push({ name, description, body: bodyLines.join("\n") });
  }
  return tables;
}

function mdxFor(tables, builtAt) {
  const head = `---
title: Database schema
description: Generated reference for every table in app/schema.sql. Source of truth, not editable here.
sidebar:
  order: 2
---

import { Aside } from "@astrojs/starlight/components";

<Aside type="note" title="Generated content">
This page is regenerated from
[\`app/schema.sql\`](https://github.com/mscrnt/artist-alley/blob/main/app/schema.sql)
on every site build. Edit the SQL file, not this MDX. Last built ${builtAt}.
</Aside>

## Tables (${tables.length})

`;

  // github-slugger's rules: lowercase, spaces → `-`, drop backticks/punct,
  // preserve underscores. For our heading shape ("table-<name>") this gives
  // a predictable anchor we can link to from the TOC.
  const anchor = (name) => `table-${name.toLowerCase()}`;

  const toc = tables
    .map((t) => `- [\`${t.name}\`](#${anchor(t.name)})`)
    .join("\n");

  const bodies = tables
    .map((t) => {
      // Heading text "Table: name" → slugged anchor "table-name". MDX won't
      // accept inline `{#...}` syntax (treats `{` as a JSX expression), so
      // we rely on the auto-slugger and keep the heading text plain.
      const heading = `\n## Table: ${t.name}\n`;
      const desc = t.description
        ? `\n${t.description}\n`
        : "\n_No description supplied in schema.sql._\n";
      const ddl = "\n```sql\n" + t.body.trim() + "\n```\n";
      return heading + desc + ddl;
    })
    .join("\n");

  return head + toc + "\n\n" + bodies + "\n";
}

async function main() {
  let sql;
  try {
    sql = await fs.readFile(SRC, "utf8");
  } catch {
    console.warn(`[gen-schema] ${SRC} not found — writing a placeholder page.`);
    await fs.mkdir(path.dirname(DST), { recursive: true });
    await fs.writeFile(
      DST,
      `---\ntitle: Database schema\ndescription: Placeholder — app/schema.sql was not found at build time.\n---\n\n_app/schema.sql was missing when this site built. Re-run after the file lands._\n`,
      "utf8",
    );
    return;
  }
  const tables = parseTables(sql);
  const builtAt = new Date().toISOString();
  await fs.mkdir(path.dirname(DST), { recursive: true });
  await fs.writeFile(DST, mdxFor(tables, builtAt), "utf8");
  console.log(`[gen-schema] wrote ${path.relative(REPO_ROOT, DST)} (${tables.length} table(s))`);
}

main().catch((e) => {
  console.error("[gen-schema] failed:", e);
  process.exit(1);
});
