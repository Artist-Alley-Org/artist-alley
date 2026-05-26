#!/usr/bin/env python3
"""
Generate the RS data-seed migration.

scripts/gen-rs-baseline.py builds the schema; this script builds the
data RS expects on a fresh install. Split out so the schema migration
stays clean (no large blobs of seed data) and so we can iterate on
which tables get seeded without touching the schema baseline.

Tables seeded (the minimum set for RS's PHP UI to render against a
fresh DB):

  resource_type_field
  resource_type_field_resource_type
  tab
  usergroup
  preview_size

The seeded `resource_type_field` rows are also used to ALTER TABLE
resource ADD COLUMN fieldN — RS expects a per-field denormalised
text column on `resource` for hardcoded queries like
`r.field{$view_title_field}` in collections_functions / do_search.

Usage:
    python3 scripts/gen-rs-seeds.py > app/internal/db/migrations/00013_rs_field_seeds.sql
"""

import csv
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DBSTRUCT = ROOT / "dbstruct"

# Tables to seed, in apply order. Order matters: parent tables before
# child tables when there are FKs (there aren't between these five,
# but conceptually resource_type_field comes before _resource_type).
SEED_TABLES = [
    "resource_type_field",
    "resource_type_field_resource_type",
    "tab",
    "usergroup",
    "preview_size",
]

# Match quote_ident in gen-rs-baseline.py — same reserved-word set.
RESERVED = {
    "user", "group", "order", "table", "select", "from", "where",
    "value", "default", "type", "to", "as", "all", "by", "is",
    "primary", "key", "limit", "offset",
}


def quote_ident(name: str) -> str:
    name = name.strip()
    if name.lower() in RESERVED or not re.match(r"^[a-z_][a-z0-9_]*$", name, re.I):
        return f'"{name}"'
    return name


def column_names(table: str) -> list[str]:
    """Read the column names (in declaration order) from table_X.txt."""
    p = DBSTRUCT / f"table_{table}.txt"
    cols = []
    with p.open() as f:
        for row in csv.reader(f):
            if not row or not row[0].strip():
                continue
            cols.append(row[0].strip())
    return cols


def column_nullable(table: str) -> dict[str, bool]:
    """Return {col: True if nullable}."""
    p = DBSTRUCT / f"table_{table}.txt"
    out = {}
    with p.open() as f:
        for row in csv.reader(f):
            if not row or not row[0].strip():
                continue
            row = (row + [""] * 6)[:6]
            out[row[0].strip()] = (row[2].strip().upper() == "YES")
    return out


# MySQL types that map to numeric Postgres columns. We render the
# implicit-default value (0 / '' / FALSE) on empty cells so NOT NULL
# constraints don't trip — that matches MySQL's permissive seed-load
# behaviour under sql_mode="" (which RS shipped with).
_NUMERIC_PREFIXES = ("int", "smallint", "tinyint", "bigint", "mediumint", "float", "double", "decimal", "numeric")


def column_kind(table: str) -> dict[str, str]:
    """Return {col: 'numeric' | 'string' | 'other'} from the MySQL type."""
    p = DBSTRUCT / f"table_{table}.txt"
    out = {}
    with p.open() as f:
        for row in csv.reader(f):
            if not row or not row[0].strip():
                continue
            row = (row + [""] * 6)[:6]
            name = row[0].strip()
            mytype = row[1].strip().lower()
            if any(mytype.startswith(p) for p in _NUMERIC_PREFIXES):
                out[name] = "numeric"
            elif mytype.startswith(("varchar", "char", "text", "longtext", "mediumtext", "tinytext")):
                out[name] = "string"
            else:
                out[name] = "other"
    return out


def primary_key_cols(table: str) -> list[str]:
    """Return PK column names in order — supports composite keys."""
    p = DBSTRUCT / f"table_{table}.txt"
    pk = []
    with p.open() as f:
        for row in csv.reader(f):
            if not row or not row[0].strip():
                continue
            row = (row + [""] * 6)[:6]
            if row[3].strip().upper() == "PRI":
                pk.append(row[0].strip())
    return pk


def has_identity(table: str) -> bool:
    """True if the table has an auto_increment column (=> IDENTITY in PG)."""
    p = DBSTRUCT / f"table_{table}.txt"
    with p.open() as f:
        for row in csv.reader(f):
            if not row or not row[0].strip():
                continue
            row = (row + [""] * 6)[:6]
            if "auto_increment" in row[5].lower():
                return True
    return False


def render_value(v: str, nullable: bool, kind: str) -> str:
    v = v.strip()
    if v == "" or v.upper() == "NULL":
        if nullable:
            return "NULL"
        # NOT NULL with empty cell: emit the type-appropriate implicit
        # default that MySQL would have silently substituted.
        if kind == "numeric":
            return "0"
        if kind == "string":
            return "''"
        return "DEFAULT"
    if re.match(r"^-?\d+(\.\d+)?$", v):
        return v
    return "'" + v.replace("'", "''") + "'"


def render_inserts(table: str) -> list[str]:
    data_file = DBSTRUCT / f"data_{table}.txt"
    if not data_file.exists():
        return []
    cols = column_names(table)
    nullable = column_nullable(table)
    kinds = column_kind(table)
    pks = primary_key_cols(table)
    qcols = ", ".join(quote_ident(c) for c in cols)
    conflict_cols = ", ".join(quote_ident(c) for c in pks) if pks else ""

    rows = []
    with data_file.open() as f:
        for row in csv.reader(f):
            if not row or all(c.strip() == "" for c in row):
                continue
            rows.append(row)

    out = [f"-- {table} ({len(rows)} rows from dbstruct/data_{table}.txt)"]
    for row in rows:
        row = (row + [""] * len(cols))[:len(cols)]
        values = [
            render_value(v, nullable.get(cols[i], True), kinds.get(cols[i], "other"))
            for i, v in enumerate(row)
        ]
        on_conflict = f" ON CONFLICT ({conflict_cols}) DO NOTHING" if conflict_cols else ""
        out.append(
            f"INSERT INTO {quote_ident(table)} ({qcols}) VALUES "
            f"({', '.join(values)}){on_conflict};"
        )

    # Advance the IDENTITY sequence past the seeded refs — only for
    # tables with an auto_increment PK (composite-key join tables
    # don't have one).
    if has_identity(table) and pks:
        seq_col = pks[0]
        out.append(f"SELECT setval(pg_get_serial_sequence('{table}', '{seq_col}'),")
        out.append(f"              COALESCE((SELECT MAX({quote_ident(seq_col)}) FROM {quote_ident(table)}), 1),")
        out.append(f"              true) WHERE pg_get_serial_sequence('{table}', '{seq_col}') IS NOT NULL;")
    out.append("")
    return out


def field_refs() -> list[int]:
    """Pull the `ref` column from data_resource_type_field.txt."""
    p = DBSTRUCT / "data_resource_type_field.txt"
    refs = []
    with p.open() as f:
        for row in csv.reader(f):
            if not row or not row[0].strip():
                continue
            try:
                refs.append(int(row[0]))
            except ValueError:
                pass
    return refs


def render_field_columns(refs: list[int]) -> list[str]:
    out = [
        "-- RS denormalises a TEXT column onto `resource` per metadata field.",
        "-- Stock RS adds these via the admin-field-create flow; we add them",
        "-- up front so hardcoded references like r.field{view_title_field}",
        "-- in do_search.php and collections_functions.php resolve.",
    ]
    for r in sorted(refs):
        out.append(f'ALTER TABLE resource ADD COLUMN IF NOT EXISTS "field{r}" TEXT;')
    out.append("")
    return out


HEADER = """-- artist-alley migration 00013 — RS data seeds.
--
-- Auto-generated from RS's dbstruct/data_*.txt by
-- scripts/gen-rs-seeds.py. Sister to 00007_rs_baseline_tables.sql
-- (which defines the empty tables); this file fills in the rows RS
-- expects to find on a fresh install so the PHP UI can render
-- against our Postgres DB during the strangler-fig phase.
--
-- ON CONFLICT DO NOTHING keeps the seeds idempotent: applying this
-- migration on a DB that already has user-added rows leaves them
-- alone. The IDENTITY sequences are bumped past the seeded refs so
-- new admin-side INSERTs don't collide.
--
-- Do not edit by hand — re-run scripts/gen-rs-seeds.py instead.

-- +goose Up

"""

FOOTER = """
-- +goose Down

-- Seed removal is a destructive operation that would break the PHP
-- layer until the data is re-seeded. Drop the goose version manually
-- if you really need to roll this back.
SELECT 1;
"""


def main() -> int:
    if not DBSTRUCT.is_dir():
        print(f"error: dbstruct directory not found at {DBSTRUCT}", file=sys.stderr)
        return 1

    out = [HEADER]

    refs = field_refs()
    out.extend(render_field_columns(refs))

    for table in SEED_TABLES:
        out.extend(render_inserts(table))

    out.append(FOOTER)
    sys.stdout.write("\n".join(out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
