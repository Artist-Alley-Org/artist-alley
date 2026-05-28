// Copy /docs/adr/*.md into /site/src/content/docs/adr/ with Starlight-shaped
// frontmatter. Idempotent — safe to rerun. Output dir is gitignored.

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(HERE, "../../");
const SRC = path.join(REPO_ROOT, "docs", "adr");
const DST = path.resolve(HERE, "../src/content/docs/adr");

function frontmatterFor(filename, body) {
  const slug = filename.replace(/\.md$/, "");
  const h1 = body.match(/^#\s+(.+)$/m);
  const title = h1 ? h1[1].trim() : slug;
  // Skip if file already has frontmatter
  if (body.startsWith("---")) return null;
  return `---\ntitle: "${title.replace(/"/g, '\\"')}"\nslug: "adr/${slug}"\n---\n\n`;
}

async function main() {
  let entries;
  try {
    entries = await fs.readdir(SRC);
  } catch (e) {
    console.warn(`[sync-adrs] ${SRC} not found — skipping.`);
    return;
  }
  await fs.mkdir(DST, { recursive: true });

  let count = 0;
  for (const name of entries) {
    if (!name.endsWith(".md")) continue;
    const src = path.join(SRC, name);
    const dst = path.join(DST, name);
    const body = await fs.readFile(src, "utf8");
    const fm = frontmatterFor(name, body);
    const out = fm ? fm + body : body;
    await fs.writeFile(dst, out, "utf8");
    count++;
  }
  console.log(`[sync-adrs] copied ${count} ADR(s) -> ${path.relative(REPO_ROOT, DST)}`);
}

main().catch((e) => {
  console.error("[sync-adrs] failed:", e);
  process.exit(1);
});
