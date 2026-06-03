// Parse app/schema.sql and emit:
//   1. site/src/content/docs/developers/database/index.mdx — landing with
//      domain-grouped table list + Mermaid ER overview.
//   2. site/src/content/docs/developers/database/<table>.mdx — one page per
//      table with description, Connected-to / Referenced-by panels, a
//      Column table (Name / Type / Notes), and the DDL block.
//
// All outputs are gitignored — schema.sql is the source of truth.

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(HERE, "../../");
const SRC = path.join(REPO_ROOT, "app", "schema.sql");
const DST_DIR = path.resolve(HERE, "../src/content/docs/developers/database");
const DATA_DIR = path.resolve(HERE, "../src/data");

// Legacy code hints scrubbed from public-docs comments. They're fine in
// app/schema.sql for devs but irrelevant to readers of the docs site.
const LEGACY_HINTS = [
  /resource\s*space/i,
  /\brs\b/i,
  /CheckDBStruct/,
  /dbstruct\//,
  /\bRS[- _]?side\b/i,
  /from\s+RS\b/i,
  /\bdbstruct\b/i,
];

const scrubLegacy = (text) =>
  text
    .split("\n")
    .filter((line) => !LEGACY_HINTS.some((re) => re.test(line)))
    .join("\n")
    .trim();

// ---------------------------------------------------------------------------
// Domain grouping — hardcoded because schema.sql has no native domain tag.
// Tables not listed here fall into "Other" so adding a table never silently
// vanishes from the landing.
// ---------------------------------------------------------------------------
const DOMAINS = [
  {
    id: "identity",
    title: "Identity & access",
    summary:
      "Users, sessions, capabilities, roles, API tokens, and the per-user grant / revoke overlay.",
    tables: [
      "user", "user_profiles", "sessions", "api_tokens",
      "capabilities", "roles", "role_capabilities",
      "user_roles", "user_capability_grants", "user_capability_revokes",
    ],
  },
  {
    id: "teams",
    title: "Teams",
    summary:
      "Team hierarchy stored as both adjacency parents and a closure table for fast reachability queries.",
    tables: ["teams", "team_parents", "team_closure", "team_memberships"],
  },
  {
    id: "assets",
    title: "Assets",
    summary:
      "The core asset row, asset-type lookup, tags, and the companion / alternate file relations.",
    tables: [
      "asset_types", "assets", "asset_tag",
      "asset_companions", "asset_alternates",
    ],
  },
  {
    id: "metadata",
    title: "Metadata",
    summary:
      "Admin-extensible field definitions, per-asset typed values, and the append-only history table.",
    tables: ["field_definition", "asset_field_value", "asset_field_value_history"],
  },
  {
    id: "collections",
    title: "Collections",
    summary:
      "Collections wrap assets and posts; per-collection ACLs cover team / role / user grants.",
    tables: ["collections", "collection_resources", "collection_posts", "collection_acls"],
  },
  {
    id: "posts",
    title: "Posts",
    summary:
      "Posts wrap one or many assets with shared metadata, tags, and per-post ACLs.",
    tables: ["posts", "post_assets", "post_tags", "post_acls"],
  },
  {
    id: "workflow",
    title: "Workflow",
    summary:
      "Per-asset / per-post state machine: states, allowed transitions, and the audit trail of state changes.",
    tables: ["workflow_states", "workflow_transitions", "workflow_audit"],
  },
  {
    id: "storage",
    title: "Storage",
    summary:
      "Content-addressed storage objects, derived variants (thumbs / previews / waveforms), and reference-count pins.",
    tables: ["storage_objects", "storage_variants", "storage_pins"],
  },
  {
    id: "social",
    title: "Social",
    summary: "Per-post comments thread and likes.",
    tables: ["comments", "likes"],
  },
  {
    id: "audit",
    title: "Audit",
    summary:
      "Project-wide event ledger consumed by the audit log surface.",
    tables: ["audit_events"],
  },
  {
    id: "jobs",
    title: "Jobs",
    summary: "Async work queue for previews, ingest, federation, and scheduled actions.",
    tables: ["jobs"],
  },
  {
    id: "annotations",
    title: "Annotations",
    summary: "Brush packs and stamp library powering the whiteboard / review annotation tools.",
    tables: ["brush_packs", "brush_pack_stamps"],
  },
  {
    id: "system",
    title: "System",
    summary: "Server-wide configuration.",
    tables: ["system_config"],
  },
];

// ---------------------------------------------------------------------------
// SQL parsing
// ---------------------------------------------------------------------------

function parseTables(sql) {
  const lines = sql.split(/\r?\n/);
  const tables = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];
    const m = line.match(/^\s*CREATE\s+TABLE\s+("?)([\w$]+)\1\s*\(/i);
    if (!m) { i++; continue; }
    const name = m[2];

    // Gather leading -- comment block.
    const descLines = [];
    let j = i - 1;
    while (j >= 0 && /^\s*--/.test(lines[j])) {
      descLines.unshift(lines[j].replace(/^\s*--\s?/, ""));
      j--;
    }
    const description = scrubLegacy(descLines.join("\n"));

    // Collect body lines until `);`.
    const bodyLines = [line];
    i++;
    while (i < lines.length) {
      bodyLines.push(lines[i]);
      if (/\)\s*;\s*$/.test(lines[i])) { i++; break; }
      i++;
    }

    const body = bodyLines.join("\n");
    const columns = parseColumns(body);
    const tableConstraints = parseTableConstraints(body);

    tables.push({ name, description, body, columns, tableConstraints });
  }
  return tables;
}

// Crude but adequate column parser. Walks lines between the opening `(` and
// closing `);`, splits each on the first sequence of whitespace into
// (name, rest), then teases constraints out of `rest`. Skips table-level
// constraint lines (PRIMARY KEY (a,b), UNIQUE (...), CHECK (...)).
function parseColumns(body) {
  const innerStart = body.indexOf("(");
  const innerEnd = body.lastIndexOf(")");
  const inner = body.slice(innerStart + 1, innerEnd);

  const cols = [];
  let buf = "";
  let depth = 0;
  for (const ch of inner) {
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (ch === "," && depth === 0) {
      cols.push(buf);
      buf = "";
      continue;
    }
    buf += ch;
  }
  if (buf.trim()) cols.push(buf);

  const out = [];
  for (const raw of cols) {
    const clean = raw.replace(/\s+/g, " ").trim();
    if (!clean) continue;
    // Skip table-level constraints — parsed separately.
    if (/^(PRIMARY\s+KEY|UNIQUE|CHECK|FOREIGN\s+KEY|CONSTRAINT)\b/i.test(clean)) {
      continue;
    }

    // Pull a trailing `-- comment` if present.
    let inline = "";
    const inlineIdx = clean.indexOf("--");
    let body = clean;
    if (inlineIdx >= 0) {
      inline = scrubLegacy(clean.slice(inlineIdx + 2).trim());
      body = clean.slice(0, inlineIdx).trim();
    }

    // First token is the column name (handles quoted names too).
    const nameMatch = body.match(/^("[^"]+"|\w+)\s+(.+)$/);
    if (!nameMatch) continue;
    const colName = nameMatch[1].replace(/^"|"$/g, "");
    const rest = nameMatch[2];

    // Heuristic type extraction: type is everything up to the first
    // constraint keyword (NOT, NULL, PRIMARY, REFERENCES, DEFAULT, CHECK,
    // UNIQUE, GENERATED, COLLATE).
    const constraintRe =
      /\b(NOT\s+NULL|NULL|PRIMARY\s+KEY|REFERENCES|DEFAULT|CHECK|UNIQUE|GENERATED|COLLATE)\b/i;
    const cidx = rest.search(constraintRe);
    const type = (cidx < 0 ? rest : rest.slice(0, cidx)).trim();
    const constraintTail = cidx < 0 ? "" : rest.slice(cidx).trim();

    const isPrimaryKey = /\bPRIMARY\s+KEY\b/i.test(constraintTail);
    const isUnique = /\bUNIQUE\b/i.test(constraintTail);
    const isNotNull = /\bNOT\s+NULL\b/i.test(constraintTail);
    const isExplicitNull = /(?<!NOT\s)\bNULL\b(?!\s+DEFAULT)/i.test(constraintTail);
    const defaultMatch = constraintTail.match(
      /\bDEFAULT\s+(.+?)(?=\b(?:NOT\s+NULL|NULL|REFERENCES|CHECK|UNIQUE|GENERATED|COLLATE|PRIMARY\s+KEY)\b|$)/i,
    );
    const def = defaultMatch ? defaultMatch[1].trim() : "";
    const referencesMatch = constraintTail.match(
      /\bREFERENCES\s+("?)(\w+)\1\s*(?:\(\s*("?)(\w+)\3\s*\))?/i,
    );
    const fkTable = referencesMatch ? referencesMatch[2] : "";
    const fkColumn = referencesMatch ? (referencesMatch[4] || "") : "";

    const generated = /\bGENERATED\b/i.test(constraintTail);

    out.push({
      name: colName,
      type: cleanType(type, generated),
      nullable: !isNotNull && !isPrimaryKey,
      hasDefault: Boolean(def),
      default: def,
      isPrimaryKey,
      isUnique,
      isExplicitNull,
      fkTable,
      fkColumn,
      comment: inline,
    });
  }
  return out;
}

function parseTableConstraints(body) {
  const innerStart = body.indexOf("(");
  const innerEnd = body.lastIndexOf(")");
  const inner = body.slice(innerStart + 1, innerEnd);

  // Same splitter; only collect lines that ARE table-level constraints.
  const cols = [];
  let buf = "";
  let depth = 0;
  for (const ch of inner) {
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (ch === "," && depth === 0) {
      cols.push(buf);
      buf = "";
      continue;
    }
    buf += ch;
  }
  if (buf.trim()) cols.push(buf);

  const constraints = [];
  for (const raw of cols) {
    const clean = raw.replace(/\s+/g, " ").trim();
    if (!/^(PRIMARY\s+KEY|UNIQUE|CHECK|FOREIGN\s+KEY|CONSTRAINT)\b/i.test(clean)) {
      continue;
    }
    constraints.push(clean);
  }
  return constraints;
}

// Tidy DDL into the kind of compact, readable type strings RS uses.
function cleanType(raw, generated) {
  let t = raw.toLowerCase().trim();
  t = t.replace(/\s+/g, " ");
  if (generated && /bigint|integer/.test(t)) return "bigserial";
  if (t === "double precision") return "double";
  if (t === "timestamp with time zone" || t === "timestamptz") return "timestamp";
  if (t === "timestamp without time zone") return "timestamp";
  if (t === "character varying") return "varchar";
  if (t.startsWith("character varying")) return t.replace("character varying", "varchar");
  if (t === "smallint") return "smallint";
  if (t === "integer") return "integer";
  if (t === "bigint") return "bigint";
  if (t === "real") return "real";
  if (t === "boolean") return "boolean";
  if (t === "text") return "text";
  if (t === "uuid") return "uuid";
  if (t === "jsonb") return "jsonb";
  if (t === "bytea") return "bytea";
  return t;
}

// ---------------------------------------------------------------------------
// Cross-table FK index — drives the "Referenced by" panel.
// ---------------------------------------------------------------------------
function buildBackrefs(tables) {
  const backrefs = {};
  for (const t of tables) backrefs[t.name] = [];
  for (const t of tables) {
    for (const c of t.columns) {
      if (!c.fkTable) continue;
      if (!backrefs[c.fkTable]) backrefs[c.fkTable] = [];
      backrefs[c.fkTable].push({
        sourceTable: t.name,
        sourceColumn: c.name,
        targetColumn: c.fkColumn || "ref",
      });
    }
  }
  return backrefs;
}

// ---------------------------------------------------------------------------
// MDX rendering
// ---------------------------------------------------------------------------

const PUBLIC_SUMMARY = (table) => {
  // First sentence of the table description, defaulting to a stock string.
  const desc = (table.description || "").trim();
  if (!desc) return "";
  const m = desc.match(/^([^.]+\.)/);
  return (m ? m[1] : desc).trim();
};

function tableSlug(name) {
  // Quoted reserved words (only "user" today) lose the quotes for the URL.
  return name.replace(/[^a-z0-9_]+/gi, "").toLowerCase();
}

function noteFor(col) {
  const parts = [];
  if (col.isPrimaryKey) parts.push("Primary key");
  if (col.fkTable) {
    parts.push(
      `References [\`${col.fkTable}\`](./${tableSlug(col.fkTable)}/)` +
        (col.fkColumn ? `(\`${col.fkColumn}\`)` : ""),
    );
  }
  if (col.isUnique && !col.isPrimaryKey) parts.push("Unique");
  if (col.hasDefault) parts.push(`Default \`${col.default}\``);
  if (col.comment) parts.push(col.comment);
  return parts.join("; ");
}

function nullabilityFor(col) {
  if (col.isPrimaryKey) return "no";
  return col.nullable ? "yes" : "no";
}

function renderLanding(tables, backrefs, builtAt) {
  // Group tables by domain (anything not assigned lands in "Other").
  const assigned = new Set();
  const grouped = DOMAINS.map((d) => {
    const present = d.tables
      .map((tn) => tables.find((t) => t.name === tn))
      .filter(Boolean);
    present.forEach((t) => assigned.add(t.name));
    return { ...d, tables: present };
  });
  const orphans = tables.filter((t) => !assigned.has(t.name));
  if (orphans.length > 0) {
    grouped.push({
      id: "other",
      title: "Other",
      summary: "Tables not yet sorted into a domain group.",
      tables: orphans,
    });
  }

  const mermaid = renderMermaid(tables);

  const lines = [
    "---",
    "title: Database",
    "description: Schema overview for Artist Alley — every table grouped by domain, with per-table detail pages.",
    "sidebar:",
    "  order: 4",
    "  label: Database",
    "tableOfContents:",
    "  minHeadingLevel: 2",
    "  maxHeadingLevel: 3",
    "---",
    "",
    "import { Aside } from \"@astrojs/starlight/components\";",
    "",
    `<Aside type="note" title="Generated reference">`,
    `Regenerated from [\`app/schema.sql\`](https://github.com/mscrnt/artist-alley/blob/main/app/schema.sql) on every site build. Edit the SQL file, not these MDX pages. Last built ${builtAt}.`,
    `</Aside>`,
    "",
    `Artist Alley's persistent state lives in **Postgres** behind [\`sqlc\`](https://sqlc.dev/) — the schema in \`app/schema.sql\` is the Go-side declaration the query compiler validates against. ${tables.length} tables total, grouped below by what they own.`,
    "",
    "## Table relationships",
    "",
    "Foreign-key edges between every table in the catalogue. Click into a table for the column-level detail.",
    "",
    "```mermaid",
    mermaid,
    "```",
    "",
    "## Tables by domain",
    "",
  ];

  for (const g of grouped) {
    if (g.tables.length === 0) continue;
    lines.push(`### ${g.title}`);
    lines.push("");
    lines.push(g.summary);
    lines.push("");
    lines.push("| Table | Summary |");
    lines.push("|---|---|");
    for (const t of g.tables) {
      const summary = PUBLIC_SUMMARY(t) || "_No description._";
      const escaped = summary.replace(/\|/g, "\\|");
      lines.push(`| [\`${t.name}\`](./${tableSlug(t.name)}/) | ${escaped} |`);
    }
    lines.push("");
  }

  lines.push("## Reading the per-table pages");
  lines.push("");
  lines.push(
    "Each table page lists every column with its type, nullability, and notes — including any foreign-key target and the column's default value. A **Connected to** panel summarises which other tables this one points at; a **Referenced by** panel lists every table that points back. The full DDL is at the bottom so you can read the exact constraints.",
  );
  lines.push("");
  lines.push(
    "Schema changes land as goose migrations under `app/internal/db/migrations/`; `app/schema.sql` is updated to mirror them so the next site build picks up the change.",
  );
  lines.push("");

  return lines.join("\n");
}

function renderMermaid(tables) {
  // ER-style mermaid diagram. Each node is a table; each edge is a FK.
  // Keep it compact — no per-column listing inside the node, just the name.
  const lines = ["erDiagram"];
  const edges = new Set();

  for (const t of tables) {
    lines.push(`  ${t.name} {`);
    // Show only the PK + a representative column so the diagram stays
    // readable. Listing every column makes the box unreadable on large
    // tables.
    const pk = t.columns.find((c) => c.isPrimaryKey);
    if (pk) lines.push(`    ${pk.type.replace(/[^\w]/g, "_")} ${pk.name} PK`);
    lines.push(`  }`);
  }

  for (const t of tables) {
    for (const c of t.columns) {
      if (!c.fkTable) continue;
      // Skip self-references in the diagram — they clutter without adding info.
      if (c.fkTable === t.name) continue;
      const key = `${c.fkTable}->${t.name}:${c.name}`;
      if (edges.has(key)) continue;
      edges.add(key);
      const cardinality = c.nullable ? `}o--|| ` : `}|--|| `;
      lines.push(`  ${t.name} ${cardinality}${c.fkTable} : ${c.name}`);
    }
  }

  return lines.join("\n");
}

function renderTablePage(table, backrefs, builtAt) {
  const desc = (table.description || "").trim();
  const summary = PUBLIC_SUMMARY(table) ||
    `Schema reference for the \`${table.name}\` table.`;

  const fkOutbound = table.columns.filter((c) => c.fkTable);
  const fkInbound = backrefs[table.name] || [];

  const lines = [
    "---",
    `title: 'Table: ${table.name}'`,
    `description: ${JSON.stringify(summary.slice(0, 160))}`,
    "tableOfContents: false",
    "sidebar:",
    `  label: ${table.name}`,
    "---",
    "",
    "import { Aside } from \"@astrojs/starlight/components\";",
    "",
  ];

  if (desc) {
    lines.push(desc);
    lines.push("");
  }

  // Connected-to panel
  if (fkOutbound.length > 0 || fkInbound.length > 0) {
    lines.push("## Connected to");
    lines.push("");
    if (fkOutbound.length > 0) {
      lines.push("**This table references:**");
      lines.push("");
      for (const c of fkOutbound) {
        lines.push(
          `- [\`${c.fkTable}\`](../${tableSlug(c.fkTable)}/)` +
            (c.fkColumn ? `(\`${c.fkColumn}\`)` : "") +
            ` via column \`${c.name}\``,
        );
      }
      lines.push("");
    }
    if (fkInbound.length > 0) {
      lines.push("**Referenced by:**");
      lines.push("");
      // Group by source table for readability.
      const grouped = {};
      for (const b of fkInbound) {
        if (!grouped[b.sourceTable]) grouped[b.sourceTable] = [];
        grouped[b.sourceTable].push(b);
      }
      const sources = Object.keys(grouped).sort();
      for (const src of sources) {
        const cols = grouped[src].map((b) => `\`${b.sourceColumn}\``).join(", ");
        lines.push(`- [\`${src}\`](../${tableSlug(src)}/) — column${grouped[src].length > 1 ? "s" : ""} ${cols}`);
      }
      lines.push("");
    }
  }

  // Columns table
  if (table.columns.length > 0) {
    lines.push("## Columns");
    lines.push("");
    lines.push("| Column | Type | Nullable | Notes |");
    lines.push("|---|---|---|---|");
    for (const c of table.columns) {
      const note = noteFor(c) || "—";
      const escaped = note.replace(/\|/g, "\\|");
      lines.push(`| \`${c.name}\` | \`${c.type}\` | ${nullabilityFor(c)} | ${escaped} |`);
    }
    lines.push("");
  }

  // Table-level constraints
  if (table.tableConstraints.length > 0) {
    lines.push("## Constraints");
    lines.push("");
    for (const tc of table.tableConstraints) {
      lines.push(`- \`${tc}\``);
    }
    lines.push("");
  }

  // Raw DDL
  lines.push("## Full DDL");
  lines.push("");
  lines.push("```sql");
  lines.push(table.body.trim());
  lines.push("```");
  lines.push("");

  lines.push("---");
  lines.push("");
  lines.push(
    `Please see the [schema overview](../) for context. ` +
      `This document was last regenerated on ${builtAt}.`,
  );
  lines.push("");

  return lines.join("\n");
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main() {
  let sql;
  try {
    sql = await fs.readFile(SRC, "utf8");
  } catch {
    console.warn(`[gen-schema] ${SRC} not found — writing placeholder.`);
    await fs.mkdir(DST_DIR, { recursive: true });
    await fs.writeFile(
      path.join(DST_DIR, "index.mdx"),
      `---\ntitle: Database\ndescription: app/schema.sql was missing at build time.\n---\n\n_app/schema.sql was not found. Rebuild after the file lands._\n`,
      "utf8",
    );
    return;
  }

  const tables = parseTables(sql);
  const backrefs = buildBackrefs(tables);
  const builtAt = new Date().toISOString();

  await fs.mkdir(DST_DIR, { recursive: true });
  await fs.mkdir(DATA_DIR, { recursive: true });

  // Wipe any previous generator output so a removed table really disappears.
  const existing = await fs.readdir(DST_DIR).catch(() => []);
  for (const name of existing) {
    if (/\.mdx$/.test(name)) {
      await fs.rm(path.join(DST_DIR, name), { force: true });
    }
  }

  // Emit the machine-readable schema for the interactive visualizer.
  await fs.writeFile(
    path.join(DATA_DIR, "schema.json"),
    JSON.stringify(buildSchemaData(tables, backrefs, builtAt), null, 2),
    "utf8",
  );

  await fs.writeFile(
    path.join(DST_DIR, "index.mdx"),
    renderLanding(tables, backrefs, builtAt),
    "utf8",
  );

  await fs.writeFile(
    path.join(DST_DIR, "visualizer.mdx"),
    renderVisualizerPage(tables, builtAt),
    "utf8",
  );

  for (const t of tables) {
    await fs.writeFile(
      path.join(DST_DIR, `${tableSlug(t.name)}.mdx`),
      renderTablePage(t, backrefs, builtAt),
      "utf8",
    );
  }

  console.log(
    `[gen-schema] wrote ${tables.length + 2} pages + schema.json → ${path.relative(REPO_ROOT, DST_DIR)}`,
  );
}

// Structured schema data for the interactive visualizer. Keep it lean —
// only the fields the Cytoscape graph + tooltip need.
function buildSchemaData(tables, backrefs, builtAt) {
  // Build table → domain lookup so the graph can colour-code by domain.
  const tableDomain = {};
  for (const d of DOMAINS) {
    for (const tn of d.tables) tableDomain[tn] = d.id;
  }

  const nodes = tables.map((t) => ({
    id: t.name,
    label: t.name,
    domain: tableDomain[t.name] || "other",
    slug: tableSlug(t.name),
    summary: PUBLIC_SUMMARY(t) || "",
    columnCount: t.columns.length,
    fkOutCount: t.columns.filter((c) => c.fkTable).length,
    fkInCount: (backrefs[t.name] || []).length,
  }));

  const edges = [];
  for (const t of tables) {
    for (const c of t.columns) {
      if (!c.fkTable) continue;
      edges.push({
        source: t.name,
        target: c.fkTable,
        column: c.name,
        nullable: c.nullable,
      });
    }
  }

  const domains = DOMAINS.map((d) => ({
    id: d.id,
    title: d.title,
    summary: d.summary,
  }));
  // Synthetic "other" domain for any table not assigned.
  if (nodes.some((n) => n.domain === "other")) {
    domains.push({
      id: "other",
      title: "Other",
      summary: "Tables not yet sorted into a domain group.",
    });
  }

  return { generatedAt: builtAt, domains, nodes, edges };
}

// Hand-written visualizer page template emitted by the generator so the
// page lives in the same gitignored directory as the rest of the schema
// output. The component itself (DBVisualizer.astro) does the real work.
function renderVisualizerPage(tables, builtAt) {
  return [
    "---",
    "title: Schema visualizer",
    "description: Interactive Cytoscape map of every Artist Alley table and its foreign-key relationships. Pan, zoom, filter by domain, click a node to open its detail page.",
    "tableOfContents: false",
    "sidebar:",
    "  order: 1",
    "  label: Schema visualizer",
    "---",
    "",
    "import { Aside } from \"@astrojs/starlight/components\";",
    `import DBVisualizer from "../../../../components/DBVisualizer.astro";`,
    "",
    `<Aside type="note" title="Live schema">`,
    `Regenerated from [\`app/schema.sql\`](https://github.com/mscrnt/artist-alley/blob/main/app/schema.sql) on every site build (${tables.length} tables). Click a node to open its detail page; drag nodes to rearrange; use the domain checkboxes to focus a slice of the graph.`,
    `</Aside>`,
    "",
    "<DBVisualizer />",
    "",
    "## How to read this",
    "",
    "- **Nodes** are tables; node colour is the domain (Identity, Assets, Metadata, etc.). Larger nodes have more columns or more foreign-key edges.",
    "- **Edges** are foreign-key references. The edge points from the table that holds the foreign key to the table it references — e.g. `posts → assets` means a `posts.*` column references `assets.id`. Dashed edges are nullable references.",
    "- **Click** a node to open its table-detail page. **Drag** nodes to rearrange the layout (positions reset on reload). **Scroll** to zoom; **drag-empty-space** to pan.",
    "- The **domain checkboxes** filter the graph to a subset; the **search box** highlights matching tables.",
    "",
    "For the print-friendly summary, see the [schema overview](../).",
    "",
  ].join("\n");
}

main().catch((e) => {
  console.error("[gen-schema] failed:", e);
  process.exit(1);
});
