// Copy /app/api/openapi.yaml -> /site/public/openapi.yaml so starlight-openapi
// can read it during build. Public so it's also available as a downloadable
// artifact at /openapi.yaml on the live site.

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(HERE, "../../");
const SRC = path.join(REPO_ROOT, "app", "api", "openapi.yaml");
const DST = path.resolve(HERE, "../public/openapi.yaml");

async function main() {
  try {
    await fs.access(SRC);
  } catch {
    console.warn(`[sync-openapi] ${SRC} not found — writing a placeholder.`);
    await fs.mkdir(path.dirname(DST), { recursive: true });
    await fs.writeFile(
      DST,
      "openapi: 3.0.3\ninfo:\n  title: artist-alley API (placeholder)\n  version: 0.0.0\npaths: {}\n",
      "utf8",
    );
    return;
  }
  await fs.mkdir(path.dirname(DST), { recursive: true });
  await fs.copyFile(SRC, DST);
  console.log(`[sync-openapi] copied ${path.relative(REPO_ROOT, SRC)} -> ${path.relative(REPO_ROOT, DST)}`);
}

main().catch((e) => {
  console.error("[sync-openapi] failed:", e);
  process.exit(1);
});
