#!/usr/bin/env python3
"""
Generate a Postgres baseline migration from RS's dbstruct/ directory.

ResourceSpace's CheckDBStruct reads `table_*.txt`, `index_*.txt`, and
`data_*.txt` files at runtime to create / update its schema. We're
forking RS and replacing PHP feature-by-feature; until each feature
moves to Go, the PHP side still needs its tables on a fresh DB.

This script converts the dbstruct definitions into a single
`CREATE TABLE IF NOT EXISTS` migration plus index and seed-data
INSERTs. It runs once; the output goes to
app/internal/db/migrations/00007_rs_baseline_tables.sql.

Usage:
    python3 scripts/gen-rs-baseline.py > app/internal/db/migrations/00007_rs_baseline_tables.sql

Format reminders (from MySQL SHOW COLUMNS / SHOW INDEX):
  table_X.txt:  name, type, nullable(YES|NO), key, default, extra
  index_X.txt:  table, non_unique, name, seq_in_index, column, collation, ...
  data_X.txt:   values in column order, one row per line
"""

import csv
import io
import os
import re
import sys
from pathlib import Path

DBSTRUCT = Path(__file__).resolve().parent.parent / "dbstruct"

# Tables artist-alley already creates in earlier migrations. The
# baseline should regenerate every RS-shaped table; only skip ones we
# own outright (none yet — all RS tables flow through this script).
SKIP_TABLES: set[str] = set()

# Column-type overrides. RS's dbstruct files were authored against
# MySQL, which silently coerces between varchar and integer. Postgres
# doesn't, so a column declared varchar but used as a numeric FK
# blows up on join. Each entry here forces a specific Postgres type
# regardless of what dbstruct says.
#
#   table -> column -> postgres_type
COLUMN_TYPE_OVERRIDES: dict[str, dict[str, str]] = {
    # usergroup.parent stores another usergroup ref; RS joins it
    # against usergroup.ref (BIGINT). dbstruct mistakenly declares
    # it varchar(50). See user_functions.php notification queries.
    "usergroup": {"parent": "BIGINT"},
}


def my_to_pg_type(mytype: str) -> str:
    t = mytype.strip().lower()
    # Strip "unsigned" qualifier and any trailing space
    unsigned = "unsigned" in t
    t = t.replace("unsigned", "").strip()

    # Match type(n) for parameterized types
    m = re.match(r"^([a-z]+)\s*(?:\(([^)]+)\))?", t)
    if not m:
        return "TEXT"
    base = m.group(1)
    arg = m.group(2)

    if base in ("varchar", "char"):
        return f"VARCHAR({arg or 255})"
    if base in ("text", "longtext", "mediumtext", "tinytext"):
        return "TEXT"
    if base in ("int", "integer", "mediumint"):
        return "BIGINT" if unsigned else "INTEGER"
    if base == "smallint":
        return "SMALLINT"
    if base == "tinyint":
        # tinyint(1) is conventionally boolean in MySQL, but RS treats
        # it as a small int. Keep as SMALLINT for compatibility.
        return "SMALLINT"
    if base == "bigint":
        return "BIGINT"
    if base == "float":
        return "REAL"
    if base in ("double", "decimal", "numeric"):
        # decimal(M,D) -> NUMERIC(M,D); fall back to NUMERIC if unparsed
        if base == "decimal" and arg:
            return f"NUMERIC({arg})"
        return "DOUBLE PRECISION"
    if base in ("date",):
        return "DATE"
    if base in ("datetime", "timestamp"):
        return "TIMESTAMPTZ"
    if base in ("time",):
        return "TIME"
    if base in ("blob", "longblob", "mediumblob", "tinyblob", "binary", "varbinary"):
        return "BYTEA"
    if base in ("json",):
        return "JSONB"
    if base in ("enum", "set"):
        # Best-effort; RS uses these rarely. Store as TEXT.
        return "TEXT"
    return "TEXT"


def quote_ident(name: str) -> str:
    """Quote a name if it's a Postgres reserved word or has weird chars."""
    name = name.strip()
    # Common SQL reserved words RS happens to use
    reserved = {"user", "group", "order", "table", "select", "from", "where",
                "value", "default", "type", "to", "as", "all", "by", "is",
                "primary", "key", "limit", "offset"}
    if name.lower() in reserved or not re.match(r"^[a-z_][a-z0-9_]*$", name, re.I):
        return f'"{name}"'
    return name


def parse_default(default: str, pgtype: str) -> str | None:
    """Translate a MySQL default value into a Postgres DEFAULT clause."""
    if default is None or default == "":
        return None
    d = default.strip()
    if d.upper() == "NULL":
        return None  # NULL default == "no default" for our purposes
    if d.upper() in ("CURRENT_TIMESTAMP", "NOW()"):
        return "DEFAULT NOW()"
    # Numeric literal
    if re.match(r"^-?\d+(\.\d+)?$", d):
        return f"DEFAULT {d}"
    # String literal
    safe = d.replace("'", "''")
    return f"DEFAULT '{safe}'"


def parse_columns(table_file: Path, table_name: str) -> list[tuple[str, str, bool, str | None, bool]]:
    """Return list of (column, pgtype, nullable, default_clause, is_pk)."""
    cols = []
    overrides = COLUMN_TYPE_OVERRIDES.get(table_name, {})
    with table_file.open() as f:
        reader = csv.reader(f)
        for row in reader:
            if not row or all(c.strip() == "" for c in row):
                continue
            # RS files sometimes pad; ensure at least 5 fields
            row = (row + [""] * 5)[:6]
            name, mytype, nullable, key, default, extra = (row + [""] * 6)[:6]
            if not name:
                continue
            pgtype = my_to_pg_type(mytype)
            null_ok = (nullable.strip().upper() == "YES")
            is_pk = (key.strip().upper() == "PRI")
            default_clause = parse_default(default, pgtype)

            # auto_increment -> use IDENTITY for the PK
            if "auto_increment" in extra.lower():
                pgtype = ("BIGINT" if "BIGINT" in pgtype.upper() else "BIGINT") + " GENERATED BY DEFAULT AS IDENTITY"
                # IDENTITY columns are NOT NULL implicitly; force null_ok False.
                null_ok = False

            # Apply per-column type overrides (see COLUMN_TYPE_OVERRIDES).
            if name in overrides:
                pgtype = overrides[name]

            cols.append((name, pgtype, null_ok, default_clause, is_pk))
    return cols


def parse_indexes(index_file: Path) -> dict[str, dict]:
    """Return {index_name: {unique: bool, columns: [col, ...]}}."""
    out = {}
    if not index_file.exists():
        return out
    with index_file.open() as f:
        for row in csv.reader(f):
            if not row or len(row) < 5:
                continue
            row = (row + [""] * 12)[:12]
            _table, non_unique, name, _seq, column, *_rest = row
            if not name:
                continue
            idx = out.setdefault(name.strip(), {"unique": non_unique.strip() == "0", "columns": []})
            idx["columns"].append(column.strip())
    return out


def render_table(table: str, cols: list, indexes: dict) -> list[str]:
    out = []
    out.append(f"CREATE TABLE IF NOT EXISTS {quote_ident(table)} (")
    lines = []
    pk_cols = [c[0] for c in cols if c[4]]
    for name, pgtype, null_ok, default_clause, _is_pk in cols:
        parts = [f"    {quote_ident(name)}", pgtype]
        if not null_ok:
            parts.append("NOT NULL")
        # MySQL silently treats NOT NULL string columns with no
        # explicit default as DEFAULT ''. Postgres doesn't — emit it
        # so RS's seed INSERTs and PHP-side INSERTs that omit the
        # column don't trip 23502.
        if (
            not null_ok
            and default_clause is None
            and ("TEXT" in pgtype.upper() or "VARCHAR" in pgtype.upper() or "CHAR" in pgtype.upper())
            and "IDENTITY" not in pgtype.upper()
        ):
            default_clause = "DEFAULT ''"
        if default_clause:
            parts.append(default_clause)
        lines.append(" ".join(parts))
    if pk_cols:
        lines.append(f"    PRIMARY KEY ({', '.join(quote_ident(c) for c in pk_cols)})")
    out.append(",\n".join(lines))
    out.append(");\n")

    # Indexes (skip the PRIMARY one — handled above)
    for ix_name, ix in indexes.items():
        if ix_name.upper() == "PRIMARY":
            continue
        unique = "UNIQUE " if ix["unique"] else ""
        cols_sql = ", ".join(quote_ident(c) for c in ix["columns"])
        out.append(f"CREATE {unique}INDEX IF NOT EXISTS {quote_ident(table + '_' + ix_name + '_idx')} ON {quote_ident(table)} ({cols_sql});")
    out.append("")
    return out


def render_seed(data_file: Path, table: str, cols: list) -> list[str]:
    if not data_file.exists():
        return []
    col_names = [c[0] for c in cols]
    col_nullable = [c[2] for c in cols]
    has_pk_identity = any("IDENTITY" in c[1] for c in cols)
    out = []
    with data_file.open() as f:
        for row in csv.reader(f):
            if not row or all(c.strip() == "" for c in row):
                continue
            row = (row + [""] * len(col_names))[:len(col_names)]
            values = []
            for i, v in enumerate(row):
                vv = v.strip()
                if vv == "" or vv.upper() == "NULL":
                    # Empty cell in NOT NULL column: use DEFAULT so we
                    # don't explicitly violate the constraint with NULL.
                    # (NOT NULL string columns get a DEFAULT '' from
                    # the CREATE TABLE side above.)
                    values.append("NULL" if col_nullable[i] else "DEFAULT")
                elif re.match(r"^-?\d+(\.\d+)?$", vv):
                    values.append(vv)
                else:
                    values.append("'" + vv.replace("'", "''") + "'")
            qcols = ", ".join(quote_ident(c) for c in col_names)
            qvals = ", ".join(values)
            # Idempotent seed: ON CONFLICT DO NOTHING on the primary
            # key. Falls back to plain INSERT if there's no PK.
            pk_cols = [c[0] for c in cols if c[4]]
            conflict = f" ON CONFLICT ({', '.join(quote_ident(c) for c in pk_cols)}) DO NOTHING" if pk_cols else ""
            out.append(f"INSERT INTO {quote_ident(table)} ({qcols}) VALUES ({qvals}){conflict};")
    if out:
        if has_pk_identity:
            # After inserting fixed-id seed rows, advance the IDENTITY
            # sequence so future INSERTs (with no explicit ref) don't
            # collide with the seeded refs.
            out.append(f"SELECT setval(pg_get_serial_sequence('{table}', 'ref'),")
            out.append(f"              COALESCE((SELECT MAX(ref) FROM {quote_ident(table)}), 1),")
            out.append("              true) WHERE pg_get_serial_sequence('" + table + "', 'ref') IS NOT NULL;")
        out.append("")
    return out


HEADER = """-- artist-alley migration 00007 — RS baseline schema.
--
-- Auto-generated from RS's dbstruct/ directory by
-- scripts/gen-rs-baseline.py. This file recreates the parts of
-- ResourceSpace's schema that PHP still depends on while artist-alley
-- ports features over to Go.
--
-- Strategy: every table in dbstruct/table_*.txt that we haven't
-- already baselined elsewhere gets a CREATE TABLE IF NOT EXISTS
-- here. Indexes from dbstruct/index_*.txt are recreated. Seed data
-- from dbstruct/data_*.txt is loaded with ON CONFLICT DO NOTHING so
-- the migration stays idempotent on re-runs.
--
-- Pruning: as Go takes over a feature, drop the corresponding
-- CREATE TABLE here in a follow-up migration. The DB is the contract
-- between PHP and Go; once Go owns a domain end-to-end, the legacy
-- RS-shaped table goes.
--
-- Do not edit by hand — re-run scripts/gen-rs-baseline.py instead.

-- +goose Up

"""

HARDCODED_SEEDS = """
-- Hand-picked seed data. Everything else from RS's data_*.txt is
-- skipped (see the comment in scripts/gen-rs-baseline.py). Add
-- entries here only when something on the Go side breaks without
-- the seeded row.

INSERT INTO resource_type (ref, name, icon, order_by) VALUES
    (1, 'Photo',    'image',     10),
    (2, 'Document', 'file-text', 20),
    (3, 'Video',    'video',     30),
    (4, 'Audio',    'music',     40)
ON CONFLICT (ref) DO NOTHING;

-- Advance the IDENTITY sequence past the seeded refs so future
-- inserts without explicit ref don't collide.
SELECT setval(
    pg_get_serial_sequence('resource_type', 'ref'),
    COALESCE((SELECT MAX(ref) FROM resource_type), 1),
    true
) WHERE pg_get_serial_sequence('resource_type', 'ref') IS NOT NULL;

-- Mark RS as already-upgraded to the current SYSTEM_UPGRADE_LEVEL
-- (defined in include/definitions.php; bump here when we bump there).
-- Without this row, upgrade/upgrade.php runs on every fresh-install
-- page hit, sets process_lock_upgrade_in_progress, and attempts to
-- run MySQL-flavoured upgrade scripts that we haven't ported to
-- Postgres. Pretending the upgrade is done lets RS PHP render
-- normally during the strangler-fig phase; the scripts that change
-- *behaviour* (vs just schema) get re-evaluated as features port to Go.
DELETE FROM sysvars WHERE name = 'upgrade_system_level';
INSERT INTO sysvars (name, value) VALUES ('upgrade_system_level', '29');
"""

FOOTER = """
-- +goose Down

-- Down migrations on a baseline of 50+ tables aren't useful — they
-- would just drop the schema. Roll back the goose version manually
-- if you really need to.
SELECT 1;
"""


def main() -> int:
    if not DBSTRUCT.is_dir():
        print(f"error: dbstruct directory not found at {DBSTRUCT}", file=sys.stderr)
        return 1

    table_files = sorted(DBSTRUCT.glob("table_*.txt"))

    out = [HEADER]
    for tf in table_files:
        name = tf.stem[len("table_"):]
        if name in SKIP_TABLES:
            continue
        cols = parse_columns(tf, name)
        if not cols:
            continue
        indexes = parse_indexes(DBSTRUCT / f"index_{name}.txt")
        out.extend(render_table(name, cols, indexes))
        # NOTE: data_*.txt seed loading is intentionally disabled.
        # RS's seed CSVs use embedded newlines and tricky quoting that
        # csv.reader handles inconsistently across rows; the ones we
        # actually need (e.g. resource_type's four canonical types)
        # are hand-written in HARDCODED_SEEDS below so we control them.
        # If a feature ever turns out to need an RS-shipped seed, lift
        # it into HARDCODED_SEEDS by hand.

    out.append(HARDCODED_SEEDS)
    out.append(FOOTER)
    sys.stdout.write("\n".join(out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
