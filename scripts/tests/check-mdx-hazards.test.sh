#!/usr/bin/env bash
# scripts/tests/check-mdx-hazards.test.sh
#
# Regression coverage for scripts/check-mdx-hazards.sh.
#
# Why this exists: the scanner reported "0 hazards" on the exact ADR
# 0093 prose that then killed artist-alley-site's `Build verify` run
# 33286001571 with
#
#   [@mdx-js/rollup] Unexpected character `=` (U+003D) before name
#   .../adr/0093-browse-and-search-compose-one-query.mdx:465:31
#
# Fixing the ADR alone would have made a BROKEN scanner pass again,
# so the important assertion here is not "the corpus is clean". It
# is that the scanner itself can tell the site-breaking prose apart
# from the safe prose. Test 1 pins the fail-before: it applies the
# scanner's HISTORICAL `<[a-zA-Z]` rule to the fixture and asserts
# it finds nothing, which is precisely how the break shipped.
#
# Run: bash scripts/tests/check-mdx-hazards.test.sh

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

SCANNER="scripts/check-mdx-hazards.sh"
FIX="scripts/tests/fixtures/mdx-hazards"

pass=0
fail=0

ok()   { pass=$((pass + 1)); printf '  ok   %s\n' "$1"; }
bad()  { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; [ $# -gt 1 ] && printf '       %s\n' "$2"; }

# Run the scanner over a fixture, bypassing the synced-path filter.
# Prints stderr (where hazards are reported); returns the exit code.
scan() { bash "$SCANNER" --unfiltered "$1" 2>&1; }

# ── 1. Fail-before: the historical rule misses the site-breaking text
#
# This is the whole point of the correction. `<[a-zA-Z]` cannot match
# `<=` or `< A`, so the pre-correction scanner passed the prose that
# broke the site. Asserting zero old-rule matches keeps that fact
# from quietly reverting.
echo '1. fail-before: the historical `<[a-zA-Z]` rule misses `A <= B`'
old_hits="$(python3 - "$FIX/unsafe-comparisons.md" <<'PY'
import re, sys
content = open(sys.argv[1]).read()
content = re.sub(r'```.*?```', lambda m: '\n' * m.group(0).count('\n'), content, flags=re.DOTALL)
content = re.sub(r'`[^`]+`', lambda m: ' ' * len(m.group(0)), content, flags=re.DOTALL)
content = re.sub(r'<https?://[^>\s]+>', '', content)
# The exact pre-correction pattern.
hits = [l for l in content.split('\n') if re.search(r'<[a-zA-Z]', l)]
# The two lines whose failure the site actually reported.
site = [l for l in hits if 'A <= B' in l or 'A <= size <= B' in l]
print(len(site))
PY
)"
if [ "$old_hits" = "0" ]; then
    ok "old rule reports 0 hazards on \`A <= B\` / \`A <= size <= B\` (this is the bug)"
else
    bad "old rule unexpectedly matched the comparison lines ($old_hits)" \
        "the fail-before premise no longer holds; re-derive it"
fi

# ── 2. The corrected scanner rejects the unsafe fixture
echo '2. corrected scanner rejects MDX-invalid prose'
out="$(scan "$FIX/unsafe-comparisons.md")"; rc=$?
if [ "$rc" -ne 0 ]; then
    ok "exits non-zero ($rc)"
else
    bad "exited 0 on the unsafe fixture" "$out"
fi

# Each of these must be named in the report. Every one of them was
# compiled through @mdx-js/mdx 3.1.1 and genuinely fails.
while IFS= read -r needle; do
    [ -z "$needle" ] && continue
    if printf '%s' "$out" | grep -qF -- "$needle"; then
        ok "reports: $needle"
    else
        bad "did not report: $needle" "$out"
    fi
done <<'NEEDLES'
populations against bounds A <= B
| X | A <= size <= B | both |
I <3 that idea
when 1<2 holds
value <- assigned
value <_name here
<!-- an HTML comment is not MDX -->
mount the <AssetCard> component
NEEDLES

# Brace behaviour must be preserved exactly.
if printf '%s' "$out" | grep -qF -- '{-prefix'; then
    ok "preserves the {[a-zA-Z_] brace hazard"
else
    bad "brace hazard no longer reported" "$out"
fi

# ── 3. The corrected scanner accepts everything MDX accepts
echo '3. corrected scanner accepts MDX-valid prose'
out="$(scan "$FIX/safe-comparisons.md")"; rc=$?
if [ "$rc" -eq 0 ]; then
    ok "exits 0 on inline code, fenced blocks, \`< \` prose, \\< and &lt;"
else
    bad "flagged safe constructs" "$out"
fi

# ── 4. The autolink allowance is unchanged
echo '4. autolink allowance preserved'
out="$(scan "$FIX/autolink-allowed.md")"; rc=$?
if [ "$rc" -eq 0 ]; then
    ok "exits 0 on <http(s)://…> autolinks"
else
    bad "autolink behaviour changed" "$out"
fi

# ── 5. The real corpus is clean
echo '5. synced corpus scans clean'
out="$(bash "$SCANNER" 2>&1)"; rc=$?
if [ "$rc" -eq 0 ]; then
    ok "$(printf '%s' "$out" | tail -1)"
else
    bad "synced docs contain hazards" "$out"
fi

echo
echo "check-mdx-hazards.test.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
