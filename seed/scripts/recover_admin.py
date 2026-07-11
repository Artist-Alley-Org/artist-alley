#!/usr/bin/env python3
"""
recover_admin.py — extract the bootstrap admin password from boot logs.

When AA boots in production mode (without AA_BOOTSTRAP_DEFAULT_ADMIN=1),
the bootstrap package generates a random 32-character password and
writes it to:

  1. The boot log (visible via `docker logs <container>`)
  2. <AdminPath>/bootstrap-admin.txt (mode 0600 — preferred; survives
     log rotation)

This script reads either source + prints the recovered password to
stdout so it can be piped into apply.py:

    pw=$(python3 recover_admin.py --logs docker-logs.txt) ; \\
    python3 apply.py --admin-pass-env PW ...

Or directly:

    python3 recover_admin.py --admin-file /var/lib/artist-alley/bootstrap-admin.txt

Usage
-----
    python3 recover_admin.py [--logs FILE | --logs-cmd CMD | --admin-file PATH]

If both --logs and --admin-file are provided, --admin-file wins (it's
the canonical source). Default tries --admin-file at the standard
container path, falling back to log scraping if available.
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from pathlib import Path

# Match the bootstrap.go boot log line. Two patterns:
#   1. dev mode banner: "password: ArtistAlleyMogul"
#   2. prod mode log:   "bootstrap admin password: <32-chars>"
# Both have known leading whitespace tolerance.
PATTERNS = [
    re.compile(r"bootstrap admin password:\s*(\S+)", re.IGNORECASE),
    re.compile(r"password:\s*(\S{8,})\s*$", re.IGNORECASE),
    re.compile(r"WARN:\s+.*password.*?:\s*(\S+)", re.IGNORECASE),
]


def extract_from_text(text: str) -> str | None:
    for pat in PATTERNS:
        for line in text.splitlines():
            m = pat.search(line)
            if m:
                pw = m.group(1).strip()
                # Filter obvious non-password tokens
                if pw and pw.lower() not in ("none", "null", "<redacted>"):
                    return pw
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--logs", type=Path,
                        help="Log file containing boot output")
    parser.add_argument("--logs-cmd",
                        help="Command whose stdout/stderr is searched "
                             "(e.g. 'docker logs aa-app')")
    parser.add_argument("--admin-file", type=Path,
                        default=Path("/var/lib/artist-alley/bootstrap-admin.txt"),
                        help="Canonical bootstrap-admin.txt path "
                             "(default: /var/lib/artist-alley/bootstrap-admin.txt)")
    parser.add_argument("--quiet", action="store_true",
                        help="Suppress source-found message on stderr")
    args = parser.parse_args()

    # Priority 1: the canonical admin-file
    if args.admin_file and args.admin_file.is_file():
        content = args.admin_file.read_text(encoding="utf-8").strip()
        # The file's content is either the raw password or a labeled line
        pw = extract_from_text(content) or content.splitlines()[0].strip()
        if pw:
            if not args.quiet:
                print(f"recovered from {args.admin_file}", file=sys.stderr)
            print(pw)
            return 0

    # Priority 2: log file
    if args.logs and args.logs.is_file():
        text = args.logs.read_text(encoding="utf-8", errors="replace")
        pw = extract_from_text(text)
        if pw:
            if not args.quiet:
                print(f"recovered from {args.logs}", file=sys.stderr)
            print(pw)
            return 0

    # Priority 3: command output
    if args.logs_cmd:
        try:
            proc = subprocess.run(args.logs_cmd, shell=True, capture_output=True,
                                  text=True, timeout=30)
            combined = proc.stdout + "\n" + proc.stderr
            pw = extract_from_text(combined)
            if pw:
                if not args.quiet:
                    print(f"recovered from `{args.logs_cmd}`", file=sys.stderr)
                print(pw)
                return 0
        except subprocess.TimeoutExpired:
            print(f"timed out running {args.logs_cmd}", file=sys.stderr)

    print("no bootstrap admin password found from any source", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
