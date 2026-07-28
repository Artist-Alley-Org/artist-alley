#!/usr/bin/env bash
# scripts/check-adr-frontmatter.sh
#
# Validates the YAML frontmatter of docs/adr/*.md.
#
# Why this gate exists: the ADR *source* lives here, but the thing
# that consumes it (`sync-adrs.mjs` in the separate, private
# artist-alley-site repo) lives somewhere else. A malformed ADR
# therefore doesn't break anything in THIS repo's CI — it breaks the
# site build, hours later, in a repo nobody was watching. That has
# happened twice:
#
#   - ADR 0071 landed with no frontmatter block at all
#   - ADR 0073 landed with `area: content`, which is not in the
#     enum the sync script accepts
#
# and ADR 0074 had to be hand-validated to avoid a third. This script
# catches both of those classes locally and in CI, before merge.
#
# What it checks (deliberately narrow — this is a lint, not a schema
# framework):
#
#   1. A frontmatter block exists, is delimited by `---`, and parses
#      as a YAML mapping.
#   2. Required keys are present: id, title, status, date, area,
#      excerpt (the required set from ADR 0035).
#   3. `id` is a *quoted* 4-digit string matching the filename
#      prefix, and is unique across the corpus.
#   4. `area` is one of the values the site's sync script accepts.
#   5. `status` is one of the documented lifecycle values.
#   6. Every id referenced by `related`, `supersedes` and
#      `superseded_by` resolves to an ADR file that exists.
#   7. The frontmatter stays inside the ADR 0035 dialect. The site
#      does not use a YAML library — it hand-parses flat keys, block
#      sequences and `>-` folded scalars. Valid YAML outside that
#      dialect (flow-style `["0008"]`, inline `#` comments on the
#      enum keys) parses fine here and means something else there.
#
# The rules mirror the site's `sync-adrs.mjs` as of site commit
# e5f9bad: same required-key set, same status enum, same area enum,
# same delimiter handling, same cross-reference errors. Where this
# script differs it is deliberately *stricter* (quoted ids, date
# shape, `superseded_by` references, real YAML parsing), so drift
# surfaces here as a cheap false red rather than there as a broken
# deploy.
#
# Usage:
#   ./scripts/check-adr-frontmatter.sh            # validate all ADRs
#   ./scripts/check-adr-frontmatter.sh a.md b.md  # validate a subset
#
# Note that the cross-reference check (6) always loads the whole
# docs/adr/ directory for its "does this id exist" lookup, even when
# only a subset is being validated.
#
# See ADR 0035 (ADR conventions and documentation pipeline).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

python3 - "$@" <<'PY'
import datetime
import glob
import os
import re
import sys

try:
    import yaml
except ImportError:  # pragma: no cover - environment problem, not a lint failure
    print(
        'check-adr-frontmatter: PyYAML is required (pip install pyyaml).',
        file=sys.stderr,
    )
    sys.exit(2)

ADR_GLOB = 'docs/adr/[0-9][0-9][0-9][0-9]-*.md'

# The frontmatter keys ADR 0035 declares required. `phases`, `tags`,
# `supersedes`, `related` and `superseded_by` are optional.
REQUIRED_KEYS = ('id', 'title', 'status', 'date', 'area', 'excerpt')

# Areas the site's sync script accepts. Nine of these are the taxonomy
# table in ADR 0035; `storage` is the tenth — it is not in that table
# but is in use by ADRs 0062 and 0071 and renders fine on the site.
# Adding a new area is intentionally a code change in two places (here
# and in the site's sync script), so a typo can't invent one.
AREAS = frozenset({
    'architecture',
    'extensibility',
    'infrastructure',
    'licensing',
    'monetization',
    'ops',
    'process',
    'security',
    'storage',
    'ux',
})

# The status lifecycle from ADR 0035. `rejected` is documented but not
# currently used by any ADR.
STATUSES = frozenset({
    'proposed',
    'accepted',
    'superseded',
    'deprecated',
    'rejected',
})

# Keys whose values are lists of ADR ids that must resolve to a file.
REFERENCE_KEYS = ('related', 'supersedes', 'superseded_by')

ID_RE = re.compile(r'^[0-9]{4}$')
DATE_RE = re.compile(r'^[0-9]{4}-[0-9]{2}-[0-9]{2}$')


def split_frontmatter(text):
    """Return (yaml_source, error). Exactly one of the two is None.

    Delimiter handling matches the site's script exactly: it requires a
    literal `---\\n` at byte 0 and finds the close with an indexOf for
    `\\n---\\n`, so a `---` with trailing whitespace or a leading blank
    line is *not* frontmatter over there either.
    """
    if not text.startswith('---\n'):
        return None, (
            'no YAML frontmatter block — the file must start with a `---` '
            'line (this is what broke ADR 0071)'
        )
    end = text.find('\n---\n', 4)
    if end < 0:
        return None, 'frontmatter block is never closed (missing the second `---`)'
    return text[4:end], None


def dialect_problems(raw):
    """Constructs that are valid YAML but that the site's parser misreads.

    The site does not use a YAML library. It hand-parses the closed
    ADR-frontmatter dialect from ADR 0035 (flat `key: value`, block
    sequences, `>-` folded scalars). Anything outside that dialect
    parses cleanly here and then silently means something else — or
    nothing — over there, which is the exact shape of the bug this
    gate exists to prevent. So the dialect is enforced, not just the
    YAML.
    """
    problems = []
    for line in raw.split('\n'):
        if not line.strip() or line.startswith((' ', '\t')):
            # Blank, a block-sequence item, or a folded-scalar
            # continuation — all handled by the site's parser.
            continue
        if line.lstrip().startswith('#'):
            continue
        m = re.match(r'^(\w+):\s*(.*)$', line)
        if not m:
            problems.append(
                'frontmatter line %r is outside the ADR 0035 dialect — the '
                "site's parser skips lines it can't match and then reports "
                'the key as missing' % line[:60]
            )
            continue
        key, value = m.group(1), m.group(2).strip()
        if value.startswith(('[', '{')) and value != '[]':
            problems.append(
                '`%s` uses a flow-style collection — the site\'s parser only '
                'reads block sequences (`- "0008"` on their own lines) and '
                '`[]` for empty' % key
            )
        # The site's parser does not strip inline `#` comments, so the
        # comment text ends up *inside* the value. Harmless in prose
        # keys, fatal on the three keys it compares exactly.
        if key in ('id', 'status', 'area') and re.search(r'\s#', value):
            problems.append(
                '`%s` has an inline `#` comment — the site\'s parser does not '
                'strip comments, so the value becomes %r and fails its enum '
                'check' % (key, value)
            )
    return problems


def id_list(value):
    """Normalise a reference field to a list of raw values."""
    if value is None:
        return []
    if isinstance(value, list):
        return value
    return [value]


def check(path, known_ids):
    """Return a list of human-readable problems with `path`."""
    problems = []
    with open(path, encoding='utf-8') as fh:
        text = fh.read()

    raw, err = split_frontmatter(text)
    if err:
        return [err]

    try:
        meta = yaml.safe_load(raw)
    except yaml.YAMLError as exc:
        detail = str(exc).replace('\n', ' ')
        return ['frontmatter is not parseable YAML: %s' % detail]

    if not isinstance(meta, dict):
        return ['frontmatter must be a YAML mapping, got %s' % type(meta).__name__]

    problems.extend(dialect_problems(raw))

    for key in REQUIRED_KEYS:
        if key not in meta or meta[key] is None or meta[key] == '':
            problems.append('missing required key `%s`' % key)

    # id: quoted 4-digit string matching the filename prefix. An
    # unquoted 0071 parses as the integer 71, so a non-str value here
    # means the quotes are missing.
    adr_id = meta.get('id')
    prefix = os.path.basename(path)[:4]
    if adr_id is not None:
        if not isinstance(adr_id, str):
            problems.append(
                '`id` must be a quoted 4-digit string (got %r — unquoted ids '
                'parse as integers and lose their zero padding)' % (adr_id,)
            )
        elif not ID_RE.match(adr_id):
            problems.append('`id` must be exactly 4 digits, got %r' % adr_id)
        elif adr_id != prefix:
            problems.append(
                '`id` %r does not match the filename prefix %r' % (adr_id, prefix)
            )

    area = meta.get('area')
    if area is not None and area not in AREAS:
        problems.append(
            '`area: %s` is not an accepted area — the site build rejects it. '
            'Accepted: %s' % (area, ', '.join(sorted(AREAS)))
        )

    status = meta.get('status')
    if status is not None and status not in STATUSES:
        problems.append(
            '`status: %s` is not an accepted status. Accepted: %s'
            % (status, ', '.join(sorted(STATUSES)))
        )

    date = meta.get('date')
    if date is not None and not isinstance(date, datetime.date):
        # An unquoted YYYY-MM-DD parses as a date; a quoted one stays a
        # string, which is fine as long as it's the right shape.
        if not (isinstance(date, str) and DATE_RE.match(date)):
            problems.append('`date` must be YYYY-MM-DD, got %r' % (date,))

    for key in REFERENCE_KEYS:
        if key not in meta:
            continue
        for ref in id_list(meta[key]):
            if not isinstance(ref, str) or not ID_RE.match(ref):
                problems.append(
                    '`%s` entry %r is not a quoted 4-digit ADR id' % (key, ref)
                )
            elif ref not in known_ids:
                problems.append(
                    '`%s` references ADR %s, which does not exist' % (key, ref)
                )
            elif ref == adr_id:
                problems.append('`%s` references this ADR itself' % key)

    return problems


all_adrs = sorted(glob.glob(ADR_GLOB))
if not all_adrs:
    print('check-adr-frontmatter: no ADRs found under docs/adr/.', file=sys.stderr)
    sys.exit(2)

# The id -> file lookup that backs the cross-reference check is always
# built from the whole corpus, even when validating a subset.
known_ids = {}
duplicates = []
for path in all_adrs:
    adr_id = os.path.basename(path)[:4]
    if adr_id in known_ids:
        duplicates.append((adr_id, known_ids[adr_id], path))
    known_ids[adr_id] = path

explicit = [p for p in sys.argv[1:] if p]
if explicit:
    targets = [p for p in explicit if os.path.basename(os.path.dirname(p)) == 'adr']
else:
    targets = all_adrs

failures = 0
for adr_id, first, second in duplicates:
    print('%s: duplicate ADR id %s (also %s)' % (second, adr_id, first), file=sys.stderr)
    failures += 1

for path in targets:
    if not os.path.exists(path):
        # Deleted in the PR being checked; nothing to validate.
        continue
    for problem in check(path, known_ids):
        print('%s: %s' % (path, problem), file=sys.stderr)
        failures += 1

if failures:
    print(
        '\ncheck-adr-frontmatter: %d problem(s) across %d ADR(s). These break the '
        'docs-site build in the artist-alley-site repo — see ADR 0035 for the '
        'frontmatter schema.' % (failures, len(targets)),
        file=sys.stderr,
    )
    sys.exit(1)

print('check-adr-frontmatter: %d ADR(s) OK.' % len(targets))
PY
