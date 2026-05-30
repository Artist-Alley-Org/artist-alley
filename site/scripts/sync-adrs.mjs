// Sync docs/adr/*.md → site/src/content/docs/adr/*.mdx with full ADR
// pipeline behaviour per ADR 0035:
//
// 1. Parse + validate YAML frontmatter against the convention schema.
// 2. Validate cross-references: phase IDs must exist in roadmap.json;
//    `related:` and `supersedes:` ADR IDs must point to real ADRs.
// 3. Derive inverse maps:
//      - `superseded_by[X]` — the ADR that supersedes X (from
//        somewhere's `supersedes:`).
//      - `back_links[X]` — ADRs that name X in their `related:`.
//      - `adrs_by_phase[1.42]` — ADRs that name 1.42 in their `phases:`.
// 4. Emit augmented MDX for each ADR — Starlight `title` +
//    `description` frontmatter, followed by an <AdrCard> component
//    that surfaces every metadata field, followed by the body.
// 5. Write `site/src/data/adrs.json` + `phases.json` for the index +
//    roadmap to consume.
// 6. Print orphan warnings and exit non-zero on any validation error.

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(HERE, "../../");
const SRC = path.join(REPO_ROOT, "docs", "adr");
const DST = path.resolve(HERE, "../src/content/docs/adr");
const DATA_DIR = path.resolve(HERE, "../src/data");
const ROADMAP_PATH = path.resolve(
  HERE,
  "../src/content/roadmap/roadmap.json",
);

const VALID_STATUSES = [
  "proposed", "accepted", "superseded", "deprecated", "rejected",
];
const VALID_AREAS = [
  "architecture", "security", "licensing", "monetization", "process",
  "ux", "ops", "infrastructure", "extensibility",
];

// ---------------------------------------------------------------------------
// Frontmatter extraction + minimal YAML parser
// ---------------------------------------------------------------------------

function splitFrontmatter(raw) {
  if (!raw.startsWith("---\n")) return null;
  const end = raw.indexOf("\n---\n", 4);
  if (end < 0) return null;
  return { fmText: raw.slice(4, end), body: raw.slice(end + 5) };
}

// Parses the ADR-frontmatter dialect: flat keys, scalars, string arrays,
// and a `>-` folded-scalar form for `excerpt`. Avoids pulling in a YAML
// dep — the dialect is closed and well-defined per ADR 0035.
function parseYamlSubset(text) {
  const lines = text.split("\n");
  const result = {};
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim() || line.trim().startsWith("#")) { i++; continue; }
    const m = line.match(/^(\w+):\s*(.*)$/);
    if (!m) { i++; continue; }
    const key = m[1];
    const rawValue = m[2].trim();

    if (rawValue === ">-" || rawValue === ">" || rawValue === "|") {
      const collected = [];
      i++;
      while (
        i < lines.length &&
        (lines[i].startsWith("  ") || lines[i].startsWith("\t") || lines[i].trim() === "")
      ) {
        if (lines[i].trim()) collected.push(lines[i].trim());
        i++;
      }
      result[key] = collected.join(" ").trim();
      continue;
    }

    if (rawValue === "" || rawValue === "[]") {
      const arr = [];
      i++;
      while (i < lines.length && lines[i].match(/^\s+-\s+/)) {
        let item = lines[i].replace(/^\s+-\s+/, "").trim();
        if (
          (item.startsWith('"') && item.endsWith('"')) ||
          (item.startsWith("'") && item.endsWith("'"))
        ) {
          item = item.slice(1, -1);
        }
        arr.push(item);
        i++;
      }
      result[key] = rawValue === "[]" && arr.length === 0 ? [] : arr;
      continue;
    }

    let value = rawValue;
    if (
      (value.startsWith('"') && value.endsWith('"')) ||
      (value.startsWith("'") && value.endsWith("'"))
    ) {
      value = value.slice(1, -1);
    }
    result[key] = value;
    i++;
  }
  return result;
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main() {
  const errors = [];
  const warnings = [];

  let entries;
  try {
    entries = await fs.readdir(SRC);
  } catch {
    console.warn(`[sync-adrs] ${SRC} not found — skipping.`);
    return;
  }

  // Roadmap → valid phase IDs (so we can validate `phases:` cross-refs).
  const roadmap = JSON.parse(await fs.readFile(ROADMAP_PATH, "utf8"));
  const validPhases = new Set();
  for (const section of roadmap.sections) {
    if (section.kind === "items") {
      for (const item of section.items) {
        if (item.phase) validPhases.add(item.phase);
      }
    } else if (section.kind === "subphases") {
      for (const sub of section.subphases) {
        if (sub.id) validPhases.add(sub.id);
      }
    }
  }

  // First pass — read + parse each ADR.
  const adrs = [];
  for (const name of entries) {
    if (!name.match(/^\d{4}-.+\.md$/)) continue;
    const idFromFile = name.match(/^(\d{4})/)[1];
    const raw = await fs.readFile(path.join(SRC, name), "utf8");
    const split = splitFrontmatter(raw);
    if (!split) {
      errors.push(`${name}: missing YAML frontmatter (open + close with '---')`);
      continue;
    }
    const fm = parseYamlSubset(split.fmText);
    if (!fm.id) fm.id = idFromFile;
    fm.phases = fm.phases || [];
    fm.related = fm.related || [];
    fm.supersedes = fm.supersedes || [];
    fm.tags = fm.tags || [];

    for (const req of ["id", "title", "status", "date", "area", "excerpt"]) {
      if (!fm[req]) errors.push(`${name}: missing required field '${req}'`);
    }
    if (fm.status && !VALID_STATUSES.includes(fm.status)) {
      errors.push(
        `${name}: status '${fm.status}' not in [${VALID_STATUSES.join(", ")}]`,
      );
    }
    if (fm.area && !VALID_AREAS.includes(fm.area)) {
      errors.push(
        `${name}: area '${fm.area}' not in [${VALID_AREAS.join(", ")}]`,
      );
    }
    if (fm.id !== idFromFile) {
      errors.push(
        `${name}: frontmatter id '${fm.id}' does not match filename '${idFromFile}'`,
      );
    }

    adrs.push({ name, fm, body: split.body });
  }

  // Build the ADR ID universe.
  const knownAdrIds = new Set(adrs.map((a) => a.fm.id));

  // Compute inverse maps.
  const supersededBy = {};
  const backLinks = {};
  const adrsByPhase = {};

  for (const a of adrs) {
    backLinks[a.fm.id] = new Set();
    for (const phase of a.fm.phases) {
      if (!validPhases.has(phase)) {
        warnings.push(`${a.name}: phase '${phase}' not in roadmap.json (orphan reference)`);
      }
      if (!adrsByPhase[phase]) adrsByPhase[phase] = new Set();
      adrsByPhase[phase].add(a.fm.id);
    }
  }

  for (const a of adrs) {
    for (const sup of a.fm.supersedes) {
      if (!knownAdrIds.has(sup)) {
        errors.push(`${a.name}: supersedes unknown ADR ${sup}`);
        continue;
      }
      supersededBy[sup] = a.fm.id;
    }
    for (const r of a.fm.related) {
      if (!knownAdrIds.has(r)) {
        errors.push(`${a.name}: related references unknown ADR ${r}`);
        continue;
      }
      if (backLinks[r]) backLinks[r].add(a.fm.id);
    }
  }

  if (errors.length) {
    for (const e of errors) console.error(`[sync-adrs] ERROR: ${e}`);
    throw new Error(`${errors.length} ADR validation error(s) — see above`);
  }

  // Orphan warning: accepted ADRs with no inbound links + no phase refs.
  for (const a of adrs) {
    if (a.fm.status !== "accepted") continue;
    if ([...backLinks[a.fm.id]].length === 0 && a.fm.phases.length === 0) {
      warnings.push(
        `${a.name}: no inbound 'related' links and no 'phases:' references — may be an orphan`,
      );
    }
  }

  for (const w of warnings) console.warn(`[sync-adrs] WARN: ${w}`);

  // Ensure dirs exist; selectively wipe stale ADR MDX so an ADR removed
  // upstream doesn't linger in the docs collection. Hand-authored files
  // in the directory (notably index.mdx — the catalogue landing page)
  // are preserved.
  await fs.mkdir(DST, { recursive: true });
  await fs.mkdir(DATA_DIR, { recursive: true });
  const existing = await fs.readdir(DST).catch(() => []);
  for (const name of existing) {
    // Wipe both the generated .mdx and any stale .md left over from
    // pre-pipeline copies, so an ADR removed upstream really disappears.
    if (/^\d{4}-.+\.(mdx|md)$/.test(name)) {
      await fs.rm(path.join(DST, name), { force: true });
    }
  }

  // Emit data files for AdrIndex + roadmap.
  const adrsJson = adrs
    .map((a) => ({
      id: a.fm.id,
      title: a.fm.title,
      slug: a.name.replace(/\.md$/, ""),
      status: a.fm.status,
      date: a.fm.date,
      area: a.fm.area,
      phases: a.fm.phases,
      supersedes: a.fm.supersedes,
      superseded_by: supersededBy[a.fm.id] || null,
      related: a.fm.related,
      back_links: [...backLinks[a.fm.id]].sort(),
      tags: a.fm.tags,
      excerpt: a.fm.excerpt,
    }))
    .sort((a, b) => a.id.localeCompare(b.id));

  await fs.writeFile(
    path.join(DATA_DIR, "adrs.json"),
    JSON.stringify({ generatedAt: new Date().toISOString(), adrs: adrsJson }, null, 2),
  );

  const phasesJson = {};
  for (const [phase, set] of Object.entries(adrsByPhase)) {
    phasesJson[phase] = [...set].sort();
  }
  await fs.writeFile(
    path.join(DATA_DIR, "phases.json"),
    JSON.stringify(phasesJson, null, 2),
  );

  // Emit each ADR's MDX into the docs collection.
  let copied = 0;
  for (const a of adrs) {
    const dstName = a.name.replace(/\.md$/, ".mdx");
    const dst = path.join(DST, dstName);
    const supBy = supersededBy[a.fm.id] || null;
    const bl = [...backLinks[a.fm.id]].sort();

    const header = [
      "---",
      `title: ${yamlScalar(a.fm.title)}`,
      `description: ${yamlScalar(a.fm.excerpt)}`,
      `slug: adr/${a.name.replace(/\.md$/, "")}`,
      `tableOfContents:`,
      `  minHeadingLevel: 2`,
      `  maxHeadingLevel: 3`,
      "---",
      "",
      `import AdrCard from "../../../components/AdrCard.astro";`,
      "",
      `<AdrCard`,
      `  id="${a.fm.id}"`,
      `  status="${a.fm.status}"`,
      `  date="${a.fm.date}"`,
      `  area="${a.fm.area}"`,
      `  phases={${JSON.stringify(a.fm.phases)}}`,
      `  supersedes={${JSON.stringify(a.fm.supersedes)}}`,
      `  supersededBy={${JSON.stringify(supBy)}}`,
      `  related={${JSON.stringify(a.fm.related)}}`,
      `  backLinks={${JSON.stringify(bl)}}`,
      `  tags={${JSON.stringify(a.fm.tags)}}`,
      `/>`,
      "",
    ].join("\n");

    await fs.writeFile(dst, header + a.body.replace(/^\n+/, ""));
    copied++;
  }

  console.log(
    `[sync-adrs] processed ${copied} ADR(s) → ${path.relative(REPO_ROOT, DST)} ` +
      `(${warnings.length} warning(s), data → ${path.relative(REPO_ROOT, DATA_DIR)})`,
  );
}

// YAML scalar escaper: single-quote when the value contains chars that
// upset the YAML parser; otherwise emit bare.
function yamlScalar(s) {
  if (s == null) return "''";
  const dangerous = /[:#&*?{}|<>=!%@`"'\\\[\]]/;
  if (dangerous.test(s) || s.startsWith(" ") || s.endsWith(" ")) {
    return `'${s.replace(/'/g, "''")}'`;
  }
  return s;
}

main().catch((e) => {
  console.error("[sync-adrs] failed:", e.message);
  process.exit(1);
});
